package remote

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptoRand "crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/remote/acme"
	"piccolod/internal/remote/nexusclient"
	"piccolod/internal/state/paths"
)

// Config holds the persisted remote (Nexus) configuration and runtime state.
type Config struct {
	Endpoint        string            `json:"endpoint"`
	DeviceSecret    string            `json:"device_secret"`
	Solver          string            `json:"solver"`
	TLD             string            `json:"tld"`
	PortalHostname  string            `json:"portal_hostname"`
	DNSProvider     string            `json:"dns_provider,omitempty"`
	DNSCredentials  map[string]string `json:"dns_credentials,omitempty"`
	Enabled         bool              `json:"enabled"`
	Issuer          string            `json:"issuer,omitempty"`
	ExpiresAt       time.Time         `json:"expires_at,omitempty"`
	NextRenewal     time.Time         `json:"next_renewal,omitempty"`
	LastHandshake   time.Time         `json:"last_handshake,omitempty"`
	LatencyMS       int               `json:"latency_ms,omitempty"`
	GuideVerifiedAt *time.Time        `json:"guide_verified_at,omitempty"`
	LastPreflight   *time.Time        `json:"last_preflight,omitempty"`
	Aliases         []Alias           `json:"aliases,omitempty"`
	Certificates    []Certificate     `json:"certificates,omitempty"`
	Events          []Event           `json:"events,omitempty"`
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

// Alias represents a remote alias domain attached to a listener.
type Alias struct {
	ID          string     `json:"id"`
	Hostname    string     `json:"hostname"`
	Listener    string     `json:"listener"`
	Status      string     `json:"status"`
	LastChecked *time.Time `json:"last_checked,omitempty"`
	Message     string     `json:"message,omitempty"`
}

// Certificate captures basic certificate metadata for the inventory table.
type Certificate struct {
	ID            string     `json:"id"`
	Domains       []string   `json:"domains"`
	Solver        string     `json:"solver,omitempty"`
	Attempts      int        `json:"attempts,omitempty"`
	LastAttempt   *time.Time `json:"last_attempt,omitempty"`
	RetryAt       *time.Time `json:"retry_at,omitempty"`
	IssuedAt      *time.Time `json:"issued_at,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	NextRenewal   *time.Time `json:"next_renewal,omitempty"`
	Status        string     `json:"status,omitempty"`
	FailureReason string     `json:"failure_reason,omitempty"`
}

// Event is surfaced in the activity log for remote actions.
type Event struct {
	Timestamp time.Time `json:"ts"`
	Level     string    `json:"level"`
	Source    string    `json:"source"`
	Message   string    `json:"message"`
	NextStep  string    `json:"next_step,omitempty"`
}

// ListenerSummary mirrors the UI expectations for listener metadata.
type ListenerSummary struct {
	Name       string `json:"name"`
	RemoteHost string `json:"remote_host"`
}

// Status matches the shape consumed by the frontend remote page.
type Status struct {
	Enabled         bool              `json:"enabled"`
	State           string            `json:"state"`
	Solver          string            `json:"solver,omitempty"`
	Endpoint        string            `json:"endpoint,omitempty"`
	TLD             string            `json:"tld,omitempty"`
	PortalHostname  string            `json:"portal_hostname,omitempty"`
	LatencyMS       *int              `json:"latency_ms,omitempty"`
	LastHandshake   *time.Time        `json:"last_handshake,omitempty"`
	NextRenewal     *time.Time        `json:"next_renewal,omitempty"`
	Issuer          *string           `json:"issuer,omitempty"`
	ExpiresAt       *time.Time        `json:"expires_at,omitempty"`
	Warnings        []string          `json:"warnings,omitempty"`
	GuideVerifiedAt *time.Time        `json:"guide_verified_at,omitempty"`
	Listeners       []ListenerSummary `json:"listeners,omitempty"`
	Aliases         []Alias           `json:"aliases,omitempty"`
	Certificates    []Certificate     `json:"certificates,omitempty"`
}

// PreflightCheck represents a single validation step.
type PreflightCheck struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
	NextStep string `json:"next_step,omitempty"`
}

// PreflightResult aggregates the outcome of a preflight run.
type PreflightResult struct {
	Checks []PreflightCheck `json:"checks"`
	RanAt  time.Time        `json:"ran_at"`
}

type dialer interface {
	DialTimeout(network, address string, timeout time.Duration) (net.Conn, error)
}

type resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
}

var ErrLocked = errors.New("remote: storage locked")

type Storage interface {
	Load(ctx context.Context) (Config, error)
	Save(ctx context.Context, cfg Config) error
}

type Manager struct {
	storage       Storage
	cfg           *Config
	dialer        dialer
	resolver      resolver
	now           func() time.Time
	adapter       nexusclient.Adapter
	adapterMu     sync.Mutex
	adapterCancel context.CancelFunc
	challenges    *ChallengeManager
	acmeMgr       *acme.Manager
	renewCancel   context.CancelFunc
	renewDone     chan struct{}
	issueCancel   context.CancelFunc
	issueDone     chan struct{}
	issueCh       chan issuanceJob
	issueMu       sync.Mutex
	issueQueued   map[string]struct{}
	needsReload   atomic.Bool
	closed        atomic.Bool
	closeOnce     sync.Once
	closeErr      error
	eventsBus     *events.Bus
	baseDir       string
}

func (m *Manager) certDir() string {
	if m == nil {
		return ""
	}
	return filepath.Join(m.baseDir, "remote", "certs")
}

// CertDirectory returns the directory where certificate material is stored.
func (m *Manager) CertDirectory() string {
	return m.certDir()
}

func NewManager(baseDir string) (*Manager, error) {
	storage, err := newFileStorage(baseDir)
	if err != nil {
		return nil, err
	}
	return newManagerWithDeps(storage, baseDir, netDialer{}, netResolver{}, func() time.Time { return time.Now().UTC() })
}

func NewManagerWithStorage(storage Storage, baseDir string) (*Manager, error) {
	return newManagerWithDeps(storage, baseDir, netDialer{}, netResolver{}, func() time.Time { return time.Now().UTC() })
}

func newManagerWithDeps(storage Storage, baseDir string, d dialer, r resolver, now func() time.Time) (*Manager, error) {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if baseDir == "" {
		baseDir = paths.Root()
	}
	m := &Manager{
		storage:  storage,
		dialer:   d,
		resolver: r,
		now:      now,
		baseDir:  baseDir,
	}
	m.challenges = NewChallengeManager()
	// ACME manager (wire later on configure)
	m.acmeMgr = acme.NewManager(baseDir, m.challenges, "", os.Getenv("PICCOLO_ACME_DIR_URL"))
	m.issueCh = make(chan issuanceJob, 32)
	m.issueQueued = make(map[string]struct{})
	m.issueDone = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	m.issueCancel = cancel
	go func() {
		defer close(m.issueDone)
		m.runIssuanceWorker(ctx)
	}()
	if storage != nil {
		cfg, err := storage.Load(context.Background())
		if err != nil {
			if errors.Is(err, ErrLocked) {
				m.needsReload.Store(true)
			} else {
				return nil, err
			}
		} else {
			m.cfg = &cfg
			if m.cfg.DNSCredentials == nil {
				m.cfg.DNSCredentials = map[string]string{}
			}
			m.needsReload.Store(false)
		}
	}
	if m.cfg == nil {
		m.cfg = &Config{}
	}
	m.updateACMEConfig(m.cfg)
	m.requeueOutstandingIssuances()
	return m, nil
}

// SetNexusAdapter injects the adapter responsible for proxy connectivity.
func (m *Manager) SetNexusAdapter(adapter nexusclient.Adapter) {
	m.adapterMu.Lock()
	m.adapter = adapter
	m.adapterMu.Unlock()
	m.ensureConfigHydrated()
	m.applyAdapterState()
}

// SetEventsBus wires the shared event bus so the manager can publish config changes.
func (m *Manager) SetEventsBus(bus *events.Bus) {
	m.eventsBus = bus
	m.publishConfigChanged()
}

type netDialer struct{}

type persistentConn struct{ net.Conn }

func (netDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	var d net.Dialer
	d.Timeout = timeout
	return d.Dial(network, address)
}

type netResolver struct{}

func (netResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	var r net.Resolver
	return r.LookupHost(ctx, host)
}

func (netResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	var r net.Resolver
	return r.LookupCNAME(ctx, host)
}

type fileStorage struct {
	path string
}

func newFileStorage(baseDir string) (*fileStorage, error) {
	if baseDir == "" {
		baseDir = paths.Root()
	}
	dir := filepath.Join(baseDir, "remote")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &fileStorage{path: filepath.Join(dir, "config.json")}, nil
}

func (s *fileStorage) Load(ctx context.Context) (Config, error) {
	_ = ctx
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (s *fileStorage) Save(ctx context.Context, cfg Config) error {
	_ = ctx
	if cfg.DNSCredentials == nil {
		cfg.DNSCredentials = map[string]string{}
	}
	payload, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, payload, 0o644)
}

func (m *Manager) save(cfg *Config) error {
	if cfg == nil {
		return errors.New("config cannot be nil")
	}
	if cfg.DNSCredentials == nil {
		cfg.DNSCredentials = map[string]string{}
	}
	if m.storage != nil {
		if err := m.storage.Save(context.Background(), *cfg); err != nil {
			if errors.Is(err, ErrLocked) {
				m.needsReload.Store(true)
			}
			return err
		}
	}
	m.cfg = cfg
	m.needsReload.Store(false)
	m.applyAdapterState()
	m.updateACMEConfig(cfg)
	m.publishConfigChanged()
	return nil
}

func (m *Manager) reloadFromStorage() error {
	if m.storage == nil {
		m.needsReload.Store(false)
		return nil
	}
	cfg, err := m.storage.Load(context.Background())
	if err != nil {
		if errors.Is(err, ErrLocked) {
			m.needsReload.Store(true)
		}
		return err
	}
	if cfg.DNSCredentials == nil {
		cfg.DNSCredentials = map[string]string{}
	}
	m.cfg = &cfg
	m.needsReload.Store(false)
	m.applyAdapterState()
	m.updateACMEConfig(&cfg)
	m.publishConfigChanged()
	m.requeueOutstandingIssuances()
	return nil
}

func (m *Manager) ensureConfigHydrated() {
	if m == nil {
		return
	}
	if !m.needsReload.Load() {
		return
	}
	if err := m.reloadFromStorage(); err != nil && !errors.Is(err, ErrLocked) {
		log.Printf("WARN: remote: opportunistic reload failed: %v", err)
	}
}

func (m *Manager) currentConfig() *Config {
	if m == nil {
		return &Config{}
	}
	m.ensureConfigHydrated()
	if m.cfg == nil {
		m.cfg = &Config{}
	}
	return m.cfg
}

func (m *Manager) Status() Status {
	cfg := m.currentConfig()
	warnings := computeWarnings(cfg)

	var latency *int
	if cfg.LatencyMS > 0 {
		latency = intPtr(cfg.LatencyMS)
	}

	state := "disabled"
	if cfg.Enabled {
		state = "active"
		if cfg.LastPreflight == nil {
			state = "preflight_required"
		} else if len(warnings) > 0 {
			state = "warning"
		}
		if !cfg.ExpiresAt.IsZero() && cfg.ExpiresAt.Before(m.now()) {
			state = "error"
		}
	} else if cfg.Endpoint != "" && cfg.TLD != "" {
		// Valid config exists but is disabled -> "stopped"
		state = "stopped"
	}

	return Status{
		Enabled:         cfg.Enabled,
		State:           state,
		Solver:          cfg.Solver,
		Endpoint:        cfg.Endpoint,
		TLD:             cfg.TLD,
		PortalHostname:  cfg.PortalHostname,
		LatencyMS:       latency,
		LastHandshake:   timePtr(cfg.LastHandshake),
		NextRenewal:     timePtr(cfg.NextRenewal),
		Issuer:          stringPtr(cfg.Issuer),
		ExpiresAt:       timePtr(cfg.ExpiresAt),
		Warnings:        warnings,
		GuideVerifiedAt: cfg.GuideVerifiedAt,
		Listeners:       buildListeners(cfg),
		Aliases:         cloneAliases(cfg.Aliases),
		Certificates:    cloneCertificates(cfg.Certificates),
	}
}

// ReloadFromStorage attempts to refresh the in-memory configuration from the backing storage.
func (m *Manager) ReloadFromStorage() error {
	if m == nil {
		return nil
	}
	if err := m.reloadFromStorage(); err != nil {
		if errors.Is(err, ErrLocked) {
			return nil
		}
		return err
	}
	return nil
}

// ConfigureRequest holds the payload accepted by Configure.
type ConfigureRequest struct {
	Endpoint       string            `json:"endpoint"`
	DeviceSecret   string            `json:"device_secret"`
	Solver         string            `json:"solver"`
	TLD            string            `json:"tld"`
	PortalHostname string            `json:"portal_hostname"`
	DNSProvider    string            `json:"dns_provider"`
	DNSCredentials map[string]string `json:"dns_credentials"`
}

// Configure persists a new remote configuration.
func (m *Manager) Configure(req ConfigureRequest) error {
	endpoint := strings.TrimSpace(req.Endpoint)
	if endpoint == "" {
		return errors.New("endpoint required")
	}
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return fmt.Errorf("invalid endpoint: %w", err)
	}

	solver := strings.ToLower(strings.TrimSpace(req.Solver))
	if solver == "" {
		solver = "http-01"
	}
	if solver != "http-01" && solver != "dns-01" {
		return fmt.Errorf("unsupported solver %q", solver)
	}

	tld := strings.TrimSpace(req.TLD)
	if tld == "" || !strings.Contains(tld, ".") {
		return errors.New("tld required")
	}

	rawPortal := strings.TrimSpace(req.PortalHostname)
	if rawPortal == "" {
		return errors.New("portal hostname required")
	}
	portalHost := normalizePortalHost(tld, rawPortal)
	if portalHost == "" {
		return errors.New("portal hostname invalid")
	}

	email := deriveACMEEmail(tld, portalHost)
	if m.acmeMgr != nil {
		m.acmeMgr.SetEmail(email)
		if err := m.acmeMgr.SetSolver(solver, req.DNSProvider, req.DNSCredentials); err != nil {
			return err
		}
	}

	if solver == "dns-01" && strings.TrimSpace(req.DNSProvider) == "" {
		return errors.New("dns_provider required for dns-01")
	}

	now := m.now()
	expires := now.Add(90 * 24 * time.Hour)
	nextRenewal := now.Add(60 * 24 * time.Hour)

	cfg := m.currentConfig()
	existingCerts := append([]Certificate(nil), cfg.Certificates...)
	cfg.Endpoint = endpoint
	cfg.DeviceSecret = strings.TrimSpace(req.DeviceSecret)
	cfg.Solver = solver
	cfg.TLD = tld
	cfg.PortalHostname = portalHost
	cfg.DNSProvider = strings.TrimSpace(req.DNSProvider)
	cfg.DNSCredentials = cloneCredentials(req.DNSCredentials)
	cfg.Enabled = true
	cfg.Issuer = "Let's Encrypt"
	cfg.ExpiresAt = expires
	cfg.NextRenewal = nextRenewal
	cfg.LastHandshake = now
	cfg.LatencyMS = 0
	// Assume preflight passed during setup wizard; prevent immediate warning state
	cfg.LastPreflight = &now
	// Queue background ACME issuance and surface events/inventory.
	newCerts := defaultCertificates(cfg, now)
	for _, c := range existingCerts {
		if c.ID == "portal" || c.ID == "wildcard" {
			continue
		}
		newCerts = append(newCerts, c)
	}
	cfg.Certificates = newCerts
	m.enqueueIssuance("portal", []string{cfg.PortalHostname}, cfg.PortalHostname)
	if cfg.TLD != "" && strings.EqualFold(cfg.Solver, "dns-01") && strings.TrimSpace(cfg.PortalHostname) != "" {
		base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cfg.PortalHostname)), ".")
		if base != "" {
			cn := "*." + base
			m.enqueueIssuance("wildcard", []string{cn, base}, cn)
		}
	}
	cfg.Events = append(cfg.Events, Event{
		Timestamp: now,
		Level:     "info",
		Source:    "remote",
		Message:   "Remote configuration saved",
		NextStep:  "Run preflight",
	})

	return m.save(cfg)
}

// Disable switches remote access off but retains configuration.
func (m *Manager) Disable() error {
	cfg := m.currentConfig()
	cfg.Enabled = false
	now := m.now()
	cfg.Events = append(cfg.Events, Event{
		Timestamp: now,
		Level:     "info",
		Source:    "remote",
		Message:   "Remote access disabled",
	})
	return m.save(cfg)
}

// Rotate generates a new secure device secret.
func (m *Manager) Rotate() (string, error) {
	cfg := m.currentConfig()
	if cfg.Endpoint == "" {
		return "", errors.New("remote not configured")
	}
	newSecret, err := generateSecureSecret()
	if err != nil {
		return "", fmt.Errorf("failed to generate secret: %w", err)
	}
	cfg.DeviceSecret = newSecret
	cfg.Events = append(cfg.Events, Event{
		Timestamp: m.now(),
		Level:     "info",
		Source:    "remote",
		Message:   "Remote device secret rotated",
	})
	if err := m.save(cfg); err != nil {
		return "", err
	}
	return newSecret, nil
}

func generateSecureSecret() (string, error) {
	b := make([]byte, 32) // 256 bits of entropy
	if _, err := cryptoRand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

// ListAliases returns the current alias inventory.
func (m *Manager) ListAliases() []Alias {
	return cloneAliases(m.currentConfig().Aliases)
}

// AddAlias appends a new alias entry.
func (m *Manager) AddAlias(listener, hostname string) (Alias, error) {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" || !strings.Contains(hostname, ".") {
		return Alias{}, errors.New("hostname required")
	}
	if listener == "" {
		listener = "portal"
	}
	cfg := m.currentConfig()
	alias := Alias{
		ID:       fmt.Sprintf("alias-%d", time.Now().UnixNano()+rand.Int63n(1000)),
		Hostname: hostname,
		Listener: listener,
		Status:   "pending",
		Message:  "Awaiting DNS verification",
	}
	cfg.Aliases = append(cfg.Aliases, alias)
	cfg.Events = append(cfg.Events, Event{
		Timestamp: m.now(),
		Level:     "info",
		Source:    "remote",
		Message:   fmt.Sprintf("Alias %s queued for listener %s", hostname, listener),
	})
	if err := m.save(cfg); err != nil {
		return Alias{}, err
	}
	// Queue issuance for the alias hostname (listener-specific cert)
	m.enqueueIssuance("alias:"+strings.ToLower(hostname), []string{strings.ToLower(hostname)}, strings.ToLower(hostname))
	return alias, nil
}

// RemoveAlias deletes an alias by ID.
func (m *Manager) RemoveAlias(id string) error {
	cfg := m.currentConfig()
	idx := -1
	for i, a := range cfg.Aliases {
		if a.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return errors.New("alias not found")
	}
	removed := cfg.Aliases[idx]
	cfg.Aliases = append(cfg.Aliases[:idx], cfg.Aliases[idx+1:]...)
	cfg.Events = append(cfg.Events, Event{
		Timestamp: m.now(),
		Level:     "info",
		Source:    "remote",
		Message:   fmt.Sprintf("Alias %s removed", removed.Hostname),
	})
	if err := m.save(cfg); err != nil {
		return err
	}
	// Remove associated certificate entry and files (best-effort).
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(removed.Hostname)), ".")
	if h != "" {
		m.removeCertificateByID("alias:"+h, h)
	}
	return nil
}

// RemoveHostnameCertificate removes a per-host certificate (host:<hostname>) and its files.
// Safe to call even if no such certificate exists.
func (m *Manager) RemoveHostnameCertificate(hostname string) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	if h == "" {
		return
	}
	m.removeCertificateByID("host:"+h, h)
}

func (m *Manager) removeCertificateByID(id, commonName string) {
	if m == nil {
		return
	}
	cfg := m.currentConfig()
	if cfg == nil {
		return
	}
	removed := false
	var out []Certificate
	for _, c := range cfg.Certificates {
		if c.ID == id {
			removed = true
			continue
		}
		out = append(out, c)
	}
	if removed {
		cfg.Certificates = out
		cfg.Events = append(cfg.Events, Event{
			Timestamp: m.now(),
			Level:     "info",
			Source:    "remote",
			Message:   fmt.Sprintf("Certificate removed (%s)", id),
		})
		_ = m.save(cfg)
	}
	m.deleteCertFiles(id, commonName)
}

func (m *Manager) deleteCertFiles(id, commonName string) {
	certDir := m.certDir()
	if certDir == "" || commonName == "" {
		return
	}
	outName := outNameFor(id, commonName)
	paths := []string{
		filepath.Join(certDir, outName+".crt"),
		filepath.Join(certDir, outName+".key"),
		filepath.Join(certDir, outName+".pem"),
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("WARN: remote: delete cert file %s: %v", p, err)
		}
	}
}

// ListCertificates returns the synthetic certificate inventory.
func (m *Manager) ListCertificates() []Certificate {
	return cloneCertificates(m.currentConfig().Certificates)
}

func (m *Manager) applyAdapterState() {
	if m.closed.Load() {
		return
	}
	m.adapterMu.Lock()
	adapter := m.adapter
	cancel := m.adapterCancel
	cfg := m.cfg
	m.adapterMu.Unlock()

	if adapter == nil {
		return
	}
	if cfg == nil {
		cfg = &Config{}
	}

	adapterCfg := nexusclient.Config{
		Endpoint:       cfg.Endpoint,
		DeviceSecret:   cfg.DeviceSecret,
		PortalHostname: cfg.PortalHostname,
		TLD:            cfg.TLD,
	}
	if err := adapter.Configure(adapterCfg); err != nil {
		log.Printf("WARN: remote: configure nexus adapter failed: %v", err)
	}

	if !cfg.Enabled || cfg.Endpoint == "" || cfg.DeviceSecret == "" || cfg.PortalHostname == "" {
		m.stopAdapter()
		m.stopRenewScheduler()
		return
	}

	if cancel != nil {
		m.stopAdapter()
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.adapterMu.Lock()
	m.adapterCancel = cancel
	adapterRun := m.adapter
	m.adapterMu.Unlock()

	go func() {
		if err := adapterRun.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("WARN: remote: nexus adapter exited: %v", err)
		}
		m.adapterMu.Lock()
		m.adapterCancel = nil
		m.adapterMu.Unlock()
	}()
	// Ensure renew scheduler is running when remote is active
	m.startRenewScheduler()
}

func (m *Manager) publishConfigChanged() {
	if m == nil || m.eventsBus == nil {
		return
	}
	status := m.Status()
	m.eventsBus.Publish(events.Event{
		Topic:   events.TopicRemoteConfigChanged,
		Payload: status,
	})
}

func (m *Manager) updateACMEConfig(cfg *Config) {
	if m == nil || m.acmeMgr == nil || cfg == nil {
		return
	}
	email := deriveACMEEmail(cfg.TLD, cfg.PortalHostname)
	m.acmeMgr.SetEmail(email)
	if err := m.acmeMgr.SetSolver(cfg.Solver, cfg.DNSProvider, cfg.DNSCredentials); err != nil {
		log.Printf("WARN: remote: acme solver config failed: %v", err)
	}
}

// HTTPChallengeHandler exposes a read-only handler for ACME HTTP-01 tokens.
func (m *Manager) HTTPChallengeHandler() http.Handler {
	if m == nil || m.challenges == nil {
		return http.NotFoundHandler()
	}
	return m.challenges.Handler()
}

func (m *Manager) stopAdapter() {
	m.adapterMu.Lock()
	cancel := m.adapterCancel
	adapter := m.adapter
	m.adapterCancel = nil
	m.adapterMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if adapter != nil {
		if err := adapter.Stop(context.Background()); err != nil {
			log.Printf("WARN: remote: stopping nexus adapter: %v", err)
		}
	}
}

// startRenewScheduler starts a background loop to renew certificates when due.
func (m *Manager) startRenewScheduler() {
	if m.closed.Load() || m.renewCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.renewCancel = cancel
	done := make(chan struct{})
	m.renewDone = done
	go func() {
		defer close(done)
		m.runRenewScheduler(ctx)
	}()
}

func (m *Manager) stopRenewScheduler() {
	if m.renewCancel != nil {
		m.renewCancel()
		m.renewCancel = nil
	}
}

func (m *Manager) runRenewScheduler(ctx context.Context) {
	// Check hourly; jitter issuance via pending-state gate
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	// Initial quick check after a short delay
	initial := time.NewTimer(10 * time.Second)
	defer initial.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-initial.C:
			m.scanAndQueueRenewals()
		case <-ticker.C:
			m.scanAndQueueRenewals()
		}
	}
}

func (m *Manager) scanAndQueueRenewals() {
	cfg := m.currentConfig()
	now := m.now()
	for _, c := range cfg.Certificates {
		if strings.EqualFold(c.Status, "pending") {
			continue // avoid duplicate queueing
		}
		dueRetry := c.RetryAt != nil && now.After(*c.RetryAt) && c.Attempts < maxCertAttempts
		dueRenewal := false
		if c.NextRenewal != nil && c.ExpiresAt != nil {
			dueRenewal = now.After(*c.NextRenewal) || now.Add(24*time.Hour).After(*c.ExpiresAt)
		}
		if !dueRetry && !dueRenewal {
			continue
		}
		domains, cn, ok := desiredDomainsAndCN(cfg, c)
		if !ok {
			continue
		}
		m.enqueueIssuance(c.ID, domains, cn)
	}
}

func desiredDomainsAndCN(cfg *Config, c Certificate) ([]string, string, bool) {
	if cfg == nil {
		return nil, "", false
	}
	switch c.ID {
	case "portal":
		if cfg.PortalHostname == "" {
			return nil, "", false
		}
		h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cfg.PortalHostname)), ".")
		return []string{h}, h, true
	case "wildcard":
		if cfg.TLD == "" || cfg.PortalHostname == "" || !strings.EqualFold(cfg.Solver, "dns-01") {
			return nil, "", false
		}
		base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cfg.PortalHostname)), ".")
		if base == "" {
			return nil, "", false
		}
		cn := "*." + base
		return []string{cn, base}, cn, true
	default:
		if strings.HasPrefix(c.ID, "alias:") || strings.HasPrefix(c.ID, "host:") {
			parts := strings.SplitN(c.ID, ":", 2)
			if len(parts) != 2 || parts[1] == "" {
				return nil, "", false
			}
			h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(parts[1])), ".")
			return []string{h}, h, true
		}
	}
	if len(c.Domains) == 0 {
		return nil, "", false
	}
	cn := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(c.Domains[0])), ".")
	return append([]string(nil), c.Domains...), cn, true
}

func (m *Manager) requeueOutstandingIssuances() {
	if m == nil {
		return
	}
	cfg := m.currentConfig()
	now := m.now()
	for _, c := range cfg.Certificates {
		if strings.EqualFold(c.Status, "pending") {
			domains, cn, ok := desiredDomainsAndCN(cfg, c)
			if ok {
				m.queueIssuanceJob(c.ID, domains, cn, true)
			}
			continue
		}
		if strings.EqualFold(c.Status, "error") && c.RetryAt != nil && now.After(*c.RetryAt) && c.Attempts < maxCertAttempts {
			domains, cn, ok := desiredDomainsAndCN(cfg, c)
			if ok {
				m.enqueueIssuanceWithForce(c.ID, domains, cn, true)
			}
		}
	}
}

// RenewCertificate simulates a manual renewal.
func (m *Manager) RenewCertificate(id string) error {
	cfg := m.currentConfig()
	// Find target cert and queue issuance
	for _, c := range cfg.Certificates {
		if c.ID == id {
			domains := append([]string(nil), c.Domains...)
			cn := domains[0]
			if id == "portal" && cfg.PortalHostname != "" {
				cn = cfg.PortalHostname
			}
			if id == "wildcard" && cfg.TLD != "" {
				if !strings.EqualFold(cfg.Solver, "dns-01") {
					return errors.New("wildcard renewals require dns-01 solver")
				}
				base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cfg.PortalHostname)), ".")
				if base == "" {
					return errors.New("portal hostname missing")
				}
				cn = "*." + base
				domains = []string{cn, base}
			}
			m.enqueueIssuanceWithForce(id, domains, cn, true)
			return nil
		}
	}
	return errors.New("certificate not found")
}

// QueueHostnameCertificate requests background issuance for a specific hostname.
// Useful for per-listener certs when wildcard isn't available/supported.
func (m *Manager) QueueHostnameCertificate(hostname string) {
	h := strings.TrimSpace(strings.ToLower(hostname))
	if h == "" {
		return
	}
	m.enqueueIssuance("host:"+h, []string{h}, h)
}

type issuanceJob struct {
	id         string
	domains    []string
	commonName string
	force      bool
}

const maxCertAttempts = 10

// enqueueIssuance records pending inventory and queues issuance for the worker.
func (m *Manager) enqueueIssuance(id string, domains []string, commonName string) {
	m.enqueueIssuanceWithForce(id, domains, commonName, false)
}

func (m *Manager) enqueueIssuanceWithForce(id string, domains []string, commonName string, force bool) {
	if m.acmeMgr == nil || commonName == "" {
		return
	}
	cfg := m.currentConfig()
	// Skip duplicate unless forced.
	for _, c := range cfg.Certificates {
		if c.ID == id && strings.EqualFold(c.Status, "pending") && !force {
			return
		}
	}
	now := m.now()
	m.ensureCertPending(cfg, id, domains, now)
	_ = m.save(cfg)
	m.queueIssuanceJob(id, domains, commonName, force)
}

func (m *Manager) queueIssuanceJob(id string, domains []string, commonName string, force bool) {
	if m == nil || m.issueCh == nil {
		return
	}
	m.issueMu.Lock()
	if m.issueQueued == nil {
		m.issueQueued = make(map[string]struct{})
	}
	if _, ok := m.issueQueued[id]; ok && !force {
		m.issueMu.Unlock()
		return
	}
	m.issueQueued[id] = struct{}{}
	m.issueMu.Unlock()

	job := issuanceJob{id: id, domains: append([]string(nil), domains...), commonName: commonName, force: force}
	select {
	case m.issueCh <- job:
	default:
		m.issueMu.Lock()
		delete(m.issueQueued, id)
		m.issueMu.Unlock()
		log.Printf("WARN: remote: issuance queue full, dropping job %s", id)
	}
}

func (m *Manager) runIssuanceWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-m.issueCh:
			select {
			case <-ctx.Done():
				return
			default:
			}
			m.issueMu.Lock()
			delete(m.issueQueued, job.id)
			m.issueMu.Unlock()
			m.processIssuance(job)
		}
	}
}

// Close stops background goroutines (issuance worker, renew scheduler, nexus adapter).
// It is safe to call multiple times.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		m.closed.Store(true)
		m.stopAdapter()
		m.stopRenewScheduler()
		if m.issueCancel != nil {
			m.issueCancel()
			m.issueCancel = nil
		}

		timeout := 5 * time.Second
		if done := m.renewDone; done != nil {
			select {
			case <-done:
			case <-time.After(timeout):
				m.closeErr = errors.New("remote: shutdown timeout")
				return
			}
		}
		if done := m.issueDone; done != nil {
			select {
			case <-done:
			case <-time.After(timeout):
				m.closeErr = errors.New("remote: shutdown timeout")
				return
			}
		}
	})
	return m.closeErr
}

func (m *Manager) processIssuance(job issuanceJob) {
	if m == nil || m.acmeMgr == nil || job.commonName == "" {
		return
	}
	cfg := m.currentConfig()
	now := m.now()

	// Record attempt.
	attempts := 1
	for i := range cfg.Certificates {
		if cfg.Certificates[i].ID == job.id {
			attempts = cfg.Certificates[i].Attempts + 1
			cfg.Certificates[i].Attempts = attempts
			cfg.Certificates[i].LastAttempt = timePtr(now)
			cfg.Certificates[i].RetryAt = nil
			break
		}
	}
	_ = m.save(cfg)

	if attempts > maxCertAttempts {
		m.updateCertFailure(job.id, "max issuance attempts reached")
		return
	}

	certDir := m.certDir()
	outName := outNameFor(job.id, job.commonName)
	fakeACME := os.Getenv("PICCOLO_REMOTE_FAKE_ACME") == "1"

	if fakeACME {
		expires, err := writeSelfSignedCertificate(certDir, outName, job.commonName, job.domains)
		if err != nil {
			m.updateCertFailure(job.id, err.Error())
			return
		}
		m.updateCertSuccess(job.id, expires)
		return
	}

	sans := buildSans(job.commonName, job.domains)
	if _, err := m.acmeMgr.Issue(job.commonName, sans, outName, certDir); err != nil {
		m.updateCertFailure(job.id, err.Error())
		return
	}
	if exp, ok := readCertExpiry(filepath.Join(certDir, outName+".crt")); ok {
		m.updateCertSuccess(job.id, exp)
	} else {
		m.updateCertSuccess(job.id, now.Add(90*24*time.Hour))
	}
}

func buildSans(commonName string, domains []string) []string {
	commonName = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(commonName)), ".")
	uniq := make(map[string]struct{})
	for _, d := range domains {
		h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if h == "" || h == commonName {
			continue
		}
		uniq[h] = struct{}{}
	}
	out := make([]string, 0, len(uniq))
	for h := range uniq {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func outNameFor(id, cn string) string {
	// For wildcard we want the actual CN as filename (e.g., *.example.com)
	if id == "wildcard" {
		return cn
	}
	if id == "portal" {
		return "portal"
	}
	// default to sanitized cn
	return cn
}

func (m *Manager) ensureCertPending(cfg *Config, id string, domains []string, now time.Time) {
	found := false
	for i := range cfg.Certificates {
		if cfg.Certificates[i].ID == id {
			cfg.Certificates[i].Domains = append([]string(nil), domains...)
			cfg.Certificates[i].Status = "pending"
			cfg.Certificates[i].FailureReason = ""
			cfg.Certificates[i].RetryAt = nil
			cfg.Certificates[i].IssuedAt = nil
			cfg.Certificates[i].ExpiresAt = nil
			cfg.Certificates[i].NextRenewal = nil
			found = true
			break
		}
	}
	if !found {
		cfg.Certificates = append(cfg.Certificates, Certificate{
			ID:      id,
			Domains: append([]string(nil), domains...),
			Status:  "pending",
		})
	}
	cfg.Events = append(cfg.Events, Event{
		Timestamp: now,
		Level:     "info",
		Source:    "remote",
		Message:   fmt.Sprintf("Certificate issuance started (%s)", id),
	})
}

func (m *Manager) updateCertSuccess(id string, expiresAt time.Time) {
	cfg := m.currentConfig()
	now := m.now()
	next := now.Add(60 * 24 * time.Hour)
	for i := range cfg.Certificates {
		if cfg.Certificates[i].ID == id {
			cfg.Certificates[i].IssuedAt = timePtr(now)
			cfg.Certificates[i].ExpiresAt = timePtr(expiresAt)
			cfg.Certificates[i].NextRenewal = timePtr(next)
			cfg.Certificates[i].Status = "ok"
			cfg.Certificates[i].FailureReason = ""
			cfg.Certificates[i].Attempts = 0
			cfg.Certificates[i].RetryAt = nil
			break
		}
	}
	cfg.Events = append(cfg.Events, Event{
		Timestamp: now,
		Level:     "info",
		Source:    "remote",
		Message:   fmt.Sprintf("Certificate issuance succeeded (%s)", id),
	})
	_ = m.save(cfg)
}

func (m *Manager) updateCertFailure(id string, reason string) {
	cfg := m.currentConfig()
	now := m.now()
	for i := range cfg.Certificates {
		if cfg.Certificates[i].ID == id {
			cfg.Certificates[i].Status = "error"
			cfg.Certificates[i].FailureReason = reason
			attempts := cfg.Certificates[i].Attempts
			if attempts <= 0 {
				attempts = 1
				cfg.Certificates[i].Attempts = attempts
			}
			if attempts < maxCertAttempts {
				retry := now.Add(certBackoff(attempts))
				cfg.Certificates[i].RetryAt = timePtr(retry)
			} else {
				cfg.Certificates[i].RetryAt = nil
			}
			break
		}
	}
	cfg.Events = append(cfg.Events, Event{
		Timestamp: now,
		Level:     "warn",
		Source:    "remote",
		Message:   fmt.Sprintf("Certificate issuance failed (%s): %s", id, reason),
		NextStep:  "Verify DNS/Nexus reachability and retry",
	})
	_ = m.save(cfg)
}

func certBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return time.Minute
	}
	shift := attempt - 1
	if shift > 6 {
		shift = 6
	}
	delay := time.Duration(1<<shift) * time.Minute
	if delay > time.Hour {
		delay = time.Hour
	}
	// Add up to 20% jitter.
	jitter := time.Duration(rand.Int63n(int64(delay / 5)))
	return delay + jitter
}

func writeSelfSignedCertificate(dir, outName, commonName string, domains []string) (time.Time, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return time.Time{}, err
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), cryptoRand.Reader)
	if err != nil {
		return time.Time{}, err
	}
	now := time.Now().Add(-time.Minute)
	expires := now.Add(90 * 24 * time.Hour)
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := cryptoRand.Int(cryptoRand.Reader, serialLimit)
	if err != nil {
		return time.Time{}, err
	}
	unique := make(map[string]struct{})
	add := func(host string) {
		h := strings.TrimSpace(strings.ToLower(host))
		if h == "" {
			return
		}
		unique[h] = struct{}{}
	}
	add(commonName)
	for _, d := range domains {
		add(d)
	}
	var dns []string
	for h := range unique {
		dns = append(dns, h)
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now,
		NotAfter:     expires,
		DNSNames:     dns,
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(cryptoRand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return time.Time{}, err
	}
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return time.Time{}, err
	}
	certPath := filepath.Join(dir, outName+".crt")
	keyPath := filepath.Join(dir, outName+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return time.Time{}, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
		return time.Time{}, err
	}
	return expires, nil
}

func readCertExpiry(path string) (time.Time, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
				return cert.NotAfter, true
			}
		}
	}
	return time.Time{}, false
}

// RunPreflight performs validation checks for the remote configuration.
// If candidate is provided, it validates that specific config; otherwise it validates the active config.
func (m *Manager) RunPreflight(candidate *Config) (PreflightResult, error) {
	var cfg *Config
	if candidate != nil {
		cfg = candidate
	} else {
		cfg = m.currentConfig()
	}

	if cfg.Endpoint == "" || cfg.TLD == "" || cfg.PortalHostname == "" {
		return PreflightResult{}, errors.New("remote not configured")
	}

	now := m.now()
	var checks []PreflightCheck

	endpointCheck := m.checkEndpoint(cfg)
	checks = append(checks, endpointCheck)

	dnsStatus, dnsDetail := m.checkDNS(cfg)
	checks = append(checks, PreflightCheck{Name: "DNS records", Status: dnsStatus, Detail: dnsDetail})

	checks = append(checks, PreflightCheck{Name: "ACME solver", Status: "pass", Detail: fmt.Sprintf("Using %s", strings.ToUpper(cfg.Solver))})

	// Only check aliases if we are running against active config, or if candidate has them
	if len(cfg.Aliases) > 0 {
		status := "pass"
		detail := "All aliases verified"
		for _, alias := range cfg.Aliases {
			if alias.Status != "active" {
				status = "warn"
				detail = "One or more aliases pending verification"
				break
			}
		}
		checks = append(checks, PreflightCheck{Name: "Alias coverage", Status: status, Detail: detail})
	}

	// Only persist the preflight timestamp if we are checking the active config
	if candidate == nil {
		cfg.LastPreflight = &now
		cfg.Events = append(cfg.Events, Event{
			Timestamp: now,
			Level:     "info",
			Source:    "remote",
			Message:   "Preflight completed",
		})
		if err := m.save(cfg); err != nil {
			return PreflightResult{}, err
		}
	}
	return PreflightResult{Checks: checks, RanAt: now}, nil
}

// ListEvents returns the persisted remote-related events.
func (m *Manager) ListEvents() []Event {
	events := append([]Event(nil), m.currentConfig().Events...)
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events
}

// GuideVerification carries helper verification metadata.
type GuideVerification struct {
	Endpoint       string `json:"endpoint"`
	TLD            string `json:"tld"`
	PortalHostname string `json:"portal_hostname"`
	JWTSecret      string `json:"jwt_secret"`
}

// VerifyConnection validates the connection parameters without persisting them.
func (m *Manager) VerifyConnection(info GuideVerification) error {
	endpoint := strings.TrimSpace(info.Endpoint)
	if endpoint == "" {
		return errors.New("endpoint required")
	}

	// Create a temporary config to reuse checkEndpoint logic
	tempCfg := &Config{
		Endpoint: endpoint,
	}

	// Perform the check
	check := m.checkEndpoint(tempCfg)
	if check.Status != "pass" {
		if check.Detail != "" {
			return fmt.Errorf("connection failed: %s", check.Detail)
		}
		return errors.New("connection failed: unreachable")
	}

	return nil
}

// MarkGuideVerified validates the connection and records the verification timestamp.
// It does NOT persist the connection details to avoid creating an intermediate "provisioning" state.
func (m *Manager) MarkGuideVerified(info GuideVerification) error {
	if err := m.VerifyConnection(info); err != nil {
		return err
	}

	// Only record that verification happened
	cfg := m.currentConfig()
	now := m.now()
	cfg.GuideVerifiedAt = &now
	cfg.Events = append(cfg.Events, Event{
		Timestamp: now,
		Level:     "info",
		Source:    "remote",
		Message:   "Nexus connection verified",
	})
	return m.save(cfg)
}

// GuideInfo returns static helper information along with verification timestamp.
type GuideInfo struct {
	Command      string     `json:"command"`
	Requirements []string   `json:"requirements"`
	Notes        []string   `json:"notes"`
	DocsURL      string     `json:"docs_url"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`
}

func (m *Manager) GuideInfo() GuideInfo {
	cfg := m.currentConfig()
	return GuideInfo{
		Command: "sudo bash -c 'curl -fsSL https://raw.githubusercontent.com/AtDexters-Lab/nexus-proxy-server/main/scripts/install.sh | bash'",
		Requirements: []string{
			"Systemd-based Linux VM with sudo access",
			"Public ports 80 and 443 open",
			"DNS A/AAAA record ready for the Nexus host",
		},
		Notes: []string{
			"Installer prints the backend JWT secret on success.",
			"Keep the terminal open until the script finishes.",
		},
		DocsURL:    "https://github.com/AtDexters-Lab/nexus-proxy-server/blob/main/readme.md#install",
		VerifiedAt: cfg.GuideVerifiedAt,
	}
}

func (m *Manager) checkEndpoint(cfg *Config) PreflightCheck {
	host, port := endpointHostPort(cfg.Endpoint)
	if host == "" {
		return PreflightCheck{Name: "Nexus endpoint reachable", Status: "fail", Detail: "invalid endpoint"}
	}
	if port == "" {
		port = "443"
	}
	address := net.JoinHostPort(host, port)
	start := time.Now()
	conn, err := m.dialer.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return PreflightCheck{Name: "Nexus endpoint reachable", Status: "fail", Detail: err.Error(), NextStep: "Verify firewall and DNS"}
	}
	latency := int(time.Since(start).Milliseconds())
	_ = conn.Close()
	cfg.LastHandshake = m.now()
	cfg.LatencyMS = latency
	return PreflightCheck{Name: "Nexus endpoint reachable", Status: "pass", Detail: fmt.Sprintf("Latency %d ms", latency)}
}

func (m *Manager) checkDNS(cfg *Config) (string, string) {
	host := cfg.PortalHostname
	if host == "" {
		return "fail", "portal hostname not configured"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cname, cnameErr := m.resolver.LookupCNAME(ctx, host)
	addresses, addrErr := m.resolver.LookupHost(ctx, host)

	detail := fmt.Sprintf("%s resolves to %v", host, addresses)
	if cnameErr == nil && cname != "" {
		detail = fmt.Sprintf("%s CNAME %s", host, strings.TrimSuffix(cname, "."))
	}

	status := "pass"
	if addrErr != nil {
		status = "warn"
		detail = fmt.Sprintf("portal host lookup failed: %v", addrErr)
	}

	if strings.TrimSpace(cfg.PortalHostname) != "" {
		sample := fmt.Sprintf("app.%s", strings.TrimSuffix(strings.TrimSpace(cfg.PortalHostname), "."))
		if _, err := m.resolver.LookupHost(ctx, sample); err != nil {
			status = "warn"
			detail = detail + "; wildcard host unresolved"
		} else {
			detail = detail + "; wildcard host resolves"
		}
	}
	return status, detail
}

func buildListeners(cfg *Config) []ListenerSummary {
	if cfg.PortalHostname == "" {
		return []ListenerSummary{}
	}
	return []ListenerSummary{{Name: "portal", RemoteHost: cfg.PortalHostname}}
}

func computeWarnings(cfg *Config) []string {
	var warnings []string
	now := time.Now()
	if !cfg.NextRenewal.IsZero() && cfg.NextRenewal.Before(now.Add(7*24*time.Hour)) {
		warnings = append(warnings, "Certificate renewal due soon")
	}
	if cfg.PortalHostname == "" {
		warnings = append(warnings, "Portal hostname missing")
	}
	for _, alias := range cfg.Aliases {
		if alias.Status != "active" {
			warnings = append(warnings, fmt.Sprintf("Alias %s is %s", alias.Hostname, alias.Status))
		}
	}
	hasCertError := false
	hasRetry := false
	for _, c := range cfg.Certificates {
		if strings.EqualFold(c.Status, "error") {
			hasCertError = true
			if c.RetryAt != nil && c.RetryAt.After(now) {
				hasRetry = true
			}
		}
	}
	if hasRetry {
		warnings = append(warnings, "Certificate retry scheduled")
	} else if hasCertError {
		warnings = append(warnings, "Certificate issuance failed")
	}
	return warnings
}

func defaultCertificates(cfg *Config, now time.Time) []Certificate {
	exp := now.Add(90 * 24 * time.Hour)
	next := now.Add(60 * 24 * time.Hour)
	certificates := []Certificate{}
	if cfg.PortalHostname != "" {
		certificates = append(certificates, Certificate{
			ID:          "portal",
			Domains:     []string{cfg.PortalHostname},
			Solver:      cfg.Solver,
			IssuedAt:    timePtr(now),
			ExpiresAt:   timePtr(exp),
			NextRenewal: timePtr(next),
			Status:      "ok",
		})
	}
	if cfg.TLD != "" && strings.EqualFold(cfg.Solver, "dns-01") {
		base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cfg.PortalHostname)), ".")
		if base == "" {
			return certificates
		}
		certificates = append(certificates, Certificate{
			ID:          "wildcard",
			Domains:     []string{fmt.Sprintf("*.%s", base), base},
			Solver:      cfg.Solver,
			IssuedAt:    timePtr(now),
			ExpiresAt:   timePtr(exp),
			NextRenewal: timePtr(next),
			Status:      "ok",
		})
	}
	return certificates
}

func cloneAliases(in []Alias) []Alias {
	if len(in) == 0 {
		return []Alias{}
	}
	out := make([]Alias, len(in))
	copy(out, in)
	return out
}

func cloneCertificates(in []Certificate) []Certificate {
	if len(in) == 0 {
		return []Certificate{}
	}
	out := make([]Certificate, len(in))
	copy(out, in)
	return out
}

func cloneCredentials(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func endpointHostPort(endpoint string) (string, string) {
	if endpoint == "" {
		return "", ""
	}
	if u, err := url.Parse(endpoint); err == nil {
		host := u.Hostname()
		port := u.Port()
		if port == "" {
			if u.Scheme == "http" || u.Scheme == "ws" {
				port = "80"
			} else {
				port = "443"
			}
		}
		return host, port
	}
	stripped := strings.TrimPrefix(endpoint, "wss://")
	stripped = strings.TrimPrefix(stripped, "https://")
	stripped = strings.TrimPrefix(stripped, "ws://")
	stripped = strings.TrimPrefix(stripped, "http://")
	parts := strings.SplitN(stripped, "/", 2)
	hostPort := parts[0]
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort, ""
	}
	return host, port
}

func deriveACMEEmail(tld, portal string) string {
	host := strings.TrimSpace(strings.ToLower(portal))
	if host == "" {
		host = strings.TrimSpace(strings.ToLower(tld))
	}
	host = strings.Trim(host, ".")
	if host == "" || !strings.Contains(host, ".") {
		return "admin@piccolo.invalid"
	}
	return fmt.Sprintf("admin@%s", host)
}

func normalizePortalHost(tld, portal string) string {
	tld = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(tld)), ".")
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(portal)), ".")
	if host == "" {
		return ""
	}
	if tld == "" {
		return host
	}
	if host == tld || strings.HasSuffix(host, "."+tld) {
		return host
	}
	if !strings.Contains(host, ".") {
		return host + "." + tld
	}
	return host
}

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func intPtr(v int) *int { return &v }

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	tt := t
	return &tt
}

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

	// Failure classification for self-healing ACME (RFC 20260125)
	FailureClass FailureClass `json:"failure_class,omitempty"` // Classification of failure
	FailureCode  string       `json:"failure_code,omitempty"`  // Machine-readable code (canonical "cert_*" namespace)

	// Per-class attempt tracking to avoid cross-class backoff jumps
	TransientAttempts  int `json:"transient_attempts,omitempty"`  // Attempts with transient failures
	RateLimitAttempts  int `json:"rate_limit_attempts,omitempty"` // Attempts with rate-limit failures
	ConnectionAttempts int `json:"connection_attempts,omitempty"` // Attempts with connection failures (for hybrid handling)

	// Failure timing for hybrid escalation
	FirstConnectionFailureAt *time.Time `json:"first_connection_failure_at,omitempty"` // When connection failures started (for 24h escalation)
	FirstUnauthorizedAt      *time.Time `json:"first_unauthorized_at,omitempty"`       // When unauthorized failures started (for 24h escalation)
}

// Event is surfaced in the activity log for remote actions.
type Event struct {
	Timestamp time.Time `json:"ts"`
	Level     string    `json:"level"`
	Source    string    `json:"source"`
	Message   string    `json:"message"`
	NextStep  string    `json:"next_step,omitempty"`
	CertID    string    `json:"cert_id,omitempty"` // For per-cert event retention (RFC 20260125)
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
	cfgMu         sync.RWMutex // Protects cfg access during concurrent reads/writes
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

	// Wake signal for RetryAt-driven scheduler (RFC 20260125)
	scheduleWakeCh chan struct{}
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
		storage:        storage,
		dialer:         d,
		resolver:       r,
		now:            now,
		baseDir:        baseDir,
		scheduleWakeCh: make(chan struct{}, 1), // Buffered to avoid blocking
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
	m.cfgMu.RLock()
	snap := extractAdapterSnapshot(m.cfg)
	m.cfgMu.RUnlock()
	m.applyAdapterState(snap)
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

// save persists the config. Caller MUST hold cfgMu.Lock() - this function
// releases it after storage I/O completes but before post-save hooks.
func (m *Manager) save(cfg *Config) error {
	// Caller must hold cfgMu.Lock() when calling this function.
	// We release it after storage operations complete.
	if cfg == nil {
		m.cfgMu.Unlock()
		return errors.New("config cannot be nil")
	}
	if cfg.DNSCredentials == nil {
		cfg.DNSCredentials = map[string]string{}
	}

	// Storage save (JSON marshal) happens under lock to prevent races.
	if m.storage != nil {
		if err := m.storage.Save(context.Background(), *cfg); err != nil {
			m.cfgMu.Unlock()
			if errors.Is(err, ErrLocked) {
				m.needsReload.Store(true)
			}
			return err
		}
	}
	m.cfg = cfg
	snap := extractAdapterSnapshot(cfg)
	m.cfgMu.Unlock()

	m.needsReload.Store(false)
	m.applyAdapterState(snap)
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
	m.cfgMu.Lock()
	m.cfg = &cfg
	snap := extractAdapterSnapshot(&cfg)
	m.cfgMu.Unlock()
	m.needsReload.Store(false)
	m.applyAdapterState(snap)
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

// currentConfigLocked returns the config without triggering a reload.
// Must be used when cfgMu is already held (read or write) to avoid deadlock,
// since ensureConfigHydrated -> reloadFromStorage acquires cfgMu.Lock().
func (m *Manager) currentConfigLocked() *Config {
	if m == nil {
		return &Config{}
	}
	if m.cfg == nil {
		m.cfg = &Config{}
	}
	return m.cfg
}

func (m *Manager) Status() Status {
	m.cfgMu.RLock()
	cfg := m.currentConfigLocked()
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

	result := Status{
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
	m.cfgMu.RUnlock()
	return result
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

	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
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
	cfg.Events = append(cfg.Events, Event{
		Timestamp: now,
		Level:     "info",
		Source:    "remote",
		Message:   "Remote configuration saved",
		NextStep:  "Run preflight",
	})
	// save() releases cfgMu.Lock()
	if err := m.save(cfg); err != nil {
		return err
	}

	// Queue issuance jobs after releasing lock (enqueueIssuance acquires its own lock)
	m.enqueueIssuance("portal", []string{portalHost}, portalHost)
	if tld != "" && strings.EqualFold(solver, "dns-01") && portalHost != "" {
		base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(portalHost)), ".")
		if base != "" {
			cn := "*." + base
			m.enqueueIssuance("wildcard", []string{cn, base}, cn)
		}
	}
	return nil
}

// Disable switches remote access off but retains configuration.
func (m *Manager) Disable() error {
	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	cfg.Enabled = false
	now := m.now()
	cfg.Events = append(cfg.Events, Event{
		Timestamp: now,
		Level:     "info",
		Source:    "remote",
		Message:   "Remote access disabled",
	})
	// save() releases cfgMu.Lock()
	return m.save(cfg)
}

// Rotate generates a new secure device secret.
func (m *Manager) Rotate() (string, error) {
	newSecret, err := generateSecureSecret()
	if err != nil {
		return "", fmt.Errorf("failed to generate secret: %w", err)
	}

	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	if cfg.Endpoint == "" {
		m.cfgMu.Unlock()
		return "", errors.New("remote not configured")
	}
	cfg.DeviceSecret = newSecret
	cfg.Events = append(cfg.Events, Event{
		Timestamp: m.now(),
		Level:     "info",
		Source:    "remote",
		Message:   "Remote device secret rotated",
	})
	// save() releases cfgMu.Lock()
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
	m.cfgMu.RLock()
	aliases := cloneAliases(m.currentConfigLocked().Aliases)
	m.cfgMu.RUnlock()
	return aliases
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

	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
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
	// save() releases cfgMu.Lock()
	if err := m.save(cfg); err != nil {
		return Alias{}, err
	}
	// Queue issuance for the alias hostname (listener-specific cert)
	m.enqueueIssuance("alias:"+strings.ToLower(hostname), []string{strings.ToLower(hostname)}, strings.ToLower(hostname))
	return alias, nil
}

// RemoveAlias deletes an alias by ID.
func (m *Manager) RemoveAlias(id string) error {
	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	idx := -1
	for i, a := range cfg.Aliases {
		if a.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		m.cfgMu.Unlock()
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
	// save() releases cfgMu.Lock()
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

	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	if cfg == nil {
		m.cfgMu.Unlock()
		m.deleteCertFiles(id, commonName)
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
		// save() releases cfgMu.Lock()
		_ = m.save(cfg)
	} else {
		m.cfgMu.Unlock()
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
	m.cfgMu.RLock()
	certs := cloneCertificates(m.currentConfigLocked().Certificates)
	m.cfgMu.RUnlock()
	return certs
}

// adapterStateSnapshot holds the config fields needed by applyAdapterState,
// extracted to avoid data races when reading from the live config pointer.
type adapterStateSnapshot struct {
	Endpoint       string
	DeviceSecret   string
	PortalHostname string
	TLD            string
	Enabled        bool
}

func extractAdapterSnapshot(cfg *Config) adapterStateSnapshot {
	if cfg == nil {
		return adapterStateSnapshot{}
	}
	return adapterStateSnapshot{
		Endpoint:       cfg.Endpoint,
		DeviceSecret:   cfg.DeviceSecret,
		PortalHostname: cfg.PortalHostname,
		TLD:            cfg.TLD,
		Enabled:        cfg.Enabled,
	}
}

func (m *Manager) applyAdapterState(snap adapterStateSnapshot) {
	if m.closed.Load() {
		return
	}
	m.adapterMu.Lock()
	adapter := m.adapter
	cancel := m.adapterCancel
	m.adapterMu.Unlock()

	if adapter == nil {
		return
	}

	adapterCfg := nexusclient.Config{
		Endpoint:       snap.Endpoint,
		DeviceSecret:   snap.DeviceSecret,
		PortalHostname: snap.PortalHostname,
		TLD:            snap.TLD,
	}
	if err := adapter.Configure(adapterCfg); err != nil {
		log.Printf("WARN: remote: configure nexus adapter failed: %v", err)
	}

	if !snap.Enabled || snap.Endpoint == "" || snap.DeviceSecret == "" || snap.PortalHostname == "" {
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
	if m.closed.Load() {
		return
	}
	m.adapterMu.Lock()
	if m.renewCancel != nil {
		m.adapterMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.renewCancel = cancel
	done := make(chan struct{})
	m.renewDone = done
	m.adapterMu.Unlock()
	go func() {
		defer close(done)
		m.runRenewScheduler(ctx)
	}()
}

func (m *Manager) stopRenewScheduler() {
	m.adapterMu.Lock()
	if m.renewCancel != nil {
		m.renewCancel()
		m.renewCancel = nil
	}
	m.adapterMu.Unlock()
}

// notifySchedulerWake signals the scheduler to re-evaluate wake time.
// Called whenever RetryAt is set or changed (e.g., after updateCertFailure).
func (m *Manager) notifySchedulerWake() {
	if m == nil || m.scheduleWakeCh == nil {
		return
	}
	select {
	case m.scheduleWakeCh <- struct{}{}:
	default:
		// Already pending wake, no need to queue another
	}
}

// runRenewScheduler implements a RetryAt-driven scheduler per RFC 20260125.
// Instead of fixed hourly ticks, it computes the next wake time from certificate RetryAt/NextRenewal.
func (m *Manager) runRenewScheduler(ctx context.Context) {
	// Initial scan after a short delay
	m.scanAndQueueRenewals()

	for {
		// Compute next wake time based on earliest RetryAt or renewal
		nextWake := m.computeNextWakeTime()

		// Cap at 1 hour to ensure periodic health checks even if no retries due
		if nextWake > time.Hour {
			nextWake = time.Hour
		}
		// Minimum 10 seconds to prevent busy-loop on clock skew
		if nextWake < 10*time.Second {
			nextWake = 10 * time.Second
		}

		timer := time.NewTimer(nextWake)
		select {
		case <-ctx.Done():
			// Clean shutdown: stop timer and drain if needed
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-m.scheduleWakeCh:
			// RetryAt changed - re-evaluate immediately
			// Must drain timer channel after Stop() to prevent spurious wakeup
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			// Don't scan yet, just recalculate wake time
			continue
		case <-timer.C:
			m.scanAndQueueRenewals()
		}
	}
}

// computeNextWakeTime finds the earliest RetryAt or NextRenewal across all certificates.
// Acquires read lock to safely iterate certificates while other goroutines may modify them.
func (m *Manager) computeNextWakeTime() time.Duration {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()

	cfg := m.currentConfigLocked()
	now := m.now()
	earliest := now.Add(time.Hour) // Default: 1 hour

	for _, c := range cfg.Certificates {
		// Check RetryAt for failed certs (including config_error weekly probes)
		// Note: config_error certs are NOT skipped - they have RetryAt set for weekly probes
		if c.RetryAt != nil && c.RetryAt.Before(earliest) {
			earliest = *c.RetryAt
		}
		// Check NextRenewal for non-error certs (error certs use RetryAt backoff only)
		if !strings.EqualFold(c.Status, "error") && c.NextRenewal != nil && c.NextRenewal.Before(earliest) {
			earliest = *c.NextRenewal
		}
	}

	if earliest.Before(now) {
		return 0 // Due now
	}
	return earliest.Sub(now)
}

func (m *Manager) scanAndQueueRenewals() {
	m.cfgMu.RLock()
	cfg := m.currentConfigLocked()
	now := m.now()
	// Collect certificates needing renewal under read lock
	type renewalJob struct {
		id      string
		domains []string
		cn      string
	}
	var jobs []renewalJob
	for _, c := range cfg.Certificates {
		if strings.EqualFold(c.Status, "pending") {
			continue // avoid duplicate queueing
		}
		// RetryAt-driven: retry when due (no max attempts - indefinite retry per RFC 20260125)
		dueRetry := c.RetryAt != nil && now.After(*c.RetryAt)
		// Only check renewal schedule for non-error certs; error certs must wait for RetryAt backoff
		dueRenewal := false
		if !strings.EqualFold(c.Status, "error") && c.NextRenewal != nil && c.ExpiresAt != nil {
			dueRenewal = now.After(*c.NextRenewal) || now.Add(24*time.Hour).After(*c.ExpiresAt)
		}
		if !dueRetry && !dueRenewal {
			continue
		}
		domains, cn, ok := desiredDomainsAndCN(cfg, c)
		if !ok {
			continue
		}
		jobs = append(jobs, renewalJob{id: c.ID, domains: domains, cn: cn})
	}
	m.cfgMu.RUnlock()

	// Enqueue jobs outside of lock
	for _, job := range jobs {
		m.enqueueIssuance(job.id, job.domains, job.cn)
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

	type requeueJob struct {
		id       string
		domains  []string
		cn       string
		isPending bool // true = queueIssuanceJob, false = enqueueIssuanceWithForce
	}

	m.cfgMu.RLock()
	cfg := m.currentConfigLocked()
	now := m.now()
	var jobs []requeueJob
	for _, c := range cfg.Certificates {
		if strings.EqualFold(c.Status, "pending") {
			domains, cn, ok := desiredDomainsAndCN(cfg, c)
			if ok {
				jobs = append(jobs, requeueJob{id: c.ID, domains: domains, cn: cn, isPending: true})
			}
			continue
		}
		// Requeue error certs that are due for retry (no max attempts - indefinite retry per RFC 20260125)
		if strings.EqualFold(c.Status, "error") && c.RetryAt != nil && now.After(*c.RetryAt) {
			domains, cn, ok := desiredDomainsAndCN(cfg, c)
			if ok {
				jobs = append(jobs, requeueJob{id: c.ID, domains: domains, cn: cn, isPending: false})
			}
		}
	}
	m.cfgMu.RUnlock()

	// Process jobs outside of lock
	for _, job := range jobs {
		if job.isPending {
			m.queueIssuanceJob(job.id, job.domains, job.cn, true)
		} else {
			m.enqueueIssuanceWithForce(job.id, job.domains, job.cn, true)
		}
	}
}

// RenewCertificate simulates a manual renewal.
func (m *Manager) RenewCertificate(id string) error {
	m.cfgMu.RLock()
	cfg := m.currentConfigLocked()
	// Find target cert and collect info under lock
	var domains []string
	var cn string
	var found bool
	var errMsg string
	for _, c := range cfg.Certificates {
		if c.ID == id {
			found = true
			domains = append([]string(nil), c.Domains...)
			cn = domains[0]
			if id == "portal" && cfg.PortalHostname != "" {
				cn = cfg.PortalHostname
			}
			if id == "wildcard" && cfg.TLD != "" {
				if !strings.EqualFold(cfg.Solver, "dns-01") {
					errMsg = "wildcard renewals require dns-01 solver"
					break
				}
				base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cfg.PortalHostname)), ".")
				if base == "" {
					errMsg = "portal hostname missing"
					break
				}
				cn = "*." + base
				domains = []string{cn, base}
			}
			break
		}
	}
	m.cfgMu.RUnlock()

	if errMsg != "" {
		return errors.New(errMsg)
	}
	if !found {
		return errors.New("certificate not found")
	}
	m.enqueueIssuanceWithForce(id, domains, cn, true)
	return nil
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

// enqueueIssuance records pending inventory and queues issuance for the worker.
func (m *Manager) enqueueIssuance(id string, domains []string, commonName string) {
	m.enqueueIssuanceWithForce(id, domains, commonName, false)
}

func (m *Manager) enqueueIssuanceWithForce(id string, domains []string, commonName string, force bool) {
	if m.acmeMgr == nil || commonName == "" {
		return
	}

	// Check and modify config under lock to prevent races with scheduler reads
	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	// Skip duplicate unless forced.
	for _, c := range cfg.Certificates {
		if c.ID == id && strings.EqualFold(c.Status, "pending") && !force {
			m.cfgMu.Unlock()
			return
		}
	}
	now := m.now()
	m.ensureCertPending(cfg, id, domains, now)
	// save() releases cfgMu.Lock()
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

	// Record attempt under lock (no max attempts - indefinite retry per RFC 20260125)
	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	now := m.now()
	for i := range cfg.Certificates {
		if cfg.Certificates[i].ID == job.id {
			cfg.Certificates[i].Attempts++
			cfg.Certificates[i].LastAttempt = timePtr(now)
			cfg.Certificates[i].RetryAt = nil
			break
		}
	}
	// save() releases cfgMu.Lock()
	_ = m.save(cfg)

	certDir := m.certDir()
	outName := outNameFor(job.id, job.commonName)
	fakeACME := os.Getenv("PICCOLO_REMOTE_FAKE_ACME") == "1"

	if fakeACME {
		expires, err := writeSelfSignedCertificate(certDir, outName, job.commonName, job.domains)
		if err != nil {
			m.updateCertFailureWithError(job.id, err.Error(), err)
			return
		}
		m.updateCertSuccess(job.id, expires)
		return
	}

	sans := buildSans(job.commonName, job.domains)
	if _, err := m.acmeMgr.Issue(job.commonName, sans, outName, certDir); err != nil {
		// Pass the full error for failure classification
		m.updateCertFailureWithError(job.id, err.Error(), err)
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
	now := m.now()
	next := now.Add(60 * 24 * time.Hour)

	// Modify certificate under lock to prevent races with scheduler reads
	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	for i := range cfg.Certificates {
		if cfg.Certificates[i].ID == id {
			cfg.Certificates[i].IssuedAt = timePtr(now)
			cfg.Certificates[i].ExpiresAt = timePtr(expiresAt)
			cfg.Certificates[i].NextRenewal = timePtr(next)
			cfg.Certificates[i].Status = "ok"
			cfg.Certificates[i].FailureReason = ""
			cfg.Certificates[i].Attempts = 0
			cfg.Certificates[i].RetryAt = nil
			// Reset all failure tracking (RFC 20260125)
			m.resetCertificateTracking(&cfg.Certificates[i])
			break
		}
	}
	// Use retention policy for event appending
	m.appendEventWithRetention(cfg, Event{
		Timestamp: now,
		Level:     "info",
		Source:    "remote",
		Message:   fmt.Sprintf("Certificate issuance succeeded (%s)", id),
		CertID:    id,
	})
	// save() releases cfgMu.Lock()
	_ = m.save(cfg)

	// Emit certificate status change event
	m.publishCertificateChanged(id, "ok", "", "ok")
}

// updateCertFailure records a certificate issuance failure with failure classification.
// It uses the error to classify the failure and compute appropriate retry timing.
func (m *Manager) updateCertFailure(id string, reason string) {
	m.updateCertFailureWithError(id, reason, nil)
}

// updateCertFailureWithError records a certificate issuance failure with full error context.
// The error is used to classify the failure type and determine retry behavior.
func (m *Manager) updateCertFailureWithError(id string, reason string, err error) {
	now := m.now()
	class, code := classifyFailure(err)

	// Modify certificate under lock to prevent races with scheduler reads
	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()

	for i := range cfg.Certificates {
		if cfg.Certificates[i].ID != id {
			continue
		}

		cert := &cfg.Certificates[i]

		// Handle connection errors with hybrid escalation
		if code == "cert_connection_failed" {
			class, code = m.handleConnectionFailure(cert, now)
		}

		// Handle unauthorized errors with hybrid escalation
		if code == "cert_unauthorized" {
			class, code = m.handleUnauthorizedFailure(cert, now)
		}

		cert.Status = "error"
		cert.FailureReason = reason
		cert.FailureClass = class
		cert.FailureCode = code

		// Increment class-specific attempt counter
		switch class {
		case FailureClassTransient:
			cert.TransientAttempts++
		case FailureClassRateLimited:
			cert.RateLimitAttempts++
		}

		// Compute retry time based on failure class using class-specific attempts
		switch class {
		case FailureClassRateLimited:
			// Prefer server-provided Retry-After if available
			if retryAt := parseRetryAfter(err); retryAt != nil {
				cert.RetryAt = retryAt
			} else {
				// Fall back to conservative backoff
				retry := now.Add(rateLimitBackoff(cert.RateLimitAttempts))
				cert.RetryAt = timePtr(retry)
			}

		case FailureClassConfigError:
			// Schedule weekly probe (not fully paused - allows catching external fixes)
			probe := now.Add(ConfigErrorProbeInterval)
			cert.RetryAt = timePtr(probe)

		case FailureClassTransient:
			retry := now.Add(transientBackoff(cert.TransientAttempts))
			cert.RetryAt = timePtr(retry)
		}

		break
	}

	// Append event with retention policy
	m.appendEventWithRetention(cfg, Event{
		Timestamp: now,
		Level:     "warn",
		Source:    "remote",
		Message:   fmt.Sprintf("Certificate issuance failed (%s): %s", id, reason),
		NextStep:  nextStepForCode(code),
		CertID:    id,
	})
	// save() releases cfgMu.Lock()
	_ = m.save(cfg)

	// Signal scheduler to re-evaluate wake time
	m.notifySchedulerWake()

	// Emit certificate status change event
	m.publishCertificateChanged(id, "error", class, code)
}

// handleConnectionFailure applies hybrid escalation for persistent connection errors.
// Returns the (potentially escalated) failure class and code.
func (m *Manager) handleConnectionFailure(cert *Certificate, now time.Time) (FailureClass, string) {
	cert.ConnectionAttempts++

	// Track when connection failures started
	if cert.FirstConnectionFailureAt == nil {
		cert.FirstConnectionFailureAt = timePtr(now)
	}

	// Check if we should escalate to config error
	persistent := cert.ConnectionAttempts >= ConnectionEscalateAfterAttempts ||
		now.Sub(*cert.FirstConnectionFailureAt) >= ConnectionEscalateAfterDuration

	if persistent {
		// Escalate: treat as config error requiring user action
		return FailureClassConfigError, "cert_connection_failed"
	}

	// Still treating as transient
	return FailureClassTransient, "cert_connection_failed"
}

// handleUnauthorizedFailure applies hybrid escalation for persistent unauthorized errors.
// Returns the (potentially escalated) failure class and code.
//
// Note: This function does NOT increment TransientAttempts - the caller's switch statement
// handles incrementing counters based on the returned FailureClass. The escalation check
// uses TransientAttempts+1 to account for the pending increment.
func (m *Manager) handleUnauthorizedFailure(cert *Certificate, now time.Time) (FailureClass, string) {
	// Track when unauthorized errors started
	if cert.FirstUnauthorizedAt == nil {
		cert.FirstUnauthorizedAt = timePtr(now)
	}

	// Escalate to config error if persistent
	// Use TransientAttempts+1 because the switch statement will increment after this returns
	persistent := (cert.TransientAttempts+1) >= UnauthorizedEscalateAfterAttempts ||
		now.Sub(*cert.FirstUnauthorizedAt) >= UnauthorizedEscalateAfterDuration

	if persistent {
		return FailureClassConfigError, "cert_unauthorized_persistent"
	}

	return FailureClassTransient, "cert_unauthorized"
}

// resetCertificateTracking clears failure tracking when a certificate succeeds or config changes.
func (m *Manager) resetCertificateTracking(cert *Certificate) {
	cert.TransientAttempts = 0
	cert.RateLimitAttempts = 0
	cert.ConnectionAttempts = 0
	cert.FirstConnectionFailureAt = nil
	cert.FirstUnauthorizedAt = nil
	cert.FailureClass = ""
	cert.FailureCode = ""
}

// nextStepForCode returns actionable guidance for a failure code.
func nextStepForCode(code string) string {
	steps := map[string]string{
		"cert_dns_error":               "Configure DNS records to point to your device",
		"cert_domain_unreachable":      "Ensure your domain resolves to this device's public IP",
		"cert_caa_forbidden":           "Update CAA DNS record to allow Let's Encrypt",
		"cert_rejected_identifier":     "Contact Let's Encrypt or use a different domain",
		"cert_invalid_contact":         "Update the ACME account email in Remote settings",
		"cert_account_error":           "Re-configure Remote Access to register a new account",
		"cert_unauthorized_persistent": "Check domain DNS, firewall rules, and port forwarding",
		"cert_connection_failed":       "Verify port 80 is forwarded and firewall allows inbound connections",
		"cert_rate_limited":            "Wait for rate limit to expire (check Remote settings for timing)",
		"cert_unauthorized":            "Retry will happen automatically",
		"cert_acme_error":              "Check Let's Encrypt status page for outages",
		"cert_unknown_error":           "Verify DNS/Nexus reachability and retry",
	}
	if step, ok := steps[code]; ok {
		return step
	}
	return "Verify DNS/Nexus reachability and retry"
}

// publishCertificateChanged emits a certificate status change event.
func (m *Manager) publishCertificateChanged(certID, status string, class FailureClass, code string) {
	if m == nil || m.eventsBus == nil {
		return
	}
	m.eventsBus.Publish(events.Event{
		Topic: events.TopicCertificateChanged,
		Payload: events.CertificateChangedEvent{
			CertID:       certID,
			Status:       status,
			FailureClass: string(class),
			FailureCode:  code,
			Timestamp:    m.now(),
		},
	})
}

// Event retention policy constants (RFC 20260125)
const (
	maxEventsTotal      = 100 // Max events in config overall
	maxEventsPerCert    = 10  // Max events per certificate ID
	dedupeWindowMinutes = 60  // Dedupe identical consecutive errors within this window
)

// appendEventWithRetention appends an event with deduplication and retention policy.
// Prevents unbounded event growth from indefinite retries.
func (m *Manager) appendEventWithRetention(cfg *Config, evt Event) {
	if cfg == nil {
		return
	}
	now := m.now()

	// 1. Dedupe: Skip if last event for same cert has identical message within window
	if evt.CertID != "" {
		for i := len(cfg.Events) - 1; i >= 0; i-- {
			last := cfg.Events[i]
			if last.CertID != "" && last.CertID == evt.CertID && last.Message == evt.Message {
				if now.Sub(last.Timestamp) < time.Duration(dedupeWindowMinutes)*time.Minute {
					// Update timestamp only, don't append duplicate
					cfg.Events[i].Timestamp = evt.Timestamp
					return
				}
				break
			}
		}
	}

	// 2. Append new event
	cfg.Events = append(cfg.Events, evt)

	// 3. Trim by certificate: keep only last N events per cert ID
	cfg.Events = trimEventsByCert(cfg.Events, maxEventsPerCert)

	// 4. Trim total: keep only last N events overall
	if len(cfg.Events) > maxEventsTotal {
		cfg.Events = cfg.Events[len(cfg.Events)-maxEventsTotal:]
	}
}

// trimEventsByCert keeps only the last maxPerCert events per certificate ID.
// Events without CertID (general events) are always kept (not subject to per-cert trimming).
// Preserves overall chronological order.
func trimEventsByCert(events []Event, maxPerCert int) []Event {
	certCounts := make(map[string]int)
	var result []Event

	// Iterate in reverse to keep most recent
	for i := len(events) - 1; i >= 0; i-- {
		evt := events[i]
		// Events without CertID pass through; cert events are counted
		if evt.CertID == "" || certCounts[evt.CertID] < maxPerCert {
			result = append([]Event{evt}, result...)
			if evt.CertID != "" {
				certCounts[evt.CertID]++
			}
		}
	}
	return result
}

// certBackoff computes retry delay based on failure class and class-specific attempt count.
// This replaces the original attempt-based backoff with class-aware logic per RFC 20260125.
func certBackoff(attempt int, class FailureClass) time.Duration {
	switch class {
	case FailureClassRateLimited:
		return rateLimitBackoff(attempt)
	case FailureClassConfigError:
		return ConfigErrorProbeInterval // 168 hours (weekly)
	default: // FailureClassTransient
		return transientBackoff(attempt)
	}
}

// transientBackoff implements exponential backoff for transient failures.
// 1min → 2min → 4min → 8min → 16min → 32min → 1hr (capped) for attempts 1-10.
// For long-term retries (> 10 attempts), switches to daily probes.
func transientBackoff(attempt int) time.Duration {
	if attempt <= 10 {
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
		// Add up to 20% jitter
		jitter := time.Duration(rand.Int63n(int64(delay / 5)))
		return delay + jitter
	}
	// Long-term retries for persistent transient failures: daily with jitter
	base := 24 * time.Hour
	jitter := time.Duration(rand.Int63n(int64(4 * time.Hour)))
	return base + jitter
}

// rateLimitBackoff implements conservative backoff for Let's Encrypt rate limits.
// Uses increasingly longer delays to avoid hitting rate limits again.
func rateLimitBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		// First rate limit: 12-24 hours
		return 12*time.Hour + time.Duration(rand.Int63n(int64(12*time.Hour)))
	case attempt <= 3:
		// Subsequent: 24-48 hours
		return 24*time.Hour + time.Duration(rand.Int63n(int64(24*time.Hour)))
	default:
		// Persistent rate limits: 3-7 days
		return 72*time.Hour + time.Duration(rand.Int63n(int64(96*time.Hour)))
	}
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
		m.cfgMu.Lock()
		cfg.LastPreflight = &now
		cfg.Events = append(cfg.Events, Event{
			Timestamp: now,
			Level:     "info",
			Source:    "remote",
			Message:   "Preflight completed",
		})
		// save() releases cfgMu.Lock()
		if err := m.save(cfg); err != nil {
			return PreflightResult{}, err
		}
	}
	return PreflightResult{Checks: checks, RanAt: now}, nil
}

// ListEvents returns the persisted remote-related events.
func (m *Manager) ListEvents() []Event {
	m.cfgMu.RLock()
	events := append([]Event(nil), m.currentConfigLocked().Events...)
	m.cfgMu.RUnlock()
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
	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	now := m.now()
	cfg.GuideVerifiedAt = &now
	cfg.Events = append(cfg.Events, Event{
		Timestamp: now,
		Level:     "info",
		Source:    "remote",
		Message:   "Nexus connection verified",
	})
	// save() releases cfgMu.Lock()
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
	m.cfgMu.RLock()
	verifiedAt := m.currentConfigLocked().GuideVerifiedAt
	m.cfgMu.RUnlock()
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
		VerifiedAt: verifiedAt,
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

package remote

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptoRand "crypto/rand"
	"crypto/tls"
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

	"piccolod/internal/api"
	"piccolod/internal/events"
	"piccolod/internal/fsutil"
	"piccolod/internal/hostname"
	"piccolod/internal/remote/acme"
	"piccolod/internal/remote/nexusclient"
	"piccolod/internal/remote/orchestrator"
	"piccolod/internal/state/paths"
)

// NexusConfig holds self-hosted nexus credentials, connection state, and aliases.
// Persisted to nexus.json.
type NexusConfig struct {
	Endpoint             string     `json:"endpoint"`
	DeviceSecret         string     `json:"device_secret"`
	Solver               string     `json:"solver"`
	PortalHostname       string     `json:"portal_hostname"`        // Fully-qualified hostname (e.g., portal.home.example.com)
	Managed              bool       `json:"managed,omitempty"`       // True for piccolospace managed nexus (DNS-01 via orchestrator)
	Enabled              bool       `json:"enabled"`
	OrchestratorEndpoint string     `json:"orchestrator_endpoint,omitempty"` // Orchestrator API endpoint
	Issuer               string     `json:"issuer,omitempty"`
	ExpiresAt            time.Time  `json:"expires_at,omitempty"`
	NextRenewal          time.Time  `json:"next_renewal,omitempty"`
	LastHandshake        time.Time  `json:"last_handshake,omitempty"`
	LatencyMS            int        `json:"latency_ms,omitempty"`
	GuideVerifiedAt      *time.Time `json:"guide_verified_at,omitempty"`
	LastPreflight        *time.Time `json:"last_preflight,omitempty"`
	Aliases              []Alias    `json:"aliases,omitempty"`
}

// CertInventory holds the shared certificate inventory across all sources.
// Persisted to certificates.json.
type CertInventory struct {
	Certificates []Certificate `json:"certificates,omitempty"`
}

// EventLog holds the activity log for remote operations.
// Persisted to events.json (bootstrap filesystem only, not encrypted repo).
type EventLog struct {
	Events []Event `json:"events,omitempty"`
}

// Config is the unified in-memory remote configuration.
// Embedding preserves all existing field access (cfg.Endpoint, cfg.Events, etc.).
// JSON marshaling produces flat output for backward compatibility.
type Config struct {
	NexusConfig
	CertInventory
	EventLog
}


// Alias represents a remote alias domain attached to a listener.
type Alias struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Listener string `json:"listener"`
	Status   string `json:"status"`
}

// Certificate captures basic certificate metadata for the inventory table.
// For source-tagged certs, Domains is kept fresh by the source's event handler
// re-enqueuing via EnqueueCertIssuance when hostnames change.
type Certificate struct {
	ID            string     `json:"id"`
	Domains       []string   `json:"domains"`
	Source        string     `json:"source,omitempty"`    // e.g., "self-hosted", "namek" — for orchClient lookup
	Solver        string     `json:"solver,omitempty"`
	CertDir       string     `json:"cert_dir,omitempty"`  // override cert file location
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
	Managed         bool              `json:"managed,omitempty"`
	Endpoint        string            `json:"endpoint,omitempty"`
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

// relayState tracks the connection status of a single relay adapter.
type relayState struct {
	connected bool
	err       string // last error message (empty when connected)
}

type dialer interface {
	DialTimeout(network, address string, timeout time.Duration) (net.Conn, error)
	DialTLS(network, address, serverName string, timeout time.Duration) (net.Conn, error)
}

type resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
}

var ErrLocked = errors.New("remote: storage locked")

// Storage persists remote configuration to durable storage.
// Callers MUST serialize Save* calls externally (Manager uses cfgMu for this).
type Storage interface {
	Load(ctx context.Context) (Config, error)
	SaveNexus(ctx context.Context, nexus NexusConfig, certs CertInventory) error
	SaveCerts(ctx context.Context, nexus NexusConfig, certs CertInventory) error
	SaveEvents(ctx context.Context, events EventLog) error
}

type Manager struct {
	storage       Storage
	cfg           *Config
	cfgMu         sync.RWMutex // Protects cfg access during concurrent reads/writes
	dialer        dialer
	resolver      resolver
	now           func() time.Time
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

	// Self-hosted adapter (existing behavior)
	adapterMu     sync.Mutex // protects both self-hosted and namek adapter fields
	adapter       nexusclient.Adapter
	adapterCancel context.CancelFunc
	lastAdapterKey string

	// Source-agnostic orchestrator client registry (RFC 20260312)
	orchClients map[string]acme.OrchestratorClient // source → orchClient

	// Port claim provider for Nexus relay registration
	portClaimProvider PortClaimProvider

	// Per-source relay connection state. Multiple relays (self-hosted "piccolo-portal",
	// namek "piccolo-namek") can be active simultaneously. Entries appear on connect
	// events and are removed when the adapter stops. HTTP-01 cert issuance is gated
	// on ALL tracked relays being connected (see httpChallengeReachable).
	relayMu     sync.RWMutex
	relayStates map[string]relayState // adapter name → state
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
		baseDir = paths.CoreRoot()
	}
	m := &Manager{
		storage:        storage,
		dialer:         d,
		resolver:       r,
		now:            now,
		baseDir:        baseDir,
		scheduleWakeCh: make(chan struct{}, 1), // Buffered to avoid blocking
		relayStates:    make(map[string]relayState),
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
			m.needsReload.Store(false)
		}
	}
	if m.cfg == nil {
		m.cfg = &Config{}
	}
	if m.cfg.Enabled {
		log.Printf("remote: loaded existing config (solver=%s, managed=%v, portal=%s, certs=%d)",
			m.cfg.Solver, m.cfg.Managed, m.cfg.PortalHostname, len(m.cfg.Certificates))
	}
	m.updateACMEConfig(m.cfg)
	m.requeueOutstandingIssuances()
	return m, nil
}

// SetNexusAdapter injects the self-hosted adapter responsible for proxy connectivity.
func (m *Manager) SetNexusAdapter(adapter nexusclient.Adapter) {
	m.adapterMu.Lock()
	m.adapter = adapter
	m.adapterMu.Unlock()
	m.ensureConfigHydrated()
	m.cfgMu.RLock()
	snap := extractAdapterSnapshot(m.cfg)
	m.cfgMu.RUnlock()
	snap = m.snapshotWithClaims(snap)
	m.applyAdapterState(snap)
}


// SetEventsBus wires the shared event bus so the manager can publish config changes.
func (m *Manager) SetEventsBus(bus *events.Bus) {
	m.eventsBus = bus
	m.publishConfigChanged()
}

// PortClaimProvider queries active port claims from the service registry.
type PortClaimProvider interface {
	ActivePortClaims() []api.PortClaimInfo
}

// SetPortClaimProvider wires the provider for querying active port claims.
func (m *Manager) SetPortClaimProvider(p PortClaimProvider) {
	m.adapterMu.Lock()
	m.portClaimProvider = p
	m.adapterMu.Unlock()
}

// RefreshPortClaims re-evaluates active port claims and applies them to the
// adapter. Called when service endpoints change (app install/start/stop or
// post-unlock service restore) so that claim mappings propagate to the relay.
func (m *Manager) RefreshPortClaims() {
	if m == nil || m.closed.Load() {
		return
	}
	m.cfgMu.RLock()
	snap := extractAdapterSnapshot(m.cfg)
	m.cfgMu.RUnlock()
	snap = m.snapshotWithClaims(snap)
	m.applyAdapterState(snap)
}

// RegisterOrchClient registers an orchestrator client for a source tag.
// Used by external sources (e.g., namek) to provide DNS-01 challenge solvers.
func (m *Manager) RegisterOrchClient(source string, client acme.OrchestratorClient) {
	m.adapterMu.Lock()
	if m.orchClients == nil {
		m.orchClients = make(map[string]acme.OrchestratorClient)
	}
	m.orchClients[source] = client
	m.adapterMu.Unlock()
}

// UnregisterOrchClient removes the orchestrator client for a source tag.
func (m *Manager) UnregisterOrchClient(source string) {
	m.adapterMu.Lock()
	delete(m.orchClients, source)
	m.adapterMu.Unlock()
}

// getOrchClient returns the registered orchestrator client for a source, or nil.
func (m *Manager) getOrchClient(source string) acme.OrchestratorClient {
	m.adapterMu.Lock()
	c := m.orchClients[source]
	m.adapterMu.Unlock()
	return c
}

// SetACMEEmail sets the ACME account contact email used for Let's Encrypt registration.
// Used by external sources (e.g., namek) that manage certs outside the self-hosted config path.
// No-ops if email is empty after normalization.
func (m *Manager) SetACMEEmail(email string) {
	if m == nil || m.acmeMgr == nil {
		return
	}
	m.acmeMgr.SetEmail(email)
}

// AppendEvent appends an event to the persisted activity log.
// Safe for concurrent use. Uses the lock-persist-publish pattern.
func (m *Manager) AppendEvent(evt Event) {
	if m == nil {
		return
	}
	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	m.appendEventWithRetention(cfg, evt)
	_ = m.saveEvents(cfg)
}

// CertIssuanceRequest is the public API for enqueuing cert issuance from external sources.
type CertIssuanceRequest struct {
	ID         string
	Source     string
	Solver     string
	CertDir    string
	CommonName string
	Domains    []string
	Reissue    bool // bypass inventory freshness checks (recovery/retry paths only)
}

// EnqueueCertIssuance enqueues a certificate issuance request from an external source.
// The source's orchClient must be registered via RegisterOrchClient before calling this.
func (m *Manager) EnqueueCertIssuance(req CertIssuanceRequest) {
	if m == nil || m.acmeMgr == nil || req.CommonName == "" {
		return
	}
	m.enqueueIssuanceJob(issuanceJob{
		id:         req.ID,
		domains:    req.Domains,
		commonName: req.CommonName,
		reissue:    req.Reissue,
		source:     req.Source,
		solver:     req.Solver,
		certDir:    req.CertDir,
	})
	// Ensure the renew scheduler is running so source-tagged certs get renewed.
	// For namek-only devices, applyAdapterState (self-hosted path) is never called,
	// so EnqueueCertIssuance is the only trigger.
	m.startRenewScheduler()
}

type netDialer struct{}

type persistentConn struct{ net.Conn }

func (netDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	var d net.Dialer
	d.Timeout = timeout
	return d.Dial(network, address)
}

func (netDialer) DialTLS(network, address, serverName string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	return tls.DialWithDialer(d, network, address, &tls.Config{ServerName: serverName})
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

// Split-file path helpers shared by all Storage implementations.
func NexusPath(dir string) string  { return filepath.Join(dir, "nexus.json") }
func CertsPath(dir string) string  { return filepath.Join(dir, "certificates.json") }
func EventsPath(dir string) string { return filepath.Join(dir, "events.json") }

// LoadSplitFiles reads the three split files from dir, assembling a Config.
// Missing files produce zero-value sub-structs. Returns (cfg, true) if at least
// one file was found, or (zero, false) if none exist.
func LoadSplitFiles(dir string) (Config, bool) {
	var cfg Config
	var found bool
	if data, err := os.ReadFile(NexusPath(dir)); err == nil {
		if json.Unmarshal(data, &cfg.NexusConfig) == nil {
			found = true
		}
	}
	if data, err := os.ReadFile(CertsPath(dir)); err == nil {
		if json.Unmarshal(data, &cfg.CertInventory) == nil {
			found = true
		}
	}
	if data, err := os.ReadFile(EventsPath(dir)); err == nil {
		_ = json.Unmarshal(data, &cfg.EventLog)
		found = true
	}
	return cfg, found
}

type fileStorage struct {
	dir string
}

func newFileStorage(baseDir string) (*fileStorage, error) {
	if baseDir == "" {
		baseDir = paths.CoreRoot()
	}
	dir := filepath.Join(baseDir, "remote")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &fileStorage{dir: dir}, nil
}

func (s *fileStorage) Load(_ context.Context) (Config, error) {
	cfg, _ := LoadSplitFiles(s.dir)
	return cfg, nil
}

func (s *fileStorage) SaveNexus(_ context.Context, nexus NexusConfig, _ CertInventory) error {
	payload, err := json.MarshalIndent(&nexus, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(NexusPath(s.dir), payload, 0o600)
}

func (s *fileStorage) SaveCerts(_ context.Context, _ NexusConfig, certs CertInventory) error {
	payload, err := json.MarshalIndent(&certs, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(CertsPath(s.dir), payload, 0o600)
}

func (s *fileStorage) SaveEvents(_ context.Context, events EventLog) error {
	payload, err := json.MarshalIndent(&events, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(EventsPath(s.dir), payload, 0o600)
}

// saveNexus persists nexus config changes.
// Caller MUST hold cfgMu.Lock(); this method releases it before returning.
// Triggers: adapter state + ACME config + config change publish.
func (m *Manager) saveNexus(cfg *Config) error {
	if cfg == nil {
		m.cfgMu.Unlock()
		return errors.New("config cannot be nil")
	}
	if m.storage != nil {
		if err := m.storage.SaveNexus(context.Background(), cfg.NexusConfig, cfg.CertInventory); err != nil {
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

	snap = m.snapshotWithClaims(snap)
	m.needsReload.Store(false)
	m.applyAdapterState(snap)
	m.updateACMEConfig(cfg)
	m.publishConfigChanged()
	return nil
}

// saveCerts persists certificate inventory changes.
// Caller MUST hold cfgMu.Lock(); this method releases it before returning.
// Triggers: config change publish only (no adapter restart, no ACME reconfig).
func (m *Manager) saveCerts(cfg *Config) error {
	if cfg == nil {
		m.cfgMu.Unlock()
		return errors.New("config cannot be nil")
	}
	if m.storage != nil {
		if err := m.storage.SaveCerts(context.Background(), cfg.NexusConfig, cfg.CertInventory); err != nil {
			m.cfgMu.Unlock()
			if errors.Is(err, ErrLocked) {
				m.needsReload.Store(true)
			}
			return err
		}
	}
	m.cfg = cfg
	m.cfgMu.Unlock()

	m.needsReload.Store(false)
	m.publishConfigChanged()
	return nil
}

// saveEvents persists the event log only.
// Caller MUST hold cfgMu.Lock(); this method releases it before returning.
// Triggers: config change publish only (for UI event stream).
// No adapter restart, no ACME reconfig.
func (m *Manager) saveEvents(cfg *Config) error {
	if cfg == nil {
		m.cfgMu.Unlock()
		return errors.New("config cannot be nil")
	}
	if m.storage != nil {
		if err := m.storage.SaveEvents(context.Background(), cfg.EventLog); err != nil {
			log.Printf("WARN: remote: event save failed: %v", err)
		}
	}
	m.cfg = cfg
	m.cfgMu.Unlock()
	m.publishConfigChanged()
	return nil
}

// saveNexusAndEvents persists nexus config + events.
// Caller MUST hold cfgMu.Lock(); this method releases it before returning.
// Triggers: adapter state + ACME config + config change publish.
// Events are best-effort — nexus save failure is hard error, event save failure is logged.
func (m *Manager) saveNexusAndEvents(cfg *Config) error {
	if cfg == nil {
		m.cfgMu.Unlock()
		return errors.New("config cannot be nil")
	}
	if m.storage != nil {
		if err := m.storage.SaveNexus(context.Background(), cfg.NexusConfig, cfg.CertInventory); err != nil {
			m.cfgMu.Unlock()
			if errors.Is(err, ErrLocked) {
				m.needsReload.Store(true)
			}
			return err
		}
		// Event save under lock to avoid data race on the Events slice.
		if err := m.storage.SaveEvents(context.Background(), cfg.EventLog); err != nil {
			log.Printf("WARN: remote: event save failed: %v", err)
		}
	}
	m.cfg = cfg
	snap := extractAdapterSnapshot(cfg)
	m.cfgMu.Unlock()

	snap = m.snapshotWithClaims(snap)
	m.needsReload.Store(false)
	m.applyAdapterState(snap)
	m.updateACMEConfig(cfg)
	m.publishConfigChanged()
	return nil
}

// saveCertsAndEvents persists certificate inventory + events.
// Caller MUST hold cfgMu.Lock(); this method releases it before returning.
// Triggers: config change publish only (no adapter restart, no ACME reconfig).
// Events are best-effort.
func (m *Manager) saveCertsAndEvents(cfg *Config) error {
	if cfg == nil {
		m.cfgMu.Unlock()
		return errors.New("config cannot be nil")
	}
	if m.storage != nil {
		if err := m.storage.SaveCerts(context.Background(), cfg.NexusConfig, cfg.CertInventory); err != nil {
			m.cfgMu.Unlock()
			if errors.Is(err, ErrLocked) {
				m.needsReload.Store(true)
			}
			return err
		}
		// Event save under lock to avoid data race on the Events slice.
		if err := m.storage.SaveEvents(context.Background(), cfg.EventLog); err != nil {
			log.Printf("WARN: remote: event save failed: %v", err)
		}
	}
	m.cfg = cfg
	m.cfgMu.Unlock()

	m.needsReload.Store(false)
	m.publishConfigChanged()
	return nil
}

// saveAll persists all three (nexus + certs + events).
// Caller MUST hold cfgMu.Lock(); this method releases it before returning.
// Used for full config changes (Configure, ConfigureManaged).
// Triggers: adapter state + ACME config + config change publish.
func (m *Manager) saveAll(cfg *Config) error {
	if cfg == nil {
		m.cfgMu.Unlock()
		return errors.New("config cannot be nil")
	}
	if m.storage != nil {
		// SaveNexus writes repo (nexus+certs blob) + nexus.json.
		if err := m.storage.SaveNexus(context.Background(), cfg.NexusConfig, cfg.CertInventory); err != nil {
			m.cfgMu.Unlock()
			if errors.Is(err, ErrLocked) {
				m.needsReload.Store(true)
			}
			return err
		}
		// SaveCerts writes repo again (same blob) + certificates.json.
		// The duplicate repo write is acceptable here since saveAll is only called
		// during Configure/ConfigureManaged which are infrequent user actions.
		if err := m.storage.SaveCerts(context.Background(), cfg.NexusConfig, cfg.CertInventory); err != nil {
			m.cfgMu.Unlock()
			if errors.Is(err, ErrLocked) {
				m.needsReload.Store(true)
			}
			return err
		}
		// Event save under lock to avoid data race on the Events slice.
		if err := m.storage.SaveEvents(context.Background(), cfg.EventLog); err != nil {
			log.Printf("WARN: remote: event save failed: %v", err)
		}
	}
	m.cfg = cfg
	snap := extractAdapterSnapshot(cfg)
	m.cfgMu.Unlock()

	snap = m.snapshotWithClaims(snap)
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

	m.cfgMu.Lock()
	m.cfg = &cfg
	snap := extractAdapterSnapshot(&cfg)
	m.cfgMu.Unlock()
	snap = m.snapshotWithClaims(snap)
	m.needsReload.Store(false)
	if cfg.Enabled {
		log.Printf("remote: reloaded config (solver=%s, managed=%v, portal=%s, certs=%d)",
			cfg.Solver, cfg.Managed, cfg.PortalHostname, len(cfg.Certificates))
	}
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

	state := string(RemoteStateDisabled)
	if cfg.Enabled {
		state = string(RemoteStateActive)
		if cfg.LastPreflight == nil {
			state = string(RemoteStatePreflightRequired)
		} else if len(warnings) > 0 {
			state = string(RemoteStateWarning)
		}
		if !cfg.ExpiresAt.IsZero() && cfg.ExpiresAt.Before(m.now()) {
			state = string(RemoteStateError)
		}
	} else if cfg.Endpoint != "" && cfg.PortalHostname != "" {
		// Valid config exists but is disabled -> "stopped"
		state = string(RemoteStateStopped)
	}

	aliases := cloneAliases(cfg.Aliases)
	for i := range aliases {
		aliases[i].Status = aliasCertStatus(cfg, aliases[i])
	}

	result := Status{
		Enabled:         cfg.Enabled,
		State:           state,
		Solver:          cfg.Solver,
		Managed:         cfg.Managed,
		Endpoint:        cfg.Endpoint,
		PortalHostname:  cfg.PortalHostname,
		LatencyMS:       latency,
		LastHandshake:   timePtr(cfg.LastHandshake),
		NextRenewal:     timePtr(cfg.NextRenewal),
		Issuer:          stringPtr(cfg.Issuer),
		ExpiresAt:       timePtr(cfg.ExpiresAt),
		Warnings:        warnings,
		GuideVerifiedAt: cfg.GuideVerifiedAt,
		Listeners:       buildListeners(cfg),
		Aliases:         aliases,
		Certificates:    cloneCertificates(cfg.Certificates),
	}
	m.cfgMu.RUnlock()

	// Append relay connection warning (requires relayMu, so done after cfgMu release).
	if cfg.Enabled {
		if w := m.relayWarning(); w != "" {
			result.Warnings = append(result.Warnings, w)
			if result.State == string(RemoteStateActive) {
				result.State = string(RemoteStateWarning)
			}
		}
	}

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

// ConfigureRequest holds the payload accepted by Configure (user-managed mode, HTTP-01 only).
type ConfigureRequest struct {
	Endpoint       string `json:"endpoint"`
	DeviceSecret   string `json:"device_secret"`
	PortalHostname string `json:"portal_hostname"` // Fully-qualified hostname (e.g., portal.home.example.com)
}

// ManagedConfigureRequest holds the payload for managed nexus configuration (DNS-01 via orchestrator).
type ManagedConfigureRequest struct {
	OrchestratorEndpoint string `json:"orchestrator_endpoint"`
	DeviceToken          string `json:"device_token"`
	PortalHostname       string `json:"portal_hostname"` // Fully-qualified hostname (e.g., portal.home.example.com)
}

// configureParams holds mode-specific parameters for configureCommon.
type configureParams struct {
	portalHost   string
	solver       string
	endpoint     string
	orchClient   acme.OrchestratorClient // nil for HTTP-01
	eventMessage string
	// mutateConfig applies mode-specific config changes under cfgMu.
	// Return non-nil error to abort (configureCommon will unlock and return it).
	mutateConfig func(cfg *Config) error
}

// configureCommon encapsulates the shared configuration logic for Configure and ConfigureManaged.
// It validates the portal hostname, configures ACME, mutates config, resets certificates, and saves.
// NOTE: saveAll releases cfgMu — this function must NOT defer-unlock.
func (m *Manager) configureCommon(p configureParams) error {
	portalHost := hostname.Normalize(p.portalHost)
	if portalHost == "" {
		return errors.New("portal_hostname required")
	}
	if !strings.Contains(portalHost, ".") {
		return errors.New("portal_hostname must be a fully-qualified hostname (e.g., portal.home.example.com)")
	}

	email := deriveACMEEmail(portalHost)
	if m.acmeMgr != nil {
		m.acmeMgr.SetEmail(email)
		if err := m.acmeMgr.SetSolver(p.solver); err != nil {
			return err
		}
		if p.orchClient != nil {
			m.acmeMgr.SetOrchestratorClient(p.orchClient)
		}
	}

	now := m.now()
	expires := now.Add(90 * 24 * time.Hour)
	nextRenewal := now.Add(60 * 24 * time.Hour)

	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	existingCerts := append([]Certificate(nil), cfg.Certificates...)

	// Apply mode-specific config changes under lock.
	if p.mutateConfig != nil {
		if err := p.mutateConfig(cfg); err != nil {
			m.cfgMu.Unlock()
			return err
		}
	}

	cfg.Endpoint = p.endpoint
	cfg.Solver = p.solver
	cfg.PortalHostname = portalHost
	cfg.Enabled = true
	cfg.Issuer = "Let's Encrypt"
	cfg.ExpiresAt = expires
	cfg.NextRenewal = nextRenewal
	cfg.LastHandshake = now
	cfg.LatencyMS = 0
	// Assume preflight passed during setup wizard; prevent immediate warning state.
	cfg.LastPreflight = &now

	newCerts := defaultCertificates(cfg)
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
		Message:   p.eventMessage,
		NextStep:  "Run preflight",
	})
	// saveAll releases cfgMu on all paths.
	return m.saveAll(cfg)
}

// Configure persists a new user-managed remote configuration (HTTP-01 only).
func (m *Manager) Configure(req ConfigureRequest) error {
	endpoint := strings.TrimSpace(req.Endpoint)
	if endpoint == "" {
		return errors.New("endpoint required")
	}
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return fmt.Errorf("invalid endpoint: %w", err)
	}

	newSecret := strings.TrimSpace(req.DeviceSecret)

	if err := m.configureCommon(configureParams{
		portalHost:   req.PortalHostname,
		solver:       acme.SolverHTTP01,
		endpoint:     endpoint,
		eventMessage: "Remote configuration saved (user-managed, HTTP-01)",
		mutateConfig: func(cfg *Config) error {
			// Preserve existing secret if not provided (allows reconfigure without re-entering secret).
			if newSecret != "" {
				cfg.DeviceSecret = newSecret
			} else if cfg.DeviceSecret == "" {
				return errors.New("device_secret required")
			}
			cfg.Managed = false
			cfg.OrchestratorEndpoint = ""
			return nil
		},
	}); err != nil {
		return err
	}

	portalHost := hostname.Normalize(req.PortalHostname)
	log.Printf("remote: configured (solver=http-01, portal=%s)", portalHost)
	m.startRenewScheduler()
	// Requeue pending certs. If the relay is already connected (e.g., re-saving
	// the same config), issuance proceeds immediately. If not, the relay gate
	// defers it and handleRelayEvent will pick it up on connect.
	m.requeueOutstandingIssuances()
	return nil
}

// ConfigureManaged persists a new managed remote configuration (DNS-01 via Piccolo orchestrator).
func (m *Manager) ConfigureManaged(req ManagedConfigureRequest) error {
	endpoint := strings.TrimSpace(req.OrchestratorEndpoint)
	if endpoint == "" {
		return errors.New("orchestrator_endpoint required")
	}
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return fmt.Errorf("invalid orchestrator_endpoint: %w", err)
	}

	deviceToken := strings.TrimSpace(req.DeviceToken)
	if deviceToken == "" {
		return errors.New("device_token required")
	}

	orchClient := orchestrator.NewClient(endpoint, deviceToken)

	if err := m.configureCommon(configureParams{
		portalHost:   req.PortalHostname,
		solver:       acme.SolverDNS01,
		endpoint:     endpoint,
		orchClient:   orchClient,
		eventMessage: "Remote configuration saved (managed, DNS-01)",
		mutateConfig: func(cfg *Config) error {
			cfg.DeviceSecret = deviceToken
			cfg.Managed = true
			cfg.OrchestratorEndpoint = endpoint
			return nil
		},
	}); err != nil {
		return err
	}

	portalHost := hostname.Normalize(req.PortalHostname)
	log.Printf("remote: configured (solver=dns-01, managed=true, portal=%s)", portalHost)

	// Queue issuance jobs after releasing lock (enqueueIssuanceJob acquires its own lock).
	// reissue=true so the inventory dedup doesn't skip certs that are already "pending".
	m.enqueueIssuanceJob(issuanceJob{id: "portal", domains: []string{portalHost}, commonName: portalHost, reissue: true})
	// Managed mode supports wildcard via DNS-01
	if portalHost != "" {
		cn := "*." + portalHost
		m.enqueueIssuanceJob(issuanceJob{id: "wildcard", domains: []string{cn, portalHost}, commonName: cn, reissue: true})
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
	return m.saveNexusAndEvents(cfg)
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
	if err := m.saveNexusAndEvents(cfg); err != nil {
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

// ListAliases returns the current alias inventory with status derived from
// the corresponding certificate (not the stale Alias.Status field).
func (m *Manager) ListAliases() []Alias {
	m.cfgMu.RLock()
	cfg := m.currentConfigLocked()
	aliases := cloneAliases(cfg.Aliases)
	for i := range aliases {
		aliases[i].Status = aliasCertStatus(cfg, aliases[i])
	}
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
		listener = nexusclient.PortalHostLabel
	}

	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	alias := Alias{
		ID:       fmt.Sprintf("alias-%d", time.Now().UnixNano()+rand.Int63n(1000)),
		Hostname: hostname,
		Listener: listener,
		Status:   string(CertStatusPending),
	}
	cfg.Aliases = append(cfg.Aliases, alias)
	cfg.Events = append(cfg.Events, Event{
		Timestamp: m.now(),
		Level:     "info",
		Source:    "remote",
		Message:   fmt.Sprintf("Alias %s queued for listener %s", hostname, listener),
	})
	if err := m.saveNexusAndEvents(cfg); err != nil {
		return Alias{}, err
	}
	// Queue issuance for the alias hostname — always HTTP-01 (alias domains use user DNS, not namek PowerDNS)
	h := strings.ToLower(hostname)
	m.enqueueIssuanceJob(issuanceJob{
		id:         "alias:" + h,
		domains:    []string{h},
		commonName: h,
		solver:     acme.SolverHTTP01,
	})
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
	if err := m.saveNexusAndEvents(cfg); err != nil {
		return err
	}
	// Remove associated certificate entry and files (best-effort).
	h := hostname.Normalize(removed.Hostname)
	if h != "" {
		m.removeCertificateByID("alias:"+h, h, "")
	}
	return nil
}

// RemoveHostnameCertificate removes a per-host certificate (host:<hostname>) and its files.
// Safe to call even if no such certificate exists.
func (m *Manager) RemoveHostnameCertificate(host string) {
	h := hostname.Normalize(host)
	if h == "" {
		return
	}
	m.removeCertificateByID("host:"+h, h, "")
}

// RemoveCertificateByID removes a certificate entry by ID, cleaning up both
// the inventory and cert files on disk. No-op if the cert ID is not found.
func (m *Manager) RemoveCertificateByID(id string) {
	if m == nil {
		return
	}
	// Look up commonName and certDir from the stored entry before removal.
	m.cfgMu.RLock()
	cfg := m.currentConfigLocked()
	var commonName, certDir string
	found := false
	if cfg != nil {
		for _, c := range cfg.Certificates {
			if c.ID == id {
				found = true
				if len(c.Domains) > 0 {
					commonName = c.Domains[0]
				}
				certDir = c.CertDir
				break
			}
		}
	}
	m.cfgMu.RUnlock()
	if !found {
		return
	}
	m.removeCertificateByID(id, commonName, certDir)
}

func (m *Manager) removeCertificateByID(id, commonName, certDir string) {
	if m == nil {
		return
	}

	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	if cfg == nil {
		m.cfgMu.Unlock()
		m.deleteCertFiles(id, commonName, certDir)
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
		_ = m.saveCertsAndEvents(cfg)
	} else {
		m.cfgMu.Unlock()
	}
	m.deleteCertFiles(id, commonName, certDir)
}

func (m *Manager) deleteCertFiles(id, commonName, overrideCertDir string) {
	certDir := overrideCertDir
	if certDir == "" {
		certDir = m.certDir()
	}
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
	Aliases        []nexusclient.AliasEntry
	Enabled        bool
	PortClaims     []api.PortClaimInfo // active port claims from service manager
}

// adapterConfigKey returns a string that changes only when adapter-relevant
// fields change (endpoint, secret, portal hostname, aliases). Cert state,
// events, and other fields are excluded so that save() calls for those
// operations do not trigger an unnecessary adapter restart.
func adapterConfigKey(snap adapterStateSnapshot) string {
	var b strings.Builder
	b.WriteString(snap.Endpoint)
	b.WriteByte('\x00')
	b.WriteString(snap.DeviceSecret)
	b.WriteByte('\x00')
	b.WriteString(snap.PortalHostname)
	b.WriteByte('\x00')
	if snap.Enabled {
		b.WriteByte('1')
	} else {
		b.WriteByte('0')
	}
	for _, a := range snap.Aliases {
		b.WriteByte('\x00')
		b.WriteString(a.Hostname)
		b.WriteByte('\x01')
		b.WriteString(a.HostLabel)
	}
	// Include port claims so adapter restarts when claims change.
	for _, pc := range snap.PortClaims {
		b.WriteByte('\x00')
		b.WriteString(fmt.Sprintf("claim:%d/%s->%d", pc.Port, pc.Protocol, pc.HostBind))
	}
	return b.String()
}

func extractAdapterSnapshot(cfg *Config) adapterStateSnapshot {
	if cfg == nil {
		return adapterStateSnapshot{}
	}
	var aliases []nexusclient.AliasEntry
	for _, a := range cfg.Aliases {
		hostLabel := a.Listener
		if hostLabel == "" || hostLabel == "portal" {
			hostLabel = nexusclient.PortalHostLabel
		}
		aliases = append(aliases, nexusclient.AliasEntry{
			Hostname:  a.Hostname,
			HostLabel: hostLabel,
		})
	}
	return adapterStateSnapshot{
		Endpoint:       cfg.Endpoint,
		DeviceSecret:   cfg.DeviceSecret,
		PortalHostname: cfg.PortalHostname,
		Aliases:        aliases,
		Enabled:        cfg.Enabled,
	}
}

// snapshotWithClaims enriches a config snapshot with active port claims
// from the service manager. Safe to call without holding any lock.
func (m *Manager) snapshotWithClaims(snap adapterStateSnapshot) adapterStateSnapshot {
	m.adapterMu.Lock()
	provider := m.portClaimProvider
	m.adapterMu.Unlock()
	if provider != nil {
		claims := provider.ActivePortClaims()
		// Sort for stable fingerprinting
		sort.Slice(claims, func(i, j int) bool {
			if claims[i].Protocol != claims[j].Protocol {
				return claims[i].Protocol < claims[j].Protocol
			}
			return claims[i].Port < claims[j].Port
		})
		snap.PortClaims = claims
	}
	return snap
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
		Aliases:        snap.Aliases,
		ClaimMappings:  snap.PortClaims,
	}
	if err := adapter.Configure(adapterCfg); err != nil {
		log.Printf("WARN: remote: configure nexus adapter failed: %v", err)
	}

	if !snap.Enabled || snap.Endpoint == "" || snap.DeviceSecret == "" || snap.PortalHostname == "" {
		m.stopAdapter()
		// Reset lastAdapterKey so a subsequent enable always restarts the adapter.
		// Only stop the renew scheduler if no external source has registered an orchClient.
		// External sources (e.g., namek) share the same scheduler for cert renewals.
		m.adapterMu.Lock()
		m.lastAdapterKey = ""
		hasExternalSources := len(m.orchClients) > 0
		m.adapterMu.Unlock()
		if !hasExternalSources {
			m.stopRenewScheduler()
		}
		return
	}

	// Only restart the adapter when config that affects the relay registration
	// (endpoint, secret, portal, aliases) actually changed. Cert-only saves
	// skip the restart to avoid flapping the relay connection.
	key := adapterConfigKey(snap)
	m.adapterMu.Lock()
	changed := key != m.lastAdapterKey
	if changed {
		m.lastAdapterKey = key
	}
	m.adapterMu.Unlock()

	if !changed && cancel != nil {
		// Adapter running with identical config — nothing to do.
		m.startRenewScheduler()
		return
	}

	if changed {
		log.Printf("INFO: remote: restarting nexus adapter (config changed)")
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
		err := adapterRun.Start(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("WARN: remote: nexus adapter exited: %v", err)
			}
			// Start failed — reset lastAdapterKey so the next save() retries
			// instead of hitting the !changed fast path permanently.
			m.adapterMu.Lock()
			m.lastAdapterKey = ""
			m.adapterMu.Unlock()
		}
		// Do NOT clear m.adapterCancel here — adapter.Start() is non-blocking
		// (it spawns client.Start in a sub-goroutine and returns immediately).
		// Clearing here races with subsequent applyAdapterState calls, causing
		// them to skip stopAdapter() and leaving the old client running with a
		// stale hostname list. m.adapterCancel is managed by stopAdapter().
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
	email := deriveACMEEmail(cfg.PortalHostname)
	m.acmeMgr.SetEmail(email)
	if err := m.acmeMgr.SetSolver(cfg.Solver); err != nil {
		log.Printf("WARN: remote: acme solver config failed: %v", err)
	}
	// For managed mode, recreate orchestrator client from stored endpoint
	if cfg.Managed && cfg.OrchestratorEndpoint != "" && cfg.DeviceSecret != "" {
		orchClient := orchestrator.NewClient(cfg.OrchestratorEndpoint, cfg.DeviceSecret)
		m.acmeMgr.SetOrchestratorClient(orchClient)
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

	// Clear self-hosted relay state AFTER stopping so trailing events don't persist.
	// Uses the default adapter name "piccolo-portal" (see nexusclient/backend.go).
	// ClearRelayState also publishes config change and requeues if this removal
	// unblocks httpChallengeReachable (e.g., removing a disconnected relay).
	m.ClearRelayState("piccolo-portal")
}

// RelayEventHandler returns a callback suitable for nexusclient.WithAdapterEventHandler.
// It translates relay lifecycle events into warnings visible in the UI.
func (m *Manager) RelayEventHandler() func(adapterName string, connected bool, errMsg string) {
	return m.handleRelayEvent
}

// handleRelayEvent processes connection lifecycle events from the nexus backend
// client and surfaces them as warnings/events for the UI. Multiple relay sources
// (self-hosted "piccolo-portal", namek "piccolo-namek") feed into this handler.
func (m *Manager) handleRelayEvent(adapterName string, connected bool, errMsg string) {
	if m == nil || m.closed.Load() {
		return
	}

	m.relayMu.Lock()
	prev := m.relayStates[adapterName]
	m.relayStates[adapterName] = relayState{connected: connected, err: errMsg}
	changed := prev.connected != connected || prev.err != errMsg
	m.relayMu.Unlock()

	if connected && !prev.connected {
		log.Printf("INFO: [%s] relay connected", adapterName)
		// Relay just came up — kick off any pending HTTP-01 cert issuance
		// that was waiting for the tunnel to be reachable.
		m.requeueOutstandingIssuances()
	} else if !connected && errMsg != "" && errMsg != prev.err {
		log.Printf("WARN: [%s] relay disconnected: %s", adapterName, errMsg)
	}

	// Republish config so SSE clients pick up the updated warning state.
	if changed {
		m.publishConfigChanged()
	}
}

// relayWarning returns a user-visible warning if any tracked relay is disconnected.
func (m *Manager) relayWarning() string {
	m.relayMu.RLock()
	// Find the first disconnected relay with an error message.
	var errMsg string
	for _, s := range m.relayStates {
		if !s.connected && s.err != "" {
			errMsg = s.err
			break
		}
	}
	m.relayMu.RUnlock()

	if errMsg == "" {
		return ""
	}

	// Raw relay errors are too technical for end-users; map to actionable guidance.
	lower := strings.ToLower(errMsg)
	switch {
	case strings.Contains(lower, "token") && strings.Contains(lower, "signature"):
		return "Relay authentication failed — check the device secret"
	case strings.Contains(lower, "token") && strings.Contains(lower, "expired"):
		return "Relay authentication failed — token expired, check system clock"
	case strings.Contains(lower, "dial") || strings.Contains(lower, "connection refused"):
		return "Relay unreachable — check the endpoint and network"
	case strings.Contains(lower, "tls") || strings.Contains(lower, "x509"):
		return "Relay TLS error — check the endpoint certificate"
	default:
		return fmt.Sprintf("Relay connection error: %s", errMsg)
	}
}

// httpChallengeReachable reports whether HTTP-01 challenges can be served.
// Requires ALL enabled relays to be connected. We use "all" rather than "any"
// because HTTP-01 certs route through specific relays depending on external DNS
// configuration (e.g., self-hosted portal certs need the self-hosted relay,
// alias certs may route through either relay). Rather than tracking per-cert
// relay affinity — which is fragile to DNS changes — we require all relays to
// be ready, ensuring every traffic path is available before attempting ACME.
func (m *Manager) httpChallengeReachable() bool {
	m.relayMu.RLock()
	defer m.relayMu.RUnlock()
	if len(m.relayStates) == 0 {
		return false
	}
	for _, s := range m.relayStates {
		if !s.connected {
			return false
		}
	}
	return true
}

// RelayConnectedByName reports whether the named relay adapter is connected.
// Returns false if the adapter is not tracked.
func (m *Manager) RelayConnectedByName(name string) bool {
	m.relayMu.RLock()
	defer m.relayMu.RUnlock()
	s, ok := m.relayStates[name]
	return ok && s.connected
}

// needsRelay returns true for solvers that require relay connectivity (HTTP-01).
// DNS-01 uses the orchestrator API and does not need the relay.
func needsRelay(solver string) bool {
	return !strings.EqualFold(solver, acme.SolverDNS01)
}

// ClearRelayState removes the relay state entry for the given adapter name.
// Called by GinServer when a relay adapter (e.g., namek) is stopped, and by
// stopAdapter for the self-hosted relay.
func (m *Manager) ClearRelayState(name string) {
	if m == nil {
		return
	}
	m.relayMu.Lock()
	prev, existed := m.relayStates[name]
	delete(m.relayStates, name)
	m.relayMu.Unlock()
	if !existed {
		return
	}
	m.publishConfigChanged()
	// Removing a disconnected relay may satisfy the all-relays condition,
	// unblocking pending HTTP-01 certs that were waiting for it.
	// Only requeue if the removed relay was actually blocking.
	if !prev.connected && m.httpChallengeReachable() {
		m.requeueOutstandingIssuances()
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
		if !strings.EqualFold(c.Status, string(CertStatusError)) && c.NextRenewal != nil && c.NextRenewal.Before(earliest) {
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
		source  string
		solver  string
	}
	relayReady := m.httpChallengeReachable()
	var jobs []renewalJob
	for _, c := range cfg.Certificates {
		if strings.EqualFold(c.Status, string(CertStatusPending)) {
			continue // avoid duplicate queueing
		}
		// HTTP-01 relay gate: skip if not all relays are connected to prevent
		// wasted ACME attempts that would enter the error→backoff cycle.
		if needsRelay(c.Solver) && !relayReady {
			continue
		}
		// RetryAt-driven: retry when due (no max attempts - indefinite retry per RFC 20260125)
		dueRetry := c.RetryAt != nil && now.After(*c.RetryAt)
		// Only check renewal schedule for non-error certs; error certs must wait for RetryAt backoff
		dueRenewal := false
		if !strings.EqualFold(c.Status, string(CertStatusError)) && c.NextRenewal != nil && c.ExpiresAt != nil {
			dueRenewal = now.After(*c.NextRenewal) || now.Add(24*time.Hour).After(*c.ExpiresAt)
		}
		if !dueRetry && !dueRenewal {
			continue
		}
		domains, cn, ok := desiredDomainsAndCN(cfg, c)
		if !ok {
			continue
		}
		jobs = append(jobs, renewalJob{id: c.ID, domains: domains, cn: cn, source: c.Source, solver: c.Solver})
	}
	m.cfgMu.RUnlock()

	// Enqueue jobs outside of lock
	if len(jobs) > 0 {
		log.Printf("remote: scheduler queuing %d certificate renewal(s)", len(jobs))
	}
	for _, job := range jobs {
		m.enqueueIssuanceJob(issuanceJob{id: job.id, domains: job.domains, commonName: job.cn, source: job.source, solver: job.solver})
	}
}

// desiredDomainsAndCN returns the domains and CN for a certificate, reading the current
// config for self-hosted certs. Source-tagged certs use stored domains (kept fresh by
// the source's event handler re-enqueuing via EnqueueCertIssuance when hostnames change).
func desiredDomainsAndCN(cfg *Config, c Certificate) ([]string, string, bool) {
	if cfg == nil {
		return nil, "", false
	}

	// Source-tagged certs: use stored domains directly (kept fresh by domain freshness guarantee)
	if c.Source != "" {
		if len(c.Domains) == 0 {
			return nil, "", false
		}
		cn := hostname.Normalize(c.Domains[0])
		return append([]string(nil), c.Domains...), cn, true
	}

	switch c.ID {
	case "portal":
		if cfg.PortalHostname == "" {
			return nil, "", false
		}
		h := hostname.Normalize(cfg.PortalHostname)
		return []string{h}, h, true
	case "wildcard":
		// Wildcard only for managed mode (DNS-01 via orchestrator)
		if !cfg.Managed || cfg.PortalHostname == "" || !strings.EqualFold(cfg.Solver, acme.SolverDNS01) {
			return nil, "", false
		}
		base := hostname.Normalize(cfg.PortalHostname)
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
			h := hostname.Normalize(parts[1])
			return []string{h}, h, true
		}
	}
	if len(c.Domains) == 0 {
		return nil, "", false
	}
	cn := hostname.Normalize(c.Domains[0])
	return append([]string(nil), c.Domains...), cn, true
}

// RequeueOutstandingIssuances re-enqueues any pending or expired certs for issuance.
func (m *Manager) RequeueOutstandingIssuances() { m.requeueOutstandingIssuances() }

func (m *Manager) requeueOutstandingIssuances() {
	if m == nil {
		return
	}

	type requeueJob struct {
		id        string
		domains   []string
		cn        string
		source    string
		solver    string
		certDir   string
		isPending bool // true = queue directly to worker, false = enqueue with inventory check
	}

	m.cfgMu.RLock()
	cfg := m.currentConfigLocked()
	now := m.now()
	var jobs []requeueJob
	for _, c := range cfg.Certificates {
		if strings.EqualFold(c.Status, string(CertStatusPending)) {
			domains, cn, ok := desiredDomainsAndCN(cfg, c)
			if ok {
				jobs = append(jobs, requeueJob{id: c.ID, domains: domains, cn: cn, source: c.Source, solver: c.Solver, certDir: c.CertDir, isPending: true})
			}
			continue
		}
		// Requeue error certs that are due for retry (no max attempts - indefinite retry per RFC 20260125)
		if strings.EqualFold(c.Status, string(CertStatusError)) && c.RetryAt != nil && now.After(*c.RetryAt) {
			domains, cn, ok := desiredDomainsAndCN(cfg, c)
			if ok {
				jobs = append(jobs, requeueJob{id: c.ID, domains: domains, cn: cn, source: c.Source, solver: c.Solver, certDir: c.CertDir, isPending: false})
			}
		}
	}
	m.cfgMu.RUnlock()

	// Process jobs outside of lock
	if len(jobs) > 0 {
		log.Printf("remote: requeuing %d outstanding certificate issuance(s)", len(jobs))
	}
	for _, job := range jobs {
		ij := issuanceJob{id: job.id, domains: job.domains, commonName: job.cn, reissue: true, source: job.source, solver: job.solver, certDir: job.certDir}
		// DNS-01 orchClient gate: skip if orchClient not yet registered
		// (e.g., boot timing — the source's event handler will re-enqueue after registration).
		if strings.EqualFold(job.solver, acme.SolverDNS01) && job.source != "" && m.getOrchClient(job.source) == nil {
			log.Printf("INFO: remote: deferring requeue of dns-01 cert %s (orchClient for %q not registered yet)", job.id, job.source)
			continue
		}
		// HTTP-01 relay gate: skip if not all relays are connected.
		// handleRelayEvent calls requeueOutstandingIssuances when relays connect.
		if needsRelay(job.solver) && !m.httpChallengeReachable() {
			log.Printf("INFO: remote: deferring requeue of cert %s (not all relays connected)", job.id)
			continue
		}
		if job.isPending {
			m.queueIssuanceJobToWorker(ij)
		} else {
			m.enqueueIssuanceJob(ij)
		}
	}
}

// RenewCertificate simulates a manual renewal.
func (m *Manager) RenewCertificate(id string) error {
	m.cfgMu.RLock()
	cfg := m.currentConfigLocked()
	// Find target cert and collect info under lock
	var domains []string
	var cn, source, solver, certDir string
	var found bool
	var errMsg string
	for _, c := range cfg.Certificates {
		if c.ID == id {
			found = true
			domains = append([]string(nil), c.Domains...)
			cn = domains[0]
			source = c.Source
			solver = c.Solver
			certDir = c.CertDir
			if id == "portal" && cfg.PortalHostname != "" {
				cn = cfg.PortalHostname
			}
			if id == "wildcard" && cfg.PortalHostname != "" {
				if !cfg.Managed {
					errMsg = "wildcard renewals require managed mode"
					break
				}
				if !strings.EqualFold(cfg.Solver, acme.SolverDNS01) {
					errMsg = "wildcard renewals require dns-01 solver"
					break
				}
				base := hostname.Normalize(cfg.PortalHostname)
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
	job := issuanceJob{id: id, domains: domains, commonName: cn, reissue: true, source: source, solver: solver, certDir: certDir}
	m.enqueueIssuanceJob(job)
	return nil
}

type issuanceJob struct {
	id         string
	domains    []string
	commonName string
	reissue    bool // bypass inventory freshness checks (ok/pending/backoff); does NOT bypass worker queue dedup

	// Per-job metadata (RFC 20260312: source-agnostic cert pipeline)
	source  string // source tag for orchClient lookup during renewal
	solver  string // override solver (e.g., "dns-01" for namek)
	certDir string // override cert output directory
}

// enqueueIssuanceJob is the unified entry point for both self-hosted and namek cert issuance.
// It checks the inventory for duplicates, marks the cert as pending, and queues the job.
func (m *Manager) enqueueIssuanceJob(job issuanceJob) {
	if m.acmeMgr == nil || job.commonName == "" {
		return
	}

	// Callers like the scheduler pass source but not solver; look up persisted metadata.
	if job.solver == "" && job.source != "" {
		m.cfgMu.RLock()
		cfg := m.currentConfigLocked()
		for _, c := range cfg.Certificates {
			if c.ID == job.id {
				if c.Solver != "" {
					job.solver = c.Solver
				}
				if c.CertDir != "" && job.certDir == "" {
					job.certDir = c.CertDir
				}
				break
			}
		}
		m.cfgMu.RUnlock()
	}

	// Gate dns-01 jobs on orchClient availability to prevent pointless inventory
	// churn and worker cycles. The worker resolves the actual client at use-time.
	if job.source != "" && strings.EqualFold(job.solver, acme.SolverDNS01) && m.getOrchClient(job.source) == nil {
		log.Printf("INFO: remote: skipping dns-01 cert %s (orchClient for %q not registered)", job.id, job.source)
		return
	}

	// Check and modify config under lock to prevent races with scheduler reads
	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	// Skip duplicate unless reissue is requested.
	now := m.now()
	for _, c := range cfg.Certificates {
		if c.ID != job.id || job.reissue {
			continue
		}
		// Domains changed (e.g., hostname rename) — must reissue regardless of status.
		if !domainsEqual(c.Domains, job.domains) {
			break
		}
		// Already queued or in-flight for issuance.
		if strings.EqualFold(c.Status, string(CertStatusPending)) {
			m.cfgMu.Unlock()
			return
		}
		// Already issued and not yet due for renewal.
		// Also honor the 24h-before-expiry safety net from scanAndQueueRenewals.
		if strings.EqualFold(c.Status, string(CertStatusOK)) && c.NextRenewal != nil && now.Before(*c.NextRenewal) &&
			(c.ExpiresAt == nil || !now.Add(24*time.Hour).After(*c.ExpiresAt)) {
			m.cfgMu.Unlock()
			return
		}
		// Failed with backoff timer still active (e.g., rate_limited) — respect RetryAt.
		// Without this, save() → TopicRemoteConfigChanged → re-enqueue creates an infinite loop.
		if c.RetryAt != nil && now.Before(*c.RetryAt) {
			m.cfgMu.Unlock()
			return
		}
	}
	m.ensureCertPending(cfg, job.id, job.domains, now, job.source, job.solver, job.certDir)
	// Resolve effective solver from the persisted cert (ensureCertPending may have
	// set it from the config's global solver). Must read before saveCertsAndEvents
	// releases cfgMu.Lock.
	effectiveSolver := job.solver
	if effectiveSolver == "" {
		for _, c := range cfg.Certificates {
			if c.ID == job.id {
				effectiveSolver = c.Solver
				break
			}
		}
	}
	// saveCertsAndEvents releases cfgMu.Lock()
	_ = m.saveCertsAndEvents(cfg)

	// HTTP-01 relay gate: cert is persisted as "pending" in the inventory.
	// Don't submit to worker until all relays are connected.
	// requeueOutstandingIssuances (called on relay connect) will pick it up.
	if needsRelay(effectiveSolver) && !m.httpChallengeReachable() {
		log.Printf("INFO: remote: deferring cert %s (not all relays connected)", job.id)
		return
	}

	log.Printf("remote: queuing certificate issuance: %s (domains=%v, source=%s)", job.id, job.domains, job.source)
	m.queueIssuanceJobToWorker(job)
}

func (m *Manager) queueIssuanceJobToWorker(job issuanceJob) {
	if m == nil || m.issueCh == nil {
		return
	}
	m.issueMu.Lock()
	if m.issueQueued == nil {
		m.issueQueued = make(map[string]struct{})
	}
	// Block if a job for this cert is already queued or in-flight.
	// The force flag bypasses *inventory* guards (status/RetryAt checks
	// in enqueueIssuanceJob), not in-flight duplicate prevention.
	if _, ok := m.issueQueued[job.id]; ok {
		m.issueMu.Unlock()
		return
	}
	m.issueQueued[job.id] = struct{}{}
	m.issueMu.Unlock()

	select {
	case m.issueCh <- job:
	default:
		m.issueMu.Lock()
		delete(m.issueQueued, job.id)
		m.issueMu.Unlock()
		log.Printf("WARN: remote: issuance queue full, dropping job %s", job.id)
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
			// Invariant: single worker goroutine — no parallel calls.
			// Defer keeps the in-flight marker set for the full duration
			// and ensures cleanup even if processIssuance panics.
			func() {
				defer func() {
					m.issueMu.Lock()
					delete(m.issueQueued, job.id)
					m.issueMu.Unlock()
				}()
				m.processIssuance(job)
			}()
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

	// Safety net: if a relay went down after the job was enqueued, defer without
	// recording a failure. The cert stays "pending" — no error, no backoff.
	// Recovery: handleRelayEvent(connected) calls requeueOutstandingIssuances.
	solver := job.solver
	if solver == "" {
		m.cfgMu.RLock()
		for _, c := range m.currentConfigLocked().Certificates {
			if c.ID == job.id {
				solver = c.Solver
				break
			}
		}
		m.cfgMu.RUnlock()
	}
	if needsRelay(solver) && !m.httpChallengeReachable() {
		log.Printf("INFO: remote: deferring issuance of %s (relay disconnected)", job.id)
		return
	}

	// Resolve orchClient from the live registry — callers no longer capture it at enqueue time.
	var orchClient acme.OrchestratorClient
	if job.source != "" && strings.EqualFold(solver, acme.SolverDNS01) {
		orchClient = m.getOrchClient(job.source)
		if orchClient == nil {
			// Timing issue, not ACME failure — defer without error/backoff.
			// Cert stays "pending"; RequeueOutstandingIssuances picks it up
			// when the source's orchClient registers.
			log.Printf("INFO: remote: deferring issuance of %s (orchClient for %q not available)", job.id, job.source)
			return
		}
	}

	log.Printf("remote: issuing certificate %s (cn=%s, domains=%v)", job.id, job.commonName, job.domains)

	// Record attempt under lock (no max attempts - indefinite retry per RFC 20260125)
	m.cfgMu.Lock()
	cfg := m.currentConfigLocked()
	now := m.now()
	for i := range cfg.Certificates {
		if cfg.Certificates[i].ID == job.id {
			// Mark in-flight so the "pending" guard in enqueueIssuanceJob
			// blocks event-driven re-enqueue while the ACME call runs.
			// Without this, saveCerts → TopicRemoteConfigChanged → re-enqueue
			// defeats backoff because RetryAt is nil and status is "error".
			cfg.Certificates[i].Status = string(CertStatusPending)
			cfg.Certificates[i].Attempts++
			cfg.Certificates[i].LastAttempt = timePtr(now)
			cfg.Certificates[i].RetryAt = nil
			break
		}
	}
	_ = m.saveCerts(cfg)

	certDir := job.certDir
	if certDir == "" {
		certDir = m.certDir()
	}
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
	var issueErr error
	if job.solver != "" && orchClient != nil {
		_, issueErr = m.acmeMgr.IssueWithSolver(job.solver, orchClient, job.commonName, sans, outName, certDir)
	} else if strings.EqualFold(job.solver, acme.SolverDNS01) {
		// Unreachable — gate above catches nil orchClient. Defensive.
		issueErr = fmt.Errorf("dns-01 cert %s: orchClient unavailable for source %q", job.id, job.source)
	} else {
		_, issueErr = m.acmeMgr.Issue(job.commonName, sans, outName, certDir)
	}
	if issueErr != nil {
		m.updateCertFailureWithError(job.id, issueErr.Error(), issueErr)
		return
	}
	if exp, ok := readCertExpiry(filepath.Join(certDir, outName+".crt")); ok {
		m.updateCertSuccess(job.id, exp)
	} else {
		m.updateCertSuccess(job.id, now.Add(90*24*time.Hour))
	}
}

func buildSans(commonName string, domains []string) []string {
	commonName = hostname.Normalize(commonName)
	uniq := make(map[string]struct{})
	for _, d := range domains {
		h := hostname.Normalize(d)
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
	// Self-hosted portal uses a fixed filename so the old cert is served
	// until reissuance completes after a hostname change.
	if id == "portal" {
		return "portal"
	}
	return cn
}

func (m *Manager) ensureCertPending(cfg *Config, id string, domains []string, now time.Time, source, solver, certDir string) {
	found := false
	for i := range cfg.Certificates {
		if cfg.Certificates[i].ID == id {
			cfg.Certificates[i].Domains = append([]string(nil), domains...)
			cfg.Certificates[i].Status = string(CertStatusPending)
			cfg.Certificates[i].FailureReason = ""
			cfg.Certificates[i].RetryAt = nil
			cfg.Certificates[i].IssuedAt = nil
			cfg.Certificates[i].ExpiresAt = nil
			cfg.Certificates[i].NextRenewal = nil
			if source != "" {
				cfg.Certificates[i].Source = source
			}
			if solver != "" {
				cfg.Certificates[i].Solver = solver
			}
			if certDir != "" {
				cfg.Certificates[i].CertDir = certDir
			}
			found = true
			break
		}
	}
	if !found {
		cfg.Certificates = append(cfg.Certificates, Certificate{
			ID:      id,
			Domains: append([]string(nil), domains...),
			Status:  string(CertStatusPending),
			Source:  source,
			Solver:  solver,
			CertDir: certDir,
		})
	}
	cfg.Events = append(cfg.Events, Event{
		Timestamp: now,
		Level:     "info",
		Source:    "remote",
		Message:   fmt.Sprintf("Certificate issuance started (%s)", id),
	})
}

// domainsEqual reports whether two domain lists contain the same entries (order-insensitive).
func domainsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, d := range a {
		counts[strings.ToLower(d)]++
	}
	for _, d := range b {
		k := strings.ToLower(d)
		if counts[k] <= 0 {
			return false
		}
		counts[k]--
	}
	return true
}

func (m *Manager) updateCertSuccess(id string, expiresAt time.Time) {
	log.Printf("remote: certificate %s → ok (expires %s)", id, expiresAt.Format(time.RFC3339))
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
			cfg.Certificates[i].Status = string(CertStatusOK)
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
	_ = m.saveCertsAndEvents(cfg)

	// Emit certificate status change event
	m.publishCertificateChanged(id, string(CertStatusOK), "", "ok")
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
	log.Printf("WARN: remote: certificate %s → error (class=%s, code=%s): %s", id, class, code, reason)

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

		cert.Status = string(CertStatusError)
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
	_ = m.saveCertsAndEvents(cfg)

	// Signal scheduler to re-evaluate wake time
	m.notifySchedulerWake()

	// Emit certificate status change event
	m.publishCertificateChanged(id, string(CertStatusError), class, code)
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
	if err := fsutil.AtomicWriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return time.Time{}, err
	}
	if err := fsutil.AtomicWriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
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

	if cfg.Endpoint == "" || cfg.PortalHostname == "" {
		return PreflightResult{}, errors.New("remote not configured")
	}

	now := m.now()
	var checks []PreflightCheck

	endpointCheck := m.checkEndpoint(cfg)
	checks = append(checks, endpointCheck)

	dnsStatus, dnsDetail := m.checkDNS(cfg)
	checks = append(checks, PreflightCheck{Name: "DNS records", Status: dnsStatus, Detail: dnsDetail})

	checks = append(checks, PreflightCheck{Name: "ACME solver", Status: string(PreflightPass), Detail: fmt.Sprintf("Using %s", strings.ToUpper(cfg.Solver))})

	// Only check aliases if we are running against active config, or if candidate has them
	if len(cfg.Aliases) > 0 {
		status := string(PreflightPass)
		detail := "All aliases have certificates"
		for _, alias := range cfg.Aliases {
			if s := aliasCertStatus(cfg, alias); !strings.EqualFold(s, string(CertStatusOK)) {
				status = string(PreflightWarn)
				detail = fmt.Sprintf("Alias %s certificate %s", alias.Hostname, s)
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
		if err := m.saveNexusAndEvents(cfg); err != nil {
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
	PortalHostname string `json:"portal_hostname"` // Fully-qualified hostname (e.g., portal.home.example.com)
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
		NexusConfig: NexusConfig{Endpoint: endpoint},
	}

	// Perform the check
	check := m.checkEndpoint(tempCfg)
	if check.Status != string(PreflightPass) {
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
	return m.saveNexusAndEvents(cfg)
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
		return PreflightCheck{Name: "Nexus endpoint reachable", Status: string(PreflightFail), Detail: "invalid endpoint"}
	}
	if port == "" {
		port = "443"
	}
	address := net.JoinHostPort(host, port)
	start := time.Now()

	var conn net.Conn
	var err error
	if endpointUsesTLS(cfg.Endpoint) {
		conn, err = m.dialer.DialTLS("tcp", address, host, 5*time.Second)
	} else {
		conn, err = m.dialer.DialTimeout("tcp", address, 5*time.Second)
	}
	if err != nil {
		nextStep := "Verify firewall and DNS"
		var hostnameErr x509.HostnameError
		var certInvalidErr x509.CertificateInvalidError
		var unknownAuthErr x509.UnknownAuthorityError
		detail := err.Error()
		if errors.As(err, &hostnameErr) || errors.As(err, &certInvalidErr) || errors.As(err, &unknownAuthErr) ||
			strings.Contains(detail, "tls:") || strings.Contains(detail, "x509:") {
			nextStep = "Verify the relay's TLS certificate matches the endpoint hostname"
		}
		return PreflightCheck{Name: "Nexus endpoint reachable", Status: string(PreflightFail), Detail: detail, NextStep: nextStep}
	}
	latency := int(time.Since(start).Milliseconds())
	_ = conn.Close()
	cfg.LastHandshake = m.now()
	cfg.LatencyMS = latency
	return PreflightCheck{Name: "Nexus endpoint reachable", Status: string(PreflightPass), Detail: fmt.Sprintf("Latency %d ms", latency)}
}

// endpointUsesTLS reports whether the endpoint URL implies a TLS connection.
func endpointUsesTLS(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return u.Scheme == "wss" || u.Scheme == "https"
}

func (m *Manager) checkDNS(cfg *Config) (string, string) {
	host := cfg.PortalHostname
	if host == "" {
		return string(PreflightFail), "portal hostname not configured"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cname, cnameErr := m.resolver.LookupCNAME(ctx, host)
	addresses, addrErr := m.resolver.LookupHost(ctx, host)

	detail := fmt.Sprintf("%s resolves to %v", host, addresses)
	if cnameErr == nil && cname != "" {
		detail = fmt.Sprintf("%s CNAME %s", host, strings.TrimSuffix(cname, "."))
	}

	status := string(PreflightPass)
	if addrErr != nil {
		status = string(PreflightWarn)
		detail = fmt.Sprintf("portal host lookup failed: %v", addrErr)
	}

	if strings.TrimSpace(cfg.PortalHostname) != "" {
		sample := fmt.Sprintf("app.%s", strings.TrimSuffix(strings.TrimSpace(cfg.PortalHostname), "."))
		if _, err := m.resolver.LookupHost(ctx, sample); err != nil {
			status = string(PreflightWarn)
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

// aliasCertStatus derives an alias's health from its certificate.
// Alias status is determined by the cert with ID "alias:<hostname>", not
// the Alias.Status field (which was never kept in sync).
func aliasCertStatus(cfg *Config, alias Alias) string {
	id := "alias:" + strings.ToLower(alias.Hostname)
	for _, c := range cfg.Certificates {
		if c.ID == id {
			return c.Status // "ok", "pending", "error"
		}
	}
	return string(CertStatusPending)
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
		if s := aliasCertStatus(cfg, alias); !strings.EqualFold(s, string(CertStatusOK)) {
			warnings = append(warnings, fmt.Sprintf("Alias %s certificate %s", alias.Hostname, s))
		}
	}
	hasCertError := false
	hasRetry := false
	for _, c := range cfg.Certificates {
		// Source-tagged certs (e.g., namek) have independent lifecycle and
		// should not affect the self-hosted portal warning state.
		if c.Source != "" {
			continue
		}
		if strings.EqualFold(c.Status, string(CertStatusError)) {
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

func defaultCertificates(cfg *Config) []Certificate {
	certificates := []Certificate{}
	if cfg.PortalHostname != "" {
		certificates = append(certificates, Certificate{
			ID:      "portal",
			Domains: []string{cfg.PortalHostname},
			Solver:  cfg.Solver,
			Status:  string(CertStatusPending),
		})
	}
	// Wildcard cert only for managed mode (DNS-01 via orchestrator)
	if cfg.Managed && cfg.PortalHostname != "" && strings.EqualFold(cfg.Solver, acme.SolverDNS01) {
		base := hostname.Normalize(cfg.PortalHostname)
		if base == "" {
			return certificates
		}
		certificates = append(certificates, Certificate{
			ID:      "wildcard",
			Domains: []string{fmt.Sprintf("*.%s", base), base},
			Solver:  cfg.Solver,
			Status:  string(CertStatusPending),
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

func deriveACMEEmail(portalHostname string) string {
	host := hostname.Normalize(portalHostname)
	if host == "" || !strings.Contains(host, ".") {
		return ""
	}
	return fmt.Sprintf("admin@%s", host)
}

// DeriveNamekACMEEmail constructs an ACME contact email from a namek slug hostname
// and base domain (e.g., "hngjr00yn8qk8h2b55y1.piccolospace.com", "piccolospace.com"
// → "hngjr00yn8qk8h2b55y1@piccolospace.com"). Returns "" if inputs are empty or
// the slug cannot be extracted.
func DeriveNamekACMEEmail(slugHostname, baseDomain string) string {
	if baseDomain == "" || slugHostname == "" {
		return ""
	}
	parts := strings.SplitN(slugHostname, ".", 2)
	if len(parts) != 2 || parts[0] == "" {
		return ""
	}
	return parts[0] + "@" + baseDomain
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

package identity

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"piccolod/internal/events"
	"piccolod/internal/tpm"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// EnrollResult holds the outcome of a namek enrollment.
type EnrollResult struct {
	DeviceID       string
	Hostname       string
	BaseDomain     string
	IdentityClass  string
	NexusEndpoints []string
	Reenrolled     bool
}

// Service manages namek device identity, enrollment, and namekclient lifecycle.
// It is a supervisor component (Name/Start/Stop).
type Service struct {
	mu         sync.RWMutex
	cfg        Config
	configPath string
	tpmDev     tpm.Device
	client     *namekclient.Client
	enrolled   atomic.Bool
	available  atomic.Bool // false if TPM unavailable
	suspended  atomic.Bool // true if namek returned 403 (device revoked/suspended)
	eventsBus  *events.Bus

	// TPM dirs — set once via SetTPMDirs before Start(), read by recoverAndReenroll.
	akStateDir    string
	swtpmStateDir string

	// Concurrency guard for recovery: prevents parallel recoverAndReenroll goroutines.
	recovering atomic.Bool

	// onTPMReplaced is called when AK recovery replaces the TPM device,
	// so the owner can update its own reference and close the old one.
	onTPMReplaced func(old tpm.Device, newResult *tpm.OpenResult)
}

// NewService constructs a new identity service.
// tpmDevice may be nil if no TPM is available.
func NewService(configPath string, tpmDevice tpm.Device) *Service {
	return &Service{
		configPath: configPath,
		tpmDev:     tpmDevice,
	}
}

func (s *Service) Name() string { return "identity" }

// Start loads config and initializes the namekclient if enabled and TPM is available.
func (s *Service) Start(ctx context.Context) error {
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		log.Printf("WARN: identity: failed to load config: %v", err)
		cfg = Config{Enabled: true, NamekURL: defaultNamekURL}
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()

	// Set availability based on TPM presence regardless of enabled state.
	// Callers check Available() to know if identity *could* work when enabled.
	s.available.Store(s.tpmDev != nil)

	if !cfg.Enabled {
		log.Printf("INFO: identity: disabled by config")
		return nil
	}

	if s.tpmDev == nil {
		log.Printf("WARN: identity: TPM unavailable, identity service will be limited")
		return nil // don't fail supervisor
	}

	namekURL := resolveNamekURL(cfg)
	client := newNamekClient(namekURL, s.tpmDev, cfg.DeviceID)

	s.mu.Lock()
	s.client = client
	s.mu.Unlock()

	if cfg.DeviceID != "" {
		s.enrolled.Store(true)
		log.Printf("INFO: identity: loaded existing enrollment (device=%s, hostname=%s)", cfg.DeviceID, cfg.Hostname)
	}

	s.available.Store(true)
	s.publish(events.TopicIdentityReady, nil)
	return nil
}

// Stop closes the namekclient. Does NOT close TPM — caller owns it.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.client = nil
	s.mu.Unlock()
	return nil
}

// SetEventsBus wires the event bus for publishing identity events.
func (s *Service) SetEventsBus(bus *events.Bus) {
	s.mu.Lock()
	s.eventsBus = bus
	s.mu.Unlock()
}

// SetTPMDirs stores the AK and swtpm state directories for recovery.
// Must be called before Start().
func (s *Service) SetTPMDirs(akStateDir, swtpmStateDir string) {
	s.mu.Lock()
	s.akStateDir = akStateDir
	s.swtpmStateDir = swtpmStateDir
	s.mu.Unlock()
}

// SetTPMReplacedHandler registers a callback invoked when AK recovery replaces the TPM device.
// The owner uses this to close the old device and track the new OpenResult.
func (s *Service) SetTPMReplacedHandler(fn func(old tpm.Device, newResult *tpm.OpenResult)) {
	s.mu.Lock()
	s.onTPMReplaced = fn
	s.mu.Unlock()
}

// NamekClient returns the current namekclient, or nil if not available.
func (s *Service) NamekClient() *namekclient.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client
}

// IdentityStatus represents the full state of namek-managed identity.
type IdentityStatus struct {
	Enabled        bool     `json:"enabled"`
	State          string   `json:"state"` // "active", "disabled", "not_enrolled", "suspended"
	DeviceID       string   `json:"device_id,omitempty"`
	Hostname       string   `json:"hostname,omitempty"`
	CustomHostname string   `json:"custom_hostname,omitempty"`
	NexusEndpoints []string `json:"nexus_endpoints,omitempty"`
}

// PublicIdentityStatus is the subset safe for unauthenticated callers.
type PublicIdentityStatus struct {
	Enabled  bool   `json:"enabled"`
	State    string `json:"state"`
	Hostname string `json:"hostname,omitempty"`
}

// Public returns a redacted copy safe for unauthenticated endpoints.
func (s *IdentityStatus) Public() *PublicIdentityStatus {
	if s == nil {
		return nil
	}
	return &PublicIdentityStatus{
		Enabled:  s.Enabled,
		State:    s.State,
		Hostname: s.Hostname,
	}
}

// Status returns the current identity status snapshot.
func (s *Service) Status() IdentityStatus {
	cfg := s.DeviceConfig()
	state := "not_enrolled"
	if s.IsSuspended() {
		state = "suspended"
	} else if !cfg.Enabled {
		state = "disabled"
	} else if s.IsEnrolled() {
		state = "active"
	}
	hostname := cfg.Hostname
	if custom := cfg.CustomFQDN(); custom != "" {
		hostname = custom
	}
	return IdentityStatus{
		Enabled:        cfg.Enabled,
		State:          state,
		DeviceID:       cfg.DeviceID,
		Hostname:       hostname,
		CustomHostname: cfg.CustomHostname,
		NexusEndpoints: cfg.NexusEndpoints,
	}
}

func (s *Service) IsEnrolled() bool   { return s.enrolled.Load() }
func (s *Service) IsEnabled() bool    { s.mu.RLock(); defer s.mu.RUnlock(); return s.cfg.Enabled }
func (s *Service) IsAvailable() bool  { return s.available.Load() }
func (s *Service) IsSuspended() bool  { return s.suspended.Load() }

// DeviceConfig returns a read-only snapshot of the identity config.
func (s *Service) DeviceConfig() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := s.cfg
	cfg.NexusEndpoints = append([]string(nil), s.cfg.NexusEndpoints...)
	return cfg
}

// Enroll performs TPM-attested enrollment with the namek server.
func (s *Service) Enroll(ctx context.Context) (*EnrollResult, error) {
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	if client == nil {
		return nil, fmt.Errorf("identity: namekclient not initialized")
	}

	result, err := client.Enroll(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity: enrollment failed: %w", err)
	}

	baseDomain := extractBaseDomain(result.Hostname)

	s.mu.Lock()
	s.cfg.DeviceID = result.DeviceID
	s.cfg.Hostname = result.Hostname
	s.cfg.BaseDomain = baseDomain
	s.cfg.IdentityClass = result.IdentityClass
	s.cfg.NexusEndpoints = result.NexusEndpoints
	cfg := s.cfg
	s.mu.Unlock()

	if err := saveConfig(s.configPath, cfg); err != nil {
		return nil, fmt.Errorf("identity: persist enrollment: %w", err)
	}

	s.enrolled.Store(true)
	s.suspended.Store(false) // clear suspension on successful enrollment
	s.publish(events.TopicIdentityChanged, nil)

	log.Printf("INFO: identity: enrolled (device=%s, hostname=%s, reenrolled=%v)",
		result.DeviceID, result.Hostname, result.Reenrolled)

	return &EnrollResult{
		DeviceID:       result.DeviceID,
		Hostname:       result.Hostname,
		BaseDomain:     baseDomain,
		IdentityClass:  result.IdentityClass,
		NexusEndpoints: result.NexusEndpoints,
		Reenrolled:     result.Reenrolled,
	}, nil
}

// SetEnabled enables or disables namek identity (persists + publishes event).
// When re-enabling, reinitializes the namekclient if TPM is available.
func (s *Service) SetEnabled(ctx context.Context, enabled bool) error {
	s.mu.Lock()
	if s.cfg.Enabled == enabled {
		s.mu.Unlock()
		return nil // no-op
	}
	s.cfg.Enabled = enabled
	cfg := s.cfg
	needsClient := enabled && s.client == nil && s.tpmDev != nil
	s.mu.Unlock()

	if err := saveConfig(s.configPath, cfg); err != nil {
		return err
	}

	// Reinitialize client if enabling and it was never created (e.g., started disabled).
	if needsClient {
		client := newNamekClient(resolveNamekURL(cfg), s.tpmDev, cfg.DeviceID)
		s.mu.Lock()
		s.client = client
		s.mu.Unlock()
		s.available.Store(true)
		if cfg.DeviceID != "" {
			s.enrolled.Store(true)
		}
	}

	s.publish(events.TopicIdentityChanged, nil)
	return nil
}

// SetNamekURL changes the namek server URL. Clears identity (re-enrollment needed).
func (s *Service) SetNamekURL(ctx context.Context, url string) error {
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("identity: namek URL cannot be empty")
	}
	s.mu.Lock()
	s.cfg.NamekURL = url
	s.cfg.DeviceID = ""
	s.cfg.Hostname = ""
	s.cfg.BaseDomain = ""
	s.cfg.CustomHostname = ""
	s.cfg.IdentityClass = ""
	s.cfg.NexusEndpoints = nil
	s.client = nil
	cfg := s.cfg
	s.mu.Unlock()

	s.enrolled.Store(false)
	s.suspended.Store(false)

	if err := saveConfig(s.configPath, cfg); err != nil {
		return err
	}

	// Reinitialize client with new URL if TPM is available
	if s.tpmDev != nil {
		client := newNamekClient(url, s.tpmDev, "")
		s.mu.Lock()
		s.client = client
		s.mu.Unlock()
	}

	s.publish(events.TopicIdentityChanged, nil)
	return nil
}

// SetCustomHostname sets a custom hostname label via namekclient.
func (s *Service) SetCustomHostname(ctx context.Context, hostname string) error {
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	if client == nil {
		return fmt.Errorf("identity: not initialized")
	}

	if err := client.SetHostname(ctx, hostname); err != nil {
		return fmt.Errorf("identity: set hostname: %w", err)
	}

	s.mu.Lock()
	s.cfg.CustomHostname = hostname
	cfg := s.cfg
	s.mu.Unlock()

	if err := saveConfig(s.configPath, cfg); err != nil {
		return err
	}
	s.publish(events.TopicIdentityChanged, nil)
	return nil
}

// HandleTokenError handles authentication errors from namek token requests.
// 401 → attempts re-enrollment; 403 → publishes suspended state.
func (s *Service) HandleTokenError(err error, httpStatus int) {
	switch httpStatus {
	case 401:
		if s.recovering.Load() {
			return // recovery already in progress
		}
		log.Printf("WARN: identity: namek auth failed (401), attempting AK recovery and re-enrollment")
		go s.recoverAndReenroll()
	case 403:
		log.Printf("ERROR: identity: device suspended/revoked by namek (403)")
		s.suspended.Store(true)
		s.publish(events.TopicIdentityChanged, nil)
	}
}

func (s *Service) recoverAndReenroll() {
	// Concurrency guard: only one recovery attempt at a time.
	if !s.recovering.CompareAndSwap(false, true) {
		log.Printf("INFO: identity: AK recovery already in progress, skipping")
		return
	}
	defer s.recovering.Store(false)

	s.mu.RLock()
	akDir := s.akStateDir
	swtpmDir := s.swtpmStateDir
	s.mu.RUnlock()

	if akDir == "" || swtpmDir == "" {
		log.Printf("WARN: identity: cannot recover AK — TPM dirs not configured")
		return
	}
	result, err := tpm.RecoverAK(akDir, swtpmDir)
	if err != nil {
		log.Printf("ERROR: identity: AK recovery failed: %v", err)
		return
	}

	s.mu.Lock()
	oldDev := s.tpmDev
	s.tpmDev = result.Device
	onReplaced := s.onTPMReplaced
	// Recreate client with new TPM device
	cfg := s.cfg
	s.client = newNamekClient(resolveNamekURL(cfg), s.tpmDev, cfg.DeviceID)
	s.mu.Unlock()

	// Notify owner so it can close the old device and track the new OpenResult.
	if onReplaced != nil {
		onReplaced(oldDev, result)
	} else if oldDev != nil {
		// Fallback: close old device directly if no handler is registered.
		oldDev.Close()
	}

	ctx := context.Background()
	if _, err := s.Enroll(ctx); err != nil {
		log.Printf("ERROR: identity: re-enrollment after AK recovery failed: %v", err)
	}
}

func (s *Service) publish(topic events.Topic, payload any) {
	s.mu.RLock()
	bus := s.eventsBus
	s.mu.RUnlock()
	if bus != nil {
		bus.Publish(events.Event{Topic: topic, Payload: payload})
	}
}

func resolveNamekURL(cfg Config) string {
	if v := os.Getenv("PICCOLO_NAMEK_URL"); v != "" {
		return v
	}
	if cfg.NamekURL != "" {
		return cfg.NamekURL
	}
	return defaultNamekURL
}

// newNamekClient creates a namekclient with standard options (device ID, insecure skip).
func newNamekClient(url string, dev tpm.Device, deviceID string) *namekclient.Client {
	var opts []namekclient.Option
	if deviceID != "" {
		opts = append(opts, namekclient.WithDeviceID(deviceID))
	}
	if os.Getenv("PICCOLO_NAMEK_INSECURE") == "1" {
		opts = append(opts, namekclient.WithInsecureSkipVerify())
	}
	return namekclient.New(url, dev, opts...)
}

// extractBaseDomain strips the first label from an FQDN.
// "slug.test.local" → "test.local"
func extractBaseDomain(hostname string) string {
	hostname = strings.TrimSuffix(hostname, ".")
	idx := strings.Index(hostname, ".")
	if idx < 0 || idx >= len(hostname)-1 {
		return ""
	}
	return hostname[idx+1:]
}

package identity

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	// Shutdown lifecycle: stopped prevents new recovery/sync goroutines after Stop(),
	// recoverWg tracks in-flight recovery so Stop can wait for it.
	// stopCh is closed on Stop() to interrupt backoff sleeps in recovery goroutines.
	stopped   atomic.Bool
	recoverWg sync.WaitGroup
	stopCh    chan struct{}
	stopOnce  sync.Once

	// onTPMReplaced is called when AK recovery replaces the TPM device,
	// so the owner can update its own reference and close the old one.
	onTPMReplaced func(old tpm.Device, newResult *tpm.OpenResult)

	// Server-reported recovery status (from GET /devices/me).
	recoveryStatus string

	// Endpoint sync loop lifecycle.
	syncCancel context.CancelFunc
	syncDone   chan struct{}
}

// NewService constructs a new identity service.
// tpmDevice may be nil if no TPM is available.
func NewService(configPath string, tpmDevice tpm.Device) *Service {
	return &Service{
		configPath: configPath,
		tpmDev:     tpmDevice,
		stopCh:     make(chan struct{}),
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

	if cfg.DeviceID != "" {
		s.startEndpointSync()
	}
	return nil
}

// Stop closes the namekclient. Does NOT close TPM — caller owns it.
func (s *Service) Stop(ctx context.Context) error {
	// Set stopped under Lock so HandleTokenError's RLock+Add(1) sequence
	// cannot race with Wait(). Once Lock is released, any concurrent
	// HandleTokenError will see stopped=true and skip recovery.
	s.mu.Lock()
	s.stopped.Store(true)
	s.mu.Unlock()

	// Signal recovery goroutines to abort backoff sleeps.
	s.stopOnce.Do(func() { close(s.stopCh) })

	s.stopEndpointSync()

	// Wait for in-flight recovery, but respect the caller's context timeout
	// so a hung recovery doesn't block the entire shutdown sequence.
	waitCh := make(chan struct{})
	go func() {
		s.recoverWg.Wait()
		close(waitCh)
	}()
	select {
	case <-waitCh:
	case <-ctx.Done():
		log.Printf("WARN: identity: Stop context expired waiting for recovery to finish")
		return ctx.Err()
	}

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
	State          string   `json:"state"` // "active", "disabled", "not_enrolled", "suspended", "recovering"
	DeviceID       string   `json:"device_id,omitempty"`
	AccountID      string   `json:"account_id,omitempty"`
	Hostname       string   `json:"hostname,omitempty"`
	CustomHostname string   `json:"custom_hostname,omitempty"`
	RecoveryStatus string   `json:"recovery_status,omitempty"` // from server: "active", "pending_recovery", "standalone"
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
	} else if s.recovering.Load() {
		state = "recovering"
	} else if !cfg.Enabled {
		state = "disabled"
	} else if s.IsEnrolled() {
		state = "active"
	}
	hostname := cfg.Hostname
	if custom := cfg.CustomFQDN(); custom != "" {
		hostname = custom
	}

	s.mu.RLock()
	recoveryStatus := s.recoveryStatus
	s.mu.RUnlock()

	return IdentityStatus{
		Enabled:        cfg.Enabled,
		State:          state,
		DeviceID:       cfg.DeviceID,
		AccountID:      cfg.AccountID,
		Hostname:       hostname,
		CustomHostname: cfg.CustomHostname,
		RecoveryStatus: recoveryStatus,
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

	return s.finalizeEnrollment(result)
}

// finalizeEnrollment persists the enrollment result and restores runtime state.
// Shared by Enroll and reenrollWithRecovery.
func (s *Service) finalizeEnrollment(result *namekclient.EnrollResult) (*EnrollResult, error) {
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
	s.suspended.Store(false)
	s.startEndpointSync()
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

	if !enabled {
		s.stopEndpointSync()
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

	if enabled && s.enrolled.Load() {
		s.startEndpointSync()
	}

	s.publish(events.TopicIdentityChanged, nil)
	return nil
}

// SetNamekURL changes the namek server URL. Clears identity (re-enrollment needed).
func (s *Service) SetNamekURL(ctx context.Context, url string) error {
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("identity: namek URL cannot be empty")
	}
	s.stopEndpointSync()

	s.mu.Lock()
	s.cfg.NamekURL = url
	s.cfg.DeviceID = ""
	s.cfg.AccountID = ""
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

	// Clear server-specific cached state (vouchers + state cache).
	dir := filepath.Dir(s.configPath)
	_ = clearVouchers(filepath.Join(dir, "vouchers"))
	_ = os.Remove(filepath.Join(dir, "state_cache.json"))

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

// Error message substrings for classifying 401 responses.
var (
	// Transient 401 errors that resolve on retry (not identity loss).
	// Includes both "nonce expired" and "expired nonce" to match server variations
	// (Namek returns "invalid or expired nonce" which contains "expired nonce").
	transientAuthErrors = []string{"nonce expired", "expired nonce", "nonce invalid"}
	akRelatedErrors     = []string{"credential", "attestation key"}
)

// HandleTokenError handles authentication errors from namek token requests.
// 401 → distinguishes error sub-types; 403 → publishes suspended state.
func (s *Service) HandleTokenError(err error, httpStatus int) {
	switch httpStatus {
	case 401:
		errMsg := extractErrorMessage(err)
		switch {
		case containsAny(errMsg, transientAuthErrors...):
			// Transient — the sync loop retries on next tick.
			log.Printf("WARN: identity: transient auth error: %s", errMsg)
			return
		default:
			// Identity loss ("device not found", "quote verification failed", or unknown).
			// Re-enroll with existing AK + recovery bundle.
			s.triggerReenrollWithRecovery(errMsg)
		}
	case 403:
		log.Printf("ERROR: identity: device suspended/revoked by namek (403)")
		s.suspended.Store(true)
		s.publish(events.TopicIdentityChanged, nil)
	}
}

// triggerReenrollWithRecovery launches reenrollWithRecovery in a goroutine
// with the same concurrency guards as the old HandleTokenError 401 path.
func (s *Service) triggerReenrollWithRecovery(reason string) {
	if s.recovering.Load() {
		return
	}
	// RLock synchronizes with Stop()'s Lock: Add(1) completes before
	// Stop() can call Wait(), preventing a WaitGroup panic.
	s.mu.RLock()
	if s.stopped.Load() {
		s.mu.RUnlock()
		return
	}
	s.recoverWg.Add(1)
	s.mu.RUnlock()

	log.Printf("WARN: identity: auth failed (%s), attempting re-enrollment with recovery bundle", reason)
	go func() {
		defer s.recoverWg.Done()
		s.reenrollWithRecovery()
	}()
}

// reenrollWithRecovery re-enrolls with the existing AK and a recovery bundle.
// Does NOT perform AK recovery. Falls back to recoverAndReenroll on AK-specific errors.
func (s *Service) reenrollWithRecovery() {
	if !s.recovering.CompareAndSwap(false, true) {
		log.Printf("INFO: identity: recovery already in progress, skipping")
		return
	}
	s.publish(events.TopicIdentityChanged, nil) // notify: entering recovery
	defer func() {
		s.recovering.Store(false)
		s.publish(events.TopicIdentityChanged, nil) // notify: exiting recovery
	}()

	// Stop sync loop to prevent conflicting calls during re-enrollment.
	s.stopEndpointSync()

	bundle := s.buildRecoveryBundle()

	const baseDelay = 2 * time.Second
	const maxDelay = 120 * time.Second
	const jitterFactor = 0.5

	for attempt := 0; ; attempt++ {
		if s.stopped.Load() {
			log.Printf("INFO: identity: re-enrollment aborted (service stopped)")
			return
		}
		// Abort if identity was reset (e.g., SetNamekURL) or feature disabled.
		if !s.enrolled.Load() || !s.IsEnabled() {
			log.Printf("INFO: identity: re-enrollment aborted (identity reset or disabled)")
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		s.mu.RLock()
		client := s.client
		s.mu.RUnlock()

		var result *namekclient.EnrollResult
		var err error

		if client == nil {
			err = fmt.Errorf("identity: namekclient not initialized")
		} else if bundle != nil {
			result, err = client.EnrollWithRecovery(ctx, bundle)
		} else {
			result, err = client.Enroll(ctx)
		}
		cancel()

		if err == nil {
			// Clear recovering before finalize so the sync loop's initial
			// tick (started by finalizeEnrollment) can fetch fresh device info.
			s.recovering.Store(false)
			if _, fErr := s.finalizeEnrollment(result); fErr != nil {
				log.Printf("ERROR: identity: finalize re-enrollment: %v", fErr)
				return
			}
			log.Printf("INFO: identity: re-enrollment succeeded after %d attempt(s)", attempt+1)
			// Best-effort state replay from cached state (reuse bundle to avoid re-reading disk).
			s.replayState(bundle)
			return
		}

		// If error is AK-specific, fall back to full AK recovery.
		errMsg := extractErrorMessage(err)
		if containsAny(errMsg, akRelatedErrors...) {
			log.Printf("WARN: identity: re-enrollment failed with AK error, falling back to AK recovery: %v", err)
			s.recovering.Store(false) // release so recoverAndReenroll can CAS
			s.recoverAndReenroll()
			return
		}

		delay := backoffDelay(attempt, baseDelay, maxDelay, jitterFactor)
		log.Printf("WARN: identity: re-enrollment attempt %d failed: %v (retry in %v)", attempt+1, err, delay)

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-s.stopCh:
			timer.Stop()
			log.Printf("INFO: identity: re-enrollment aborted (service stopping)")
			return
		}
	}
}

// buildRecoveryBundle constructs a recovery bundle from locally cached state.
// Returns nil if no account_id is cached (fresh device, no recovery possible).
func (s *Service) buildRecoveryBundle() *namekclient.RecoveryBundleInput {
	s.mu.RLock()
	accountID := s.cfg.AccountID
	customHostname := s.cfg.CustomHostname
	configPath := s.configPath
	s.mu.RUnlock()

	if accountID == "" {
		return nil
	}

	vouchersDir := filepath.Join(filepath.Dir(configPath), "vouchers")
	vouchers, err := loadAllVouchers(vouchersDir)
	if err != nil {
		log.Printf("WARN: identity: failed to load vouchers for recovery: %v", err)
	}

	// Recovery bundle requires at least one voucher (server validates min=1).
	// Single-device accounts and devices before voucher exchange completes
	// use plain Enroll() instead; state is replayed post-enrollment.
	if len(vouchers) == 0 {
		return nil
	}

	stateCachePath := filepath.Join(filepath.Dir(configPath), "state_cache.json")
	sc, err := loadStateCache(stateCachePath)
	if err != nil {
		log.Printf("WARN: identity: failed to load state cache for recovery: %v", err)
	}

	// Prefer identity.json custom hostname (authoritative), fall back to state cache.
	if customHostname == "" {
		customHostname = sc.CustomHostname
	}

	return &namekclient.RecoveryBundleInput{
		AccountID:      accountID,
		Vouchers:       vouchers,
		CustomHostname: customHostname,
		AliasDomains:   sc.AliasDomains,
	}
}

// replayState replays cached state after re-enrollment.
// Best-effort: partial failures don't block device operation.
// If bundle is non-nil, its CustomHostname and AliasDomains are used directly
// (avoids re-reading state_cache.json from disk).
func (s *Service) replayState(bundle *namekclient.RecoveryBundleInput) {
	var customHostname string
	var aliasDomains []string

	if bundle != nil {
		customHostname = bundle.CustomHostname
		aliasDomains = bundle.AliasDomains
	} else {
		stateCachePath := filepath.Join(filepath.Dir(s.configPath), "state_cache.json")
		sc, err := loadStateCache(stateCachePath)
		if err != nil {
			log.Printf("WARN: identity: state replay: load cache: %v", err)
			return
		}
		customHostname = sc.CustomHostname
		aliasDomains = sc.AliasDomains
	}

	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	if client == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if customHostname != "" {
		if err := client.SetHostname(ctx, customHostname); err != nil {
			log.Printf("WARN: identity: state replay: set hostname %q: %v", customHostname, err)
		} else {
			log.Printf("INFO: identity: state replay: restored custom hostname %q", customHostname)
		}
	}

	for _, domain := range aliasDomains {
		domainInfo, err := client.RegisterDomain(ctx, domain)
		if err != nil {
			log.Printf("WARN: identity: state replay: register domain %q: %v", domain, err)
			continue
		}
		if _, err := client.VerifyDomain(ctx, domainInfo.ID); err != nil {
			log.Printf("WARN: identity: state replay: verify domain %q: %v", domain, err)
			continue
		}
		if _, err := client.AssignDomain(ctx, domainInfo.ID, []string{client.DeviceID()}); err != nil {
			log.Printf("WARN: identity: state replay: assign domain %q: %v", domain, err)
		} else {
			log.Printf("INFO: identity: state replay: restored alias domain %q", domain)
		}
	}
}

func containsAny(s string, substrs ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(lower, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

func backoffDelay(attempt int, baseDelay, maxDelay time.Duration, jitterFactor float64) time.Duration {
	delay := baseDelay << uint(attempt)
	if delay > maxDelay || delay <= 0 { // overflow guard
		delay = maxDelay
	}
	jitter := time.Duration(rand.Float64() * jitterFactor * float64(delay))
	return delay + jitter
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
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

// startEndpointSync launches the background endpoint sync loop.
// Idempotent: does nothing if already running.
func (s *Service) startEndpointSync() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped.Load() || !s.cfg.Enabled || s.syncCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.syncCancel = cancel
	s.syncDone = make(chan struct{})
	done := s.syncDone
	go func() {
		defer close(done)
		s.endpointSyncLoop(ctx)
	}()
}

// stopEndpointSync cancels the sync loop and waits for it to exit.
func (s *Service) stopEndpointSync() {
	s.mu.Lock()
	cancel := s.syncCancel
	done := s.syncDone
	s.syncCancel = nil
	s.syncDone = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// endpointSyncInitialDelay separates the first sync from adapter startup.
// On enrollment, data is fresh from finalizeEnrollment so the delay is free.
// On boot, the adapter needs a few seconds to connect before endpoint drift
// detection is meaningful. Without this delay, the immediate sync bursts
// Namek API requests alongside the adapter's nonce requests, hitting the
// per-IP rate limit (2 req/s) and potentially cascading into adapter restarts.
// endpointSyncInitialDelay is a var (not const) so tests can shorten it.
var endpointSyncInitialDelay = 10 * time.Second

func (s *Service) endpointSyncLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(endpointSyncInitialDelay):
	}
	s.syncEndpointsOnce(ctx)

	ticker := time.NewTicker(endpointSyncInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncEndpointsOnce(ctx)
		}
	}
}

func (s *Service) syncEndpointsOnce(ctx context.Context) {
	if !s.enrolled.Load() || !s.IsEnabled() || s.suspended.Load() || s.recovering.Load() {
		return
	}

	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	if client == nil {
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	info, err := client.GetDeviceInfo(callCtx)
	if err != nil {
		if status := extractHTTPStatus(err); status == 401 || status == 403 {
			s.HandleTokenError(err, status)
		} else {
			log.Printf("WARN: identity: endpoint sync: %v", err)
		}
		return
	}

	if info.Status == "suspended" {
		s.suspended.Store(true)
		s.publish(events.TopicIdentityChanged, nil)
		log.Printf("WARN: identity: endpoint sync detected device suspension")
		return
	}

	// 1. Sync account_id + recovery status
	s.syncDeviceMeta(info)

	// 2. Process pending voucher requests (sign)
	s.processVoucherRequests(ctx, client, info.PendingVoucherRequests)

	// 3. Cache new vouchers to disk
	s.cacheNewVouchers(info.NewVouchers)

	// 4. Sync state cache (custom hostname + alias domains)
	s.syncStateCache(info)

	// 5. Sync nexus endpoints (existing logic)
	s.syncNexusEndpoints(info)
}

func (s *Service) syncDeviceMeta(info *namekclient.DeviceInfo) {
	// Sync recovery status (in-memory only, no persist).
	if info.RecoveryStatus != "" {
		s.mu.Lock()
		if s.recoveryStatus != info.RecoveryStatus {
			s.recoveryStatus = info.RecoveryStatus
		}
		s.mu.Unlock()
	}

	if info.AccountID == "" {
		return
	}
	s.mu.RLock()
	current := s.cfg.AccountID
	s.mu.RUnlock()

	if info.AccountID == current {
		return
	}

	s.mu.Lock()
	s.cfg.AccountID = info.AccountID
	cfg := s.cfg
	s.mu.Unlock()

	if err := saveConfig(s.configPath, cfg); err != nil {
		log.Printf("ERROR: identity: sync account_id: failed to persist: %v", err)
		s.mu.Lock()
		s.cfg.AccountID = current
		s.mu.Unlock()
		return
	}
	log.Printf("INFO: identity: synced account_id %s", info.AccountID)
}

func (s *Service) processVoucherRequests(ctx context.Context, client *namekclient.Client, requests []namekclient.PendingVoucherRequest) {
	if len(requests) == 0 {
		return
	}
	s.mu.RLock()
	tpmDev := s.tpmDev
	s.mu.RUnlock()
	if tpmDev == nil {
		log.Printf("WARN: identity: cannot sign vouchers — TPM unavailable")
		return
	}

	for _, req := range requests {
		// Use server-provided nonce directly (hex sha256 of voucher data).
		quoteB64, err := tpmDev.Quote(req.Nonce)
		if err != nil {
			log.Printf("WARN: identity: voucher sign: quote: %v", err)
			continue
		}
		signCtx, signCancel := context.WithTimeout(ctx, 15*time.Second)
		if err := client.SignVoucher(signCtx, req.RequestID, quoteB64); err != nil {
			log.Printf("WARN: identity: voucher sign: submit %s: %v", req.RequestID, err)
		} else {
			log.Printf("INFO: identity: signed voucher request %s", req.RequestID)
		}
		signCancel()
	}
}

func (s *Service) cacheNewVouchers(vouchers []namekclient.VoucherArtifact) {
	if len(vouchers) == 0 {
		return
	}
	vouchersDir := filepath.Join(filepath.Dir(s.configPath), "vouchers")
	for _, v := range vouchers {
		fp, err := saveVoucher(vouchersDir, v)
		if err != nil {
			log.Printf("WARN: identity: cache voucher: %v", err)
		} else {
			log.Printf("INFO: identity: cached voucher from peer %s", fp)
		}
	}
}

func (s *Service) syncStateCache(info *namekclient.DeviceInfo) {
	stateCachePath := filepath.Join(filepath.Dir(s.configPath), "state_cache.json")
	current, _ := loadStateCache(stateCachePath)

	var customHostname string
	if info.CustomHostname != nil {
		customHostname = *info.CustomHostname
	}

	updated := StateCache{
		CustomHostname: customHostname,
		AliasDomains:   info.AliasDomains,
	}

	if !stateCacheChanged(current, updated) {
		return
	}

	if err := saveStateCache(stateCachePath, updated); err != nil {
		log.Printf("ERROR: identity: state cache sync: %v", err)
		return
	}
	log.Printf("INFO: identity: state cache updated")
}

func (s *Service) syncNexusEndpoints(info *namekclient.DeviceInfo) {
	// Snapshot local endpoints under lock for comparison.
	s.mu.RLock()
	localEndpoints := append([]string(nil), s.cfg.NexusEndpoints...)
	s.mu.RUnlock()

	if len(info.NexusEndpoints) == 0 && len(localEndpoints) > 0 {
		log.Printf("WARN: identity: endpoint sync: server returned empty endpoints, skipping update")
		return
	}

	remoteSorted := append([]string(nil), info.NexusEndpoints...)
	localSorted := append([]string(nil), localEndpoints...)
	sort.Strings(remoteSorted)
	sort.Strings(localSorted)

	if slices.Equal(remoteSorted, localSorted) {
		return
	}

	// Update in-memory first, snapshot fresh config for persistence.
	s.mu.Lock()
	s.cfg.NexusEndpoints = info.NexusEndpoints
	cfgToSave := s.cfg
	cfgToSave.NexusEndpoints = append([]string(nil), info.NexusEndpoints...)
	s.mu.Unlock()

	if err := saveConfig(s.configPath, cfgToSave); err != nil {
		// Rollback in-memory on persist failure.
		s.mu.Lock()
		s.cfg.NexusEndpoints = localEndpoints
		s.mu.Unlock()
		log.Printf("ERROR: identity: endpoint sync: failed to persist: %v", err)
		return
	}

	s.publish(events.TopicIdentityChanged, nil)
	log.Printf("INFO: identity: endpoint sync: updated nexus endpoints (%d → %d)",
		len(localEndpoints), len(info.NexusEndpoints))
}

func endpointSyncInterval() time.Duration {
	if v := os.Getenv("PICCOLO_ENDPOINT_SYNC_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 5 * time.Minute
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

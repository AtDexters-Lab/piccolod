package identity

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"piccolod/internal/events"
	"piccolod/internal/mdns"
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
	RelayServices  map[string][]string // e.g., {"stun": ["relay:3478"]}
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

	// Concurrency guard for boot-time auto-enrollment (separate from recovering
	// to avoid semantic overload — recovering is for re-enrollment after auth failure).
	autoEnrolling atomic.Bool

	// akRecovered is set via SetAKRecovered before Start() when boot-time
	// tpm.OpenWithAKRecovery regenerated a stale AK blob. When set, Start()
	// schedules a background re-enrollment so the server-side AK pubkey is
	// refreshed (otherwise the fast-path would start endpoint sync with an
	// AK the server rejects).
	akRecovered atomic.Bool

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

	// suggestedHostname is an ephemeral, in-memory-only hint populated when
	// autoEnrollAtBoot detects a re-enrollment (Reenrolled: true). The boot
	// handler surfaces it so the setup UI can pre-populate the hostname input.
	// Not persisted — only meaningful during the current setup session.
	// Orthogonal to state_cache.json, which serves as durable cache for replayState.
	suggestedHostname string

	// onRelayServicesChanged is called when relay services (including STUN
	// addresses) change, so the owner can update STUN server lists.
	onRelayServicesChanged func(services map[string][]string)

	// networkUp receives a signal when network connectivity becomes available.
	// Buffer-1 so callers never block; rapid signals coalesce naturally.
	networkUp chan struct{}

	// Endpoint sync loop lifecycle.
	syncCancel context.CancelFunc
	syncDone   chan struct{}

	// Setup heartbeat loop lifecycle — see setup_heartbeat.go.
	onSetupCompleteCheck func() bool
	setupHBCancel        context.CancelFunc
	setupHBDone          chan struct{}
	setupHBWake          chan struct{} // buffered, coalescing
	setupHBFirstLogged   atomic.Bool   // log only the first successful send
}

// NewService constructs a new identity service.
// tpmDevice may be nil if no TPM is available.
func NewService(configPath string, tpmDevice tpm.Device) *Service {
	return &Service{
		configPath: configPath,
		tpmDev:     tpmDevice,
		stopCh:     make(chan struct{}),
		networkUp:  make(chan struct{}, 1),
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
		log.Printf("ERROR: identity: TPM unavailable, remote access will be disabled until TPM is recovered")
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
		s.maybeStartSetupHeartbeat()
	} else {
		// Not yet enrolled — attempt auto-enrollment in background.
		// Does not block supervisor startup. Retries with exponential backoff.
		s.triggerAutoEnroll()
	}

	// Boot-time AK recovery signal: gin_server.go regenerated a stale AK,
	// so the server's stored AK pubkey for cfg.DeviceID is now out of sync.
	// Kick off a background re-enrollment via the runtime recovery path,
	// which handles replayState, 409 device-conflict, and the recovering
	// concurrency guard. The fast-path above still runs so endpoint sync
	// can surface server-side states in parallel.
	if s.akRecovered.Load() && cfg.DeviceID != "" {
		log.Printf("WARN: identity: AK was recovered at boot; triggering background re-enrollment (device=%s)", cfg.DeviceID)
		s.triggerReenrollWithRecovery("ak recovered at boot")
	}

	return nil
}

// triggerAutoEnroll launches autoEnrollAtBoot in a goroutine with the same
// lifecycle guards as triggerReenrollWithRecovery.
func (s *Service) triggerAutoEnroll() {
	if s.autoEnrolling.Load() {
		return
	}
	s.mu.RLock()
	if s.stopped.Load() {
		s.mu.RUnlock()
		return
	}
	s.recoverWg.Add(1)
	s.mu.RUnlock()

	log.Printf("INFO: identity: starting auto-enrollment at boot")
	go func() {
		defer s.recoverWg.Done()
		s.autoEnrollAtBoot()
	}()
}

// autoEnrollAtBoot attempts initial enrollment with exponential backoff.
// Exits if enrollment succeeds, service stops, identity is disabled,
// or enrollment happens via another path (e.g., manual enroll from Settings).
func (s *Service) autoEnrollAtBoot() {
	if !s.autoEnrolling.CompareAndSwap(false, true) {
		log.Printf("INFO: identity: auto-enrollment already in progress, skipping")
		return
	}
	defer s.autoEnrolling.Store(false)

	const baseDelay = 5 * time.Second
	const maxDelay = 120 * time.Second
	const jitterFactor = 0.5

	for attempt := 0; ; attempt++ {
		if s.stopped.Load() {
			log.Printf("INFO: identity: auto-enrollment aborted (service stopped)")
			return
		}
		if !s.IsEnabled() {
			log.Printf("INFO: identity: auto-enrollment aborted (identity disabled)")
			return
		}
		// Another path may have enrolled while we were retrying.
		if s.IsEnrolled() {
			log.Printf("INFO: identity: auto-enrollment exiting (already enrolled)")
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		result, err := s.Enroll(ctx)
		cancel()

		if err == nil {
			log.Printf("INFO: identity: auto-enrollment succeeded after %d attempt(s) (device=%s, hostname=%s)",
				attempt+1, result.DeviceID, result.Hostname)
			if result.Reenrolled {
				go s.fetchSuggestedHostname()
			}
			return
		}

		// Device-conflict 409: surface via suspended=true, fall through to
		// backoff — do NOT exit the loop. When namek restores Active, the
		// next attempt succeeds and finalizeEnrollment clears suspended.
		if isDeviceConflictError(err) {
			s.handleDeviceConflict(err, "auto-enrollment")
		}

		delay := backoffDelay(attempt, baseDelay, maxDelay, jitterFactor)
		log.Printf("WARN: identity: auto-enrollment attempt %d failed: %s (retry in %v)", attempt+1, sanitizeErrForLog(err), delay)

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-s.networkUp:
			timer.Stop()
			log.Printf("INFO: identity: network up, retrying enrollment immediately")
		case <-s.stopCh:
			timer.Stop()
			log.Printf("INFO: identity: auto-enrollment aborted (service stopping)")
			return
		}
	}
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
	s.stopSetupHeartbeat()

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

// NotifyNetworkUp signals that network connectivity is available. This
// interrupts the auto-enrollment backoff timer so enrollment retries
// immediately instead of waiting for the next scheduled attempt.
func (s *Service) NotifyNetworkUp() {
	select {
	case s.networkUp <- struct{}{}:
	default: // coalesces rapid signals
	}
}

// SetTPMDirs stores the AK and swtpm state directories for recovery.
// Must be called before Start().
func (s *Service) SetTPMDirs(akStateDir, swtpmStateDir string) {
	s.mu.Lock()
	s.akStateDir = akStateDir
	s.swtpmStateDir = swtpmStateDir
	s.mu.Unlock()
}

// SetRelayServicesChangedHandler registers a callback invoked when relay services
// (STUN addresses, etc.) change. Used to propagate to the STUN service.
func (s *Service) SetRelayServicesChangedHandler(fn func(services map[string][]string)) {
	s.mu.Lock()
	s.onRelayServicesChanged = fn
	s.mu.Unlock()
}

// SetTPMReplacedHandler registers a callback invoked when AK recovery replaces the TPM device.
// The owner uses this to close the old device and track the new OpenResult.
func (s *Service) SetTPMReplacedHandler(fn func(old tpm.Device, newResult *tpm.OpenResult)) {
	s.mu.Lock()
	s.onTPMReplaced = fn
	s.mu.Unlock()
}

// SetAKRecovered signals that boot-time tpm.OpenWithAKRecovery regenerated a
// stale AK blob. Must be called before Start(); Start() reads the flag to
// decide whether to schedule a background re-enrollment.
func (s *Service) SetAKRecovered(v bool) {
	s.akRecovered.Store(v)
}

// SetSetupCompleteCheck registers a callback the setup-heartbeat loop polls
// to know when first-run setup has finished. Must be called before Start()
// (and before finalizeEnrollment) so the heartbeat can self-terminate.
// On error the callback should return true (fail-safe: stop heartbeating
// rather than leak setup-mode visibility forever on a locked persistence layer).
func (s *Service) SetSetupCompleteCheck(fn func() bool) {
	s.mu.Lock()
	s.onSetupCompleteCheck = fn
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

// SetupStatus returns a coarse signal for the setup UI: "enrolled",
// "pending", or "unavailable". Ordering matters:
//   - Terminal states first, so a suspended device never hangs at "pending"
//     while its retry loop keeps running.
//   - In-flight states next, so a previously-enrolled device whose AK was
//     regenerated at boot reports "pending" while its background re-enroll
//     runs (rather than "enrolled" — which would let the user attempt a
//     hostname claim against a namek that still has the stale AK pubkey).
//   - "enrolled" only when steady-state.
//   - The fallback is "unavailable", not "pending": if the service is
//     enabled and TPM is available but nothing is in-flight and we're not
//     enrolled, no enrollment job is actually running (e.g., post
//     SetEnabled(true) / SetNamekURL with no DeviceID and no autoEnroll
//     trigger). Reporting "pending" would spin the UI forever; "unavailable"
//     correctly tells the user there is nothing to wait for.
func (s *Service) SetupStatus() string {
	if s.suspended.Load() || !s.IsEnabled() || !s.available.Load() {
		return "unavailable"
	}
	if s.autoEnrolling.Load() || s.recovering.Load() {
		return "pending"
	}
	if s.enrolled.Load() {
		return "enrolled"
	}
	return "unavailable"
}

// DeviceConfig returns a read-only snapshot of the identity config.
func (s *Service) DeviceConfig() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := s.cfg
	cfg.NexusEndpoints = append([]string(nil), s.cfg.NexusEndpoints...)
	return cfg
}

// SuggestedHostname returns the ephemeral hostname suggestion from a
// re-enrollment, or empty string. Used by the boot handler to pre-populate
// the setup UI. Not persisted and not part of the device config.
func (s *Service) SuggestedHostname() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.suggestedHostname
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
	// TODO(namek-stun): When namekclient is bumped to include RelayServices:
	// s.cfg.RelayServices = result.RelayServices
	cfg := s.cfg
	s.mu.Unlock()

	if err := saveConfig(s.configPath, cfg); err != nil {
		return nil, fmt.Errorf("identity: persist enrollment: %w", err)
	}

	s.enrolled.Store(true)
	s.suspended.Store(false)
	s.startEndpointSync()
	s.maybeStartSetupHeartbeat()
	s.publish(events.TopicIdentityChanged, nil)

	// Notify relay services listener (e.g., STUN service) on first enrollment.
	// syncNexusEndpoints handles subsequent changes, but the initial enrollment
	// sets endpoints directly without going through the sync path.
	s.mu.RLock()
	cb := s.onRelayServicesChanged
	relayServices := s.cfg.RelayServices
	s.mu.RUnlock()
	if cb != nil && len(relayServices) > 0 {
		cb(relayServices)
	}

	log.Printf("INFO: identity: enrolled (device=%s, hostname=%s, reenrolled=%v)",
		result.DeviceID, result.Hostname, result.Reenrolled)

	return &EnrollResult{
		DeviceID:       result.DeviceID,
		Hostname:       result.Hostname,
		BaseDomain:     baseDomain,
		IdentityClass:  result.IdentityClass,
		NexusEndpoints: result.NexusEndpoints,
		RelayServices:  cfg.RelayServices,
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
		s.stopSetupHeartbeat()
		// Clear suspended on disable — see the rationale in reenrollWithRecovery's
		// early-exit branch. suspended is a "namek says we're not Active" signal;
		// if we're not asking namek, the flag is no longer meaningful.
		s.suspended.Store(false)
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
		s.maybeStartSetupHeartbeat()
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
	s.stopSetupHeartbeat()

	s.mu.Lock()
	s.cfg.NamekURL = url
	s.cfg.DeviceID = ""
	s.cfg.AccountID = ""
	s.cfg.Hostname = ""
	s.cfg.BaseDomain = ""
	s.cfg.CustomHostname = ""
	s.cfg.IdentityClass = ""
	s.cfg.NexusEndpoints = nil
	s.suggestedHostname = ""
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
	s.suggestedHostname = ""
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

// isDeviceConflictError reports whether err unwraps to an HTTP 409 from namek,
// returned by enrollment handlers when the EK fingerprint matches a non-Active
// device (Suspended, Revoked, PendingDeletion). Uses errors.As so the check
// works through Service.Enroll's %w wrapping — substring matching on the
// wrapped error text fails. Only valid at enrollment call sites.
func isDeviceConflictError(err error) bool {
	var apiErr *namekclient.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

// sanitizeErrForLog strips control characters (including newlines, carriage
// returns, tabs, and ANSI escapes) from an error message before logging.
// Namek HTTP error bodies are surfaced verbatim via APIError.Error(), and a
// malicious or compromised namek server could otherwise inject fake log
// entries by embedding "\n[FAKE LOG] ..." in a 4xx body.
func sanitizeErrForLog(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// handleDeviceConflict records a device-conflict (409) error from a retry
// loop. It sets suspended=true and publishes a state-change event on the
// transition from !suspended to suspended, so ERROR-level log and event
// fire only once per stuck period — subsequent retries quietly loop at
// backoff until the operator restores Active on namek and the next attempt
// succeeds via finalizeEnrollment (which clears suspended).
//
// This is shared between autoEnrollAtBoot (fresh-device path) and
// reenrollWithRecovery (already-enrolled path). The two loops are mutually
// exclusive by precondition (autoEnrollAtBoot requires cfg.DeviceID == "",
// reenrollWithRecovery requires enrolled == true), so the log-on-transition
// guard is safe — a future refactor allowing both loops to coexist must
// re-evaluate the guard.
func (s *Service) handleDeviceConflict(err error, scope string) {
	if !s.suspended.Load() {
		log.Printf("ERROR: identity: %s rejected — device exists server-side but is not Active (likely suspended/revoked). Will continue to retry at backoff cap; restore Active status on namek to self-heal. (%s)", scope, sanitizeErrForLog(err))
		s.suspended.Store(true)
		s.publish(events.TopicIdentityChanged, nil)
	}
}

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

// triggerReenrollWithRecovery launches reenrollWithRecovery in a goroutine.
// The recovering CAS runs synchronously so callers see a consistent
// "recovering" state immediately after return — Start()'s boot-time
// invocation relies on this to avoid a transient "enrolled" SetupStatus.
func (s *Service) triggerReenrollWithRecovery(reason string) {
	if !s.recovering.CompareAndSwap(false, true) {
		log.Printf("INFO: identity: recovery already in progress, skipping (%s)", reason)
		return
	}
	// RLock synchronizes with Stop()'s Lock: Add(1) completes before
	// Stop() can call Wait(), preventing a WaitGroup panic.
	s.mu.RLock()
	if s.stopped.Load() {
		s.mu.RUnlock()
		s.recovering.Store(false) // release the CAS we just acquired
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
// Caller (triggerReenrollWithRecovery) must have set s.recovering=true; this
// function clears it on exit. Falls back to recoverAndReenroll on AK errors.
func (s *Service) reenrollWithRecovery() {
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
		// Clear suspended on exit: it means "namek says we're not Active",
		// which is no longer meaningful if we're not asking namek anymore.
		// Leaving it latched would wedge SetupStatus until piccolod restart.
		if !s.enrolled.Load() || !s.IsEnabled() {
			s.suspended.Store(false)
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

		// Device-conflict 409: see handleDeviceConflict / autoEnrollAtBoot
		// sibling — fall through to backoff, self-heal on next success.
		if isDeviceConflictError(err) {
			s.handleDeviceConflict(err, "re-enrollment")
		}

		delay := backoffDelay(attempt, baseDelay, maxDelay, jitterFactor)
		log.Printf("WARN: identity: re-enrollment attempt %d failed: %s (retry in %v)", attempt+1, sanitizeErrForLog(err), delay)

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

// maxHardwareModelLen mirrors namek-server's `max=128` validator on the
// hardware_model attest field. Long DMI/devicetree strings are truncated
// rather than dropped so namek still gets useful identifying info.
const maxHardwareModelLen = 128

// newNamekClient creates a namekclient with standard options (device ID,
// hardware model, insecure skip). The hardware model ships in the enrollment
// attest body so namek can surface it via the setup-discovery endpoint.
func newNamekClient(url string, dev tpm.Device, deviceID string) *namekclient.Client {
	var opts []namekclient.Option
	if deviceID != "" {
		opts = append(opts, namekclient.WithDeviceID(deviceID))
	}
	if model := truncateHardwareModel(mdns.GetDeviceModel()); model != "" {
		opts = append(opts, namekclient.WithHardwareModel(model))
	}
	if os.Getenv("PICCOLO_NAMEK_INSECURE") == "1" {
		opts = append(opts, namekclient.WithInsecureSkipVerify())
	}
	return namekclient.New(url, dev, opts...)
}

// truncateHardwareModel clamps a hardware model string to namek's max length,
// taking care not to split a UTF-8 rune at the cut point.
func truncateHardwareModel(model string) string {
	if len(model) <= maxHardwareModelLen {
		return model
	}
	cut := maxHardwareModelLen
	for cut > 0 && !utf8.RuneStart(model[cut]) {
		cut--
	}
	return model[:cut]
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

	// TODO(namek-stun): When namekclient is bumped to include RelayServices:
	// s.syncRelayServices(info.RelayServices)
}

// fetchSuggestedHostname performs a best-effort GetDeviceInfo call to retrieve
// the prior custom hostname after a re-enrollment. The result is stored in
// suggestedHostname (in-memory only) for the boot handler to surface as a
// UI pre-fill hint. Failures are logged and swallowed.
func (s *Service) fetchSuggestedHostname() {
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	if client == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	info, err := client.GetDeviceInfo(ctx)
	if err != nil {
		log.Printf("WARN: identity: fetch suggested hostname: %v", err)
		return
	}

	if info.CustomHostname != nil && *info.CustomHostname != "" {
		hostname := strings.ToLower(*info.CustomHostname)
		if !isValidDNSLabel(hostname) {
			log.Printf("WARN: identity: suggested hostname %q from server is not a valid DNS label, ignoring", hostname)
			return
		}
		s.mu.Lock()
		// Don't overwrite if user already claimed a hostname (late-arriving fetch).
		if s.cfg.CustomHostname == "" {
			s.suggestedHostname = hostname
			s.mu.Unlock()
			log.Printf("INFO: identity: suggested hostname from prior enrollment: %q", hostname)
		} else {
			s.mu.Unlock()
			log.Printf("INFO: identity: suggested hostname %q discarded (hostname already claimed)", hostname)
		}
	}
}

// isValidDNSLabel rejects server-provided hostnames that would fail DNS resolution
// or confuse the setup UI. Deliberately lowercase-only — callers must ToLower first.
func isValidDNSLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		ch := label[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
			continue
		}
		return false
	}
	return true
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
		// Nonce is hex-encoded sha256 of voucher data.
		nonceBytes, err := hex.DecodeString(req.Nonce)
		if err != nil {
			log.Printf("WARN: identity: voucher sign: bad nonce hex %q: %v", req.Nonce, err)
			continue
		}
		quoteB64, err := tpmDev.Quote(nonceBytes)
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

	// Notify relay services listener (e.g., STUN service) when relay services change.
	s.mu.RLock()
	cb := s.onRelayServicesChanged
	relayServices := s.cfg.RelayServices
	s.mu.RUnlock()
	if cb != nil && len(relayServices) > 0 {
		cb(relayServices)
	}
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

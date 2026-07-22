package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"piccolod/internal/fsutil"
	"piccolod/internal/health"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/state/paths"
)

const (
	defaultTimeout          = 45 * time.Minute
	defaultRuntimeDir       = "/run/piccolo"
	defaultStateSubdir      = "update"
	defaultStateFilename    = "state.json"
	transactionalUpdateUnit = "transactional-update.service"

	// Status snapshots keep UI reads off the slow live shell-out path. Fresh cache
	// is returned directly; stale cache is returned with "refreshing" metadata while
	// a bounded background refresh probes snapper/rpm/btrfs/journalctl/zypper.
	// Apply/Rollback/Reboot invalidate freshness at the Manager facade so post-action
	// UI does not present stale data as fresh.
	statusSnapshotTTL       = 60 * time.Second
	statusRefreshInterval   = 30 * time.Second
	statusRequestTimeout    = 5 * time.Second
	statusRefreshTimeout    = 20 * time.Second
	statusEnrichmentBackoff = 5 * time.Minute

	// Auto-recovery circuit breaker: bound how often checkAndRecover may fire
	// the agent-package fallback so a persistently-failing OS update can't loop
	// and exhaust the disk with snapshots. The window re-arms on expiry, and a
	// successful OS update (exit 0) resets it immediately.
	maxAutoRecoveryPerWindow = 3
	autoRecoveryWindow       = 24 * time.Hour

	// minFreeBytesForRecovery is the free-space floor below which piccolod
	// refuses to start a snapshot-creating update. It must exceed the peak
	// transient footprint of a single transactional-update (download cache +
	// new snapshot delta). The OS itself is ~0.9 GiB, so 2 GiB clears a
	// worst-case dup with margin while leaving most of a 16 GiB disk usable.
	// Tunable: raise on larger images.
	minFreeBytesForRecovery = 2 << 30 // 2 GiB

	recoveryLedgerFilename = "recovery.json"
)

var (
	ErrInProgress               = errors.New("transactional-update in progress")
	ErrUnsupported              = errors.New("transactional-update unsupported on this host")
	ErrInvalidSnapshot          = errors.New("invalid snapshot id")
	ErrTimeout                  = errors.New("transactional-update timed out")
	ErrSnapshotValidationFailed = errors.New("staged snapshot missing critical components")
	ErrInsufficientDisk         = errors.New("insufficient disk space for update")
)

// Status mirrors the public API shape and carries a meta section for richer data.
type Status struct {
	CurrentVersion   string                 `json:"current_version"`
	AvailableVersion string                 `json:"available_version"`
	Pending          bool                   `json:"pending"`
	RequiresReboot   bool                   `json:"requires_reboot"`
	LastChecked      time.Time              `json:"last_checked"`
	Meta             map[string]interface{} `json:"meta,omitempty"`
}

// SnapshotReadiness is the fast-path staged-root state used before any
// snapper/zypper/RPM enrichment is attempted.
type SnapshotReadiness string

const (
	SnapshotReadinessStaged     SnapshotReadiness = "staged"
	SnapshotReadinessAbsent     SnapshotReadiness = "absent"
	SnapshotReadinessInProgress SnapshotReadiness = "in_progress"
	SnapshotReadinessUnknown    SnapshotReadiness = "unknown"
)

// SnapshotState reports the authoritative active/default root relationship.
// ActiveSnapshot and DefaultSnapshot are normalized snapper snapshot numbers
// when Readiness is staged or absent.
type SnapshotState struct {
	ActiveSnapshot        string            `json:"active_snapshot,omitempty"`
	DefaultSnapshot       string            `json:"default_snapshot,omitempty"`
	ActiveSnapshotSource  string            `json:"active_snapshot_source,omitempty"`
	DefaultSnapshotSource string            `json:"default_snapshot_source,omitempty"`
	Readiness             SnapshotReadiness `json:"readiness"`
	RequiresReboot        bool              `json:"requires_reboot"`
	Source                string            `json:"source,omitempty"`
}

// Manager fronts the OS-specific backend (MicroOS today; pluggable later).
type Manager struct {
	backend osBackend
}

// osBackend defines the per-platform surface.
type osBackend interface {
	Status(context.Context) (Status, error)
	SnapshotState(context.Context) (SnapshotState, error)
	Apply(context.Context) error
	Rollback(context.Context, string) error
	Reboot(context.Context) error
	ForceReboot(context.Context) error
	PowerOff(context.Context) error
	Watch(context.Context) error
	invalidateStatusCache()
}

// microOSBackend interacts with MicroOS transactional-update to report and apply updates.
type microOSBackend struct {
	runner         commandRunner
	clock          func() time.Time
	timeout        time.Duration
	runtimeDir     string
	statePath      string
	readFile       func(string) ([]byte, error)
	currentVersion string
	snapshotsDir   string

	mu              sync.Mutex
	supported       bool
	overrideSupport bool

	// statusMu is separate from m.mu to avoid contention with runTransactionalUpdate
	// (which holds m.mu for the full TU lifetime). statusInvalidatedAt is the
	// high-water mark for invalidation: a sample taken before this timestamp
	// must NOT be published (would overwrite a concurrent invalidate from
	// Apply/Rollback/Reboot with pre-mutation data).
	statusMu                     sync.RWMutex
	statusCache                  Status
	statusCachedAt               time.Time
	statusInvalidatedAt          time.Time
	statusRefreshActive          bool
	statusEnrichmentBackoffUntil time.Time
	statusLastEnrichmentErr      string
	statusRequestTimeout         time.Duration
	statusRefreshTimeout         time.Duration

	// Auto-recovery circuit breaker, guarded by mu. In-memory is authoritative;
	// it is mirrored best-effort to recoveryPath so a reboot-loop accumulates
	// toward the cap. Under a read-only fs the mirror write fails harmlessly —
	// the disk-headroom gate is the protection there.
	recoveryPath            string
	autoRecoveryCount       int
	autoRecoveryWindowStart time.Time
	lastHandledFailure      time.Time
	lastHealthLevel         health.Level
	lastHealthMsg           string

	// setHealth escalates the "update" health key (nil => no-op). Injected to
	// keep this package decoupled from the server. freeBytes probes free space
	// (defaults to statfs); injectable for tests.
	setHealth func(level health.Level, msg string)
	freeBytes func(path string) (uint64, error)
}

// Option configures the MicroOS backend.
type Option func(*microOSBackend)

// WithRunner injects a custom command runner (used in tests).
func WithRunner(r commandRunner) Option { return func(m *microOSBackend) { m.runner = r } }

// WithClock injects a custom clock (used in tests).
func WithClock(fn func() time.Time) Option { return func(m *microOSBackend) { m.clock = fn } }

// WithTimeout overrides the transactional-update timeout.
func WithTimeout(d time.Duration) Option { return func(m *microOSBackend) { m.timeout = d } }

// WithStatusRequestTimeout bounds the initial uncached status probe (used in tests).
func WithStatusRequestTimeout(d time.Duration) Option {
	return func(m *microOSBackend) {
		if d > 0 {
			m.statusRequestTimeout = d
		}
	}
}

// WithStatusRefreshTimeout bounds background status refresh probes (used in tests).
func WithStatusRefreshTimeout(d time.Duration) Option {
	return func(m *microOSBackend) {
		if d > 0 {
			m.statusRefreshTimeout = d
		}
	}
}

// WithStateDir overrides the persistent state directory (default PICCOLO_CORE_ROOT/update).
func WithStateDir(dir string) Option {
	return func(m *microOSBackend) {
		m.statePath = filepath.Join(dir, defaultStateSubdir, defaultStateFilename)
	}
}

// WithRuntimeDir overrides the runtime directory used for short-lived markers (default /run/piccolo).
func WithRuntimeDir(dir string) Option { return func(m *microOSBackend) { m.runtimeDir = dir } }

// WithSupportOverride forces support detection (useful for tests without TU binaries).
func WithSupportOverride(supported bool) Option {
	return func(m *microOSBackend) { m.overrideSupport = supported }
}

// WithReadFile injects a file reader (used in tests).
func WithReadFile(f func(string) ([]byte, error)) Option {
	return func(m *microOSBackend) { m.readFile = f }
}

// WithCurrentVersion injects the running application version string.
func WithCurrentVersion(v string) Option {
	return func(m *microOSBackend) { m.currentVersion = v }
}

// WithSnapshotsDir overrides the btrfs snapshots directory (default /.snapshots).
func WithSnapshotsDir(dir string) Option {
	return func(m *microOSBackend) { m.snapshotsDir = dir }
}

// WithHealthReporter injects a callback used to escalate the "update" health
// key. Nil-safe: if unset, escalation is a no-op.
func WithHealthReporter(fn func(level health.Level, msg string)) Option {
	return func(m *microOSBackend) { m.setHealth = fn }
}

// WithFreeBytesFn overrides the free-space probe (used in tests).
func WithFreeBytesFn(fn func(path string) (uint64, error)) Option {
	return func(m *microOSBackend) { m.freeBytes = fn }
}

// NewManager constructs a Manager with an OS backend (MicroOS today).
func NewManager(opts ...Option) (*Manager, error) {
	b, err := newMicroOSBackend(opts...)
	if err != nil {
		return nil, err
	}
	return &Manager{backend: b}, nil
}

// Status returns the current OS update status (graceful on unsupported hosts).
func (m *Manager) Status(ctx context.Context) (Status, error) {
	return m.backend.Status(pressure.WithWorkClass(ctx, pressure.WorkUpdate))
}

// SnapshotState returns the fast active/default snapshot relationship without
// snapper/zypper/RPM enrichment.
func (m *Manager) SnapshotState(ctx context.Context) (SnapshotState, error) {
	return m.backend.SnapshotState(pressure.WithWorkClass(ctx, pressure.WorkUpdate))
}

// Apply, Rollback, and Reboot invalidate the status cache unconditionally on
// return — including on no-op rejections like ErrInProgress. We accept one
// extra readStatus over reaching into backend internals to discriminate
// "actually mutated" from "rejected before mutation."

// Apply triggers transactional-update dup.
func (m *Manager) Apply(ctx context.Context) error {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkUpdate)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkUpdate); err != nil {
		return err
	}
	defer pressure.BeginLifecycleOwner("update")()
	defer m.backend.invalidateStatusCache()
	return m.backend.Apply(ctx)
}

// Rollback sets the requested snapshot as default for next boot.
func (m *Manager) Rollback(ctx context.Context, targetID string) error {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkUpdate)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkUpdate); err != nil {
		return err
	}
	defer pressure.BeginLifecycleOwner("update")()
	defer m.backend.invalidateStatusCache()
	return m.backend.Rollback(ctx, targetID)
}

// Reboot validates the staged snapshot and triggers a system reboot.
func (m *Manager) Reboot(ctx context.Context) error {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkUpdate)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkUpdate); err != nil {
		return err
	}
	defer pressure.BeginLifecycleOwner("update")()
	defer m.backend.invalidateStatusCache()
	return m.backend.Reboot(ctx)
}

// ForceReboot triggers a system reboot without snapshot validation.
func (m *Manager) ForceReboot(ctx context.Context) error {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkUpdate)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkUpdate); err != nil {
		return err
	}
	defer pressure.BeginLifecycleOwner("update")()
	return m.backend.ForceReboot(ctx)
}

// PowerOff triggers a system power off.
func (m *Manager) PowerOff(ctx context.Context) error {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkUpdate)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkUpdate); err != nil {
		return err
	}
	defer pressure.BeginLifecycleOwner("update")()
	return m.backend.PowerOff(ctx)
}

// Watch starts a background monitoring loop to detect and recover from update failures.
func (m *Manager) Watch(ctx context.Context) error {
	return m.backend.Watch(pressure.WithWorkClass(ctx, pressure.WorkUpdate))
}

type initialRecoveryBackend interface {
	RunInitialRecovery(context.Context)
	WatchAfterInitial(context.Context) error
}

// RunInitialRecovery joins the update owner's bounded startup probes. Backends
// without an eager recovery pass intentionally no-op.
func (m *Manager) RunInitialRecovery(ctx context.Context) {
	if backend, ok := m.backend.(initialRecoveryBackend); ok {
		backend.RunInitialRecovery(pressure.WithWorkClass(ctx, pressure.WorkUpdate))
	}
}

// WatchAfterInitial starts only steady-state update monitoring after a joined
// task-recovery pass. It falls back to Watch for backends without split startup
// semantics.
func (m *Manager) WatchAfterInitial(ctx context.Context) error {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkUpdate)
	if backend, ok := m.backend.(initialRecoveryBackend); ok {
		return backend.WatchAfterInitial(ctx)
	}
	return m.backend.Watch(ctx)
}

// newMicroOSBackend constructs the MicroOS implementation.
func newMicroOSBackend(opts ...Option) (*microOSBackend, error) {
	timeout := defaultTimeout
	if env := os.Getenv("PICCOLO_UPDATE_TIMEOUT_S"); env != "" {
		if secs, err := strconv.Atoi(env); err == nil && secs > 0 {
			timeout = time.Duration(secs) * time.Second
		}
	}

	m := &microOSBackend{
		runner:               execRunner{},
		clock:                time.Now,
		timeout:              timeout,
		statusRequestTimeout: statusRequestTimeout,
		statusRefreshTimeout: statusRefreshTimeout,
		runtimeDir:           defaultRuntimeDir,
		statePath:            filepath.Join(paths.CoreRoot(), defaultStateSubdir, defaultStateFilename),
		readFile:             os.ReadFile,
		snapshotsDir:         "/.snapshots",
	}
	for _, opt := range opts {
		opt(m)
	}

	if err := os.MkdirAll(filepath.Dir(m.statePath), 0o700); err != nil {
		return nil, fmt.Errorf("ensure state dir: %w", err)
	}
	if err := os.MkdirAll(m.runtimeDir, 0o755); err != nil {
		alt := filepath.Join(os.TempDir(), "piccolo-run")
		if err2 := os.MkdirAll(alt, 0o755); err2 != nil {
			return nil, fmt.Errorf("ensure runtime dir: %w", err)
		}
		m.runtimeDir = alt
	}

	if m.freeBytes == nil {
		m.freeBytes = statfsFree
	}
	m.recoveryPath = filepath.Join(filepath.Dir(m.statePath), recoveryLedgerFilename)
	m.loadRecoveryLedger()

	m.supported = m.overrideSupport || m.detectSupported()

	return m, nil
}

func (m *microOSBackend) SnapshotState(ctx context.Context) (SnapshotState, error) {
	if !m.supported {
		return SnapshotState{Readiness: SnapshotReadinessUnknown, Source: "unsupported"}, ErrUnsupported
	}
	probeCtx, cancel := context.WithTimeout(ctx, m.statusRequestTimeout)
	defer cancel()
	return m.snapshotState(probeCtx)
}

// Status returns the current OS update status (graceful on unsupported hosts).
func (m *microOSBackend) Status(ctx context.Context) (Status, error) {
	if !m.supported {
		return Status{
			CurrentVersion:   "unknown",
			AvailableVersion: "unknown",
			Pending:          false,
			RequiresReboot:   false,
			LastChecked:      m.clock(),
			Meta: map[string]interface{}{
				"supported": false,
			},
		}, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, m.statusRequestTimeout)
	defer cancel()

	snapshot, snapshotErr := m.snapshotState(probeCtx)
	if snapshot.Readiness == SnapshotReadinessInProgress {
		return Status{}, ErrInProgress
	}
	if snapshotErr != nil {
		m.scheduleStatusRefresh()
		return m.statusFallback(snapshotErr, snapshot, nil), nil
	}

	if st, ok := m.cachedStatus(false); ok {
		return m.applySnapshotState(st, snapshot), nil
	}
	if st, ok := m.cachedStatus(true); ok {
		m.scheduleStatusRefresh()
		return m.applySnapshotState(st, snapshot), nil
	}

	sampleStart := time.Now()
	if ok, fields := m.tryBeginStatusEnrichment(sampleStart); !ok {
		return m.statusFallback(nil, snapshot, fields), nil
	}
	// Enrichment failures still return a partial Status; request cancellation
	// can leave that sample incomplete, so don't pollute the 60s cache with it.
	st, err := m.readStatus(probeCtx)
	if probeErr := probeCtx.Err(); probeErr != nil {
		if errors.Is(probeErr, context.Canceled) {
			m.finishStatusEnrichment(nil)
		} else {
			m.finishStatusEnrichment(err, probeErr)
		}
		m.scheduleStatusRefresh()
		return m.statusFallback(firstNonNil(err, probeErr), snapshot, nil), nil
	}
	m.finishStatusEnrichment(err)
	st = m.applySnapshotState(st, snapshot)
	m.publishStatusSnapshot(st, sampleStart)
	return st, nil
}

func (m *microOSBackend) invalidateStatusCache() {
	m.statusMu.Lock()
	m.statusInvalidatedAt = time.Now()
	m.statusMu.Unlock()
}

func (m *microOSBackend) cachedStatus(allowStale bool) (Status, bool) {
	m.statusMu.RLock()
	if m.statusCachedAt.IsZero() {
		m.statusMu.RUnlock()
		return Status{}, false
	}
	isInvalidated := m.statusCachedAt.Before(m.statusInvalidatedAt)
	isExpired := time.Since(m.statusCachedAt) >= statusSnapshotTTL
	if !allowStale && (isInvalidated || isExpired) {
		m.statusMu.RUnlock()
		return Status{}, false
	}
	st := m.statusCache
	cachedAt := m.statusCachedAt
	m.statusMu.RUnlock()
	if allowStale && (isInvalidated || isExpired) {
		st = withStatusMeta(st, map[string]interface{}{
			"stale":             true,
			"refreshing":        true,
			"cached_at":         cachedAt.UTC().Format(time.RFC3339),
			"cache_age_seconds": int(time.Since(cachedAt).Seconds()),
		})
	}
	return st, true
}

func (m *microOSBackend) statusFallback(err error, snapshot SnapshotState, fields map[string]interface{}) Status {
	if st, ok := m.cachedStatus(true); ok {
		return m.decorateStatusWithSnapshot(st, snapshot, fields, err)
	}
	return m.minimalStatusFromSnapshot(snapshot, fields, err)
}

func (m *microOSBackend) fastCoreStatus(err error, snapshot SnapshotState, fields map[string]interface{}) Status {
	if st, ok := m.cachedStatus(false); ok {
		return m.decorateStatusWithSnapshot(st, snapshot, fields, err)
	}
	return m.minimalStatusFromSnapshot(snapshot, fields, err)
}

func (m *microOSBackend) decorateStatusWithSnapshot(st Status, snapshot SnapshotState, fields map[string]interface{}, err error) Status {
	st = m.applySnapshotState(st, snapshot)
	if err != nil {
		fields = mergeMeta(fields, map[string]interface{}{
			"degraded":        true,
			"degraded_reason": err.Error(),
		})
	}
	if len(fields) > 0 {
		st = withStatusMeta(st, fields)
	}
	return st
}

func (m *microOSBackend) minimalStatusFromSnapshot(snapshot SnapshotState, fields map[string]interface{}, err error) Status {
	currentVersion := m.currentVersion
	if currentVersion == "" {
		currentVersion = m.getOSReleaseVersion("")
	}
	if currentVersion == "" {
		currentVersion = "unknown"
	}
	pending := snapshot.Readiness == SnapshotReadinessStaged
	availableVersion := currentVersion
	if pending && snapshot.DefaultSnapshot != "" {
		availableVersion = fmt.Sprintf("%s (System Update %s)", currentVersion, snapshot.DefaultSnapshot)
	}
	meta := map[string]interface{}{
		"supported":   true,
		"degraded":    true,
		"refreshing":  true,
		"cache_empty": true,
	}
	meta = mergeMeta(meta, snapshotStatusMeta(snapshot))
	meta = mergeMeta(meta, fields)
	if err != nil {
		meta["degraded_reason"] = err.Error()
	}
	if intent := m.loadState(); intent != nil {
		meta["last_request"] = intent
	}
	return Status{
		CurrentVersion:   currentVersion,
		AvailableVersion: availableVersion,
		Pending:          pending,
		RequiresReboot:   pending,
		LastChecked:      m.clock(),
		Meta:             meta,
	}
}

func (m *microOSBackend) applySnapshotState(st Status, snapshot SnapshotState) Status {
	switch snapshot.Readiness {
	case SnapshotReadinessStaged:
		st.Pending = true
		st.RequiresReboot = true
		if st.AvailableVersion == "" || st.AvailableVersion == st.CurrentVersion {
			if snapshot.DefaultSnapshot != "" {
				st.AvailableVersion = fmt.Sprintf("%s (System Update %s)", st.CurrentVersion, snapshot.DefaultSnapshot)
			}
		}
	case SnapshotReadinessAbsent:
		st.Pending = false
		st.RequiresReboot = false
	case SnapshotReadinessUnknown:
		st.Pending = false
		st.RequiresReboot = false
		st = withStatusMeta(st, map[string]interface{}{
			"degraded": true,
		})
	}
	st.Meta = mergeMeta(st.Meta, snapshotStatusMeta(snapshot))
	return st
}

func snapshotStatusMeta(snapshot SnapshotState) map[string]interface{} {
	meta := map[string]interface{}{}
	if snapshot.Readiness != "" {
		meta["snapshot_readiness"] = string(snapshot.Readiness)
	}
	if snapshot.ActiveSnapshot != "" {
		meta["active_snapshot_id"] = snapshot.ActiveSnapshot
	}
	if snapshot.DefaultSnapshot != "" {
		meta["default_snapshot_id"] = snapshot.DefaultSnapshot
	}
	if snapshot.ActiveSnapshotSource != "" {
		meta["active_snapshot_source"] = snapshot.ActiveSnapshotSource
	}
	if snapshot.DefaultSnapshotSource != "" {
		meta["default_snapshot_source"] = snapshot.DefaultSnapshotSource
	}
	if snapshot.RequiresReboot {
		meta["requires_reboot_source"] = "fast_snapshot"
	}
	return meta
}

func withStatusMeta(st Status, fields map[string]interface{}) Status {
	st.Meta = mergeMeta(st.Meta, fields)
	return st
}

func mergeMeta(base map[string]interface{}, fields map[string]interface{}) map[string]interface{} {
	meta := make(map[string]interface{}, len(base)+len(fields))
	for k, v := range base {
		meta[k] = v
	}
	for k, v := range fields {
		meta[k] = v
	}
	return meta
}

func firstNonNil(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

type enrichmentErrorCollector struct {
	errs []error
}

func (c *enrichmentErrorCollector) add(source string, err error) {
	if err == nil {
		return
	}
	c.errs = append(c.errs, fmt.Errorf("%s: %w", source, err))
}

func (c *enrichmentErrorCollector) err() error {
	if len(c.errs) == 0 {
		return nil
	}
	return fmt.Errorf("status enrichment failed: %w", errors.Join(c.errs...))
}

func commandFailure(stderr string, code int, err error) error {
	msg := strings.TrimSpace(stderr)
	if err != nil {
		if msg != "" {
			return fmt.Errorf("%s: %w", msg, err)
		}
		return err
	}
	if code != 0 {
		if msg != "" {
			return fmt.Errorf("exit code %d: %s", code, msg)
		}
		return fmt.Errorf("exit code %d", code)
	}
	return nil
}

func (m *microOSBackend) scheduleStatusRefresh() {
	if ok, _ := m.tryBeginStatusEnrichment(time.Now()); !ok {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), m.statusRefreshTimeout)
		defer cancel()
		m.refreshStatusCacheLocked(ctx)
	}()
}

func (m *microOSBackend) tryBeginStatusEnrichment(now time.Time) (bool, map[string]interface{}) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	if m.statusRefreshActive {
		return false, map[string]interface{}{"refreshing": true}
	}
	if !m.statusEnrichmentBackoffUntil.IsZero() && now.Before(m.statusEnrichmentBackoffUntil) {
		fields := map[string]interface{}{
			"degraded":                 true,
			"enrichment_backoff":       true,
			"enrichment_backoff_until": m.statusEnrichmentBackoffUntil.UTC().Format(time.RFC3339),
		}
		if m.statusLastEnrichmentErr != "" {
			fields["degraded_reason"] = m.statusLastEnrichmentErr
		}
		return false, fields
	}
	m.statusRefreshActive = true
	return true, nil
}

func (m *microOSBackend) finishStatusEnrichment(errs ...error) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	m.statusRefreshActive = false
	for _, err := range errs {
		if err != nil {
			m.statusEnrichmentBackoffUntil = m.clock().Add(statusEnrichmentBackoff)
			m.statusLastEnrichmentErr = err.Error()
			return
		}
	}
	m.statusEnrichmentBackoffUntil = time.Time{}
	m.statusLastEnrichmentErr = ""
}

// publishStatusSnapshot writes a freshly-sampled status under statusMu, but
// drops the write if an invalidation occurred after the sample started — that
// would overwrite the invalidate with stale pre-mutation data.
func (m *microOSBackend) publishStatusSnapshot(st Status, sampleStart time.Time) {
	m.statusMu.Lock()
	if sampleStart.After(m.statusInvalidatedAt) {
		m.statusCache = st
		m.statusCachedAt = sampleStart
	}
	m.statusMu.Unlock()
}

// Apply triggers transactional-update dup with a fallback to package-only update.
func (m *microOSBackend) Apply(ctx context.Context) error {
	if !m.supported {
		return ErrUnsupported
	}
	// Strategy: Try full distro upgrade first ("dup").
	// If that fails (e.g. dependency conflicts in base OS), fall back to updating
	// just the agent packages ("pkg update ..."). This ensures we can still ship
	// fixes to piccolod even if the base OS repos are messy.
	// We chain these in a shell to keep it as one "job" from the API's perspective.
	cmd := []string{
		"/bin/sh",
		"-c",
		"transactional-update --non-interactive dup || transactional-update --non-interactive pkg update piccolo-os-support piccolod",
	}
	return m.runTransactionalUpdate(ctx, cmd, "apply", "", false)
}

// Rollback sets the requested snapshot as default for next boot.
func (m *microOSBackend) Rollback(ctx context.Context, targetID string) error {
	if !m.supported {
		return ErrUnsupported
	}
	targetID = strings.TrimSpace(targetID)
	snaps, _ := m.snapperSnapshots(ctx)
	if targetID == "" {
		// best-effort: pick the newest non-active snapshot
		targetID = m.pickRollbackTarget(ctx)
	} else {
		if _, ok := snaps[targetID]; !ok {
			return ErrInvalidSnapshot
		}
	}
	if targetID == "" {
		return ErrInvalidSnapshot
	}
	// Fire and return; the TU run continues via systemd.
	return m.runTransactionalUpdate(ctx, []string{"transactional-update", "--non-interactive", "rollback", targetID}, "rollback", targetID, false)
}

// Reboot validates the staged snapshot (if any) and triggers systemctl reboot.
// If validation fails, the bad snapshot is reverted and deleted before returning an error.
func (m *microOSBackend) Reboot(ctx context.Context) error {
	if !m.supported {
		return ErrUnsupported
	}

	defaultID, err := m.validateStagedSnapshot(ctx)
	if err != nil {
		// Only run destructive cleanup (revert+delete) for confirmed content
		// failures. Lookup/probe errors block reboot but don't touch snapshots.
		if !errors.Is(err, ErrSnapshotValidationFailed) {
			return err
		}

		// Revert to active snapshot — only delete if revert succeeds,
		// otherwise we'd leave boot default pointing at a deleted snapshot.
		if revertErr := m.revertDefaultSnapshot(ctx); revertErr != nil {
			return fmt.Errorf("validation failed AND revert failed (revert: %v): %w", revertErr, err)
		}

		// Best-effort delete the bad snapshot (safe: default already reverted)
		if defaultID != "" {
			_, _, _, _ = m.runner.Run(ctx, "snapper", "delete", defaultID)
		}

		return err
	}

	_, _, _, err = m.runner.Run(ctx, "systemctl", "reboot")
	return err
}

// ForceReboot triggers systemctl reboot without snapshot validation.
// Intended as an emergency escape hatch.
func (m *microOSBackend) ForceReboot(ctx context.Context) error {
	if !m.supported {
		return ErrUnsupported
	}
	_, _, _, err := m.runner.Run(ctx, "systemctl", "reboot")
	return err
}

// PowerOff triggers systemctl poweroff.
func (m *microOSBackend) PowerOff(ctx context.Context) error {
	if !m.supported {
		return ErrUnsupported
	}
	_, _, _, err := m.runner.Run(ctx, "systemctl", "poweroff")
	return err
}

// Watch runs a background loop to monitor system update status and trigger fallbacks.
func (m *microOSBackend) Watch(ctx context.Context) error {
	return m.watch(ctx, true)
}

// RunInitialRecovery executes the same bounded probes that normal Watch would
// launch eagerly, but synchronously so recovery-mode owner ordering is real.
func (m *microOSBackend) RunInitialRecovery(ctx context.Context) {
	if !m.supported {
		return
	}
	m.cleanupStaleState(ctx)
	if ctx.Err() != nil {
		return
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return
	}
	m.checkAndRecover(ctx)
	if ctx.Err() != nil {
		return
	}
	m.watchSnapshots(ctx)
}

func (m *microOSBackend) WatchAfterInitial(ctx context.Context) error {
	return m.watch(ctx, false)
}

func (m *microOSBackend) watch(ctx context.Context, runInitial bool) error {
	if !m.supported {
		// Just block until done if unsupported, to satisfy interface
		<-ctx.Done()
		return nil
	}
	// This probe may shell out and therefore belongs to the post-Ready update
	// owner, not construction. Probe failure preserves the marker below.
	if runInitial {
		m.cleanupStaleState(ctx)
	}

	// Run an immediate check (in a goroutine to not block startup if called synchronously)
	if runInitial {
		go func() {
			// Small delay to let system settle
			timer := time.NewTimer(10 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return
			}
			m.checkAndRecover(ctx)
			m.watchSnapshots(ctx)
		}()
	}

	recoveryTicker := time.NewTicker(15 * time.Minute)
	defer recoveryTicker.Stop()

	snapshotTicker := time.NewTicker(2 * time.Minute)
	defer snapshotTicker.Stop()

	statusRefreshTicker := time.NewTicker(statusRefreshInterval)
	defer statusRefreshTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-recoveryTicker.C:
			m.checkAndRecover(ctx)
		case <-snapshotTicker.C:
			m.watchSnapshots(ctx)
		case <-statusRefreshTicker.C:
			m.refreshStatusCache(ctx)
		}
	}
}

func (m *microOSBackend) refreshStatusCache(ctx context.Context) {
	sampleStart := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, m.statusRefreshTimeout)
	defer cancel()

	snapshot, snapshotErr := m.snapshotState(probeCtx)
	if snapshot.Readiness == SnapshotReadinessInProgress {
		return
	}
	if snapshotErr != nil {
		m.publishStatusSnapshot(m.fastCoreStatus(snapshotErr, snapshot, nil), sampleStart)
		return
	}

	if ok, fields := m.tryBeginStatusEnrichment(time.Now()); !ok {
		m.publishStatusSnapshot(m.fastCoreStatus(nil, snapshot, fields), sampleStart)
		return
	}
	m.refreshStatusCacheEnrichment(probeCtx, sampleStart, snapshot)
}

func (m *microOSBackend) refreshStatusCacheLocked(ctx context.Context) {
	sampleStart := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, m.statusRefreshTimeout)
	defer cancel()
	snapshot, snapshotErr := m.snapshotState(probeCtx)
	if snapshot.Readiness == SnapshotReadinessInProgress {
		m.finishStatusEnrichment(nil)
		return
	}
	if snapshotErr != nil {
		m.finishStatusEnrichment(nil)
		m.publishStatusSnapshot(m.fastCoreStatus(snapshotErr, snapshot, nil), sampleStart)
		return
	}
	m.refreshStatusCacheEnrichment(probeCtx, sampleStart, snapshot)
}

func (m *microOSBackend) refreshStatusCacheEnrichment(ctx context.Context, sampleStart time.Time, snapshot SnapshotState) {
	st, err := m.readStatus(ctx)
	if ctx.Err() != nil {
		m.finishStatusEnrichment(err, ctx.Err())
		m.publishStatusSnapshot(m.fastCoreStatus(firstNonNil(err, ctx.Err()), snapshot, nil), sampleStart)
		return
	}
	m.finishStatusEnrichment(err)
	st = m.applySnapshotState(st, snapshot)
	m.publishStatusSnapshot(st, sampleStart)
}

// watchSnapshots validates any staged snapshot and auto-reverts if it's missing
// critical components. Silent operation — consistent with checkAndRecover pattern.
// Delegates to validateStagedSnapshot which handles the no-staged-snapshot case.
func (m *microOSBackend) watchSnapshots(ctx context.Context) {
	if m.isInProgress(ctx) {
		return
	}

	defaultID, err := m.validateStagedSnapshot(ctx)
	if err != nil {
		// Only cleanup for confirmed content failures, not lookup errors
		if !errors.Is(err, ErrSnapshotValidationFailed) {
			return
		}

		if revertErr := m.revertDefaultSnapshot(ctx); revertErr != nil {
			return
		}
		// Best-effort delete the bad snapshot
		if defaultID != "" {
			_, _, _, _ = m.runner.Run(ctx, "snapper", "delete", defaultID)
		}
	}
}

func (m *microOSBackend) checkAndRecover(ctx context.Context) {
	probeCtx, cancel := context.WithTimeout(ctx, m.statusRefreshTimeout)
	defer cancel()

	// 1. Sample fast snapshot readiness. Recovery only proceeds when we have a
	// proven absent staged snapshot; unknown/in-progress/staged all pause.
	snapshot, err := m.snapshotState(probeCtx)
	if err != nil {
		return
	}

	if snapshot.Readiness != SnapshotReadinessAbsent {
		return
	}

	// 2. Did the last OS update fail? last_run reflects transactional-update.service
	//    (the OS timer's unit) — the signal piccolod reacts to. This must be a
	//    fresh bounded probe, not stale status enrichment.
	lastRun := m.lastRunInfo(probeCtx)
	if probeCtx.Err() != nil || lastRun == nil || lastRun.RanAt == nil {
		return
	}
	if lastRun.ExitCode == 0 {
		// The OS update path is healthy again — clear the breaker and health.
		m.resetAutoRecovery()
		return
	}

	// 5. Already responded to this exact failed run? Our fallback runs as a
	//    separate piccolo-tu-* unit and doesn't replace last_run, so without this
	//    we'd re-fire for the same failure every tick until the cap.
	if m.failureAlreadyHandled(*lastRun.RanAt) {
		return
	}

	// 5b. Some transactional-update action already responded to this failed run
	//     since it ran — a manual rollback/apply, or an auto-fallback recorded by
	//     an older daemon before the ledger existed (the upgrade path most likely
	//     to hit this). Those actions run as separate units and don't clear
	//     last_run, so without this we'd fire on top of an operator's fix or
	//     repeat a pre-ledger recovery. This is a best-effort, legacy-compatible
	//     layer; the robust Equal-based dedup above (plus the breaker cap) is what
	//     actually prevents a loop, so this timestamp compare can't reintroduce one.
	if st := m.loadState(); st != nil && st.ExitCode == 0 &&
		(st.LastAction == "apply" || st.LastAction == "rollback" || st.LastAction == "auto-fallback") &&
		st.RequestedAt.After(*lastRun.RanAt) {
		return
	}

	// 6. Reserve a slot in the rolling window before firing — atomically, so
	//    concurrent ticks can't both slip past the cap. The window re-arms on
	//    expiry, so this throttles rather than permanently disables. Stays at
	//    LevelWarn (never LevelError): update health feeds the readiness probe,
	//    and a boot-fatal status there can trigger a snapshot rollback — an
	//    inability to auto-recover must not roll back an otherwise-healthy boot.
	if !m.tryReserveAutoRecovery() {
		m.escalateHealth(health.LevelWarn,
			"OS auto-recovery paused after repeated failures; manual intervention may be required")
		return
	}

	// 7. Fire the narrow agent-package fallback. The headroom gate inside
	//    runTransactionalUpdate refuses (ErrInsufficientDisk) if the disk is low.
	cmd := []string{
		"transactional-update",
		"--non-interactive",
		"pkg", "update", "piccolo-os-support", "piccolod",
	}
	switch err := m.runTransactionalUpdate(ctx, cmd, "auto-fallback", "", false); {
	case err == nil:
		// Launched (async). Only a real launch creates a snapshot, so only here do
		// we keep the reserved slot and mark this failure handled.
		m.markFailureHandled(*lastRun.RanAt)
		m.escalateHealth(health.LevelWarn, "attempting OS auto-recovery (agent packages)")
	case errors.Is(err, ErrInsufficientDisk):
		// Gate refused — no snapshot created; return the slot and leave the failure
		// unhandled so recovery retries once space frees.
		m.releaseAutoRecovery()
		m.escalateHealth(health.LevelWarn,
			"OS auto-recovery suspended: insufficient disk space; free space to allow updates")
	default:
		// ErrInProgress, or a transient launch failure (e.g. systemd/D-Bus): no
		// snapshot was created and nothing was handled — return the slot so a
		// failed launch can't exhaust the budget, and let a later tick retry.
		m.releaseAutoRecovery()
	}
}

// createsSnapshot reports whether an action produces a new btrfs snapshot (and
// thus consumes disk). Rollback only repoints the default subvolume.
func createsSnapshot(action string) bool {
	return action == "apply" || action == "auto-fallback"
}

// tryReserveAutoRecovery atomically rolls the window if expired and, when the cap
// has room, reserves a slot (increments) and returns true. Performing the cap
// check and the increment under the same lock is what stops concurrent ticks from
// both slipping past the cap. A reserved slot that does not result in a launch is
// returned via releaseAutoRecovery.
func (m *microOSBackend) tryReserveAutoRecovery() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	if m.autoRecoveryWindowStart.IsZero() || now.Sub(m.autoRecoveryWindowStart) > autoRecoveryWindow {
		m.autoRecoveryWindowStart = now
		m.autoRecoveryCount = 0
	}
	if m.autoRecoveryCount >= maxAutoRecoveryPerWindow {
		return false
	}
	m.autoRecoveryCount++
	m.persistLedgerLocked() // under m.mu so ledger writes can't reorder vs the count
	return true
}

// releaseAutoRecovery rolls back a reservation when the fire did not launch
// (a concurrent run won, or the disk gate refused) so it isn't charged to the
// window.
func (m *microOSBackend) releaseAutoRecovery() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.autoRecoveryCount > 0 {
		m.autoRecoveryCount--
	}
	m.persistLedgerLocked()
}

// failureAlreadyHandled reports whether auto-recovery already responded to the
// failed run identified by ranAt. Because our fallback runs as a separate
// piccolo-tu-* unit, it does not replace the sampled transactional-update.service
// run — so without this we would re-fire for the same failure on every tick.
func (m *microOSBackend) failureAlreadyHandled(ranAt time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.lastHandledFailure.IsZero() && m.lastHandledFailure.Equal(ranAt)
}

// markFailureHandled records (and persists) that we have launched recovery for
// the failed run identified by ranAt, so later ticks don't re-fire for it.
func (m *microOSBackend) markFailureHandled(ranAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastHandledFailure = ranAt
	m.persistLedgerLocked()
}

// resetAutoRecovery clears the breaker after a verified-healthy update and lowers
// the health key — but only if something actually changed, to avoid re-stamping.
func (m *microOSBackend) resetAutoRecovery() {
	m.mu.Lock()
	changed := m.autoRecoveryCount != 0 || !m.autoRecoveryWindowStart.IsZero() || !m.lastHandledFailure.IsZero()
	m.autoRecoveryCount = 0
	m.autoRecoveryWindowStart = time.Time{}
	m.lastHandledFailure = time.Time{}
	if changed {
		m.persistLedgerLocked()
	}
	m.mu.Unlock()
	if changed {
		m.escalateHealth(health.LevelOK, "OS updates healthy")
	}
}

// escalateHealth reports an update-health transition via the injected reporter,
// suppressing repeats (set-once) so a stuck condition doesn't re-stamp every tick.
func (m *microOSBackend) escalateHealth(level health.Level, msg string) {
	m.mu.Lock()
	if m.setHealth == nil || (level == m.lastHealthLevel && msg == m.lastHealthMsg) {
		m.mu.Unlock()
		return
	}
	m.lastHealthLevel, m.lastHealthMsg = level, msg
	report := m.setHealth
	m.mu.Unlock()
	report(level, msg)
}

type recoveryLedger struct {
	Count            int       `json:"count"`
	WindowStart      time.Time `json:"window_start"`
	HandledFailureAt time.Time `json:"handled_failure_at"`
}

// loadRecoveryLedger restores the breaker counters at startup (best-effort).
func (m *microOSBackend) loadRecoveryLedger() {
	b, err := os.ReadFile(m.recoveryPath)
	if err != nil {
		return
	}
	var l recoveryLedger
	if json.Unmarshal(b, &l) != nil {
		return
	}
	// Clamp against a corrupt or tampered ledger: it can neither lift the cap nor
	// (via a future window start that never elapses) permanently disable recovery.
	if l.Count < 0 {
		l.Count = 0
	}
	if l.Count > maxAutoRecoveryPerWindow {
		l.Count = maxAutoRecoveryPerWindow
	}
	if l.WindowStart.After(m.clock()) {
		l.WindowStart = time.Time{}
	}
	m.autoRecoveryCount = l.Count
	m.autoRecoveryWindowStart = l.WindowStart
	m.lastHandledFailure = l.HandledFailureAt
}

// persistLedgerLocked mirrors the breaker state to disk. The caller must hold
// m.mu — keeping the write under the lock serializes ledger writes with the count
// mutations so an older snapshot can't overwrite a newer one. Best-effort: a
// failure under a read-only fs is harmless because the in-memory state stays
// authoritative and the headroom gate is the protection in that state.
func (m *microOSBackend) persistLedgerLocked() {
	if m.recoveryPath == "" {
		return
	}
	l := recoveryLedger{
		Count:            m.autoRecoveryCount,
		WindowStart:      m.autoRecoveryWindowStart,
		HandledFailureAt: m.lastHandledFailure,
	}
	_ = os.MkdirAll(filepath.Dir(m.recoveryPath), 0o700)
	_ = fsutil.AtomicWriteFile(m.recoveryPath, mustJSON(l), 0o600)
}

// statfsFree returns the bytes available to unprivileged writers on path's
// filesystem (btrfs AVAIL is pool-wide).
func statfsFree(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// validateStagedSnapshot checks that the default (staged) snapshot contains
// critical system binaries. Returns the resolved default snapshot ID and nil
// if no staged snapshot exists or if the snapshot looks healthy. Returns an
// error wrapping ErrSnapshotValidationFailed only when actual content validation
// fails (missing binaries). Lookup/probe failures return a plain error to
// prevent callers from running destructive cleanup (revert+delete) on what may
// be a healthy snapshot. Callers should use the returned defaultID for cleanup
// instead of re-resolving it (avoids TOCTOU races).
func (m *microOSBackend) validateStagedSnapshot(ctx context.Context) (string, error) {
	activeID, _ := m.activeSnapshot(ctx)
	rawDefaultID := m.defaultSnapshot(ctx)
	if rawDefaultID == "" {
		return "", fmt.Errorf("cannot determine default snapshot")
	}
	defaultID := m.snapperNumberFromID(ctx, rawDefaultID)

	// No staged snapshot — nothing to validate
	if defaultID == activeID {
		return defaultID, nil
	}

	snapshotRoot := filepath.Join(m.snapshotsDir, defaultID, "snapshot")

	// Critical binaries that must exist
	criticalPaths := []string{
		"usr/lib/systemd/systemd",
		"usr/sbin/cryptsetup",
		"usr/sbin/ip",
	}
	var missing []string
	for _, p := range criticalPaths {
		if _, err := os.Stat(filepath.Join(snapshotRoot, p)); err != nil {
			missing = append(missing, p)
		}
	}

	// At least one kernel image must exist
	kernelGlobs := []string{
		filepath.Join(snapshotRoot, "usr/lib/modules/*/vmlinuz"),
		filepath.Join(snapshotRoot, "usr/lib/modules/*/Image"),
	}
	hasKernel := false
	for _, pattern := range kernelGlobs {
		if matches, err := filepath.Glob(pattern); err == nil && len(matches) > 0 {
			hasKernel = true
			break
		}
	}
	if !hasKernel {
		missing = append(missing, "usr/lib/modules/*/vmlinuz|Image")
	}

	if len(missing) > 0 {
		return defaultID, fmt.Errorf("staged snapshot %s missing critical components %v: %w", defaultID, missing, ErrSnapshotValidationFailed)
	}
	return defaultID, nil
}

// revertDefaultSnapshot sets the active snapshot as the btrfs default,
// effectively un-staging a bad pending snapshot.
func (m *microOSBackend) revertDefaultSnapshot(ctx context.Context) error {
	activeID, _ := m.activeSnapshot(ctx)
	if activeID == "" {
		return fmt.Errorf("cannot determine active snapshot for revert")
	}
	_, _, code, err := m.runner.Run(ctx, "snapper", "modify", "--default", activeID)
	if err != nil || code != 0 {
		return fmt.Errorf("snapper modify --default %s failed (code %d): %w", activeID, code, err)
	}
	return nil
}

// ---- internals ----

type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, stderr string, exitCode int, err error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkClassFromContext(ctx, pressure.WorkUpdate)); err != nil {
		return "", "", -1, err
	}
	defer pressure.BeginLifecycleOwner("update")()
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		code = -1
	}
	return string(output), string(output), code, err
}

type snapshotInfo struct {
	ID          string     `json:"id"`
	Description string     `json:"description,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
}

type lastRunInfo struct {
	Result   string     `json:"result,omitempty"`
	ExitCode int        `json:"exit_code,omitempty"`
	RanAt    *time.Time `json:"ran_at,omitempty"`
	Logs     []string   `json:"logs,omitempty"`
}

type persistedState struct {
	LastAction      string    `json:"last_action,omitempty"`
	TargetSnapshot  string    `json:"target_snapshot,omitempty"`
	RequestedAt     time.Time `json:"requested_at,omitempty"`
	UnitName        string    `json:"unit_name,omitempty"`
	ExitCode        int       `json:"exit_code,omitempty"`
	ImmediateResult string    `json:"immediate_result,omitempty"`
	Message         string    `json:"message,omitempty"`
}

func (m *microOSBackend) detectSupported() bool {
	needed := []string{"transactional-update", "snapper", "btrfs", "findmnt", "systemctl"}
	for _, bin := range needed {
		if _, err := exec.LookPath(bin); err != nil {
			return false
		}
	}
	return true
}

func (m *microOSBackend) readStatus(ctx context.Context) (Status, error) {
	var enrichmentErrs enrichmentErrorCollector
	meta := make(map[string]interface{})
	meta["supported"] = true

	activeID, activeSrc := m.activeSnapshot(ctx)
	rawDefaultID := m.defaultSnapshot(ctx)
	defaultID := m.snapperNumberFromID(ctx, rawDefaultID)

	meta["active_snapshot_source"] = activeSrc
	meta["active_snapshot_id"] = activeID
	meta["default_snapshot_id"] = defaultID

	snapshots, err := m.snapperSnapshots(ctx)
	enrichmentErrs.add("snapper --json list", err)
	if active, ok := snapshots[activeID]; ok {
		meta["active_snapshot"] = active
	}
	if def, ok := snapshots[defaultID]; ok {
		meta["default_snapshot"] = def
	}

	stagedID := ""
	if defaultID != "" && activeID != "" && defaultID != activeID {
		stagedID = defaultID
		if staged, ok := snapshots[stagedID]; ok {
			meta["staged_snapshot"] = staged
		}
	}

	lastRun := m.lastRunInfo(ctx)
	if lastRun != nil {
		meta["last_run"] = lastRun
	}

	if cnt, ok, err := m.rpmUpdateCount(ctx); err != nil {
		enrichmentErrs.add("zypper --xmlout lu", err)
	} else if ok {
		meta["rpm_updates_available"] = cnt
	}

	activePiccolo, err := m.queryRPM(ctx, "piccolod", "")
	enrichmentErrs.add("rpm -q piccolod", err)
	if activePiccolo != "" {
		meta["piccolod_active"] = activePiccolo
	}
	if stagedID != "" {
		stagedRoot := filepath.Join(m.snapshotsDir, stagedID, "snapshot")
		stagedPiccolo, err := m.queryRPM(ctx, "piccolod", stagedRoot)
		enrichmentErrs.add("rpm --root staged -q piccolod", err)
		if stagedPiccolo != "" && stagedPiccolo != activePiccolo {
			meta["piccolod_staged"] = stagedPiccolo
		}
	}

	intent := m.loadState()
	derived := ""
	if intent != nil {
		meta["last_request"] = intent
		derived = m.deriveOutcome(activeID, defaultID, intent)
	}
	if derived == "" && defaultID != "" && activeID != "" && defaultID != activeID {
		// Fallback: if we have no record of the request but the system effectively
		// has a pending update (default != active), report it.
		derived = "pending-reboot"
	}
	if derived != "" {
		meta["derived_outcome"] = derived
	}

	pending := stagedID != ""

	// Strategy: "Piccolo OS" version is the piccolod RPM version.
	// Fallback to underlying OS version (os-release) only if RPM is not installed (e.g. dev).
	osVersion := m.getOSReleaseVersion("")
	meta["os_version"] = osVersion

	currentVersion := m.currentVersion
	if currentVersion == "" {
		currentVersion = activePiccolo
	}
	if currentVersion == "" {
		currentVersion = osVersion
	}
	if currentVersion == "" {
		currentVersion = humanSnapshotLabel(activeID, snapshots)
	}

	availableVersion := currentVersion
	if stagedID != "" {
		// If we have a staged RPM, that's the new version.
		if stagedV := meta["piccolod_staged"]; stagedV != nil {
			availableVersion = stagedV.(string)
		} else if activePiccolo == "" {
			// Fallback path: if no RPM, check staged OS version
			stagedRoot := filepath.Join(m.snapshotsDir, stagedID, "snapshot")
			if vStaged := m.getOSReleaseVersion(stagedRoot); vStaged != "" {
				availableVersion = vStaged
			} else {
				// If OS version string is missing/empty, append system update ID to current app version
				// so it doesn't look like a downgrade or weird number (e.g. "269").
				availableVersion = fmt.Sprintf("%s (System Update %s)", currentVersion, stagedID)
			}
		}
	}

	st := Status{
		CurrentVersion:   currentVersion,
		AvailableVersion: availableVersion,
		Pending:          pending,
		RequiresReboot:   pending,
		LastChecked:      m.clock(),
		Meta:             meta,
	}
	if err := enrichmentErrs.err(); err != nil {
		st = withStatusMeta(st, map[string]interface{}{
			"degraded":        true,
			"degraded_reason": err.Error(),
		})
		return st, err
	}
	return st, nil
}

func (m *microOSBackend) getOSReleaseVersion(root string) string {
	path := filepath.Join(root, "etc", "os-release")
	if root == "" {
		path = "/etc/os-release"
	}
	data, err := m.readFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	var prettyName, versionID string
	for _, line := range lines {
		if strings.HasPrefix(line, "VERSION_ID=") {
			versionID = strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), `"`)
		}
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			prettyName = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	if versionID != "" {
		return versionID
	}
	return prettyName
}

func (m *microOSBackend) runTransactionalUpdate(ctx context.Context, cmd []string, action string, targetHint string, wait bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.isInProgress(ctx) {
		return ErrInProgress
	}

	// Disk-headroom gate. Snapshot-creating updates (apply, auto-fallback) are
	// refused when free space is below the floor one update needs to complete —
	// this stops a runaway from filling the disk and preserves room for the OS
	// timer's own recovery dup. Rollback is exempt: it repoints the default
	// snapshot (creates nothing) and must run to escape a full disk. statfs is a
	// pure read, so the gate holds even after the fs has gone read-only, and it
	// runs before any persistent-pool write below.
	if createsSnapshot(action) {
		if free, err := m.freeBytes(m.snapshotsDir); err != nil || free < minFreeBytesForRecovery {
			return ErrInsufficientDisk
		}
	}

	unit := fmt.Sprintf("piccolo-tu-%s-%d", action, m.clock().Unix())
	runCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	marker := filepath.Join(m.runtimeDir, "update.inprogress")
	_ = fsutil.AtomicWriteFile(marker, []byte(action+" "+unit), 0o644)
	if wait {
		defer os.Remove(marker)
	}
	// Persist intent immediately so we survive restarts during TU
	m.persistState(action, targetHint, unit, -1, "started")
	runCtx = pressure.WithTransitionContinuation(runCtx)
	// Enforce 1h hard timeout at the systemd level to kill hung zypper processes.
	args := []string{"--unit", unit, "--property=RuntimeMaxSec=3600"}
	if wait {
		args = append(args, "--wait")
	}
	args = append(args, cmd...)
	stdout, stderr, code, err := m.runner.Run(runCtx, "systemd-run", args...)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		// Best-effort: stop the transient unit to avoid a still-running TU after timeout.
		stopCtx, cancelStop := context.WithTimeout(pressure.WithTransitionContinuation(context.Background()), 30*time.Second)
		_, _, _, _ = m.runner.Run(stopCtx, "systemctl", "stop", unit)
		cancelStop()
		m.persistState(action, "", unit, code, "timeout")
		return ErrTimeout
	}
	if code != 0 {
		m.persistState(action, "", unit, code, strings.TrimSpace(stderr))
		// systemd may reject when already running; translate if obvious
		if strings.Contains(stderr, "already running") || strings.Contains(stderr, "Unit") && strings.Contains(stderr, "is queued") {
			return ErrInProgress
		}
		if err != nil {
			return fmt.Errorf("transactional-update %s failed (code %d): %w", action, code, err)
		}
		return fmt.Errorf("transactional-update %s failed (code %d): %s", action, code, stderr)
	}

	targetID := targetHint
	if targetID == "" {
		// Best-effort bounded fast probe; do not enter the snapper/zypper/RPM
		// status path while a just-launched transactional update may still own
		// the snapshot producer state.
		targetCtx, cancelTarget := context.WithTimeout(context.Background(), m.statusRequestTimeout)
		if defaultID, _, err := m.defaultSnapshotNumber(targetCtx); err == nil && defaultID != "" {
			targetID = defaultID
		}
		cancelTarget()
	}
	msg := strings.TrimSpace(stdout)
	if msg == "" {
		msg = strings.TrimSpace(stderr)
	}
	m.persistState(action, targetID, unit, code, msg)
	return nil
}

func (m *microOSBackend) cleanupStaleState(ctx context.Context) {
	// Re-use logic from isInProgress to check and clean marker
	// We just ignore the "true" return value, as we only care about the side effect
	// of removing the file if the unit is dead.
	_ = m.isInProgress(ctx)
}

func (m *microOSBackend) isInProgress(ctx context.Context) bool {
	marker := filepath.Join(m.runtimeDir, "update.inprogress")
	if data, err := os.ReadFile(marker); err == nil {
		fields := strings.Fields(string(data))
		unit := ""
		if len(fields) > 1 {
			unit = fields[1]
		}
		if unit != "" {
			if _, _, code, probeErr := m.runner.Run(ctx, "systemctl", "is-active", "--quiet", unit); code == 0 {
				return true
			} else if probeErr != nil {
				// Admission pressure, timeout, and control-plane errors are not
				// proof that the durable operation is stale.
				if ctx.Err() != nil {
					return false
				}
				return true
			}
			if ctx.Err() != nil {
				return false
			}
		}
		// Clean up stale marker
		_ = os.Remove(marker)
	}

	// Check for any running piccolo-tu-* transient units
	stdout, _, _, _ := m.runner.Run(ctx, "systemctl", "list-units", "--type=service", "--state=running", "--no-legend", "--plain", "piccolo-tu-*")
	if ctx.Err() != nil {
		return false
	}
	if strings.TrimSpace(stdout) != "" {
		return true
	}

	// Fall back to transactional-update.service (legacy) just in case
	if _, _, code, _ := m.runner.Run(ctx, "systemctl", "is-active", "--quiet", transactionalUpdateUnit); code == 0 {
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	return false
}

func (m *microOSBackend) persistState(action, target, unit string, exit int, msg string) {
	ps := persistedState{
		LastAction:      action,
		TargetSnapshot:  target,
		RequestedAt:     m.clock(),
		UnitName:        unit,
		ExitCode:        exit,
		ImmediateResult: resultFromExit(exit),
		Message:         msg,
	}
	_ = os.MkdirAll(filepath.Dir(m.statePath), 0o700)
	_ = fsutil.AtomicWriteFile(m.statePath, mustJSON(ps), 0o600)
}

func (m *microOSBackend) loadState() *persistedState {
	b, err := os.ReadFile(m.statePath)
	if err != nil {
		return nil
	}
	var ps persistedState
	if err := json.Unmarshal(b, &ps); err != nil {
		return nil
	}
	return &ps
}

func (m *microOSBackend) deriveOutcome(activeID, defaultID string, ps *persistedState) string {
	if ps == nil || ps.TargetSnapshot == "" {
		return ""
	}
	if activeID == ps.TargetSnapshot {
		return "applied"
	}
	if ps.LastAction == "apply" && defaultID == ps.TargetSnapshot {
		return "pending-reboot"
	}
	return "not-applied"
}

func (m *microOSBackend) snapshotState(ctx context.Context) (SnapshotState, error) {
	state := SnapshotState{
		Readiness: SnapshotReadinessUnknown,
		Source:    "btrfs",
	}
	if m.isInProgress(ctx) {
		state.Readiness = SnapshotReadinessInProgress
		return state, nil
	}
	if err := ctx.Err(); err != nil {
		return state, err
	}

	activeID, activeSrc := m.activeSnapshot(ctx)
	state.ActiveSnapshot = activeID
	state.ActiveSnapshotSource = activeSrc
	if err := ctx.Err(); err != nil {
		return state, err
	}
	if activeID == "" || activeID == activeSrc {
		return state, fmt.Errorf("cannot normalize active snapshot")
	}

	defaultID, defaultSrc, err := m.defaultSnapshotNumber(ctx)
	state.DefaultSnapshot = defaultID
	state.DefaultSnapshotSource = defaultSrc
	if err != nil {
		return state, err
	}
	if err := ctx.Err(); err != nil {
		return state, err
	}
	if defaultID == "" {
		return state, fmt.Errorf("cannot normalize default snapshot")
	}

	if defaultID == activeID {
		state.Readiness = SnapshotReadinessAbsent
		state.RequiresReboot = false
		return state, nil
	}
	state.Readiness = SnapshotReadinessStaged
	state.RequiresReboot = true
	return state, nil
}

func (m *microOSBackend) snapperNumberFromID(ctx context.Context, id string) string {
	if snapperID, ok := m.snapperNumberFromBtrfsID(ctx, id); ok {
		return snapperID
	}
	return id
}

func (m *microOSBackend) snapperNumberFromBtrfsID(ctx context.Context, id string) (string, bool) {
	// btrfs subvolume list output example:
	// ID 269 gen 59 top level 257 path @/.snapshots/2/snapshot
	stdout, _, _, err := m.runner.Run(ctx, "btrfs", "subvolume", "list", "/")
	if err != nil {
		return "", false
	}
	lines := strings.Split(stdout, "\n")
	for _, line := range lines {
		if strings.Contains(line, fmt.Sprintf("ID %s ", id)) {
			// Found the ID, now extract path
			// path @/.snapshots/2/snapshot
			if snapperID, ok := snapshotNumberFromPath(line); ok {
				return snapperID, true
			}
			return "", false
		}
	}
	return "", false
}

func (m *microOSBackend) defaultSnapshotNumber(ctx context.Context) (string, string, error) {
	stdout, _, _, err := m.runner.Run(ctx, "btrfs", "subvolume", "get-default", "/")
	if err != nil {
		return "", strings.TrimSpace(stdout), err
	}
	src := strings.TrimSpace(stdout)
	if src == "" {
		return "", src, fmt.Errorf("cannot determine default snapshot")
	}
	if snapperID, ok := snapshotNumberFromPath(src); ok {
		return snapperID, src, nil
	}
	re := regexp.MustCompile(`(?i)ID\s+(\d+)`)
	if match := re.FindStringSubmatch(src); len(match) == 2 {
		if snapperID, ok := m.snapperNumberFromBtrfsID(ctx, match[1]); ok {
			return snapperID, src, nil
		}
		return "", src, fmt.Errorf("cannot normalize default snapshot ID %s", match[1])
	}
	return "", src, fmt.Errorf("cannot parse default snapshot")
}

func (m *microOSBackend) activeSnapshot(ctx context.Context) (string, string) {
	stdout, _, _, err := m.runner.Run(ctx, "findmnt", "-no", "SOURCE", "/")
	if err != nil {
		return "", ""
	}
	src := strings.TrimSpace(stdout)
	return snapshotIDFromPath(src), src
}

func (m *microOSBackend) defaultSnapshot(ctx context.Context) string {
	stdout, _, _, err := m.runner.Run(ctx, "btrfs", "subvolume", "get-default", "/")
	if err != nil {
		return ""
	}
	re := regexp.MustCompile(`(?i)ID\s+(\d+)`)
	if match := re.FindStringSubmatch(stdout); len(match) == 2 {
		return match[1]
	}
	return strings.TrimSpace(stdout)
}

func snapshotIDFromPath(path string) string {
	if id, ok := snapshotNumberFromPath(path); ok {
		return id
	}
	return path
}

func snapshotNumberFromPath(path string) (string, bool) {
	re := regexp.MustCompile(`/\.snapshots/(\d+)/snapshot`)
	if match := re.FindStringSubmatch(path); len(match) == 2 {
		return match[1], true
	}
	return "", false
}

// snapper --json list parser (best effort, with failure signal for backoff).
func (m *microOSBackend) snapperSnapshots(ctx context.Context) (map[string]snapshotInfo, error) {
	stdout, stderr, code, err := m.runner.Run(ctx, "snapper", "--json", "list")
	if err := commandFailure(stderr, code, err); err != nil {
		return map[string]snapshotInfo{}, err
	}

	// Tumbleweed snapper returns: { "root": [ { "number": 0, ... } ] }
	// Older/Other versions might return: { "configs": [ { "config": "root", "snapshots": ... } ] }
	// We try to decode into a generic map to support the observed format first.
	var simpleFormat map[string][]struct {
		Number      int    `json:"number"`
		Date        string `json:"date"`
		Description string `json:"description"`
		Type        string `json:"type"`
	}

	var chosen []struct {
		Number      int    `json:"number"`
		Date        string `json:"date"`
		Description string `json:"description"`
		Type        string `json:"type"`
	}

	simpleErr := json.Unmarshal([]byte(stdout), &simpleFormat)
	if simpleErr == nil && len(simpleFormat["root"]) > 0 {
		chosen = simpleFormat["root"]
	} else {
		// Fallback to the nested "configs" format
		var complexFormat struct {
			Configs []struct {
				Config    string `json:"config"`
				Snapshots []struct {
					Number      int    `json:"number"`
					Date        string `json:"date"`
					Description string `json:"description"`
					Type        string `json:"type"`
				} `json:"snapshots"`
			} `json:"configs"`
		}
		complexErr := json.Unmarshal([]byte(stdout), &complexFormat)
		if complexErr != nil {
			return map[string]snapshotInfo{}, fmt.Errorf("parse snapper list JSON: %w", firstNonNil(simpleErr, complexErr))
		}
		if complexErr == nil {
			for _, cfg := range complexFormat.Configs {
				if cfg.Config == "root" {
					chosen = cfg.Snapshots
					break
				}
			}
			if chosen == nil && len(complexFormat.Configs) > 0 {
				chosen = complexFormat.Configs[0].Snapshots
			}
		}
	}

	out := make(map[string]snapshotInfo)
	for _, snap := range chosen {
		id := strconv.Itoa(snap.Number)
		var ts *time.Time
		if snap.Date != "" {
			if parsed, err := time.Parse("2006-01-02 15:04:05", snap.Date); err == nil {
				ts = &parsed
			}
		}
		out[id] = snapshotInfo{ID: id, Description: snap.Description, CreatedAt: ts}
	}
	return out, nil
}

func (m *microOSBackend) pickRollbackTarget(ctx context.Context) string {
	activeID, _ := m.activeSnapshot(ctx)
	defaultID := m.defaultSnapshot(ctx)
	snaps, _ := m.snapperSnapshots(ctx)
	bestID := ""
	bestNum := -1
	for id := range snaps {
		if id == activeID || id == defaultID {
			continue
		}
		if n, err := strconv.Atoi(id); err == nil {
			if n > bestNum {
				bestNum = n
				bestID = id
			}
		}
	}
	return bestID
}

func (m *microOSBackend) lastRunInfo(ctx context.Context) *lastRunInfo {
	// Parse by key (Key=Value), not by position: `systemctl show --value` does NOT
	// emit properties in -p order, and an empty value shifts positional parsing —
	// which previously made ExitCode always 0 and RanAt always nil on real devices.
	//
	// Use ExecMainExitTimestamp (when the main process exited) for RanAt, NOT
	// ActiveEnterTimestamp: transactional-update.service is Type=oneshot, and a
	// FAILED dup goes activating->failed without ever becoming "active", so
	// ActiveEnterTimestamp is EMPTY exactly on failure (verified on-device) — the
	// one case we must detect. ExecMainExitTimestamp is set on both success and
	// failure. isInProgress already gates out a still-running unit, so the main
	// process has exited by the time we read this.
	stdout, _, _, err := m.runner.Run(ctx, "systemctl", "show", transactionalUpdateUnit, "-p", "Result", "-p", "ExecMainStatus", "-p", "ExecMainExitTimestamp")
	if err != nil || strings.TrimSpace(stdout) == "" {
		return nil
	}
	info := &lastRunInfo{}
	for _, line := range strings.Split(stdout, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "Result":
			info.Result = val
		case "ExecMainStatus":
			if code, err := strconv.Atoi(val); err == nil {
				info.ExitCode = code
			}
		case "ExecMainExitTimestamp":
			if ts := parseSystemdTime(val); ts != nil {
				info.RanAt = ts
			}
		}
	}
	logs := m.readJournal(ctx, transactionalUpdateUnit)
	if len(logs) > 0 {
		info.Logs = logs
	}
	return info
}

func (m *microOSBackend) readJournal(ctx context.Context, unit string) []string {
	stdout, _, _, err := m.runner.Run(ctx, "journalctl", "-u", unit, "-n", "50", "--no-pager", "--output=cat")
	if err != nil {
		return nil
	}
	lines := splitNonEmpty(strings.Split(stdout, "\n"))
	if len(lines) > 50 {
		lines = lines[len(lines)-50:]
	}
	return lines
}

func (m *microOSBackend) rpmUpdateCount(ctx context.Context) (int, bool, error) {
	stdout, stderr, code, err := m.runner.Run(ctx, "zypper", "--xmlout", "lu")
	if err := commandFailure(stderr, code, err); err != nil {
		return 0, false, err
	}
	count := strings.Count(stdout, "<update")
	return count, true, nil
}

func (m *microOSBackend) queryRPM(ctx context.Context, pkg, root string) (string, error) {
	// Return clean "Version-Release" string (e.g. "0.1.0-1")
	args := []string{"-q", "--qf", "%{VERSION}-%{RELEASE}", pkg}
	if root != "" {
		args = append([]string{"--root", root}, args...)
	}
	stdout, stderr, code, err := m.runner.Run(ctx, "rpm", args...)
	if err := commandFailure(stderr, code, err); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// helpers
func splitNonEmpty(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func parseSystemdTime(val string) *time.Time {
	val = strings.TrimSpace(val)
	if val == "" || strings.EqualFold(val, "n/a") {
		return nil
	}
	// Resolve the zone abbreviation systemd emits (e.g. "IST") against the device's
	// LIVE local zone, not Go's time.Local: time.Local is cached at process start
	// and never refreshed, but piccolod changes the zone at runtime via
	// `timedatectl set-timezone` with no restart, so a cached time.Local would skew
	// the parsed instant by the true UTC offset.
	loc := systemLocation()
	layouts := []string{time.RFC3339, "Mon 2006-01-02 15:04:05 MST", "Mon 2006-01-02 15:04:05 -0700"}
	for _, layout := range layouts {
		if ts, err := time.ParseInLocation(layout, val, loc); err == nil {
			return &ts
		}
	}
	return nil
}

// systemLocation resolves the device's current local zone by reading
// /etc/localtime live (mirrors internal/sysconfig/timezone.Manager.Get and the
// app-handler probe). Read fresh each call so a runtime `timedatectl` change is
// honored without a piccolod restart. Falls back to time.Local. Overridable in tests.
var systemLocation = func() *time.Location {
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		const prefix = "/usr/share/zoneinfo/"
		if idx := strings.Index(target, prefix); idx != -1 {
			if loc, err := time.LoadLocation(target[idx+len(prefix):]); err == nil {
				return loc
			}
		}
	}
	return time.Local
}

func resultFromExit(code int) string {
	if code == 0 {
		return "success"
	}
	if code == -1 {
		return "unknown"
	}
	return "failed"
}

func humanSnapshotLabel(id string, snaps map[string]snapshotInfo) string {
	if s, ok := snaps[id]; ok {
		if s.Description != "" {
			return fmt.Sprintf("%s (%s)", id, s.Description)
		}
		return id
	}
	return id
}

func mustJSON(v interface{}) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return []byte("{}")
	}
	return b
}

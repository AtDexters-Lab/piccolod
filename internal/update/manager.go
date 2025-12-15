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

	"piccolod/internal/state/paths"
)

const (
	defaultTimeout          = 45 * time.Minute
	defaultRuntimeDir       = "/run/piccolo"
	defaultStateSubdir      = "update"
	defaultStateFilename    = "state.json"
	transactionalUpdateUnit = "transactional-update.service"
)

var (
	ErrInProgress      = errors.New("transactional-update in progress")
	ErrUnsupported     = errors.New("transactional-update unsupported on this host")
	ErrInvalidSnapshot = errors.New("invalid snapshot id")
	ErrTimeout         = errors.New("transactional-update timed out")
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

// Manager fronts the OS-specific backend (MicroOS today; pluggable later).
type Manager struct {
	backend osBackend
}

// osBackend defines the per-platform surface.
type osBackend interface {
	Status(context.Context) (Status, error)
	Apply(context.Context) error
	Rollback(context.Context, string) error
	Reboot(context.Context) error
	Watch(context.Context) error
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

	mu              sync.Mutex
	supported       bool
	overrideSupport bool
}

// Option configures the MicroOS backend.
type Option func(*microOSBackend)

// WithRunner injects a custom command runner (used in tests).
func WithRunner(r commandRunner) Option { return func(m *microOSBackend) { m.runner = r } }

// WithClock injects a custom clock (used in tests).
func WithClock(fn func() time.Time) Option { return func(m *microOSBackend) { m.clock = fn } }

// WithTimeout overrides the transactional-update timeout.
func WithTimeout(d time.Duration) Option { return func(m *microOSBackend) { m.timeout = d } }

// WithStateDir overrides the persistent state directory (default PICCOLO_STATE_DIR/update).
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

// NewManager constructs a Manager with an OS backend (MicroOS today).
func NewManager(opts ...Option) (*Manager, error) {
	b, err := newMicroOSBackend(opts...)
	if err != nil {
		return nil, err
	}
	return &Manager{backend: b}, nil
}

// Status returns the current OS update status (graceful on unsupported hosts).
func (m *Manager) Status(ctx context.Context) (Status, error) { return m.backend.Status(ctx) }

// Apply triggers transactional-update dup.
func (m *Manager) Apply(ctx context.Context) error { return m.backend.Apply(ctx) }

// Rollback sets the requested snapshot as default for next boot.
func (m *Manager) Rollback(ctx context.Context, targetID string) error {
	return m.backend.Rollback(ctx, targetID)
}

// Reboot triggers a system reboot.
func (m *Manager) Reboot(ctx context.Context) error {
	return m.backend.Reboot(ctx)
}

// Watch starts a background monitoring loop to detect and recover from update failures.
func (m *Manager) Watch(ctx context.Context) error {
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
		runner:     execRunner{},
		clock:      time.Now,
		timeout:    timeout,
		runtimeDir: defaultRuntimeDir,
		statePath:  filepath.Join(paths.Root(), defaultStateSubdir, defaultStateFilename),
		readFile:   os.ReadFile,
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

	m.supported = m.overrideSupport || m.detectSupported()

	// Proactive cleanup of stale locks on startup
	m.cleanupStaleState(context.Background())

	return m, nil
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

	// If an update is running, return 429 so clients know to poll/wait.
	// This ensures multi-tab consistency (Tab A starts update, Tab B sees 429).
	if m.isInProgress(ctx) {
		return Status{}, ErrInProgress
	}
	return m.readStatus(ctx)
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
	snaps := m.snapperSnapshots(ctx)
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

// Reboot triggers systemctl reboot.
func (m *microOSBackend) Reboot(ctx context.Context) error {
	if !m.supported {
		return ErrUnsupported
	}
	// We use the runner to execute reboot. Note that this will likely terminate the API server
	// before the command returns or shortly after.
	_, _, _, err := m.runner.Run(ctx, "systemctl", "reboot")
	return err
}

// Watch runs a background loop to monitor system update status and trigger fallbacks.
func (m *microOSBackend) Watch(ctx context.Context) error {
	if !m.supported {
		// Just block until done if unsupported, to satisfy interface
		<-ctx.Done()
		return nil
	}

	// Run an immediate check (in a goroutine to not block startup if called synchronously)
	go func() {
		// Small delay to let system settle
		time.Sleep(10 * time.Second)
		m.checkAndRecover(ctx)
	}()

	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.checkAndRecover(ctx)
		}
	}
}

func (m *microOSBackend) checkAndRecover(ctx context.Context) {
	// 1. Check if we are already running something
	if m.isInProgress(ctx) {
		return
	}

	// 2. Check system status
	// Use a detached context for the check so we don't fail if the watch loop is cancelling (though mostly it shares life)
	status, err := m.readStatus(ctx)
	if err != nil {
		return
	}

	// 3. If update is pending (staged), we are good. User needs to reboot.
	if status.Pending {
		return
	}

	// 4. Check Last Run result
	// The map value is *lastRunInfo because readStatus populates it that way.
	lastRun, ok := status.Meta["last_run"].(*lastRunInfo)
	if !ok || lastRun == nil {
		return
	}

	// If success or unknown, do nothing
	if lastRun.ExitCode == 0 {
		return
	}

	// 5. Check if we (or the user) already reacted to this failure.
	if lastRun.RanAt == nil {
		return
	}

	state := m.loadState()
	if state != nil {
		// If our last request was AFTER the system failure, we assume we responded.
		if state.RequestedAt.After(*lastRun.RanAt) {
			return
		}
	}

	// 6. Trigger Fallback
	// "auto-recovery": Update only the agent packages.
	cmd := []string{
		"transactional-update",
		"--non-interactive",
		"pkg", "update", "piccolo-os-support", "piccolod",
	}
	// Fire and forget (wait=false), but we log the start.
	// We use "auto-fallback" as the action name to distinguish from user actions.
	// This will update state.json, preventing a retry loop on the next tick (step 5).
	_ = m.runTransactionalUpdate(ctx, cmd, "auto-fallback", "", false)
}

// ---- internals ----

type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, stderr string, exitCode int, err error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
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
	meta := make(map[string]interface{})
	meta["supported"] = true

	activeID, activeSrc := m.activeSnapshot(ctx)
	rawDefaultID := m.defaultSnapshot(ctx)
	defaultID := m.snapperNumberFromID(ctx, rawDefaultID)

	meta["active_snapshot_source"] = activeSrc
	meta["active_snapshot_id"] = activeID
	meta["default_snapshot_id"] = defaultID

	snapshots := m.snapperSnapshots(ctx)
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

	if cnt, ok := m.rpmUpdateCount(ctx); ok {
		meta["rpm_updates_available"] = cnt
	}

	activePiccolo := m.queryRPM(ctx, "piccolod", "")
	if activePiccolo != "" {
		meta["piccolod_active"] = activePiccolo
	}
	if stagedID != "" {
		stagedRoot := filepath.Join("/.snapshots", stagedID, "snapshot")
		if stagedPiccolo := m.queryRPM(ctx, "piccolod", stagedRoot); stagedPiccolo != "" && stagedPiccolo != activePiccolo {
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
			stagedRoot := filepath.Join("/.snapshots", stagedID, "snapshot")
			if vStaged := m.getOSReleaseVersion(stagedRoot); vStaged != "" {
				availableVersion = vStaged
			} else {
				// If OS version string is missing/empty, append system update ID to current app version
				// so it doesn't look like a downgrade or weird number (e.g. "269").
				availableVersion = fmt.Sprintf("%s (System Update %s)", currentVersion, stagedID)
			}
		}
	}

	return Status{
		CurrentVersion:   currentVersion,
		AvailableVersion: availableVersion,
		Pending:          pending,
		RequiresReboot:   pending,
		LastChecked:      m.clock(),
		Meta:             meta,
	}, nil
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

	unit := fmt.Sprintf("piccolo-tu-%s-%d", action, m.clock().Unix())
	runCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	marker := filepath.Join(m.runtimeDir, "update.inprogress")
	_ = os.WriteFile(marker, []byte(action+" "+unit), 0o644)
	if wait {
		defer os.Remove(marker)
	}
	// Persist intent immediately so we survive restarts during TU
	m.persistState(action, targetHint, unit, -1, "started")
	// Enforce 1h hard timeout at the systemd level to kill hung zypper processes.
	args := []string{"--unit", unit, "--property=RuntimeMaxSec=3600"}
	if wait {
		args = append(args, "--wait")
	}
	args = append(args, cmd...)
	stdout, stderr, code, err := m.runner.Run(runCtx, "systemd-run", args...)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		// Best-effort: stop the transient unit to avoid a still-running TU after timeout.
		stopCtx, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
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
	// Refresh status to capture new default snapshot (best-effort)
	status, _ := m.readStatus(context.Background())
	if targetID == "" {
		if staged, ok := status.Meta["default_snapshot_id"].(string); ok {
			targetID = staged
		}
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
			if _, _, code, _ := m.runner.Run(ctx, "systemctl", "is-active", "--quiet", unit); code == 0 {
				return true
			}
		}
		// Clean up stale marker
		_ = os.Remove(marker)
	}

	// Check for any running piccolo-tu-* transient units
	stdout, _, _, _ := m.runner.Run(ctx, "systemctl", "list-units", "--type=service", "--state=running", "--no-legend", "--plain", "piccolo-tu-*")
	if strings.TrimSpace(stdout) != "" {
		return true
	}

	// Fall back to transactional-update.service (legacy) just in case
	if _, _, code, _ := m.runner.Run(ctx, "systemctl", "is-active", "--quiet", transactionalUpdateUnit); code == 0 {
		return true
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
	_ = os.WriteFile(m.statePath, mustJSON(ps), 0o600)
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

func (m *microOSBackend) snapperNumberFromID(ctx context.Context, id string) string {
	// btrfs subvolume list output example:
	// ID 269 gen 59 top level 257 path @/.snapshots/2/snapshot
	stdout, _, _, err := m.runner.Run(ctx, "btrfs", "subvolume", "list", "/")
	if err != nil {
		return id // fallback
	}
	lines := strings.Split(stdout, "\n")
	for _, line := range lines {
		if strings.Contains(line, fmt.Sprintf("ID %s ", id)) {
			// Found the ID, now extract path
			// path @/.snapshots/2/snapshot
			return snapshotIDFromPath(line)
		}
	}
	return id
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
	re := regexp.MustCompile(`/\.snapshots/(\d+)/snapshot`)
	if match := re.FindStringSubmatch(path); len(match) == 2 {
		return match[1]
	}
	return path
}

// snapper --json list parser (best effort)
func (m *microOSBackend) snapperSnapshots(ctx context.Context) map[string]snapshotInfo {
	stdout, _, _, err := m.runner.Run(ctx, "snapper", "--json", "list")
	if err != nil {
		return map[string]snapshotInfo{}
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

	if err := json.Unmarshal([]byte(stdout), &simpleFormat); err == nil && len(simpleFormat["root"]) > 0 {
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
		if err := json.Unmarshal([]byte(stdout), &complexFormat); err == nil {
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
	return out
}

func (m *microOSBackend) pickRollbackTarget(ctx context.Context) string {
	activeID, _ := m.activeSnapshot(ctx)
	defaultID := m.defaultSnapshot(ctx)
	snaps := m.snapperSnapshots(ctx)
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
	stdout, _, _, err := m.runner.Run(ctx, "systemctl", "show", transactionalUpdateUnit, "-p", "Result", "-p", "ExecMainStatus", "-p", "ActiveEnterTimestamp", "--value")
	if err != nil || strings.TrimSpace(stdout) == "" {
		return nil
	}
	lines := splitNonEmpty(strings.Split(strings.TrimSpace(stdout), "\n"))
	info := &lastRunInfo{}
	if len(lines) > 0 {
		info.Result = strings.TrimSpace(lines[0])
	}
	if len(lines) > 1 {
		if code, err := strconv.Atoi(strings.TrimSpace(lines[1])); err == nil {
			info.ExitCode = code
		}
	}
	if len(lines) > 2 {
		if ts := parseSystemdTime(lines[2]); ts != nil {
			info.RanAt = ts
		}
	}
	logs := m.readJournal(ctx, transactionalUpdateUnit)
	if len(logs) > 0 {
		info.Logs = logs
	}
	return info
}

func (m *microOSBackend) readJournal(ctx context.Context, unit string) []string {
	stdout, _, _, err := m.runner.Run(ctx, "journalctl", "-u", unit, "-n", "10", "--no-pager", "--output=cat")
	if err != nil {
		return nil
	}
	lines := splitNonEmpty(strings.Split(stdout, "\n"))
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return lines
}

func (m *microOSBackend) rpmUpdateCount(ctx context.Context) (int, bool) {
	stdout, _, _, err := m.runner.Run(ctx, "zypper", "--xmlout", "lu")
	if err != nil {
		return 0, false
	}
	count := strings.Count(stdout, "<update")
	return count, true
}

func (m *microOSBackend) queryRPM(ctx context.Context, pkg, root string) string {
	// Return clean "Version-Release" string (e.g. "0.1.0-1")
	args := []string{"-q", "--qf", "%{VERSION}-%{RELEASE}", pkg}
	if root != "" {
		args = append([]string{"--root", root}, args...)
	}
	stdout, _, _, err := m.runner.Run(ctx, "rpm", args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(stdout)
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
	layouts := []string{time.RFC3339, "Mon 2006-01-02 15:04:05 MST", "Mon 2006-01-02 15:04:05 -0700"}
	for _, layout := range layouts {
		if ts, err := time.Parse(layout, val); err == nil {
			return &ts
		}
	}
	return nil
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

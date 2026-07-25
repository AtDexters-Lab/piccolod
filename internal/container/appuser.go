package container

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// maxLinuxUsername is the maximum length of a Linux username.
	maxLinuxUsername = 32

	userSessionReadyTimeout = 5 * time.Second
	userSessionPollInterval = 100 * time.Millisecond
)

var (
	// ErrUserSessionUnavailable identifies a per-app systemd user session that
	// cannot currently support rootless Podman. Callers use errors.Is to choose
	// observe-only failure or the PID 1 quiescence fallback.
	ErrUserSessionUnavailable = errors.New("per-app user session unavailable")

	userSessionCgroupRoot = "/sys/fs/cgroup"
	userProcessRoot       = "/proc"
	userRuntimeRoot       = "/run/user"
	userLingerRoot        = "/var/lib/systemd/linger"
	openProcessPIDFD      = unix.PidfdOpen
	signalProcessPIDFD    = func(fd int, signal unix.Signal) error {
		return unix.PidfdSendSignal(fd, signal, nil, 0)
	}
	closeProcessPIDFD = unix.Close
)

// provisionMu serializes user provisioning to prevent race conditions
// in subuid/subgid allocation when multiple apps are installed concurrently.
var provisionMu sync.Mutex

// AppUser holds the resolved credentials for a per-app Linux user.
type AppUser struct {
	RuntimeUser // embeds Credential + HomeDir
	Username    string
}

// appUsername returns the deterministic Linux username for an app instance.
// If the resulting name exceeds 32 characters (Linux limit), it is truncated
// and an 8-character hash suffix is appended to avoid collisions.
// With 4 bytes (32 bits) of SHA-256, collision probability stays negligible
// for the expected number of apps (< 1000).
func appUsername(instanceID string) string {
	name := AppUserPrefix + instanceID
	if len(name) <= maxLinuxUsername {
		return name
	}
	hash := sha256.Sum256([]byte(instanceID))
	// 23 (prefix) + 1 (dash) + 8 (hex of 4 bytes) = 32
	return name[:23] + "-" + hex.EncodeToString(hash[:4])
}

// AppUsername returns the deterministic Linux username for an app instance.
func AppUsername(instanceID string) string {
	return appUsername(instanceID)
}

// ProvisionAppUser creates or resolves a per-app Linux user for the given
// app instance. This is fully idempotent — if the user already exists, it
// verifies subuid/subgid allocation and returns. On failure after useradd,
// the user is rolled back (userdel --remove). Serialized via provisionMu to
// prevent concurrent subuid allocation races.
func ProvisionAppUser(instanceID string) (au *AppUser, err error) {
	return ProvisionAppUserContext(context.Background(), instanceID)
}

// ProvisionAppUserContext is ProvisionAppUser with cancellation propagated to
// systemd session activation and D-Bus readiness checks.
func ProvisionAppUserContext(ctx context.Context, instanceID string) (au *AppUser, err error) {
	username := appUsername(instanceID)

	// Fast path: user already exists and has subuid allocation.
	// NOTE: := intentionally shadows named returns (au, err) so the rollback
	// defer (registered later) does not fire on these early-return success paths.
	if au, err := resolveAppUser(username); err == nil {
		if hasSubUIDAllocation(username) {
			if err := ensureUserSession(ctx, instanceID, username, au.Credential.Uid); err != nil {
				return nil, err
			}
			return au, nil
		}
		// User exists but subuid not configured (interrupted provisioning).
		// Fall through to complete provisioning under lock.
		log.Printf("INFO: per-app user %s exists but has no subuid allocation, completing provisioning", username)
	}

	// Serialize provisioning to prevent subuid allocation races.
	provisionMu.Lock()
	defer provisionMu.Unlock()

	// Re-check after acquiring lock (another goroutine may have completed it).
	if au, err := resolveAppUser(username); err == nil && hasSubUIDAllocation(username) {
		if err := ensureUserSession(ctx, instanceID, username, au.Credential.Uid); err != nil {
			return nil, err
		}
		return au, nil
	}

	// Create the user if it doesn't exist yet.
	userExists := false
	createdByThisCall := false
	if _, lookupErr := defaultResolver.LookupUser(username); lookupErr == nil {
		userExists = true
	}

	// Roll back user creation on any failure. Only deletes the user if THIS call
	// created it (not if the user pre-existed or was created by a concurrent call).
	// Using createdByThisCall instead of !userExists prevents destructive rollback
	// when a transient LookupUser failure makes an existing user appear absent.
	defer func() {
		if err != nil && createdByThisCall {
			if out, delErr := defaultExecutor.Run("userdel", "--remove", username); delErr != nil {
				log.Printf("WARN: rollback userdel %s failed: %v: %s", username, delErr, strings.TrimSpace(string(out)))
			}
		}
	}()

	if !userExists {
		out, addErr := defaultExecutor.Run("useradd",
			"--system",
			"--shell", "/usr/sbin/nologin",
			"--create-home",
			username,
		)
		if addErr != nil {
			// Check if user was created by a concurrent call.
			if _, lookupErr := defaultResolver.LookupUser(username); lookupErr != nil {
				return nil, fmt.Errorf("provision user %s: useradd: %w: %s", username, addErr, strings.TrimSpace(string(out)))
			}
			// Concurrent creation — don't claim ownership for rollback.
		} else {
			createdByThisCall = true
		}
	}

	// Allocate subuid/subgid range if not already allocated.
	if !hasSubUIDAllocation(username) {
		start, allocErr := allocateSubUIDRange(username)
		if allocErr != nil {
			return nil, fmt.Errorf("provision user %s: allocate subuid range: %w", username, allocErr)
		}

		rangeSpec := fmt.Sprintf("%d-%d", start, start+SubUIDRangeSize-1)
		out, modErr := defaultExecutor.Run("usermod",
			"--add-subuids", rangeSpec,
			"--add-subgids", rangeSpec,
			username,
		)
		if modErr != nil {
			return nil, fmt.Errorf("provision user %s: usermod add-subuids: %w: %s", username, modErr, strings.TrimSpace(string(out)))
		}
	}

	if lingerErr := enableLinger(ctx, instanceID, username); lingerErr != nil {
		return nil, fmt.Errorf("provision user %s: enable linger: %w", username, lingerErr)
	}

	au, err = resolveAppUser(username)
	if err != nil {
		return nil, fmt.Errorf("provision user %s: resolve user: %w", username, err)
	}

	log.Printf("INFO: provisioned per-app user %s (uid=%d)", username, au.Credential.Uid)
	return au, nil
}

// DestroyAppUser removes the per-app Linux user for the given app instance.
// This kills user processes, disables linger, and removes the user with userdel --remove.
// Returns nil if the user does not exist.
func DestroyAppUser(instanceID string) error {
	return destroyAppUserByName(appUsername(instanceID))
}

// destroyAppUserByName removes a per-app Linux user by username.
// Kills processes, disables linger, runs userdel. Returns nil if user doesn't exist.
// Refuses to proceed if the UID is 0 or unparseable (protects root).
func destroyAppUserByName(username string) error {
	u, err := defaultResolver.LookupUser(username)
	if err != nil {
		return nil // User doesn't exist, nothing to do.
	}

	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		log.Printf("ERROR: destroy user %s: failed to parse UID %q: %v — refusing to proceed", username, u.Uid, err)
		return fmt.Errorf("destroy user %s: unparseable UID %q: %w", username, u.Uid, err)
	}
	if uid == 0 {
		log.Printf("ERROR: destroy user %s: UID is 0 — refusing to destroy to protect root", username)
		return fmt.Errorf("destroy user %s: UID is 0, refusing to proceed", username)
	}

	instanceID := strings.TrimPrefix(username, AppUserPrefix)
	if instanceID == "" || instanceID == username {
		return fmt.Errorf("destroy user %s: invalid per-app username", username)
	}
	// User deletion can release both the outer UID and subordinate ID ranges.
	// Prove the complete dedicated user cgroup inactive and empty first; a
	// best-effort kill of only the outer UID would miss rootless workloads
	// executing under mapped subordinate UIDs.
	if err := QuiesceAppUserSession(context.Background(), instanceID); err != nil {
		return fmt.Errorf("destroy user %s: prove app user session quiescent: %w", username, err)
	}
	if err := disableLinger(username); err != nil {
		return fmt.Errorf("destroy user %s: disable linger: %w", username, err)
	}
	if err := releaseUserRuntime(uint32(uid)); err != nil {
		return fmt.Errorf("destroy user %s: release runtime directory: %w", username, err)
	}

	out, delErr := defaultExecutor.Run("userdel", "--remove", username)
	if delErr != nil {
		// Post-userdel recovery: if userdel fails but user is already gone, treat as success.
		if _, lookupErr := defaultResolver.LookupUser(username); lookupErr != nil {
			return nil
		}
		return fmt.Errorf("destroy user %s: userdel: %w: %s", username, delErr, strings.TrimSpace(string(out)))
	}

	log.Printf("INFO: destroyed per-app user %s", username)
	return nil
}

// ResolveAppUser looks up an existing per-app user without creating it.
// Returns an error if the user does not exist.
func ResolveAppUser(instanceID string) (*AppUser, error) {
	return resolveAppUser(appUsername(instanceID))
}

// resolveAppUser looks up a Linux user by username and returns an AppUser.
func resolveAppUser(username string) (*AppUser, error) {
	ru, err := ResolveRuntimeCredential(username)
	if err != nil {
		return nil, err
	}
	return &AppUser{
		RuntimeUser: *ru,
		Username:    username,
	}, nil
}

// LookupSubUIDRange returns the subuid start and count for the given username.
// Returns an error if the user has no subuid allocation.
func LookupSubUIDRange(username string) (start, count uint32, err error) {
	entries, err := parseSubUIDFile("/etc/subuid")
	if err != nil {
		return 0, 0, fmt.Errorf("parse /etc/subuid: %w", err)
	}
	for _, e := range entries {
		if e.Username == username {
			return e.Start, e.Count, nil
		}
	}
	return 0, 0, fmt.Errorf("no subuid allocation for %s", username)
}

// CleanupOrphanAppUsers removes per-app users that don't correspond to any
// known app instance. knownInstanceIDs is the set of all current app instance IDs.
func CleanupOrphanAppUsers(knownInstanceIDs map[string]bool) {
	passwdUsers, err := listAppUsers()
	if err != nil {
		log.Printf("WARN: orphan cleanup: failed to scan /etc/passwd: %v", err)
		return
	}

	// Build the set of expected usernames from known instance IDs.
	expected := make(map[string]bool, len(knownInstanceIDs))
	for id := range knownInstanceIDs {
		expected[appUsername(id)] = true
	}

	for _, username := range passwdUsers {
		if expected[username] {
			continue
		}
		log.Printf("INFO: cleaning up orphan per-app user %s", username)
		if err := destroyAppUserByName(username); err != nil {
			log.Printf("WARN: orphan cleanup %s: %v", username, err)
		}
	}
}

// listAppUsers scans /etc/passwd and returns all usernames starting with
// the per-app prefix "pa-".
func listAppUsers() ([]string, error) {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var users []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.Index(line, ":"); idx > 0 {
			username := line[:idx]
			if strings.HasPrefix(username, AppUserPrefix) {
				users = append(users, username)
			}
		}
	}
	return users, scanner.Err()
}

// subUIDEntry represents a single entry in /etc/subuid or /etc/subgid.
type subUIDEntry struct {
	Username string
	Start    uint32
	Count    uint32
}

// parseSubUIDFile parses /etc/subuid (or /etc/subgid) format: username:start:count
func parseSubUIDFile(path string) ([]subUIDEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var entries []subUIDEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		start, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			continue
		}
		count, err := strconv.ParseUint(parts[2], 10, 32)
		if err != nil {
			continue
		}
		entries = append(entries, subUIDEntry{
			Username: parts[0],
			Start:    uint32(start),
			Count:    uint32(count),
		})
	}
	return entries, scanner.Err()
}

// allocateSubUIDRange finds the next available non-overlapping subuid/subgid slot.
// Scans both /etc/subuid and /etc/subgid for all existing entries and finds the
// first gap starting from SubUIDBase (200000). Both files are scanned because the
// same range is applied to both, and manual edits could create asymmetry.
func allocateSubUIDRange(username string) (uint32, error) {
	uidEntries, err := parseSubUIDFile("/etc/subuid")
	if err != nil {
		return 0, fmt.Errorf("parse /etc/subuid: %w", err)
	}

	// Check if this user already has an allocation.
	for _, e := range uidEntries {
		if e.Username == username {
			return e.Start, nil
		}
	}

	gidEntries, err := parseSubUIDFile("/etc/subgid")
	if err != nil {
		return 0, fmt.Errorf("parse /etc/subgid: %w", err)
	}

	allEntries := append(uidEntries, gidEntries...)
	return findNextSlot(allEntries, SubUIDBase, SubUIDRangeSize)
}

// findNextSlot finds the first non-overlapping range of the given size starting
// from base, given the existing entries. Entries are sorted by start position
// for correct gap detection.
func findNextSlot(entries []subUIDEntry, base, rangeSize uint32) (uint32, error) {
	type interval struct{ start, end uint32 }
	occupied := make([]interval, 0, len(entries))
	for _, e := range entries {
		occupied = append(occupied, interval{e.Start, e.Start + e.Count - 1})
	}

	// Sort by start position for correct sequential gap-finding.
	sort.Slice(occupied, func(i, j int) bool {
		return occupied[i].start < occupied[j].start
	})

	candidate := base
	for _, o := range occupied {
		candidateEnd := candidate + rangeSize - 1
		if candidate <= o.end && candidateEnd >= o.start {
			// Overlap — advance past this range, aligned to rangeSize.
			candidate = o.end + 1
			if candidate%rangeSize != 0 {
				candidate = ((candidate / rangeSize) + 1) * rangeSize
			}
		}
	}

	if candidate > 4000000000 {
		return 0, fmt.Errorf("exhausted subuid space")
	}
	return candidate, nil
}

// hasSubUIDAllocation checks if a user has a subuid entry in /etc/subuid.
func hasSubUIDAllocation(username string) bool {
	entries, err := parseSubUIDFile("/etc/subuid")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Username == username {
			return true
		}
	}
	return false
}

// EnsureCgroupDelegation creates a systemd drop-in for user@.service that
// delegates cgroup v2 controllers (memory, cpu, pids, io, cpuset) to user
// sessions. Without this, cgroup controller files (memory.max, cpu.max, etc.)
// don't appear in user cgroup trees, and rootless Podman resource limits fail.
// Must be called before ProvisionAppUser so that new user services start with
// delegated controllers. Idempotent — skips if already configured.
func EnsureCgroupDelegation() error {
	dropinDir := "/etc/systemd/system/user@.service.d"
	dropinFile := filepath.Join(dropinDir, "delegate.conf")

	// Fast path: already exists with correct content.
	content := "[Service]\nDelegate=cpu cpuset io memory pids\n"
	if existing, err := os.ReadFile(dropinFile); err == nil && string(existing) == content {
		return nil
	}

	if err := os.MkdirAll(dropinDir, 0o755); err != nil {
		return fmt.Errorf("create delegate drop-in dir: %w", err)
	}
	if err := os.WriteFile(dropinFile, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write delegate drop-in: %w", err)
	}

	// Reload systemd to pick up the new drop-in.
	out, err := defaultExecutor.Run("systemctl", "daemon-reload")
	if err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}

	log.Printf("INFO: created cgroup delegation drop-in at %s", dropinFile)
	return nil
}

// userSessionState is the PID 1 view of a per-app user manager. The D-Bus
// endpoint is probed separately because a socket inode is not liveness proof.
type userSessionState struct {
	ActiveState  string
	SubState     string
	Result       string
	ControlGroup string
}

type userSessionUnavailableError struct {
	InstanceID      string
	Username        string
	UID             uint32
	Unit            string
	PreActionState  userSessionState
	PostActionState userSessionState
	RepairAction    string
	RepairResult    string
	Cause           error
}

func (e *userSessionUnavailableError) Error() string {
	return fmt.Sprintf("%v: instance=%s user=%s uid=%d unit=%s pre_active=%s pre_sub=%s pre_result=%s post_active=%s post_sub=%s post_result=%s repair_action=%s repair_result=%s: %v",
		ErrUserSessionUnavailable, e.InstanceID, e.Username, e.UID, e.Unit,
		e.PreActionState.ActiveState, e.PreActionState.SubState, e.PreActionState.Result,
		e.PostActionState.ActiveState, e.PostActionState.SubState, e.PostActionState.Result,
		e.RepairAction, e.RepairResult, e.Cause)
}

func (e *userSessionUnavailableError) Unwrap() []error {
	return []error{ErrUserSessionUnavailable, e.Cause}
}

type userSessionRepairContext struct {
	Action   string
	PreState userSessionState
	Result   string
}

func normalizeUserSessionRepairContext(state userSessionState, repair userSessionRepairContext) userSessionRepairContext {
	if repair.Action == "" {
		repair.Action = "none"
		repair.Result = "not-attempted"
		repair.PreState = state
	}
	return repair
}

func newUserSessionUnavailable(instanceID, username string, uid uint32, state userSessionState, cause error, repair userSessionRepairContext) error {
	if cause == nil {
		cause = errors.New("session not ready")
	}
	repair = normalizeUserSessionRepairContext(state, repair)
	return &userSessionUnavailableError{
		InstanceID:      instanceID,
		Username:        username,
		UID:             uid,
		Unit:            fmt.Sprintf("user@%d.service", uid),
		PreActionState:  repair.PreState,
		PostActionState: state,
		RepairAction:    repair.Action,
		RepairResult:    repair.Result,
		Cause:           cause,
	}
}

func querySystemUnitState(ctx context.Context, unit string) (userSessionState, error) {
	out, err := defaultExecutor.RunContext(ctx, "systemctl", "show", unit,
		"--property=ActiveState", "--property=SubState", "--property=Result",
		"--property=ControlGroup", "--no-pager")
	if err != nil {
		return userSessionState{}, fmt.Errorf("systemctl show %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	state := userSessionState{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "ActiveState":
			state.ActiveState = strings.TrimSpace(value)
		case "SubState":
			state.SubState = strings.TrimSpace(value)
		case "Result":
			state.Result = strings.TrimSpace(value)
		case "ControlGroup":
			state.ControlGroup = strings.TrimSpace(value)
		}
	}
	if state.ActiveState == "" {
		return userSessionState{}, fmt.Errorf("systemctl show %s returned no ActiveState", unit)
	}
	return state, nil
}

func queryUserSession(ctx context.Context, uid uint32) (userSessionState, error) {
	return querySystemUnitState(ctx, fmt.Sprintf("user@%d.service", uid))
}

func probeUserBus(ctx context.Context, username string, uid uint32) error {
	runtimeDir := fmt.Sprintf("/run/user/%d", uid)
	busAddress := "unix:path=" + filepath.Join(runtimeDir, "bus")
	out, err := defaultExecutor.RunContext(ctx, "/usr/sbin/runuser",
		"--user", username, "--",
		"/usr/bin/env",
		"XDG_RUNTIME_DIR="+runtimeDir,
		"DBUS_SESSION_BUS_ADDRESS="+busAddress,
		"/usr/bin/busctl", "--user", "--no-pager", "--quiet", "list")
	if err != nil {
		return fmt.Errorf("probe user bus: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runUserSessionAction(ctx context.Context, action string, uid uint32) error {
	unit := fmt.Sprintf("user@%d.service", uid)
	out, err := defaultExecutor.RunContext(ctx, "systemctl", action, unit)
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", action, unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func waitForUserSession(ctx context.Context, instanceID, username string, uid uint32, allowRepair bool) error {
	var (
		lastState       userSessionState
		lastErr         error
		repairAttempted bool
		repair          userSessionRepairContext
	)
	for {
		state, err := queryUserSession(ctx, uid)
		if err != nil {
			return newUserSessionUnavailable(instanceID, username, uid, lastState, err, repair)
		}
		lastState = state
		switch state.ActiveState {
		case "active":
			if err := probeUserBus(ctx, username, uid); err == nil {
				repair = normalizeUserSessionRepairContext(state, repair)
				log.Printf("INFO: user session ready instance=%s user=%s uid=%d unit=user@%d.service pre_active=%s pre_sub=%s pre_result=%s post_active=%s post_sub=%s post_result=%s repair_action=%s repair_result=%s",
					instanceID, username, uid, uid,
					repair.PreState.ActiveState, repair.PreState.SubState, repair.PreState.Result,
					state.ActiveState, state.SubState, state.Result, repair.Action, repair.Result)
				return nil
			} else {
				lastErr = err
			}
			if !allowRepair {
				return newUserSessionUnavailable(instanceID, username, uid, state, lastErr, repair)
			}
			if allowRepair && !repairAttempted {
				log.Printf("WARN: repairing active but unusable user session user=%s uid=%d unit=user@%d.service active=%s sub=%s result=%s: %v",
					username, uid, uid, state.ActiveState, state.SubState, state.Result, lastErr)
				repair = userSessionRepairContext{Action: "restart", PreState: state}
				if err := runUserSessionAction(ctx, repair.Action, uid); err != nil {
					repair.Result = "failed"
					postState, postErr := queryUserSession(ctx, uid)
					if postErr != nil {
						postState = state
					}
					return newUserSessionUnavailable(instanceID, username, uid, postState, errors.Join(err, postErr), repair)
				}
				repair.Result = "success"
				repairAttempted = true
				continue
			}
		case "inactive", "failed":
			lastErr = fmt.Errorf("unit is %s/%s result=%s", state.ActiveState, state.SubState, state.Result)
			if !allowRepair {
				return newUserSessionUnavailable(instanceID, username, uid, state, lastErr, repair)
			}
			if !repairAttempted {
				log.Printf("INFO: starting unavailable user session user=%s uid=%d unit=user@%d.service active=%s sub=%s result=%s",
					username, uid, uid, state.ActiveState, state.SubState, state.Result)
				repair = userSessionRepairContext{Action: "start", PreState: state}
				if err := runUserSessionAction(ctx, repair.Action, uid); err != nil {
					repair.Result = "failed"
					postState, postErr := queryUserSession(ctx, uid)
					if postErr != nil {
						postState = state
					}
					return newUserSessionUnavailable(instanceID, username, uid, postState, errors.Join(err, postErr), repair)
				}
				repair.Result = "success"
				repairAttempted = true
				continue
			}
		case "activating", "deactivating", "reloading":
			lastErr = fmt.Errorf("unit remains transitional: %s/%s", state.ActiveState, state.SubState)
		default:
			return newUserSessionUnavailable(instanceID, username, uid, state,
				fmt.Errorf("unsupported unit state %q/%q", state.ActiveState, state.SubState), repair)
		}

		timer := time.NewTimer(userSessionPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return newUserSessionUnavailable(instanceID, username, uid, lastState, errors.Join(lastErr, ctx.Err()), repair)
		case <-timer.C:
		}
	}
}

func withUserSessionDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, userSessionReadyTimeout)
}

// enableLinger enables systemd linger and proves that the resulting session is
// usable by rootless Podman. Provisioning fails if readiness cannot be proven.
func enableLinger(ctx context.Context, instanceID, username string) error {
	deadlineCtx, cancel := withUserSessionDeadline(ctx)
	defer cancel()

	if out, err := defaultExecutor.RunContext(deadlineCtx, "loginctl", "enable-linger", username); err != nil {
		return fmt.Errorf("loginctl enable-linger %s: %w: %s", username, err, strings.TrimSpace(string(out)))
	}
	u, err := defaultResolver.LookupUser(username)
	if err != nil {
		return fmt.Errorf("resolve user %s after enabling linger: %w", username, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil || uid == 0 {
		return fmt.Errorf("resolve user %s after enabling linger: invalid UID %q", username, u.Uid)
	}
	return waitForUserSession(deadlineCtx, instanceID, username, uint32(uid), true)
}

// ensureUserSession proves or repairs the dedicated per-app user session.
func ensureUserSession(ctx context.Context, instanceID, username string, uid uint32) error {
	deadlineCtx, cancel := withUserSessionDeadline(ctx)
	defer cancel()
	return waitForUserSession(deadlineCtx, instanceID, username, uid, true)
}

// ResolveReadyAppUser resolves an existing per-app user and observes its
// session without starting or restarting it.
func ResolveReadyAppUser(ctx context.Context, instanceID string) (*AppUser, error) {
	appUser, err := ResolveAppUser(instanceID)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve app user %s: %w", ErrUserSessionUnavailable, appUsername(instanceID), err)
	}
	deadlineCtx, cancel := withUserSessionDeadline(ctx)
	defer cancel()
	if err := waitForUserSession(deadlineCtx, instanceID, appUser.Username, appUser.Credential.Uid, false); err != nil {
		return nil, err
	}
	return appUser, nil
}

func userSessionCgroupEmpty(state userSessionState) (bool, error) {
	if state.ControlGroup == "" {
		return userSessionUnitInactive(state), nil
	}
	clean := filepath.Clean("/" + strings.TrimPrefix(state.ControlGroup, "/"))
	if !strings.HasPrefix(clean, "/user.slice/") {
		return false, fmt.Errorf("refusing unexpected user-session cgroup path %q", state.ControlGroup)
	}
	eventsPath := filepath.Join(userSessionCgroupRoot, strings.TrimPrefix(clean, "/"), "cgroup.events")
	data, err := os.ReadFile(eventsPath)
	if errors.Is(err, os.ErrNotExist) {
		return userSessionUnitInactive(state), nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", eventsPath, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" {
			return fields[1] == "0", nil
		}
	}
	return false, fmt.Errorf("%s has no populated field", eventsPath)
}

func userSessionUnitInactive(state userSessionState) bool {
	return state.ActiveState == "inactive" || state.ActiveState == "failed"
}

// QuiesceAppUserSession asks PID 1 to stop the dedicated app user manager and
// returns only after the unit is non-active, its cgroup is proven empty, and
// no process remains under the app's numeric UID. The UID proof also covers
// rootless helpers launched by a privileged parent before systemd moves their
// container workloads into the dedicated user hierarchy.
func QuiesceAppUserSession(ctx context.Context, instanceID string) error {
	appUser, err := ResolveAppUser(instanceID)
	if err != nil {
		// A missing passwd entry is not process-absence proof: processes can
		// outlive deletion of their numeric UID. Without the UID we cannot query
		// the dedicated user unit/cgroup, so teardown must fail closed.
		return fmt.Errorf("resolve app user before quiesce (cannot prove cgroup empty): %w", err)
	}
	uid := appUser.Credential.Uid
	if uid == 0 {
		return errors.New("app user UID is 0, refusing to quiesce root session")
	}
	deadlineCtx, cancel := withUserSessionDeadline(ctx)
	defer cancel()

	state, err := queryUserSession(deadlineCtx, uid)
	if err != nil {
		return newUserSessionUnavailable(instanceID, appUser.Username, uid, state, err, userSessionRepairContext{})
	}
	if empty, emptyErr := userSessionCgroupEmpty(state); emptyErr == nil && empty && userSessionUnitInactive(state) {
		if err := terminateUserProcesses(deadlineCtx, uid); err != nil {
			return newUserSessionUnavailable(instanceID, appUser.Username, uid, state, err, userSessionRepairContext{})
		}
		return nil
	}
	if err := runUserSessionAction(deadlineCtx, "stop", uid); err != nil {
		return newUserSessionUnavailable(instanceID, appUser.Username, uid, state, err, userSessionRepairContext{Action: "stop", PreState: state, Result: "failed"})
	}
	repair := userSessionRepairContext{Action: "stop", PreState: state, Result: "success"}

	for {
		state, err = queryUserSession(deadlineCtx, uid)
		if err != nil {
			return newUserSessionUnavailable(instanceID, appUser.Username, uid, state, err, repair)
		}
		empty, emptyErr := userSessionCgroupEmpty(state)
		if emptyErr == nil && empty && userSessionUnitInactive(state) {
			if processErr := terminateUserProcesses(deadlineCtx, uid); processErr != nil {
				return newUserSessionUnavailable(instanceID, appUser.Username, uid, state, processErr, repair)
			}
			log.Printf("INFO: user session quiesced instance=%s user=%s uid=%d unit=user@%d.service pre_active=%s pre_sub=%s pre_result=%s post_active=%s post_sub=%s post_result=%s repair_action=%s repair_result=%s",
				instanceID, appUser.Username, uid, uid,
				repair.PreState.ActiveState, repair.PreState.SubState, repair.PreState.Result,
				state.ActiveState, state.SubState, state.Result, repair.Action, repair.Result)
			return nil
		}
		if emptyErr != nil {
			err = emptyErr
		}
		timer := time.NewTimer(userSessionPollInterval)
		select {
		case <-deadlineCtx.Done():
			timer.Stop()
			repair.Result = "timeout"
			return newUserSessionUnavailable(instanceID, appUser.Username, uid, state, errors.Join(err, deadlineCtx.Err()), repair)
		case <-timer.C:
		}
	}
}

func processStatusHasUID(data []byte, uid uint32) (bool, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "Uid:" {
			continue
		}
		if len(fields) != 5 {
			return false, fmt.Errorf("malformed Uid field %q", line)
		}
		for _, raw := range fields[1:] {
			value, err := strconv.ParseUint(raw, 10, 32)
			if err != nil {
				return false, fmt.Errorf("parse Uid field %q: %w", line, err)
			}
			if uint32(value) == uid {
				return true, nil
			}
		}
		return false, nil
	}
	return false, errors.New("process status has no Uid field")
}

func processHasUID(pid int, uid uint32) (bool, error) {
	data, err := os.ReadFile(filepath.Join(userProcessRoot, strconv.Itoa(pid), "status"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return processStatusHasUID(data, uid)
}

func userProcessIDs(uid uint32) ([]int, error) {
	entries, err := os.ReadDir(userProcessRoot)
	if err != nil {
		return nil, fmt.Errorf("scan process root %s: %w", userProcessRoot, err)
	}
	pids := make([]int, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		owned, err := processHasUID(pid, uid)
		if err != nil {
			return nil, fmt.Errorf("inspect process %d: %w", pid, err)
		}
		if owned {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids, nil
}

// terminateUserProcesses kills every process whose real, effective, saved, or
// filesystem UID matches the per-app UID, then waits until /proc proves none
// remain. A pidfd binds the signal to the inspected process; ownership is
// re-read after opening it so PID exit/reuse cannot redirect SIGKILL.
func terminateUserProcesses(ctx context.Context, uid uint32) error {
	if uid == 0 {
		return errors.New("UID is 0, refusing to terminate processes")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		pids, err := userProcessIDs(uid)
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}

		for _, pid := range pids {
			fd, err := openProcessPIDFD(pid, 0)
			if errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				return fmt.Errorf("open pidfd for UID %d process %d: %w", uid, pid, err)
			}

			owned, ownershipErr := processHasUID(pid, uid)
			if ownershipErr != nil {
				_ = closeProcessPIDFD(fd)
				return fmt.Errorf("recheck UID %d process %d: %w", uid, pid, ownershipErr)
			}
			if !owned {
				_ = closeProcessPIDFD(fd)
				continue
			}

			signalErr := signalProcessPIDFD(fd, unix.SIGKILL)
			_ = closeProcessPIDFD(fd)
			if signalErr != nil && !errors.Is(signalErr, unix.ESRCH) {
				return fmt.Errorf("kill UID %d process %d: %w", uid, pid, signalErr)
			}
			log.Printf("INFO: terminated escaped per-app process uid=%d pid=%d", uid, pid)
		}

		timer := time.NewTimer(userSessionPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			remaining, _ := userProcessIDs(uid)
			return fmt.Errorf("wait for UID %d process absence (remaining=%v): %w", uid, remaining, ctx.Err())
		case <-timer.C:
		}
	}
}

// killUserProcesses terminates all processes owned by the given UID.
// Required before userdel — lingering user sessions and helpers outside the
// user cgroup can otherwise prevent deletion (userdel exit code 8).
// releaseUserRuntime asks systemd's existing per-UID runtime-dir owner to stop,
// then removes any directory left by Piccolo's own EnsureXDGRuntimeDir fallback.
// The caller has already proven the nonzero UID has no processes, so no live
// session can race the cleanup or retain files across UID reuse.
func releaseUserRuntime(uid uint32) error {
	if uid == 0 {
		return errors.New("UID is 0, refusing to release runtime directory")
	}
	unit := fmt.Sprintf("user-runtime-dir@%d.service", uid)
	ctx, cancel := context.WithTimeout(context.Background(), userSessionReadyTimeout)
	defer cancel()
	out, stopErr := defaultExecutor.RunContext(ctx, "systemctl", "stop", unit)
	state, stateErr := querySystemUnitState(ctx, unit)
	if stateErr != nil {
		return fmt.Errorf("prove runtime directory owner %s inactive after stop: %w", unit,
			errors.Join(stopErr, stateErr))
	}
	if !userSessionUnitInactive(state) {
		if stopErr == nil {
			stopErr = errors.New("stop returned success without reaching an inactive state")
		}
		return fmt.Errorf("runtime directory owner %s remains %s/%s after stop: %w: %s",
			unit, state.ActiveState, state.SubState, stopErr, strings.TrimSpace(string(out)))
	}
	if stopErr != nil {
		log.Printf("DEBUG: systemctl stop %s failed but PID 1 proves it inactive: %v: %s",
			unit, stopErr, strings.TrimSpace(string(out)))
	}
	runtimeDir := filepath.Join(userRuntimeRoot, strconv.FormatUint(uint64(uid), 10))
	if err := os.RemoveAll(runtimeDir); err != nil {
		return fmt.Errorf("remove %s: %w", runtimeDir, err)
	}
	postState, err := querySystemUnitState(ctx, unit)
	if err != nil {
		return fmt.Errorf("prove runtime directory owner %s stayed inactive: %w", unit, err)
	}
	if !userSessionUnitInactive(postState) {
		return fmt.Errorf("runtime directory owner %s reactivated as %s/%s during cleanup",
			unit, postState.ActiveState, postState.SubState)
	}
	if _, err := os.Stat(runtimeDir); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("runtime directory %s still exists", runtimeDir)
		}
		return fmt.Errorf("verify runtime directory %s absent: %w", runtimeDir, err)
	}
	return nil
}

// disableLinger disables systemd linger and proves that PID 1 no longer has a
// persistent-login marker which could recreate the user runtime during UID
// cleanup.
func disableLinger(username string) error {
	if out, err := defaultExecutor.Run("loginctl", "disable-linger", username); err != nil {
		return fmt.Errorf("loginctl disable-linger %s: %w: %s", username, err, strings.TrimSpace(string(out)))
	}
	lingerPath := filepath.Join(userLingerRoot, username)
	if _, err := os.Stat(lingerPath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("linger marker %s still exists", lingerPath)
		}
		return fmt.Errorf("verify linger marker %s absent: %w", lingerPath, err)
	}
	return nil
}

package container

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// appUserPrefix is the username prefix for per-app Linux users.
	appUserPrefix = "pa-"

	// appsGroupName is the shared POSIX group for imagestore access.
	appsGroupName = "piccolo-apps"

	// subUIDBase is the starting offset for per-app subuid/subgid ranges.
	// Above piccolo-runtime's 100000:65536.
	subUIDBase = 200000

	// subUIDRangeSize is the number of subordinate UIDs/GIDs per app user.
	subUIDRangeSize = 65536

	// maxLinuxUsername is the maximum length of a Linux username.
	maxLinuxUsername = 32
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
	name := appUserPrefix + instanceID
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
func ProvisionAppUser(instanceID string) (*AppUser, error) {
	username := appUsername(instanceID)

	// Fast path: user already exists and has subuid allocation.
	if au, err := resolveAppUser(username); err == nil {
		if hasSubUIDAllocation(username) {
			// Ensure the systemd user session is active. After daemon restarts
			// the user exists but linger/dbus may be dead (tmpfs lost on reboot).
			ensureUserSession(username, au.Credential.Uid)
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
		ensureUserSession(username, au.Credential.Uid)
		return au, nil
	}

	// Ensure the shared group exists.
	if err := ensureGroup(appsGroupName); err != nil {
		return nil, fmt.Errorf("provision app user: ensure group: %w", err)
	}

	// Create the user if it doesn't exist yet.
	userExists := false
	if _, err := user.Lookup(username); err == nil {
		userExists = true
	}

	if !userExists {
		cmd := exec.Command("useradd",
			"--system",
			"--shell", "/usr/sbin/nologin",
			"--create-home",
			"--groups", appsGroupName,
			username,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			// Check if user was created by a concurrent call.
			if _, lookupErr := user.Lookup(username); lookupErr != nil {
				return nil, fmt.Errorf("useradd %s: %w: %s", username, err, strings.TrimSpace(string(out)))
			}
		}
	}

	// Allocate subuid/subgid range if not already allocated.
	if !hasSubUIDAllocation(username) {
		start, err := allocateSubUIDRange(username)
		if err != nil {
			if !userExists {
				if out, delErr := exec.Command("userdel", "--remove", username).CombinedOutput(); delErr != nil {
					log.Printf("WARN: rollback userdel %s failed: %v: %s", username, delErr, strings.TrimSpace(string(out)))
				}
			}
			return nil, fmt.Errorf("allocate subuid range for %s: %w", username, err)
		}

		rangeSpec := fmt.Sprintf("%d-%d", start, start+subUIDRangeSize-1)
		cmd := exec.Command("usermod",
			"--add-subuids", rangeSpec,
			"--add-subgids", rangeSpec,
			username,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			if !userExists {
				if out, delErr := exec.Command("userdel", "--remove", username).CombinedOutput(); delErr != nil {
					log.Printf("WARN: rollback userdel %s failed: %v: %s", username, delErr, strings.TrimSpace(string(out)))
				}
			}
			return nil, fmt.Errorf("usermod add-subuids for %s: %w: %s", username, err, strings.TrimSpace(string(out)))
		}
	}

	// Enable linger for cgroup delegation (idempotent).
	if err := enableLinger(username); err != nil {
		if !userExists {
			if out, delErr := exec.Command("userdel", "--remove", username).CombinedOutput(); delErr != nil {
					log.Printf("WARN: rollback userdel %s failed: %v: %s", username, delErr, strings.TrimSpace(string(out)))
				}
		}
		return nil, fmt.Errorf("enable linger for %s: %w", username, err)
	}

	au, err := resolveAppUser(username)
	if err != nil {
		if !userExists {
			if out, delErr := exec.Command("userdel", "--remove", username).CombinedOutput(); delErr != nil {
					log.Printf("WARN: rollback userdel %s failed: %v: %s", username, delErr, strings.TrimSpace(string(out)))
				}
		}
		return nil, fmt.Errorf("resolve newly created user %s: %w", username, err)
	}

	log.Printf("INFO: provisioned per-app user %s (uid=%d)", username, au.Credential.Uid)
	return au, nil
}

// DestroyAppUser removes the per-app Linux user for the given app instance.
// This kills user processes, disables linger, and removes the user with userdel --remove.
// Returns nil if the user does not exist.
func DestroyAppUser(instanceID string) error {
	username := appUsername(instanceID)

	u, err := user.Lookup(username)
	if err != nil {
		return nil // User doesn't exist, nothing to do.
	}

	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	killUserProcesses(username, uint32(uid))
	disableLinger(username)

	cmd := exec.Command("userdel", "--remove", username)
	if out, err := cmd.CombinedOutput(); err != nil {
		// If user was already removed, treat as success.
		if _, lookupErr := user.Lookup(username); lookupErr != nil {
			return nil
		}
		return fmt.Errorf("userdel %s: %w: %s", username, err, strings.TrimSpace(string(out)))
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
		u, err := user.Lookup(username)
		if err == nil {
			uid, _ := strconv.ParseUint(u.Uid, 10, 32)
			killUserProcesses(username, uint32(uid))
		}
		disableLinger(username)
		cmd := exec.Command("userdel", "--remove", username)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("WARN: failed to remove orphan user %s: %v: %s", username, err, strings.TrimSpace(string(out)))
		}
	}
}

// EnsureAppsGroup creates the piccolo-apps shared group if it doesn't exist.
func EnsureAppsGroup() error {
	return ensureGroup(appsGroupName)
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
			if strings.HasPrefix(username, appUserPrefix) {
				users = append(users, username)
			}
		}
	}
	return users, scanner.Err()
}

// ensureGroup creates a POSIX group if it doesn't already exist.
// Uses groupadd -f which is idempotent.
func ensureGroup(name string) error {
	cmd := exec.Command("groupadd", "-f", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("groupadd -f %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
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
// first gap starting from subUIDBase (200000). Both files are scanned because the
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
	return findNextSlot(allEntries, subUIDBase, subUIDRangeSize)
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
	cmd := exec.Command("systemctl", "daemon-reload")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}

	log.Printf("INFO: created cgroup delegation drop-in at %s", dropinFile)
	return nil
}

// enableLinger enables systemd linger for the given user and ensures the
// user's systemd session is fully started. The session provides dbus and
// cgroup delegation that rootless Podman requires.
func enableLinger(username string) error {
	cmd := exec.Command("loginctl", "enable-linger", username)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("loginctl enable-linger %s: %w: %s", username, err, strings.TrimSpace(string(out)))
	}

	// Resolve UID for the user service name.
	u, err := user.Lookup(username)
	if err != nil {
		return nil // Linger enabled, just can't verify session.
	}

	// Explicitly start the user service. loginctl enable-linger is async
	// and may not start the service immediately, especially for newly
	// created users. systemctl start is synchronous and blocks until the
	// service is active.
	svcName := fmt.Sprintf("user@%s.service", u.Uid)
	startCmd := exec.Command("systemctl", "start", svcName)
	if out, err := startCmd.CombinedOutput(); err != nil {
		log.Printf("WARN: systemctl start %s failed: %v: %s", svcName, err, strings.TrimSpace(string(out)))
	}

	// Wait for the dbus socket — confirms the user session is fully ready.
	dbusSocket := fmt.Sprintf("/run/user/%s/bus", u.Uid)
	for i := 0; i < 50; i++ { // 50 × 100ms = 5s max
		if _, err := os.Stat(dbusSocket); err == nil {
			log.Printf("INFO: user session ready for %s (dbus socket at %s)", username, dbusSocket)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("WARN: dbus socket %s not ready after 5s for user %s — rootless Podman cgroup management may fail", dbusSocket, username)
	return nil
}

// ensureUserSession verifies the systemd user session is active for a per-app
// user and re-enables linger if the dbus socket is missing. After daemon restarts,
// the user exists and has subuid but the session may be dead (tmpfs lost on reboot).
func ensureUserSession(username string, uid uint32) {
	dbusSocket := fmt.Sprintf("/run/user/%d/bus", uid)
	if _, err := os.Stat(dbusSocket); err == nil {
		return // Session is active.
	}
	log.Printf("INFO: re-enabling linger for %s (dbus socket missing)", username)
	if err := enableLinger(username); err != nil {
		log.Printf("WARN: failed to re-enable linger for %s: %v", username, err)
	}
}

// killUserProcesses terminates all processes owned by the given UID.
// Required before userdel — lingering user sessions keep processes alive
// that prevent user deletion (userdel exit code 8).
func killUserProcesses(username string, uid uint32) {
	// Stop the systemd user service first (graceful).
	svcName := fmt.Sprintf("user@%d.service", uid)
	if out, err := exec.Command("systemctl", "stop", svcName).CombinedOutput(); err != nil {
		log.Printf("DEBUG: systemctl stop %s: %v: %s", svcName, err, strings.TrimSpace(string(out)))
	}
	// Kill any remaining processes (belt and suspenders).
	if out, err := exec.Command("loginctl", "kill-user", username).CombinedOutput(); err != nil {
		log.Printf("DEBUG: loginctl kill-user %s: %v: %s", username, err, strings.TrimSpace(string(out)))
	}
	// Brief pause for process cleanup.
	time.Sleep(500 * time.Millisecond)
}

// disableLinger disables systemd linger for the given user. Best-effort.
func disableLinger(username string) {
	cmd := exec.Command("loginctl", "disable-linger", username)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("WARN: loginctl disable-linger %s: %v: %s", username, err, strings.TrimSpace(string(out)))
	}
}

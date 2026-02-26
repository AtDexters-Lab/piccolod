package persistence

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"strings"

	"piccolod/internal/crypt"
	"piccolod/internal/events"
	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"
)

type commandRunner interface {
	Run(ctx context.Context, name string, args []string, stdin []byte) error
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, stdin []byte) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func configureForegroundAttrs(cmd *exec.Cmd) {
	if runtime.GOOS != "linux" {
		return
	}
	attr := &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGTERM,
	}
	cmd.SysProcAttr = attr
}

type mountProcess interface {
	Wait() <-chan error
	Signal(os.Signal) error
	Kill() error
	Pid() int
}

type mountLauncher interface {
	Launch(ctx context.Context, path string, args []string, stdin []byte) (mountProcess, error)
}

type mountWaiter func(mountPoint string, timeout time.Duration) error

type execMountLauncher struct{}

func (execMountLauncher) Launch(ctx context.Context, path string, args []string, stdin []byte) (mountProcess, error) {
	// Use Background context so the process is not killed when the parent context (request/app) is cancelled.
	cmd := exec.CommandContext(context.Background(), path, args...)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Create a new session group to detach from the parent's signals.
	// Do NOT use configureForegroundAttrs here as Pdeathsig would kill the mount on parent exit.
	if runtime.GOOS == "linux" {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid: true,
		}
	}

	if err := cmd.Start(); err != nil {
		stdinPipe.Close()
		return nil, err
	}
	if stdin != nil {
		if _, err := io.Copy(stdinPipe, bytes.NewReader(stdin)); err != nil {
			stdinPipe.Close()
			_ = cmd.Process.Kill()
			return nil, err
		}
	}
	stdinPipe.Close()
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	return &execMountProcess{cmd: cmd, done: done}, nil
}

type execMountProcess struct {
	cmd  *exec.Cmd
	done chan error
}

func (p *execMountProcess) Wait() <-chan error { return p.done }

func (p *execMountProcess) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return errors.New("process not started")
	}
	return p.cmd.Process.Signal(sig)
}

func (p *execMountProcess) Kill() error {
	if p.cmd.Process == nil {
		return errors.New("process not started")
	}
	return p.cmd.Process.Kill()
}

func (p *execMountProcess) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// fileVolumeManager orchestrates gocryptfs-backed volumes using a two-root
// layout: coreRoot for control-plane volumes, dataRoot for application volumes.
// Mountpoints always reside under coreRoot/mounts/ regardless of class.
type fileVolumeManager struct {
	coreRoot       string
	dataRoot       string
	crypto         *crypt.Manager
	runner         commandRunner
	gocryptfsPath  string
	fusermountPath string
	volumes        map[string]*volumeEntry
	launcher       mountLauncher
	waitMount      mountWaiter
	bus            *events.Bus
	roleChecker    func(string, VolumeRole) bool
	bypassMount    bool
	mu             sync.RWMutex
}

type volumeEntry struct {
	handle        VolumeHandle
	cipherDir     string
	stateDir      string
	class         VolumeClass // set once at creation, never mutated
	metadata      volumeMetadata
	metadataReady bool
	role          VolumeRole
	process       mountProcess
	metaMu        sync.Mutex
	mountMu       sync.Mutex
}

type volumeMetadata struct {
	Version    int    `json:"version"`
	WrappedKey string `json:"wrapped_key"`
	Nonce      string `json:"nonce"`
}

type volumeState struct {
	Class       string    `json:"class,omitempty"`
	Desired     string    `json:"desired_state"`
	Observed    string    `json:"observed_state"`
	Role        string    `json:"role"`
	LastError   string    `json:"last_error,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	Generation  int       `json:"generation"`
	NeedsRepair bool      `json:"needs_repair"`
	Description string    `json:"description,omitempty"`
}

const (
	volumeMetadataName   = "piccolo.volume.json"
	metadataVersion      = 1
	volumeStateMounted   = "mounted"
	volumeStateUnmounted = "unmounted"
	volumeStatePending   = "pending"
	volumeStateError     = "error"
)

func newFileVolumeManager(coreRoot, dataRoot string, crypto *crypt.Manager, bus *events.Bus) *fileVolumeManager {
	if coreRoot == "" {
		coreRoot = paths.CoreRoot()
	}
	if dataRoot == "" {
		dataRoot = paths.CoreRoot()
	}
	bypass := os.Getenv("PICCOLO_ALLOW_UNMOUNTED_TESTS") == "1"
	waiter := waitForMountReady
	if bypass {
		waiter = func(string, time.Duration) error { return nil }
	}
	return &fileVolumeManager{
		coreRoot:       coreRoot,
		dataRoot:       dataRoot,
		crypto:         crypto,
		runner:         execRunner{},
		gocryptfsPath:  defaultGocryptfsBinary(),
		fusermountPath: defaultFusermountBinary(),
		volumes:        make(map[string]*volumeEntry),
		launcher:       execMountLauncher{},
		waitMount:      waiter,
		bus:            bus,
		roleChecker:    func(string, VolumeRole) bool { return true },
		bypassMount:    bypass,
	}
}

// Helper for tests.
func newFileVolumeManagerWithDeps(coreRoot, dataRoot string, crypto *crypt.Manager, runner commandRunner, gocryptfsPath, fusermountPath string, launcher mountLauncher, waiter mountWaiter) *fileVolumeManager {
	mgr := newFileVolumeManager(coreRoot, dataRoot, crypto, nil)
	if runner != nil {
		mgr.runner = runner
	}
	if gocryptfsPath != "" {
		mgr.gocryptfsPath = gocryptfsPath
	}
	if fusermountPath != "" {
		mgr.fusermountPath = fusermountPath
	}
	if launcher != nil {
		mgr.launcher = launcher
	}
	if waiter != nil {
		mgr.waitMount = waiter
	}
	return mgr
}

func (f *fileVolumeManager) setRoleChecker(fn func(string, VolumeRole) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fn == nil {
		f.roleChecker = func(string, VolumeRole) bool { return true }
		return
	}
	f.roleChecker = fn
}

func defaultGocryptfsBinary() string {
	if v := os.Getenv("PICCOLO_GOCRYPTFS_PATH"); v != "" {
		return v
	}
	return "gocryptfs"
}

func defaultFusermountBinary() string {
	if v := os.Getenv("PICCOLO_FUSERMOUNT_PATH"); v != "" {
		return v
	}
	if _, err := exec.LookPath("fusermount3"); err == nil {
		return "fusermount3"
	}
	if _, err := exec.LookPath("fusermount"); err == nil {
		return "fusermount"
	}
	return "fusermount3"
}

// inferVolumeClass returns the expected VolumeClass based on the volume ID convention.
func inferVolumeClass(id string) VolumeClass {
	if id == "control-plane" {
		return VolumeClassControl
	}
	if strings.HasPrefix(id, "app-") {
		return VolumeClassApplication
	}
	return VolumeClassControl // default: callers must follow naming conventions
}

// rootForClass returns the storage root for a given volume class.
func (f *fileVolumeManager) rootForClass(class VolumeClass) string {
	if class == VolumeClassApplication {
		return f.dataRoot
	}
	return f.coreRoot
}

// rootForVolume determines the storage root for a volume by probing both roots
// for an existing state directory. If neither exists, falls back to ID inference.
func (f *fileVolumeManager) rootForVolume(id string) string {
	// When both roots are the same directory (e.g., tests), skip probing.
	if f.coreRoot == f.dataRoot {
		return f.coreRoot
	}

	coreState := filepath.Join(f.coreRoot, "volumes", id, "state.json")
	dataState := filepath.Join(f.dataRoot, "volumes", id, "state.json")
	coreExists := fileExists(coreState)
	dataExists := fileExists(dataState)

	switch {
	case coreExists && !dataExists:
		return f.coreRoot
	case dataExists && !coreExists:
		return f.dataRoot
	case coreExists && dataExists:
		// Should not happen; prefer the root matching ID convention.
		log.Printf("WARN: volume %s: state.json found under both roots, using ID inference", id)
		return f.rootForClass(inferVolumeClass(id))
	default:
		// New volume — use ID inference.
		return f.rootForClass(inferVolumeClass(id))
	}
}

// volumePaths returns the ciphertext dir, state dir, and mount dir for a volume.
// Ciphertext and state use rootForVolume (class-based); mount is always coreRoot.
func (f *fileVolumeManager) volumePaths(id string) (cipherDir, stateDir, mountDir string) {
	root := f.rootForVolume(id)
	cipherDir = filepath.Join(root, "ciphertext", id)
	stateDir = filepath.Join(root, "volumes", id)
	mountDir = filepath.Join(f.coreRoot, "mounts", id)
	return
}

// volumePathsForClass returns paths using an explicit class (for first creation
// when state.json does not yet exist).
func (f *fileVolumeManager) volumePathsForClass(id string, class VolumeClass) (cipherDir, stateDir, mountDir string) {
	root := f.rootForClass(class)
	cipherDir = filepath.Join(root, "ciphertext", id)
	stateDir = filepath.Join(root, "volumes", id)
	mountDir = filepath.Join(f.coreRoot, "mounts", id)
	return
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (f *fileVolumeManager) getOrCreateEntry(id string) *volumeEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	if entry, ok := f.volumes[id]; ok {
		return entry
	}
	cipherDir, stateDir, mountDir := f.volumePaths(id)
	entry := &volumeEntry{
		handle: VolumeHandle{
			ID:       id,
			MountDir: mountDir,
		},
		cipherDir: cipherDir,
		stateDir:  stateDir,
		class:     inferVolumeClass(id),
	}
	f.volumes[id] = entry
	return entry
}

func shouldGuardMountDir(volumeID string) bool {
	// Guard all mount points under PICCOLO_CORE_ROOT/mounts to prevent accidental
	// plaintext writes to an unmounted directory where an encrypted volume is expected.
	return volumeID != ""
}

// reprotectMountDir re-applies the immutable guard to a mount directory after unmount.
// If the directory is not empty (which indicates a bug - data written outside encrypted volume),
// it logs a CRITICAL error but does NOT delete anything. This ensures bugs are surfaced
// immediately rather than silently causing data loss.
//
// When files are found, gocryptfs will fail to mount on next attempt (it requires an
// empty mount directory), which is the desired "fail loudly" behavior.
func reprotectMountDir(volumeID, mountDir string) {
	// Check if directory is empty - it should be after gocryptfs unmount
	entries, err := os.ReadDir(mountDir)
	if err != nil && !os.IsNotExist(err) {
		log.Printf("WARN: volume %s: failed to read mount directory: %v", volumeID, err)
	}

	if len(entries) > 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		log.Printf("CRITICAL: volume %s: mount directory not empty after unmount - contains: %v. This indicates data was written outside the encrypted volume (a bug). NOT cleaning to preserve data. gocryptfs will fail to mount until this is manually resolved.", volumeID, names)
	}

	// Re-apply immutable guard if configured
	if shouldGuardMountDir(volumeID) {
		if err := protectMountDir(mountDir); err != nil {
			log.Printf("WARN: volume %s: failed to re-protect mountpoint: %v", volumeID, err)
		}
	}
}

// ensureCipherDir creates the ciphertext directory for a volume. For
// control-class volumes it attempts btrfs subvolume create to enable atomic
// snapshots for PCV publishing. Other volumes use a regular directory.
func (f *fileVolumeManager) ensureCipherDir(class VolumeClass, cipherDir string) error {
	if _, err := os.Stat(cipherDir); err == nil {
		return nil // already exists
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(cipherDir), 0o700); err != nil {
		return err
	}

	if class != VolumeClassControl {
		return os.MkdirAll(cipherDir, 0o700)
	}

	// In test/bypass mode, skip btrfs and use a regular directory.
	if f.bypassMount {
		return os.MkdirAll(cipherDir, 0o700)
	}

	// Control-plane: prefer btrfs subvolume for PCV snapshots.
	cmd := exec.CommandContext(context.Background(), "btrfs", "subvolume", "create", cipherDir)
	if err := cmd.Run(); err != nil {
		// Dev fallback: allow regular directory when PICCOLO_DEV_NO_BTRFS=1
		// AND the test-image sentinel exists.
		if os.Getenv("PICCOLO_DEV_NO_BTRFS") == "1" {
			if _, sErr := os.Stat("/etc/piccolo-test-image"); sErr == nil {
				log.Printf("WARN: btrfs subvolume create %s failed (%v), using regular directory (dev fallback)", cipherDir, err)
				return os.MkdirAll(cipherDir, 0o700)
			}
		}
		return fmt.Errorf("btrfs subvolume create %s: %w", cipherDir, err)
	}
	return nil
}

// ensureVolumePrerequisites performs idempotent setup for a volume entry:
// creates the cipher dir, mount dir (with stale FUSE cleanup), applies the
// immutable guard, and initializes metadata. Safe to call on every
// EnsureVolume invocation — each operation early-exits if already done.
//
// Concurrency safety: callers hold entry.mountMu to serialize the
// non-atomic stat-then-create in ensureCipherDir. Fields read from entry
// (handle, cipherDir, class) are immutable after creation. ensureMetadata
// serializes via entry.metaMu internally.
func (f *fileVolumeManager) ensureVolumePrerequisites(ctx context.Context, entry *volumeEntry) error {
	if err := f.ensureCipherDir(entry.class, entry.cipherDir); err != nil {
		return fmt.Errorf("ensure volume %s ciphertext: %w", entry.handle.ID, err)
	}
	// Ensure the mounts/ parent directory is traversable (0o711) so per-app
	// users can reach their own mount points without being able to list siblings.
	// Chmod explicitly since MkdirAll is a no-op on existing directories.
	mountsParent := filepath.Dir(entry.handle.MountDir)
	if err := os.MkdirAll(mountsParent, 0o711); err != nil {
		return fmt.Errorf("ensure volume %s mounts parent: %w", entry.handle.ID, err)
	}
	if err := os.Chmod(mountsParent, 0o711); err != nil {
		return fmt.Errorf("chmod mounts parent for volume %s: %w", entry.handle.ID, err)
	}
	if err := os.MkdirAll(entry.handle.MountDir, 0o700); err != nil {
		if mounted, mErr := isMountPoint(entry.handle.MountDir); mErr == nil && mounted {
			log.Printf("WARN: volume %s: stale mount at %s during init, cleaning up", entry.handle.ID, entry.handle.MountDir)
			if fErr := f.runner.Run(ctx, f.fusermountPath, []string{"-uz", entry.handle.MountDir}, nil); fErr != nil {
				log.Printf("WARN: volume %s: fusermount cleanup failed: %v", entry.handle.ID, fErr)
			}
			if shouldGuardMountDir(entry.handle.ID) {
				_ = unprotectMountDir(entry.handle.MountDir)
			}
			err = os.MkdirAll(entry.handle.MountDir, 0o700)
		}
		if err != nil {
			return fmt.Errorf("ensure volume %s mount: %w", entry.handle.ID, err)
		}
	}
	if shouldGuardMountDir(entry.handle.ID) && !f.bypassMount {
		if mounted, err := isMountPoint(entry.handle.MountDir); err == nil && !mounted {
			if err := protectMountDir(entry.handle.MountDir); err != nil {
				log.Printf("WARN: volume %s: failed to protect mountpoint: %v", entry.handle.ID, err)
			}
		}
	}
	if err := f.ensureMetadata(ctx, entry); err != nil {
		if !errors.Is(err, crypt.ErrLocked) && !errors.Is(err, crypt.ErrNotInitialized) {
			return err
		}
	}
	return nil
}

func (f *fileVolumeManager) EnsureVolume(ctx context.Context, req VolumeRequest) (VolumeHandle, error) {
	f.mu.Lock()
	entry, ok := f.volumes[req.ID]
	if ok {
		f.mu.Unlock()
		// Warn-only, not an error: inferVolumeClass (used by getOrCreateEntry)
		// and req.Class are always consistent for current ID conventions.
		// Erroring here would break reconcile-then-EnsureVolume flows where
		// the entry was pre-registered with the inferred class.
		if entry.class != req.Class && req.Class != "" {
			log.Printf("WARN: volume %s: class mismatch: entry=%s req=%s", req.ID, entry.class, req.Class)
		}
		entry.mountMu.Lock()
		err := f.ensureVolumePrerequisites(ctx, entry)
		entry.mountMu.Unlock()
		if err != nil {
			return entry.handle, err
		}
		if err := f.reconcileVolumeState(ctx, entry); err != nil {
			return entry.handle, err
		}
		return entry.handle, nil
	}
	// Hold the lock to prevent concurrent init of the same volume.
	// Create a placeholder entry so other callers see it immediately.
	// Use the explicit class from the request for first-creation path routing.
	cipherDir, stateDir, mountDir := f.volumePathsForClass(req.ID, req.Class)
	entry = &volumeEntry{
		handle:    VolumeHandle{ID: req.ID, MountDir: mountDir},
		cipherDir: cipherDir,
		stateDir:  stateDir,
		class:     req.Class,
	}
	f.volumes[req.ID] = entry
	f.mu.Unlock()

	initOK := false
	defer func() {
		if !initOK {
			f.mu.Lock()
			// Only remove if the map still points to our entry; a concurrent
			// caller may have replaced it after we released the lock.
			if f.volumes[req.ID] == entry {
				delete(f.volumes, req.ID)
			}
			f.mu.Unlock()
		}
	}()

	entry.mountMu.Lock()
	err := f.ensureVolumePrerequisites(ctx, entry)
	entry.mountMu.Unlock()
	if err != nil {
		return VolumeHandle{}, err
	}

	initOK = true
	if err := f.reconcileVolumeState(ctx, entry); err != nil {
		return entry.handle, err
	}
	return entry.handle, nil
}

func (f *fileVolumeManager) Attach(ctx context.Context, handle VolumeHandle, opts AttachOptions) error {
	f.mu.RLock()
	entry, ok := f.volumes[handle.ID]
	checker := f.roleChecker
	f.mu.RUnlock()
	if !ok {
		return fmt.Errorf("attach: unknown volume %s", handle.ID)
	}
	if opts.Role == VolumeRoleLeader && checker != nil && !checker(handle.ID, opts.Role) {
		return fmt.Errorf("attach: leadership not granted for volume %s", handle.ID)
	}

	// Serialize mount operations for this volume to ensure idempotency.
	entry.mountMu.Lock()
	defer entry.mountMu.Unlock()

	// Volume already mounted by this process — return early.
	// Defense-in-depth below handles stale mounts from previous processes only.
	// We probe the Wait channel to detect a gocryptfs process that died without
	// an explicit Detach (awaitProcessExit only runs during Detach).
	if alive := func() bool {
		f.mu.RLock()
		p := entry.process
		f.mu.RUnlock()
		if p == nil {
			return false
		}
		select {
		case <-p.Wait():
			// Process exited unexpectedly — clear and fall through to re-mount.
			f.mu.Lock()
			if entry.process == p {
				entry.process = nil
			}
			f.mu.Unlock()
			return false
		default:
			return true
		}
	}(); alive {
		return nil
	}

	if err := os.MkdirAll(handle.MountDir, 0o700); err != nil {
		return fmt.Errorf("attach: ensure mount directory for volume %s: %w", handle.ID, err)
	}
	if shouldGuardMountDir(handle.ID) && !f.bypassMount {
		if mounted, err := isMountPoint(handle.MountDir); err == nil && !mounted {
			if err := protectMountDir(handle.MountDir); err != nil {
				log.Printf("WARN: volume %s: failed to protect mountpoint: %v", handle.ID, err)
			}
		}
	}

	if f.bypassMount {
		if err := f.ensureMetadata(ctx, entry); err != nil {
			return err
		}
		modeBytes := []byte("rw")
		if opts.Role == VolumeRoleFollower {
			modeBytes = []byte("ro")
		}
		if err := fsutil.AtomicWriteFile(filepath.Join(entry.handle.MountDir, ".mode"), modeBytes, 0o600); err != nil {
			return err
		}
		if err := fsutil.AtomicWriteFile(filepath.Join(entry.handle.MountDir, ".cipher"), []byte(entry.cipherDir), 0o600); err != nil {
			return err
		}
		f.mu.Lock()
		entry.role = opts.Role
		entry.metadataReady = true
		f.mu.Unlock()
		return f.recordVolumeState(handle.ID, volumeStateMounted, volumeStateMounted, opts.Role, nil)
	}

	// Defense-in-depth: if a mount already exists (stale mount missed by startup cleanup),
	// clean it up before creating a fresh mount to prevent double-stacking.
	// isMountPoint reads /proc/self/mountinfo (procfs text, no FUSE round-trip) —
	// reliable even on stale FUSE mounts, unlike os.Stat which returns cached VFS attributes.
	if mounted, err := isMountPoint(handle.MountDir); err == nil && mounted {
		log.Printf("WARN: volume %s: clearing pre-existing mount at %s (should not happen if startup cleanup ran)", handle.ID, handle.MountDir)
		if detachErr := f.Detach(ctx, handle); detachErr != nil {
			log.Printf("WARN: volume %s: detach failed: %v", handle.ID, detachErr)
		} else if shouldGuardMountDir(handle.ID) {
			// Detach applies immutable flag via reprotectMountDir; clear it for fresh mount.
			// Only unprotect if Detach succeeded (mount is gone), otherwise we'd modify
			// the mounted filesystem's root rather than the underlying mountpoint directory.
			_ = unprotectMountDir(handle.MountDir)
		}
		// Post-condition: verify mount was actually cleared.
		if still, _ := isMountPoint(handle.MountDir); still {
			return fmt.Errorf("attach: cannot clear pre-existing mount at %s", handle.MountDir)
		}
	}

	if err := f.ensureMetadata(ctx, entry); err != nil {
		return err
	}

	passphrase, err := f.unwrapVolumeKey(ctx, entry.metadata)
	if err != nil {
		return err
	}

	if err := f.recordVolumeState(handle.ID, volumeStateMounted, volumeStatePending, opts.Role, nil); err != nil {
		return err
	}

	args := []string{"-f", "-q", "-passfile", "/dev/stdin", "-allow_other"}
	if opts.Role == VolumeRoleFollower {
		args = append(args, "-ro")
	}
	args = append(args, entry.cipherDir, entry.handle.MountDir)

	proc, err := f.launcher.Launch(ctx, f.gocryptfsPath, args, append(passphrase, '\n'))
	if err != nil {
		_ = f.recordVolumeState(handle.ID, volumeStateMounted, volumeStateError, opts.Role, err)
		return fmt.Errorf("mount volume %s: %w", handle.ID, err)
	}
	if err := f.waitMount(entry.handle.MountDir, 5*time.Second); err != nil {
		_ = proc.Signal(syscall.SIGTERM)
		select {
		case <-proc.Wait():
		case <-time.After(2 * time.Second):
			_ = proc.Kill()
			<-proc.Wait()
		}
		_ = f.recordVolumeState(handle.ID, volumeStateMounted, volumeStateError, opts.Role, err)
		return fmt.Errorf("wait for mount %s: %w", handle.ID, err)
	}

	mode := []byte("rw")
	if opts.Role == VolumeRoleFollower {
		mode = []byte("ro")
	}
	if err := fsutil.AtomicWriteFile(filepath.Join(entry.handle.MountDir, ".mode"), mode, 0o600); err != nil {
		return err
	}
	if err := fsutil.AtomicWriteFile(filepath.Join(entry.handle.MountDir, ".cipher"), []byte(entry.cipherDir), 0o600); err != nil {
		return err
	}

	f.mu.Lock()
	entry.role = opts.Role
	entry.metadataReady = true
	entry.process = proc
	f.mu.Unlock()
	if err := f.recordVolumeState(handle.ID, volumeStateMounted, volumeStateMounted, opts.Role, nil); err != nil {
		return err
	}
	return nil
}

func (f *fileVolumeManager) Detach(ctx context.Context, handle VolumeHandle) error {
	if f.bypassMount {
		role := VolumeRoleUnknown
		f.mu.Lock()
		if entry, ok := f.volumes[handle.ID]; ok {
			role = entry.role
			entry.role = VolumeRoleUnknown
		}
		f.mu.Unlock()
		return f.recordVolumeState(handle.ID, volumeStateUnmounted, volumeStateUnmounted, role, nil)
	}
	// Use retry logic to handle cases where container cleanup (fuse-overlayfs unmount)
	// is still in progress after container stop. This is especially important during
	// graceful shutdown where podman stop returns before container storage is fully released.
	return f.detachWithRetry(ctx, handle)
}

// fusermountDetach performs the fusermount unmount, state recording, and process
// cleanup without the post-unmount directory cleanup. Used by detachWithRetry to
// avoid running cleanup on each retry attempt.
func (f *fileVolumeManager) fusermountDetach(ctx context.Context, handle VolumeHandle) error {
	role := VolumeRoleUnknown
	f.mu.RLock()
	if entry, ok := f.volumes[handle.ID]; ok {
		role = entry.role
	}
	f.mu.RUnlock()
	if err := f.recordVolumeState(handle.ID, volumeStateUnmounted, volumeStatePending, role, nil); err != nil {
		return err
	}
	args := []string{"-u", handle.MountDir}
	if err := f.runner.Run(ctx, f.fusermountPath, args, nil); err != nil {
		_ = f.recordVolumeState(handle.ID, volumeStateUnmounted, volumeStateError, role, err)
		return fmt.Errorf("detach volume %s: %w", handle.ID, err)
	}
	f.awaitProcessExit(handle.ID)
	if err := f.recordVolumeState(handle.ID, volumeStateUnmounted, volumeStateUnmounted, role, nil); err != nil {
		return err
	}
	return nil
}

func (f *fileVolumeManager) DestroyVolume(ctx context.Context, id string) error {
	cipherDir, stateDir, mountDir := f.volumePaths(id)
	handle := VolumeHandle{ID: id, MountDir: mountDir}

	// Acquire the entry's metaMu to serialize against concurrent Attach/ensureMetadata.
	f.mu.RLock()
	entry, hasEntry := f.volumes[id]
	f.mu.RUnlock()
	if hasEntry {
		entry.metaMu.Lock()
		defer entry.metaMu.Unlock()
	}

	// 1. Detach if mounted - with retries and lazy unmount fallback
	if mounted, _ := isMountPoint(mountDir); mounted {
		if err := f.detachWithRetry(ctx, handle); err != nil {
			return fmt.Errorf("destroy: detach failed: %w", err)
		}
	}

	// 2. Remove Ciphertext
	if err := os.RemoveAll(cipherDir); err != nil {
		return fmt.Errorf("destroy: remove ciphertext: %w", err)
	}

	// 3. Remove Metadata/State
	if err := os.RemoveAll(stateDir); err != nil {
		return fmt.Errorf("destroy: remove state: %w", err)
	}

	// 4. Remove Mountpoint (use RemoveAll to handle non-empty directories from lazy unmounts)
	if shouldGuardMountDir(id) {
		if err := unprotectMountDir(mountDir); err != nil {
			return fmt.Errorf("destroy: unprotect mountpoint: %w", err)
		}
	}
	if err := os.RemoveAll(mountDir); err != nil {
		log.Printf("WARN: destroy volume %s: failed to remove mount directory: %v", id, err)
	}

	// 5. Remove from memory
	f.mu.Lock()
	delete(f.volumes, id)
	f.mu.Unlock()

	return nil
}

// detachWithRetry attempts normal unmount first, then retries with lazy unmount if device is busy.
func (f *fileVolumeManager) detachWithRetry(ctx context.Context, handle VolumeHandle) error {
	const maxRetries = 3
	const retryDelay = 500 * time.Millisecond

	// First try normal unmount (without post-unmount cleanup)
	err := f.fusermountDetach(ctx, handle)
	if err == nil {
		reprotectMountDir(handle.ID, handle.MountDir)
		return nil
	}

	// Retry only if fusermount exited non-zero (e.g. device busy); propagate
	// other errors (context cancelled, binary not found, etc.) immediately.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return err
	}

	// Retry with delay - sometimes processes need a moment to release
	for i := 0; i < maxRetries; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryDelay):
		}

		// Check if still mounted
		if mounted, _ := isMountPoint(handle.MountDir); !mounted {
			reprotectMountDir(handle.ID, handle.MountDir)
			return nil
		}

		// Try normal unmount again (no cleanup on each retry)
		if err := f.fusermountDetach(ctx, handle); err == nil {
			reprotectMountDir(handle.ID, handle.MountDir)
			return nil
		}
	}

	// Last resort: lazy unmount (-z flag) which detaches immediately and cleans up when no longer busy
	args := []string{"-uz", handle.MountDir}
	if err := f.runner.Run(ctx, f.fusermountPath, args, nil); err != nil {
		return fmt.Errorf("lazy unmount failed for %s: %w", handle.ID, err)
	}

	f.awaitProcessExit(handle.ID)

	// Give a moment for lazy unmount to complete
	time.Sleep(100 * time.Millisecond)

	// Verify unmount succeeded
	if mounted, _ := isMountPoint(handle.MountDir); mounted {
		return fmt.Errorf("volume %s still mounted after lazy unmount", handle.ID)
	}

	reprotectMountDir(handle.ID, handle.MountDir)
	return nil
}

func (f *fileVolumeManager) RoleStream(volumeID string) (<-chan VolumeRole, error) {
	ch := make(chan VolumeRole)
	close(ch)
	return ch, nil
}

func (f *fileVolumeManager) ensureMetadata(ctx context.Context, entry *volumeEntry) error {
	entry.metaMu.Lock()
	defer entry.metaMu.Unlock()

	// 1. Check State Directory (New Location)
	metaPath := filepath.Join(entry.stateDir, volumeMetadataName)
	data, err := os.ReadFile(metaPath)

	// 2. Check Cipher Directory (Legacy Location) & Migrate
	if errors.Is(err, os.ErrNotExist) {
		legacyPath := filepath.Join(entry.cipherDir, volumeMetadataName)
		if legacyData, legacyErr := os.ReadFile(legacyPath); legacyErr == nil {
			// Found in legacy location. Migrate to state dir.
			if err := os.MkdirAll(entry.stateDir, 0o700); err != nil {
				return fmt.Errorf("ensure state dir for migration: %w", err)
			}
			if err := fsutil.AtomicWriteFile(metaPath, legacyData, 0o600); err != nil {
				return fmt.Errorf("migrate metadata: %w", err)
			}
			// Remove from legacy location to stop gocryptfs warnings
			_ = os.Remove(legacyPath)
			data = legacyData
			err = nil
		}
	}

	if err == nil {
		var meta volumeMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			return fmt.Errorf("%w: %v", ErrVolumeMetadataCorrupted, err)
		}
		if meta.WrappedKey == "" || meta.Nonce == "" {
			return fmt.Errorf("%w: missing fields", ErrVolumeMetadataCorrupted)
		}
		entry.metadata = meta
		entry.metadataReady = true
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read volume metadata %s: %w", metaPath, err)
	}

	entry.metadataReady = false
	if f.bypassMount {
		meta := volumeMetadata{
			Version:    metadataVersion,
			WrappedKey: base64.StdEncoding.EncodeToString([]byte("piccolo-bypass-key")),
			Nonce:      base64.StdEncoding.EncodeToString([]byte("bypass-nonce!!")),
		}
		metaBytes, err := json.MarshalIndent(&meta, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(entry.stateDir, 0o700); err != nil {
			return err
		}
		if err := fsutil.AtomicWriteFile(metaPath, metaBytes, 0o600); err != nil {
			return err
		}
		// Bypass mode also needs a legacy conf file check? Or just ensure cipherDir init?
		// Usually bypass doesn't use real gocryptfs, but let's leave legacy conf check if needed.
		// Actually, bypass usually just writes metadata.
		entry.metadata = meta
		entry.metadataReady = true
		return nil
	}

	// Defense-in-depth: refuse re-init if the cipher directory already contains
	// gocryptfs state. This prevents data loss from a race where metadata is
	// temporarily unreadable (e.g. unmounted data volume) but gocryptfs artifacts
	// exist from a prior initialization.
	// See: docs/rca/20260212-gocryptfs-password-mismatch-on-reboot.md
	for _, artifact := range []string{"gocryptfs.conf", "gocryptfs.diriv"} {
		p := filepath.Join(entry.cipherDir, artifact)
		if _, statErr := os.Stat(p); statErr == nil {
			return fmt.Errorf("%s exists in %s but volume metadata is missing — refusing re-init to prevent data loss", artifact, entry.cipherDir)
		}
	}

	passphrase, err := generatePassphrase()
	if err != nil {
		return err
	}

	meta, err := f.sealVolumeKey(ctx, passphrase)
	if err != nil {
		return err
	}

	if err := f.runner.Run(ctx, f.gocryptfsPath, []string{"-q", "-init", "-passfile", "/dev/stdin", entry.cipherDir}, append(passphrase, '\n')); err != nil {
		return fmt.Errorf("init gocryptfs for %s: %w", entry.cipherDir, err)
	}

	metaBytes, err := json.MarshalIndent(&meta, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(entry.stateDir, 0o700); err != nil {
		return err
	}
	if err := fsutil.AtomicWriteFile(metaPath, metaBytes, 0o600); err != nil {
		return err
	}

	entry.metadata = meta
	entry.metadataReady = true
	return nil
}

func (f *fileVolumeManager) recordVolumeState(volumeID string, desired string, observed string, role VolumeRole, cause error) error {
	if volumeID == "" {
		return nil
	}
	prev, err := f.readVolumeState(volumeID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Carry forward the persisted class; infer from ID if missing (defense-in-depth).
	class := prev.Class
	if class == "" {
		class = string(inferVolumeClass(volumeID))
	}
	state := volumeState{
		Class:     class,
		Desired:   desired,
		Observed:  observed,
		Role:      string(role),
		UpdatedAt: time.Now().UTC(),
	}
	if cause != nil {
		state.LastError = cause.Error()
	} else if prev.LastError != "" && observed != volumeStateError {
		// Clear stale errors once we transition back to a stable state.
		state.LastError = ""
	}
	state.Generation = prev.Generation + 1
	state.NeedsRepair = prev.NeedsRepair
	if observed == volumeStateError || cause != nil {
		state.NeedsRepair = true
	}
	if (observed == volumeStateMounted || observed == volumeStateUnmounted) && cause == nil && observed == desired {
		state.NeedsRepair = false
	}
	if err := f.writeVolumeState(volumeID, state); err != nil {
		return err
	}
	f.publishVolumeEvent(volumeID, state)
	return nil
}

func (f *fileVolumeManager) readVolumeState(volumeID string) (volumeState, error) {
	_, stateDir, _ := f.volumePaths(volumeID)
	path := filepath.Join(stateDir, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return volumeState{}, err
	}
	var state volumeState
	if err := json.Unmarshal(data, &state); err != nil {
		return volumeState{}, err
	}
	return state, nil
}

func (f *fileVolumeManager) publishVolumeEvent(volumeID string, state volumeState) {
	if f.bus == nil {
		return
	}
	f.bus.Publish(events.Event{
		Topic: events.TopicVolumeStateChanged,
		Payload: events.VolumeStateChanged{
			ID:          volumeID,
			Desired:     state.Desired,
			Observed:    state.Observed,
			Role:        state.Role,
			Generation:  state.Generation,
			NeedsRepair: state.NeedsRepair,
			LastError:   state.LastError,
		},
	})
}

func (f *fileVolumeManager) reconcileVolumeState(ctx context.Context, entry *volumeEntry) error {
	if f.bypassMount {
		return nil
	}
	state, err := f.readVolumeState(entry.handle.ID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	mounted, err := isMountPoint(entry.handle.MountDir)
	if err != nil {
		return err
	}
	if mounted {
		// Probe mount liveness via readdir. getdents64(2) forces a FUSE round-trip
		// that fails with ENOTCONN on stale mounts. os.Stat (stat(2)) is unreliable
		// because the kernel returns cached VFS attributes for stale FUSE mountpoints.
		var probeErr error
		if dir, err := os.Open(entry.handle.MountDir); err != nil {
			probeErr = err
		} else {
			_, probeErr = dir.Readdirnames(1)
			dir.Close()
			if errors.Is(probeErr, io.EOF) {
				probeErr = nil // Empty directory — mount is live
			}
		}
		if probeErr != nil {
			if errors.Is(probeErr, syscall.ENOTCONN) {
				_ = f.recordVolumeState(entry.handle.ID, state.Desired, volumeStateError, parseVolumeRole(state.Role), fmt.Errorf("broken mount detected: %w", probeErr))
				if detachErr := f.Detach(ctx, entry.handle); detachErr != nil {
					return fmt.Errorf("broken mount detected for %s, detach failed: %w", entry.handle.ID, detachErr)
				}
				mounted = false
			} else {
				log.Printf("WARN: volume %s: mount liveness probe failed: %v", entry.handle.ID, probeErr)
			}
		}
	}
	role := parseVolumeRole(state.Role)
	if state.Desired == volumeStateMounted && !mounted {
		checker := f.roleChecker
		if state.Role == string(VolumeRoleLeader) && checker != nil && !checker(entry.handle.ID, VolumeRoleLeader) {
			return nil
		}
		if role == VolumeRoleUnknown {
			return f.recordVolumeState(entry.handle.ID, state.Desired, volumeStateError, role, fmt.Errorf("volume %s missing mount and role unknown", entry.handle.ID))
		}
		if err := f.Attach(ctx, entry.handle, AttachOptions{Role: role}); err != nil {
			if errors.Is(err, crypt.ErrLocked) || errors.Is(err, crypt.ErrNotInitialized) {
				if recErr := f.recordVolumeState(entry.handle.ID, state.Desired, volumeStatePending, role, err); recErr != nil {
					return recErr
				}
				return nil
			}
			return err
		}
		return nil
	}
	if state.Desired == volumeStateUnmounted && mounted {
		if err := f.Detach(ctx, entry.handle); err != nil {
			return err
		}
		return nil
	}
	if state.NeedsRepair && state.Desired == volumeStateMounted && mounted {
		return f.recordVolumeState(entry.handle.ID, state.Desired, volumeStateMounted, role, nil)
	}
	if state.NeedsRepair && state.Desired == volumeStateUnmounted && !mounted {
		return f.recordVolumeState(entry.handle.ID, state.Desired, volumeStateUnmounted, role, nil)
	}
	return nil
}

func parseVolumeRole(role string) VolumeRole {
	switch role {
	case string(VolumeRoleLeader):
		return VolumeRoleLeader
	case string(VolumeRoleFollower):
		return VolumeRoleFollower
	default:
		return VolumeRoleUnknown
	}
}

func (f *fileVolumeManager) reconcileAllVolumeStates() error {
	seen := make(map[string]bool)

	// Scan both roots for volume state directories.
	for _, root := range []string{f.coreRoot, f.dataRoot} {
		stateRoot := filepath.Join(root, "volumes")
		entries, err := os.ReadDir(stateRoot)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // dataRoot may not be mounted yet
			}
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			volID := entry.Name()
			if seen[volID] {
				continue
			}
			seen[volID] = true
			volEntry := f.getOrCreateEntry(volID)
			if err := f.reconcileVolumeState(context.Background(), volEntry); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fileVolumeManager) writeVolumeState(volumeID string, state volumeState) error {
	_, stateDir, _ := f.volumePaths(volumeID)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(&state, "", "  ")
	if err != nil {
		return err
	}
	finalPath := filepath.Join(stateDir, "state.json")
	if err := fsutil.AtomicWriteFile(finalPath, data, 0o600); err != nil {
		return err
	}
	// Also sync the parent volumes dir for extra durability.
	return fsutil.SyncDir(filepath.Dir(stateDir))
}

func (f *fileVolumeManager) sealVolumeKey(ctx context.Context, passphrase []byte) (volumeMetadata, error) {
	if f.crypto == nil {
		return volumeMetadata{}, errors.New("crypto manager unavailable")
	}
	meta := volumeMetadata{Version: metadataVersion}
	err := f.crypto.WithSDEK(func(sdek []byte) error {
		block, err := aes.NewCipher(sdek)
		if err != nil {
			return err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return err
		}
		nonce := make([]byte, aead.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return err
		}
		sealed := aead.Seal(nil, nonce, passphrase, nil)
		meta.WrappedKey = base64.StdEncoding.EncodeToString(sealed)
		meta.Nonce = base64.StdEncoding.EncodeToString(nonce)
		return nil
	})
	if err != nil {
		return volumeMetadata{}, err
	}
	return meta, nil
}

func (f *fileVolumeManager) unwrapVolumeKey(ctx context.Context, meta volumeMetadata) ([]byte, error) {
	if f.crypto == nil {
		return nil, errors.New("crypto manager unavailable")
	}
	var passphrase []byte
	err := f.crypto.WithSDEK(func(sdek []byte) error {
		block, err := aes.NewCipher(sdek)
		if err != nil {
			return err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return err
		}
		nonce, err := base64.StdEncoding.DecodeString(meta.Nonce)
		if err != nil {
			return fmt.Errorf("%w: decode nonce: %v", ErrVolumeMetadataCorrupted, err)
		}
		if len(nonce) != aead.NonceSize() {
			return fmt.Errorf("%w: invalid nonce length %d (expected %d)", ErrVolumeMetadataCorrupted, len(nonce), aead.NonceSize())
		}
		sealed, err := base64.StdEncoding.DecodeString(meta.WrappedKey)
		if err != nil {
			return fmt.Errorf("%w: decode wrapped key: %v", ErrVolumeMetadataCorrupted, err)
		}
		if len(sealed) == 0 {
			return fmt.Errorf("%w: empty wrapped key", ErrVolumeMetadataCorrupted)
		}
		key, err := aead.Open(nil, nonce, sealed, nil)
		if err != nil {
			return fmt.Errorf("%w: unwrap failed: %v", ErrVolumeMetadataCorrupted, err)
		}
		passphrase = key
		return nil
	})
	if err != nil {
		return nil, err
	}
	return passphrase, nil
}

func (f *fileVolumeManager) awaitProcessExit(volumeID string) {
	f.mu.Lock()
	entry, ok := f.volumes[volumeID]
	if !ok {
		f.mu.Unlock()
		return
	}
	proc := entry.process
	entry.process = nil
	f.mu.Unlock()
	if proc == nil {
		return
	}
	select {
	case <-proc.Wait():
	case <-time.After(2 * time.Second):
		_ = proc.Kill()
		<-proc.Wait()
	}
}

func generatePassphrase() ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate passphrase: %w", err)
	}
	encoded := base64.RawStdEncoding.EncodeToString(raw)
	return []byte(encoded), nil
}

var _ VolumeManager = (*fileVolumeManager)(nil)

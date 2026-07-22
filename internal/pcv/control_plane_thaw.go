package pcv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"
)

// These are _IOWR('X', 119, int) and _IOWR('X', 120, int) from linux/fs.h.
// x/sys does not expose the filesystem freeze/thaw request constants.
const (
	linuxFIFREEZE = 0xc0045877
	linuxFITHAW   = 0xc0045878

	controlPlaneFreezeIntentPath = "/run/piccolo/control-plane-freeze.intent"
)

type controlPlaneThawOps struct {
	open   func(string) (int, error)
	freeze func(int) error
	thaw   func(int) error
	close  func(int) error
}

func linuxControlPlaneThawOps() controlPlaneThawOps {
	return controlPlaneThawOps{
		open: func(path string) (int, error) {
			return unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		},
		freeze: func(fd int) error {
			_, err := unix.IoctlRetInt(fd, linuxFIFREEZE)
			return err
		},
		thaw: func(fd int) error {
			_, err := unix.IoctlRetInt(fd, linuxFITHAW)
			return err
		},
		close: unix.Close,
	}
}

// controlPlaneThawCoordinator owns the sole in-flight control-plane thaw.
// PCV publishes are serialized, so a single slot mirrors the real lifecycle
// without introducing another scheduler or durable registry.
type controlPlaneThawCoordinator struct {
	mu          sync.Mutex
	active      *controlPlaneThawObligation
	fatalFenced atomic.Bool
	ops         controlPlaneThawOps
	intentPath  string
}

type controlPlaneThawObligation struct {
	coordinator *controlPlaneThawCoordinator
	fd          int
	done        bool
	err         error
}

func newControlPlaneThawCoordinator(ops controlPlaneThawOps) *controlPlaneThawCoordinator {
	return &controlPlaneThawCoordinator{ops: ops}
}

func newPersistentControlPlaneThawCoordinator(ops controlPlaneThawOps, intentPath string) *controlPlaneThawCoordinator {
	return &controlPlaneThawCoordinator{ops: ops, intentPath: intentPath}
}

var errControlPlaneFreezeFatalFenced = errors.New("control-plane freeze fenced by process-fatal cleanup")

// freeze serializes the in-process open -> FIFREEZE -> active lifecycle. The
// durable-in-/run intent is committed before FIFREEZE, so a fatal fence may
// return immediately even if the syscall is still unresolved: either no freeze
// occurred, or ExecStopPost owns a conservative thaw obligation. There is no
// success-to-registration gap that depends on this process surviving.
func (c *controlPlaneThawCoordinator) freeze(mountDir string) (*controlPlaneThawObligation, error) {
	if c == nil {
		return nil, errors.New("nil control-plane freeze/thaw coordinator")
	}
	if c.fatalFenced.Load() {
		return nil, errControlPlaneFreezeFatalFenced
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fatalFenced.Load() {
		return nil, errControlPlaneFreezeFatalFenced
	}
	if c.active != nil {
		return nil, errors.New("control-plane thaw obligation already active")
	}

	fd, err := c.ops.open(mountDir)
	if err != nil {
		return nil, fmt.Errorf("open control-plane mount for thaw: %w", err)
	}
	if err := writeControlPlaneFreezeIntent(c.intentPath); err != nil {
		return nil, errors.Join(fmt.Errorf("record control-plane freeze intent: %w", err), c.ops.close(fd))
	}
	o := &controlPlaneThawObligation{coordinator: c, fd: fd}
	if err := c.ops.freeze(fd); err != nil {
		return nil, errors.Join(
			fmt.Errorf("freeze control-plane mount: %w", err),
			c.ops.close(fd),
			clearControlPlaneFreezeIntent(c.intentPath),
		)
	}
	c.active = o
	return o, nil
}

// thaw performs at most one ordinary in-process FITHAW attempt and closes the
// prepared mount descriptor. The independent post-stop owner runs only after
// this process has exited; a retained intent makes a redundant later thaw safe.
func (o *controlPlaneThawObligation) thaw() error {
	if o == nil || o.coordinator == nil {
		return nil
	}
	c := o.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.thawLocked(o)
}

func (c *controlPlaneThawCoordinator) thawActive() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.thawLocked(c.active)
}

// fenceFatal is intentionally allocation-free and cannot wait for an ioctl or
// the coordinator mutex. The process-fatal owner may therefore keep its
// absolute exit deadline even when an in-flight kernel freeze/thaw is stuck.
// Any freeze that crossed the fence race already has a conservative /run
// intent which transfers thaw ownership to systemd ExecStopPost.
func (c *controlPlaneThawCoordinator) fenceFatal() {
	if c != nil {
		c.fatalFenced.Store(true)
	}
}

func (c *controlPlaneThawCoordinator) thawLocked(o *controlPlaneThawObligation) error {
	if o == nil {
		return nil
	}
	if o.done {
		return o.err
	}
	thawErr := c.ops.thaw(o.fd)
	if errors.Is(thawErr, unix.EINVAL) {
		// The filesystem was already thawed by the independent post-stop owner.
		thawErr = nil
	}
	closeErr := c.ops.close(o.fd)
	var intentErr error
	if thawErr == nil && closeErr == nil {
		intentErr = clearControlPlaneFreezeIntent(c.intentPath)
	}
	o.err = errors.Join(thawErr, closeErr, intentErr)
	o.done = true
	if c.active == o {
		c.active = nil
	}
	return o.err
}

var processControlPlaneThaw = newPersistentControlPlaneThawCoordinator(
	linuxControlPlaneThawOps(),
	controlPlaneFreezeIntentPath,
)

// FenceEmergencyControlPlaneFreeze prevents new PCV freezes without waiting
// for an in-flight kernel ioctl. The ordinary publisher may still thaw before
// process exit; otherwise the persisted intent is recovered by ExecStopPost.
func FenceEmergencyControlPlaneFreeze() {
	processControlPlaneThaw.fenceFatal()
}

// RecoverPendingControlPlaneThaw is the independent systemd/replacement-process
// recovery boundary. It is deliberately called before service-exit recording
// and before normal startup touches control-plane state.
func RecoverPendingControlPlaneThaw() error {
	return recoverPendingControlPlaneThaw(
		controlPlaneFreezeIntentPath,
		paths.MountDir("control-plane"),
		linuxControlPlaneThawOps(),
	)
}

func recoverPendingControlPlaneThaw(intentPath, mountDir string, ops controlPlaneThawOps) error {
	pending, err := controlPlaneFreezeIntentPending(intentPath)
	if err != nil {
		return fmt.Errorf("inspect control-plane freeze intent: %w", err)
	}
	if !pending {
		return nil
	}

	fd, err := ops.open(mountDir)
	if err != nil {
		return fmt.Errorf("open control-plane mount for emergency thaw: %w", err)
	}
	thawErr := ops.thaw(fd)
	if errors.Is(thawErr, unix.EINVAL) {
		// A conservative intent may outlive an already-completed ordinary thaw.
		thawErr = nil
	}
	closeErr := ops.close(fd)
	if thawErr != nil || closeErr != nil {
		// Retain the intent so the next independent owner retries.
		return errors.Join(
			wrapControlPlaneThawError(thawErr),
			wrapControlPlaneCloseError(closeErr),
		)
	}
	if err := clearControlPlaneFreezeIntent(intentPath); err != nil {
		return fmt.Errorf("clear recovered control-plane freeze intent: %w", err)
	}
	return nil
}

func writeControlPlaneFreezeIntent(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, []byte("pcv-control-plane-freeze-v1\n"), 0o600)
}

func clearControlPlaneFreezeIntent(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func controlPlaneFreezeIntentPending(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func wrapControlPlaneThawError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("emergency thaw control-plane mount: %w", err)
}

func wrapControlPlaneCloseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close emergency control-plane mount: %w", err)
}

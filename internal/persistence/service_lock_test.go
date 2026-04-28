package persistence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errVolumeNotMounted = errors.New("test: volume not mounted")

func TestModuleSetLockStateToleratesUnmounted(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mountDir := filepath.Join(root, "mounts", "control-plane")
	if err := os.MkdirAll(mountDir, 0o700); err != nil {
		t.Fatalf("mkdir mount dir: %v", err)
	}

	ctrl := &stubLockableControl{}
	vol := &stubVolumeManager{}
	vol.onDetach = func(context.Context, VolumeHandle) error {
		return errVolumeNotMounted
	}

	mod := &Module{
		control:       ctrl,
		volumes:       vol,
		controlHandle: VolumeHandle{ID: "control-plane", MountDir: mountDir},
	}

	if err := mod.setLockState(context.Background(), true); err != nil {
		t.Fatalf("setLockState should tolerate unmounted volume: %v", err)
	}
}

type stubLockableControl struct {
	locked bool
}

func (s *stubLockableControl) Lock() {
	s.locked = true
}

func (s *stubLockableControl) Unlock(context.Context) error {
	s.locked = false
	return nil
}

func (s *stubLockableControl) Auth() AuthRepo {
	return nil
}

func (s *stubLockableControl) Remote() RemoteRepo {
	return nil
}

func (s *stubLockableControl) AppState() AppStateRepo {
	return nil
}

func (s *stubLockableControl) Users() UserRepo {
	return nil
}

func (s *stubLockableControl) OIDCClients() OIDCClientRepo {
	return nil
}

func (s *stubLockableControl) OIDCKeys() OIDCKeyRepo {
	return nil
}

func (s *stubLockableControl) OIDCAuthCodes() OIDCAuthCodeRepo {
	return nil
}

func (s *stubLockableControl) OIDCRefreshTokens() OIDCRefreshTokenRepo {
	return nil
}

func (s *stubLockableControl) OIDCConfig() OIDCConfigRepo {
	return nil
}

func (s *stubLockableControl) WebAuthnCredentials() WebAuthnCredentialRepo {
	return nil
}

func (s *stubLockableControl) InviteTokens() InviteTokenRepo {
	return nil
}

func (s *stubLockableControl) Close(context.Context) error {
	return nil
}

func (s *stubLockableControl) Revision(context.Context) (uint64, string, error) {
	return 0, "", nil
}

func (s *stubLockableControl) QuickCheck(context.Context) (ControlHealthReport, error) {
	return ControlHealthReport{Status: ControlHealthStatusOK, Message: "ok", CheckedAt: time.Now().UTC()}, nil
}

type stubVolumeManager struct {
	onEnsure          func(context.Context, VolumeRequest) (VolumeHandle, error)
	onAttach          func(context.Context, VolumeHandle, AttachOptions) error
	onDetach          func(context.Context, VolumeHandle) error
	onDestroy         func(context.Context, string) error
	onStream          func(string) (<-chan VolumeRole, error)
	onAttachStateOf   func(context.Context, string) (AttachState, error)
	onIsAttachedAdvis func(context.Context, string) bool
}

func (s *stubVolumeManager) EnsureVolume(ctx context.Context, req VolumeRequest) (VolumeHandle, error) {
	if s.onEnsure != nil {
		return s.onEnsure(ctx, req)
	}
	return VolumeHandle{}, nil
}

func (s *stubVolumeManager) Attach(ctx context.Context, handle VolumeHandle, opts AttachOptions) error {
	if s.onAttach != nil {
		return s.onAttach(ctx, handle, opts)
	}
	return nil
}

func (s *stubVolumeManager) Detach(ctx context.Context, handle VolumeHandle) error {
	if s.onDetach != nil {
		return s.onDetach(ctx, handle)
	}
	return nil
}

func (s *stubVolumeManager) DestroyVolume(ctx context.Context, id string) error {
	if s.onDestroy != nil {
		return s.onDestroy(ctx, id)
	}
	return nil
}

func (s *stubVolumeManager) RoleStream(id string) (<-chan VolumeRole, error) {
	if s.onStream != nil {
		return s.onStream(id)
	}
	ch := make(chan VolumeRole)
	close(ch)
	return ch, nil
}

func (s *stubVolumeManager) AttachStateOf(ctx context.Context, id string) (AttachState, error) {
	if s.onAttachStateOf != nil {
		return s.onAttachStateOf(ctx, id)
	}
	return AttachStateDetached, nil
}

func (s *stubVolumeManager) IsAttachedAdvisory(ctx context.Context, id string) bool {
	if s.onIsAttachedAdvis != nil {
		return s.onIsAttachedAdvis(ctx, id)
	}
	return false
}

func TestSetLockState_idempotent(t *testing.T) {
	t.Parallel()

	var attachCount atomic.Int32
	vol := &stubVolumeManager{}
	vol.onAttach = func(context.Context, VolumeHandle, AttachOptions) error {
		attachCount.Add(1)
		return nil
	}
	ctrl := &stubLockableControl{}

	mod := &Module{
		control:       ctrl,
		volumes:       vol,
		controlHandle: VolumeHandle{ID: "control-plane", MountDir: t.TempDir()},
		lockState:     true,
	}

	// First unlock should attach.
	if err := mod.setLockState(context.Background(), false); err != nil {
		t.Fatalf("first unlock: %v", err)
	}
	if got := attachCount.Load(); got != 1 {
		t.Fatalf("expected 1 attach, got %d", got)
	}

	// Second unlock should be a no-op (idempotent).
	if err := mod.setLockState(context.Background(), false); err != nil {
		t.Fatalf("second unlock: %v", err)
	}
	if got := attachCount.Load(); got != 1 {
		t.Fatalf("expected still 1 attach after idempotent unlock, got %d", got)
	}
}

func TestSetLockState_concurrent(t *testing.T) {
	t.Parallel()

	var attachCount atomic.Int32
	vol := &stubVolumeManager{}
	vol.onAttach = func(context.Context, VolumeHandle, AttachOptions) error {
		attachCount.Add(1)
		// Simulate work to widen the race window.
		time.Sleep(10 * time.Millisecond)
		return nil
	}
	ctrl := &stubLockableControl{}

	mod := &Module{
		control:       ctrl,
		volumes:       vol,
		controlHandle: VolumeHandle{ID: "control-plane", MountDir: t.TempDir()},
		lockState:     true,
	}

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = mod.setLockState(context.Background(), false)
		}()
	}
	wg.Wait()

	if got := attachCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attach from concurrent unlocks, got %d", got)
	}
}

// TestModuleSetLockState_FailsClosedOnAmbiguous pins the contract that the
// lock-teardown path refuses to flip lockState=true when the kernel-state
// probe is ambiguous (Foreign mount, Unknown without err, Corrupted without
// err). Without this end-to-end coverage, a future regression in the typed
// detach wrappers — or in the way setLockState consumes them — could re-open
// the "locked but mapper still open" failure mode.
func TestModuleSetLockState_FailsClosedOnAmbiguous(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		state    AttachState
		stateErr error
	}{
		{"Foreign", AttachStateForeignMountAtPath, nil},
		{"Unknown_no_err", AttachStateUnknown, nil},
		{"Corrupted_no_err", AttachStateKernelStateCorrupted, nil},
		{"Unknown_with_err", AttachStateUnknown, ErrKernelStateAmbiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var detachCalled atomic.Int32
			vol := &stubVolumeManager{}
			vol.onAttachStateOf = func(context.Context, string) (AttachState, error) {
				return tc.state, tc.stateErr
			}
			vol.onDetach = func(context.Context, VolumeHandle) error {
				detachCalled.Add(1)
				return nil
			}
			ctrl := &stubLockableControl{}

			mod := &Module{
				control:       ctrl,
				volumes:       vol,
				controlHandle: VolumeHandle{ID: "control-plane", MountDir: t.TempDir()},
				lockState:     false,
			}

			err := mod.setLockState(context.Background(), true)
			if err == nil {
				t.Fatalf("setLockState(true) must fail closed on %s, got nil", tc.name)
			}
			if mod.lockState {
				t.Fatalf("setLockState(true) returned error but flipped lockState=true; the system is now lying about being locked")
			}
			if detachCalled.Load() != 0 {
				t.Fatalf("Detach must not be called when probe is ambiguous; got %d calls", detachCalled.Load())
			}
		})
	}
}

// TestModuleSetLockState_RejectsZeroHandle pins codex iter-7 B1: at
// startup, NewService calls setLockState(true) BEFORE ensureCoreVolumes
// populates the controlHandle. Pre-fix, the zero VolumeHandle classified
// as NoOp and the strict wrapper returned nil — leaving lockState=true
// while a stale post-crash control mapper could survive.
//
// Two layers of defense are now in place:
//   (a) detachVolumeStrict refuses zero/incomplete handles up-front
//       (this test) — structural close for the regression class.
//   (b) NewService seeds the canonical handle before the first
//       setLockState — keeps the strict path from ever firing with a
//       zero handle in production.
//
// detachVolumeBestEffort retains the NoOp-on-zero-handle behavior so
// shutdown paths don't wedge on a transient programmer error.
func TestModuleSetLockState_RejectsZeroHandle(t *testing.T) {
	t.Parallel()

	vol := &stubVolumeManager{}
	mod := &Module{volumes: vol}
	// Zero handle — strict wrapper now refuses, fail-closed.
	if err := mod.detachVolumeStrict(context.Background(), VolumeHandle{}); err == nil {
		t.Fatalf("zero-handle strict detach must error (was silent NoOp pre-fix); got nil")
	}
	// Best-effort wrapper still NoOps on zero-handle so shutdown paths
	// don't wedge on a partial init.
	if err := mod.detachVolumeBestEffort(context.Background(), VolumeHandle{}); err != nil {
		t.Fatalf("zero-handle best-effort detach should return nil (NoOp class), got %v", err)
	}
	// Canonical handle (the post-fix shape) — would actually probe.
	canonical := VolumeHandle{ID: "control-plane", MountDir: "/mounts/control-plane"}
	vol.onAttachStateOf = func(context.Context, string) (AttachState, error) {
		return AttachStateForeignMountAtPath, nil
	}
	if err := mod.detachVolumeStrict(context.Background(), canonical); err == nil {
		t.Fatalf("canonical handle with Foreign probe must fail closed; got nil")
	}
}

// TestModuleSetLockState_DetachesAttached is the positive counterpart: when
// the probe says the volume is genuinely attached, setLockState(true) MUST
// invoke Detach and flip lockState=true.
func TestModuleSetLockState_DetachesAttached(t *testing.T) {
	t.Parallel()

	var detachCalled atomic.Int32
	vol := &stubVolumeManager{}
	vol.onAttachStateOf = func(context.Context, string) (AttachState, error) {
		return AttachStateAttached, nil
	}
	vol.onDetach = func(context.Context, VolumeHandle) error {
		detachCalled.Add(1)
		return nil
	}
	ctrl := &stubLockableControl{}

	mod := &Module{
		control:       ctrl,
		volumes:       vol,
		controlHandle: VolumeHandle{ID: "control-plane", MountDir: t.TempDir()},
		lockState:     false,
	}

	if err := mod.setLockState(context.Background(), true); err != nil {
		t.Fatalf("setLockState(true) on Attached: %v", err)
	}
	if !mod.lockState {
		t.Fatalf("setLockState(true) succeeded but lockState is false")
	}
	if got := detachCalled.Load(); got != 1 {
		t.Fatalf("expected 1 Detach call on Attached state, got %d", got)
	}
	if !ctrl.locked {
		t.Fatalf("expected control store to be locked")
	}
}

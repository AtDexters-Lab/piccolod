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
	onEnsure  func(context.Context, VolumeRequest) (VolumeHandle, error)
	onAttach  func(context.Context, VolumeHandle, AttachOptions) error
	onDetach  func(context.Context, VolumeHandle) error
	onDestroy func(context.Context, string) error
	onStream  func(string) (<-chan VolumeRole, error)
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

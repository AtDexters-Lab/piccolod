package persistence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"piccolod/internal/state/paths"
)

// --- Policy-API: detachVolumeStrict / detachVolumeBestEffort ---

// stubDetachVolumeManager is a minimal VolumeManager for the typed
// detach-wrapper tests. Records Detach calls and lets a test drive
// AttachStateOf's return.
type stubDetachVolumeManager struct {
	stubVolumeManager
	state       AttachState
	stateErr    error
	detachCalls int
}

func (s *stubDetachVolumeManager) AttachStateOf(_ context.Context, _ string) (AttachState, error) {
	return s.state, s.stateErr
}

func (s *stubDetachVolumeManager) IsAttachedAdvisory(_ context.Context, _ string) bool {
	return s.state == AttachStateAttached && s.stateErr == nil
}

func (s *stubDetachVolumeManager) Detach(_ context.Context, _ VolumeHandle) error {
	s.detachCalls++
	return nil
}

func newDetachStub(state AttachState, err error) *stubDetachVolumeManager {
	return &stubDetachVolumeManager{state: state, stateErr: err}
}

func TestDetachVolumeStrict_FailsClosedOnAmbiguousProbe(t *testing.T) {
	cases := []struct {
		name      string
		state     AttachState
		stateErr  error
		wantErr   bool
		wantCalls int
	}{
		{"Detached_no_op", AttachStateDetached, nil, false, 0},
		{"Attached_detaches", AttachStateAttached, nil, false, 1},
		{"Partial_detaches", AttachStatePartialMapperOnly, nil, false, 1},
		{"Stale_detaches", AttachStateStaleMountRecord, nil, false, 1},
		{"Foreign_fails_closed", AttachStateForeignMountAtPath, nil, true, 0},
		{"Unknown_with_err_fails_closed", AttachStateUnknown, ErrKernelStateAmbiguous, true, 0},
		{"Corrupted_with_err_fails_closed", AttachStateKernelStateCorrupted, ErrKernelStateCorrupted, true, 0},
		// The probe's I/O-failure retry path returns (Unknown, nil); the
		// pre-typed-enum implementation classified that as Skip and
		// silently fell through. Pin the post-fix contract: these must
		// fail closed regardless of whether the probe attached an error.
		{"Unknown_no_err_fails_closed", AttachStateUnknown, nil, true, 0},
		{"Corrupted_no_err_fails_closed", AttachStateKernelStateCorrupted, nil, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vol := newDetachStub(tc.state, tc.stateErr)
			mod := &Module{volumes: vol}
			handle := VolumeHandle{ID: "test", MountDir: "/mnt/test"}
			err := mod.detachVolumeStrict(context.Background(), handle)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if vol.detachCalls != tc.wantCalls {
				t.Fatalf("Detach calls: got %d, want %d", vol.detachCalls, tc.wantCalls)
			}
		})
	}
}

func TestDetachVolumeBestEffort_NeverErrors(t *testing.T) {
	cases := []struct {
		name      string
		state     AttachState
		stateErr  error
		wantCalls int
	}{
		{"Foreign_logs_skips", AttachStateForeignMountAtPath, nil, 0},
		{"Unknown_logs_skips", AttachStateUnknown, ErrKernelStateAmbiguous, 0},
		{"Attached_detaches", AttachStateAttached, nil, 1},
		{"Detached_no_op", AttachStateDetached, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vol := newDetachStub(tc.state, tc.stateErr)
			mod := &Module{volumes: vol}
			handle := VolumeHandle{ID: "test", MountDir: "/mnt/test"}
			if err := mod.detachVolumeBestEffort(context.Background(), handle); err != nil {
				t.Fatalf("BestEffort returned error: %v", err)
			}
			if vol.detachCalls != tc.wantCalls {
				t.Fatalf("Detach calls: got %d, want %d", vol.detachCalls, tc.wantCalls)
			}
		})
	}
}

func TestDetachVolumeStrict_PropagatesProbeError(t *testing.T) {
	probeErr := errors.New("synthetic probe failure")
	vol := newDetachStub(AttachStateUnknown, probeErr)
	mod := &Module{volumes: vol}
	handle := VolumeHandle{ID: "test", MountDir: "/mnt/test"}
	err := mod.detachVolumeStrict(context.Background(), handle)
	if err == nil {
		t.Fatalf("expected probe error to propagate")
	}
	if !errors.Is(err, probeErr) {
		t.Fatalf("error chain missing original: %v", err)
	}
}

func TestModuleEnsureCoreVolumesIgnoresReconcileErrors(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")

	core, _ := paths.SetRootsForTest(t)

	mgr := &luksVolumeManager{}

	// Seed a corrupted volume metadata file so ReconcileAllVolumeStates logs
	// a warning. ensureCoreVolumes should still continue and bring up the
	// control-plane volume handle so piccolod can start.
	badDir := filepath.Join(core, "volumes", "app-code-server")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatalf("mkdir bad volume dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badDir, metadataV2File), []byte("{"), 0o600); err != nil {
		t.Fatalf("write bad volume metadata: %v", err)
	}

	mod := &Module{volumes: mgr}
	if err := mod.ensureCoreVolumes(context.Background()); err != nil {
		t.Fatalf("ensureCoreVolumes: %v", err)
	}
	if mod.controlHandle.ID != "control-plane" {
		t.Fatalf("expected control-plane volume handle, got %q", mod.controlHandle.ID)
	}
}

package server

import (
	"context"
	"errors"
	"testing"

	"piccolod/internal/autounlock"
	"piccolod/internal/update"
)

type fakeOSUpdateMgr struct {
	statusResp     update.Status
	statusErr      error
	snapshotResp   update.SnapshotState
	snapshotErr    error
	rollbackErr    error
	rollbacked     int
	rollbackTarget string
	rebootErr      error
	rebooted       int
}

func (f *fakeOSUpdateMgr) Status(ctx context.Context) (update.Status, error) {
	return f.statusResp, f.statusErr
}
func (f *fakeOSUpdateMgr) SnapshotState(ctx context.Context) (update.SnapshotState, error) {
	return f.snapshotResp, f.snapshotErr
}
func (f *fakeOSUpdateMgr) Apply(ctx context.Context) error { return nil }
func (f *fakeOSUpdateMgr) Rollback(ctx context.Context, target string) error {
	f.rollbacked++
	f.rollbackTarget = target
	return f.rollbackErr
}
func (f *fakeOSUpdateMgr) Reboot(ctx context.Context) error {
	f.rebooted++
	return f.rebootErr
}
func (f *fakeOSUpdateMgr) ForceReboot(ctx context.Context) error { return nil }
func (f *fakeOSUpdateMgr) PowerOff(ctx context.Context) error    { return nil }
func (f *fakeOSUpdateMgr) Watch(ctx context.Context) error       { return nil }

func TestOSUpdateAdapter_UpdateReadiness_NilInner(t *testing.T) {
	a := &osUpdateManagerAdapter{inner: func() osUpdateManager { return nil }}
	got, err := a.UpdateReadiness(context.Background())
	if !errors.Is(err, errUpdateManagerUnavailable) {
		t.Fatalf("nil inner should return errUpdateManagerUnavailable, got %v", err)
	}
	if got != autounlock.UpdateReadinessUnknown {
		t.Errorf("nil inner readiness = %q, want unknown", got)
	}
}

func TestOSUpdateAdapter_UpdateReadiness_SnapshotError(t *testing.T) {
	backendErr := errors.New("backend down")
	m := &fakeOSUpdateMgr{snapshotErr: backendErr}
	a := &osUpdateManagerAdapter{inner: func() osUpdateManager { return m }}
	got, err := a.UpdateReadiness(context.Background())
	if !errors.Is(err, backendErr) {
		t.Fatalf("snapshot error should propagate, got %v", err)
	}
	if got != autounlock.UpdateReadinessUnknown {
		t.Errorf("snapshot error readiness = %q, want unknown", got)
	}
}

func TestOSUpdateAdapter_UpdateReadiness_Staged(t *testing.T) {
	m := &fakeOSUpdateMgr{snapshotResp: update.SnapshotState{Readiness: update.SnapshotReadinessStaged}}
	a := &osUpdateManagerAdapter{inner: func() osUpdateManager { return m }}
	got, err := a.UpdateReadiness(context.Background())
	if err != nil {
		t.Fatalf("UpdateReadiness: %v", err)
	}
	if got != autounlock.UpdateReadinessStaged {
		t.Errorf("staged readiness = %q", got)
	}
}

func TestOSUpdateAdapter_UpdateReadiness_MapsStates(t *testing.T) {
	cases := []struct {
		name string
		in   update.SnapshotReadiness
		want autounlock.UpdateReadiness
	}{
		{"absent", update.SnapshotReadinessAbsent, autounlock.UpdateReadinessAbsent},
		{"in_progress", update.SnapshotReadinessInProgress, autounlock.UpdateReadinessInProgress},
		{"unknown", update.SnapshotReadinessUnknown, autounlock.UpdateReadinessUnknown},
		{"empty", "", autounlock.UpdateReadinessUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &fakeOSUpdateMgr{snapshotResp: update.SnapshotState{Readiness: tc.in}}
			a := &osUpdateManagerAdapter{inner: func() osUpdateManager { return m }}
			got, err := a.UpdateReadiness(context.Background())
			if err != nil {
				t.Fatalf("UpdateReadiness: %v", err)
			}
			if got != tc.want {
				t.Errorf("readiness = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOSUpdateAdapter_Reboot_NilInner_ReturnsError(t *testing.T) {
	// Fail-loud: a nil-inner Reboot must NOT return nil. The scheduler reads
	// Reboot's nil/non-nil to drive the failed-fire audit; nil-as-success
	// would silently flip last_fired_at + alreadyTriedThisWindow without a
	// real reboot, suppressing every subsequent fire until tEdge rollover.
	a := &osUpdateManagerAdapter{inner: func() osUpdateManager { return nil }}
	if err := a.Reboot(context.Background()); !errors.Is(err, errUpdateManagerUnavailable) {
		t.Errorf("nil inner should return errUpdateManagerUnavailable, got %v", err)
	}
}

func TestOSUpdateAdapter_Reboot_DelegatesToInner(t *testing.T) {
	m := &fakeOSUpdateMgr{}
	a := &osUpdateManagerAdapter{inner: func() osUpdateManager { return m }}
	if err := a.Reboot(context.Background()); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if m.rebooted != 1 {
		t.Errorf("expected 1 Reboot call, got %d", m.rebooted)
	}
}

func TestOSUpdateAdapter_Reboot_PropagatesError(t *testing.T) {
	m := &fakeOSUpdateMgr{rebootErr: errors.New("systemd refused")}
	a := &osUpdateManagerAdapter{inner: func() osUpdateManager { return m }}
	err := a.Reboot(context.Background())
	if err == nil {
		t.Errorf("expected error to propagate from inner.Reboot")
	}
}

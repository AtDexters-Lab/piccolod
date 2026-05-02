package server

import (
	"context"
	"errors"
	"testing"

	"piccolod/internal/update"
)

type fakeOSUpdateMgr struct {
	statusResp update.Status
	statusErr  error
	rebootErr  error
	rebooted   int
}

func (f *fakeOSUpdateMgr) Status(ctx context.Context) (update.Status, error) {
	return f.statusResp, f.statusErr
}
func (f *fakeOSUpdateMgr) Apply(ctx context.Context) error            { return nil }
func (f *fakeOSUpdateMgr) Rollback(ctx context.Context, _ string) error { return nil }
func (f *fakeOSUpdateMgr) Reboot(ctx context.Context) error {
	f.rebooted++
	return f.rebootErr
}
func (f *fakeOSUpdateMgr) ForceReboot(ctx context.Context) error { return nil }
func (f *fakeOSUpdateMgr) PowerOff(ctx context.Context) error    { return nil }
func (f *fakeOSUpdateMgr) Watch(ctx context.Context) error       { return nil }

func TestOSUpdateAdapter_HasStagedUpdate_NilInner(t *testing.T) {
	a := &osUpdateManagerAdapter{inner: func() osUpdateManager { return nil }}
	if a.HasStagedUpdate(context.Background()) {
		t.Errorf("nil inner should return false")
	}
}

func TestOSUpdateAdapter_HasStagedUpdate_StatusError(t *testing.T) {
	m := &fakeOSUpdateMgr{statusErr: errors.New("backend down")}
	a := &osUpdateManagerAdapter{inner: func() osUpdateManager { return m }}
	if a.HasStagedUpdate(context.Background()) {
		t.Errorf("Status error should return false (fail-closed)")
	}
}

func TestOSUpdateAdapter_HasStagedUpdate_PendingReboot(t *testing.T) {
	m := &fakeOSUpdateMgr{statusResp: update.Status{RequiresReboot: true}}
	a := &osUpdateManagerAdapter{inner: func() osUpdateManager { return m }}
	if !a.HasStagedUpdate(context.Background()) {
		t.Errorf("RequiresReboot=true should propagate")
	}
}

func TestOSUpdateAdapter_HasStagedUpdate_NoReboot(t *testing.T) {
	m := &fakeOSUpdateMgr{statusResp: update.Status{RequiresReboot: false}}
	a := &osUpdateManagerAdapter{inner: func() osUpdateManager { return m }}
	if a.HasStagedUpdate(context.Background()) {
		t.Errorf("RequiresReboot=false should return false")
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

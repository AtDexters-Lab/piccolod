package autounlock

import (
	"context"
	"testing"
)

func ptrBool(v bool) *bool { return &v }
func ptrInt(v int) *int    { return &v }

func TestUpdate_EnableFromDisabled(t *testing.T) {
	o, _, _, audits := newOrchestrator(t)
	// Start from explicit disabled (the post-default-flip "user opted out"
	// state) so we can exercise the disabled→enabled transition.
	if err := SaveState(State{Enabled: false}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := o.Update(context.Background(), UpdateInput{Enabled: ptrBool(true)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := LoadState()
	if !got.Enabled {
		t.Errorf("expected enabled=true after enable")
	}
	// First disabled→enabled seeds default auto_reboot bounds.
	if got.AutoReboot.WindowStartHour != 3 || got.AutoReboot.WindowEndHour != 5 {
		t.Errorf("auto_reboot defaults not seeded: %+v", got.AutoReboot)
	}
	if len(*audits) != 1 || (*audits)[0].kind != AuditEnabledChanged {
		t.Errorf("expected enabled.changed audit; got %v", *audits)
	}
}

func TestUpdate_DisableTriggersCleanup(t *testing.T) {
	o, _, nc, audits := newOrchestrator(t)
	// Set up an enabled state with an outstanding blob.
	if err := SaveState(State{Enabled: true, AutoReboot: DefaultAutoReboot()}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := WriteBlob([]byte("opaque")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if err := o.Update(context.Background(), UpdateInput{Enabled: ptrBool(false)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if BlobExists() {
		t.Errorf("blob retained after disable")
	}
	if nc.revokeCount != 0 {
		t.Errorf("legacy revoke called on disable (count=%d)", nc.revokeCount)
	}
	got, _ := LoadState()
	if got.Enabled {
		t.Errorf("expected enabled=false after disable")
	}
	// Two audits: cycle.revoked (cleanup) + enabled.changed.
	kinds := map[string]bool{}
	for _, a := range *audits {
		kinds[a.kind] = true
	}
	if !kinds[AuditCycleRevoked] || !kinds[AuditEnabledChanged] {
		t.Errorf("expected revoked + enabled.changed; got %v", *audits)
	}
}

func TestUpdate_DisableWhenAlreadyDisabled_NoCleanup(t *testing.T) {
	o, _, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Enabled: false}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := o.Update(context.Background(), UpdateInput{Enabled: ptrBool(false)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if nc.revokeCount != 0 {
		t.Errorf("revoke called when already disabled (count=%d)", nc.revokeCount)
	}
	if len(*audits) != 0 {
		t.Errorf("audit emitted on no-op update; got %v", *audits)
	}
}

func TestUpdate_AutoRebootPartial(t *testing.T) {
	o, _, _, _ := newOrchestrator(t)
	if err := SaveState(State{Enabled: true, AutoReboot: DefaultAutoReboot()}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	// Toggle just auto_reboot.enabled.
	if err := o.Update(context.Background(), UpdateInput{
		AutoReboot: &AutoRebootUpdate{Enabled: ptrBool(true)},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := LoadState()
	if !got.AutoReboot.Enabled {
		t.Errorf("auto_reboot.enabled not toggled")
	}
	if got.AutoReboot.WindowStartHour != 3 {
		t.Errorf("partial update clobbered window: %+v", got.AutoReboot)
	}
}

func TestUpdate_InvalidWindow_RejectedAtomically(t *testing.T) {
	o, _, _, _ := newOrchestrator(t)
	if err := SaveState(State{Enabled: true, AutoReboot: DefaultAutoReboot()}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	cases := []struct {
		name  string
		start *int
		end   *int
	}{
		{"start out of range", ptrInt(24), nil},
		{"end out of range", nil, ptrInt(-1)},
		{"start equals end", ptrInt(5), ptrInt(5)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := o.Update(context.Background(), UpdateInput{
				AutoReboot: &AutoRebootUpdate{
					WindowStartHour: tc.start,
					WindowEndHour:   tc.end,
				},
			})
			if err != ErrInvalidWindow {
				t.Errorf("expected ErrInvalidWindow; got %v", err)
			}
		})
	}
	// State unchanged after rejected updates.
	got, _ := LoadState()
	if got.AutoReboot.WindowStartHour != 3 || got.AutoReboot.WindowEndHour != 5 {
		t.Errorf("rejected update mutated state: %+v", got.AutoReboot)
	}
}

func TestUpdate_NoOpReturnsCurrentState(t *testing.T) {
	o, _, _, audits := newOrchestrator(t)
	// Empty UpdateInput — neither enabled nor auto_reboot supplied.
	if err := o.Update(context.Background(), UpdateInput{}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(*audits) != 0 {
		t.Errorf("no-op should not emit audit; got %v", *audits)
	}
}

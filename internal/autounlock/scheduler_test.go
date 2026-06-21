package autounlock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"piccolod/internal/state/paths"
)

type fakeUpdateManager struct {
	mu           sync.Mutex
	readiness    UpdateReadiness
	readinessErr error
	rebootErr    error
	rebootCount  int
}

func (f *fakeUpdateManager) UpdateReadiness(ctx context.Context) (UpdateReadiness, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readiness == "" {
		return UpdateReadinessAbsent, f.readinessErr
	}
	return f.readiness, f.readinessErr
}
func (f *fakeUpdateManager) Reboot(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebootCount++
	return f.rebootErr
}

type blockingReadinessManager struct {
	rebootCount int
}

func (b *blockingReadinessManager) UpdateReadiness(ctx context.Context) (UpdateReadiness, error) {
	<-ctx.Done()
	return UpdateReadinessUnknown, ctx.Err()
}

func (b *blockingReadinessManager) Reboot(ctx context.Context) error {
	b.rebootCount++
	return nil
}

func newScheduler(t *testing.T, now time.Time) (*Scheduler, *fakeUpdateManager, *[]capturedAudit) {
	t.Helper()
	paths.SetCoreRootForTest(t, t.TempDir())
	um := &fakeUpdateManager{}
	audits := &[]capturedAudit{}
	mu := sync.Mutex{}
	s := NewScheduler(SchedulerDeps{
		UpdateManager: um,
		PublishAudit: func(kind string, details map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			*audits = append(*audits, capturedAudit{kind, details})
		},
		Now:          func() time.Time { return now },
		TZSafeToFire: func() bool { return true },
	})
	return s, um, audits
}

// --- inWindow ---

func TestInWindow_NonWrap(t *testing.T) {
	cases := []struct {
		hour, start, end int
		want             bool
	}{
		{2, 3, 5, false},
		{3, 3, 5, true},
		{4, 3, 5, true},
		{5, 3, 5, false},
		{6, 3, 5, false},
	}
	for _, tc := range cases {
		got := inWindow(tc.hour, tc.start, tc.end)
		if got != tc.want {
			t.Errorf("inWindow(%d, %d, %d) = %v; want %v", tc.hour, tc.start, tc.end, got, tc.want)
		}
	}
}

func TestInWindow_Wrap(t *testing.T) {
	// Window 23 → 1 means [23..23] ∪ [0..1) = 23, 0.
	cases := []struct {
		hour int
		want bool
	}{
		{22, false},
		{23, true},
		{0, true},
		{1, false},
		{2, false},
	}
	for _, tc := range cases {
		got := inWindow(tc.hour, 23, 1)
		if got != tc.want {
			t.Errorf("inWindow(%d, 23, 1) = %v; want %v", tc.hour, got, tc.want)
		}
	}
}

// --- tEdge ---

func TestTEdge_NonWrap_BeforeWindow(t *testing.T) {
	// 02:00, window 3-5 → most recent edge is yesterday's 03:00.
	now := time.Date(2026, 5, 2, 2, 0, 0, 0, time.UTC)
	edge := tEdge(now, 3, 5)
	want := time.Date(2026, 5, 1, 3, 0, 0, 0, time.UTC)
	if !edge.Equal(want) {
		t.Errorf("tEdge = %v; want %v", edge, want)
	}
}

func TestTEdge_NonWrap_InWindow(t *testing.T) {
	// 04:00, window 3-5 → today's 03:00.
	now := time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC)
	edge := tEdge(now, 3, 5)
	want := time.Date(2026, 5, 2, 3, 0, 0, 0, time.UTC)
	if !edge.Equal(want) {
		t.Errorf("tEdge = %v; want %v", edge, want)
	}
}

func TestTEdge_NonWrap_AfterWindow(t *testing.T) {
	// 14:00, window 3-5 → today's 03:00 (still the most recent edge).
	now := time.Date(2026, 5, 2, 14, 0, 0, 0, time.UTC)
	edge := tEdge(now, 3, 5)
	want := time.Date(2026, 5, 2, 3, 0, 0, 0, time.UTC)
	if !edge.Equal(want) {
		t.Errorf("tEdge = %v; want %v", edge, want)
	}
}

func TestTEdge_Wrap_PostMidnight(t *testing.T) {
	// 00:30, window 23-1 → yesterday's 23:00.
	now := time.Date(2026, 5, 2, 0, 30, 0, 0, time.UTC)
	edge := tEdge(now, 23, 1)
	want := time.Date(2026, 5, 1, 23, 0, 0, 0, time.UTC)
	if !edge.Equal(want) {
		t.Errorf("tEdge = %v; want %v", edge, want)
	}
}

func TestTEdge_Wrap_PreMidnight(t *testing.T) {
	// 23:30, window 23-1 → today's 23:00.
	now := time.Date(2026, 5, 2, 23, 30, 0, 0, time.UTC)
	edge := tEdge(now, 23, 1)
	want := time.Date(2026, 5, 2, 23, 0, 0, 0, time.UTC)
	if !edge.Equal(want) {
		t.Errorf("tEdge = %v; want %v", edge, want)
	}
}

// --- Scheduler tick ---

func TestTick_DisabledNoOp(t *testing.T) {
	s, um, _ := newScheduler(t, time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC))
	// state.Enabled = false (default)
	s.tick(context.Background())
	if um.rebootCount != 0 {
		t.Errorf("reboot called when disabled")
	}
}

func TestTick_AutoRebootOffNoOp(t *testing.T) {
	s, um, _ := newScheduler(t, time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC))
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: false, WindowStartHour: 3, WindowEndHour: 5}})
	s.tick(context.Background())
	if um.rebootCount != 0 {
		t.Errorf("reboot called when auto_reboot disabled")
	}
}

func TestTick_OutOfWindowNoOp(t *testing.T) {
	s, um, _ := newScheduler(t, time.Date(2026, 5, 2, 14, 0, 0, 0, time.UTC))
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})
	um.readiness = UpdateReadinessStaged
	s.tick(context.Background())
	if um.rebootCount != 0 {
		t.Errorf("reboot called outside window")
	}
}

func TestTick_NoStagedUpdateNoOp(t *testing.T) {
	s, um, _ := newScheduler(t, time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC))
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})
	um.readiness = UpdateReadinessAbsent
	s.tick(context.Background())
	if um.rebootCount != 0 {
		t.Errorf("reboot called with no staged update")
	}
	got, _ := LoadState()
	if got.AutoReboot.LastFiredAt != nil {
		t.Errorf("last_fired_at should not persist for absent update")
	}
}

func TestTick_InProgressNoOp(t *testing.T) {
	s, um, _ := newScheduler(t, time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC))
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})
	um.readiness = UpdateReadinessInProgress
	s.tick(context.Background())
	if um.rebootCount != 0 {
		t.Errorf("reboot called while update in progress")
	}
	got, _ := LoadState()
	if got.AutoReboot.LastFiredAt != nil {
		t.Errorf("last_fired_at should not persist while update is in progress")
	}
}

func TestTick_UnknownNoOp(t *testing.T) {
	s, um, _ := newScheduler(t, time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC))
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})
	um.readiness = UpdateReadinessUnknown
	um.readinessErr = errors.New("snapshot probe failed")
	s.tick(context.Background())
	if um.rebootCount != 0 {
		t.Errorf("reboot called with unknown readiness")
	}
	got, _ := LoadState()
	if got.AutoReboot.LastFiredAt != nil {
		t.Errorf("last_fired_at should not persist for unknown readiness")
	}
}

func TestTick_UpdateReadinessTimeoutNoFire(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	now := time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC)
	um := &blockingReadinessManager{}
	s := NewScheduler(SchedulerDeps{
		UpdateManager:          um,
		UpdateReadinessTimeout: 10 * time.Millisecond,
		Now:                    func() time.Time { return now },
		TZSafeToFire:           func() bool { return true },
	})
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})

	start := time.Now()
	s.tick(context.Background())
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("tick blocked for %s despite readiness timeout", elapsed)
	}
	if um.rebootCount != 0 {
		t.Fatalf("reboot called after readiness timeout")
	}
	got, _ := LoadState()
	if got.AutoReboot.LastFiredAt != nil {
		t.Fatalf("last_fired_at should not persist after readiness timeout")
	}
}

func TestTick_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC)
	s, um, audits := newScheduler(t, now)
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})
	um.readiness = UpdateReadinessStaged
	s.tick(context.Background())
	if um.rebootCount != 1 {
		t.Errorf("reboot count = %d; want 1", um.rebootCount)
	}
	if len(*audits) != 1 || (*audits)[0].kind != AuditSchedulerFired {
		t.Errorf("expected scheduler.fired audit; got %v", *audits)
	}
	// State should reflect last_fired_at.
	got, _ := LoadState()
	if got.AutoReboot.LastFiredAt == nil {
		t.Errorf("last_fired_at not persisted")
	}
}

func TestTick_AlreadyTriedThisWindow(t *testing.T) {
	now := time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC)
	s, um, _ := newScheduler(t, now)
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})
	um.readiness = UpdateReadinessStaged
	s.tick(context.Background())
	s.tick(context.Background())
	if um.rebootCount != 1 {
		t.Errorf("reboot count = %d; want 1 (second tick should skip)", um.rebootCount)
	}
}

func TestTick_RebootError_AuditsAndPersists(t *testing.T) {
	now := time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC)
	s, um, audits := newScheduler(t, now)
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})
	um.readiness = UpdateReadinessStaged
	um.rebootErr = errors.New("reboot fail")
	s.tick(context.Background())
	got, _ := LoadState()
	if got.AutoReboot.LastFailedAt == nil {
		t.Errorf("last_failed_at not persisted on Reboot error")
	}
	kinds := map[string]bool{}
	for _, a := range *audits {
		kinds[a.kind] = true
	}
	if !kinds[AuditSchedulerFired] || !kinds[AuditSchedulerFailed] {
		t.Errorf("expected scheduler.fired + scheduler.failed; got %v", *audits)
	}
}

func TestTick_WindowRolloverResetsFlag(t *testing.T) {
	day1Hour4 := time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC)
	s, um, _ := newScheduler(t, day1Hour4)
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})
	um.readiness = UpdateReadinessStaged
	s.tick(context.Background())
	if um.rebootCount != 1 {
		t.Errorf("first fire failed: %d", um.rebootCount)
	}
	// Advance Now to next day, in window again.
	day2Hour4 := day1Hour4.Add(24 * time.Hour)
	s.deps.Now = func() time.Time { return day2Hour4 }
	um.readiness = UpdateReadinessStaged // staged again
	s.tick(context.Background())
	if um.rebootCount != 2 {
		t.Errorf("second-day fire failed: %d", um.rebootCount)
	}
}

func TestRehydrate_LastFiredInCurrentWindow(t *testing.T) {
	now := time.Date(2026, 5, 2, 4, 30, 0, 0, time.UTC)
	s, um, _ := newScheduler(t, now)
	priorFire := time.Date(2026, 5, 2, 3, 30, 0, 0, time.UTC)
	SaveState(State{
		Enabled: true,
		AutoReboot: AutoReboot{
			Enabled:         true,
			WindowStartHour: 3,
			WindowEndHour:   5,
			LastFiredAt:     &priorFire,
		},
	})
	um.readiness = UpdateReadinessStaged
	s.rehydrate()
	s.tick(context.Background())
	if um.rebootCount != 0 {
		t.Errorf("rehydrate failed to mark already-tried; reboot count = %d", um.rebootCount)
	}
}

func TestTick_TZUnsetGatesFire(t *testing.T) {
	now := time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC)
	paths.SetCoreRootForTest(t, t.TempDir())
	um := &fakeUpdateManager{}
	s := NewScheduler(SchedulerDeps{
		UpdateManager: um,
		Now:           func() time.Time { return now },
		TZSafeToFire:  func() bool { return false },
	})
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})
	um.readiness = UpdateReadinessStaged
	s.tick(context.Background())
	if um.rebootCount != 0 {
		t.Errorf("TZ-unset gate failed to block fire; reboot count = %d", um.rebootCount)
	}
}

func TestMarkWindowEdit_BlocksFire(t *testing.T) {
	now := time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC)
	s, um, _ := newScheduler(t, now)
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})
	um.readiness = UpdateReadinessStaged
	s.MarkWindowEdit()
	s.tick(context.Background())
	if um.rebootCount != 0 {
		t.Errorf("MarkWindowEdit failed to block fire; reboot count = %d", um.rebootCount)
	}
}

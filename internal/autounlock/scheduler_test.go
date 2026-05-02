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
	mu          sync.Mutex
	hasStaged   bool
	rebootErr   error
	rebootCount int
}

func (f *fakeUpdateManager) HasStagedUpdate(ctx context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hasStaged
}
func (f *fakeUpdateManager) Reboot(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebootCount++
	return f.rebootErr
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
		Now: func() time.Time { return now },
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
	um.hasStaged = true
	s.tick(context.Background())
	if um.rebootCount != 0 {
		t.Errorf("reboot called outside window")
	}
}

func TestTick_NoStagedUpdateNoOp(t *testing.T) {
	s, um, _ := newScheduler(t, time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC))
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})
	um.hasStaged = false
	s.tick(context.Background())
	if um.rebootCount != 0 {
		t.Errorf("reboot called with no staged update")
	}
}

func TestTick_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC)
	s, um, audits := newScheduler(t, now)
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})
	um.hasStaged = true
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
	um.hasStaged = true
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
	um.hasStaged = true
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
	um.hasStaged = true
	s.tick(context.Background())
	if um.rebootCount != 1 {
		t.Errorf("first fire failed: %d", um.rebootCount)
	}
	// Advance Now to next day, in window again.
	day2Hour4 := day1Hour4.Add(24 * time.Hour)
	s.deps.Now = func() time.Time { return day2Hour4 }
	um.hasStaged = true // staged again
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
	um.hasStaged = true
	s.rehydrate()
	s.tick(context.Background())
	if um.rebootCount != 0 {
		t.Errorf("rehydrate failed to mark already-tried; reboot count = %d", um.rebootCount)
	}
}

func TestMarkWindowEdit_BlocksFire(t *testing.T) {
	now := time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC)
	s, um, _ := newScheduler(t, now)
	SaveState(State{Enabled: true, AutoReboot: AutoReboot{Enabled: true, WindowStartHour: 3, WindowEndHour: 5}})
	um.hasStaged = true
	s.MarkWindowEdit()
	s.tick(context.Background())
	if um.rebootCount != 0 {
		t.Errorf("MarkWindowEdit failed to block fire; reboot count = %d", um.rebootCount)
	}
}

package nmagent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// newTestAgent builds an Agent suitable for testing GetSecrets / cache logic
// without opening a real D-Bus connection. The bus-related fields stay nil
// because the methods under test do not touch them.
func newTestAgent(t *testing.T, registered bool) *Agent {
	t.Helper()
	a := &Agent{
		cache: make(map[string]string),
	}
	if registered {
		a.state.Store(int32(stateRegistered))
	} else {
		a.state.Store(int32(stateLost))
	}
	return a
}

func wifiSettingsWithSSID(ssid string) map[string]map[string]dbus.Variant {
	return map[string]map[string]dbus.Variant{
		wifiSetting: {
			"ssid": dbus.MakeVariant([]byte(ssid)),
		},
		"connection": {
			"id":   dbus.MakeVariant(ssid),
			"type": dbus.MakeVariant("802-11-wireless"),
		},
	}
}

// extractPSK pulls the psk string from a GetSecrets result. Returns "" on
// missing/wrong-typed values — same nil-tolerance as the production accessor.
func extractPSK(t *testing.T, resp map[string]map[string]dbus.Variant) string {
	t.Helper()
	sec, ok := resp[wifiSecuritySetting]
	if !ok {
		return ""
	}
	v, ok := sec["psk"]
	if !ok {
		return ""
	}
	psk, _ := v.Value().(string)
	return psk
}

func TestGetSecrets_NotRegistered_ReturnsEmpty(t *testing.T) {
	a := newTestAgent(t, false)
	a.Stash("MySSID", "secret123")

	resp, err := a.GetSecrets(wifiSettingsWithSSID("MySSID"), "/path", wifiSecuritySetting, nil, 0)
	if err != nil {
		t.Fatalf("unexpected D-Bus error: %v", err)
	}
	if got := extractPSK(t, resp); got != "" {
		t.Fatalf("expected empty response when not Registered, got psk=%q", got)
	}
}

func TestGetSecrets_WrongSetting_ReturnsEmpty(t *testing.T) {
	a := newTestAgent(t, true)
	a.Stash("MySSID", "secret123")

	resp, _ := a.GetSecrets(wifiSettingsWithSSID("MySSID"), "/path", "vpn", nil, 0)
	if got := extractPSK(t, resp); got != "" {
		t.Fatalf("expected empty for non-WiFi setting, got psk=%q", got)
	}
}

func TestGetSecrets_RequestNewFlag_ReturnsEmpty(t *testing.T) {
	a := newTestAgent(t, true)
	a.Stash("MySSID", "secret123")

	resp, _ := a.GetSecrets(wifiSettingsWithSSID("MySSID"), "/path", wifiSecuritySetting, nil, flagRequestNew)
	if got := extractPSK(t, resp); got != "" {
		t.Fatalf("expected empty on REQUEST_NEW, got psk=%q", got)
	}
}

func TestGetSecrets_EmptyCache_ReturnsEmpty(t *testing.T) {
	a := newTestAgent(t, true)

	resp, _ := a.GetSecrets(wifiSettingsWithSSID("MySSID"), "/path", wifiSecuritySetting, nil, 0)
	if got := extractPSK(t, resp); got != "" {
		t.Fatalf("expected empty on empty cache, got psk=%q", got)
	}
}

func TestGetSecrets_ExactMatch_Returns(t *testing.T) {
	a := newTestAgent(t, true)
	a.Stash("MySSID", "secret123")

	resp, _ := a.GetSecrets(wifiSettingsWithSSID("MySSID"), "/path", wifiSecuritySetting, nil, 0)
	if got := extractPSK(t, resp); got != "secret123" {
		t.Fatalf("expected exact-match psk, got %q", got)
	}
}

func TestGetSecrets_UnknownSSID_NoFallback_ReturnsEmpty_WhenConnectNotInFlight(t *testing.T) {
	a := newTestAgent(t, true)
	a.Stash("MySSID", "secret123")
	// connectInFlight defaults to false — fallback is inhibited

	resp, _ := a.GetSecrets(wifiSettingsWithSSID("OtherSSID"), "/path", wifiSecuritySetting, nil, 0)
	if got := extractPSK(t, resp); got != "" {
		t.Fatalf("expected empty when SSID mismatch + connectInFlight=false, got psk=%q", got)
	}
}

func TestGetSecrets_UnknownSSID_FallbackHits_WhenConnectInFlight(t *testing.T) {
	a := newTestAgent(t, true)
	a.Stash("MySSID", "secret123")
	a.SetConnectInFlight(true)
	defer a.SetConnectInFlight(false)

	// SSID byte mismatch (e.g. trailing whitespace from broadcast bytes)
	resp, _ := a.GetSecrets(wifiSettingsWithSSID("MySSID "), "/path", wifiSecuritySetting, nil, 0)
	if got := extractPSK(t, resp); got != "secret123" {
		t.Fatalf("expected fallback-hit psk on single-entry-cache, got %q", got)
	}
}

func TestGetSecrets_UnknownSSID_FallbackInhibited_WhenMultipleEntries(t *testing.T) {
	a := newTestAgent(t, true)
	a.Stash("Primary", "primary-secret")
	a.Stash("Rollback", "rollback-secret")
	a.SetConnectInFlight(true)
	defer a.SetConnectInFlight(false)

	// SSID mismatch + cache has 2 entries → fallback NOT triggered
	resp, _ := a.GetSecrets(wifiSettingsWithSSID("OtherSSID"), "/path", wifiSecuritySetting, nil, 0)
	if got := extractPSK(t, resp); got != "" {
		t.Fatalf("expected empty (2-entry cache disables fallback), got %q", got)
	}
}

func TestGetSecrets_TwoEntries_ExactMatchStillWorks(t *testing.T) {
	a := newTestAgent(t, true)
	a.Stash("Primary", "primary-secret")
	a.Stash("Rollback", "rollback-secret")
	a.SetConnectInFlight(true)
	defer a.SetConnectInFlight(false)

	for _, tc := range []struct{ ssid, want string }{
		{"Primary", "primary-secret"},
		{"Rollback", "rollback-secret"},
	} {
		resp, _ := a.GetSecrets(wifiSettingsWithSSID(tc.ssid), "/path", wifiSecuritySetting, nil, 0)
		if got := extractPSK(t, resp); got != tc.want {
			t.Errorf("ssid=%q want=%q got=%q", tc.ssid, tc.want, got)
		}
	}
}

func TestStash_EmptyValues_Rejected(t *testing.T) {
	a := newTestAgent(t, true)
	a.Stash("", "secret")
	a.Stash("ssid", "")
	a.Stash("", "")

	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if len(a.cache) != 0 {
		t.Fatalf("cache should be empty after rejected stashes, got %d entries", len(a.cache))
	}
}

func TestStash_Drain_Roundtrip(t *testing.T) {
	a := newTestAgent(t, true)
	a.Stash("A", "secretA")
	a.Stash("B", "secretB")

	a.cacheMu.Lock()
	if len(a.cache) != 2 {
		t.Fatalf("expected 2 entries after stashes, got %d", len(a.cache))
	}
	a.cacheMu.Unlock()

	a.Drain("A")
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if _, ok := a.cache["A"]; ok {
		t.Fatalf("A should be drained")
	}
	if _, ok := a.cache["B"]; !ok {
		t.Fatalf("B should still be present after draining A")
	}
}

func TestStash_OverwritesSameSSID(t *testing.T) {
	a := newTestAgent(t, true)
	a.Stash("MySSID", "old")
	a.Stash("MySSID", "new")

	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if a.cache["MySSID"] != "new" {
		t.Fatalf("Stash should overwrite, got %q", a.cache["MySSID"])
	}
}

func TestSetConnectInFlight_ToggleVisibleToGetSecrets(t *testing.T) {
	a := newTestAgent(t, true)
	a.Stash("MySSID", "secret123")

	// Without connectInFlight, fallback inhibited
	a.SetConnectInFlight(false)
	resp, _ := a.GetSecrets(wifiSettingsWithSSID("Wrong"), "/path", wifiSecuritySetting, nil, 0)
	if got := extractPSK(t, resp); got != "" {
		t.Fatalf("expected empty with connectInFlight=false, got %q", got)
	}

	// With connectInFlight, fallback fires
	a.SetConnectInFlight(true)
	resp, _ = a.GetSecrets(wifiSettingsWithSSID("Wrong"), "/path", wifiSecuritySetting, nil, 0)
	if got := extractPSK(t, resp); got != "secret123" {
		t.Fatalf("expected fallback hit with connectInFlight=true, got %q", got)
	}

	// Toggle back
	a.SetConnectInFlight(false)
	resp, _ = a.GetSecrets(wifiSettingsWithSSID("Wrong"), "/path", wifiSecuritySetting, nil, 0)
	if got := extractPSK(t, resp); got != "" {
		t.Fatalf("expected empty after toggling connectInFlight back to false, got %q", got)
	}
}

func TestNoOpDBusMethods_ReturnNil(t *testing.T) {
	a := newTestAgent(t, true)
	if err := a.CancelGetSecrets("/p", "x"); err != nil {
		t.Fatalf("CancelGetSecrets returned %v, want nil", err)
	}
	if err := a.SaveSecrets(nil, "/p"); err != nil {
		t.Fatalf("SaveSecrets returned %v, want nil", err)
	}
	if err := a.DeleteSecrets(nil, "/p"); err != nil {
		t.Fatalf("DeleteSecrets returned %v, want nil", err)
	}
}

func TestConcurrent_StashDrainGetSecrets_NoRace(t *testing.T) {
	// Run with `go test -race` to verify mutex correctness.
	a := newTestAgent(t, true)
	a.SetConnectInFlight(true)

	var wg sync.WaitGroup
	const writers = 8
	const readers = 16
	const iters = 200

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ssid := []string{"A", "B", "C"}[id%3]
			for j := 0; j < iters; j++ {
				a.Stash(ssid, "secret")
				a.Drain(ssid)
			}
		}(i)
	}

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				_, _ = a.GetSecrets(wifiSettingsWithSSID("A"), "/p", wifiSecuritySetting, nil, 0)
			}
		}()
	}

	wg.Wait()
}

func TestIsRegistered(t *testing.T) {
	a := newTestAgent(t, false)
	if a.IsRegistered() {
		t.Fatalf("should not be Registered after newTestAgent(false)")
	}
	a.state.Store(int32(stateRegistered))
	if !a.IsRegistered() {
		t.Fatalf("should be Registered after state.Store")
	}
}

func TestIsLocalDisconnected(t *testing.T) {
	cases := []struct {
		name string
		sig  *dbus.Signal
		want bool
	}{
		{"local disconnected",
			&dbus.Signal{Name: "org.freedesktop.DBus.Local.Disconnected"},
			true},
		{"some other signal",
			&dbus.Signal{Name: "org.freedesktop.NetworkManager.StateChanged"},
			false},
		{"empty name",
			&dbus.Signal{},
			false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLocalDisconnected(tc.sig); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestIsNameOwnerChangedForNM(t *testing.T) {
	cases := []struct {
		name string
		sig  *dbus.Signal
		want bool
	}{
		{"matches NM",
			&dbus.Signal{
				Name: "org.freedesktop.DBus.NameOwnerChanged",
				Body: []interface{}{agentManagerBus, "", ":1.42"},
			}, true},
		{"different bus name",
			&dbus.Signal{
				Name: "org.freedesktop.DBus.NameOwnerChanged",
				Body: []interface{}{"some.other.Service", "", ":1.42"},
			}, false},
		{"wrong signal name",
			&dbus.Signal{
				Name: "org.freedesktop.DBus.NameAcquired",
				Body: []interface{}{agentManagerBus},
			}, false},
		{"empty body",
			&dbus.Signal{
				Name: "org.freedesktop.DBus.NameOwnerChanged",
			}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNameOwnerChangedForNM(tc.sig); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestParseNameOwnerChanged(t *testing.T) {
	sig := &dbus.Signal{
		Body: []interface{}{"name", "old", "new"},
	}
	name, oldOwner, newOwner := parseNameOwnerChanged(sig)
	if name != "name" || oldOwner != "old" || newOwner != "new" {
		t.Fatalf("got (%q, %q, %q)", name, oldOwner, newOwner)
	}

	// Empty body returns zeros, doesn't panic.
	empty := &dbus.Signal{}
	name, oldOwner, newOwner = parseNameOwnerChanged(empty)
	if name != "" || oldOwner != "" || newOwner != "" {
		t.Fatalf("expected zero-values for empty body, got (%q, %q, %q)", name, oldOwner, newOwner)
	}
}

// TestLifecycleLoop_OpenBusRetryDoesNotSpin asserts that when openBus fails
// repeatedly, the lifecycle goroutine retries on the backoff schedule
// rather than spinning on a closed sigCh. Specifically guards against
// the regression where openBus failure left a closed sigCh installed,
// causing `<-closedCh` to win the select with nil and pin a CPU.
func TestLifecycleLoop_OpenBusRetryDoesNotSpin(t *testing.T) {
	var dialCount atomic.Int32
	failingDialer := func() (*dbus.Conn, error) {
		dialCount.Add(1)
		return nil, errors.New("test: dialer always fails")
	}

	a := &Agent{
		dialer: failingDialer,
		cache:  make(map[string]string),
		lcDone: make(chan struct{}),
	}
	a.state.Store(int32(stateLost))

	// First openBus to fail — should leave sigCh nil so the loop's
	// signal case is disabled. Then start the goroutine and verify it
	// retries via the backoff timer without spinning.
	if err := a.openBus(); err == nil {
		t.Fatalf("expected openBus to fail with failing dialer")
	}
	if a.sigCh != nil {
		t.Fatalf("after failed openBus, a.sigCh must be nil to disable signal case")
	}
	if a.conn != nil {
		t.Fatalf("after failed openBus, a.conn must be nil")
	}

	a.lcCtx, a.lcCancel = context.WithCancel(context.Background())
	go a.lifecycleLoop()

	// Watch for ~1.3s — long enough to see at least one retry-timer fire
	// (backoffMin=1s) but short enough that a tight spin would have
	// produced thousands of dial attempts. The first dial happened in
	// the openBus call above (counter=1); we expect 1–3 more during the
	// observation window.
	time.Sleep(1300 * time.Millisecond)
	a.lcCancel()
	<-a.lcDone

	dials := dialCount.Load()
	if dials < 2 {
		t.Fatalf("expected goroutine to retry openBus at least once; saw %d total dials", dials)
	}
	if dials > 50 {
		t.Fatalf("openBus retried %d times in 1.3s — likely a tight spin", dials)
	}
}

// TestLifecycleLoop_ClosedSigChDoesNotSpin asserts that when godbus
// closes the signal channel (its primary bus-disconnect mechanism in v5),
// the lifecycle goroutine treats it as bus-death, transitions to Lost,
// clears bus state, and retries via the timer rather than spinning on
// the closed channel. Regression guard for the P1 found in Phase 3.
func TestLifecycleLoop_ClosedSigChDoesNotSpin(t *testing.T) {
	var dialCount atomic.Int32
	failingDialer := func() (*dbus.Conn, error) {
		dialCount.Add(1)
		return nil, errors.New("test: dialer always fails (post-close)")
	}

	a := &Agent{
		dialer: failingDialer,
		cache:  make(map[string]string),
		lcDone: make(chan struct{}),
	}
	// Simulate the post-Terminate state: state was Registered, the signal
	// channel is closed by godbus. We leave a.conn=nil because the loop's
	// first tryRegister returns false on nil conn (correct behavior); the
	// scenario we're testing is the closed-channel detection itself, which
	// must transition state to Lost and arm the retry timer.
	a.state.Store(int32(stateRegistered))
	closedCh := make(chan *dbus.Signal)
	close(closedCh)
	a.sigCh = closedCh

	a.lcCtx, a.lcCancel = context.WithCancel(context.Background())
	go a.lifecycleLoop()

	// Watch for ~1.3s — long enough for the closed-channel detection to
	// fire (immediate), the bus-disconnect teardown to run, and at least
	// one retry-timer cycle to attempt openBus (backoffMin=1s).
	time.Sleep(1300 * time.Millisecond)
	a.lcCancel()
	<-a.lcDone

	// State should have transitioned to Lost when the channel closed.
	if a.IsRegistered() {
		t.Fatalf("expected Registered=false after closed-sigCh teardown")
	}
	dials := dialCount.Load()
	if dials < 1 {
		t.Fatalf("expected at least 1 reopen attempt after closed-sigCh; saw %d", dials)
	}
	if dials > 50 {
		t.Fatalf("openBus attempted %d times in 1.3s — likely a tight spin", dials)
	}
}

// TestLifecycleLoop_StopWithoutSpin verifies the goroutine exits cleanly
// on context cancel even when stuck in retry mode.
func TestLifecycleLoop_StopWithoutSpin(t *testing.T) {
	failingDialer := func() (*dbus.Conn, error) {
		return nil, errors.New("test: dialer always fails")
	}
	a := &Agent{
		dialer: failingDialer,
		cache:  make(map[string]string),
		lcDone: make(chan struct{}),
	}
	a.state.Store(int32(stateLost))
	_ = a.openBus() // expected error; bus left closed

	a.lcCtx, a.lcCancel = context.WithCancel(context.Background())
	go a.lifecycleLoop()

	time.Sleep(50 * time.Millisecond)
	a.lcCancel()

	select {
	case <-a.lcDone:
		// success
	case <-time.After(2 * time.Second):
		t.Fatalf("lifecycle goroutine did not exit within 2s of cancel")
	}
}

func TestNextBackoff_DoublesUntilCap(t *testing.T) {
	cur := backoffMin
	for i := 0; i < 20; i++ {
		next := nextBackoff(cur)
		if next < cur {
			t.Fatalf("backoff regressed: %v -> %v", cur, next)
		}
		if next > backoffMax {
			t.Fatalf("backoff exceeded cap: %v > %v", next, backoffMax)
		}
		cur = next
	}
	if cur != backoffMax {
		t.Fatalf("after enough doublings, expected to be at cap %v, got %v", backoffMax, cur)
	}
}

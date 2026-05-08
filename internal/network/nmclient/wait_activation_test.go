// Path-scoped WaitForActivation regression tests per RFC 20260508.
//
// LOAD-BEARING: A-Risk-5 of the RFC names this file's filter-chain integration
// surface as the only detector for D6 filter-chain regressions. If you weaken
// or remove the captive-reentry / failure / TOCTOU coverage below, re-evaluate
// the deferred Uuid post-check defense-in-depth in the RFC.

package nmclient

import (
	"context"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

const testPath dbus.ObjectPath = "/org/freedesktop/NetworkManager/ActiveConnection/test-1"

// Captive AP→STA success: wait subscribes after AddAndActivate, sees
// Activating → Activated lifecycle on the path. No reads from device-state
// (which would be in Deactivating from the OLD AP teardown). Returns
// Activated cleanly.
//
// This is the headline brcmfmac case. Pre-RFC, device-scoped wait would
// have falsely returned (Disconnected, UserRequested) on the AP teardown's
// trailing event before NEW path even started.
func TestRunActivationWaitByPath_CaptiveAPtoSTASuccess(t *testing.T) {
	t.Parallel()

	ch := make(chan ActiveConnectionStateChange, 16)
	events := []ActiveConnectionStateChange{
		{Path: testPath, NewState: NMActiveConnectionStateActivating, Reason: NMActiveConnectionStateReasonNone},
		{Path: testPath, NewState: NMActiveConnectionStateActivated, Reason: NMActiveConnectionStateReasonNone},
	}
	for _, e := range events {
		ch <- e
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	state, reason, err := runActivationWaitByPath(ctx, ch, nilTerminalReader, testPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != NMDeviceStateActivated {
		t.Errorf("state=%s, want Activated", state)
	}
	if reason != NMDeviceStateReasonNone {
		t.Errorf("reason=%s, want None", reason)
	}
}

// New connection fails at need-auth (e.g., wrong PSK after secret agent
// answers). NM transitions Activating → Deactivating with NoSecrets.
// Wait returns translated (Disconnected, NoSecrets) — Connect logs
// reason=no_secrets so operators see the precise cause.
func TestRunActivationWaitByPath_NoSecretsFailure(t *testing.T) {
	t.Parallel()

	ch := make(chan ActiveConnectionStateChange, 16)
	ch <- ActiveConnectionStateChange{Path: testPath, NewState: NMActiveConnectionStateActivating}
	ch <- ActiveConnectionStateChange{Path: testPath, NewState: NMActiveConnectionStateDeactivating, Reason: NMActiveConnectionStateReasonNoSecrets}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	state, reason, err := runActivationWaitByPath(ctx, ch, nilTerminalReader, testPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != NMDeviceStateDisconnected {
		t.Errorf("state=%s, want Disconnected", state)
	}
	if reason != NMDeviceStateReasonNoSecrets {
		t.Errorf("reason=%s, want NoSecrets", reason)
	}
}

// brcmfmac firmware-failure mode: NM transitions to Deactivating with
// DeviceRealizeFailed. Translation must produce FirmwareMissing (not
// "other") so operator triage on the headline hardware works.
func TestRunActivationWaitByPath_BrcmfmacFirmwareFailure(t *testing.T) {
	t.Parallel()

	ch := make(chan ActiveConnectionStateChange, 16)
	ch <- ActiveConnectionStateChange{Path: testPath, NewState: NMActiveConnectionStateActivating}
	ch <- ActiveConnectionStateChange{Path: testPath, NewState: NMActiveConnectionStateDeactivated, Reason: NMActiveConnectionStateReasonDeviceRealizeFailed}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	state, reason, err := runActivationWaitByPath(ctx, ch, nilTerminalReader, testPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != NMDeviceStateDisconnected {
		t.Errorf("state=%s, want Disconnected", state)
	}
	if reason != NMDeviceStateReasonFirmwareMissing {
		t.Errorf("reason=%s, want FirmwareMissing — brcmfmac triage degrades on this", reason)
	}
}

// Connection-removed via path's own lifecycle: NM tears down because
// piccolod's rollback issued DeleteConnection, OR user did ForgetNetwork.
// reason=connection_removed is the explicit-removal fingerprint, distinct
// from the synth=path_removed ambiguous-cause case (D4-iv).
func TestRunActivationWaitByPath_ConnectionRemoved(t *testing.T) {
	t.Parallel()

	ch := make(chan ActiveConnectionStateChange, 16)
	ch <- ActiveConnectionStateChange{Path: testPath, NewState: NMActiveConnectionStateActivating}
	ch <- ActiveConnectionStateChange{Path: testPath, NewState: NMActiveConnectionStateDeactivating, Reason: NMActiveConnectionStateReasonConnectionRemoved}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, reason, err := runActivationWaitByPath(ctx, ch, nilTerminalReader, testPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason != NMDeviceStateReasonConnectionRemoved {
		t.Errorf("reason=%s, want ConnectionRemoved", reason)
	}
}

// Channel close mid-wait surfaces as explicit error (not a silent stall).
// Mirrors signal-handler termination on bus disconnect.
func TestRunActivationWaitByPath_ChannelClosed(t *testing.T) {
	t.Parallel()

	ch := make(chan ActiveConnectionStateChange)
	close(ch)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, _, err := runActivationWaitByPath(ctx, ch, nilTerminalReader, testPath)
	if err == nil {
		t.Fatal("expected error on closed channel, got nil")
	}
}

// Context cancellation returns ctx.Err() — used by Connect's 30s budget.
func TestRunActivationWaitByPath_ContextCancel(t *testing.T) {
	t.Parallel()

	ch := make(chan ActiveConnectionStateChange) // never sends
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := runActivationWaitByPath(ctx, ch, nilTerminalReader, testPath)
	if err == nil {
		t.Fatal("expected ctx error, got nil")
	}
}

// Activating-then-non-State events are ignored; loop continues. Verifies
// non-State PropertiesChanged (Ip4Config, Default, etc.) don't terminate.
// Stub channel only emits State changes per the package contract, so this
// is exercised by the "Activating doesn't terminate" assertion: send a
// stream of Activating events and verify ctx-timeout fires.
func TestRunActivationWaitByPath_ActivatingDoesNotTerminate(t *testing.T) {
	t.Parallel()

	ch := make(chan ActiveConnectionStateChange, 16)
	for i := 0; i < 5; i++ {
		ch <- ActiveConnectionStateChange{Path: testPath, NewState: NMActiveConnectionStateActivating}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _, err := runActivationWaitByPath(ctx, ch, nilTerminalReader, testPath)
	if err == nil {
		t.Fatal("expected ctx-timeout error (no terminal events sent), got nil")
	}
}

// translateActiveConnReason truth table: the brcmfmac firmware-failure
// rows (D3) must NOT degrade to Unknown. Captive-flow hot-path rows
// (NoSecrets, ConnectionRemoved, UserRequested) must round-trip cleanly.
func TestTranslateActiveConnReason(t *testing.T) {
	t.Parallel()

	cases := []struct {
		acr  NMActiveConnectionStateReason
		want NMDeviceStateReason
	}{
		{NMActiveConnectionStateReasonNone, NMDeviceStateReasonNone},
		{NMActiveConnectionStateReasonUserDisconnected, NMDeviceStateReasonUserRequested},
		{NMActiveConnectionStateReasonDeviceDisconnect, NMDeviceStateReasonUserRequested},
		{NMActiveConnectionStateReasonServiceStopped, NMDeviceStateReasonSupplicantDisconnect},
		{NMActiveConnectionStateReasonIPConfigInvalid, NMDeviceStateReasonConfigFailed},
		{NMActiveConnectionStateReasonConnectTimeout, NMDeviceStateReasonSupplicantTimeout},
		{NMActiveConnectionStateReasonServiceStartTimeout, NMDeviceStateReasonSupplicantTimeout},
		{NMActiveConnectionStateReasonServiceStartFailed, NMDeviceStateReasonSupplicantFailed},
		{NMActiveConnectionStateReasonNoSecrets, NMDeviceStateReasonNoSecrets},
		{NMActiveConnectionStateReasonLoginFailed, NMDeviceStateReasonSupplicantFailed},
		{NMActiveConnectionStateReasonConnectionRemoved, NMDeviceStateReasonConnectionRemoved},
		// brcmfmac firmware-failure modes — must not degrade to Unknown
		{NMActiveConnectionStateReasonDependencyFailed, NMDeviceStateReasonConfigFailed},
		{NMActiveConnectionStateReasonDeviceRealizeFailed, NMDeviceStateReasonFirmwareMissing},
		{NMActiveConnectionStateReasonDeviceRemoved, NMDeviceStateReasonRemoved},
		// Unknown explicit (not table-fallthrough)
		{NMActiveConnectionStateReasonUnknown, NMDeviceStateReasonUnknown},
	}
	for _, c := range cases {
		if got := translateActiveConnReason(c.acr); got != c.want {
			t.Errorf("translateActiveConnReason(%d)=%s, want %s", c.acr, got, c.want)
		}
	}
}

// isUnknownObjectErr matches the three D-Bus error names path-removed
// detection relies on. False positives here would silently route real
// transient errors to the synth=path_removed branch, masking them as
// path-disappearance instead of the actual cause.
func TestIsUnknownObjectErr(t *testing.T) {
	t.Parallel()

	mkDBusErr := func(name string) error {
		return dbus.Error{Name: name, Body: []interface{}{"detail"}}
	}

	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{mkDBusErr("org.freedesktop.DBus.Error.UnknownObject"), true},
		{mkDBusErr("org.freedesktop.DBus.Error.UnknownInterface"), true},
		{mkDBusErr("org.freedesktop.DBus.Error.UnknownProperty"), true},
		{mkDBusErr("org.freedesktop.DBus.Error.NoReply"), false},
		{mkDBusErr("org.freedesktop.DBus.Error.AccessDenied"), false},
		{errFakeDBus("connection broken"), false}, // non-dbus.Error
	}
	for _, c := range cases {
		if got := isUnknownObjectErr(c.err); got != c.want {
			t.Errorf("isUnknownObjectErr(%v)=%v, want %v", c.err, got, c.want)
		}
	}
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

func errFakeDBus(s string) error { return fakeErr(s) }

// nilTerminalReader is a terminalReader stub for tests whose event streams
// never contain NewState=Unknown(0) — the read is unreachable so the stub
// returns a non-terminal signal that would fail loudly if mistakenly invoked.
func nilTerminalReader(path dbus.ObjectPath) (NMActiveConnectionState, NMActiveConnectionStateReason, bool, error) {
	return NMActiveConnectionStateActivating, NMActiveConnectionStateReasonUnknown, false, nil
}

// fakeTerminalReader returns canned outcomes for D1 case-C tests.
type fakeTerminalReader struct {
	state    NMActiveConnectionState
	reason   NMActiveConnectionStateReason
	terminal bool
	err      error
	calls    int
}

func (f *fakeTerminalReader) read(path dbus.ObjectPath) (NMActiveConnectionState, NMActiveConnectionStateReason, bool, error) {
	f.calls++
	return f.state, f.reason, f.terminal, f.err
}

// D1 case-C: invalidated[State] arrives → wait loop calls read() →
// returns Activated terminal. Verifies the seam is exercised and the
// terminal reaches the caller via translation.
func TestRunActivationWaitByPath_CaseC_GetReturnsTerminalActivated(t *testing.T) {
	t.Parallel()

	ch := make(chan ActiveConnectionStateChange, 4)
	// Decoder emits NewState=Unknown(0) for invalidated[State] per signals.go.
	ch <- ActiveConnectionStateChange{Path: testPath, NewState: NMActiveConnectionStateUnknown}

	reader := &fakeTerminalReader{
		state:    NMActiveConnectionStateActivated,
		reason:   NMActiveConnectionStateReasonNone,
		terminal: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	state, _, err := runActivationWaitByPath(ctx, ch, reader.read, testPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != NMDeviceStateActivated {
		t.Errorf("state=%s, want Activated", state)
	}
	if reader.calls != 1 {
		t.Errorf("reader.calls=%d, want 1 (case-C should invoke the seam exactly once)", reader.calls)
	}
}

// D1 case-C: invalidated[State] arrives → read() returns UnknownObject
// terminal sentinel (already translated by readActiveConnTerminalState in
// production; the stub returns the terminal flag set with state=Deactivated).
// Verifies the path-removed-on-toctou path reaches translation.
func TestRunActivationWaitByPath_CaseC_GetReturnsUnknownObject(t *testing.T) {
	t.Parallel()

	ch := make(chan ActiveConnectionStateChange, 4)
	ch <- ActiveConnectionStateChange{Path: testPath, NewState: NMActiveConnectionStateUnknown}

	// readActiveConnTerminalState's UnknownObject branch returns
	// (Deactivated, Unknown, terminal=true, err=nil) and emits the
	// synth=path_removed log inline. The stub mirrors that outcome.
	reader := &fakeTerminalReader{
		state:    NMActiveConnectionStateDeactivated,
		reason:   NMActiveConnectionStateReasonUnknown,
		terminal: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	state, reason, err := runActivationWaitByPath(ctx, ch, reader.read, testPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != NMDeviceStateDisconnected {
		t.Errorf("state=%s, want Disconnected (translated from Deactivated)", state)
	}
	if reason != NMDeviceStateReasonUnknown {
		t.Errorf("reason=%s, want Unknown (path-removed-on-toctou is cause-ambiguous)", reason)
	}
}

// D1 case-C: invalidated[State] arrives → read() returns non-terminal →
// loop continues. A subsequent terminal event resolves the wait.
func TestRunActivationWaitByPath_CaseC_GetReturnsNonTerminalThenContinues(t *testing.T) {
	t.Parallel()

	ch := make(chan ActiveConnectionStateChange, 4)
	ch <- ActiveConnectionStateChange{Path: testPath, NewState: NMActiveConnectionStateUnknown}
	ch <- ActiveConnectionStateChange{Path: testPath, NewState: NMActiveConnectionStateActivated}

	reader := &fakeTerminalReader{
		state:    NMActiveConnectionStateActivating,
		reason:   NMActiveConnectionStateReasonUnknown,
		terminal: false, // not terminal — loop should continue past this read
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	state, _, err := runActivationWaitByPath(ctx, ch, reader.read, testPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != NMDeviceStateActivated {
		t.Errorf("state=%s, want Activated (resolved by event after non-terminal read)", state)
	}
	if reader.calls != 1 {
		t.Errorf("reader.calls=%d, want exactly 1 — only the first invalidated event invokes read", reader.calls)
	}
}

// D1 case-C: read() returns programmer-error class issue → wait surfaces.
func TestRunActivationWaitByPath_CaseC_GetReturnsError(t *testing.T) {
	t.Parallel()

	ch := make(chan ActiveConnectionStateChange, 4)
	ch <- ActiveConnectionStateChange{Path: testPath, NewState: NMActiveConnectionStateUnknown}

	reader := &fakeTerminalReader{err: fakeErr("programmer error")}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, _, err := runActivationWaitByPath(ctx, ch, reader.read, testPath)
	if err == nil {
		t.Fatal("expected error from reader to surface")
	}
}

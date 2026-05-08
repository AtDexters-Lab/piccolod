// LOAD-BEARING: A-Risk-5 of RFC 20260508 names this file as the only
// detector for D6 filter-chain regressions. The post-success Id check in
// Manager.Connect was removed (D12) on the explicit promise that this
// integration test would catch wrong-path-accepted bugs. If you weaken or
// remove the foreign-path / foreign-interface / foreign-signal-name cases
// below, re-evaluate the deferred Uuid post-check defense-in-depth.

package nmclient

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

const ourPath dbus.ObjectPath = "/org/freedesktop/NetworkManager/ActiveConnection/42"

// makeSignal constructs a *dbus.Signal mimicking godbus's Properties
// PropertiesChanged emission shape: (interface_name string,
// changed_properties map[string]Variant, invalidated_properties []string).
func makeSignal(path dbus.ObjectPath, name, iface string, changed map[string]dbus.Variant, invalidated []string) *dbus.Signal {
	if changed == nil {
		changed = map[string]dbus.Variant{}
	}
	return &dbus.Signal{
		Path: path,
		Name: name,
		Body: []interface{}{iface, changed, invalidated},
	}
}

// Filter-chain step 1: foreign path must be dropped. godbus delivers every
// signal on the connection to every registered channel — without the in-
// process path filter, a foreign-path State=Activated event for someone
// else's subscription could feed our wait loop and trip a false success.
func TestDecodeActiveConnPropertiesChanged_DropsForeignPath(t *testing.T) {
	t.Parallel()

	foreign := dbus.ObjectPath("/org/freedesktop/NetworkManager/ActiveConnection/99")
	sig := makeSignal(foreign, dbusPropertiesInterface+".PropertiesChanged", nmActiveConnInterface,
		map[string]dbus.Variant{"State": dbus.MakeVariant(uint32(NMActiveConnectionStateActivated))}, nil)

	if _, ok := decodeActiveConnPropertiesChanged(sig, ourPath); ok {
		t.Fatal("expected foreign-path signal to be dropped")
	}
}

// Filter-chain step 2: foreign signal name must be dropped. A misconfigured
// AddMatch or a future godbus quirk could deliver non-PropertiesChanged
// signals through the channel.
func TestDecodeActiveConnPropertiesChanged_DropsForeignSignalName(t *testing.T) {
	t.Parallel()

	sig := makeSignal(ourPath, dbusPropertiesInterface+".PropertiesInvalidated", nmActiveConnInterface,
		map[string]dbus.Variant{"State": dbus.MakeVariant(uint32(NMActiveConnectionStateActivated))}, nil)

	if _, ok := decodeActiveConnPropertiesChanged(sig, ourPath); ok {
		t.Fatal("expected foreign-signal-name to be dropped")
	}
}

// Filter-chain step 3: foreign D-Bus interface in body[0] must be dropped.
// Active-connection paths can in principle host other interfaces; this
// guards against handling foreign-interface PropertiesChanged routed
// through the same path.
func TestDecodeActiveConnPropertiesChanged_DropsForeignInterface(t *testing.T) {
	t.Parallel()

	sig := makeSignal(ourPath, dbusPropertiesInterface+".PropertiesChanged", nmDeviceInterface,
		map[string]dbus.Variant{"State": dbus.MakeVariant(uint32(NMActiveConnectionStateActivated))}, nil)

	if _, ok := decodeActiveConnPropertiesChanged(sig, ourPath); ok {
		t.Fatal("expected foreign-interface signal to be dropped")
	}
}

// Body too short → drop. Defensive against malformed signals.
func TestDecodeActiveConnPropertiesChanged_DropsShortBody(t *testing.T) {
	t.Parallel()

	sig := &dbus.Signal{
		Path: ourPath,
		Name: dbusPropertiesInterface + ".PropertiesChanged",
		Body: []interface{}{nmActiveConnInterface}, // missing changed + invalidated
	}
	if _, ok := decodeActiveConnPropertiesChanged(sig, ourPath); ok {
		t.Fatal("expected short-body signal to be dropped")
	}
}

// Case A — changed[State] present, valid uint32. Standard path; emits
// the State value.
func TestDecodeActiveConnPropertiesChanged_CaseA_ChangedState(t *testing.T) {
	t.Parallel()

	sig := makeSignal(ourPath, dbusPropertiesInterface+".PropertiesChanged", nmActiveConnInterface,
		map[string]dbus.Variant{
			"State":  dbus.MakeVariant(uint32(NMActiveConnectionStateActivated)),
			"Reason": dbus.MakeVariant(uint32(NMActiveConnectionStateReasonNone)),
		}, nil)

	evt, ok := decodeActiveConnPropertiesChanged(sig, ourPath)
	if !ok {
		t.Fatal("expected valid State change to be accepted")
	}
	if evt.NewState != NMActiveConnectionStateActivated {
		t.Errorf("NewState=%s, want Activated", evt.NewState)
	}
	if evt.Path != ourPath {
		t.Errorf("Path=%s, want %s", evt.Path, ourPath)
	}
}

// Case B — non-State property changed (Ip4Config, Default, etc.). Must
// be dropped to avoid flooding the wait loop with non-load-bearing events.
func TestDecodeActiveConnPropertiesChanged_CaseB_NonStateChanged(t *testing.T) {
	t.Parallel()

	sig := makeSignal(ourPath, dbusPropertiesInterface+".PropertiesChanged", nmActiveConnInterface,
		map[string]dbus.Variant{
			"Ip4Config": dbus.MakeVariant(dbus.ObjectPath("/foo")),
			"Default":   dbus.MakeVariant(true),
		}, nil)

	if _, ok := decodeActiveConnPropertiesChanged(sig, ourPath); ok {
		t.Fatal("expected non-State PropertiesChanged to be dropped")
	}
}

// Case C — invalidated[State]. NM emits this when the property value
// cannot be efficiently conveyed (typically during object teardown).
// Decoder emits NewState=Unknown(0) sentinel; the consumer
// (WaitForActivation) is expected to resolve via Properties.Get.
func TestDecodeActiveConnPropertiesChanged_CaseC_InvalidatedState(t *testing.T) {
	t.Parallel()

	sig := makeSignal(ourPath, dbusPropertiesInterface+".PropertiesChanged", nmActiveConnInterface,
		nil, []string{"State"})

	evt, ok := decodeActiveConnPropertiesChanged(sig, ourPath)
	if !ok {
		t.Fatal("expected invalidated[State] to emit a sentinel event")
	}
	if evt.NewState != NMActiveConnectionStateUnknown {
		t.Errorf("NewState=%s, want Unknown(0) sentinel for case-C", evt.NewState)
	}
}

// State present in invalidated[] AND non-State key in changed{} —
// invalidated[State] still triggers the sentinel emission. Edge case
// where NM sends both shapes in one signal.
func TestDecodeActiveConnPropertiesChanged_CaseC_InvalidatedStateWithNonStateChanged(t *testing.T) {
	t.Parallel()

	sig := makeSignal(ourPath, dbusPropertiesInterface+".PropertiesChanged", nmActiveConnInterface,
		map[string]dbus.Variant{"Ip4Config": dbus.MakeVariant(dbus.ObjectPath("/foo"))},
		[]string{"State"})

	evt, ok := decodeActiveConnPropertiesChanged(sig, ourPath)
	if !ok {
		t.Fatal("expected invalidated[State] to emit a sentinel event")
	}
	if evt.NewState != NMActiveConnectionStateUnknown {
		t.Errorf("NewState=%s, want Unknown(0) sentinel", evt.NewState)
	}
}

// changed[State] present but typed as something other than uint32.
// Defensive against godbus quirks; should drop, not panic.
func TestDecodeActiveConnPropertiesChanged_DropsWrongTypedState(t *testing.T) {
	t.Parallel()

	sig := makeSignal(ourPath, dbusPropertiesInterface+".PropertiesChanged", nmActiveConnInterface,
		map[string]dbus.Variant{"State": dbus.MakeVariant("activated")}, nil) // string, not uint32

	if _, ok := decodeActiveConnPropertiesChanged(sig, ourPath); ok {
		t.Fatal("expected wrong-typed State to be dropped")
	}
}

// Reason field decoded alongside State when present and uint32.
func TestDecodeActiveConnPropertiesChanged_DecodesReason(t *testing.T) {
	t.Parallel()

	sig := makeSignal(ourPath, dbusPropertiesInterface+".PropertiesChanged", nmActiveConnInterface,
		map[string]dbus.Variant{
			"State":  dbus.MakeVariant(uint32(NMActiveConnectionStateDeactivating)),
			"Reason": dbus.MakeVariant(uint32(NMActiveConnectionStateReasonNoSecrets)),
		}, nil)

	evt, ok := decodeActiveConnPropertiesChanged(sig, ourPath)
	if !ok {
		t.Fatal("expected accepted")
	}
	if evt.Reason != NMActiveConnectionStateReasonNoSecrets {
		t.Errorf("Reason=%d, want NoSecrets(9)", evt.Reason)
	}
}

// Reason missing → defaults to Unknown(0). Consumer is expected to read
// it via Properties.Get if needed.
func TestDecodeActiveConnPropertiesChanged_NoReasonDefaultsUnknown(t *testing.T) {
	t.Parallel()

	sig := makeSignal(ourPath, dbusPropertiesInterface+".PropertiesChanged", nmActiveConnInterface,
		map[string]dbus.Variant{"State": dbus.MakeVariant(uint32(NMActiveConnectionStateActivated))}, nil)

	evt, ok := decodeActiveConnPropertiesChanged(sig, ourPath)
	if !ok {
		t.Fatal("expected accepted")
	}
	if evt.Reason != NMActiveConnectionStateReasonUnknown {
		t.Errorf("Reason=%d, want Unknown(0)", evt.Reason)
	}
}

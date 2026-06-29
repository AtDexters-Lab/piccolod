package nmclient

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestAccessPoint_SecurityType(t *testing.T) {
	tests := []struct {
		name     string
		ap       AccessPoint
		expected string
	}{
		{"WPA3-SAE", AccessPoint{RSNFlags: NM80211ApSecKeyMgmtSAE}, "wpa3"},
		{"WPA2-PSK", AccessPoint{RSNFlags: NM80211ApSecKeyMgmtPSK}, "wpa2"},
		{"WPA-PSK", AccessPoint{WPAFlags: NM80211ApSecKeyMgmtPSK}, "wpa"},
		{"WPA2-Enterprise", AccessPoint{RSNFlags: NM80211ApSecKeyMgmt8021X}, "enterprise"},
		{"WPA-Enterprise", AccessPoint{WPAFlags: NM80211ApSecKeyMgmt8021X}, "enterprise"},
		{"WEP", AccessPoint{Flags: 0x1}, "wep"},
		{"Open", AccessPoint{}, "open"},
		// WPA3 takes priority over WPA2
		{"WPA3+WPA2", AccessPoint{RSNFlags: NM80211ApSecKeyMgmtSAE | NM80211ApSecKeyMgmtPSK}, "wpa3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ap.SecurityType()
			if got != tt.expected {
				t.Errorf("SecurityType() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestAccessPoint_Band(t *testing.T) {
	tests := []struct {
		freq uint32
		want string
	}{
		{2412, "2.4GHz"},
		{2437, "2.4GHz"},
		{2484, "2.4GHz"},
		{4999, "2.4GHz"},
		{5000, "5GHz"},
		{5180, "5GHz"},
		{5745, "5GHz"},
	}
	for _, tt := range tests {
		ap := AccessPoint{Frequency: tt.freq}
		got := ap.Band()
		if got != tt.want {
			t.Errorf("Band() for %d MHz = %q, want %q", tt.freq, got, tt.want)
		}
	}
}

func TestAccessPoint_SignalDBm(t *testing.T) {
	tests := []struct {
		strength uint8
		wantDBm  int
	}{
		{100, -30}, // strongest
		{50, -60},  // mid
		{0, -90},   // weakest
		{75, -45},
	}
	for _, tt := range tests {
		ap := AccessPoint{Strength: tt.strength}
		got := ap.SignalDBm()
		if got != tt.wantDBm {
			t.Errorf("SignalDBm() for strength=%d = %d, want %d", tt.strength, got, tt.wantDBm)
		}
	}
}

func TestNMDeviceState_IsConnected(t *testing.T) {
	tests := []struct {
		state NMDeviceState
		want  bool
	}{
		{NMDeviceStateActivated, true},
		{NMDeviceStateDisconnected, false},
		{NMDeviceStatePrepare, false},
		{NMDeviceStateConfig, false},
		{NMDeviceStateFailed, false},
		{NMDeviceStateUnknown, false},
	}
	for _, tt := range tests {
		got := tt.state.IsConnected()
		if got != tt.want {
			t.Errorf("NMDeviceState(%d).IsConnected() = %v, want %v", tt.state, got, tt.want)
		}
	}
}

func TestMergeDefaultRouteMetricIgnoresFamiliesWithoutDefaultRoutes(t *testing.T) {
	metric, known := mergeDefaultRouteMetric(0, false, ipConfigSnapshot{
		HasDefaultRoute:  true,
		RouteMetric:      100,
		RouteMetricKnown: true,
	})
	metric, known = mergeDefaultRouteMetric(metric, known, ipConfigSnapshot{
		HasDefaultRoute:  false,
		RouteMetric:      0,
		RouteMetricKnown: true,
	})

	if !known || metric != 100 {
		t.Fatalf("metric = (%d, %v), want (100, true)", metric, known)
	}
}

func TestMergeDefaultRouteMetricKeepsLowestDefaultRouteMetric(t *testing.T) {
	metric, known := mergeDefaultRouteMetric(0, false, ipConfigSnapshot{
		HasDefaultRoute:  true,
		RouteMetric:      100,
		RouteMetricKnown: true,
	})
	metric, known = mergeDefaultRouteMetric(metric, known, ipConfigSnapshot{
		HasDefaultRoute:  true,
		RouteMetric:      50,
		RouteMetricKnown: true,
	})

	if !known || metric != 50 {
		t.Fatalf("metric = (%d, %v), want (50, true)", metric, known)
	}
}

func TestMergeDefaultRouteMetricTreatsMissingMetricAsZero(t *testing.T) {
	metric, known := mergeDefaultRouteMetric(0, false, ipConfigSnapshot{
		HasDefaultRoute: true,
	})

	if !known || metric != 0 {
		t.Fatalf("metric = (%d, %v), want (0, true)", metric, known)
	}
}

func TestObserveDefaultRoutesDoesNotTreatGatewayAsDefaultRoute(t *testing.T) {
	cfg := ipConfigSnapshot{Gateway: "192.168.1.1"}
	observeDefaultRoutes(&cfg, []map[string]dbus.Variant{{
		"dest":   dbus.MakeVariant("192.168.1.0"),
		"prefix": dbus.MakeVariant(uint32(24)),
		"metric": dbus.MakeVariant(uint32(5)),
	}})

	if cfg.HasDefaultRoute || cfg.RouteMetricKnown {
		t.Fatalf("gateway/non-default route marked as default: %+v", cfg)
	}
}

func TestObserveDefaultRoutesMarksDefaultRouteMetric(t *testing.T) {
	cfg := ipConfigSnapshot{Gateway: "192.168.1.1"}
	observeDefaultRoutes(&cfg, []map[string]dbus.Variant{{
		"dest":   dbus.MakeVariant("0.0.0.0"),
		"prefix": dbus.MakeVariant(uint32(0)),
		"metric": dbus.MakeVariant(uint32(50)),
	}})

	if !cfg.HasDefaultRoute || !cfg.RouteMetricKnown || cfg.RouteMetric != 50 {
		t.Fatalf("default route snapshot = %+v, want default metric 50", cfg)
	}
}

func TestStubClient_CallRecording(t *testing.T) {
	stub := NewStubClient()

	stub.WiFiDevices()
	stub.WiFiDevices()
	stub.WirelessEnabled()

	if stub.CallCount("WiFiDevices") != 2 {
		t.Errorf("WiFiDevices call count = %d, want 2", stub.CallCount("WiFiDevices"))
	}
	if stub.CallCount("WirelessEnabled") != 1 {
		t.Errorf("WirelessEnabled call count = %d, want 1", stub.CallCount("WirelessEnabled"))
	}
	if stub.CallCount("Scan") != 0 {
		t.Errorf("Scan call count = %d, want 0", stub.CallCount("Scan"))
	}

	last := stub.LastCall("WiFiDevices")
	if last == nil {
		t.Fatal("LastCall(WiFiDevices) = nil")
	}
	if last.Method != "WiFiDevices" {
		t.Errorf("LastCall method = %q, want WiFiDevices", last.Method)
	}
}

func TestConnectionSnapshot_SSID(t *testing.T) {
	cases := []struct {
		name string
		s    *ConnectionSnapshot
		want string
	}{
		{"nil receiver", nil, ""},
		{"empty settings", &ConnectionSnapshot{Settings: map[string]map[string]dbus.Variant{}}, ""},
		{"missing wifi section",
			&ConnectionSnapshot{Settings: map[string]map[string]dbus.Variant{
				"connection": {"id": dbus.MakeVariant("foo")},
			}}, ""},
		{"non-byte ssid",
			&ConnectionSnapshot{Settings: map[string]map[string]dbus.Variant{
				"802-11-wireless": {"ssid": dbus.MakeVariant("string-ssid")},
			}}, ""},
		{"happy path",
			&ConnectionSnapshot{Settings: map[string]map[string]dbus.Variant{
				"802-11-wireless": {"ssid": dbus.MakeVariant([]byte("HomeNet"))},
			}}, "HomeNet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.SSID(); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestConnectionSnapshot_PSK(t *testing.T) {
	cases := []struct {
		name string
		s    *ConnectionSnapshot
		want string
	}{
		{"nil receiver", nil, ""},
		{"open profile (no security section)",
			&ConnectionSnapshot{Settings: map[string]map[string]dbus.Variant{
				"802-11-wireless": {"ssid": dbus.MakeVariant([]byte("Open"))},
			}}, ""},
		{"security section present, psk missing",
			&ConnectionSnapshot{Settings: map[string]map[string]dbus.Variant{
				"802-11-wireless-security": {"key-mgmt": dbus.MakeVariant("wpa-psk")},
			}}, ""},
		{"non-string psk value",
			&ConnectionSnapshot{Settings: map[string]map[string]dbus.Variant{
				"802-11-wireless-security": {"psk": dbus.MakeVariant([]byte("bytes-not-string"))},
			}}, ""},
		{"happy path",
			&ConnectionSnapshot{Settings: map[string]map[string]dbus.Variant{
				"802-11-wireless-security": {
					"key-mgmt": dbus.MakeVariant("wpa-psk"),
					"psk":      dbus.MakeVariant("secret123"),
				},
			}}, "secret123"},
		{"empty string psk",
			&ConnectionSnapshot{Settings: map[string]map[string]dbus.Variant{
				"802-11-wireless-security": {"psk": dbus.MakeVariant("")},
			}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.PSK(); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

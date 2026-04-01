package nmclient

import "testing"

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

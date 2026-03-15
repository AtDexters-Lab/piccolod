package firewall

import "testing"

func TestStubManager(t *testing.T) {
	stub := &stubManager{}

	t.Run("open_no_error", func(t *testing.T) {
		if err := stub.OpenPort(Rule{Port: 53, Protocol: "udp"}); err != nil {
			t.Fatalf("stub OpenPort should succeed: %v", err)
		}
	})

	t.Run("close_no_error", func(t *testing.T) {
		if err := stub.ClosePort(Rule{Port: 53, Protocol: "udp"}); err != nil {
			t.Fatalf("stub ClosePort should succeed: %v", err)
		}
	})
}

func TestNewFirewalldManager_fallback(t *testing.T) {
	// On dev machines without firewall-cmd, should return a stub
	mgr := NewFirewalldManager()
	if mgr == nil {
		t.Fatal("NewFirewalldManager should never return nil")
	}
	// Both stub and real manager should implement the interface
	var _ Manager = mgr
}

func TestRichRule(t *testing.T) {
	m := &FirewalldManager{zone: "piccolo"}
	rule := m.richRule(Rule{Port: 53, Protocol: "udp"}, "10.0.0.0/8")
	expected := `rule family="ipv4" source address="10.0.0.0/8" port port="53" protocol="udp" accept`
	if rule != expected {
		t.Fatalf("richRule = %q, want %q", rule, expected)
	}
}

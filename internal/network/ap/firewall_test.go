package ap

import (
	"context"
	"strings"
	"testing"

	"piccolod/internal/testutil"
)

func TestFirewall_EnsureZone_AlreadyExists(t *testing.T) {
	fr := &testutil.FakeRunner{}
	fw := &firewallManager{runner: fr}

	// firewall-cmd --info-zone succeeds → zone exists
	if err := fw.ensureZone(context.Background()); err != nil {
		t.Fatalf("ensureZone: %v", err)
	}

	calls := fr.GetCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1 (just the info check)", len(calls))
	}
	if !strings.Contains(calls[0], "--info-zone") {
		t.Fatalf("call = %q, want --info-zone check", calls[0])
	}
}

func TestFirewall_ApplyRules(t *testing.T) {
	fr := &testutil.FakeRunner{}
	fw := &firewallManager{runner: fr}

	if err := fw.applyRules(context.Background()); err != nil {
		t.Fatalf("applyRules: %v", err)
	}

	calls := fr.GetCalls()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3 (dhcp, dns, http)", len(calls))
	}

	wantSubstrings := []string{"--add-service=dhcp", "--add-service=dns", "--add-port=80/tcp"}
	for i, want := range wantSubstrings {
		if !strings.Contains(calls[i], want) {
			t.Errorf("call[%d] = %q, want substring %q", i, calls[i], want)
		}
	}
}

func TestFirewall_AddNATRedirect(t *testing.T) {
	fr := &testutil.FakeRunner{}
	fw := &firewallManager{runner: fr}

	if err := fw.addNATRedirect(context.Background(), 8080); err != nil {
		t.Fatalf("addNATRedirect: %v", err)
	}

	calls := fr.GetCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if !strings.Contains(calls[0], "port=80:proto=tcp:toport=8080") {
		t.Fatalf("call = %q, want NAT redirect rule", calls[0])
	}
}

func TestFirewall_AssignInterface(t *testing.T) {
	fr := &testutil.FakeRunner{}
	fw := &firewallManager{runner: fr}

	if err := fw.assignInterface(context.Background(), "wlan0"); err != nil {
		t.Fatalf("assignInterface: %v", err)
	}

	calls := fr.GetCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if !strings.Contains(calls[0], "--change-interface=wlan0") {
		t.Fatalf("call = %q, want --change-interface=wlan0", calls[0])
	}
}

func TestFirewall_STAValidationLockdown(t *testing.T) {
	fr := &testutil.FakeRunner{}
	fw := &firewallManager{runner: fr}

	if err := fw.applySTAValidationLockdown(context.Background()); err != nil {
		t.Fatalf("applySTAValidationLockdown: %v", err)
	}

	calls := fr.GetCalls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (dhcp + icmp)", len(calls))
	}

	if !strings.Contains(calls[0], "dhcp") {
		t.Errorf("call[0] = %q, want dhcp rule", calls[0])
	}
	if !strings.Contains(calls[1], "icmp") {
		t.Errorf("call[1] = %q, want icmp rule", calls[1])
	}

	// Clean up
	fw.removeSTAValidationLockdown(context.Background())

	allCalls := fr.GetCalls()
	// Should have 4 total: 2 add + 2 remove
	if len(allCalls) != 4 {
		t.Fatalf("total calls = %d, want 4", len(allCalls))
	}
	if !strings.Contains(allCalls[2], "--remove-rich-rule") {
		t.Errorf("call[2] = %q, want --remove-rich-rule", allCalls[2])
	}
}

package mdns

import (
	"strings"
	"testing"
)

func TestNameRegistryAliases(t *testing.T) {
	reg := newNameRegistry("piccolo", "piccolo-abc123")
	reg.SetAliases([]string{"immich", "metrics-immich"})

	names := reg.Names()
	// RFC 20260122 §4.1: 2-level mDNS format uses hyphen separator
	expect := []string{
		"immich-piccolo.local.",
		"metrics-immich-piccolo.local.",
		"piccolo.local.",
		"piccolo-abc123.local.", // specific hostname always published
	}

	for _, name := range expect {
		if !contains(names, name) {
			t.Fatalf("expected %s in advertised names, got %v", name, names)
		}
	}
}

func TestNameRegistrySpecificHostnameAlwaysPublished(t *testing.T) {
	// When baseName differs from specificName, both should be published
	reg := newNameRegistry("piccolo", "piccolo-abc123")
	names := reg.Names()

	if !contains(names, "piccolo.local.") {
		t.Fatalf("base hostname should be published, got %v", names)
	}
	if !contains(names, "piccolo-abc123.local.") {
		t.Fatalf("specific hostname should always be published, got %v", names)
	}

	// After SetBaseName, specific hostname still published
	reg.SetBaseName("piccolo-abc123")
	names = reg.Names()

	// Should still have piccolo-abc123.local (now as baseName, but not duplicated)
	if !contains(names, "piccolo-abc123.local.") {
		t.Fatalf("specific hostname should still be published, got %v", names)
	}
	// Should only have one entry for piccolo-abc123.local (no duplicate)
	count := 0
	for _, n := range names {
		if n == "piccolo-abc123.local." {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("specific hostname should appear exactly once, got %d in %v", count, names)
	}
}

func TestManagerSetHostAliasesFiltersInvalid(t *testing.T) {
	manager := NewManager()
	baseName := manager.baseName // e.g., "piccolo-abc123"

	err := manager.SetHostAliases([]string{"immich", "bad.label", "metrics-immich", "-bad"})
	if err == nil {
		t.Fatalf("expected error for invalid alias labels")
	}

	aliases := manager.HostAliases()
	if contains(aliases, "bad.label") || contains(aliases, "-bad") {
		t.Fatalf("invalid aliases should be filtered, got %v", aliases)
	}
	if !contains(aliases, "immich") || !contains(aliases, "metrics-immich") {
		t.Fatalf("valid aliases missing: %v", aliases)
	}

	fqdns := manager.AdvertisedNames()
	// RFC 20260122 §4.1: 2-level mDNS format uses hyphen separator
	// Now baseName is piccolo-<machineId>, not just "piccolo"
	expect := []string{
		"immich-" + baseName + ".local.",
		"metrics-immich-" + baseName + ".local.",
		baseName + ".local.",
	}
	for _, name := range expect {
		if !contains(fqdns, name) {
			t.Fatalf("expected %s in advertised names, got %v", name, fqdns)
		}
	}
}

// --- onChange callback tests ---

func TestOnChange_FiresOnSetAliases(t *testing.T) {
	reg := newNameRegistry("piccolo", "piccolo-abc123")
	fired := 0
	reg.SetOnChange(func() { fired++ })

	reg.SetAliases([]string{"immich"})
	if fired != 1 {
		t.Fatalf("expected onChange to fire once, got %d", fired)
	}
}

func TestOnChange_DoesNotFireWhenSnapshotUnchanged(t *testing.T) {
	reg := newNameRegistry("piccolo", "piccolo-abc123")
	reg.SetAliases([]string{"immich"})

	fired := 0
	reg.SetOnChange(func() { fired++ })

	// Set same aliases again — snapshot unchanged
	reg.SetAliases([]string{"immich"})
	if fired != 0 {
		t.Fatalf("expected onChange not to fire for identical snapshot, got %d", fired)
	}
}

func TestOnChange_FiresOnGatewayHostname(t *testing.T) {
	reg := newNameRegistry("piccolo-abc123", "piccolo-abc123")
	fired := 0
	reg.SetOnChange(func() { fired++ })

	reg.AddGatewayHostname()
	if fired != 1 {
		t.Fatalf("expected onChange on AddGatewayHostname, got %d", fired)
	}

	reg.RemoveGatewayHostname()
	if fired != 2 {
		t.Fatalf("expected onChange on RemoveGatewayHostname, got %d", fired)
	}
}

func TestOnChange_NilCallbackNoPanic(t *testing.T) {
	reg := newNameRegistry("piccolo", "piccolo-abc123")
	// No callback set — should not panic
	reg.SetAliases([]string{"immich"})
}

func TestOnChange_Deregister(t *testing.T) {
	reg := newNameRegistry("piccolo", "piccolo-abc123")
	fired := 0
	reg.SetOnChange(func() { fired++ })

	reg.SetAliases([]string{"immich"})
	if fired != 1 {
		t.Fatalf("expected 1, got %d", fired)
	}

	reg.SetOnChange(nil) // deregister
	reg.SetAliases([]string{"immich", "blog"})
	if fired != 1 {
		t.Fatalf("expected still 1 after deregister, got %d", fired)
	}
}

func TestTLSHostnames(t *testing.T) {
	t.Run("excludes_gateway_hostname", func(t *testing.T) {
		reg := newNameRegistry("piccolo-abc123", "piccolo-abc123")
		reg.SetAliases([]string{"immich"})
		reg.AddGatewayHostname()

		tlsHosts := reg.TLSHostnames()
		allHosts := reg.Hostnames()

		// piccolo.local must not appear in TLS hostnames
		if contains(tlsHosts, "piccolo.local") {
			t.Fatalf("TLSHostnames() should exclude piccolo.local, got %v", tlsHosts)
		}
		// but it should be in Hostnames()
		if !contains(allHosts, "piccolo.local") {
			t.Fatalf("Hostnames() should include piccolo.local when gateway enabled, got %v", allHosts)
		}
		// other hostnames still present
		if !contains(tlsHosts, "piccolo-abc123.local") {
			t.Fatalf("TLSHostnames() should include base hostname, got %v", tlsHosts)
		}
		if !contains(tlsHosts, "immich-piccolo-abc123.local") {
			t.Fatalf("TLSHostnames() should include alias hostname, got %v", tlsHosts)
		}
	})

	t.Run("baseName_is_piccolo_with_gateway", func(t *testing.T) {
		// When baseName == "piccolo", piccolo.local appears once (deduped in rebuild).
		// TLSHostnames() must still exclude it.
		reg := newNameRegistry("piccolo", "piccolo-abc123")
		reg.AddGatewayHostname()

		tlsHosts := reg.TLSHostnames()

		if contains(tlsHosts, "piccolo.local") {
			t.Fatalf("TLSHostnames() should exclude piccolo.local even when it's the base, got %v", tlsHosts)
		}
		if !contains(tlsHosts, "piccolo-abc123.local") {
			t.Fatalf("TLSHostnames() should include specific hostname, got %v", tlsHosts)
		}
	})

	t.Run("gateway_disabled_matches_hostnames", func(t *testing.T) {
		reg := newNameRegistry("piccolo-abc123", "piccolo-abc123")
		reg.SetAliases([]string{"immich"})

		tlsHosts := reg.TLSHostnames()
		allHosts := reg.Hostnames()

		if len(tlsHosts) != len(allHosts) {
			t.Fatalf("without gateway, TLSHostnames() and Hostnames() should have same count: TLS=%v All=%v", tlsHosts, allHosts)
		}
		// Same set of hostnames (order may differ — TLS puts specific hostname first)
		for _, h := range allHosts {
			if !contains(tlsHosts, h) {
				t.Fatalf("TLSHostnames() missing %q from Hostnames(): TLS=%v All=%v", h, tlsHosts, allHosts)
			}
		}
	})

	t.Run("specific_hostname_first", func(t *testing.T) {
		reg := newNameRegistry("piccolo-abc123", "piccolo-abc123")
		reg.SetAliases([]string{"immich"})
		reg.AddGatewayHostname()

		tlsHosts := reg.TLSHostnames()
		if len(tlsHosts) == 0 {
			t.Fatal("TLSHostnames() should not be empty")
		}
		if tlsHosts[0] != "piccolo-abc123.local" {
			t.Fatalf("TLSHostnames()[0] should be machine-specific hostname, got %q (all: %v)", tlsHosts[0], tlsHosts)
		}
	})
}

func contains(list []string, target string) bool {
	for _, item := range list {
		if strings.EqualFold(item, target) {
			return true
		}
	}
	return false
}

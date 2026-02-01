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

	// After conflict resolution, baseName changes but specific stays
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

func contains(list []string, target string) bool {
	for _, item := range list {
		if strings.EqualFold(item, target) {
			return true
		}
	}
	return false
}

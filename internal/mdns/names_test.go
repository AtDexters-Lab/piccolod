package mdns

import (
	"strings"
	"testing"
)

func TestNameRegistryAliases(t *testing.T) {
	reg := newNameRegistry("piccolo")
	reg.SetAliases([]string{"immich", "metrics-immich"})

	names := reg.Names()
	expect := []string{
		"immich.piccolo.local.",
		"metrics-immich.piccolo.local.",
		"piccolo.local.",
	}

	for _, name := range expect {
		if !contains(names, name) {
			t.Fatalf("expected %s in advertised names, got %v", name, names)
		}
	}
}

func TestManagerSetHostAliasesFiltersInvalid(t *testing.T) {
	manager := NewManager()

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
	expect := []string{
		"immich.piccolo.local.",
		"metrics-immich.piccolo.local.",
		"piccolo.local.",
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

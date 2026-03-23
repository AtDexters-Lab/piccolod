package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadStateCache_Missing(t *testing.T) {
	sc, err := loadStateCache(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if sc.CustomHostname != "" || len(sc.AliasDomains) != 0 {
		t.Fatal("expected zero-value cache for missing file")
	}
}

func TestSaveAndLoadStateCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state_cache.json")
	sc := StateCache{
		CustomHostname: "mydevice",
		AliasDomains:   []string{"app.example.com", "web.example.com"},
	}
	if err := saveStateCache(path, sc); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadStateCache(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.CustomHostname != "mydevice" {
		t.Errorf("custom_hostname = %q, want %q", loaded.CustomHostname, "mydevice")
	}
	if len(loaded.AliasDomains) != 2 {
		t.Errorf("alias_domains len = %d, want 2", len(loaded.AliasDomains))
	}
	if loaded.LastSyncedAt == "" {
		t.Error("expected last_synced_at to be set")
	}
}

func TestStateCacheChanged(t *testing.T) {
	tests := []struct {
		name    string
		old     StateCache
		new     StateCache
		changed bool
	}{
		{
			"identical",
			StateCache{CustomHostname: "a", AliasDomains: []string{"x.com"}},
			StateCache{CustomHostname: "a", AliasDomains: []string{"x.com"}},
			false,
		},
		{
			"same domains different order",
			StateCache{AliasDomains: []string{"b.com", "a.com"}},
			StateCache{AliasDomains: []string{"a.com", "b.com"}},
			false,
		},
		{
			"hostname changed",
			StateCache{CustomHostname: "a"},
			StateCache{CustomHostname: "b"},
			true,
		},
		{
			"domain added",
			StateCache{AliasDomains: []string{"a.com"}},
			StateCache{AliasDomains: []string{"a.com", "b.com"}},
			true,
		},
		{
			"domain removed",
			StateCache{AliasDomains: []string{"a.com", "b.com"}},
			StateCache{AliasDomains: []string{"a.com"}},
			true,
		},
		{
			"both empty",
			StateCache{},
			StateCache{},
			false,
		},
		{
			"ignores LastSyncedAt",
			StateCache{CustomHostname: "a", LastSyncedAt: "2026-01-01T00:00:00Z"},
			StateCache{CustomHostname: "a", LastSyncedAt: "2026-03-23T00:00:00Z"},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stateCacheChanged(tt.old, tt.new)
			if got != tt.changed {
				t.Errorf("stateCacheChanged() = %v, want %v", got, tt.changed)
			}
		})
	}
}

func TestLoadStateCache_Corrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state_cache.json")
	os.WriteFile(path, []byte("not json"), 0o600)

	_, err := loadStateCache(path)
	if err == nil {
		t.Fatal("expected error for corrupt file")
	}
}

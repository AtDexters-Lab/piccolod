package identity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"piccolod/internal/fsutil"
)

// StateCache holds recoverable device state cached locally.
// Synced from GET /devices/me responses; replayed after re-enrollment.
// Stored at {coreRoot}/network-bootstrap/remote/state_cache.json (sibling of identity.json).
type StateCache struct {
	CustomHostname string   `json:"custom_hostname,omitempty"`
	AliasDomains   []string `json:"alias_domains,omitempty"`
	LastSyncedAt   string   `json:"last_synced_at,omitempty"`
}

func loadStateCache(path string) (StateCache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StateCache{}, nil
		}
		return StateCache{}, err
	}
	var sc StateCache
	if err := json.Unmarshal(data, &sc); err != nil {
		return StateCache{}, err
	}
	return sc, nil
}

func saveStateCache(path string, sc StateCache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	sc.LastSyncedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(&sc, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, data, 0o600)
}

// stateCacheChanged compares two caches by their meaningful fields
// (CustomHostname + sorted AliasDomains). LastSyncedAt is ignored.
func stateCacheChanged(old, new StateCache) bool {
	if old.CustomHostname != new.CustomHostname {
		return true
	}
	oldDomains := append([]string(nil), old.AliasDomains...)
	newDomains := append([]string(nil), new.AliasDomains...)
	sort.Strings(oldDomains)
	sort.Strings(newDomains)
	return !slices.Equal(oldDomains, newDomains)
}

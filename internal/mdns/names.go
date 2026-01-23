package mdns

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/miekg/dns"
)

const localTLD = "local"

// NameRegistry tracks the base hostname and per-app alias labels.
type NameRegistry struct {
	mu       sync.RWMutex
	baseName string
	aliases  map[string]struct{}
	fqdns    map[string]struct{}
	snapshot []string
}

func newNameRegistry(base string) *NameRegistry {
	reg := &NameRegistry{
		baseName: base,
		aliases:  make(map[string]struct{}),
	}
	reg.rebuildLocked()
	return reg
}

func (r *NameRegistry) BaseName() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.baseName
}

func (r *NameRegistry) Hostname() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.baseName + "." + localTLD
}

// Names returns the current advertised FQDNs (with trailing dot).
func (r *NameRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.snapshot))
	copy(out, r.snapshot)
	return out
}

// AliasLabels returns the alias labels (not FQDNs).
func (r *NameRegistry) AliasLabels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.aliases))
	for label := range r.aliases {
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

// MatchName reports whether a DNS name matches one of the advertised FQDNs.
func (r *NameRegistry) MatchName(name string) bool {
	key := normalizeFQDN(name)
	if key == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.fqdns[key]
	return ok
}

// SetBaseName updates the base hostname and rebuilds derived FQDNs.
func (r *NameRegistry) SetBaseName(base string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.baseName = base
	r.rebuildLocked()
}

// SetAliases replaces the alias set with the provided labels.
func (r *NameRegistry) SetAliases(labels []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aliases = make(map[string]struct{}, len(labels))
	for _, label := range labels {
		if label == "" || label == r.baseName {
			continue
		}
		r.aliases[label] = struct{}{}
	}
	r.rebuildLocked()
}

func (r *NameRegistry) rebuildLocked() {
	fqdns := make(map[string]struct{}, len(r.aliases)+1)
	snapshot := make([]string, 0, len(r.aliases)+1)

	baseFQDN := r.baseName + "." + localTLD + "."
	fqdns[baseFQDN] = struct{}{}
	snapshot = append(snapshot, baseFQDN)

	// RFC 20260122 §4.1: 2-level mDNS format uses hyphen separator
	// Before: <label>.<baseName>.<localTLD>. (e.g., immich.piccolo.local.)
	// After:  <label>-<baseName>.<localTLD>. (e.g., immich-piccolo.local.)
	for label := range r.aliases {
		combined := label + "-" + r.baseName
		if len(combined) > 63 {
			log.Printf("[mdns] skipping alias %q: combined label %q exceeds 63-char DNS limit (%d)", label, combined, len(combined))
			continue
		}
		name := combined + "." + localTLD + "."
		fqdns[name] = struct{}{}
		snapshot = append(snapshot, name)
	}

	sort.Strings(snapshot)
	r.fqdns = fqdns
	r.snapshot = snapshot
}

func normalizeFQDN(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return strings.ToLower(dns.Fqdn(name))
}

func normalizeLabel(label string) (string, error) {
	label = strings.ToLower(strings.TrimSpace(label))
	if label == "" {
		return "", fmt.Errorf("label is empty")
	}
	if strings.Contains(label, ".") {
		return "", fmt.Errorf("label must not contain '.'")
	}
	if len(label) > 63 {
		return "", fmt.Errorf("label too long")
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return "", fmt.Errorf("label must not start or end with '-'")
	}
	for i := 0; i < len(label); i++ {
		ch := label[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
			continue
		}
		return "", fmt.Errorf("invalid character in label")
	}
	return label, nil
}

func normalizeLabels(labels []string) (valid []string, invalid []string) {
	seen := make(map[string]struct{}, len(labels))
	for _, raw := range labels {
		label, err := normalizeLabel(raw)
		if err != nil {
			if strings.TrimSpace(raw) != "" {
				invalid = append(invalid, strings.TrimSpace(raw))
			}
			continue
		}
		if _, exists := seen[label]; exists {
			continue
		}
		seen[label] = struct{}{}
		valid = append(valid, label)
	}
	sort.Strings(valid)
	sort.Strings(invalid)
	return valid, invalid
}

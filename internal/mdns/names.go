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
	mu              sync.RWMutex
	baseName        string            // Current base name (may be "piccolo" or "piccolo-<machineId>" after conflict)
	specificName    string            // Always "piccolo-<machineId>" - published regardless of conflict state
	aliases         map[string]struct{}
	fqdns           map[string]struct{}
	snapshot        []string
	includeGateway  bool              // Whether to include piccolo.local (gateway leader)
}

func newNameRegistry(base, specificName string) *NameRegistry {
	reg := &NameRegistry{
		baseName:     base,
		specificName: specificName,
		aliases:      make(map[string]struct{}),
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

// Hostnames returns all advertised hostnames without trailing dots.
// Suitable for TLS certificate SANs.
func (r *NameRegistry) Hostnames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.snapshot))
	for _, fqdn := range r.snapshot {
		host := strings.TrimSuffix(fqdn, ".")
		out = append(out, host)
	}
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

// AddGatewayHostname adds piccolo.local to the advertised hostnames.
// This is called when the device becomes the gateway leader.
func (r *NameRegistry) AddGatewayHostname() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.includeGateway {
		return // Already included
	}
	r.includeGateway = true
	r.rebuildLocked()
	log.Printf("[mdns] Gateway hostname added: piccolo.local")
}

// RemoveGatewayHostname removes piccolo.local from the advertised hostnames.
// This is called when the device yields gateway leadership.
func (r *NameRegistry) RemoveGatewayHostname() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.includeGateway {
		return // Already excluded
	}
	r.includeGateway = false
	r.rebuildLocked()
	log.Printf("[mdns] Gateway hostname removed: piccolo.local")
}

// IncludesGateway returns true if piccolo.local is currently advertised.
func (r *NameRegistry) IncludesGateway() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.includeGateway
}

func (r *NameRegistry) rebuildLocked() {
	capacity := len(r.aliases) + 3 // base + specific + possible gateway + aliases
	fqdns := make(map[string]struct{}, capacity)
	snapshot := make([]string, 0, capacity)

	// Primary hostname based on conflict state (may be "piccolo" or "piccolo-<machineId>")
	baseFQDN := r.baseName + "." + localTLD + "."
	fqdns[baseFQDN] = struct{}{}
	snapshot = append(snapshot, baseFQDN)

	// Specific hostname: always publish piccolo-<machineId>.local regardless of conflict state
	// This ensures the device is always reachable via its unique hostname
	if r.specificName != "" && r.specificName != r.baseName {
		specificFQDN := r.specificName + "." + localTLD + "."
		if _, exists := fqdns[specificFQDN]; !exists {
			fqdns[specificFQDN] = struct{}{}
			snapshot = append(snapshot, specificFQDN)
		}
	}

	// Gateway hostname: piccolo.local (only when leader)
	if r.includeGateway {
		gatewayFQDN := "piccolo." + localTLD + "."
		// Only add if not already the base (avoid duplicate if base is "piccolo")
		if _, exists := fqdns[gatewayFQDN]; !exists {
			fqdns[gatewayFQDN] = struct{}{}
			snapshot = append(snapshot, gatewayFQDN)
		}
	}

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

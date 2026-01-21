// Package hostname provides validation and hostname derivation for RFC 20260114.
// It implements the unified hostname scheme for HTTP/WebSocket listeners:
// - Primary listener: <app>.<base>
// - Additional listeners: <listener>-<app>.<base>
package hostname

import (
	"fmt"
	"regexp"
	"strings"

	"piccolod/internal/api"
)

// appNameRegex validates app names per RFC 20260114 Section 4.4 and specification.yaml.
// Must start with a letter, lowercase letters and numbers only (NO hyphens), 1-31 chars.
var appNameRegex = regexp.MustCompile(`^[a-z][a-z0-9]{0,30}$`)

// ReservedAppNames are names that cannot be used as app names.
// Per specification.yaml: api, www, admin, root, system, piccolo, piccoloos
var ReservedAppNames = []string{"api", "www", "admin", "root", "system", "piccolo", "piccoloos"}

// ReservedListenerNames are names that cannot be used as listener names.
// Per specification.yaml: piccolo, piccoloos
var ReservedListenerNames = []string{"piccolo", "piccoloos"}

// ValidateAppName validates an app name per RFC 20260114 Section 4.4.
// Rules:
// - Must start with a letter
// - Lowercase letters and numbers only (NO hyphens)
// - Length 1-31 characters
// - Cannot be a reserved name
func ValidateAppName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}

	if len(name) > 31 {
		return fmt.Errorf("name must be 31 characters or less")
	}

	if !appNameRegex.MatchString(name) {
		return fmt.Errorf("name must contain only lowercase letters and numbers, and must start with a letter (no hyphens allowed)")
	}

	for _, r := range ReservedAppNames {
		if name == r {
			return fmt.Errorf("name '%s' is reserved", name)
		}
	}

	return nil
}

// ValidateListenerName validates a listener name per specification.yaml.
// Same rules as app name: 1-31 chars, [a-z][a-z0-9]*, starts with letter.
func ValidateListenerName(name string) error {
	if name == "" {
		return fmt.Errorf("listener name is required")
	}

	if len(name) > 31 {
		return fmt.Errorf("listener name must be 31 characters or less")
	}

	if !appNameRegex.MatchString(name) {
		return fmt.Errorf("listener name must contain only lowercase letters and numbers, and must start with a letter (no hyphens allowed)")
	}

	for _, r := range ReservedListenerNames {
		if name == r {
			return fmt.Errorf("listener name '%s' is reserved", name)
		}
	}

	return nil
}

// DeriveHostLabel returns the host label for a listener.
// - Primary HTTP/WS listener: returns app name (e.g., "immich")
// - Non-primary HTTP/WS listener: returns "listener-app" (e.g., "metrics-immich")
// - Raw/TLS listeners (eligible=false): returns "" (no host-based routing)
//
// The eligible parameter indicates whether the listener can have host-based routing.
// Callers should use services.IsEligibleForHostRouting to compute this value.
func DeriveHostLabel(app, listener string, primary, eligible bool) string {
	if !eligible {
		return ""
	}

	if primary {
		return app
	}
	return listener + "-" + app
}

// CheckCollisions validates that no host labels collide across:
// - Other apps' host labels
// - Reserved names
//
// The existingLabels map is keyed by host label, value is the owner (e.g., "app:myapp" or "listener:myapp/web").
// Returns an error if any newLabel collides with an existing label or reserved name.
func CheckCollisions(newLabels []string, existingLabels map[string]string) error {
	// Check against reserved names
	reserved := make(map[string]struct{})
	for _, r := range ReservedAppNames {
		reserved[r] = struct{}{}
	}
	for _, r := range ReservedListenerNames {
		reserved[r] = struct{}{}
	}

	for _, label := range newLabels {
		if _, ok := reserved[label]; ok {
			return fmt.Errorf("host label '%s' collides with reserved name", label)
		}
		if owner, ok := existingLabels[label]; ok {
			return fmt.Errorf("host label '%s' collides with existing %s", label, owner)
		}
	}

	return nil
}

// ResolvePrimaryListener determines which listener should be the primary listener.
// Returns the primary listener name and any validation error.
//
// Rules:
// - If a listener has Primary=true, it becomes primary (only one allowed)
// - Primary=true is not allowed on flow:tls or protocol:raw listeners
// - If no explicit primary, the first HTTP/WS listener becomes primary
// - Returns "" if there are no eligible primary listeners
func ResolvePrimaryListener(listeners []api.AppListener) (string, error) {
	var explicit string
	for _, l := range listeners {
		if l.Primary {
			if explicit != "" {
				return "", fmt.Errorf("multiple primary listeners: '%s' and '%s'", explicit, l.Name)
			}
			if l.Flow == api.FlowTLS {
				return "", fmt.Errorf("primary not allowed on flow:tls listener '%s'", l.Name)
			}
			if l.Protocol == api.ListenerProtocolRaw {
				return "", fmt.Errorf("primary not allowed on protocol:raw listener '%s'", l.Name)
			}
			explicit = l.Name
		}
	}
	if explicit != "" {
		return explicit, nil
	}

	// First HTTP/WebSocket listener becomes primary
	// (flow:tcp + protocol:http|websocket are eligible for host-based routing)
	for _, l := range listeners {
		if l.Flow != api.FlowTLS && (l.Protocol == api.ListenerProtocolHTTP || l.Protocol == api.ListenerProtocolWebsocket) {
			return l.Name, nil
		}
	}

	return "", nil // No eligible primary
}

// NormalizeHostLabel extracts the host label from a full hostname.
// Given "immich.piccolo.local" and base "piccolo.local", returns "immich".
// Given "metrics-immich.piccolo.local" and base "piccolo.local", returns "metrics-immich".
// Returns empty string if the hostname doesn't match the base.
func NormalizeHostLabel(hostname, base string) string {
	hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	base = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(base)), ".")

	if hostname == base {
		return ""
	}

	suffix := "." + base
	if !strings.HasSuffix(hostname, suffix) {
		return ""
	}

	return strings.TrimSuffix(hostname, suffix)
}

// ParseHostLabel parses a host label into its components.
// "immich" returns (app="immich", listener="", isPrimary=true)
// "metrics-immich" returns (app="immich", listener="metrics", isPrimary=false)
// Note: This is a best-effort parse; actual validation should use the service registry.
func ParseHostLabel(label string) (app, listener string, isPrimary bool) {
	if label == "" {
		return "", "", false
	}

	idx := strings.Index(label, "-")
	if idx == -1 {
		// No hyphen means it's a primary label (app name only)
		return label, "", true
	}

	// Has hyphen: listener-app format
	return label[idx+1:], label[:idx], false
}

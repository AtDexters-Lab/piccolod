// Package hostname provides validation and hostname derivation for RFC 20260114, RFC 20260122 & RFC 20260130.
// It implements the unified hostname scheme for HTTP/WebSocket listeners:
// - Primary listener (LAN): <listener>-<base> (2-level mDNS format per RFC 20260122, RFC 20260130)
// - Additional listeners (LAN): <listener>-<app>-<base>
// - Remote access continues to use subdomain format: <listener>.<portal-base> for primary
package hostname

import (
	"fmt"
	"regexp"
	"strings"

	"piccolod/internal/api"
)

// appNameRegex validates app names per RFC 20260122 Section 4.3.
// Must start with a letter, lowercase letters and numbers only (NO hyphens), 1-16 chars.
// Rationale for 16-char limit: With 2-level format <listener>-<app>-<base>.local,
// worst case is 16+1+16+1+16 = 50 chars for the left-most label, under DNS 63-char limit.
var appNameRegex = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)

// ReservedNames are names that cannot be used as app names, primary listener names, or workspace names.
// Non-primary listener names are exempt (their derived hostname <listener>-<app> cannot collide).
// Per RFC 20260130, all identity-level identifiers share the same reserved list.
// __primary is the magic listener name marker and must not be used as an actual name.
var ReservedNames = []string{"api", "www", "admin", "root", "system", "piccolo", "piccoloos", "__primary"}

// ReservedAppNames is kept for backward compatibility but points to ReservedNames.
// Deprecated: Use ReservedNames instead.
var ReservedAppNames = ReservedNames

// ReservedListenerNames is kept for backward compatibility but points to ReservedNames.
// Deprecated: Use ReservedNames instead.
var ReservedListenerNames = ReservedNames

// PrimaryListenerMarker is the magic listener name that indicates user input is required.
// Per RFC 20260130, this marker is replaced with the user-provided value at install time.
const PrimaryListenerMarker = "__primary"

// Normalize lowercases, trims whitespace, and strips a trailing dot from a hostname.
func Normalize(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// IsValidDNSLabel reports whether label is a valid single DNS label
// (1-63 chars, ASCII letters/digits/hyphens, no leading/trailing hyphen).
// Mixed case is accepted; the function lowercases internally.
func IsValidDNSLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	label = strings.ToLower(label)
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		ch := label[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
			continue
		}
		return false
	}
	return true
}

// ValidateAppName validates an app name per RFC 20260122 Section 4.3 and RFC 20260130.
// Rules:
// - Must start with a letter
// - Lowercase letters and numbers only (NO hyphens)
// - Length 1-16 characters (to ensure derived hostnames stay under DNS 63-char limit)
// - Cannot be a reserved name
func ValidateAppName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}

	if len(name) > 16 {
		return fmt.Errorf("name must be 16 characters or less")
	}

	if !appNameRegex.MatchString(name) {
		return fmt.Errorf("name must contain only lowercase letters and numbers, and must start with a letter (no hyphens allowed)")
	}

	for _, r := range ReservedNames {
		if name == r {
			return fmt.Errorf("name '%s' is reserved", name)
		}
	}

	return nil
}

// ValidateListenerNameFormat validates listener name format only (regex + length),
// without checking against reserved names. Use this for non-primary listeners whose
// derived hostname <listener>-<app> cannot collide with bare reserved names.
//
// Note: The magic marker "__primary" is exempt from this validation during initial parsing.
// Callers should use IsPrimaryMarker() to check before calling this function.
func ValidateListenerNameFormat(name string) error {
	if name == "" {
		return fmt.Errorf("listener name is required")
	}

	if len(name) > 16 {
		return fmt.Errorf("listener name must be 16 characters or less")
	}

	if !appNameRegex.MatchString(name) {
		return fmt.Errorf("listener name must contain only lowercase letters and numbers, and must start with a letter (no hyphens allowed)")
	}

	return nil
}

// ValidateListenerName validates a listener name for use as a primary listener.
// Checks format (regex + length) and rejects reserved names, since a primary
// listener's name becomes the app identity (e.g., "admin" → "admin-piccolo.local").
//
// Note: The magic marker "__primary" is exempt from this validation during initial parsing.
// Callers should use IsPrimaryMarker() to check before calling this function.
func ValidateListenerName(name string) error {
	if err := ValidateListenerNameFormat(name); err != nil {
		return err
	}

	for _, r := range ReservedNames {
		if name == r {
			return fmt.Errorf("listener name '%s' is reserved", name)
		}
	}

	return nil
}

// IsPrimaryMarker returns true if the name is the __primary magic marker.
// Per RFC 20260130, this marker is exempt from normal validation during initial parsing.
func IsPrimaryMarker(name string) bool {
	return name == PrimaryListenerMarker
}

// ValidateWorkspaceName validates a workspace_name per RFC 20260130.
// Same rules as app/listener names: 1-16 chars, [a-z][a-z0-9]*, starts with letter, no hyphens.
func ValidateWorkspaceName(name string) error {
	if name == "" {
		return fmt.Errorf("workspace_name is required")
	}

	if len(name) > 16 {
		return fmt.Errorf("workspace_name must be 16 characters or less")
	}

	if !appNameRegex.MatchString(name) {
		return fmt.Errorf("workspace_name must contain only lowercase letters and numbers, and must start with a letter (no hyphens allowed)")
	}

	for _, r := range ReservedNames {
		if name == r {
			return fmt.Errorf("workspace_name '%s' is reserved", name)
		}
	}

	return nil
}

// DeriveHostLabel returns the host label for a listener.
// Per RFC 20260130:
// - Primary HTTP/WS listener: returns listener name (e.g., "blog")
// - Non-primary HTTP/WS listener: returns "listener-app" (e.g., "api-blog")
// - Raw/TLS listeners (eligible=false): returns "" (no host-based routing)
//
// The eligible parameter indicates whether the listener can have host-based routing.
// Callers should use services.IsEligibleForHostRouting to compute this value.
//
// Note: For apps installed with listeners, instanceID = primary listener name,
// so for primary listeners: app == listener and the result is the same.
// For workspace apps that later add listeners, app (instanceID) may differ from listener name.
func DeriveHostLabel(app, listener string, primary, eligible bool) string {
	if !eligible {
		return ""
	}

	if primary {
		return listener
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
	// Check against reserved names (unified list per RFC 20260130)
	reserved := make(map[string]struct{})
	for _, r := range ReservedNames {
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
// - Primary=true is not allowed on flow:tcp + protocol:raw (no host routing)
// - If no explicit primary, the first eligible listener becomes primary
//   (any listener except flow:tcp + protocol:raw)
// - Returns "" if there are no eligible primary listeners
func ResolvePrimaryListener(listeners []api.AppListener) (string, error) {
	var explicit string
	for _, l := range listeners {
		if l.Primary {
			if explicit != "" {
				return "", fmt.Errorf("multiple primary listeners: '%s' and '%s'", explicit, l.Name)
			}
			if l.Flow == api.FlowTCP && l.Protocol == api.ListenerProtocolRaw {
				return "", fmt.Errorf("primary not allowed on protocol:raw with flow:tcp listener '%s'", l.Name)
			}
			explicit = l.Name
		}
	}
	if explicit != "" {
		return explicit, nil
	}

	// Auto-select: first eligible listener becomes primary.
	// Skip flow:tcp + protocol:raw (not eligible for host-based routing).
	// All other combinations are allowed per RFC 20260316.
	for _, l := range listeners {
		if l.Flow == api.FlowTCP && l.Protocol == api.ListenerProtocolRaw {
			continue
		}
		return l.Name, nil
	}

	return "", nil // No eligible primary
}

// NormalizeHostLabel extracts the host label from a full hostname.
// Uses 2-level mDNS format per RFC 20260122: <app>-<base>.local
// Given "immich-piccolo.local" and base "piccolo.local", returns "immich".
// Given "metrics-immich-piccolo.local" and base "piccolo.local", returns "metrics-immich".
// Returns empty string if the hostname doesn't match the base.
func NormalizeHostLabel(hostname, base string) string {
	hostname = Normalize(hostname)
	base = Normalize(base)

	if hostname == base {
		return ""
	}

	// RFC 20260122 §4.4: 2-level format uses hyphen separator
	suffix := "-" + base
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

// DeriveLANHostname constructs a 2-level mDNS hostname per RFC 20260122.
// Given hostLabel "immich" and base "piccolo.local", returns "immich-piccolo.local".
// Given hostLabel "metrics-immich" and base "piccolo.local", returns "metrics-immich-piccolo.local".
func DeriveLANHostname(hostLabel, base string) string {
	if hostLabel == "" {
		return base
	}
	base = Normalize(base)
	return strings.ToLower(hostLabel) + "-" + base
}

// ValidateDerivedHostname validates that a derived hostname's left-most DNS label
// does not exceed 63 characters (DNS limit). Per RFC 20260122 §4.3, this is defense in depth.
func ValidateDerivedHostname(hostname string) error {
	hostname = Normalize(hostname)
	if hostname == "" {
		return nil
	}

	// Extract left-most label (before first dot)
	idx := strings.Index(hostname, ".")
	var leftmostLabel string
	if idx == -1 {
		leftmostLabel = hostname
	} else {
		leftmostLabel = hostname[:idx]
	}

	if len(leftmostLabel) > 63 {
		return fmt.Errorf("derived hostname left-most label exceeds 63 characters: %d", len(leftmostLabel))
	}

	return nil
}

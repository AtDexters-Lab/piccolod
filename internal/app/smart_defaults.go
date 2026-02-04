package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"piccolod/internal/api"
	"piccolod/internal/hostname"
)

// GenerateSecurePassword generates a random secure string (32 bytes, base64 encoded)
func GenerateSecurePassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// IsSubdomainTaken checks if a subdomain is already in use by any app listener.
// It iterates through all installed apps and checks their listener definitions.
func IsSubdomainTaken(ctx context.Context, mgr *AppManager, subdomain string) (bool, error) {
	apps, err := mgr.List(ctx)
	if err != nil {
		return false, err
	}

	for _, app := range apps {
		// We need the definition to see listener names.
		// AppManager now needs to expose GetAppDefinition or we use a helper.
		def, err := mgr.GetAppDefinition(ctx, app.InstanceID)
		if err != nil {
			// If we can't read an app's definition, we assume it's not conflicting
			// (or we could be conservative and say it is, but let's be optimistic).
			continue
		}

		for _, l := range def.Listeners {
			if l.Name == subdomain {
				return true, nil
			}
		}
	}
	return false, nil
}

// IsInstanceIDTaken checks if a proposed instance ID is already in use by any app.
func IsInstanceIDTaken(ctx context.Context, mgr *AppManager, id string) (bool, error) {
	apps, err := mgr.List(ctx)
	if err != nil {
		return false, err
	}
	for _, a := range apps {
		if a.InstanceID == id {
			return true, nil
		}
	}
	return false, nil
}

// nameVariants returns the base name followed by base1, base2, ..., base99.
// The base is truncated so that the suffixed form never exceeds 16 characters
// (the instance ID / listener name limit).
func nameVariants(base string) []string {
	const maxLen = 16
	if len(base) > maxLen {
		base = base[:maxLen]
	}
	variants := []string{base}
	for i := 1; i < 100; i++ {
		suffix := fmt.Sprintf("%d", i)
		name := base
		if len(name)+len(suffix) > maxLen {
			name = name[:maxLen-len(suffix)]
		}
		variants = append(variants, name+suffix)
	}
	return variants
}

// FindFreeName checks both subdomain availability and instance-ID availability for each
// candidate in a single loop. This ensures the chosen name is free in both namespaces.
func FindFreeName(ctx context.Context, mgr *AppManager, base string) (string, error) {
	for _, candidate := range nameVariants(base) {
		subTaken, err := IsSubdomainTaken(ctx, mgr, candidate)
		if err != nil {
			return base, err
		}
		idTaken, err := IsInstanceIDTaken(ctx, mgr, candidate)
		if err != nil {
			return base, err
		}
		if !subTaken && !idTaken {
			return candidate, nil
		}
	}
	return base, fmt.Errorf("could not find free name for %s", base)
}

// PrepareSmartDefaults modifies the input schema with generated values and collision-free defaults.
// This is called by the API handler before sending the schema to the UI.
// catalogItemName is optional - if provided, it's used as the default for __app_address__.
func PrepareSmartDefaults(ctx context.Context, mgr *AppManager, schema *api.AppDefinition, catalogItemName string) error {
	// RFC 20260130: Detect __primary listener and inject __app_address__ synthetic input
	hasPrimaryMarker := detectPrimaryMarker(schema.Listeners)
	if hasPrimaryMarker {
		if schema.Inputs == nil {
			schema.Inputs = make(map[string]api.AppInput)
		}
		// Inject __app_address__ input if not already present
		if _, exists := schema.Inputs["__app_address__"]; !exists {
			// Use catalog item name as default, sanitized for hostname rules
			defaultName := sanitizeForHostname(catalogItemName)
			if defaultName == "" {
				defaultName = "app"
			}
			schema.Inputs["__app_address__"] = api.AppInput{
				Type:        "string",
				Label:       "App Address",
				Description: "The subdomain/address for this app (e.g., 'blog' for blog-piccolo.local)",
				Required:    true,
				Default:     defaultName,
				Validation: &api.AppInputValidation{
					Regex:   "^[a-z][a-z0-9]{0,15}$",
					Message: "Lowercase letters and numbers only, must start with letter, max 16 chars",
				},
			}
		}
	}

	// Workspace-mode apps without listeners: inject __app_address__ for workspace naming.
	// Evolved workspaces (workspace apps that gained listeners) are excluded — the
	// __primary listener substitution above handles them.
	if !hasPrimaryMarker && len(schema.Listeners) == 0 &&
		piccoloModeFromExtensions(schema.Extensions) == ModeWorkspace {
		if schema.Inputs == nil {
			schema.Inputs = make(map[string]api.AppInput)
		}
		if _, exists := schema.Inputs["__app_address__"]; !exists {
			defaultName := sanitizeForHostname(catalogItemName)
			if defaultName == "" {
				defaultName = "workspace"
			}
			schema.Inputs["__app_address__"] = api.AppInput{
				Type:        "string",
				Label:       "Workspace Name",
				Description: "A unique name for this workspace instance",
				Required:    true,
				Default:     defaultName,
				Validation: &api.AppInputValidation{
					Regex:   "^[a-z][a-z0-9]{0,15}$",
					Message: "Lowercase letters and numbers only, start with letter, max 16 chars",
				},
			}
		}
	}

	if schema.Inputs == nil {
		return nil
	}

	for key, input := range schema.Inputs {
		// 1. Password Generation
		if input.Type == "password" && input.Generate {
			pwd, err := GenerateSecurePassword()
			if err == nil {
				input.Default = pwd
				schema.Inputs[key] = input
			}
		}

		// 2. Name Collision (for __app_address__ or legacy subdomain inputs)
		if key == "subdomain" || key == "__app_address__" {
			if val, ok := input.Default.(string); ok && val != "" {
				freeName, err := FindFreeName(ctx, mgr, val)
				if err == nil {
					input.Default = freeName
					schema.Inputs[key] = input
				}
			}
		}
	}
	return nil
}

// detectPrimaryMarker checks if any listener is named __primary.
func detectPrimaryMarker(listeners []api.AppListener) bool {
	for _, l := range listeners {
		if hostname.IsPrimaryMarker(l.Name) {
			return true
		}
	}
	return false
}

// sanitizeForHostname converts a string to a valid hostname component.
// Removes hyphens, underscores, converts to lowercase, keeps only letters and numbers,
// ensures it starts with a letter, and truncates to 16 characters.
func sanitizeForHostname(s string) string {
	if s == "" {
		return ""
	}

	// Convert to lowercase and remove hyphens/underscores
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			result = append(result, c+'a'-'A')
		} else if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result = append(result, c)
		}
		// Skip hyphens, underscores, and other characters
	}

	// Ensure starts with a letter
	for len(result) > 0 && result[0] >= '0' && result[0] <= '9' {
		result = result[1:]
	}

	// Truncate to 16 characters
	if len(result) > 16 {
		result = result[:16]
	}

	return string(result)
}

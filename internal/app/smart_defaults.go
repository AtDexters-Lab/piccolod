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

// FindFreeSubdomain tries the base name, then appends 1, 2, etc. until a free one is found.
// RFC 20260130: Suffixes use no hyphen (e.g., "blog1", "blog2") since hyphens are not allowed
// in listener names and primary listener names.
func FindFreeSubdomain(ctx context.Context, mgr *AppManager, base string) (string, error) {
	taken, err := IsSubdomainTaken(ctx, mgr, base)
	if err != nil {
		return base, err
	}
	if !taken {
		return base, nil
	}

	for i := 1; i < 100; i++ {
		candidate := fmt.Sprintf("%s%d", base, i)
		taken, err := IsSubdomainTaken(ctx, mgr, candidate)
		if err != nil {
			return base, err
		}
		if !taken {
			return candidate, nil
		}
	}
	return base, fmt.Errorf("could not find free subdomain for %s", base)
}

// PrepareSmartDefaults modifies the input schema with generated values and collision-free defaults.
// This is called by the API handler before sending the schema to the UI.
// catalogItemName is optional - if provided, it's used as the default for __app_address__.
func PrepareSmartDefaults(ctx context.Context, mgr *AppManager, schema *api.AppDefinition, catalogItemName string) error {
	// RFC 20260130: Detect __primary listener and inject __app_address__ synthetic input
	if hasPrimaryMarker := detectPrimaryMarker(schema.Listeners); hasPrimaryMarker {
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

		// 2. Subdomain Collision (for __app_address__ or legacy subdomain inputs)
		if key == "subdomain" || key == "__app_address__" {
			if val, ok := input.Default.(string); ok && val != "" {
				freeName, err := FindFreeSubdomain(ctx, mgr, val)
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

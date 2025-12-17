package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"piccolod/internal/api"
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
		def, err := mgr.GetAppDefinition(ctx, app.Name)
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

// FindFreeSubdomain tries the base name, then appends -1, -2, etc. until a free one is found.
func FindFreeSubdomain(ctx context.Context, mgr *AppManager, base string) (string, error) {
	taken, err := IsSubdomainTaken(ctx, mgr, base)
	if err != nil {
		return base, err
	}
	if !taken {
		return base, nil
	}

	for i := 1; i < 100; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
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
func PrepareSmartDefaults(ctx context.Context, mgr *AppManager, schema *api.AppDefinition) error {
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

		// 2. Subdomain Collision
		// Heuristic: If key is "subdomain", check availability.
		if key == "subdomain" {
			if val, ok := input.Default.(string); ok {
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

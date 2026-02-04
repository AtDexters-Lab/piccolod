package app

import (
	"fmt"

	"piccolod/internal/hostname"
)

// ValidatePrimaryNameAvailable checks that the proposed instance ID (from primary listener
// name or workspace_name) doesn't collide with any existing app instance ID.
// Per RFC 20260130, we no longer auto-generate suffixes - collision is a user-facing error.
func ValidatePrimaryNameAvailable(primaryName string, existingIDs []string) error {
	for _, id := range existingIDs {
		if id == primaryName {
			return fmt.Errorf("app with identifier '%s' already exists; choose a different name", primaryName)
		}
	}
	return nil
}

// ValidateInstanceID validates that an instance ID follows the naming rules.
// Instance IDs are derived from listener names or workspace_names and follow the same rules:
// - lowercase letters and numbers only (no hyphens)
// - must start with a letter
// - 1-16 characters
// - cannot be a reserved name
func ValidateInstanceID(id string) error {
	return hostname.ValidateAppName(id)
}

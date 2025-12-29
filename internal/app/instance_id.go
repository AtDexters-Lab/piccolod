package app

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// MaxInstanceIDRetries is the maximum number of attempts to generate a unique instance ID.
const MaxInstanceIDRetries = 10

// GenerateInstanceID creates a unique instance ID for a new app installation.
// Algorithm:
//  1. Try {appName} first (suffix-less if available)
//  2. If conflict, try {appName}-{4-char-hex} (e.g., code-server-a7b2)
//  3. Retry with new random hex if still conflicts (up to MaxInstanceIDRetries attempts)
func GenerateInstanceID(appName string, existingIDs []string) (string, error) {
	existing := make(map[string]struct{}, len(existingIDs))
	for _, id := range existingIDs {
		existing[id] = struct{}{}
	}

	// Try base name first (suffix-less)
	if _, ok := existing[appName]; !ok {
		return appName, nil
	}

	// Try with random hex suffixes
	for attempt := 0; attempt < MaxInstanceIDRetries; attempt++ {
		suffix, err := randomHexSuffix(4)
		if err != nil {
			return "", fmt.Errorf("generate instance ID: %w", err)
		}
		candidate := fmt.Sprintf("%s-%s", appName, suffix)
		if _, ok := existing[candidate]; !ok {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("exhausted instance ID namespace for app %s after %d attempts", appName, MaxInstanceIDRetries)
}

// randomHexSuffix generates a random lowercase hex string of the specified length.
func randomHexSuffix(length int) (string, error) {
	// We need length/2 bytes to produce length hex characters
	bytes := make([]byte, (length+1)/2)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes)[:length], nil
}

// ValidateInstanceID validates that an instance ID follows the app naming rules.
// Instance IDs must follow the same validation as app names:
// - lowercase letters, numbers, and hyphens
// - must start with a letter
// - must end with letter or number
func ValidateInstanceID(id string) error {
	return validateName(id)
}

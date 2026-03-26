package cryptoutil

import (
	"crypto/rand"
	"encoding/base64"
)

// GenerateSecureToken generates a cryptographically secure random token
// of the given byte length, returned as a base64 raw URL-safe string.
func GenerateSecureToken(length int) (string, error) {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

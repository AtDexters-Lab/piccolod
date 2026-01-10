package auth

import (
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

const minPasswordLength = 8

// ValidatePasswordStrength checks if a password meets the minimum requirements.
// Exported for use by crypto setup to validate before any state changes.
func ValidatePasswordStrength(password string) error {
	return validatePasswordStrength(password)
}

func validatePasswordStrength(password string) error {
	if utf8.RuneCountInString(password) < minPasswordLength {
		return fmt.Errorf("password must be at least %d characters long", minPasswordLength)
	}

	hasLetter := false
	hasNumber := false
	for _, r := range password {
		if unicode.IsLetter(r) {
			hasLetter = true
			continue
		}
		if unicode.IsNumber(r) {
			hasNumber = true
		}
	}

	if !hasLetter || !hasNumber {
		return errors.New("password must include at least one letter and one number")
	}

	return nil
}

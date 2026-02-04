package app

import (
	"testing"
)

func TestValidatePrimaryNameAvailable_NoConflict(t *testing.T) {
	err := ValidatePrimaryNameAvailable("blog", []string{"app1", "app2"})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidatePrimaryNameAvailable_Conflict(t *testing.T) {
	err := ValidatePrimaryNameAvailable("blog", []string{"blog", "app1"})
	if err == nil {
		t.Fatalf("expected conflict error, got nil")
	}
}

func TestValidatePrimaryNameAvailable_EmptyList(t *testing.T) {
	err := ValidatePrimaryNameAvailable("blog", nil)
	if err != nil {
		t.Fatalf("expected no error for empty list, got %v", err)
	}
}

func TestValidateInstanceID(t *testing.T) {
	// Valid names per RFC 20260130: lowercase letters and numbers, start with letter, no hyphens, 1-16 chars
	validCases := []string{"blog", "app1", "a", "myapp123", "a1b2c3d4e5f6g7h8"}
	for _, name := range validCases {
		if err := ValidateInstanceID(name); err != nil {
			t.Errorf("expected %q to be valid, got %v", name, err)
		}
	}

	// Invalid: hyphens not allowed (RFC 20260130 §4.5.2)
	if err := ValidateInstanceID("my-app"); err == nil {
		t.Errorf("expected my-app to be invalid (hyphens not allowed)")
	}
	if err := ValidateInstanceID("blog-1"); err == nil {
		t.Errorf("expected blog-1 to be invalid (hyphens not allowed)")
	}

	// Invalid: uppercase
	if err := ValidateInstanceID("BadName"); err == nil {
		t.Errorf("expected BadName to be invalid (uppercase not allowed)")
	}

	// Invalid: must start with letter
	if err := ValidateInstanceID("1blog"); err == nil {
		t.Errorf("expected 1blog to be invalid (must start with letter)")
	}

	// Invalid: too long (>16 chars)
	if err := ValidateInstanceID("abcdefghijklmnopq"); err == nil {
		t.Errorf("expected 17-char name to be invalid (max 16 chars)")
	}

	// Invalid: reserved names
	reservedNames := []string{"api", "www", "admin", "root", "system", "piccolo", "piccoloos", "__primary"}
	for _, name := range reservedNames {
		if err := ValidateInstanceID(name); err == nil {
			t.Errorf("expected reserved name %q to be invalid", name)
		}
	}

	// Invalid: empty
	if err := ValidateInstanceID(""); err == nil {
		t.Errorf("expected empty string to be invalid")
	}
}

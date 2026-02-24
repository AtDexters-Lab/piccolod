package container

import (
	"testing"
)

func TestResolveRuntimeCredential_nonexistent_user(t *testing.T) {
	result, err := ResolveRuntimeCredential("piccolo-nonexistent-user-xyz")
	if err == nil {
		t.Fatal("expected error for nonexistent user, got nil")
	}
	if result != nil {
		t.Fatal("expected nil result for nonexistent user, got non-nil")
	}
}

func TestChownIfNeeded_nonexistent_path(t *testing.T) {
	// Nonexistent path should be a no-op (no error).
	err := ChownIfNeeded("/nonexistent/path/abc123", 1000, 1000)
	if err != nil {
		t.Fatalf("expected no error for nonexistent path, got: %v", err)
	}
}

func TestCheckCgroupDelegation_nonexistent_path(t *testing.T) {
	// Should log a warning but not panic. Non-fatal by design.
	CheckCgroupDelegation(99999)
}

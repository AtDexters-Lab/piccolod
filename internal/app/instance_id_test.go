package app

import (
	"crypto/rand"
	"testing"
)

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestGenerateInstanceID_FirstInstallation(t *testing.T) {
	id, err := GenerateInstanceID("code-server", nil)
	if err != nil {
		t.Fatalf("GenerateInstanceID: %v", err)
	}
	if id != "code-server" {
		t.Fatalf("expected code-server, got %s", id)
	}
}

func TestGenerateInstanceID_ConflictResolution(t *testing.T) {
	prev := rand.Reader
	rand.Reader = zeroReader{}
	t.Cleanup(func() { rand.Reader = prev })

	id, err := GenerateInstanceID("code-server", []string{"code-server"})
	if err != nil {
		t.Fatalf("GenerateInstanceID: %v", err)
	}
	if id != "code-server-0000" {
		t.Fatalf("expected code-server-0000, got %s", id)
	}
}

func TestGenerateInstanceID_MaxRetries(t *testing.T) {
	prev := rand.Reader
	rand.Reader = zeroReader{}
	t.Cleanup(func() { rand.Reader = prev })

	_, err := GenerateInstanceID("demo", []string{"demo", "demo-0000"})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestValidateInstanceID(t *testing.T) {
	if err := ValidateInstanceID("demo-app-1"); err != nil {
		t.Fatalf("expected demo-app-1 to be valid, got %v", err)
	}
	if err := ValidateInstanceID("BadName"); err == nil {
		t.Fatalf("expected BadName to be invalid")
	}
}

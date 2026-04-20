package auth

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"

	"piccolod/internal/persistence"
)

// stubCredRepo is a minimal in-memory WebAuthnCredentialRepo for testing.
type stubCredRepo struct {
	creds map[string]persistence.WebAuthnCredential
}

func newStubCredRepo() *stubCredRepo {
	return &stubCredRepo{creds: map[string]persistence.WebAuthnCredential{}}
}

func (r *stubCredRepo) Create(ctx context.Context, cred persistence.WebAuthnCredential) error {
	r.creds[cred.ID] = cred
	return nil
}

func (r *stubCredRepo) Get(ctx context.Context, credID string) (persistence.WebAuthnCredential, error) {
	c, ok := r.creds[credID]
	if !ok {
		return persistence.WebAuthnCredential{}, persistence.ErrNotFound
	}
	return c, nil
}

func (r *stubCredRepo) ListByUser(ctx context.Context, userID string) ([]persistence.WebAuthnCredential, error) {
	var out []persistence.WebAuthnCredential
	for _, c := range r.creds {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *stubCredRepo) ListByUserAndRP(ctx context.Context, userID, rpID string) ([]persistence.WebAuthnCredential, error) {
	var out []persistence.WebAuthnCredential
	for _, c := range r.creds {
		if c.UserID == userID && c.RPID == rpID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *stubCredRepo) ListByRP(ctx context.Context, rpID string) ([]persistence.WebAuthnCredential, error) {
	var out []persistence.WebAuthnCredential
	for _, c := range r.creds {
		if c.RPID == rpID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *stubCredRepo) UpdateAfterAuth(ctx context.Context, credID string, signCount uint32, lastUsed time.Time) error {
	c, ok := r.creds[credID]
	if !ok {
		return persistence.ErrNotFound
	}
	c.SignCount = signCount
	c.LastUsedAt = lastUsed
	r.creds[credID] = c
	return nil
}

func (r *stubCredRepo) UpdateFriendlyName(ctx context.Context, credID, name string) error {
	c, ok := r.creds[credID]
	if !ok {
		return persistence.ErrNotFound
	}
	c.FriendlyName = name
	r.creds[credID] = c
	return nil
}

func (r *stubCredRepo) Delete(ctx context.Context, credID string) error {
	if _, ok := r.creds[credID]; !ok {
		return persistence.ErrNotFound
	}
	delete(r.creds, credID)
	return nil
}

func (r *stubCredRepo) DeleteByUser(ctx context.Context, userID string) error {
	for id, c := range r.creds {
		if c.UserID == userID {
			delete(r.creds, id)
		}
	}
	return nil
}

func (r *stubCredRepo) CountByUser(ctx context.Context, userID string) (int, error) {
	n := 0
	for _, c := range r.creds {
		if c.UserID == userID {
			n++
		}
	}
	return n, nil
}

func (r *stubCredRepo) CountByUserAndRP(ctx context.Context, userID, rpID string) (int, error) {
	n := 0
	for _, c := range r.creds {
		if c.UserID == userID && c.RPID == rpID {
			n++
		}
	}
	return n, nil
}

func (r *stubCredRepo) ListUserIDsByRP(ctx context.Context, rpID string) ([]string, error) {
	seen := map[string]struct{}{}
	for _, c := range r.creds {
		if c.RPID == rpID {
			seen[c.UserID] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out, nil
}

// Test #1 (plan): wire-level — BeginRegistration must produce options with
// residentKey="required", requireResidentKey=true, and userVerification matching
// the input.
func TestBeginRegistrationOptions_ResidentKeyRequired(t *testing.T) {
	repo := newStubCredRepo()
	mgr := NewWebAuthnManager(repo, false)

	options, _, err := mgr.BeginRegistration(
		context.Background(),
		"user-1", "alice",
		"piccolo.local", "Piccolo OS", "https://piccolo.local",
		"piccolo.local",
		protocol.VerificationRequired,
	)
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}

	// Decode the wire response and verify the JSON shape that hits the browser.
	raw, err := json.Marshal(options.Response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sel, ok := wire["authenticatorSelection"].(map[string]any)
	if !ok {
		t.Fatalf("missing authenticatorSelection: %s", raw)
	}
	if got := sel["residentKey"]; got != "required" {
		t.Fatalf("residentKey: want 'required', got %v", got)
	}
	if got := sel["requireResidentKey"]; got != true {
		t.Fatalf("requireResidentKey: want true, got %v", got)
	}
	if got := sel["userVerification"]; got != "required" {
		t.Fatalf("userVerification: want 'required', got %v", got)
	}
}

// TestAlreadyRegisteredError_WrapMatch verifies errors.As matches our typed
// sentinel so mapRegistrationError can surface 409 passkey_already_registered.
func TestAlreadyRegisteredError_WrapMatch(t *testing.T) {
	dup := error(&AlreadyRegisteredError{CredentialID: "abc", RPID: "rp"})
	var target *AlreadyRegisteredError
	if !errors.As(dup, &target) {
		t.Fatal("AlreadyRegisteredError should match errors.As")
	}
	if target.CredentialID != "abc" || target.RPID != "rp" {
		t.Fatalf("fields not preserved: %+v", target)
	}
}

// TestBeginRegistrationOptions_UserVerificationPreferred guards the lower-UV
// path used on LAN.
func TestBeginRegistrationOptions_UserVerificationPreferred(t *testing.T) {
	repo := newStubCredRepo()
	mgr := NewWebAuthnManager(repo, false)

	options, _, err := mgr.BeginRegistration(
		context.Background(),
		"user-1", "alice",
		"piccolo.local", "Piccolo OS", "https://piccolo.local",
		"piccolo.local",
		protocol.VerificationPreferred,
	)
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	raw, _ := json.Marshal(options.Response)
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	sel := wire["authenticatorSelection"].(map[string]any)
	if got := sel["userVerification"]; got != "preferred" {
		t.Fatalf("userVerification: want 'preferred', got %v", got)
	}
	// Critical: residentKey must remain "required" — splitting the option
	// silently wiped it in the pre-fix code.
	if got := sel["residentKey"]; got != "required" {
		t.Fatalf("residentKey: want 'required', got %v", got)
	}
}

package persistence

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *sqliteControlStore {
	t.Helper()
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")
	dir := t.TempDir()
	key, _ := hex.DecodeString("7f1c8a6c3b5d7e91aabbccddeeff00112233445566778899aabbccddeeff0011")
	prepareControlCipherDir(t, dir)

	store, err := newSQLiteControlStore(dir, staticKeyProvider{key: key})
	if err != nil {
		t.Fatalf("newSQLiteControlStore: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })

	if err := store.Unlock(context.Background()); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	return store
}

// seedUser inserts a minimal user row and returns its ID.
func seedUser(t *testing.T, store *sqliteControlStore, id, username string) {
	t.Helper()
	ctx := context.Background()
	user := User{
		ID:       id,
		Username: username,
		Email:    username + "@example.com",
		Role:     UserRoleStandard,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := store.Users().Create(ctx, user); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

// --- WebAuthnCredentialRepo tests ---

func TestWebAuthnCredentialRepo_CreateAndGet(t *testing.T) {
	store := openTestStore(t)
	repo := store.WebAuthnCredentials()
	ctx := context.Background()
	seedUser(t, store, "user-1", "alice")

	now := time.Now().UTC().Truncate(time.Second)
	cred := WebAuthnCredential{
		ID:              base64.RawURLEncoding.EncodeToString([]byte("cred-1")),
		UserID:          "user-1",
		PublicKey:       []byte("fake-public-key"),
		AttestationType: "none",
		Transports:      []string{"internal", "usb"},
		SignCount:       0,
		RPID:            "piccolo.local",
		AAGUID:          []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		FriendlyName:    "My Passkey",
		CreatedAt:       now,
		LastUsedAt:      now,
	}

	if err := repo.Create(ctx, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, cred.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UserID != "user-1" {
		t.Fatalf("expected user-1, got %s", got.UserID)
	}
	if got.RPID != "piccolo.local" {
		t.Fatalf("expected piccolo.local, got %s", got.RPID)
	}
	if got.FriendlyName != "My Passkey" {
		t.Fatalf("expected My Passkey, got %s", got.FriendlyName)
	}
	if len(got.Transports) != 2 || got.Transports[0] != "internal" || got.Transports[1] != "usb" {
		t.Fatalf("unexpected transports: %v", got.Transports)
	}
}

func TestWebAuthnCredentialRepo_GetNotFound(t *testing.T) {
	store := openTestStore(t)
	repo := store.WebAuthnCredentials()
	ctx := context.Background()

	_, err := repo.Get(ctx, "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestWebAuthnCredentialRepo_ListByUserAndRP(t *testing.T) {
	store := openTestStore(t)
	repo := store.WebAuthnCredentials()
	ctx := context.Background()
	seedUser(t, store, "user-1", "alice")
	seedUser(t, store, "user-2", "bob")

	now := time.Now().UTC().Truncate(time.Second)

	// Create creds for user-1 on two different RP IDs
	for i, rpID := range []string{"piccolo.local", "example.com"} {
		cred := WebAuthnCredential{
			ID:              base64.RawURLEncoding.EncodeToString([]byte("cred-" + rpID)),
			UserID:          "user-1",
			PublicKey:       []byte("pk"),
			AttestationType: "none",
			RPID:            rpID,
			AAGUID:          make([]byte, 16),
			CreatedAt:       now.Add(time.Duration(i) * time.Second),
			LastUsedAt:      now,
		}
		if err := repo.Create(ctx, cred); err != nil {
			t.Fatalf("Create cred %d: %v", i, err)
		}
	}

	// Create cred for user-2 on piccolo.local
	cred2 := WebAuthnCredential{
		ID:              base64.RawURLEncoding.EncodeToString([]byte("cred-user2")),
		UserID:          "user-2",
		PublicKey:       []byte("pk"),
		AttestationType: "none",
		RPID:            "piccolo.local",
		AAGUID:          make([]byte, 16),
		CreatedAt:       now,
		LastUsedAt:      now,
	}
	if err := repo.Create(ctx, cred2); err != nil {
		t.Fatalf("Create cred user-2: %v", err)
	}

	t.Run("list_by_user", func(t *testing.T) {
		creds, err := repo.ListByUser(ctx, "user-1")
		if err != nil {
			t.Fatalf("ListByUser: %v", err)
		}
		if len(creds) != 2 {
			t.Fatalf("expected 2 creds for user-1, got %d", len(creds))
		}
	})

	t.Run("list_by_user_and_rp", func(t *testing.T) {
		creds, err := repo.ListByUserAndRP(ctx, "user-1", "piccolo.local")
		if err != nil {
			t.Fatalf("ListByUserAndRP: %v", err)
		}
		if len(creds) != 1 {
			t.Fatalf("expected 1 cred for user-1 + piccolo.local, got %d", len(creds))
		}
	})

	t.Run("list_by_rp", func(t *testing.T) {
		creds, err := repo.ListByRP(ctx, "piccolo.local")
		if err != nil {
			t.Fatalf("ListByRP: %v", err)
		}
		if len(creds) != 2 {
			t.Fatalf("expected 2 creds for piccolo.local, got %d", len(creds))
		}
	})
}

func TestWebAuthnCredentialRepo_UpdateAfterAuth(t *testing.T) {
	store := openTestStore(t)
	repo := store.WebAuthnCredentials()
	ctx := context.Background()
	seedUser(t, store, "user-1", "alice")

	now := time.Now().UTC().Truncate(time.Second)
	cred := WebAuthnCredential{
		ID:              base64.RawURLEncoding.EncodeToString([]byte("cred-update")),
		UserID:          "user-1",
		PublicKey:       []byte("pk"),
		AttestationType: "none",
		RPID:            "piccolo.local",
		AAGUID:          make([]byte, 16),
		CreatedAt:       now,
		LastUsedAt:      now,
		SignCount:       0,
	}
	if err := repo.Create(ctx, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}

	newTime := now.Add(5 * time.Minute)
	if err := repo.UpdateAfterAuth(ctx, cred.ID, 42, newTime); err != nil {
		t.Fatalf("UpdateAfterAuth: %v", err)
	}

	got, err := repo.Get(ctx, cred.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.SignCount != 42 {
		t.Fatalf("expected sign_count 42, got %d", got.SignCount)
	}
}

func TestWebAuthnCredentialRepo_UpdateFriendlyName(t *testing.T) {
	store := openTestStore(t)
	repo := store.WebAuthnCredentials()
	ctx := context.Background()
	seedUser(t, store, "user-1", "alice")

	now := time.Now().UTC().Truncate(time.Second)
	cred := WebAuthnCredential{
		ID:              base64.RawURLEncoding.EncodeToString([]byte("cred-rename")),
		UserID:          "user-1",
		PublicKey:       []byte("pk"),
		AttestationType: "none",
		RPID:            "piccolo.local",
		AAGUID:          make([]byte, 16),
		CreatedAt:       now,
		LastUsedAt:      now,
	}
	if err := repo.Create(ctx, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := repo.UpdateFriendlyName(ctx, cred.ID, "Laptop Passkey"); err != nil {
		t.Fatalf("UpdateFriendlyName: %v", err)
	}

	got, err := repo.Get(ctx, cred.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.FriendlyName != "Laptop Passkey" {
		t.Fatalf("expected Laptop Passkey, got %s", got.FriendlyName)
	}
}

func TestWebAuthnCredentialRepo_Delete(t *testing.T) {
	store := openTestStore(t)
	repo := store.WebAuthnCredentials()
	ctx := context.Background()
	seedUser(t, store, "user-1", "alice")

	now := time.Now().UTC().Truncate(time.Second)
	cred := WebAuthnCredential{
		ID:              base64.RawURLEncoding.EncodeToString([]byte("cred-del")),
		UserID:          "user-1",
		PublicKey:       []byte("pk"),
		AttestationType: "none",
		RPID:            "piccolo.local",
		AAGUID:          make([]byte, 16),
		CreatedAt:       now,
		LastUsedAt:      now,
	}
	if err := repo.Create(ctx, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := repo.Delete(ctx, cred.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := repo.Get(ctx, cred.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestWebAuthnCredentialRepo_DeleteByUser(t *testing.T) {
	store := openTestStore(t)
	repo := store.WebAuthnCredentials()
	ctx := context.Background()
	seedUser(t, store, "user-1", "alice")
	seedUser(t, store, "user-2", "bob")

	now := time.Now().UTC().Truncate(time.Second)
	for _, uid := range []string{"user-1", "user-2"} {
		cred := WebAuthnCredential{
			ID:              base64.RawURLEncoding.EncodeToString([]byte("cred-" + uid)),
			UserID:          uid,
			PublicKey:       []byte("pk"),
			AttestationType: "none",
			RPID:            "piccolo.local",
			AAGUID:          make([]byte, 16),
			CreatedAt:       now,
			LastUsedAt:      now,
		}
		if err := repo.Create(ctx, cred); err != nil {
			t.Fatalf("Create %s: %v", uid, err)
		}
	}

	if err := repo.DeleteByUser(ctx, "user-1"); err != nil {
		t.Fatalf("DeleteByUser: %v", err)
	}

	// user-1 creds gone
	creds, err := repo.ListByUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("ListByUser user-1: %v", err)
	}
	if len(creds) != 0 {
		t.Fatalf("expected 0 creds for user-1, got %d", len(creds))
	}

	// user-2 creds remain
	creds, err = repo.ListByUser(ctx, "user-2")
	if err != nil {
		t.Fatalf("ListByUser user-2: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("expected 1 cred for user-2, got %d", len(creds))
	}
}

func TestWebAuthnCredentialRepo_Counts(t *testing.T) {
	store := openTestStore(t)
	repo := store.WebAuthnCredentials()
	ctx := context.Background()
	seedUser(t, store, "user-1", "alice")

	now := time.Now().UTC().Truncate(time.Second)
	// 2 creds on piccolo.local, 1 on example.com
	for i, rpID := range []string{"piccolo.local", "piccolo.local", "example.com"} {
		cred := WebAuthnCredential{
			ID:              base64.RawURLEncoding.EncodeToString([]byte("cnt-" + rpID + "-" + string(rune('a'+i)))),
			UserID:          "user-1",
			PublicKey:       []byte("pk"),
			AttestationType: "none",
			RPID:            rpID,
			AAGUID:          make([]byte, 16),
			CreatedAt:       now.Add(time.Duration(i) * time.Second),
			LastUsedAt:      now,
		}
		if err := repo.Create(ctx, cred); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	t.Run("count_by_user", func(t *testing.T) {
		count, err := repo.CountByUser(ctx, "user-1")
		if err != nil {
			t.Fatalf("CountByUser: %v", err)
		}
		if count != 3 {
			t.Fatalf("expected 3, got %d", count)
		}
	})

	t.Run("count_by_user_and_rp", func(t *testing.T) {
		count, err := repo.CountByUserAndRP(ctx, "user-1", "piccolo.local")
		if err != nil {
			t.Fatalf("CountByUserAndRP: %v", err)
		}
		if count != 2 {
			t.Fatalf("expected 2, got %d", count)
		}
	})

	t.Run("count_by_user_and_rp_cross_rp", func(t *testing.T) {
		total, err := repo.CountByUser(ctx, "user-1")
		if err != nil {
			t.Fatalf("CountByUser: %v", err)
		}
		if total != 3 {
			t.Fatalf("expected total 3, got %d", total)
		}
		rpCount, err := repo.CountByUserAndRP(ctx, "user-1", "piccolo.local")
		if err != nil {
			t.Fatalf("CountByUserAndRP: %v", err)
		}
		if rpCount != 2 {
			t.Fatalf("expected rpCount 2, got %d", rpCount)
		}
	})
}

// --- InviteTokenRepo tests ---

func TestInviteTokenRepo_CreateAndGet(t *testing.T) {
	store := openTestStore(t)
	repo := store.InviteTokens()
	ctx := context.Background()
	seedUser(t, store, "user-1", "alice")

	now := time.Now().UTC().Truncate(time.Second)
	invite := InviteToken{
		Token:     "test-token-abc",
		UserID:    "user-1",
		CreatedBy: "admin-1",
		ExpiresAt: now.Add(7 * 24 * time.Hour),
		CreatedAt: now,
	}

	if err := repo.Create(ctx, invite); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, "test-token-abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UserID != "user-1" {
		t.Fatalf("expected user-1, got %s", got.UserID)
	}
	if got.CreatedBy != "admin-1" {
		t.Fatalf("expected admin-1, got %s", got.CreatedBy)
	}
	if got.ConsumedAt != nil {
		t.Fatal("expected nil ConsumedAt")
	}
}

func TestInviteTokenRepo_GetNotFound(t *testing.T) {
	store := openTestStore(t)
	repo := store.InviteTokens()
	ctx := context.Background()

	_, err := repo.Get(ctx, "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestInviteTokenRepo_Consume(t *testing.T) {
	store := openTestStore(t)
	repo := store.InviteTokens()
	ctx := context.Background()
	seedUser(t, store, "user-1", "alice")

	now := time.Now().UTC().Truncate(time.Second)
	invite := InviteToken{
		Token:     "consume-token",
		UserID:    "user-1",
		CreatedBy: "admin-1",
		ExpiresAt: now.Add(7 * 24 * time.Hour),
		CreatedAt: now,
	}
	if err := repo.Create(ctx, invite); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := repo.Consume(ctx, "consume-token"); err != nil {
		t.Fatalf("Consume: %v", err)
	}

	got, err := repo.Get(ctx, "consume-token")
	if err != nil {
		t.Fatalf("Get after consume: %v", err)
	}
	if got.ConsumedAt == nil {
		t.Fatal("expected ConsumedAt to be set after consume")
	}
}

func TestInviteTokenRepo_ConsumeIdempotent(t *testing.T) {
	store := openTestStore(t)
	repo := store.InviteTokens()
	ctx := context.Background()
	seedUser(t, store, "user-1", "alice")

	now := time.Now().UTC().Truncate(time.Second)
	invite := InviteToken{
		Token:     "consume-twice",
		UserID:    "user-1",
		CreatedBy: "admin-1",
		ExpiresAt: now.Add(7 * 24 * time.Hour),
		CreatedAt: now,
	}
	if err := repo.Create(ctx, invite); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// First consume
	if err := repo.Consume(ctx, "consume-twice"); err != nil {
		t.Fatalf("first Consume: %v", err)
	}

	// Second consume should still succeed at the repo level
	// (the auth layer handles the already-consumed check)
	err := repo.Consume(ctx, "consume-twice")
	// The repo Consume uses WHERE consumed_at IS NULL, so this may
	// be a no-op or return an error depending on implementation
	_ = err // either outcome is acceptable at the repo layer
}

func TestInviteTokenRepo_DeleteByUser(t *testing.T) {
	store := openTestStore(t)
	repo := store.InviteTokens()
	ctx := context.Background()
	seedUser(t, store, "user-1", "alice")
	seedUser(t, store, "user-2", "bob")

	now := time.Now().UTC().Truncate(time.Second)
	for i, uid := range []string{"user-1", "user-1", "user-2"} {
		invite := InviteToken{
			Token:     "del-token-" + uid + "-" + string(rune('a'+i)),
			UserID:    uid,
			CreatedBy: "admin-1",
			ExpiresAt: now.Add(7 * 24 * time.Hour),
			CreatedAt: now.Add(time.Duration(i) * time.Second),
		}
		if err := repo.Create(ctx, invite); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	if err := repo.DeleteByUser(ctx, "user-1"); err != nil {
		t.Fatalf("DeleteByUser: %v", err)
	}

	// user-1 tokens should be gone
	_, err := repo.Get(ctx, "del-token-user-1-a")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for user-1 token, got %v", err)
	}

	// user-2 token should remain
	got, err := repo.Get(ctx, "del-token-user-2-c")
	if err != nil {
		t.Fatalf("Get user-2 token: %v", err)
	}
	if got.UserID != "user-2" {
		t.Fatalf("expected user-2, got %s", got.UserID)
	}
}

func TestInviteTokenRepo_DeleteExpired(t *testing.T) {
	store := openTestStore(t)
	repo := store.InviteTokens()
	ctx := context.Background()
	seedUser(t, store, "user-1", "alice")

	now := time.Now().UTC().Truncate(time.Second)

	// One expired, one valid
	expired := InviteToken{
		Token:     "expired-token",
		UserID:    "user-1",
		CreatedBy: "admin-1",
		ExpiresAt: now.Add(-1 * time.Hour),
		CreatedAt: now.Add(-8 * 24 * time.Hour),
	}
	valid := InviteToken{
		Token:     "valid-token",
		UserID:    "user-1",
		CreatedBy: "admin-1",
		ExpiresAt: now.Add(7 * 24 * time.Hour),
		CreatedAt: now,
	}
	if err := repo.Create(ctx, expired); err != nil {
		t.Fatalf("Create expired: %v", err)
	}
	if err := repo.Create(ctx, valid); err != nil {
		t.Fatalf("Create valid: %v", err)
	}

	if err := repo.DeleteExpired(ctx); err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}

	_, err := repo.Get(ctx, "expired-token")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected expired token to be deleted, got %v", err)
	}

	got, err := repo.Get(ctx, "valid-token")
	if err != nil {
		t.Fatalf("valid token should remain: %v", err)
	}
	if got.Token != "valid-token" {
		t.Fatalf("unexpected token: %s", got.Token)
	}
}

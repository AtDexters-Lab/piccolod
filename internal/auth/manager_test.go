package auth

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestManager_SetupAndVerify(t *testing.T) {
	dir, err := os.MkdirTemp("", "authmgr")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	defer os.RemoveAll(dir)
	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	if init, err := m.IsInitialized(ctx); err != nil || init {
		t.Fatalf("unexpected initialized=%v err=%v", init, err)
	}
	if err := m.Setup(ctx, "pw123456"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if ok, err := m.Verify(ctx, "admin", "pw123456"); err != nil || !ok {
		t.Fatalf("verify failed ok=%v err=%v", ok, err)
	}
}

func TestArgon2_HashAndVerify(t *testing.T) {
	ref, err := hashArgon2id("pw123456")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !verifyArgon2id(ref, "pw123456") {
		t.Fatalf("verifyArgon2id failed: %s", ref)
	}
}

func TestSessionStore_ExtendTTLIfNeeded(t *testing.T) {
	t.Run("extends_below_threshold", func(t *testing.T) {
		store := NewSessionStore()
		sess := store.CreateWithUserInfo("u1", "admin", "admin", 3600)
		// Age session so remaining (2600s) < threshold (2700s)
		sess.ExpiresAt = time.Now().Unix() + 2600

		ok := store.ExtendTTLIfNeeded(sess.ID, 3600, 2700)
		if !ok {
			t.Fatal("expected ExtendTTLIfNeeded to extend")
		}
		got, found := store.Get(sess.ID)
		if !found {
			t.Fatal("session should exist after extend")
		}
		expected := time.Now().Unix() + 3600
		if got.ExpiresAt < expected-5 || got.ExpiresAt > expected+5 {
			t.Fatalf("expected ExpiresAt ~%d, got %d", expected, got.ExpiresAt)
		}
	})

	t.Run("no_extend_above_threshold", func(t *testing.T) {
		store := NewSessionStore()
		sess := store.CreateWithUserInfo("u1", "admin", "admin", 3600)
		originalExpiry := sess.ExpiresAt

		// Remaining (~3600s) >= threshold (2700s), should NOT extend.
		ok := store.ExtendTTLIfNeeded(sess.ID, 7200, 2700)
		if ok {
			t.Fatal("expected ExtendTTLIfNeeded to skip extension")
		}
		got, found := store.Get(sess.ID)
		if !found {
			t.Fatal("session should still exist")
		}
		if got.ExpiresAt != originalExpiry {
			t.Fatalf("ExpiresAt should be unchanged: want %d, got %d", originalExpiry, got.ExpiresAt)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		store := NewSessionStore()
		if store.ExtendTTLIfNeeded("nonexistent", 3600, 2700) {
			t.Fatal("expected false for nonexistent ID")
		}
	})

	t.Run("expired", func(t *testing.T) {
		orig := timeNow
		defer func() { timeNow = orig }()

		store := NewSessionStore()
		timeNow = func() time.Time { return time.Now() }
		sess := store.CreateWithUserInfo("u1", "admin", "admin", 10)

		// Advance time past expiry
		timeNow = func() time.Time { return time.Now().Add(20 * time.Second) }

		if store.ExtendTTLIfNeeded(sess.ID, 3600, 2700) {
			t.Fatal("expected false for expired session")
		}
		if _, found := store.Get(sess.ID); found {
			t.Fatal("expired session should have been deleted")
		}
	})
}

func TestManager_ChangePasswordWithRecovery(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	if err := m.Setup(ctx, "initial1"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := m.ChangePasswordWithRecovery(ctx, "recovered1"); err != nil {
		t.Fatalf("ChangePasswordWithRecovery: %v", err)
	}
	if ok, _ := m.Verify(ctx, "admin", "initial1"); ok {
		t.Fatalf("expected old password to fail")
	}
	if ok, err := m.Verify(ctx, "admin", "recovered1"); err != nil || !ok {
		t.Fatalf("expected recovered password to verify, ok=%v err=%v", ok, err)
	}
}

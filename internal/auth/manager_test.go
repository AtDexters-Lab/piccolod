package auth

import (
	"context"
	"os"
	"sync"
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

func TestSessionStore_ClearMustRegisterPasskey(t *testing.T) {
	store := NewSessionStore()
	// userA: three gated LAN sessions (RP=piccolo.local) + one off-gate LAN
	// session + one gated REMOTE session (RP=example.com). Clear for LAN
	// must only touch the three gated LAN sessions.
	newSess := func(userID, username, origin string) *Session {
		sess := store.CreatePortalSession(userID, username, "admin", origin, 3600)
		return sess
	}
	a1 := newSess("userA", "alice", "https://piccolo.local")
	a2 := newSess("userA", "alice", "https://piccolo.local:8080")
	a3 := newSess("userA", "alice", "https://piccolo.local")
	a4 := newSess("userA", "alice", "https://piccolo.local") // stays off-gate
	aRemote := newSess("userA", "alice", "https://remote.example.com")
	a1.MustRegisterPasskey.Store(true)
	a2.MustRegisterPasskey.Store(true)
	a3.MustRegisterPasskey.Store(true)
	aRemote.MustRegisterPasskey.Store(true)

	// userB: separate user, must not be affected.
	b1 := newSess("userB", "bob", "https://piccolo.local")
	b1.MustRegisterPasskey.Store(true)

	// Clear for LAN RP only.
	n := store.ClearMustRegisterPasskey("userA", "piccolo.local", "")
	if n != 3 {
		t.Fatalf("expected 3 LAN sessions cleared, got %d", n)
	}
	for _, id := range []string{a1.ID, a2.ID, a3.ID, a4.ID} {
		got, _ := store.Get(id)
		if got.MustRegisterPasskey.Load() {
			t.Fatalf("userA LAN session %s still has forcing flag set", id)
		}
	}
	// Remote session on a different RP must stay gated (cross-RP bypass
	// guard — see Codex P2 review 2026-04-20).
	gotRemote, _ := store.Get(aRemote.ID)
	if !gotRemote.MustRegisterPasskey.Load() {
		t.Fatalf("cross-RP bypass: userA remote-RP session was cleared by a LAN clear")
	}
	// userB untouched.
	gotB, _ := store.Get(b1.ID)
	if !gotB.MustRegisterPasskey.Load() {
		t.Fatalf("userB session forcing flag was incorrectly cleared")
	}

	// Idempotent: a second call clears zero.
	if n := store.ClearMustRegisterPasskey("userA", "piccolo.local", ""); n != 0 {
		t.Fatalf("expected 0 on second call, got %d", n)
	}
	// Now clear the remote RP — should release exactly the remote session.
	if n := store.ClearMustRegisterPasskey("userA", "remote.example.com", ""); n != 1 {
		t.Fatalf("expected 1 remote session cleared, got %d", n)
	}
	// Empty args are a no-op.
	if n := store.ClearMustRegisterPasskey("", "piccolo.local", ""); n != 0 {
		t.Fatalf("expected 0 for empty userID, got %d", n)
	}
	if n := store.ClearMustRegisterPasskey("userA", "", ""); n != 0 {
		t.Fatalf("expected 0 for empty rpID, got %d", n)
	}
}

// TestSessionStore_MustRegisterPasskey_ConcurrentRace guards against the
// data race that existed when MustRegisterPasskey was a plain bool: readers
// in request handlers dereferenced *Session after SessionStore.Get released
// its lock while ClearMustRegisterPasskey fanned writes across sessions.
// Under -race this fires immediately if the atomic.Bool is regressed.
func TestSessionStore_MustRegisterPasskey_ConcurrentRace(t *testing.T) {
	store := NewSessionStore()
	const sessionsPerUser = 8
	var sids []string
	for range sessionsPerUser {
		s := store.CreatePortalSession("u", "alice", "admin", "https://piccolo.local", 3600)
		s.MustRegisterPasskey.Store(true)
		sids = append(sids, s.ID)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, id := range sids {
					if sess, ok := store.Get(id); ok {
						_ = sess.MustRegisterPasskey.Load()
					}
				}
			}
		}()
	}

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				store.ClearMustRegisterPasskey("u", "piccolo.local", "")
				for _, id := range sids {
					store.SetMustRegisterPasskey(id)
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestSessionStore_SetMustRegisterPasskey(t *testing.T) {
	store := NewSessionStore()
	sess := store.CreateWithUserInfo("u1", "alice", "admin", 3600)

	if !store.SetMustRegisterPasskey(sess.ID) {
		t.Fatal("expected SetMustRegisterPasskey to succeed on live session")
	}
	got, _ := store.Get(sess.ID)
	if !got.MustRegisterPasskey.Load() {
		t.Fatalf("MustRegisterPasskey not set")
	}

	// Missing session returns false.
	if store.SetMustRegisterPasskey("nonexistent") {
		t.Fatal("expected false for nonexistent session")
	}
	// Empty id returns false.
	if store.SetMustRegisterPasskey("") {
		t.Fatal("expected false for empty id")
	}
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

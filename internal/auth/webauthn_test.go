package auth

import (
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

func TestCeremonyStore_StoreAndConsume(t *testing.T) {
	store := newCeremonyStore()

	session := &ceremonySession{
		UserID:      "user-1",
		RPID:        "piccolo.local",
		RPOrigin:    "https://piccolo.local",
		Type:        ceremonyRegistration,
		SessionData: &webauthn.SessionData{Challenge: "test-challenge"},
	}

	id, err := store.Store(session)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty session ID")
	}

	got, err := store.Consume(id)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got.UserID != "user-1" {
		t.Fatalf("expected UserID user-1, got %s", got.UserID)
	}
	if got.RPID != "piccolo.local" {
		t.Fatalf("expected RPID piccolo.local, got %s", got.RPID)
	}
	if got.Type != ceremonyRegistration {
		t.Fatalf("expected registration type, got %s", got.Type)
	}
	if got.SessionData.Challenge != "test-challenge" {
		t.Fatalf("expected challenge test-challenge, got %s", got.SessionData.Challenge)
	}
}

func TestCeremonyStore_Expiry(t *testing.T) {
	store := newCeremonyStore()

	session := &ceremonySession{
		UserID:      "user-1",
		RPID:        "piccolo.local",
		RPOrigin:    "https://piccolo.local",
		Type:        ceremonyAuthentication,
		SessionData: &webauthn.SessionData{},
	}

	id, err := store.Store(session)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Manually expire the session
	store.mu.Lock()
	store.sessions[id].ExpiresAt = time.Now().Add(-1 * time.Second)
	store.mu.Unlock()

	_, err = store.Consume(id)
	if err != ErrCeremonyNotFound {
		t.Fatalf("expected ErrCeremonyNotFound for expired session, got %v", err)
	}

	// Verify the expired session was cleaned up
	store.mu.Lock()
	_, exists := store.sessions[id]
	store.mu.Unlock()
	if exists {
		t.Fatal("expected expired session to be deleted from map")
	}
}

func TestCeremonyStore_Capacity(t *testing.T) {
	store := newCeremonyStore()

	// Fill the store to capacity
	for i := 0; i < ceremonyMaxCapacity; i++ {
		_, err := store.Store(&ceremonySession{
			UserID:      "user-fill",
			RPID:        "piccolo.local",
			RPOrigin:    "https://piccolo.local",
			Type:        ceremonyRegistration,
			SessionData: &webauthn.SessionData{},
		})
		if err != nil {
			t.Fatalf("Store #%d: %v", i, err)
		}
	}

	// Next store should fail
	_, err := store.Store(&ceremonySession{
		UserID:      "user-overflow",
		RPID:        "piccolo.local",
		RPOrigin:    "https://piccolo.local",
		Type:        ceremonyRegistration,
		SessionData: &webauthn.SessionData{},
	})
	if err != ErrCeremonyStoreFull {
		t.Fatalf("expected ErrCeremonyStoreFull, got %v", err)
	}
}

func TestCeremonyStore_CapacityRecoveryAfterExpiry(t *testing.T) {
	store := newCeremonyStore()

	// Fill to capacity
	for i := 0; i < ceremonyMaxCapacity; i++ {
		_, err := store.Store(&ceremonySession{
			UserID:      "user-fill",
			RPID:        "piccolo.local",
			RPOrigin:    "https://piccolo.local",
			Type:        ceremonyRegistration,
			SessionData: &webauthn.SessionData{},
		})
		if err != nil {
			t.Fatalf("Store #%d: %v", i, err)
		}
	}

	// Expire all sessions
	store.mu.Lock()
	for id := range store.sessions {
		store.sessions[id].ExpiresAt = time.Now().Add(-1 * time.Second)
	}
	store.mu.Unlock()

	// Should succeed now because lazy eviction clears expired entries
	_, err := store.Store(&ceremonySession{
		UserID:      "user-after-eviction",
		RPID:        "piccolo.local",
		RPOrigin:    "https://piccolo.local",
		Type:        ceremonyRegistration,
		SessionData: &webauthn.SessionData{},
	})
	if err != nil {
		t.Fatalf("expected Store to succeed after expiry eviction, got %v", err)
	}
}

func TestCeremonyStore_SingleUse(t *testing.T) {
	store := newCeremonyStore()

	id, err := store.Store(&ceremonySession{
		UserID:      "user-1",
		RPID:        "piccolo.local",
		RPOrigin:    "https://piccolo.local",
		Type:        ceremonyRegistration,
		SessionData: &webauthn.SessionData{},
	})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	// First consume succeeds
	if _, err := store.Consume(id); err != nil {
		t.Fatalf("first Consume: %v", err)
	}

	// Second consume fails
	_, err = store.Consume(id)
	if err != ErrCeremonyNotFound {
		t.Fatalf("expected ErrCeremonyNotFound on second consume, got %v", err)
	}
}

func TestCeremonyStore_UniqueIDs(t *testing.T) {
	store := newCeremonyStore()
	seen := make(map[string]bool)

	for i := 0; i < 50; i++ {
		id, err := store.Store(&ceremonySession{
			UserID:      "user-1",
			RPID:        "piccolo.local",
			RPOrigin:    "https://piccolo.local",
			Type:        ceremonyRegistration,
			SessionData: &webauthn.SessionData{},
		})
		if err != nil {
			t.Fatalf("Store #%d: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("duplicate session ID: %s", id)
		}
		seen[id] = true
	}
}

func TestCeremonyStore_ConsumeNonexistent(t *testing.T) {
	store := newCeremonyStore()

	_, err := store.Consume("does-not-exist")
	if err != ErrCeremonyNotFound {
		t.Fatalf("expected ErrCeremonyNotFound, got %v", err)
	}
}

func TestCeremonyStore_ExpiresAtIsSet(t *testing.T) {
	store := newCeremonyStore()
	before := time.Now()

	id, err := store.Store(&ceremonySession{
		UserID:      "user-1",
		RPID:        "piccolo.local",
		RPOrigin:    "https://piccolo.local",
		Type:        ceremonyAuthentication,
		SessionData: &webauthn.SessionData{},
	})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	store.mu.Lock()
	session := store.sessions[id]
	store.mu.Unlock()

	expectedMin := before.Add(ceremonyTTL)
	expectedMax := time.Now().Add(ceremonyTTL)
	if session.ExpiresAt.Before(expectedMin) || session.ExpiresAt.After(expectedMax) {
		t.Fatalf("ExpiresAt %v not within expected range [%v, %v]", session.ExpiresAt, expectedMin, expectedMax)
	}
}

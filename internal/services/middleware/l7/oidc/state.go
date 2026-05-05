// Package oidc implements proxy-level OIDC client flow per RFC 20260122 §5.
// Lives under middleware/l7/ because it composes only on the L7 path
// (HTTP request/response).
package oidc

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"piccolod/internal/cryptoutil"
)

const (
	// State store defaults per RFC 20260122 §5.7.
	defaultStateTTL   = 10 * time.Minute
	defaultMaxEntries = 10000
	stateTokenBytes   = 32
)

// State represents the stored state for an OIDC authorization flow per
// RFC 20260122 §5.7.
type State struct {
	ID             string    // Opaque state token sent to authorization server
	CodeVerifier   string    // PKCE code_verifier (for token exchange)
	OriginalPath   string    // Path+query to redirect back to (MUST be relative)
	ExpectedApp    string    // App name (prevents confused deputy)
	ExpectedOrigin string    // Expected callback origin (scheme://host[:port])
	IsIframe       bool      // True when the OIDC flow was initiated from an iframe context (CHIPS)
	CreatedAt      time.Time // For expiry
}

// StateStore manages OIDC state entries with TTL and LRU eviction per
// RFC 20260122 §5.7.
type StateStore struct {
	mu         sync.Mutex
	states     map[string]*list.Element
	lruList    *list.List
	maxEntries int
	ttl        time.Duration
}

type stateEntry struct {
	state     *State
	expiresAt time.Time
}

// NewStateStore creates a new OIDC state store with default configuration.
func NewStateStore() *StateStore {
	return &StateStore{
		states:     make(map[string]*list.Element),
		lruList:    list.New(),
		maxEntries: defaultMaxEntries,
		ttl:        defaultStateTTL,
	}
}

// Create assigns an ID to state, stores it, and returns the populated state.
func (s *StateStore) Create(state *State) error {
	if state == nil {
		return fmt.Errorf("state is nil")
	}

	stateID, err := cryptoutil.GenerateSecureToken(stateTokenBytes)
	if err != nil {
		return fmt.Errorf("failed to generate state ID: %w", err)
	}
	state.ID = stateID
	state.CreatedAt = time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneExpiredLocked()

	for len(s.states) >= s.maxEntries && s.lruList.Len() > 0 {
		oldest := s.lruList.Back()
		if oldest != nil {
			entry := oldest.Value.(*stateEntry)
			delete(s.states, entry.state.ID)
			s.lruList.Remove(oldest)
		}
	}

	entry := &stateEntry{
		state:     state,
		expiresAt: state.CreatedAt.Add(s.ttl),
	}
	elem := s.lruList.PushFront(entry)
	s.states[state.ID] = elem

	return nil
}

// Validate retrieves and removes an OIDC state entry (one-time use).
// Returns the state and true if valid, nil and false otherwise.
func (s *StateStore) Validate(stateID string) (*State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneExpiredLocked()

	elem, ok := s.states[stateID]
	if !ok {
		return nil, false
	}

	entry := elem.Value.(*stateEntry)
	if time.Now().After(entry.expiresAt) {
		delete(s.states, stateID)
		s.lruList.Remove(elem)
		return nil, false
	}

	delete(s.states, stateID)
	s.lruList.Remove(elem)

	return entry.state, true
}

func (s *StateStore) pruneExpiredLocked() {
	now := time.Now()
	for s.lruList.Len() > 0 {
		oldest := s.lruList.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*stateEntry)
		if now.After(entry.expiresAt) {
			delete(s.states, entry.state.ID)
			s.lruList.Remove(oldest)
		} else {
			// List is ordered by insertion time, so if oldest is not expired, none are.
			break
		}
	}
}

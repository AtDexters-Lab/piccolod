package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"piccolod/internal/persistence"
)

var (
	ErrWebAuthnDisabled  = errors.New("passkey authentication is disabled")
	ErrCeremonyNotFound  = errors.New("ceremony session not found or expired")
	ErrCeremonyStoreFull = errors.New("too many pending ceremonies")
)

type ceremonyType string

const (
	ceremonyRegistration   ceremonyType = "registration"
	ceremonyAuthentication ceremonyType = "authentication"
)

const (
	ceremonyMaxCapacity = 200
	ceremonyTTL         = 120 * time.Second
)

// WebAuthnManager handles WebAuthn registration and authentication ceremonies.
type WebAuthnManager struct {
	credRepo   persistence.WebAuthnCredentialRepo
	ceremonies *ceremonyStore
	disabled   bool
}

// NewWebAuthnManager creates a new WebAuthnManager.
func NewWebAuthnManager(credRepo persistence.WebAuthnCredentialRepo, disabled bool) *WebAuthnManager {
	return &WebAuthnManager{
		credRepo:   credRepo,
		ceremonies: newCeremonyStore(),
		disabled:   disabled,
	}
}

// newWebAuthn creates a per-request webauthn.WebAuthn instance for the given RP ID.
func newWebAuthn(rpID, rpDisplayName, rpOrigin string) (*webauthn.WebAuthn, error) {
	return webauthn.New(&webauthn.Config{
		RPDisplayName: rpDisplayName,
		RPID:          rpID,
		RPOrigins:     []string{rpOrigin},
	})
}

// webAuthnUser adapts a persistence.User + credentials to the webauthn.User interface.
type webAuthnUser struct {
	id          []byte
	name        string
	displayName string
	credentials []webauthn.Credential
}

func (u *webAuthnUser) WebAuthnID() []byte                         { return u.id }
func (u *webAuthnUser) WebAuthnName() string                       { return u.name }
func (u *webAuthnUser) WebAuthnDisplayName() string                { return u.displayName }
func (u *webAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

// toWebAuthnCredential converts a persistence credential to a webauthn.Credential.
func toWebAuthnCredential(c persistence.WebAuthnCredential) webauthn.Credential {
	credID, _ := base64.RawURLEncoding.DecodeString(c.ID)
	var transports []protocol.AuthenticatorTransport
	for _, t := range c.Transports {
		transports = append(transports, protocol.AuthenticatorTransport(t))
	}
	return webauthn.Credential{
		ID:              credID,
		PublicKey:       c.PublicKey,
		AttestationType: c.AttestationType,
		Transport:       transports,
		Authenticator: webauthn.Authenticator{
			AAGUID:    c.AAGUID,
			SignCount: c.SignCount,
		},
	}
}

// loadCredentials fetches and converts stored credentials for a user+RP pair.
func (m *WebAuthnManager) loadCredentials(ctx context.Context, userID, rpID string) ([]webauthn.Credential, error) {
	existing, err := m.credRepo.ListByUserAndRP(ctx, userID, rpID)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	var creds []webauthn.Credential
	for _, c := range existing {
		creds = append(creds, toWebAuthnCredential(c))
	}
	return creds, nil
}

// BeginRegistration starts a passkey registration ceremony.
func (m *WebAuthnManager) BeginRegistration(ctx context.Context, userID, username, rpID, rpDisplayName, rpOrigin string, userVerification protocol.UserVerificationRequirement) (*protocol.CredentialCreation, string, error) {
	if m.disabled {
		return nil, "", ErrWebAuthnDisabled
	}

	w, err := newWebAuthn(rpID, rpDisplayName, rpOrigin)
	if err != nil {
		return nil, "", fmt.Errorf("create webauthn: %w", err)
	}

	creds, err := m.loadCredentials(ctx, userID, rpID)
	if err != nil {
		return nil, "", err
	}

	user := &webAuthnUser{
		id:          []byte(userID),
		name:        username,
		displayName: username,
		credentials: creds,
	}

	options, session, err := w.BeginRegistration(user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			UserVerification: userVerification,
		}),
		webauthn.WithExclusions(webauthn.Credentials(creds).CredentialDescriptors()),
	)
	if err != nil {
		return nil, "", fmt.Errorf("begin registration: %w", err)
	}

	sessionID, err := m.ceremonies.Store(&ceremonySession{
		UserID:      userID,
		RPID:        rpID,
		RPOrigin:    rpOrigin,
		Type:        ceremonyRegistration,
		SessionData: session,
	})
	if err != nil {
		return nil, "", err
	}

	return options, sessionID, nil
}

// FinishRegistration completes registration and stores the credential.
func (m *WebAuthnManager) FinishRegistration(ctx context.Context, sessionID string, rpDisplayName string, parsedResponse *protocol.ParsedCredentialCreationData) (*persistence.WebAuthnCredential, error) {
	if m.disabled {
		return nil, ErrWebAuthnDisabled
	}

	cs, err := m.ceremonies.Consume(sessionID)
	if err != nil {
		return nil, err
	}
	if cs.Type != ceremonyRegistration {
		return nil, errors.New("invalid ceremony type")
	}

	w, err := newWebAuthn(cs.RPID, rpDisplayName, cs.RPOrigin)
	if err != nil {
		return nil, fmt.Errorf("create webauthn: %w", err)
	}

	creds, err := m.loadCredentials(ctx, cs.UserID, cs.RPID)
	if err != nil {
		return nil, err
	}

	user := &webAuthnUser{
		id:          []byte(cs.UserID),
		name:        "",
		displayName: "",
		credentials: creds,
	}

	credential, err := w.CreateCredential(user, *cs.SessionData, parsedResponse)
	if err != nil {
		return nil, fmt.Errorf("create credential: %w", err)
	}

	var transports []string
	for _, t := range credential.Transport {
		transports = append(transports, string(t))
	}

	now := time.Now().UTC()
	cred := persistence.WebAuthnCredential{
		ID:              base64.RawURLEncoding.EncodeToString(credential.ID),
		UserID:          cs.UserID,
		PublicKey:       credential.PublicKey,
		AttestationType: credential.AttestationType,
		Transports:      transports,
		SignCount:       credential.Authenticator.SignCount,
		RPID:            cs.RPID,
		AAGUID:          credential.Authenticator.AAGUID,
		FriendlyName:    "",
		CreatedAt:       now,
		LastUsedAt:      now,
	}

	if err := m.credRepo.Create(ctx, cred); err != nil {
		return nil, fmt.Errorf("store credential: %w", err)
	}

	return &cred, nil
}

// BeginAuthentication starts a passkey authentication ceremony (discoverable credentials).
func (m *WebAuthnManager) BeginAuthentication(ctx context.Context, rpID, rpDisplayName, rpOrigin string, userVerification protocol.UserVerificationRequirement) (*protocol.CredentialAssertion, string, error) {
	if m.disabled {
		return nil, "", ErrWebAuthnDisabled
	}

	w, err := newWebAuthn(rpID, rpDisplayName, rpOrigin)
	if err != nil {
		return nil, "", fmt.Errorf("create webauthn: %w", err)
	}

	options, session, err := w.BeginDiscoverableLogin(
		webauthn.WithUserVerification(userVerification),
	)
	if err != nil {
		return nil, "", fmt.Errorf("begin authentication: %w", err)
	}

	sessionID, err := m.ceremonies.Store(&ceremonySession{
		RPID:        rpID,
		RPOrigin:    rpOrigin,
		Type:        ceremonyAuthentication,
		SessionData: session,
	})
	if err != nil {
		return nil, "", err
	}

	return options, sessionID, nil
}

// FinishAuthentication completes authentication and returns the authenticated user ID.
func (m *WebAuthnManager) FinishAuthentication(ctx context.Context, sessionID, rpDisplayName string, parsedResponse *protocol.ParsedCredentialAssertionData) (string, error) {
	if m.disabled {
		return "", ErrWebAuthnDisabled
	}

	cs, err := m.ceremonies.Consume(sessionID)
	if err != nil {
		return "", err
	}
	if cs.Type != ceremonyAuthentication {
		return "", errors.New("invalid ceremony type")
	}

	w, err := newWebAuthn(cs.RPID, rpDisplayName, cs.RPOrigin)
	if err != nil {
		return "", fmt.Errorf("create webauthn: %w", err)
	}

	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		uid := string(userHandle)
		wCreds, lookupErr := m.loadCredentials(ctx, uid, cs.RPID)
		if lookupErr != nil {
			return nil, lookupErr
		}
		return &webAuthnUser{
			id:          userHandle,
			name:        "",
			displayName: "",
			credentials: wCreds,
		}, nil
	}

	credential, err := w.ValidateDiscoverableLogin(handler, *cs.SessionData, parsedResponse)
	if err != nil {
		return "", fmt.Errorf("validate login: %w", err)
	}

	credID := base64.RawURLEncoding.EncodeToString(credential.ID)
	if err := m.credRepo.UpdateAfterAuth(ctx, credID, credential.Authenticator.SignCount, time.Now().UTC()); err != nil {
		log.Printf("WARN: webauthn: update credential after auth: %v", err)
	}

	userID := string(parsedResponse.Response.UserHandle)
	return userID, nil
}

// ListCredentials returns all credentials for a user.
func (m *WebAuthnManager) ListCredentials(ctx context.Context, userID string) ([]persistence.WebAuthnCredential, error) {
	return m.credRepo.ListByUser(ctx, userID)
}

// DeleteCredential removes a credential.
func (m *WebAuthnManager) DeleteCredential(ctx context.Context, credID string) error {
	return m.credRepo.Delete(ctx, credID)
}

// GetCredential returns a single credential by ID.
func (m *WebAuthnManager) GetCredential(ctx context.Context, credID string) (persistence.WebAuthnCredential, error) {
	return m.credRepo.Get(ctx, credID)
}

// RenameCredential updates the friendly name for a credential.
func (m *WebAuthnManager) RenameCredential(ctx context.Context, credID, name string) error {
	return m.credRepo.UpdateFriendlyName(ctx, credID, name)
}

// HasCredentialsForRP checks if a user has any credentials for a specific RP ID.
func (m *WebAuthnManager) HasCredentialsForRP(ctx context.Context, userID, rpID string) (bool, error) {
	count, err := m.credRepo.CountByUserAndRP(ctx, userID, rpID)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// CountCredentials returns the total credential count for a user.
func (m *WebAuthnManager) CountCredentials(ctx context.Context, userID string) (int, error) {
	return m.credRepo.CountByUser(ctx, userID)
}

// CountByUserSplitRP returns both total credential count and RP-specific count in a single query.
func (m *WebAuthnManager) CountByUserSplitRP(ctx context.Context, userID, rpID string) (int, int, error) {
	return m.credRepo.CountByUserSplitRP(ctx, userID, rpID)
}

// --- Ceremony Store ---

type ceremonySession struct {
	UserID      string
	RPID        string
	RPOrigin    string
	Type        ceremonyType
	SessionData *webauthn.SessionData
	ExpiresAt   time.Time
}

type ceremonyStore struct {
	mu       sync.Mutex
	sessions map[string]*ceremonySession
}

func newCeremonyStore() *ceremonyStore {
	return &ceremonyStore{
		sessions: make(map[string]*ceremonySession),
	}
}

func (s *ceremonyStore) Store(cs *ceremonySession) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Lazy eviction of expired entries
	now := time.Now()
	for id, sess := range s.sessions {
		if sess.ExpiresAt.Before(now) {
			delete(s.sessions, id)
		}
	}

	if len(s.sessions) >= ceremonyMaxCapacity {
		return "", ErrCeremonyStoreFull
	}

	id, err := generateSecureToken()
	if err != nil {
		return "", err
	}

	cs.ExpiresAt = now.Add(ceremonyTTL)
	s.sessions[id] = cs
	return id, nil
}

// Consume retrieves and atomically deletes a ceremony session (single-use).
func (s *ceremonyStore) Consume(id string) (*ceremonySession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cs, ok := s.sessions[id]
	if !ok || cs.ExpiresAt.Before(time.Now()) {
		if ok {
			delete(s.sessions, id)
		}
		return nil, ErrCeremonyNotFound
	}
	delete(s.sessions, id)
	return cs, nil
}

func generateSecureToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

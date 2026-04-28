package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"piccolod/internal/cryptoutil"
	"piccolod/internal/persistence"
)

var (
	ErrWebAuthnDisabled  = errors.New("passkey authentication is disabled")
	ErrCeremonyNotFound  = errors.New("ceremony session not found or expired")
	ErrCeremonyStoreFull = errors.New("too many pending ceremonies")

	// ErrPasskeyAlreadyRegistered indicates a registration ceremony returned a
	// credential ID that already exists — a benign duplicate. The frontend
	// shows a non-destructive "already registered" message.
	ErrPasskeyAlreadyRegistered = errors.New("passkey already registered")

	// ErrCredentialNotFoundInDB indicates a discoverable login asserted a
	// credential ID that is not registered server-side. Distinct from generic
	// auth failures so handlers can include a Signal API hint to prune the
	// stale picker entry without leaking other validation failure modes.
	ErrCredentialNotFoundInDB = errors.New("credential not found in db")
)

// AlreadyRegisteredError carries the existing credential ID alongside
// ErrPasskeyAlreadyRegistered. Wrap-checked via errors.As.
type AlreadyRegisteredError struct {
	CredentialID string
	RPID         string
}

func (e *AlreadyRegisteredError) Error() string {
	return ErrPasskeyAlreadyRegistered.Error()
}

func (e *AlreadyRegisteredError) Unwrap() error { return ErrPasskeyAlreadyRegistered }

// CredentialNotFoundError carries the missing credential ID and RP ID alongside
// ErrCredentialNotFoundInDB. Wrap-checked via errors.As.
type CredentialNotFoundError struct {
	CredentialID string
	RPID         string
}

func (e *CredentialNotFoundError) Error() string {
	return ErrCredentialNotFoundInDB.Error()
}

func (e *CredentialNotFoundError) Unwrap() error { return ErrCredentialNotFoundInDB }

type ceremonyType string

const (
	ceremonyRegistration   ceremonyType = "registration"
	ceremonyAuthentication ceremonyType = "authentication"
)

const (
	ceremonyMaxCapacity = 200
	ceremonyTTL         = 120 * time.Second
	ceremonyIDBytes     = 32 // 256-bit ceremony session ID
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
		Flags: webauthn.CredentialFlags{
			BackupEligible: c.BackupEligible,
			BackupState:    c.BackupState,
		},
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
// registrationHost is the normalized request hostname used for browser picker
// disambiguation (user.name) and credential FriendlyName.
func (m *WebAuthnManager) BeginRegistration(ctx context.Context, userID, username, rpID, rpDisplayName, rpOrigin, registrationHost string, userVerification protocol.UserVerificationRequirement) (*protocol.CredentialCreation, string, error) {
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

	// Construct picker-friendly name: "admin on jane404" for browser credential UI.
	pickerName := username
	if registrationHost != "" {
		pickerName = username + " on " + ShortHostLabel(registrationHost, rpID)
	}

	user := &webAuthnUser{
		id:          []byte(userID),
		name:        pickerName,
		displayName: username,
		credentials: creds,
	}

	// Single AuthenticatorSelection struct: WithAuthenticatorSelection replaces the
	// entire struct, so residentKey/requireResidentKey/userVerification must be set
	// together. Splitting these across multiple options silently wipes residentKey
	// and produces non-discoverable credentials that won't appear in the picker.
	options, session, err := w.BeginRegistration(user,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   userVerification,
		}),
		webauthn.WithExclusions(webauthn.Credentials(creds).CredentialDescriptors()),
	)
	if err != nil {
		return nil, "", fmt.Errorf("begin registration: %w", err)
	}

	sessionID, err := m.ceremonies.Store(&ceremonySession{
		UserID:           userID,
		RPID:             rpID,
		RPOrigin:         rpOrigin,
		RegistrationHost: registrationHost,
		Type:             ceremonyRegistration,
		SessionData:      session,
	})
	if err != nil {
		return nil, "", err
	}

	return options, sessionID, nil
}

// FinishRegistration completes registration and stores the credential.
// BeginRegistration sets residentKey=Required so every persisted credential
// is guaranteed discoverable. If the response carries a credential ID that
// already exists for this user, returns an *AlreadyRegisteredError.
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

	credID := base64.RawURLEncoding.EncodeToString(credential.ID)

	// Re-registration over an existing entry: some authenticators ignore the
	// excludeCredentials hint and return a credential whose ID is already in
	// our DB. Surface AlreadyRegisteredError so the frontend shows a
	// non-destructive "already registered" message.
	if existing, getErr := m.credRepo.Get(ctx, credID); getErr == nil {
		// Cross-user collision: credential ID already belongs to a DIFFERENT
		// user. Extremely unlikely (credential IDs are ≥16 random bytes), but
		// echoing the foreign credential ID to the caller would drive a dead-
		// end delete flow (DELETE is cross-user-gated → 403). Return a
		// generic registration failure instead.
		if existing.UserID != cs.UserID {
			return nil, fmt.Errorf("create credential: id collision")
		}
		return nil, &AlreadyRegisteredError{
			CredentialID: existing.ID,
			RPID:         existing.RPID,
		}
	}

	var transports []string
	for _, t := range credential.Transport {
		transports = append(transports, string(t))
	}

	now := time.Now().UTC()
	cred := persistence.WebAuthnCredential{
		ID:              credID,
		UserID:          cs.UserID,
		PublicKey:       credential.PublicKey,
		AttestationType: credential.AttestationType,
		Transports:      transports,
		SignCount:       credential.Authenticator.SignCount,
		RPID:            cs.RPID,
		AAGUID:          credential.Authenticator.AAGUID,
		FriendlyName:    truncate(cs.RegistrationHost, 100),
		BackupEligible:  credential.Flags.BackupEligible,
		BackupState:     credential.Flags.BackupState,
		CreatedAt:       now,
		LastUsedAt:      time.Time{},
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

// AuthenticationResult bundles the authenticated user ID after a successful
// passkey assertion.
type AuthenticationResult struct {
	UserID string
}

// FinishAuthentication completes authentication and returns the authenticated user.
//
// If the parsed response carries a credential ID that does not exist in the DB,
// returns *CredentialNotFoundError so the handler can include a Signal API
// pruning hint without conflating with other validation failures.
func (m *WebAuthnManager) FinishAuthentication(ctx context.Context, sessionID, rpDisplayName string, parsedResponse *protocol.ParsedCredentialAssertionData) (*AuthenticationResult, error) {
	if m.disabled {
		return nil, ErrWebAuthnDisabled
	}

	cs, err := m.ceremonies.Consume(sessionID)
	if err != nil {
		return nil, err
	}
	if cs.Type != ceremonyAuthentication {
		return nil, errors.New("invalid ceremony type")
	}

	w, err := newWebAuthn(cs.RPID, rpDisplayName, cs.RPOrigin)
	if err != nil {
		return nil, fmt.Errorf("create webauthn: %w", err)
	}

	// Pre-check: if the asserted credential ID is not in our DB, surface a
	// typed sentinel so the handler can emit a signalUnknownCredential hint.
	// This is checked before ValidateDiscoverableLogin so the "genuine DB
	// miss" condition is mechanically distinct from signature/origin/etc.
	// failures, which the library surfaces via generic *protocol.Error and
	// would otherwise be brittle to pattern-match.
	assertedCredID := base64.RawURLEncoding.EncodeToString(parsedResponse.RawID)
	if _, getErr := m.credRepo.Get(ctx, assertedCredID); getErr != nil {
		if errors.Is(getErr, persistence.ErrNotFound) {
			return nil, &CredentialNotFoundError{
				CredentialID: assertedCredID,
				RPID:         cs.RPID,
			}
		}
		return nil, fmt.Errorf("lookup credential: %w", getErr)
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
		return nil, fmt.Errorf("validate login: %w", err)
	}

	credID := base64.RawURLEncoding.EncodeToString(credential.ID)
	if err := m.credRepo.UpdateAfterAuth(ctx, credID, credential.Authenticator.SignCount, time.Now().UTC()); err != nil {
		log.Printf("WARN: webauthn: update credential after auth: %v", err)
	}

	return &AuthenticationResult{
		UserID: string(parsedResponse.Response.UserHandle),
	}, nil
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

// UserIDsWithPasskeyForRP returns the set of user IDs that have at least one
// credential for the given RP ID. Single query, avoids N+1 per-user lookups
// and avoids pulling credential blobs (uses ListUserIDsByRP).
func (m *WebAuthnManager) UserIDsWithPasskeyForRP(ctx context.Context, rpID string) (map[string]struct{}, error) {
	uids, err := m.credRepo.ListUserIDsByRP(ctx, rpID)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(uids))
	for _, uid := range uids {
		set[uid] = struct{}{}
	}
	return set, nil
}

// CountCredentials returns the total credential count for a user.
func (m *WebAuthnManager) CountCredentials(ctx context.Context, userID string) (int, error) {
	return m.credRepo.CountByUser(ctx, userID)
}

// CountByUserAndRP returns the credential count for the (user, rp) pair.
func (m *WebAuthnManager) CountByUserAndRP(ctx context.Context, userID, rpID string) (int, error) {
	return m.credRepo.CountByUserAndRP(ctx, userID, rpID)
}

// --- Ceremony Store ---

type ceremonySession struct {
	UserID           string
	RPID             string
	RPOrigin         string
	RegistrationHost string // normalized request host at registration time
	Type             ceremonyType
	SessionData      *webauthn.SessionData
	ExpiresAt        time.Time
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

	id, err := cryptoutil.GenerateSecureToken(ceremonyIDBytes)
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

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

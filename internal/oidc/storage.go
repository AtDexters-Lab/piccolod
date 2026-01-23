package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"

	"piccolod/internal/persistence"
)

// ErrInvalidRefreshToken is returned when a token is not a refresh token.
var ErrInvalidRefreshToken = errors.New("invalid refresh token")

// portalSessionIDContextKey is used to pass portal session ID through context.
// RFC 20260122 §6.3: Links auth codes to portal sessions for logout propagation.
type portalSessionIDContextKey struct{}

// WithPortalSessionID adds the portal session ID to the context.
func WithPortalSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, portalSessionIDContextKey{}, sessionID)
}

// portalSessionIDFromContext extracts the portal session ID from the context.
func portalSessionIDFromContext(ctx context.Context) string {
	if v := ctx.Value(portalSessionIDContextKey{}); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func (s *Storage) ensureUserAllowedForClient(ctx context.Context, userID, clientID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return errors.New("user id required")
	}
	client, err := s.clients.Get(ctx, clientID)
	if err != nil {
		return fmt.Errorf("client not found: %w", err)
	}
	user, err := s.users.Get(ctx, userID)
	if err != nil {
		return fmt.Errorf("user not found: %w", err)
	}

	if user.Role != persistence.UserRoleStandard {
		return nil
	}

	allowed := false
	for _, appID := range user.AllowedApps {
		if appID == client.AppID {
			allowed = true
			break
		}
	}
	if !allowed {
		return oidc.ErrAccessDenied().WithDescription("access denied")
	}
	return nil
}

// Storage implements op.Storage using our persistence layer.
type Storage struct {
	users         persistence.UserRepo
	clients       persistence.OIDCClientRepo
	keys          persistence.OIDCKeyRepo
	authCodes     persistence.OIDCAuthCodeRepo
	refreshTokens persistence.OIDCRefreshTokenRepo

	// In-memory auth request store (short-lived, no need for persistence)
	authRequests   map[string]*AuthRequest
	authRequestsMu sync.RWMutex

	// In-memory metadata for auth codes (needed for hybrid token auth)
	consumedAuthCodes   map[string]consumedAuthCodeMeta
	consumedAuthCodesMu sync.Mutex

	// Auth request limits
	maxAuthRequests int
	authRequestTTL  time.Duration

	// Shutdown signal for cleanup goroutine
	stopCleanup chan struct{}

	// Cached signing key
	signingKey   *signingKey
	signingKeyMu sync.RWMutex

	// App redirect resolver
	resolveRedirect func(ctx context.Context, appID string) ([]string, error)

	logger *slog.Logger
}

type signingKey struct {
	kid        string
	privateKey *rsa.PrivateKey
}

type consumedAuthCodeMeta struct {
	ClientID          string
	RedirectURI       string
	AllowPKCEOnlyAuth bool
	ConsumedAt        time.Time
}

// StorageConfig configures the OIDC storage.
type StorageConfig struct {
	Users           persistence.UserRepo
	Clients         persistence.OIDCClientRepo
	Keys            persistence.OIDCKeyRepo
	AuthCodes       persistence.OIDCAuthCodeRepo
	RefreshTokens   persistence.OIDCRefreshTokenRepo
	ResolveRedirect func(ctx context.Context, appID string) ([]string, error)
	Logger          *slog.Logger

	// Auth request limits (optional, defaults applied if zero)
	MaxAuthRequests int           // Default: 1000
	AuthRequestTTL  time.Duration // Default: 10 minutes
}

// NewStorage creates a new OIDC storage backed by our persistence layer.
func NewStorage(cfg StorageConfig) *Storage {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	// Apply defaults for auth request limits
	maxAuthRequests := cfg.MaxAuthRequests
	if maxAuthRequests == 0 {
		maxAuthRequests = 1000
	}
	authRequestTTL := cfg.AuthRequestTTL
	if authRequestTTL == 0 {
		authRequestTTL = 10 * time.Minute
	}

	s := &Storage{
		users:             cfg.Users,
		clients:           cfg.Clients,
		keys:              cfg.Keys,
		authCodes:         cfg.AuthCodes,
		refreshTokens:     cfg.RefreshTokens,
		authRequests:      make(map[string]*AuthRequest),
		consumedAuthCodes: make(map[string]consumedAuthCodeMeta),
		maxAuthRequests:   maxAuthRequests,
		authRequestTTL:    authRequestTTL,
		stopCleanup:       make(chan struct{}),
		resolveRedirect:   cfg.ResolveRedirect,
		logger:            cfg.Logger,
	}
	if s.resolveRedirect == nil {
		s.resolveRedirect = func(ctx context.Context, appID string) ([]string, error) { return nil, nil }
	}

	// Start background cleanup goroutine
	go s.authRequestCleanupLoop()

	return s
}

// Ensure Storage implements op.Storage
var _ op.Storage = (*Storage)(nil)

// -----------------------------------------------------------------------------
// Health check
// -----------------------------------------------------------------------------

func (s *Storage) Health(ctx context.Context) error {
	// Simple health check - try to list users
	_, err := s.users.Count(ctx)
	return err
}

// -----------------------------------------------------------------------------
// Auth request cleanup
// -----------------------------------------------------------------------------

// authRequestCleanupLoop runs periodically to remove expired auth requests.
func (s *Storage) authRequestCleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.cleanupExpiredAuthRequests()
		case <-s.stopCleanup:
			return
		}
	}
}

// Close stops background goroutines. Safe to call multiple times.
func (s *Storage) Close() {
	select {
	case <-s.stopCleanup:
		// Already closed
	default:
		close(s.stopCleanup)
	}
}

// cleanupExpiredAuthRequests removes auth requests older than the TTL.
func (s *Storage) cleanupExpiredAuthRequests() {
	s.authRequestsMu.Lock()
	defer s.authRequestsMu.Unlock()

	now := time.Now()
	cleaned := 0
	for id, req := range s.authRequests {
		if now.Sub(req.CreatedAt) > s.authRequestTTL {
			delete(s.authRequests, id)
			cleaned++
		}
	}

	if cleaned > 0 {
		s.logger.Debug("cleaned up expired auth requests", "count", cleaned)
	}

	// Best-effort cleanup for consumed auth-code metadata (should usually be deleted during token exchange).
	s.consumedAuthCodesMu.Lock()
	for code, meta := range s.consumedAuthCodes {
		if now.Sub(meta.ConsumedAt) > 5*time.Minute {
			delete(s.consumedAuthCodes, code)
		}
	}
	s.consumedAuthCodesMu.Unlock()
}

// -----------------------------------------------------------------------------
// Client operations (OPStorage)
// -----------------------------------------------------------------------------

func (s *Storage) GetClientByClientID(ctx context.Context, clientID string) (op.Client, error) {
	client, err := s.clients.Get(ctx, clientID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			// Generic error to prevent client ID enumeration
			return nil, errors.New("invalid client")
		}
		return nil, err
	}

	redirectURIs, err := s.resolveRedirect(ctx, client.AppID)
	if err != nil {
		return nil, err
	}

	return &Client{
		id:           client.ID,
		secret:       client.Secret,
		appID:        client.AppID,
		redirectURIs: redirectURIs,
	}, nil
}

func (s *Storage) AuthorizeClientIDSecret(ctx context.Context, clientID, clientSecret string) error {
	exchange, hasExchange := TokenExchangeContextFrom(ctx)
	if hasExchange && exchange.Code != "" {
		defer func() {
			s.consumedAuthCodesMu.Lock()
			delete(s.consumedAuthCodes, exchange.Code)
			s.consumedAuthCodesMu.Unlock()
		}()
	}

	client, err := s.clients.Get(ctx, clientID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			// Log for security monitoring, return generic error to prevent enumeration
			s.logger.Warn("OIDC client auth failed: unknown client", "client_id", clientID)
			return errors.New("invalid client credentials")
		}
		return err
	}

	// Hybrid model: allow token exchange without a client secret only when PKCE is present and the
	// original redirect URI was loopback/custom-scheme and matches the token request redirect_uri.
	if clientSecret == "" {
		if !hasExchange || strings.TrimSpace(exchange.Code) == "" || strings.TrimSpace(exchange.RedirectURI) == "" {
			s.logger.Warn("OIDC client auth failed: missing secret and no token exchange context", "client_id", clientID)
			return errors.New("invalid client credentials")
		}

		s.consumedAuthCodesMu.Lock()
		meta, ok := s.consumedAuthCodes[exchange.Code]
		s.consumedAuthCodesMu.Unlock()
		if !ok {
			s.logger.Warn("OIDC client auth failed: missing auth code metadata", "client_id", clientID)
			return errors.New("invalid client credentials")
		}

		if meta.ClientID != clientID {
			s.logger.Warn("OIDC client auth failed: client_id mismatch for auth code", "client_id", clientID)
			return errors.New("invalid client credentials")
		}
		if meta.RedirectURI != exchange.RedirectURI {
			s.logger.Warn("OIDC client auth failed: redirect_uri mismatch for auth code", "client_id", clientID)
			return errors.New("invalid client credentials")
		}
		if !meta.AllowPKCEOnlyAuth {
			s.logger.Warn("OIDC client auth failed: pkce-only auth not allowed for redirect uri", "client_id", clientID)
			return errors.New("invalid client credentials")
		}

		s.logger.Info("OIDC client auth: allowed pkce-only token exchange", "client_id", clientID, "app_id", client.AppID)
		return nil
	}

	if !VerifyClientSecret(client.Secret, clientSecret) {
		s.logger.Warn("OIDC client auth failed: invalid secret", "client_id", clientID)
		return errors.New("invalid client credentials")
	}

	return nil
}

func (s *Storage) SetUserinfoFromScopes(ctx context.Context, userinfo *oidc.UserInfo, userID, clientID string, scopes []string) error {
	// Deprecated - use SetUserinfoFromRequest instead
	user, err := s.users.Get(ctx, userID)
	if err != nil {
		return err
	}

	info := UserInfoClaims(&user, scopes)
	userinfo.Subject = info.Subject
	userinfo.Name = info.Name
	userinfo.PreferredUsername = info.PreferredUsername
	userinfo.Email = info.Email
	userinfo.EmailVerified = info.EmailVerified

	return nil
}

func (s *Storage) SetUserinfoFromToken(ctx context.Context, userinfo *oidc.UserInfo, tokenID, subject, origin string) error {
	user, err := s.users.Get(ctx, subject)
	if err != nil {
		return err
	}

	// Default to all scopes for token-based userinfo
	scopes := []string{oidc.ScopeOpenID, oidc.ScopeProfile, oidc.ScopeEmail}
	info := UserInfoClaims(&user, scopes)
	userinfo.Subject = info.Subject
	userinfo.Name = info.Name
	userinfo.PreferredUsername = info.PreferredUsername
	userinfo.Email = info.Email
	userinfo.EmailVerified = info.EmailVerified

	return nil
}

func (s *Storage) SetIntrospectionFromToken(ctx context.Context, introspection *oidc.IntrospectionResponse, tokenID, subject, clientID string) error {
	user, err := s.users.Get(ctx, subject)
	if err != nil {
		introspection.Active = false
		return nil
	}

	introspection.Active = true
	introspection.Subject = user.ID
	introspection.Username = user.Username
	introspection.ClientID = clientID

	return nil
}

func (s *Storage) GetPrivateClaimsFromScopes(ctx context.Context, userID, clientID string, scopes []string) (map[string]any, error) {
	user, err := s.users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}

	return GetPrivateClaimsFromScopes(ctx, &user, scopes), nil
}

func (s *Storage) GetKeyByIDAndClientID(ctx context.Context, keyID, clientID string) (*jose.JSONWebKey, error) {
	// JWT Profile Grant is not supported
	return nil, errors.New("JWT profile not supported")
}

func (s *Storage) ValidateJWTProfileScopes(ctx context.Context, userID string, scopes []string) ([]string, error) {
	// JWT Profile Grant is not supported
	return nil, errors.New("JWT profile not supported")
}

// -----------------------------------------------------------------------------
// Auth request operations (AuthStorage)
// -----------------------------------------------------------------------------

func (s *Storage) CreateAuthRequest(ctx context.Context, authReq *oidc.AuthRequest, userID string) (op.AuthRequest, error) {
	// RFC 20260112: PKCE required for all authorization flows (S256 only).
	if strings.TrimSpace(authReq.CodeChallenge) == "" {
		return nil, oidc.ErrInvalidRequest().WithDescription("code_challenge required")
	}
	if authReq.CodeChallengeMethod != oidc.CodeChallengeMethodS256 {
		return nil, oidc.ErrInvalidRequest().WithDescription("code_challenge_method must be S256")
	}

	if strings.TrimSpace(userID) != "" {
		if err := s.ensureUserAllowedForClient(ctx, userID, authReq.ClientID); err != nil {
			return nil, err
		}
	}

	// Check limit before creating
	s.authRequestsMu.RLock()
	currentCount := len(s.authRequests)
	s.authRequestsMu.RUnlock()

	if currentCount >= s.maxAuthRequests {
		return nil, errors.New("too many pending auth requests")
	}

	id, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("generate request ID: %w", err)
	}

	codeChallenge := &oidc.CodeChallenge{
		Challenge: authReq.CodeChallenge,
		Method:    authReq.CodeChallengeMethod,
	}

	now := time.Now().UTC()
	request := &AuthRequest{
		ID:              id,
		ClientID:        authReq.ClientID,
		RedirectURI:     authReq.RedirectURI,
		Scopes:          authReq.Scopes,
		State:           authReq.State,
		Nonce:           authReq.Nonce,
		ResponseType:    authReq.ResponseType,
		ResponseMode:    authReq.ResponseMode,
		CodeChallenge:   codeChallenge,
		UserID:          userID,
		IsDone:          userID != "",
		PortalSessionID: portalSessionIDFromContext(ctx), // RFC 20260122 §6.3
		CreatedAt:       now,
	}
	if userID != "" {
		request.AuthTime = now
	}

	s.logger.Debug("OIDC CreateAuthRequest",
		"id", id,
		"client_id", authReq.ClientID,
		"redirect_uri", authReq.RedirectURI,
		"scopes", authReq.Scopes,
		"user_id", userID,
	)

	s.authRequestsMu.Lock()
	s.authRequests[id] = request
	s.authRequestsMu.Unlock()

	return request, nil
}

func (s *Storage) AuthRequestByID(ctx context.Context, id string) (op.AuthRequest, error) {
	s.authRequestsMu.RLock()
	request, ok := s.authRequests[id]
	s.authRequestsMu.RUnlock()

	if !ok {
		s.logger.Warn("OIDC AuthRequestByID not found", "id", id)
		return nil, fmt.Errorf("auth request not found: %s", id)
	}

	s.logger.Debug("OIDC AuthRequestByID found",
		"id", id,
		"client_id", request.ClientID,
		"redirect_uri", request.RedirectURI,
		"user_id", request.UserID,
		"is_done", request.IsDone,
	)
	return request, nil
}

func (s *Storage) AuthRequestByCode(ctx context.Context, code string) (op.AuthRequest, error) {
	authCode, err := s.authCodes.Consume(ctx, code)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, fmt.Errorf("auth code not found or expired")
		}
		return nil, err
	}

	var codeChallenge *oidc.CodeChallenge
	if authCode.CodeChallenge != "" {
		codeChallenge = &oidc.CodeChallenge{
			Challenge: authCode.CodeChallenge,
			Method:    oidc.CodeChallengeMethod(authCode.CodeChallengeMethod),
		}
	}

	// Store metadata for hybrid token endpoint authentication.
	// This is read later by AuthorizeClientIDSecret for pkce-only (native) flows.
	allowPKCEOnly := false
	if codeChallenge != nil {
		allowPKCEOnly = isLoopbackOrCustomSchemeRedirectURI(authCode.RedirectURI)
	}
	s.consumedAuthCodesMu.Lock()
	s.consumedAuthCodes[authCode.Code] = consumedAuthCodeMeta{
		ClientID:          authCode.ClientID,
		RedirectURI:       authCode.RedirectURI,
		AllowPKCEOnlyAuth: allowPKCEOnly,
		ConsumedAt:        time.Now().UTC(),
	}
	s.consumedAuthCodesMu.Unlock()

	return &AuthRequest{
		ID:              authCode.Code,
		ClientID:        authCode.ClientID,
		RedirectURI:     authCode.RedirectURI,
		Scopes:          strings.Split(authCode.Scope, " "),
		Nonce:           authCode.Nonce,
		CodeChallenge:   codeChallenge,
		UserID:          authCode.UserID,
		AuthTime:        authCode.CreatedAt,
		IsDone:          true,
		PortalSessionID: authCode.PortalSessionID, // RFC 20260122 §6.3
	}, nil
}

func (s *Storage) SaveAuthCode(ctx context.Context, id, code string) error {
	s.authRequestsMu.RLock()
	request, ok := s.authRequests[id]
	s.authRequestsMu.RUnlock()

	if !ok {
		return fmt.Errorf("auth request not found: %s", id)
	}

	var challengeMethod string
	var challenge string
	if request.CodeChallenge != nil {
		challengeMethod = string(request.CodeChallenge.Method)
		challenge = request.CodeChallenge.Challenge
	}

	authCode := persistence.OIDCAuthCode{
		Code:                code,
		ClientID:            request.ClientID,
		UserID:              request.UserID,
		RedirectURI:         request.RedirectURI,
		Scope:               strings.Join(request.Scopes, " "),
		Nonce:               request.Nonce,
		CodeChallenge:       challenge,
		CodeChallengeMethod: challengeMethod,
		PortalSessionID:     request.PortalSessionID, // RFC 20260122 §6.3
		ExpiresAt:           time.Now().UTC().Add(10 * time.Minute),
		CreatedAt:           time.Now().UTC(),
	}

	return s.authCodes.Store(ctx, authCode)
}

func (s *Storage) DeleteAuthRequest(ctx context.Context, id string) error {
	s.authRequestsMu.Lock()
	delete(s.authRequests, id)
	s.authRequestsMu.Unlock()
	return nil
}

// CompleteAuthRequest marks an auth request as authenticated.
// portalSessionID links the auth flow to the portal session for logout propagation (RFC 20260122 §6.3).
func (s *Storage) CompleteAuthRequest(ctx context.Context, id, userID, portalSessionID string) error {
	s.logger.Debug("OIDC CompleteAuthRequest starting", "id", id, "user_id", userID)

	// 1. Get Request (Read Lock)
	s.authRequestsMu.RLock()
	request, ok := s.authRequests[id]
	clientID := ""
	if ok {
		clientID = request.ClientID
		s.logger.Debug("OIDC CompleteAuthRequest found request",
			"id", id,
			"client_id", clientID,
			"redirect_uri", request.RedirectURI,
		)
	}
	s.authRequestsMu.RUnlock()

	if !ok {
		s.logger.Warn("OIDC CompleteAuthRequest request not found", "id", id)
		return fmt.Errorf("auth request not found: %s", id)
	}

	// 2. Validate Access (DB Ops)
	if err := s.ensureUserAllowedForClient(ctx, userID, clientID); err != nil {
		return err
	}

	// 3. Update Request (Write Lock)
	s.authRequestsMu.Lock()
	defer s.authRequestsMu.Unlock()

	// Re-check existence and verify clientID hasn't changed (TOCTOU protection)
	request, ok = s.authRequests[id]
	if !ok {
		return fmt.Errorf("auth request not found: %s", id)
	}
	if request.ClientID != clientID {
		return errors.New("auth request modified during validation")
	}

	request.UserID = userID
	request.AuthTime = time.Now().UTC()
	request.IsDone = true
	if portalSessionID != "" {
		request.PortalSessionID = portalSessionID
	}
	return nil
}

// -----------------------------------------------------------------------------
// Token operations (AuthStorage)
// -----------------------------------------------------------------------------

func (s *Storage) CreateAccessToken(ctx context.Context, request op.TokenRequest) (string, time.Time, error) {
	// Access tokens are JWTs signed by the provider.
	// We return a unique ID (JTI) for the token.
	id, err := generateID()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate token ID: %w", err)
	}
	expiry := time.Now().UTC().Add(15 * time.Minute)
	return id, expiry, nil
}

func (s *Storage) CreateAccessAndRefreshTokens(ctx context.Context, request op.TokenRequest, currentRefreshToken string) (accessTokenID string, newRefreshToken string, expiration time.Time, err error) {
	accessTokenID, err = generateID()
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("generate access token ID: %w", err)
	}
	expiry := time.Now().UTC().Add(15 * time.Minute)

	// Revoke old refresh token if rotating
	if currentRefreshToken != "" {
		if err := s.refreshTokens.Revoke(ctx, currentRefreshToken); err != nil {
			s.logger.Warn("failed to revoke old refresh token during rotation", "error", err)
		}
	}

	// Check if offline_access scope is requested
	var scopes []string
	if scopeReq, ok := request.(interface{ GetScopes() []string }); ok {
		scopes = scopeReq.GetScopes()
	}

	hasOfflineAccess := false
	for _, scope := range scopes {
		if scope == oidc.ScopeOfflineAccess {
			hasOfflineAccess = true
			break
		}
	}

	// Only create refresh token if offline_access scope is granted
	if hasOfflineAccess {
		newRefreshToken, err = generateID()
		if err != nil {
			return "", "", time.Time{}, fmt.Errorf("generate refresh token: %w", err)
		}
		refreshExpiry := time.Now().UTC().Add(7 * 24 * time.Hour) // 7 days

		// Get client ID from audience (first audience is typically the client)
		clientID := ""
		if audiences := request.GetAudience(); len(audiences) > 0 {
			clientID = audiences[0]
		}

		token := persistence.OIDCRefreshToken{
			Token:     newRefreshToken,
			ClientID:  clientID,
			UserID:    request.GetSubject(),
			Scope:     strings.Join(scopes, " "),
			ExpiresAt: refreshExpiry,
			CreatedAt: time.Now().UTC(),
		}

		if err := s.refreshTokens.Store(ctx, token); err != nil {
			return "", "", time.Time{}, err
		}
	}

	return accessTokenID, newRefreshToken, expiry, nil
}

func (s *Storage) TokenRequestByRefreshToken(ctx context.Context, refreshToken string) (op.RefreshTokenRequest, error) {
	token, err := s.refreshTokens.Get(ctx, refreshToken)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, fmt.Errorf("refresh token not found or revoked")
		}
		return nil, err
	}

	if time.Now().UTC().After(token.ExpiresAt) {
		if err := s.refreshTokens.Revoke(ctx, refreshToken); err != nil {
			s.logger.Warn("failed to revoke expired refresh token", "error", err)
		}
		return nil, fmt.Errorf("refresh token expired")
	}

	user, err := s.users.Get(ctx, token.UserID)
	if err != nil {
		return nil, fmt.Errorf("user not found: %w", err)
	}

	// Validate Access (Enforce allowed_apps on refresh)
	if err := s.ensureUserAllowedForClient(ctx, token.UserID, token.ClientID); err != nil {
		return nil, err
	}

	return &RefreshTokenRequest{
		Token:    token,
		User:     user,
		ClientID: token.ClientID,
		Scopes:   strings.Split(token.Scope, " "),
		AuthTime: token.CreatedAt,
	}, nil
}

func (s *Storage) TerminateSession(ctx context.Context, userID, clientID string) error {
	return s.refreshTokens.RevokeByUserAndClient(ctx, userID, clientID)
}

// RevokeToken revokes a token (refresh token).
func (s *Storage) RevokeToken(ctx context.Context, tokenOrTokenID string, userID string, clientID string) *oidc.Error {
	err := s.refreshTokens.Revoke(ctx, tokenOrTokenID)
	if err != nil {
		return oidc.ErrServerError().WithDescription("token revocation failed: %s", err.Error())
	}
	return nil
}

// GetRefreshTokenInfo returns information about a refresh token.
func (s *Storage) GetRefreshTokenInfo(ctx context.Context, clientID string, token string) (userID string, tokenID string, err error) {
	refreshToken, err := s.refreshTokens.Get(ctx, token)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return "", "", ErrInvalidRefreshToken
		}
		return "", "", err
	}

	if refreshToken.ClientID != clientID {
		return "", "", ErrInvalidRefreshToken
	}

	return refreshToken.UserID, refreshToken.Token, nil
}

// -----------------------------------------------------------------------------
// Key operations (AuthStorage)
// -----------------------------------------------------------------------------

func (s *Storage) SigningKey(ctx context.Context) (op.SigningKey, error) {
	s.signingKeyMu.RLock()
	if s.signingKey != nil {
		defer s.signingKeyMu.RUnlock()
		return &opSigningKey{
			id:         s.signingKey.kid,
			algorithm:  jose.RS256,
			privateKey: s.signingKey.privateKey,
		}, nil
	}
	s.signingKeyMu.RUnlock()

	// Try to load from database
	s.signingKeyMu.Lock()
	defer s.signingKeyMu.Unlock()

	// Double-check after acquiring write lock
	if s.signingKey != nil {
		return &opSigningKey{
			id:         s.signingKey.kid,
			algorithm:  jose.RS256,
			privateKey: s.signingKey.privateKey,
		}, nil
	}

	keys, err := s.keys.GetActive(ctx)
	if err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		// Generate new key
		key, err := s.generateAndStoreKey(ctx)
		if err != nil {
			return nil, err
		}
		return key, nil
	}

	// Use the first active key
	k := keys[0]
	privateKey, err := parsePrivateKey(k.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	s.signingKey = &signingKey{
		kid:        k.KID,
		privateKey: privateKey,
	}

	return &opSigningKey{
		id:         k.KID,
		algorithm:  jose.RS256,
		privateKey: privateKey,
	}, nil
}

func (s *Storage) SignatureAlgorithms(ctx context.Context) ([]jose.SignatureAlgorithm, error) {
	return []jose.SignatureAlgorithm{jose.RS256}, nil
}

func (s *Storage) generateAndStoreKey(ctx context.Context) (op.SigningKey, error) {
	// Use 3072-bit RSA keys for long-term security (NIST recommendation for post-2030)
	privateKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, fmt.Errorf("generate RSA key: %w", err)
	}

	fullKid, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("generate key ID: %w", err)
	}
	kid := fullKid[:16]
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})

	key := persistence.OIDCKey{
		KID:        kid,
		Alg:        "RS256",
		PrivateKey: pemBytes,
		CreatedAt:  time.Now().UTC(),
	}

	if err := s.keys.Create(ctx, key); err != nil {
		return nil, err
	}

	s.signingKey = &signingKey{
		kid:        kid,
		privateKey: privateKey,
	}

	return &opSigningKey{
		id:         kid,
		algorithm:  jose.RS256,
		privateKey: privateKey,
	}, nil
}

func (s *Storage) KeySet(ctx context.Context) ([]op.Key, error) {
	keys, err := s.keys.GetActive(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]op.Key, 0, len(keys))
	for _, k := range keys {
		privateKey, err := parsePrivateKey(k.PrivateKey)
		if err != nil {
			s.logger.Warn("skipping invalid OIDC signing key", "kid", k.KID, "error", err)
			continue
		}
		result = append(result, &opKey{
			id:        k.KID,
			algorithm: jose.RS256,
			publicKey: &privateKey.PublicKey,
		})
	}

	return result, nil
}

// -----------------------------------------------------------------------------
// User info operations
// -----------------------------------------------------------------------------

// GetUserByID returns user info by user ID.
func (s *Storage) GetUserByID(ctx context.Context, userID string) (*persistence.User, error) {
	user, err := s.users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// VerifyPassword verifies user credentials and returns the user.
func (s *Storage) VerifyPassword(ctx context.Context, username, password string, verifyFunc func(hash, password string) bool) (*persistence.User, error) {
	user, err := s.users.GetByUsername(ctx, username)
	if err != nil {
		return nil, err
	}

	if !verifyFunc(user.PasswordHash, password) {
		return nil, errors.New("invalid credentials")
	}

	return &user, nil
}

// -----------------------------------------------------------------------------
// Helper types for op.SigningKey and op.Key
// -----------------------------------------------------------------------------

type opSigningKey struct {
	id         string
	algorithm  jose.SignatureAlgorithm
	privateKey *rsa.PrivateKey
}

func (k *opSigningKey) SignatureAlgorithm() jose.SignatureAlgorithm {
	return k.algorithm
}

func (k *opSigningKey) Key() any {
	return k.privateKey
}

func (k *opSigningKey) ID() string {
	return k.id
}

type opKey struct {
	id        string
	algorithm jose.SignatureAlgorithm
	publicKey crypto.PublicKey
}

func (k *opKey) Algorithm() jose.SignatureAlgorithm {
	return k.algorithm
}

func (k *opKey) Use() string {
	return "sig"
}

func (k *opKey) Key() any {
	return k.publicKey
}

func (k *opKey) ID() string {
	return k.id
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func generateID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func parsePrivateKey(pemData []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("failed to decode PEM block")
	}

	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("not an RSA key")
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("unknown key type: %s", block.Type)
	}
}

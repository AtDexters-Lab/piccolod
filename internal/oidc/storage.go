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

// StorageConfig configures the OIDC storage.
type StorageConfig struct {
	Users           persistence.UserRepo
	Clients         persistence.OIDCClientRepo
	Keys            persistence.OIDCKeyRepo
	AuthCodes       persistence.OIDCAuthCodeRepo
	RefreshTokens   persistence.OIDCRefreshTokenRepo
	ResolveRedirect func(ctx context.Context, appID string) ([]string, error)
	Logger          *slog.Logger
}

// NewStorage creates a new OIDC storage backed by our persistence layer.
func NewStorage(cfg StorageConfig) *Storage {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Storage{
		users:           cfg.Users,
		clients:         cfg.Clients,
		keys:            cfg.Keys,
		authCodes:       cfg.AuthCodes,
		refreshTokens:   cfg.RefreshTokens,
		authRequests:    make(map[string]*AuthRequest),
		resolveRedirect: cfg.ResolveRedirect,
		logger:          cfg.Logger,
	}
	if s.resolveRedirect == nil {
		s.resolveRedirect = func(ctx context.Context, appID string) ([]string, error) { return nil, nil }
	}
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
// Client operations (OPStorage)
// -----------------------------------------------------------------------------

func (s *Storage) GetClientByClientID(ctx context.Context, clientID string) (op.Client, error) {
	client, err := s.clients.Get(ctx, clientID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, fmt.Errorf("client not found: %s", clientID)
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
	client, err := s.clients.Get(ctx, clientID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return fmt.Errorf("client not found: %s", clientID)
		}
		return err
	}

	if !VerifyClientSecret(client.Secret, clientSecret) {
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
	id, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("generate request ID: %w", err)
	}

	var codeChallenge *oidc.CodeChallenge
	if authReq.CodeChallenge != "" {
		codeChallenge = &oidc.CodeChallenge{
			Challenge: authReq.CodeChallenge,
			Method:    authReq.CodeChallengeMethod,
		}
	}

	request := &AuthRequest{
		ID:            id,
		ClientID:      authReq.ClientID,
		RedirectURI:   authReq.RedirectURI,
		Scopes:        authReq.Scopes,
		State:         authReq.State,
		Nonce:         authReq.Nonce,
		ResponseType:  authReq.ResponseType,
		ResponseMode:  authReq.ResponseMode,
		CodeChallenge: codeChallenge,
		UserID:        userID,
		IsDone:        userID != "",
	}
	if userID != "" {
		request.AuthTime = time.Now().UTC()
	}

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
		return nil, fmt.Errorf("auth request not found: %s", id)
	}
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

	return &AuthRequest{
		ID:            authCode.Code,
		ClientID:      authCode.ClientID,
		RedirectURI:   authCode.RedirectURI,
		Scopes:        strings.Split(authCode.Scope, " "),
		Nonce:         authCode.Nonce,
		CodeChallenge: codeChallenge,
		UserID:        authCode.UserID,
		AuthTime:      authCode.CreatedAt,
		IsDone:        true,
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
func (s *Storage) CompleteAuthRequest(ctx context.Context, id, userID string) error {
	// 1. Get Request (Read Lock)
	s.authRequestsMu.RLock()
	request, ok := s.authRequests[id]
	clientID := ""
	if ok {
		clientID = request.ClientID
	}
	s.authRequestsMu.RUnlock()

	if !ok {
		return fmt.Errorf("auth request not found: %s", id)
	}

	// 2. Validate Access (DB Ops)
	client, err := s.clients.Get(ctx, clientID)
	if err != nil {
		return fmt.Errorf("client not found: %w", err)
	}

	user, err := s.users.Get(ctx, userID)
	if err != nil {
		return fmt.Errorf("user not found: %w", err)
	}

	// Standard users must be explicitly allowed
	if user.Role == persistence.UserRoleStandard {
		allowed := false
		if user.AllowedApps != nil {
			for _, appID := range user.AllowedApps {
				if appID == client.AppID {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			return fmt.Errorf("access denied to app %s", client.AppID)
		}
	}

	// 3. Update Request (Write Lock)
	s.authRequestsMu.Lock()
	defer s.authRequestsMu.Unlock()

	// Re-check existence after lock
	request, ok = s.authRequests[id]
	if !ok {
		return fmt.Errorf("auth request not found: %s", id)
	}

	request.UserID = userID
	request.AuthTime = time.Now().UTC()
	request.IsDone = true
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
	expiry := time.Now().UTC().Add(time.Hour)
	return id, expiry, nil
}

func (s *Storage) CreateAccessAndRefreshTokens(ctx context.Context, request op.TokenRequest, currentRefreshToken string) (accessTokenID string, newRefreshToken string, expiration time.Time, err error) {
	accessTokenID, err = generateID()
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("generate access token ID: %w", err)
	}
	expiry := time.Now().UTC().Add(time.Hour)

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
		refreshExpiry := time.Now().UTC().Add(30 * 24 * time.Hour) // 30 days

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
	client, err := s.clients.Get(ctx, token.ClientID)
	if err != nil {
		return nil, fmt.Errorf("client not found: %w", err)
	}

	if user.Role == persistence.UserRoleStandard {
		allowed := false
		if user.AllowedApps != nil {
			for _, appID := range user.AllowedApps {
				if appID == client.AppID {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			return nil, fmt.Errorf("access denied to app %s", client.AppID)
		}
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
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
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

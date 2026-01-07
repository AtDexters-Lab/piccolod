package oidc

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
	"golang.org/x/crypto/argon2"

	"piccolod/internal/persistence"
)

// Client implements op.Client
type Client struct {
	id           string
	secret       string
	appID        string
	redirectURIs []string
}

func (c *Client) GetID() string {
	return c.id
}

func (c *Client) RedirectURIs() []string {
	return c.redirectURIs
}

func (c *Client) PostLogoutRedirectURIs() []string {
	return []string{}
}

func (c *Client) ApplicationType() op.ApplicationType {
	return op.ApplicationTypeWeb
}

func (c *Client) AuthMethod() oidc.AuthMethod {
	return oidc.AuthMethodBasic
}

func (c *Client) ResponseTypes() []oidc.ResponseType {
	return []oidc.ResponseType{oidc.ResponseTypeCode}
}

func (c *Client) GrantTypes() []oidc.GrantType {
	return []oidc.GrantType{oidc.GrantTypeCode, oidc.GrantTypeRefreshToken}
}

func (c *Client) LoginURL(id string) string {
	return "/?authRequestID=" + id
}

func (c *Client) AccessTokenType() op.AccessTokenType {
	return op.AccessTokenTypeJWT
}

func (c *Client) IDTokenLifetime() time.Duration {
	return 1 * time.Hour
}

func (c *Client) DevMode() bool {
	return false
}

func (c *Client) RestrictAdditionalIdTokenScopes() func(scopes []string) []string {
	return func(scopes []string) []string { return scopes }
}

func (c *Client) RestrictAdditionalAccessTokenScopes() func(scopes []string) []string {
	return func(scopes []string) []string { return scopes }
}

func (c *Client) IsScopeAllowed(scope string) bool {
	return true
}

func (c *Client) IDTokenUserinfoClaimsAssertion() bool {
	return false
}

func (c *Client) ClockSkew() time.Duration {
	return 0
}

// ClientManager handles OIDC client registration for apps.
type ClientManager struct {
	repo persistence.OIDCClientRepo
}

// NewClientManager creates a new client manager.
func NewClientManager(repo persistence.OIDCClientRepo) *ClientManager {
	return &ClientManager{repo: repo}
}

// RegisterClient creates a new OIDC client for an app.
// Returns the client ID and unhashed secret (secret is only returned once).
func (m *ClientManager) RegisterClient(ctx context.Context, appID string) (clientID, clientSecret string, err error) {
	// Generate client credentials
	clientID, err = generateClientID()
	if err != nil {
		return "", "", fmt.Errorf("generate client ID: %w", err)
	}
	clientSecret, err = generateClientSecret()
	if err != nil {
		return "", "", fmt.Errorf("generate client secret: %w", err)
	}

	// Hash the secret for storage
	hashedSecret, err := hashClientSecret(clientSecret)
	if err != nil {
		return "", "", fmt.Errorf("hash client secret: %w", err)
	}

	client := persistence.OIDCClient{
		ID:        clientID,
		Secret:    hashedSecret,
		AppID:     appID,
		CreatedAt: time.Now().UTC(),
	}

	if err := m.repo.Create(ctx, client); err != nil {
		return "", "", err
	}

	return clientID, clientSecret, nil
}

// GenerateCredentials generates a new client ID and secret.
func (m *ClientManager) GenerateCredentials() (clientID, clientSecret string, err error) {
	clientID, err = generateClientID()
	if err != nil {
		return "", "", err
	}
	clientSecret, err = generateClientSecret()
	if err != nil {
		return "", "", err
	}
	return clientID, clientSecret, nil
}

// CreateClient registers a client with pre-generated credentials.
func (m *ClientManager) CreateClient(ctx context.Context, clientID, clientSecret, appID string) error {
	hashedSecret, err := hashClientSecret(clientSecret)
	if err != nil {
		return fmt.Errorf("hash client secret: %w", err)
	}
	client := persistence.OIDCClient{
		ID:        clientID,
		Secret:    hashedSecret,
		AppID:     appID,
		CreatedAt: time.Now().UTC(),
	}
	return m.repo.Create(ctx, client)
}

// GetClientByAppID returns the client for an app, if registered.
func (m *ClientManager) GetClientByAppID(ctx context.Context, appID string) (*persistence.OIDCClient, error) {
	client, err := m.repo.GetByAppID(ctx, appID)
	if err != nil {
		return nil, err
	}
	return &client, nil
}

// DeleteClient removes an OIDC client.
func (m *ClientManager) DeleteClient(ctx context.Context, clientID string) error {
	return m.repo.Delete(ctx, clientID)
}

// VerifyClientSecret verifies a client secret against the stored hash.
// Uses constant-time comparison to prevent timing attacks.
func VerifyClientSecret(storedHash, providedSecret string) bool {
	// Parse stored hash to extract salt
	parts := strings.SplitN(storedHash, "$", 2)
	if len(parts) != 2 {
		return false
	}

	salt, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(salt) != 16 {
		return false
	}

	storedHashBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}

	// Compute hash with same salt - using stronger params: time=2, memory=64MB, threads=4
	computed := argon2.IDKey([]byte(providedSecret), salt, 2, 64*1024, 4, 32)

	// Constant-time comparison to prevent timing attacks
	return subtle.ConstantTimeCompare(computed, storedHashBytes) == 1
}

// generateClientID generates a random client ID.
func generateClientID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return "piccolo-" + base64.RawURLEncoding.EncodeToString(b), nil
}

// generateClientSecret generates a random client secret.
func generateClientSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashClientSecret hashes a client secret using Argon2id with a random salt.
// Returns format: "salt$hash" where both are base64-encoded.
// Uses secure parameters: time=2, memory=64MB, threads=4, keyLen=32.
func hashClientSecret(secret string) (string, error) {
	// Generate random 16-byte salt
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}

	// Hash with strong parameters
	hash := argon2.IDKey([]byte(secret), salt, 2, 64*1024, 4, 32)

	// Return salt$hash format
	return base64.RawURLEncoding.EncodeToString(salt) + "$" + base64.RawURLEncoding.EncodeToString(hash), nil
}

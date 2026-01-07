package oidc

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/zitadel/oidc/v3/pkg/op"

	"piccolod/internal/persistence"
)

// Provider wraps the zitadel/oidc op.Provider with our storage and configuration.
type Provider struct {
	inner   *op.Provider
	storage *Storage
	issuer  string
	logger  *slog.Logger
}

// ProviderConfig configures the OIDC provider.
type ProviderConfig struct {
	// Issuer is the stable issuer URL (e.g., "https://piccolo.local")
	Issuer string

	// Storage repositories
	Users         persistence.UserRepo
	Clients       persistence.OIDCClientRepo
	Keys          persistence.OIDCKeyRepo
	AuthCodes     persistence.OIDCAuthCodeRepo
	RefreshTokens persistence.OIDCRefreshTokenRepo
	Config        persistence.OIDCConfigRepo

	// ResolveRedirect resolves app ID to redirect URI
	ResolveRedirect func(ctx context.Context, appID string) ([]string, error)

	// VerifyPassword verifies user password (reuse from auth package)
	VerifyPassword func(hash, password string) bool

	// Logger for OIDC operations
	Logger *slog.Logger
}

// NewProvider creates a new OIDC provider.
func NewProvider(ctx context.Context, cfg ProviderConfig) (*Provider, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	// Load or generate encryption key
	cryptoKey, err := loadOrGenerateEncryptionKey(ctx, cfg.Config, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("load encryption key: %w", err)
	}

	storage := NewStorage(StorageConfig{
		Users:           cfg.Users,
		Clients:         cfg.Clients,
		Keys:            cfg.Keys,
		AuthCodes:       cfg.AuthCodes,
		RefreshTokens:   cfg.RefreshTokens,
		ResolveRedirect: cfg.ResolveRedirect,
		Logger:          cfg.Logger,
	})

	// Configure the OIDC provider
	config := &op.Config{
		CryptoKey:                cryptoKey,
		DefaultLogoutRedirectURI: "/",
		CodeMethodS256:           true,
		AuthMethodPost:           true,
		AuthMethodPrivateKeyJWT:  false,
		GrantTypeRefreshToken:    true,
		RequestObjectSupported:   false,
		SupportedUILocales:       nil,
		DeviceAuthorization:      op.DeviceAuthorizationConfig{},
	}

	// Create the provider with stable issuer.
	// RFC 3.1: The issuer is ALWAYS the configured stable issuer (e.g., "https://piccolo.local").
	// Tokens are minted with this issuer regardless of how the request arrives.
	// The dynamic authorization_endpoint is handled by our custom DiscoveryHandler.
	issuerFactory := func(insecure bool) (op.IssuerFromRequest, error) {
		return func(r *http.Request) string {
			return cfg.Issuer
		}, nil
	}

	inner, err := op.NewProvider(config, storage, issuerFactory,
		op.WithAllowInsecure(), // Allow HTTP for local development
	)
	if err != nil {
		return nil, err
	}

	return &Provider{
		inner:   inner,
		storage: storage,
		issuer:  cfg.Issuer,
		logger:  cfg.Logger,
	}, nil
}

// Handler returns the HTTP handler for OIDC endpoints.
// Note: We use custom handlers in the server instead of this default router
// to have more control over the discovery endpoint.
func (p *Provider) Handler() http.Handler {
	return p.inner
}

// Storage returns the underlying storage for direct operations.
func (p *Provider) Storage() *Storage {
	return p.storage
}

// Issuer returns the configured issuer URL.
func (p *Provider) Issuer() string {
	return p.issuer
}

// Inner returns the underlying zitadel/oidc provider for advanced use.
func (p *Provider) Inner() *op.Provider {
	return p.inner
}

// CompleteAuthRequest completes an auth request after successful login.
func (p *Provider) CompleteAuthRequest(ctx context.Context, authRequestID, userID string) error {
	return p.storage.CompleteAuthRequest(ctx, authRequestID, userID)
}

// AuthCallbackURL returns the URL to redirect to after login.
func (p *Provider) AuthCallbackURL() string {
	return p.issuer + "/callback"
}

// loadOrGenerateEncryptionKey loads the encryption key from the config repo,
// or generates and stores a new random 32-byte key if none exists.
func loadOrGenerateEncryptionKey(ctx context.Context, repo persistence.OIDCConfigRepo, logger *slog.Logger) ([32]byte, error) {
	var key [32]byte

	if repo == nil {
		// No repo configured - generate ephemeral key (not recommended for production)
		logger.Warn("OIDC config repo not configured, using ephemeral encryption key")
		if _, err := rand.Read(key[:]); err != nil {
			return key, fmt.Errorf("generate ephemeral key: %w", err)
		}
		return key, nil
	}

	// Try to load existing key
	existing, err := repo.GetEncryptionKey(ctx)
	if err != nil {
		return key, fmt.Errorf("load encryption key: %w", err)
	}

	if len(existing) == 32 {
		copy(key[:], existing)
		logger.Debug("loaded OIDC encryption key from storage")
		return key, nil
	}

	// Generate new random key
	if _, err := rand.Read(key[:]); err != nil {
		return key, fmt.Errorf("generate encryption key: %w", err)
	}

	// Store it
	if err := repo.SetEncryptionKey(ctx, key[:]); err != nil {
		return key, fmt.Errorf("store encryption key: %w", err)
	}

	logger.Info("generated and stored new OIDC encryption key")
	return key, nil
}

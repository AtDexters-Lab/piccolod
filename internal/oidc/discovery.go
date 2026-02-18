package oidc

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// DiscoveryConfig configures the dynamic discovery endpoint.
type DiscoveryConfig struct {
	// StableIssuer is the canonical issuer (e.g., "https://piccolo-<machineId>.local")
	StableIssuer string

	// IsRemoteActive returns true if remote/WAN access is enabled
	IsRemoteActive func() bool

	// GetPortalHostname returns the portal hostname for WAN access
	GetPortalHostname func() string

	// GetLocalHostname returns the local hostname for LAN access (e.g., "piccolo.local",
	// "piccolo-abc123.local", or an IP address if mDNS is disabled)
	GetLocalHostname func() string

	// Logger for discovery handler warnings
	Logger *slog.Logger
}

// DiscoveryHandler returns a handler for /.well-known/openid-configuration
// that implements the "Stable Issuer, Dynamic Discovery" strategy.
type DiscoveryHandler struct {
	config DiscoveryConfig
}

// NewDiscoveryHandler creates a new discovery handler.
func NewDiscoveryHandler(cfg DiscoveryConfig) *DiscoveryHandler {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &DiscoveryHandler{config: cfg}
}

// OpenIDConfiguration represents the OIDC discovery document.
type OpenIDConfiguration struct {
	Issuer                           string   `json:"issuer"`
	AuthorizationEndpoint            string   `json:"authorization_endpoint"`
	TokenEndpoint                    string   `json:"token_endpoint"`
	UserinfoEndpoint                 string   `json:"userinfo_endpoint"`
	JwksURI                          string   `json:"jwks_uri"`
	EndSessionEndpoint               string   `json:"end_session_endpoint,omitempty"`
	IntrospectionEndpoint            string   `json:"introspection_endpoint,omitempty"`
	RevocationEndpoint               string   `json:"revocation_endpoint,omitempty"`
	RegistrationEndpoint             string   `json:"registration_endpoint,omitempty"`
	ScopesSupported                  []string `json:"scopes_supported"`
	ResponseTypesSupported           []string `json:"response_types_supported"`
	ResponseModesSupported           []string `json:"response_modes_supported,omitempty"`
	GrantTypesSupported              []string `json:"grant_types_supported"`
	SubjectTypesSupported            []string `json:"subject_types_supported"`
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
	TokenEndpointAuthMethods         []string `json:"token_endpoint_auth_methods_supported"`
	ClaimsSupported                  []string `json:"claims_supported"`
	CodeChallengeMethodsSupported    []string `json:"code_challenge_methods_supported,omitempty"`
}

// ServeHTTP handles the discovery request.
func (h *DiscoveryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Issuer is the machine-specific issuer (e.g., "https://piccolo-<machineId>.local").
	// Only the authorization_endpoint varies based on LAN/WAN state.
	// All back-channel endpoints (token, jwks, userinfo) use the issuer.
	issuer := h.config.StableIssuer

	// Determine the authorization endpoint based on remote state (front-channel)
	var authEndpoint string
	if h.config.IsRemoteActive != nil && h.config.IsRemoteActive() {
		// Remote is active: use portal hostname for browser-facing authorization
		if h.config.GetPortalHostname != nil {
			portalHost := h.config.GetPortalHostname()
			if portalHost != "" {
				authEndpoint = "https://" + portalHost + "/oauth/authorize"
			}
		}
	}
	if authEndpoint == "" {
		// Local only or no portal hostname: use local hostname for LAN authorization.
		// This allows the browser to reach the authorization endpoint on LAN without TLS issues.
		localHost := "piccolo.local" // fallback
		if h.config.GetLocalHostname != nil {
			if lh := h.config.GetLocalHostname(); lh != "" {
				localHost = lh
			}
		}
		authEndpoint = "http://" + localHost + "/oauth/authorize"
	}

	config := OpenIDConfiguration{
		Issuer:                issuer,
		AuthorizationEndpoint: authEndpoint, // Dynamic based on remote state
		TokenEndpoint:         issuer + "/oauth/token",
		UserinfoEndpoint:      issuer + "/oauth/userinfo",
		JwksURI:               issuer + "/oauth/jwks",
		EndSessionEndpoint:    issuer + "/oauth/logout",
		IntrospectionEndpoint: issuer + "/oauth/introspect",
		RevocationEndpoint:    issuer + "/oauth/revoke",
		ScopesSupported: []string{
			"openid",
			"profile",
			"email",
			"offline_access",
		},
		ResponseTypesSupported: []string{
			"code",
			"id_token",
			"id_token token",
			"code id_token",
			"code token",
			"code id_token token",
		},
		ResponseModesSupported: []string{
			"query",
			"fragment",
			"form_post",
		},
		GrantTypesSupported: []string{
			"authorization_code",
			"refresh_token",
		},
		SubjectTypesSupported: []string{
			"public",
		},
		IDTokenSigningAlgValuesSupported: []string{
			"RS256",
		},
		TokenEndpointAuthMethods: []string{
			"client_secret_basic",
			"client_secret_post",
		},
		ClaimsSupported: []string{
			"sub",
			"iss",
			"aud",
			"exp",
			"iat",
			"auth_time",
			"nonce",
			"email",
			"email_verified",
			"name",
			"preferred_username",
			"picture",
		},
		CodeChallengeMethodsSupported: []string{
			"S256",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(config); err != nil {
		h.config.Logger.Warn("failed to encode OIDC discovery", "error", err)
		// Note: headers already sent, can't change status code at this point
	}
}

// Handler returns the discovery handler as http.HandlerFunc.
func (h *DiscoveryHandler) Handler() http.HandlerFunc {
	return h.ServeHTTP
}

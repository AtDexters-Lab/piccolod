package oidc

import (
	"time"

	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"

	"piccolod/internal/persistence"
)

// AuthRequest wraps an in-progress authorization request to implement op.AuthRequest.
type AuthRequest struct {
	ID            string
	ClientID      string
	RedirectURI   string
	Scopes        []string
	State         string
	Nonce         string
	ResponseType  oidc.ResponseType
	ResponseMode  oidc.ResponseMode
	CodeChallenge *oidc.CodeChallenge

	// Set after user authenticates
	UserID   string
	AuthTime time.Time
	IsDone   bool

	// For cleanup tracking
	CreatedAt time.Time
}

var _ op.AuthRequest = (*AuthRequest)(nil)

func (a *AuthRequest) GetID() string {
	return a.ID
}

func (a *AuthRequest) GetACR() string {
	return ""
}

func (a *AuthRequest) GetAMR() []string {
	return []string{"pwd"}
}

func (a *AuthRequest) GetAudience() []string {
	return []string{a.ClientID}
}

func (a *AuthRequest) GetAuthTime() time.Time {
	return a.AuthTime
}

func (a *AuthRequest) GetClientID() string {
	return a.ClientID
}

func (a *AuthRequest) GetCodeChallenge() *oidc.CodeChallenge {
	return a.CodeChallenge
}

func (a *AuthRequest) GetNonce() string {
	return a.Nonce
}

func (a *AuthRequest) GetRedirectURI() string {
	return a.RedirectURI
}

func (a *AuthRequest) GetResponseType() oidc.ResponseType {
	return a.ResponseType
}

func (a *AuthRequest) GetResponseMode() oidc.ResponseMode {
	return a.ResponseMode
}

func (a *AuthRequest) GetScopes() []string {
	return a.Scopes
}

func (a *AuthRequest) GetState() string {
	return a.State
}

func (a *AuthRequest) GetSubject() string {
	return a.UserID
}

func (a *AuthRequest) Done() bool {
	return a.IsDone
}

// RefreshTokenRequest implements op.RefreshTokenRequest for token refresh flows.
type RefreshTokenRequest struct {
	Token     persistence.OIDCRefreshToken
	User      persistence.User
	ClientID  string
	Scopes    []string
	AuthTime  time.Time
}

var _ op.RefreshTokenRequest = (*RefreshTokenRequest)(nil)

func (r *RefreshTokenRequest) GetAMR() []string {
	return []string{"pwd"}
}

func (r *RefreshTokenRequest) GetAudience() []string {
	return []string{r.ClientID}
}

func (r *RefreshTokenRequest) GetAuthTime() time.Time {
	return r.AuthTime
}

func (r *RefreshTokenRequest) GetClientID() string {
	return r.ClientID
}

func (r *RefreshTokenRequest) GetScopes() []string {
	return r.Scopes
}

func (r *RefreshTokenRequest) GetSubject() string {
	return r.Token.UserID
}

func (r *RefreshTokenRequest) SetCurrentScopes(scopes []string) {
	r.Scopes = scopes
}

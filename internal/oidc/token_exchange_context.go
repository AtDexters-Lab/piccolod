package oidc

import "context"

type tokenExchangeContextKey struct{}

// TokenExchangeContext carries values needed to evaluate hybrid token endpoint authentication.
type TokenExchangeContext struct {
	Code        string
	RedirectURI string
}

// WithTokenExchangeContext attaches token-exchange metadata to a request context.
func WithTokenExchangeContext(ctx context.Context, code, redirectURI string) context.Context {
	return context.WithValue(ctx, tokenExchangeContextKey{}, TokenExchangeContext{
		Code:        code,
		RedirectURI: redirectURI,
	})
}

// TokenExchangeContextFrom extracts token-exchange metadata from a request context.
func TokenExchangeContextFrom(ctx context.Context) (TokenExchangeContext, bool) {
	v, ok := ctx.Value(tokenExchangeContextKey{}).(TokenExchangeContext)
	if !ok {
		return TokenExchangeContext{}, false
	}
	return v, true
}

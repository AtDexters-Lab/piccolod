package middleware

import "context"

// hintContextKey is the unexported key under which Hint values are stored in
// request contexts. Only this package writes/reads it via the typed accessors
// HintFromContext and ContextWithHint.
type hintContextKey struct{}

// HintFromContext returns the Hint stored on ctx by an upstream writer (the L4
// hint_consumer_l4 middleware via http.Server.ConnContext, or the L7
// hint_consumer_l7 middleware reading the X-Piccolo-Hint-Token header).
//
// L7 read site. L4 middlewares read the lazy accessor directly via
// ConnContext.Hint (types.go) — the L4→L7 bridge resolves it once and stashes
// the value here so L7 doesn't pay the lookup cost per request.
//
// Returns (Hint{}, false) when no hint is present on ctx — callers should
// treat this as "no real-client metadata available; fall back to socket-level
// source addr."
//
// This is the single read site for resolved hint data. Two write sites (L4
// conn-level via ContextWithHint, L7 header-token via the same), with L7
// overwriting L4 when both apply — the LAN-host-based hop's header-token is
// source of truth for that case.
func HintFromContext(ctx context.Context) (Hint, bool) {
	if ctx == nil {
		return Hint{}, false
	}
	h, ok := ctx.Value(hintContextKey{}).(Hint)
	if !ok {
		return Hint{}, false
	}
	return h, true
}

// ContextWithHint returns a copy of ctx that carries the given Hint. Used by
// hint_consumer_l4 (via http.Server.ConnContext bridge) and hint_consumer_l7
// (header-token consumption) to populate the request context.
//
// Overwrite semantics: if ctx already carries a Hint, the new value replaces it.
// This implements the L7-overwrites-L4 rule (LAN-host-based hop's header-token
// is the source of truth for that case).
func ContextWithHint(ctx context.Context, h Hint) context.Context {
	return context.WithValue(ctx, hintContextKey{}, h)
}

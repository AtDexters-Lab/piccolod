// Package builtin registers the canonical L4 / L4UDP / L7 / L7Response chain
// factories. Lives in a sibling package of middleware/ to avoid the
// middleware → l4|l7 → middleware import cycle.
package builtin

import (
	"fmt"
	"net/http"

	"piccolod/internal/services/middleware"
	"piccolod/internal/services/middleware/l4"
	"piccolod/internal/services/middleware/l7"
	l7oidc "piccolod/internal/services/middleware/l7/oidc"
)

// Canonical middleware names. Listed in canonical execution order — Build
// composes them in the order they are registered below.
//
// The conditional gates from registry.go (canonicalApplies) still apply:
//   - NamePathAuth runs only when spec.HasAuth (ep.Auth != nil)
//   - NameConnectionAuth runs only when spec.HasConnectionAuth (step 6)
//
// Two of the canonical L7 entries (hint_consumer_l7, cookie_context) are
// transitional inlines that wrap services-internal closures via deps. They
// retire in step 9 when the hint chain migrates to middleware.HintFromContext.
const (
	// L4 (TCP) + L4UDP canonical names.
	NameHintConsumerL4 = "hint_consumer_l4"
	NameConnMetrics    = "conn_metrics"
	// L4 / L4UDP operator-listable names.
	NameIPAllowlist = "ip_allowlist"
	NameIPRateLimit = "ip_rate_limit"

	NameForwardedScrub        = "forwarded_scrub"
	NamePathNormalize         = "path_normalize"
	NameHintConsumerL7        = "hint_consumer_l7"
	NameReservedPathIntercept = "reserved_path_intercept"
	NameStripPiccoloHeaders   = "strip_piccolo_headers"
	NameACMEBypass            = "acme_bypass"
	// NamePathAuth defined in registry.go (conditionally canonical).
	NameCookieContext                = "cookie_context"
	NameOIDCAuthorizeSnapshot        = "oidc_authorize_snapshot"
	NameForwardHeaders               = "forward_headers"
	NameSecurityHeadersResponse      = "security_headers_response"
	NameCookieIsolationResponse      = "cookie_isolation_response"
	NameEmbeddedMarkerResponse       = "embedded_marker_response"
	NameOIDCAuthorizeRewriteResponse = "oidc_authorize_rewrite_response"
)

// Dep keys for stringly-typed RegistryDeps lookup. Each name documents the
// expected concrete type that the services package binds at deps-construction
// time.
const (
	DepHintLookupL4    = "hint_consumer_l4.lookup" // l4.HintLookupFn
	DepListenerPort    = "shared.listener_port"    // int
	DepMetricsRegistry = "shared.metrics_registry" // *l4.MetricsRegistry

	DepHintConsumerL7        = "hint_consumer_l7.consume"            // func(*http.Request) *http.Request
	DepReservedPathCallback  = "reserved_path_intercept.callback_fn" // func() l7oidc.CallbackHandlerFn
	DepACMEHandler           = "acme_bypass.handler_fn"              // l7.ACMEHandlerFn
	DepArrivedViaTLS         = "shared.arrived_via_tls"              // l7.ArrivedViaTLSFn
	DepPathAuthSnapshot      = "path_auth.snapshot_fn"               // func() l7.PathAuthSnapshot
	DepCookieContext         = "cookie_context.apply"                // func(*http.Request) *http.Request
	DepOIDCAuthorizeSnapshot = "oidc_authorize_snapshot.snapshot_fn" // func() l7oidc.AuthorizeSnapshotState
	DepForwardHeaders        = "forward_headers.apply"               // func(*http.Request)
	DepNeedsMarker           = "embedded_marker_response.needs"      // l7.NeedsMarkerFn
	DepMarkerCookie          = "embedded_marker_response.cookie_fn"  // l7.MarkerCookieFn
)

// RegisterDefaults registers the canonical L4 / L4UDP / L7 / L7Response
// chain factories plus the operator-listable IP-rule built-ins. Call once
// per Registry (services package wires this in NewProxyManager).
func RegisterDefaults(reg *middleware.Registry) {
	// L4 (TCP) canonical — outermost first.
	reg.RegisterCanonical(NameConnMetrics, middleware.LayerL4, factoryConnMetrics)
	reg.RegisterCanonical(NameHintConsumerL4, middleware.LayerL4, factoryHintConsumerL4)

	// L4UDP canonical.
	reg.RegisterCanonical(NameConnMetrics+"_udp", middleware.LayerL4UDP, factoryConnMetricsUDP)

	// L4 / L4UDP operator-listable IP-rule middlewares. Both layers share a
	// single name — Build picks the appropriate signature based on the
	// listener's flow.
	reg.Register(NameIPAllowlist, []middleware.Layer{middleware.LayerL4, middleware.LayerL4UDP}, factoryIPAllowlist)
	reg.Register(NameIPRateLimit, []middleware.Layer{middleware.LayerL4, middleware.LayerL4UDP}, factoryIPRateLimit)

	// Request-side L7 — canonical order matches plan §H.
	reg.RegisterCanonical(NameForwardedScrub, middleware.LayerL7, factoryForwardedScrub)
	reg.RegisterCanonical(NamePathNormalize, middleware.LayerL7, factoryPathNormalize)
	reg.RegisterCanonical(NameHintConsumerL7, middleware.LayerL7, factoryHintConsumerL7)
	reg.RegisterCanonical(NameReservedPathIntercept, middleware.LayerL7, factoryReservedPathIntercept)
	reg.RegisterCanonical(NameStripPiccoloHeaders, middleware.LayerL7, factoryStripPiccoloHeaders)
	reg.RegisterCanonical(NameACMEBypass, middleware.LayerL7, factoryACMEBypass)
	reg.RegisterCanonical(middleware.NamePathAuth, middleware.LayerL7, factoryPathAuth) // conditionally canonical
	reg.RegisterCanonical(NameCookieContext, middleware.LayerL7, factoryCookieContext)
	reg.RegisterCanonical(NameOIDCAuthorizeSnapshot, middleware.LayerL7, factoryOIDCAuthorizeSnapshot)
	reg.RegisterCanonical(NameForwardHeaders, middleware.LayerL7, factoryForwardHeaders)

	// Response-side L7Response — canonical order matches plan §H.
	reg.RegisterCanonical(NameSecurityHeadersResponse, middleware.LayerL7Response, factorySecurityHeadersResponse)
	reg.RegisterCanonical(NameCookieIsolationResponse, middleware.LayerL7Response, factoryCookieIsolationResponse)
	reg.RegisterCanonical(NameEmbeddedMarkerResponse, middleware.LayerL7Response, factoryEmbeddedMarkerResponse)
	reg.RegisterCanonical(NameOIDCAuthorizeRewriteResponse, middleware.LayerL7Response, factoryOIDCAuthorizeRewriteResponse)
}

// --- L4 / L4UDP factories ---

func factoryHintConsumerL4(_ map[string]any, ep middleware.EndpointInfo, deps middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	lookup, ok := deps.Get(DepHintLookupL4).(l4.HintLookupFn)
	if !ok {
		return nil, fmt.Errorf("hint_consumer_l4: missing dep %q", DepHintLookupL4)
	}
	return l4.HintConsumer(lookup, ep.PublicPort), nil
}

func factoryConnMetrics(_ map[string]any, _ middleware.EndpointInfo, deps middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	reg, ok := deps.Get(DepMetricsRegistry).(*l4.MetricsRegistry)
	if !ok {
		return nil, fmt.Errorf("conn_metrics: missing dep %q", DepMetricsRegistry)
	}
	return l4.ConnMetrics(reg), nil
}

func factoryConnMetricsUDP(_ map[string]any, _ middleware.EndpointInfo, deps middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	reg, ok := deps.Get(DepMetricsRegistry).(*l4.MetricsRegistry)
	if !ok {
		return nil, fmt.Errorf("conn_metrics_udp: missing dep %q", DepMetricsRegistry)
	}
	return l4.ConnMetricsUDP(reg), nil
}

func factoryIPAllowlist(params map[string]any, _ middleware.EndpointInfo, deps middleware.RegistryDeps, layer middleware.Layer) (any, error) {
	metrics, _ := deps.Get(DepMetricsRegistry).(*l4.MetricsRegistry) // optional — denies still work without metrics
	switch layer {
	case middleware.LayerL4:
		return l4.IPAllowlist(params, metrics)
	case middleware.LayerL4UDP:
		return l4.IPAllowlistUDP(params, metrics)
	default:
		return nil, fmt.Errorf("ip_allowlist: unsupported layer %s", layer)
	}
}

func factoryIPRateLimit(params map[string]any, _ middleware.EndpointInfo, deps middleware.RegistryDeps, layer middleware.Layer) (any, error) {
	metrics, _ := deps.Get(DepMetricsRegistry).(*l4.MetricsRegistry)
	switch layer {
	case middleware.LayerL4:
		return l4.IPRateLimit(params, metrics)
	case middleware.LayerL4UDP:
		return l4.IPRateLimitUDP(params, metrics)
	default:
		return nil, fmt.Errorf("ip_rate_limit: unsupported layer %s", layer)
	}
}

// --- Request-side L7 factories ---

func factoryForwardedScrub(_ map[string]any, _ middleware.EndpointInfo, _ middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	return l7.ForwardedScrub(), nil
}

func factoryPathNormalize(_ map[string]any, _ middleware.EndpointInfo, _ middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	return l7.PathNormalize(l7.NormalizeAndSetRequestPath, l7.WriteJSONError), nil
}

func factoryHintConsumerL7(_ map[string]any, _ middleware.EndpointInfo, deps middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	consume, ok := deps.Get(DepHintConsumerL7).(func(*http.Request) *http.Request)
	if !ok {
		return nil, fmt.Errorf("hint_consumer_l7: missing dep %q", DepHintConsumerL7)
	}
	return middleware.L7Middleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = consume(r)
			next.ServeHTTP(w, r)
		})
	}), nil
}

func factoryReservedPathIntercept(_ map[string]any, ep middleware.EndpointInfo, deps middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	getCallback, ok := deps.Get(DepReservedPathCallback).(func() l7oidc.CallbackHandlerFn)
	if !ok {
		return nil, fmt.Errorf("reserved_path_intercept: missing dep %q", DepReservedPathCallback)
	}
	return l7oidc.ReservedPathIntercept(ep, l7oidc.ReservedPathInterceptDeps{
		GetCallbackHandler: getCallback,
	}), nil
}

func factoryStripPiccoloHeaders(_ map[string]any, _ middleware.EndpointInfo, _ middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	return l7.StripPiccoloHeaders(), nil
}

func factoryACMEBypass(_ map[string]any, _ middleware.EndpointInfo, deps middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	getHandler, ok := deps.Get(DepACMEHandler).(l7.ACMEHandlerFn)
	if !ok {
		return nil, fmt.Errorf("acme_bypass: missing dep %q", DepACMEHandler)
	}
	return l7.ACMEBypass(getHandler), nil
}

func factoryPathAuth(_ map[string]any, ep middleware.EndpointInfo, deps middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	snapshot, ok := deps.Get(DepPathAuthSnapshot).(func() l7.PathAuthSnapshot)
	if !ok {
		return nil, fmt.Errorf("path_auth: missing dep %q", DepPathAuthSnapshot)
	}
	arrivedViaTLS, ok := deps.Get(DepArrivedViaTLS).(l7.ArrivedViaTLSFn)
	if !ok {
		return nil, fmt.Errorf("path_auth: missing dep %q", DepArrivedViaTLS)
	}
	return l7.PathAuth(ep, ep.Auth, l7.PathAuthDeps{
		Snapshot:      snapshot,
		ArrivedViaTLS: arrivedViaTLS,
	}), nil
}

func factoryCookieContext(_ map[string]any, _ middleware.EndpointInfo, deps middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	apply, ok := deps.Get(DepCookieContext).(func(*http.Request) *http.Request)
	if !ok {
		return nil, fmt.Errorf("cookie_context: missing dep %q", DepCookieContext)
	}
	return middleware.L7Middleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = apply(r)
			next.ServeHTTP(w, r)
		})
	}), nil
}

func factoryOIDCAuthorizeSnapshot(_ map[string]any, _ middleware.EndpointInfo, deps middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	snapshot, ok := deps.Get(DepOIDCAuthorizeSnapshot).(func() l7oidc.AuthorizeSnapshotState)
	if !ok {
		return nil, fmt.Errorf("oidc_authorize_snapshot: missing dep %q", DepOIDCAuthorizeSnapshot)
	}
	arrivedViaTLS, ok := deps.Get(DepArrivedViaTLS).(l7.ArrivedViaTLSFn)
	if !ok {
		return nil, fmt.Errorf("oidc_authorize_snapshot: missing dep %q", DepArrivedViaTLS)
	}
	return l7oidc.AuthorizeSnapshot(l7oidc.AuthorizeSnapshotDeps{
		ArrivedViaTLS: arrivedViaTLS,
		Snapshot:      snapshot,
	}), nil
}

func factoryForwardHeaders(_ map[string]any, _ middleware.EndpointInfo, deps middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	apply, ok := deps.Get(DepForwardHeaders).(func(*http.Request))
	if !ok {
		return nil, fmt.Errorf("forward_headers: missing dep %q", DepForwardHeaders)
	}
	return l7.ForwardHeaders(apply), nil
}

// --- Response-side L7Response factories ---

func factorySecurityHeadersResponse(_ map[string]any, _ middleware.EndpointInfo, _ middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	return l7.SecurityHeadersResponse(), nil
}

func factoryCookieIsolationResponse(_ map[string]any, ep middleware.EndpointInfo, _ middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	return l7.CookieIsolationResponse(ep.App), nil
}

func factoryEmbeddedMarkerResponse(_ map[string]any, _ middleware.EndpointInfo, deps middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	needs, ok := deps.Get(DepNeedsMarker).(l7.NeedsMarkerFn)
	if !ok {
		return nil, fmt.Errorf("embedded_marker_response: missing dep %q", DepNeedsMarker)
	}
	cookie, ok := deps.Get(DepMarkerCookie).(l7.MarkerCookieFn)
	if !ok {
		return nil, fmt.Errorf("embedded_marker_response: missing dep %q", DepMarkerCookie)
	}
	return l7.EmbeddedMarkerResponse(needs, cookie), nil
}

func factoryOIDCAuthorizeRewriteResponse(_ map[string]any, ep middleware.EndpointInfo, _ middleware.RegistryDeps, _ middleware.Layer) (any, error) {
	return l7oidc.AuthorizeRewriteResponse(ep.App), nil
}

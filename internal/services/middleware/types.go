package middleware

import (
	"net"
	"net/http"
	"time"
)

// SourceTrust distinguishes connections that arrived directly (LAN/internet) from
// connections relayed through the trusted-loopback path (Nexus → TlsMux → public port).
// Derived by the L4 chain entry from LocalAddr.IsLoopback().
type SourceTrust int

const (
	// Direct: connection from non-loopback. IP-rule middlewares evaluate against SourceAddr.
	Direct SourceTrust = iota
	// TrustedLoopback: connection from loopback (Nexus relay). hint_consumer_l4 may resolve
	// the real client IP via ConnContext.Hint; IP-rule middlewares evaluate against the
	// resolved hint (when present and non-empty), else fall through to SourceAddr.
	TrustedLoopback
)

// Hint carries the resolved real-client metadata for a connection that traversed
// the trusted-loopback path. Populated lazily by hint_consumer_l4.
type Hint struct {
	// ClientIP is the original client IP as resolved by Nexus, or empty if not present.
	ClientIP string
	// IsTLS indicates whether the original client connection used TLS (gates OIDC URL rewriting).
	IsTLS bool
	// RemotePort is the original client-facing port (e.g., 443 even when the listener is on 80).
	RemotePort int
}

// EndpointInfo is the minimal endpoint metadata middlewares observe at chain time.
// Constructed by the services package from a ServiceEndpoint and passed through Build.
// Kept minimal to avoid dragging the full ServiceEndpoint surface into the middleware
// package (which would create an import cycle).
type EndpointInfo struct {
	App              string
	Listener         string
	HostBind         int
	PublicPort       int
	Flow             string // "tcp" | "tls" | "udp"
	Protocol         string // "http" | "websocket" | "raw"
	DerivedHostLabel string
}

// ConnContext is the per-connection context passed to L4 middlewares.
// All fields are populated by the L4 chain entry before any middleware runs.
type ConnContext struct {
	Endpoint    EndpointInfo
	SourceAddr  net.Addr
	LocalAddr   net.Addr
	AcceptedAt  time.Time
	SourceTrust SourceTrust
	// Hint is a lazy accessor. Returns (Hint{}, false) if no hint is registered or
	// SourceTrust == Direct. First call may block briefly (~20ms budget) waiting for
	// the TlsMux to register the hint; subsequent calls return cached result via
	// sync.Once. Direct connections never trigger lookup.
	//
	// L4 read site only — middlewares running in the L4 chain see the lazy func
	// directly. L7 middlewares read a resolved Hint value from the request context
	// via HintFromContext (hint.go), populated by the L4→L7 bridge that step 5+
	// installs (http.Server.ConnContext invoking the lazy lookup once and stashing
	// the result via ContextWithHint).
	Hint func() (Hint, bool)
}

// ConnHandler is the terminal of the L4 chain — proxies the connection to the backend.
type ConnHandler func(ctx ConnContext, conn net.Conn)

// L4Middleware wraps a ConnHandler with pre/post behavior.
type L4Middleware func(next ConnHandler) ConnHandler

// UDPContext is the per-datagram context passed to L4-UDP middlewares.
type UDPContext struct {
	Endpoint   EndpointInfo
	Source     *net.UDPAddr
	Local      *net.UDPAddr
	AcceptedAt time.Time
}

// UDPSink writes a response datagram back to a source address. Provided to UDP
// handlers so they can respond without needing direct conn access.
type UDPSink interface {
	WriteTo(payload []byte, addr *net.UDPAddr) (int, error)
}

// UDPHandler is the terminal of the L4-UDP chain — forwards the datagram to the backend.
type UDPHandler func(ctx UDPContext, payload []byte, sink UDPSink)

// L4UDPMiddleware wraps a UDPHandler.
type L4UDPMiddleware func(next UDPHandler) UDPHandler

// L7Middleware is the standard Go HTTP middleware shape. The terminal handler is
// httputil.ReverseProxy. Defined as a named type for symmetry with L4 chain types.
type L7Middleware func(next http.Handler) http.Handler

// ResponseModifier is the response-side counterpart of L7Middleware. Multiple
// ResponseModifiers compose into the single httputil.ReverseProxy.ModifyResponse
// callback (run in declared order; first error short-circuits).
type ResponseModifier func(*http.Response) error

// Layer identifies which chain a middleware belongs to. A factory may register under
// multiple layers (e.g., ip_allowlist registers as both LayerL4 and LayerL4UDP).
type Layer int

const (
	LayerL4 Layer = iota
	LayerL4UDP
	LayerL7
	LayerL7Response
)

// String returns a human-readable layer name (used in error messages).
func (l Layer) String() string {
	switch l {
	case LayerL4:
		return "L4"
	case LayerL4UDP:
		return "L4UDP"
	case LayerL7:
		return "L7"
	case LayerL7Response:
		return "L7Response"
	default:
		return "unknown"
	}
}

// Factory constructs a middleware instance for a specific endpoint.
//
// The returned interface{} must be one of: L4Middleware, L4UDPMiddleware,
// L7Middleware, ResponseModifier — matching one of the layers the factory was
// registered for. Build picks the appropriate type based on which chain it is
// composing.
//
// Per S5 fix in plan: factories MUST capture deps as getter functions (read each
// invocation), not snapshot values, so dependency hot-swap (SetUserManager,
// SetSessionStore, etc.) propagates to in-flight chains without rebuild.
type Factory func(params map[string]any, ep EndpointInfo, deps RegistryDeps) (any, error)

// RegistryDeps is the opaque dep-bag passed to factories. Middlewares fetch
// named entries via Get and type-assert to the concrete type they need.
//
// The bag is opaque (any-typed) so the middleware package does not import
// concrete service types like *auth.UserManager — that would force every
// consumer of the middleware package to drag those imports.
//
// Per S5 fix in plan: each entry is a getter function so dep hot-swap propagates.
type RegistryDeps interface {
	// Get returns the value for the named dep, or nil if not registered.
	// Implementations should invoke a getter function on each call so callers
	// see the current value (not a stale snapshot).
	Get(name string) any
}

// MapDeps is a simple RegistryDeps implementation backed by a name → getter map.
// Each Get invocation calls the registered getter function, supporting hot-swap.
type MapDeps map[string]func() any

// Get implements RegistryDeps.
func (m MapDeps) Get(name string) any {
	if g, ok := m[name]; ok && g != nil {
		return g()
	}
	return nil
}

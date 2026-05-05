package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/auth"
	"piccolod/internal/services/middleware"
	"piccolod/internal/services/middleware/builtin"
	"piccolod/internal/services/middleware/l7"
	l7oidc "piccolod/internal/services/middleware/l7/oidc"
)

type connectionHint struct {
	clientIP   string
	isTLS      bool
	remotePort int
}

type hintContextKey struct{}
type hintLookupContextKey struct{}

const (
	HeaderPiccoloHintToken = "X-Piccolo-Hint-Token"
	requestHintTokenBytes  = 18
	requestHintTokenTTL    = 15 * time.Second
	requestHintTokenMax    = 4096
)

type tokenHintEntry struct {
	hint         connectionHint
	listenerPort int
	expiresAt    time.Time
}

type hintLookup struct {
	pm           *ProxyManager
	listenerPort int
	sourcePort   int

	once sync.Once
	hint connectionHint
	ok   bool
}

func (l *hintLookup) get() (connectionHint, bool) {
	if l == nil || l.pm == nil || l.listenerPort <= 0 || l.sourcePort <= 0 {
		return connectionHint{}, false
	}
	l.once.Do(func() {
		l.hint, l.ok = l.pm.consumeHint(l.listenerPort, l.sourcePort)
	})
	return l.hint, l.ok
}

// ProxyManager manages TCP listeners and proxies traffic based on ServiceEndpoint
type ProxyManager struct {
	mu                sync.Mutex
	listeners         map[int]net.Listener   // TCP listeners by public port
	udpListeners      map[int]*udpProxyState // UDP listeners by public port
	hints             map[int]map[int]connectionHint
	tokenHints        map[string]tokenHintEntry
	cspFrameAncestors string // pre-calculated CSP header value
	wg                sync.WaitGroup
	acme              http.Handler

	// Auth dependencies for trusted headers middleware (RFC 5.2)
	userManager   *auth.UserManager
	sessionGetter func(r *http.Request) (*auth.Session, bool)
	portalOrigin  func(r *http.Request) string
	aliasChecker  func(host, listener string) bool

	// RFC 20260122: Proxy OIDC handler for headers/protected strategies
	proxyOIDC           *l7oidc.Handler
	sessionStore        *auth.SessionStore
	localHostnameGetter func() string

	// OIDC authorize URL rewriting for oidc_passthrough apps (WAN only).
	// The proxy rewrites the stable LAN-based authorization URL to the
	// correct portal origin for each WAN request. The issuer origin is a
	// closure (not a snapshot) so it tracks hostname/IP changes — important
	// when mDNS is disabled and the value comes from getPreferredOutboundIP.
	oidcIssuerOriginFn func() string
	oidcAuthorizePaths map[string][]string // app name → authorize_paths from manifest

	// Middleware registry holds canonical L7 + L7Response factories that
	// drive startHTTPProxy chain composition. Initialized once per
	// ProxyManager via builtin.RegisterDefaults.
	registry *middleware.Registry
}

func NewProxyManager() *ProxyManager {
	reg := middleware.NewRegistry()
	builtin.RegisterDefaults(reg)
	// Default safe CSP
	return &ProxyManager{
		registry:          reg,
		listeners:         make(map[int]net.Listener),
		udpListeners:      make(map[int]*udpProxyState),
		cspFrameAncestors: "frame-ancestors \"self\" http://localhost:* http://*.local:* https://*.local:*",
	}
}

func (p *ProxyManager) IssueRequestHint(listenerPort int, clientIP string, isTLS bool, remotePort int) (string, bool) {
	if listenerPort <= 0 {
		return "", false
	}
	clientIP = strings.TrimSpace(clientIP)
	if clientIP != "" {
		if ip := net.ParseIP(clientIP); ip == nil {
			return "", false
		}
	}
	if remotePort < 0 {
		return "", false
	}

	token, err := randomHintToken()
	if err != nil {
		return "", false
	}
	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pruneTokenHintsLocked(now)
	if p.tokenHints == nil {
		p.tokenHints = make(map[string]tokenHintEntry)
	}

	for i := 0; i < 4; i++ {
		if _, exists := p.tokenHints[token]; !exists {
			break
		}
		tok, err := randomHintToken()
		if err != nil {
			return "", false
		}
		token = tok
	}
	if _, exists := p.tokenHints[token]; exists {
		return "", false
	}

	// Best-effort bound: if we're at max after pruning, evict one arbitrary entry.
	if len(p.tokenHints) >= requestHintTokenMax {
		for k := range p.tokenHints {
			delete(p.tokenHints, k)
			break
		}
	}

	p.tokenHints[token] = tokenHintEntry{
		hint: connectionHint{
			clientIP:   clientIP,
			isTLS:      isTLS,
			remotePort: remotePort,
		},
		listenerPort: listenerPort,
		expiresAt:    now.Add(requestHintTokenTTL),
	}
	return token, true
}

func (p *ProxyManager) consumeRequestHint(listenerPort int, token string) (connectionHint, bool) {
	if p == nil || listenerPort <= 0 {
		return connectionHint{}, false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return connectionHint{}, false
	}
	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneTokenHintsLocked(now)
	if p.tokenHints == nil {
		return connectionHint{}, false
	}
	entry, ok := p.tokenHints[token]
	if ok {
		delete(p.tokenHints, token)
	}
	if !ok {
		return connectionHint{}, false
	}
	if entry.listenerPort != listenerPort {
		return connectionHint{}, false
	}
	if now.After(entry.expiresAt) {
		return connectionHint{}, false
	}
	return entry.hint, true
}

func (p *ProxyManager) pruneTokenHintsLocked(now time.Time) {
	if p == nil || p.tokenHints == nil {
		return
	}
	for k, v := range p.tokenHints {
		if now.After(v.expiresAt) {
			delete(p.tokenHints, k)
		}
	}
}

func randomHintToken() (string, error) {
	var b [requestHintTokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func (p *ProxyManager) registerHint(listenerPort, sourcePort int, hint connectionHint) {
	if sourcePort <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hints == nil {
		p.hints = make(map[int]map[int]connectionHint)
	}
	m := p.hints[listenerPort]
	if m == nil {
		m = make(map[int]connectionHint)
		p.hints[listenerPort] = m
	}
	m[sourcePort] = hint
}

func (p *ProxyManager) consumeHint(listenerPort, sourcePort int) (connectionHint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.hints[listenerPort]; m != nil {
		if hint, ok := m[sourcePort]; ok {
			delete(m, sourcePort)
			if len(m) == 0 {
				delete(p.hints, listenerPort)
			}
			return hint, true
		}
	}
	return connectionHint{}, false
}

// SetAcmeHandler registers a handler to serve HTTP-01 challenges for all HTTP proxies.
func (p *ProxyManager) SetAcmeHandler(h http.Handler) { p.mu.Lock(); p.acme = h; p.mu.Unlock() }

// SetAuthConfig configures the auth dependencies required for trusted headers middleware.
// This must be called before starting proxies for apps using auth.strategy: headers.
func (p *ProxyManager) SetAuthConfig(um *auth.UserManager, sg func(r *http.Request) (*auth.Session, bool)) {
	p.mu.Lock()
	p.userManager = um
	p.sessionGetter = sg
	p.mu.Unlock()
}

// SetPortalOriginResolver configures how browser redirects should resolve the portal origin.
// This is required for correct login redirects in WAN mode (portal and app are different origins).
func (p *ProxyManager) SetPortalOriginResolver(fn func(r *http.Request) string) {
	p.mu.Lock()
	p.portalOrigin = fn
	p.mu.Unlock()
}

// SetAliasChecker configures a callback that reports whether a request host is an alias domain
// for a given listener name. Used for RFC 20260112 alias-domain warnings.
func (p *ProxyManager) SetAliasChecker(fn func(host, listener string) bool) {
	p.mu.Lock()
	p.aliasChecker = fn
	p.mu.Unlock()
}

// SetSessionStore sets the session store for app session creation per RFC 20260122.
func (p *ProxyManager) SetSessionStore(store *auth.SessionStore) {
	p.mu.Lock()
	p.sessionStore = store
	p.mu.Unlock()
}

// SetLocalHostnameGetter sets the function to get the current mDNS hostname.
func (p *ProxyManager) SetLocalHostnameGetter(fn func() string) {
	p.mu.Lock()
	p.localHostnameGetter = fn
	p.mu.Unlock()
}

// SetProxyOIDCConfig configures the proxy OIDC handler per RFC 20260122. The
// services-internal predicates (TLS detection + CHIPS classification) are
// wired in here because they depend on the hint chain that step 9 migrates to
// middleware.HintFromContext.
func (p *ProxyManager) SetProxyOIDCConfig(config l7oidc.Config) {
	config.ArrivedViaTLS = RequestArrivedViaTLS
	config.NeedsEmbeddedMarker = needsEmbeddedMarker
	config.ShouldPartitionCookies = shouldPartitionCookies

	p.mu.Lock()
	p.proxyOIDC = l7oidc.NewHandler(config)
	p.mu.Unlock()
}

// SetOIDCIssuerOrigin sets a callback that returns the stable LAN-based origin
// the OIDC discovery document advertises for the authorization_endpoint
// (e.g., "http://piccolo-abc123.local"). The callback is invoked per-request
// so the proxy tracks hostname/IP changes (e.g., when mDNS is disabled and the
// value comes from getPreferredOutboundIP). Must match the discovery handler's
// authorization_endpoint origin exactly.
func (p *ProxyManager) SetOIDCIssuerOrigin(fn func() string) {
	if fn == nil {
		log.Printf("ERROR: SetOIDCIssuerOrigin called with nil function, OIDC rewriting disabled")
		return
	}
	p.mu.Lock()
	p.oidcIssuerOriginFn = fn
	p.mu.Unlock()
	log.Printf("INFO: OIDC issuer origin function configured for proxy rewriting")
}

// SetOIDCAuthorizePaths stores the authorize_paths from an app's manifest.
// These paths scope Layer 2 body rewriting for OIDC authorization URLs.
func (p *ProxyManager) SetOIDCAuthorizePaths(appName string, paths []string) {
	p.mu.Lock()
	if p.oidcAuthorizePaths == nil {
		p.oidcAuthorizePaths = make(map[string][]string)
	}
	if len(paths) > 0 {
		p.oidcAuthorizePaths[appName] = paths
	} else {
		delete(p.oidcAuthorizePaths, appName)
	}
	p.mu.Unlock()
}

// ClearOIDCAuthorizePaths removes the authorize_paths for an app.
func (p *ProxyManager) ClearOIDCAuthorizePaths(appName string) {
	p.mu.Lock()
	delete(p.oidcAuthorizePaths, appName)
	p.mu.Unlock()
}

// SetAllowedAncestors updates the list of hostnames allowed to frame apps.
// It constructs a CSP frame-ancestors directive including default local origins and wildcard ports.
func (p *ProxyManager) SetAllowedAncestors(hosts []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Base defaults with wildcard ports to support dev/non-standard ports
	sources := []string{
		"'self'",
		"http://localhost:*",
		"http://*.local:*",
		"https://*.local:*",
	}

	for _, h := range hosts {
		if h == "" {
			continue
		}
		// Assume https for remote hosts unless specified
		var entry string
		if !strings.Contains(h, "://") {
			entry = "https://" + h
		} else {
			entry = h
		}
		// Also allow wildcard ports for the configured host
		sources = append(sources, entry+":*")
	}

	p.cspFrameAncestors = fmt.Sprintf("frame-ancestors %s", strings.Join(sources, " "))
}

// StartListener starts a proxy for the given endpoint (TCP or UDP).
func (p *ProxyManager) StartListener(ep ServiceEndpoint) {
	if ep.Flow == api.FlowUDP {
		p.startUDPProxy(ep)
		return
	}

	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(ep.PublicPort))
	// Avoid double-start
	p.mu.Lock()
	if _, exists := p.listeners[ep.PublicPort]; exists {
		p.mu.Unlock()
		return
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("WARN: Failed to bind public listener on %s: %v", addr, err)
		p.mu.Unlock()
		return
	}
	p.listeners[ep.PublicPort] = ln
	p.mu.Unlock()

	switch ep.Flow {
	case api.FlowTLS:
		// Raw TCP passthrough
		p.startTCPProxy(ln, ep)
	case api.FlowTCP:
		switch ep.Protocol {
		case api.ListenerProtocolHTTP, api.ListenerProtocolWebsocket:
			p.startHTTPProxy(ln, ep)
		default:
			p.startTCPProxy(ln, ep)
		}
	default:
		p.startTCPProxy(ln, ep)
	}
}

func (p *ProxyManager) handleConn(ep ServiceEndpoint, client net.Conn) {
	defer client.Close()
	backendAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ep.HostBind))

	// For v1: passthrough for all flows; framework in place to add protocol handlers
	backend, err := net.DialTimeout("tcp", backendAddr, 5*time.Second)
	if err != nil {
		log.Printf("WARN: Backend connect failed %s: %v", backendAddr, err)
		return
	}
	defer backend.Close()

	// Bi-directional copy
	done := make(chan struct{}, 2)
	go func() { io.Copy(backend, client); backend.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
	go func() { io.Copy(client, backend); client.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
	<-done
}

func (p *ProxyManager) startTCPProxy(ln net.Listener, ep ServiceEndpoint) {
	mwEndpoint := ep.AsMiddlewareInfo()

	// Build L4 chain via the registry. With no L4 factories registered today,
	// the chain is empty and ComposeL4Chain returns the terminal unchanged.
	// Step 5 lands the canonical L4 entries (ip_allowlist, ip_rate_limit,
	// conn_metrics, hint_consumer_l4) and step 6 adds connection_auth.
	l4Mws, err := p.registry.BuildL4(middleware.BuildSpec{
		Endpoint: mwEndpoint,
		Deps:     p.buildL4Deps(ep),
	})
	if err != nil {
		log.Printf("ERROR: registry.BuildL4 for app=%s listener=%s: %v", ep.App, ep.Name, err)
		return
	}
	terminal := middleware.ConnHandler(func(_ middleware.ConnContext, c net.Conn) {
		p.handleConn(ep, c)
	})
	chain := middleware.ComposeL4Chain(l4Mws, terminal)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		log.Printf("INFO: TCP proxy %s → 127.0.0.1:%d (app=%s listener=%s)", ln.Addr().String(), ep.HostBind, ep.App, ep.Name)
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Temporary() {
					time.Sleep(50 * time.Millisecond)
					continue
				}
				return
			}
			ctx := middleware.ConnContext{
				Endpoint:    mwEndpoint,
				SourceAddr:  conn.RemoteAddr(),
				LocalAddr:   conn.LocalAddr(),
				AcceptedAt:  time.Now(),
				SourceTrust: middleware.DeriveSourceTrust(conn.LocalAddr()),
				Hint:        l4HintLookup(p, ep.PublicPort, conn.RemoteAddr()),
			}
			p.wg.Add(1)
			go func(c net.Conn, ctx middleware.ConnContext) {
				defer p.wg.Done()
				chain(ctx, c)
			}(conn, ctx)
		}
	}()
}

func (p *ProxyManager) startHTTPProxy(ln net.Listener, ep ServiceEndpoint) {
	target := "http://127.0.0.1:" + strconv.Itoa(ep.HostBind)
	u, err := url.Parse(target)
	if err != nil {
		log.Printf("WARN: invalid reverse proxy target %s: %v", target, err)
		return
	}
	rp := httputil.NewSingleHostReverseProxy(u)

	// Build the L7 chain via the registry. spec carries the per-listener facts
	// (HasAuth gates path_auth); deps carries the closures that bridge into
	// services-internal state (mutable proxy-manager fields read live per call).
	mwEndpoint := ep.AsMiddlewareInfo()
	l7Spec := middleware.BuildSpec{
		Endpoint: mwEndpoint,
		HasAuth:  ep.Auth != nil && len(ep.Auth.Rules) > 0,
		Deps:     p.buildL7Deps(ep),
	}
	l7Mws, err := p.registry.BuildL7(l7Spec)
	if err != nil {
		log.Printf("ERROR: registry.BuildL7 for app=%s listener=%s: %v", ep.App, ep.Name, err)
		return
	}
	respMods, err := p.registry.BuildL7Response(l7Spec)
	if err != nil {
		log.Printf("ERROR: registry.BuildL7Response for app=%s listener=%s: %v", ep.App, ep.Name, err)
		return
	}

	rp.ModifyResponse = middleware.ComposeResponseChain(respMods)

	terminal := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gzw := l7.NewGzipResponseWriter(w, r)
		defer gzw.Close()
		rp.ServeHTTP(gzw, r)
	}))
	handler := middleware.ComposeRequestChain(l7Mws, terminal)

	// Outer non-listener-specific middleware wrapping (security headers,
	// request logging, rate limiting). Stays out of the registry until those
	// concerns get their own canonical entries.
	handler = p.securityHeaders(handler)
	handler = requestLogging(handler)
	handler = basicRateLimit(handler) // stub

	// L4 chain at accept boundary. The HTTP variant runs the L4 chain for
	// side-effects + deny semantics: when the chain calls its terminal the
	// connection proceeds to http.Server, otherwise it is closed and the
	// listener loops. With no L4 factories registered today, the chain is
	// empty and Accept behavior is identical to the underlying listener.
	l4Mws, err := p.registry.BuildL4(middleware.BuildSpec{
		Endpoint: mwEndpoint,
		Deps:     p.buildL4Deps(ep),
	})
	if err != nil {
		log.Printf("ERROR: registry.BuildL4 for app=%s listener=%s: %v", ep.App, ep.Name, err)
		return
	}
	wrappedLn := newL4AcceptBridge(ln, mwEndpoint, l4Mws, p, ep.PublicPort)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		log.Printf("INFO: HTTP proxy %s → %s (app=%s listener=%s protocol=%s)", ln.Addr().String(), target, ep.App, ep.Name, ep.Protocol.String())
		srv := &http.Server{
			Handler: handler,
			ConnContext: func(ctx context.Context, c net.Conn) context.Context {
				if addr, ok := c.RemoteAddr().(*net.TCPAddr); ok {
					ctx = context.WithValue(ctx, hintLookupContextKey{}, &hintLookup{
						pm:           p,
						listenerPort: ep.PublicPort,
						sourcePort:   addr.Port,
					})
				}
				return ctx
			},
		}
		_ = srv.Serve(wrappedLn) // returns on ln.Close()
	}()
}

// l4AcceptBridge wraps a net.Listener with the L4 middleware chain. Each
// accepted conn passes through the chain; if the chain invokes its terminal
// the conn proceeds to whatever consumes the wrapped listener (http.Server).
// If the chain returns without calling terminal, the conn is closed and
// Accept loops. With an empty chain, terminal is invoked synchronously and
// behavior matches the underlying listener byte-for-byte.
type l4AcceptBridge struct {
	net.Listener
	endpoint     middleware.EndpointInfo
	middlewares  []middleware.L4Middleware
	pm           *ProxyManager
	listenerPort int
}

func newL4AcceptBridge(ln net.Listener, ep middleware.EndpointInfo, mws []middleware.L4Middleware, pm *ProxyManager, listenerPort int) net.Listener {
	if len(mws) == 0 {
		// Empty chain: skip the wrapper entirely. Step 4 default.
		return ln
	}
	return &l4AcceptBridge{
		Listener:     ln,
		endpoint:     ep,
		middlewares:  mws,
		pm:           pm,
		listenerPort: listenerPort,
	}
}

func (l *l4AcceptBridge) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		allowed := false
		terminal := middleware.ConnHandler(func(_ middleware.ConnContext, _ net.Conn) {
			allowed = true
		})
		chain := middleware.ComposeL4Chain(l.middlewares, terminal)
		ctx := middleware.ConnContext{
			Endpoint:    l.endpoint,
			SourceAddr:  conn.RemoteAddr(),
			LocalAddr:   conn.LocalAddr(),
			AcceptedAt:  time.Now(),
			SourceTrust: middleware.DeriveSourceTrust(conn.LocalAddr()),
			Hint:        l4HintLookup(l.pm, l.listenerPort, conn.RemoteAddr()),
		}
		chain(ctx, conn)
		if allowed {
			return conn, nil
		}
		_ = conn.Close()
	}
}

// Middleware stubs
func (p *ProxyManager) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		// CORP cross-origin: required because portal (piccolo.domain.com) embeds apps
		// (app.piccolo.domain.com) in iframes. These are different origins, so same-site
		// would block embedding. frame-ancestors CSP still restricts framing origins.
		w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
		w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")

		p.mu.Lock()
		csp := p.cspFrameAncestors
		p.mu.Unlock()

		remoteIp, _ := splitHostPortValue(r.RemoteAddr)
		ip := net.ParseIP(remoteIp)
		if ip != nil && (ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate()) {
			host, _ := splitHostPortValue(r.Host)
			if host != "" {
				if net.ParseIP(host) == nil {
					// Hostname-based access: add wildcard-port entries.
					host = host + ":*"
					csp += " https://" + host + " http://" + host
				} else {
					// IP-based LAN access: validate via LocalAddrContextKey to prevent
					// Host header spoofing, then allow the portal IP as frame ancestor.
					// Skip loopback — already covered by http://localhost:* in base CSP.
					hostIP := net.ParseIP(host)
					if localAddr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && hostIP != nil {
						if localHost, _, err := net.SplitHostPort(localAddr.String()); err == nil {
							if localIP := net.ParseIP(localHost); localIP != nil && !localIP.IsLoopback() && localIP.Equal(hostIP) {
								// Bracket IPv6 for valid URI syntax in CSP source expressions.
								cspHost := host
								if hostIP.To4() == nil {
									cspHost = "[" + host + "]"
								}
								csp += " http://" + cspHost + ":* https://" + cspHost + ":*"
							}
						}
					}
				}
			}
		}

		w.Header().Add("Content-Security-Policy", csp)

		next.ServeHTTP(w, r)
	})
}

func requestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Minimal logging to avoid noise in tests
		next.ServeHTTP(w, r)
	})
}

func basicRateLimit(next http.Handler) http.Handler { // placeholder
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

func hintFromRequest(r *http.Request) (connectionHint, bool) {
	if hint, ok := r.Context().Value(hintContextKey{}).(connectionHint); ok {
		return hint, true
	}
	if lookup, ok := r.Context().Value(hintLookupContextKey{}).(*hintLookup); ok {
		return lookup.get()
	}
	return connectionHint{}, false
}

func applyForwardHeaders(r *http.Request, ep ServiceEndpoint) {
	clientIP := ""
	if remoteHost, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		clientIP = remoteHost
	} else {
		clientIP = r.RemoteAddr
	}
	if hint, ok := hintFromRequest(r); ok {
		if strings.TrimSpace(hint.clientIP) != "" {
			clientIP = hint.clientIP
		}
	}

	// Trust based on LocalAddr (the interface that received the connection), not RemoteAddr.
	// This prevents header spoofing via internal routing (e.g., LAN host-based routing)
	// while still trusting traffic that arrives on loopback-only listeners (e.g., Nexus via TLS mux).
	trusted := false
	if localAddr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if host, _, err := net.SplitHostPort(localAddr.String()); err == nil {
			if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
				trusted = true
			}
		}
	}

	host, hostPort := splitHostPortValue(r.Host)
	if host == "" {
		altHost, altPort := splitHostPortValue(r.URL.Host)
		host, hostPort = altHost, altPort
	}
	forwardHost := host
	if hostPort != "" {
		forwardHost = net.JoinHostPort(host, hostPort)
	}

	// Derive proto/host/port, but preserve existing forwarded headers for trusted loopback sources.
	proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	if !trusted || proto == "" {
		proto = "http"
		if l7.ShouldRewriteAsHTTPS(ep.AsMiddlewareInfo(), r, RequestArrivedViaTLS) {
			proto = "https"
		}
	}

	if trusted {
		if r.Header.Get("X-Forwarded-Proto") == "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		if forwardHost != "" && r.Header.Get("X-Forwarded-Host") == "" {
			r.Header.Set("X-Forwarded-Host", forwardHost)
		}
	} else {
		r.Header.Set("X-Forwarded-Proto", proto)
		if forwardHost != "" {
			r.Header.Set("X-Forwarded-Host", forwardHost)
		}
	}

	port := ""
	if hint, ok := hintFromRequest(r); ok && hint.remotePort > 0 {
		port = strconv.Itoa(hint.remotePort)
	} else if hostPort != "" {
		port = hostPort
	} else if proto == "https" {
		port = "443"
	} else {
		port = "80"
	}

	if trusted {
		if r.Header.Get("X-Forwarded-Port") == "" && port != "" {
			r.Header.Set("X-Forwarded-Port", port)
		}
		if r.Header.Get("X-Real-IP") == "" && clientIP != "" {
			r.Header.Set("X-Real-IP", clientIP)
		}
		// X-Forwarded-For: append
		if clientIP != "" {
			if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
				r.Header.Set("X-Forwarded-For", prior+", "+clientIP)
			} else {
				r.Header.Set("X-Forwarded-For", clientIP)
			}
		}
	} else {
		if port != "" {
			r.Header.Set("X-Forwarded-Port", port)
		}
		if clientIP != "" {
			r.Header.Set("X-Real-IP", clientIP)
			r.Header.Set("X-Forwarded-For", clientIP)
		}
	}

	// Forwarded header (RFC 7239): append for trusted, overwrite for untrusted.
	fwdHost := normalizeForwardedHostValue(r.Header.Get("X-Forwarded-Host"))
	parts := []string{fmt.Sprintf("proto=%s", proto)}
	if fwdHost != "" {
		parts = append(parts, fmt.Sprintf("host=%s", forwardedPairValue(fwdHost)))
	}
	if forValue := normalizeForwardedForValue(clientIP); forValue != "" {
		parts = append(parts, fmt.Sprintf("for=%s", forwardedPairValue(forValue)))
	}
	fwdValue := strings.Join(parts, ";")
	if trusted {
		if prior := r.Header.Get("Forwarded"); prior != "" {
			r.Header.Set("Forwarded", prior+", "+fwdValue)
		} else {
			r.Header.Set("Forwarded", fwdValue)
		}
	} else {
		r.Header.Set("Forwarded", fwdValue)
	}

	r.URL.Scheme = proto
}

func splitHostPortValue(value string) (string, string) {
	if value == "" {
		return "", ""
	}
	if strings.Contains(value, ":") {
		if host, port, err := net.SplitHostPort(value); err == nil {
			return host, port
		}
	}
	return value, ""
}

func normalizeForwardedHostValue(hostport string) string {
	h, p := splitHostPortValue(strings.TrimSpace(hostport))
	if h == "" {
		return ""
	}

	h = strings.Trim(h, "[]")
	if i := strings.IndexByte(h, '%'); i != -1 {
		h = h[:i] // drop zone (e.g., fe80::1%eth0)
	}

	if ip := net.ParseIP(h); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			h = ip4.String()
		} else {
			h = "[" + ip.String() + "]"
		}
	} else {
		h = strings.ToLower(h)
	}

	if p != "" {
		return h + ":" + p
	}
	return h
}

func normalizeForwardedForValue(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	v = strings.Trim(v, "[]")
	if i := strings.IndexByte(v, '%'); i != -1 {
		v = v[:i]
	}
	if ip := net.ParseIP(v); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return ip4.String()
		}
		return "[" + ip.String() + "]"
	}
	return v
}

func forwardedPairValue(v string) string {
	if v == "" {
		return ""
	}
	if !needsForwardedQuotedString(v) {
		return v
	}
	escaped := strings.ReplaceAll(v, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return "\"" + escaped + "\""
}

func needsForwardedQuotedString(v string) bool {
	for i := 0; i < len(v); i++ {
		ch := v[i]
		switch {
		case 'a' <= ch && ch <= 'z':
			continue
		case 'A' <= ch && ch <= 'Z':
			continue
		case '0' <= ch && ch <= '9':
			continue
		}
		switch ch {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return true
		}
	}
	return false
}

// RequestArrivedViaTLS reports whether the original client request was made
// over TLS, even if the current hop is plain HTTP. It checks direct TLS
// (r.TLS) and the connection hint issued by the portal's lanHostRoutingMiddleware.
//
// Stays in services/ until step 9 because the hint chain (hintFromRequest →
// connectionHint) is services-internal. Step 9 migrates to
// middleware.HintFromContext, after which this function moves to middleware/l7/
// (or its callers inline middleware.HintFromContext directly).
func RequestArrivedViaTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if hint, ok := hintFromRequest(r); ok && hint.isTLS {
		return true
	}
	return false
}

// buildL4Deps assembles the per-listener RegistryDeps for L4 / L4UDP chain
// factories. Empty in step 4 — the L4 chain has no factories registered yet.
// Step 5 lands hint_consumer_l4 / ip_allowlist / ip_rate_limit / conn_metrics
// and populates this map with their dep keys.
func (p *ProxyManager) buildL4Deps(_ ServiceEndpoint) middleware.RegistryDeps {
	return middleware.MapDeps{}
}

// l4HintLookup returns the lazy Hint accessor attached to ConnContext at
// L4 chain entry. Step 4: stub — no L4 middleware reads Hint. Step 5
// (hint_consumer_l4) replaces the stub with the real bridge into
// p.consumeHint, mirroring the L7 hint chain that lives in startHTTPProxy
// via http.Server.ConnContext.
func l4HintLookup(_ *ProxyManager, _ int, _ net.Addr) func() (middleware.Hint, bool) {
	return func() (middleware.Hint, bool) { return middleware.Hint{}, false }
}

// buildL7Deps assembles the per-listener RegistryDeps that the canonical
// chain factories pull from. Each entry is a getter function so dep hot-swap
// (SetUserManager etc.) propagates without registry rebuild.
func (p *ProxyManager) buildL7Deps(ep ServiceEndpoint) middleware.RegistryDeps {
	listenerPort := ep.PublicPort
	appName := ep.App

	hintConsumer := func(r *http.Request) *http.Request {
		token := strings.TrimSpace(r.Header.Get(HeaderPiccoloHintToken))
		if token == "" {
			return r
		}
		r.Header.Del(HeaderPiccoloHintToken)
		if hint, ok := p.consumeRequestHint(listenerPort, token); ok {
			r = r.WithContext(context.WithValue(r.Context(), hintContextKey{}, hint))
		}
		return r
	}

	cookieContext := func(r *http.Request) *http.Request {
		rewriteCookies := l7.ShouldRewriteLegacyCookies(r.Host)
		partitionCookies := shouldPartitionCookies(r)
		needsMarker := needsEmbeddedMarker(r)
		l7.StripAndRewriteRequestCookies(r, appName, rewriteCookies)
		return l7.SetProxyContext(r, appName, l7.NormalizeHostNoPort(r.Host), rewriteCookies, partitionCookies, needsMarker)
	}

	pathAuthSnapshot := func() l7.PathAuthSnapshot {
		p.mu.Lock()
		defer p.mu.Unlock()
		snap := l7.PathAuthSnapshot{
			UserManager:   p.userManager,
			SessionGetter: p.sessionGetter,
			PortalOrigin:  p.portalOrigin,
			AliasChecker:  p.aliasChecker,
			SessionStore:  p.sessionStore,
		}
		if p.proxyOIDC != nil {
			snap.InitiateOIDC = p.proxyOIDC.InitiateFlow
		}
		return snap
	}

	authSnapshot := func() l7oidc.AuthorizeSnapshotState {
		p.mu.Lock()
		defer p.mu.Unlock()
		return l7oidc.AuthorizeSnapshotState{
			IssuerOrigin:   p.oidcIssuerOriginFn,
			PortalOrigin:   p.portalOrigin,
			AuthorizePaths: p.oidcAuthorizePaths[appName],
		}
	}

	acmeHandler := func() http.Handler {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.acme
	}

	reservedCallback := func() l7oidc.CallbackHandlerFn {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.proxyOIDC == nil {
			return nil
		}
		return p.proxyOIDC.HandleCallback
	}

	forwardHeaders := func(r *http.Request) {
		applyForwardHeaders(r, ep)
	}

	return middleware.MapDeps{
		builtin.DepHintConsumerL7:        func() any { return hintConsumer },
		builtin.DepReservedPathCallback:  func() any { return reservedCallback },
		builtin.DepACMEHandler:           func() any { return l7.ACMEHandlerFn(acmeHandler) },
		builtin.DepArrivedViaTLS:         func() any { return l7.ArrivedViaTLSFn(RequestArrivedViaTLS) },
		builtin.DepPathAuthSnapshot:      func() any { return pathAuthSnapshot },
		builtin.DepCookieContext:         func() any { return cookieContext },
		builtin.DepOIDCAuthorizeSnapshot: func() any { return authSnapshot },
		builtin.DepForwardHeaders:        func() any { return forwardHeaders },
		builtin.DepNeedsMarker:           func() any { return l7.NeedsMarkerFn(needsEmbeddedMarker) },
		builtin.DepMarkerCookie:          func() any { return l7.MarkerCookieFn(l7.EmbeddedMarkerSetCookie) },
	}
}

// StopAll stops all listeners
func (p *ProxyManager) StopAll() {
	p.mu.Lock()
	for port, ln := range p.listeners {
		_ = ln.Close()
		delete(p.listeners, port)
		delete(p.hints, port)
	}
	// Collect UDP states under the lock, stop them after releasing to avoid
	// blocking p.mu during drain (each stop() waits for goroutines).
	udpStates := make([]*udpProxyState, 0, len(p.udpListeners))
	for port, state := range p.udpListeners {
		udpStates = append(udpStates, state)
		delete(p.udpListeners, port)
	}
	p.mu.Unlock()
	for _, state := range udpStates {
		state.stop()
	}
	p.wg.Wait()
}

// StopPort stops a specific public listener if running (TCP or UDP).
// For backward compatibility, stops both protocols on the same port.
func (p *ProxyManager) StopPort(port int) {
	p.StopEndpoint(port, api.FlowTCP)
	p.StopEndpoint(port, api.FlowUDP)
}

// StopEndpoint stops a specific listener by port and flow type.
// Only stops the matching protocol, leaving the other intact.
func (p *ProxyManager) StopEndpoint(port int, flow api.ListenerFlow) {
	p.mu.Lock()
	if flow != api.FlowUDP {
		// TCP/TLS listeners are in the TCP map
		if ln, ok := p.listeners[port]; ok {
			_ = ln.Close()
			delete(p.listeners, port)
		}
		delete(p.hints, port)
	}
	var udpState *udpProxyState
	if flow == api.FlowUDP {
		if state, ok := p.udpListeners[port]; ok {
			udpState = state
			delete(p.udpListeners, port)
		}
	}
	p.mu.Unlock()
	if udpState != nil {
		udpState.stop()
	}
}

// small int→string helper without strconv to keep deps minimal
// no extra helpers

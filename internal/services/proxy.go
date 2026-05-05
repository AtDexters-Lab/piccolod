package services

import (
	"bytes"
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
	"piccolod/internal/services/middleware/l7"
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
	listeners         map[int]net.Listener     // TCP listeners by public port
	udpListeners      map[int]*udpProxyState   // UDP listeners by public port
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
	proxyOIDC           *ProxyOIDCHandler
	sessionStore        *auth.SessionStore
	localHostnameGetter func() string

	// OIDC authorize URL rewriting for oidc_passthrough apps (WAN only).
	// The proxy rewrites the stable LAN-based authorization URL to the
	// correct portal origin for each WAN request. The issuer origin is a
	// closure (not a snapshot) so it tracks hostname/IP changes — important
	// when mDNS is disabled and the value comes from getPreferredOutboundIP.
	oidcIssuerOriginFn func() string
	oidcAuthorizePaths map[string][]string // app name → authorize_paths from manifest
}

func NewProxyManager() *ProxyManager {
	// Default safe CSP
	return &ProxyManager{
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

// SetProxyOIDCConfig configures the proxy OIDC handler per RFC 20260122.
func (p *ProxyManager) SetProxyOIDCConfig(config ProxyOIDCConfig) {
	p.mu.Lock()
	p.proxyOIDC = NewProxyOIDCHandler(config)
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
			// TODO L0: rate-limit + metrics per IP (stub)
			p.wg.Add(1)
			go func(c net.Conn) {
				defer p.wg.Done()
				p.handleConn(ep, c)
			}(conn)
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

	// Response-side modifier composition (extraction substep 2a per plan §H).
	// Modifiers run in canonical order:
	//   1. security_headers_response — extracted (l7.SecurityHeadersResponse)
	//   2. cookie_isolation_response — INLINE (substep 2c)
	//   3. embedded_marker_response  — extracted (l7.EmbeddedMarkerResponse)
	//   4. oidc_authorize_rewrite_response — INLINE (substep 2d)
	secHeadersResp := l7.SecurityHeadersResponse()
	markerResp := l7.EmbeddedMarkerResponse(needsEmbeddedMarker, embeddedMarkerSetCookie)
	rp.ModifyResponse = func(resp *http.Response) error {
		if err := secHeadersResp(resp); err != nil {
			return err
		}

		// RFC 20260112: Set-Cookie blocking + optional LAN port-based cookie isolation.
		setCookies := resp.Header.Values("Set-Cookie")
		if len(setCookies) > 0 {
			resp.Header.Del("Set-Cookie")

			appHost := normalizeHostNoPort(proxyContextAppHost(resp.Request.Context()))
			rewriteCookies := proxyContextCookieRewrite(resp.Request.Context())
			partitionCookies := proxyContextPartitionCookies(resp.Request.Context())
			appPrefix := cookiePrefixForApp(ep.App)

			for _, sc := range setCookies {
				name, eq := parseSetCookieName(sc)
				if name == "" || eq == -1 {
					continue
				}
				if isPiccoloCookieName(name) {
					continue
				}

				if dom, ok := setCookieDomain(sc); ok {
					// If we can't determine the app host, fail closed for Domain cookies.
					if appHost == "" {
						continue
					}
					if normalizeCookieDomain(dom) != appHost {
						continue
					}
				}

				if rewriteCookies && setCookieHasHttpOnly(sc) && !strings.HasPrefix(name, appPrefix) {
					sc = appPrefix + name + sc[eq:]
				}

				// CHIPS: add Partitioned, SameSite=None, Secure for cross-site iframe embedding
				if partitionCookies {
					sc = ensurePartitionedAttributes(sc)
				}

				resp.Header.Add("Set-Cookie", sc)
			}
		}

		if err := markerResp(resp); err != nil {
			return err
		}

		// OIDC authorize URL rewriting for WAN requests.
		if snap, ok := resp.Request.Context().Value(proxyOIDCRewriteContextKey{}).(*oidcRewriteSnapshot); ok && snap != nil {
			rewriteOIDCAuthorizeResponse(resp, snap, ep.App)
		}

		return nil
	}

	// Request-side L7 chain composition (extraction substep 2a per plan §H).
	// Build innermost-first; outer wraps inner.
	//
	//   forwarded_scrub
	//     → path_normalize
	//       → INLINE handler (hint_consumer_l7, reserved_path_intercept, ACME bypass,
	//                          path_auth, cookie context, oidc_authorize_snapshot)
	//         → forward_headers
	//           → gzip + reverse_proxy (terminal)
	//
	// Extracted in this substep: forwarded_scrub, path_normalize, forward_headers.
	// Remaining inline pending substeps 2b–2e.

	terminal := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gzw := newGzipResponseWriter(w, r)
		defer gzw.Close()
		rp.ServeHTTP(gzw, r)
	}))
	withForward := l7.ForwardHeaders(func(r *http.Request) { applyForwardHeaders(r, ep) })(terminal)

	inlineHandler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// hint_consumer_l7 (inline; extraction deferred to step 9 alongside the
		// connectionHint→middleware.HintFromContext migration).
		if token := strings.TrimSpace(r.Header.Get(HeaderPiccoloHintToken)); token != "" {
			r.Header.Del(HeaderPiccoloHintToken)
			if hint, ok := p.consumeRequestHint(ep.PublicPort, token); ok {
				r = r.WithContext(context.WithValue(r.Context(), hintContextKey{}, hint))
			}
		}

		// path_normalize already mutated r.URL.Path; downstream reads it directly.
		cleanedPath := r.URL.Path

		// RFC 20260122 §5.10: Intercept reserved proxy OIDC paths
		if IsReservedPath(cleanedPath) {
			p.mu.Lock()
			proxyOIDC := p.proxyOIDC
			p.mu.Unlock()

			if proxyOIDC == nil {
				writeProxyJSONError(w, http.StatusServiceUnavailable, "proxy_oidc_not_configured", "OIDC_UNAVAILABLE")
				return
			}

			if cleanedPath == ProxyOIDCCallbackPath {
				proxyOIDC.HandleCallback(w, r, ep.App, ep)
				return
			}

			// Other reserved paths (future: logout, session status)
			writeProxyJSONError(w, http.StatusNotFound, "not_found", "RESERVED_PATH_NOT_IMPLEMENTED")
			return
		}

		// RFC 4.1.5: Strip spoofed trusted headers for all strategies.
		// Must happen before any handler (ACME, backend) to prevent header spoofing.
		StripHeadersFromRequest(r)

		// Intercept ACME HTTP-01 challenges for remote cert issuance (RFC 20260122).
		// These bypass auth rules because they're infrastructure-level (piccolod's TLS
		// termination), not app business logic. External ACME verifiers have no session.
		if strings.HasPrefix(cleanedPath, "/.well-known/acme-challenge/") {
			p.mu.Lock()
			acme := p.acme
			p.mu.Unlock()
			if acme != nil {
				acme.ServeHTTP(w, r)
				return
			}
		}

		strategy := l7.ListenerStrategyForPath(ep.Auth, cleanedPath)

		// RFC 4.1.6: Strategy-specific behavior.
		switch strategy {
		case "protected", "headers":
			// RFC 20260122 §5: Allow CORS preflight (OPTIONS) to bypass auth.
			// Browsers send preflight without cookies; blocking them breaks cross-origin API calls.
			if r.Method == http.MethodOptions && r.Header.Get("Origin") != "" && r.Header.Get("Access-Control-Request-Method") != "" {
				break
			}

			p.mu.Lock()
			um := p.userManager
			sg := p.sessionGetter
			portalOrigin := p.portalOrigin
			aliasChecker := p.aliasChecker
			proxyOIDC := p.proxyOIDC
			sessionStore := p.sessionStore
			p.mu.Unlock()

			if sg == nil {
				http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
				return
			}

			// RFC 20260112: alias domains are not compatible with protected/headers strategies.
			// The session cookie cannot be shared across custom domains.
			if aliasChecker != nil && aliasChecker(normalizeHostNoPort(r.Host), ep.DerivedHostLabel) {
				log.Printf("WARN: alias domain access is not supported for auth strategy=%s (host=%s app=%s listener=%s)", strategy, normalizeHostNoPort(r.Host), ep.App, ep.Name)
			}

			_, cookieErr := r.Cookie(sessionCookieName)
			cookiePresent := cookieErr == nil

			// RFC 20260122 §5: Try to get session and validate with app audience/origin binding
			sess, ok := sg(r)
			if ok && sess != nil && sessionStore != nil {
				// Validate app session with audience and origin binding
				requestOrigin := computeRequestOrigin(r, ep)
				sess, ok = sessionStore.ValidateAppSession(sess.ID, ep.App, requestOrigin)
			}

			if !ok || sess == nil {
				// RFC 20260122 §5.9: Only redirect safe methods (GET/HEAD) into OIDC flow.
				// Non-safe methods (POST/PUT/DELETE) would lose the request body on redirect.
				isSafeMethod := r.Method == http.MethodGet || r.Method == http.MethodHead
				if isSafeMethod && isBrowserNavigation(r) {
					if proxyOIDC != nil {
						proxyOIDC.InitiateOIDCFlow(w, r, ep.App, ep)
						return
					}
					// Fallback to portal redirect if proxy OIDC not configured
					origin := ""
					if portalOrigin != nil {
						origin = portalOrigin(r)
					}
					if origin == "" {
						scheme := "http"
						if shouldRewriteAsHTTPS(ep, r) {
							scheme = "https"
						}
						origin = scheme + "://" + normalizeHostNoPort(r.Host)
					}
					http.Redirect(w, r, portalLoginURL(origin, absoluteRequestURL(r, ep)), http.StatusFound)
					return
				}
				if cookiePresent {
					writeProxyJSONError(w, http.StatusUnauthorized, "session_expired", "SESSION_EXPIRED")
				} else {
					writeProxyJSONError(w, http.StatusUnauthorized, "authentication_required", "AUTH_REQUIRED")
				}
				return
			}

			// Check allowed_apps for standard users (admin has full access).
			if sess.Role != "admin" {
				if um == nil {
					http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
					return
				}
				allowed, err := um.IsAppAllowed(r.Context(), sess.UserID, ep.App)
				if err != nil || !allowed {
					if isBrowserNavigation(r) {
						origin := ""
						if portalOrigin != nil {
							origin = portalOrigin(r)
						}
						if origin == "" {
							scheme := "http"
							if shouldRewriteAsHTTPS(ep, r) {
								scheme = "https"
							}
							origin = scheme + "://" + normalizeHostNoPort(r.Host)
						}
						http.Redirect(w, r, portalAccessDeniedURL(origin, absoluteRequestURL(r, ep)), http.StatusFound)
						return
					}
					writeProxyJSONError(w, http.StatusForbidden, "app_access_denied", "APP_NOT_ALLOWED")
					return
				}
			}

			if strategy == "headers" {
				if um == nil {
					http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
					return
				}
				user, err := um.Get(r.Context(), sess.UserID)
				if err != nil {
					writeProxyJSONError(w, http.StatusUnauthorized, "session_expired", "SESSION_EXPIRED")
					return
				}
				// Inject trusted identity headers (RFC 4.1.6).
				r.Header.Set(HeaderPiccoloUser, user.Username)
				r.Header.Set(HeaderPiccoloEmail, user.Email)
				r.Header.Set(HeaderPiccoloName, user.Username)
				r.Header.Set(HeaderPiccoloRole, string(user.Role))
			}
		case "oidc_passthrough", "public":
			// Pass-through; app manages auth or requires none.
		default:
			// Unknown strategy should fail closed.
			writeProxyJSONError(w, http.StatusUnauthorized, "authentication_required", "AUTH_REQUIRED")
			return
		}

		// RFC 4.1.5 + 4.1.8: Strip Piccolo cookies before forwarding and optionally rewrite cookies
		// for LAN port-based isolation.
		rewriteCookies := shouldRewriteLegacyCookies(r.Host)
		partitionCookies := shouldPartitionCookies(r)
		needsMarker := needsEmbeddedMarker(r)
		stripAndRewriteRequestCookies(r, ep.App, rewriteCookies)
		r = withProxyContext(r, ep.App, normalizeHostNoPort(r.Host), rewriteCookies, partitionCookies, needsMarker)

		// Snapshot OIDC rewrite state for WAN authorize URL rewriting.
		if RequestArrivedViaTLS(r) {
			p.mu.Lock()
			issuerOriginFn := p.oidcIssuerOriginFn
			authPaths := p.oidcAuthorizePaths[ep.App]
			portalOriginFn := p.portalOrigin
			p.mu.Unlock()

			if issuerOriginFn != nil && portalOriginFn != nil {
				issuerOrigin := issuerOriginFn()
				portal := portalOriginFn(r)
				if issuerOrigin != "" && portal != "" {
					snap := &oidcRewriteSnapshot{
						issuerOrigin:   issuerOrigin,
						portalOrigin:   portal,
						authorizePaths: authPaths,
					}
					r = r.WithContext(context.WithValue(r.Context(), proxyOIDCRewriteContextKey{}, snap))
				}
			}
		}

		// forward_headers + gzip + reverse_proxy are the terminal of the chain,
		// composed into withForward above.
		withForward.ServeHTTP(w, r)
	}))

	// Wrap inline handler with extracted L7 middlewares (innermost-first; outer wraps inner).
	handler := l7.PathNormalize(l7.NormalizeAndSetRequestPath, writeProxyJSONError)(inlineHandler)
	handler = l7.ForwardedScrub()(handler)

	// Apply common middleware chain
	handler = p.securityHeaders(handler)
	handler = requestLogging(handler)
	handler = basicRateLimit(handler) // stub

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
		_ = srv.Serve(ln) // returns on ln.Close()
	}()
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
		if shouldRewriteAsHTTPS(ep, r) {
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

func shouldRewriteAsHTTPS(ep ServiceEndpoint, r *http.Request) bool {
	if ep.Flow != api.FlowTCP {
		return false
	}
	switch ep.Protocol {
	case api.ListenerProtocolHTTP, api.ListenerProtocolWebsocket:
		return RequestArrivedViaTLS(r)
	default:
		return false
	}
}

// RequestArrivedViaTLS reports whether the original client request was made
// over TLS, even if the current hop is plain HTTP. It checks direct TLS
// (r.TLS) and the connection hint issued by the portal's lanHostRoutingMiddleware.
func RequestArrivedViaTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if hint, ok := hintFromRequest(r); ok && hint.isTLS {
		return true
	}
	return false
}

// oidcAuthorizeRewriteMaxBody is the maximum response body size for Layer 2
// body rewriting. Responses larger than this are passed through unmodified.
const oidcAuthorizeRewriteMaxBody = 1 << 20 // 1 MB

// rewriteOIDCAuthorizeResponse performs dual-layer OIDC authorization URL
// rewriting for WAN requests. Layer 1 rewrites Location headers on 3xx
// responses. Layer 2 rewrites response bodies on declared authorize_paths.
func rewriteOIDCAuthorizeResponse(resp *http.Response, snap *oidcRewriteSnapshot, appName string) {
	oldURL := snap.issuerOrigin + "/oauth/authorize"
	newURL := snap.portalOrigin + "/oauth/authorize"

	// Layer 1: Location header rewrite on 3xx responses.
	// Match the exact authorize URL or one followed by '?' (query string) or '/'
	// (subpath) to avoid matching unrelated paths like .../authorize_token.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); loc != "" {
			if loc == oldURL || strings.HasPrefix(loc, oldURL+"?") || strings.HasPrefix(loc, oldURL+"/") || strings.HasPrefix(loc, oldURL+"#") {
				resp.Header.Set("Location", newURL+loc[len(oldURL):])
				log.Printf("DEBUG: OIDC rewrite L1: app=%s location rewritten", appName)
			}
		}
	}

	// Layer 2: Body text replacement on declared authorize_paths.
	if len(snap.authorizePaths) == 0 {
		return
	}
	reqPath := ""
	if resp.Request != nil && resp.Request.URL != nil {
		reqPath = resp.Request.URL.Path
	}
	pathMatch := false
	for _, ap := range snap.authorizePaths {
		if reqPath == ap {
			pathMatch = true
			break
		}
	}
	if !pathMatch {
		return
	}

	// Only rewrite text-based content types. Reuses the proxy's existing
	// compressible-types definition (mime.ParseMediaType-based, handles
	// charset suffixes correctly).
	if !isCompressibleContentType(resp.Header.Get("Content-Type")) {
		return
	}

	// Skip compressed responses — we cannot rewrite without decompressing.
	if resp.Header.Get("Content-Encoding") != "" {
		log.Printf("WARN: OIDC rewrite L2 skipped (Content-Encoding present): app=%s path=%s", appName, reqPath)
		return
	}

	// Pre-check Content-Length to avoid consuming oversized responses.
	if resp.ContentLength > oidcAuthorizeRewriteMaxBody {
		log.Printf("WARN: OIDC rewrite L2 skipped (Content-Length %d > %d): app=%s path=%s", resp.ContentLength, oidcAuthorizeRewriteMaxBody, appName, reqPath)
		return
	}

	// Read up to maxBody+1 bytes to detect oversize from chunked responses.
	body, err := io.ReadAll(io.LimitReader(resp.Body, oidcAuthorizeRewriteMaxBody+1))
	if err != nil {
		// Splice the read bytes back so the downstream client gets the full body.
		// Preserve the original Closer so resources are released correctly.
		log.Printf("WARN: OIDC rewrite L2 body read error: app=%s path=%s err=%v", appName, reqPath, err)
		resp.Body = &spliceCloser{
			Reader: io.MultiReader(bytes.NewReader(body), resp.Body),
			closer: resp.Body,
		}
		return
	}
	if int64(len(body)) > oidcAuthorizeRewriteMaxBody {
		// Body exceeded the limit. Splice back the consumed portion plus the
		// rest of the original body so nothing is truncated.
		log.Printf("WARN: OIDC rewrite L2 skipped (body too large: >%d bytes): app=%s path=%s", oidcAuthorizeRewriteMaxBody, appName, reqPath)
		resp.Body = &spliceCloser{
			Reader: io.MultiReader(bytes.NewReader(body), resp.Body),
			closer: resp.Body,
		}
		return
	}

	// Body fully read; safe to close the original.
	resp.Body.Close()

	// Empty body — nothing to do.
	if len(body) == 0 {
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		resp.ContentLength = 0
		resp.Header.Set("Content-Length", "0")
		return
	}

	// Replace the authorize URL pattern, matching only when followed by a
	// valid URL boundary to avoid false positives like `/oauth/authorize_token`.
	// This mirrors Layer 1's boundary check.
	newBody, count := replaceAuthorizeURL(body, []byte(oldURL), []byte(newURL))
	if count == 0 {
		log.Printf("WARN: OIDC rewrite L2 matched path but zero replacements: app=%s path=%s", appName, reqPath)
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return
	}
	resp.Body = io.NopCloser(bytes.NewReader(newBody))
	resp.ContentLength = int64(len(newBody))
	resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	resp.Header.Del("Transfer-Encoding")
	log.Printf("DEBUG: OIDC rewrite L2: app=%s path=%s replacements=%d oldSize=%d newSize=%d", appName, reqPath, count, len(body), len(newBody))
}

// replaceAuthorizeURL replaces `old` with `new` in `body`, but only when the
// match is followed by a valid URL boundary (end-of-buffer or one of the
// characters that legitimately terminate a URL in HTML/JSON/JS contexts).
// This prevents false positives like `/oauth/authorize_token`.
func replaceAuthorizeURL(body, old, new []byte) ([]byte, int) {
	if len(old) == 0 {
		return body, 0
	}
	var out bytes.Buffer
	out.Grow(len(body))
	count := 0
	i := 0
	for {
		j := bytes.Index(body[i:], old)
		if j < 0 {
			out.Write(body[i:])
			break
		}
		matchStart := i + j
		matchEnd := matchStart + len(old)
		if matchEnd == len(body) || isURLBoundary(body[matchEnd]) {
			out.Write(body[i:matchStart])
			out.Write(new)
			count++
			i = matchEnd
		} else {
			// Not a boundary match — keep original bytes up to and including the prefix
			// and advance past the first byte of the match to allow overlapping searches.
			out.Write(body[i : matchStart+1])
			i = matchStart + 1
		}
	}
	return out.Bytes(), count
}

// isURLBoundary reports whether b is a character that legitimately terminates
// a URL in HTML/JSON/JS contexts.
func isURLBoundary(b byte) bool {
	switch b {
	case '?', '#', '/', '"', '\'', '<', '>', ' ', '\t', '\n', '\r', ',', ';', ')', '}', ']', '\\':
		return true
	}
	return false
}

// spliceCloser combines a Reader (typically a MultiReader) with the original
// body's Closer so the downstream HTTP transport closes the underlying body.
type spliceCloser struct {
	io.Reader
	closer io.Closer
}

func (s *spliceCloser) Close() error { return s.closer.Close() }

// computeRequestOrigin computes the canonical origin (scheme://host[:port]) for a request.
// Used for session origin binding per RFC 20260122 §6.1.
// IPv6 addresses are preserved with brackets per RFC 3986.
func computeRequestOrigin(r *http.Request, ep ServiceEndpoint) string {
	scheme := "http"
	if shouldRewriteAsHTTPS(ep, r) || r.TLS != nil {
		scheme = "https"
	}

	// Use net.SplitHostPort for correct IPv6 handling
	host, portStr, err := net.SplitHostPort(r.Host)
	if err != nil {
		// No port in Host header
		host = r.Host
		portStr = ""
	}

	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return ""
	}

	// Strip existing brackets before re-adding to avoid double-bracketing (e.g., Host: [::1] without port)
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")

	// Preserve IPv6 brackets per RFC 3986
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}

	// Omit default ports
	if portStr != "" {
		port, parseErr := strconv.Atoi(portStr)
		if parseErr == nil {
			if (scheme == "http" && port != 80) || (scheme == "https" && port != 443) {
				return scheme + "://" + host + ":" + portStr
			}
		}
	}

	return scheme + "://" + host
}

// no extra helpers

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

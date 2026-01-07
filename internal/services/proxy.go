package services

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/auth"
)

var frameAncestorsRe = regexp.MustCompile(`(?i)frame-ancestors\s+[^;]+(; ?)?`)

type connectionHint struct {
	isTLS      bool
	remotePort int
}

type hintContextKey struct{}

// ProxyManager manages TCP listeners and proxies traffic based on ServiceEndpoint
type ProxyManager struct {
	mu                sync.Mutex
	listeners         map[int]net.Listener // by public port
	hints             map[int]map[int]connectionHint
	cspFrameAncestors string // pre-calculated CSP header value
	wg                sync.WaitGroup
	acme              http.Handler

	// Auth dependencies for trusted headers middleware (RFC 5.2)
	userManager   *auth.UserManager
	sessionGetter func(r *http.Request) (*auth.Session, bool)
}

func NewProxyManager() *ProxyManager {
	// Default safe CSP
	return &ProxyManager{
		listeners:         make(map[int]net.Listener),
		cspFrameAncestors: "frame-ancestors \"self\" http://localhost:* http://*.local:* https://*.local:*",
	}
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

// StartListener starts a TCP proxy for the given endpoint
func (p *ProxyManager) StartListener(ep ServiceEndpoint) {
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
	// Strip backend restrictions so we can apply our own
	rp.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del("X-Frame-Options")

		resp.Header.Set("Cross-Origin-Resource-Policy", "same-site")
		resp.Header.Set("Cross-Origin-Embedder-Policy", "require-corp")

		// Remove existing frame-ancestors directive if present, but keep other CSP directives
		if val := resp.Header.Get("Content-Security-Policy"); val != "" {
			newVal := frameAncestorsRe.ReplaceAllString(val, "")
			// If cleaning resulted in empty or just whitespace/semicolons, strictly we might want to keep it empty
			// But for now, just setting it back is fine.
			resp.Header.Set("Content-Security-Policy", newVal)
		}
		return nil
	}

	// Core handler that forwards to backend
	coreHandler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		applyForwardHeaders(r, ep)
		// Intercept ACME HTTP-01 challenges on HTTP proxies only
		if strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
			p.mu.Lock()
			acme := p.acme
			p.mu.Unlock()
			if acme != nil {
				acme.ServeHTTP(w, r)
				return
			}
		}
		rp.ServeHTTP(w, r)
	}))

	// Build middleware chain based on auth strategy
	var handler http.Handler
	if ep.AuthStrategy == "headers" {
		// RFC 5.2: Trusted Headers auth - validate session, check allowed_apps, inject headers
		p.mu.Lock()
		um := p.userManager
		sg := p.sessionGetter
		p.mu.Unlock()

		if um != nil && sg != nil {
			thm := NewTrustedHeadersMiddleware(TrustedHeadersConfig{
				UserManager:   um,
				SessionGetter: sg,
				AppIDResolver: func(r *http.Request) string { return ep.App },
			})
			handler = thm.Wrap(coreHandler)
			log.Printf("INFO: Trusted headers middleware enabled for app=%s listener=%s", ep.App, ep.Name)
		} else {
			// Auth config not set - log warning and fall back to stripping headers only
			log.Printf("WARN: Auth config not set for headers strategy app=%s; stripping headers only", ep.App)
			handler = stripPiccoloHeadersMiddleware(coreHandler)
		}
	} else {
		// For OIDC or no auth: just strip any spoofed X-Piccolo-* headers as a security baseline
		handler = stripPiccoloHeadersMiddleware(coreHandler)
	}

	// Apply common middleware chain
	handler = p.securityHeaders(handler)
	handler = requestLogging(handler)
	handler = basicRateLimit(handler) // stub

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		log.Printf("INFO: HTTP proxy %s → %s (app=%s listener=%s protocol=%s auth=%s)", ln.Addr().String(), target, ep.App, ep.Name, ep.Protocol.String(), ep.AuthStrategy)
		srv := &http.Server{
			Handler: handler,
			ConnContext: func(ctx context.Context, c net.Conn) context.Context {
				if addr, ok := c.RemoteAddr().(*net.TCPAddr); ok {
					if hint, ok := p.consumeHint(ep.PublicPort, addr.Port); ok {
						ctx = context.WithValue(ctx, hintContextKey{}, hint)
					}
				}
				return ctx
			},
		}
		_ = srv.Serve(ln) // returns on ln.Close()
	}()
}

// stripPiccoloHeadersMiddleware removes all X-Piccolo-* headers from incoming requests.
// This prevents clients from spoofing trusted headers regardless of auth strategy.
func stripPiccoloHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StripHeadersFromRequest(r)
		next.ServeHTTP(w, r)
	})
}

// Middleware stubs
func (p *ProxyManager) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-XSS-Protection", "1; mode=block")

		p.mu.Lock()
		csp := p.cspFrameAncestors
		p.mu.Unlock()

		remoteIp, _ := splitHostPortValue(r.RemoteAddr)
		ip := net.ParseIP(remoteIp)
		if ip != nil && (ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate()) {
			host, _ := splitHostPortValue(r.Host)
			host = host + ":*"
			csp += " https://" + host + " http://" + host
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
	return connectionHint{}, false
}

func applyForwardHeaders(r *http.Request, ep ServiceEndpoint) {
	host, hostPort := splitHostPortValue(r.Host)
	if host == "" {
		altHost, altPort := splitHostPortValue(r.URL.Host)
		host, hostPort = altHost, altPort
	}

	proto := resolveProto(r, ep)
	ensureHeader(r, "X-Forwarded-Proto", proto)
	if host != "" {
		forwardHost := host
		if hostPort != "" {
			forwardHost = net.JoinHostPort(host, hostPort)
		}
		ensureHeader(r, "X-Forwarded-Host", forwardHost)
		host = forwardHost
	}

	port := resolvePortHeader(r, proto, hostPort)
	ensureHeader(r, "X-Forwarded-Port", port)

	ip := ensureClientIPHeaders(r)
	appendForwardedHeader(r, proto, host, ip)

	if proto == "https" {
		r.URL.Scheme = "https"
	} else {
		r.URL.Scheme = "http"
	}
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

func resolveProto(r *http.Request, ep ServiceEndpoint) string {
	if v := strings.ToLower(r.Header.Get("X-Forwarded-Proto")); v != "" {
		return v
	}
	if shouldRewriteAsHTTPS(ep, r) {
		return "https"
	}
	return "http"
}

func shouldRewriteAsHTTPS(ep ServiceEndpoint, r *http.Request) bool {
	if ep.Flow != api.FlowTCP {
		return false
	}
	switch ep.Protocol {
	case api.ListenerProtocolHTTP, api.ListenerProtocolWebsocket:
		return requestArrivedViaTLS(r)
	default:
		return false
	}
}

func requestArrivedViaTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if hint, ok := hintFromRequest(r); ok && hint.isTLS {
		return true
	}
	return false
}

func resolvePortHeader(r *http.Request, proto, hostPort string) string {
	if v := r.Header.Get("X-Forwarded-Port"); v != "" {
		return v
	}
	if hint, ok := hintFromRequest(r); ok && hint.remotePort > 0 {
		return strconv.Itoa(hint.remotePort)
	}
	if hostPort != "" {
		return hostPort
	}
	if proto == "https" {
		return "443"
	}
	return "80"
}

func ensureClientIPHeaders(r *http.Request) string {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = host
	}
	if ip == "" {
		return ""
	}
	if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
		r.Header.Set("X-Forwarded-For", prior+", "+ip)
	} else {
		r.Header.Set("X-Forwarded-For", ip)
	}
	ensureHeader(r, "X-Real-Ip", ip)
	return ip
}

func appendForwardedHeader(r *http.Request, proto, host, ip string) {
	parts := []string{fmt.Sprintf("proto=%s", proto)}
	if host != "" {
		parts = append(parts, fmt.Sprintf("host=%s", strings.ToLower(host)))
	}
	if ip != "" {
		parts = append(parts, fmt.Sprintf("for=%s", ip))
	}
	value := strings.Join(parts, ";")
	if prior := r.Header.Get("Forwarded"); prior != "" {
		r.Header.Set("Forwarded", prior+", "+value)
	} else {
		r.Header.Set("Forwarded", value)
	}
}

func ensureHeader(r *http.Request, key, value string) {
	if value == "" {
		return
	}
	if r.Header.Get(key) == "" {
		r.Header.Set(key, value)
	}
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
	p.mu.Unlock()
	p.wg.Wait()
}

// StopPort stops a specific public listener if running
func (p *ProxyManager) StopPort(port int) {
	p.mu.Lock()
	if ln, ok := p.listeners[port]; ok {
		_ = ln.Close()
		delete(p.listeners, port)
	}
	delete(p.hints, port)
	p.mu.Unlock()
}

// small int→string helper without strconv to keep deps minimal
// no extra helpers

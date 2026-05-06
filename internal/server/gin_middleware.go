package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"piccolod/internal/api"
	"piccolod/internal/health"
	"piccolod/internal/hostname"
	"piccolod/internal/network/stun"
	"piccolod/internal/services"
)

// corsMiddleware adds CORS headers for web UI access
func (s *GinServer) corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Strict same-origin CORS policy with credentials allowed only for same-origin
		origin := c.GetHeader("Origin")
		reqHost := c.Request.Host // may include :port
		allow := false
		if origin != "" {
			// Compare origin host to request host
			// Origin format: scheme://host[:port]
			// Cheap parse: strip scheme and compare host:port suffix
			// Note: in reverse proxy deployments, host should be preserved for same-origin.
			if strings.HasPrefix(origin, "http://") || strings.HasPrefix(origin, "https://") {
				o := origin
				if i := strings.Index(o, "://"); i >= 0 {
					o = o[i+3:]
				}
				// o now 'host[:port]'
				if o == reqHost {
					allow = true
				} else if (strings.HasPrefix(o, "localhost") || strings.HasPrefix(o, "127.0.0.1")) && (strings.HasPrefix(reqHost, "localhost") || strings.HasPrefix(reqHost, "127.0.0.1")) {
					// Allow cross-port localhost for dev
					allow = true
				}
			}
		}
		if allow {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
			c.Header("Access-Control-Allow-Credentials", "true")
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization, X-Requested-With, X-CSRF-Token")

		// Handle preflight requests
		if c.Request.Method == "OPTIONS" {
			if allow {
				c.AbortWithStatus(http.StatusOK)
			} else {
				// Not same-origin: deny preflight
				c.AbortWithStatus(http.StatusForbidden)
			}
			return
		}

		c.Next()
	}
}

// securityHeadersMiddleware adds security headers
func (s *GinServer) securityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Security headers
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-XSS-Protection", "1; mode=block")
		if s != nil && s.isRemoteSecureRequest(c.Request) {
			c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")

		// COOP/COEP headers required for Flutter WASM / SharedArrayBuffer
		c.Header("Cross-Origin-Opener-Policy", "same-origin")
		c.Header("Cross-Origin-Embedder-Policy", "require-corp")

		// API identification
		c.Header("X-Powered-By", "Piccolo OS")
		c.Header("X-Service-Version", s.version)
		if s != nil && s.apiValidator != nil {
			c.Header("X-API-Validation", "enabled")
		} else {
			c.Header("X-API-Validation", "disabled")
		}

		c.Next()
	}
}

// rateLimitMiddleware provides basic rate limiting (placeholder for future enhancement)
func (s *GinServer) rateLimitMiddleware() gin.HandlerFunc {
	// This is a placeholder - in production, use gin-contrib/limiter
	return func(c *gin.Context) {
		// TODO: Implement rate limiting with redis or memory store
		// For now, just pass through
		c.Next()
	}
}

// authMiddleware provides authentication (placeholder for future enhancement)
func (s *GinServer) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// TODO: Implement JWT or session-based authentication
		// For now, just pass through

		// Example of how it would work:
		// token := c.GetHeader("Authorization")
		// if token == "" {
		//     c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization required"})
		//     c.Abort()
		//     return
		// }
		//
		// // Validate token...
		// c.Set("user_id", userID)
		c.Next()
	}
}

// requestLoggingMiddleware provides structured request logging
func (s *GinServer) requestLoggingMiddleware() gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		return fmt.Sprintf("[PICCOLO] %s - [%s] \"%s %s %s %d %s \"%s\" %s\"\n",
			param.ClientIP,
			param.TimeStamp.Format(time.RFC3339),
			param.Method,
			param.Path,
			param.Request.Proto,
			param.StatusCode,
			param.Latency,
			param.Request.UserAgent(),
			param.ErrorMessage,
		)
	})
}

// timezoneCaptureMiddleware does a one-shot capture of the browser's IANA
// timezone hint via the `X-Browser-Timezone` request header, calling
// `timedatectl set-timezone` once when the device is still on its UTC
// default. Lets piccolod's auto-unlock scheduler interpret 3-5AM windows in
// the operator's local time without ever surfacing a timezone setup screen.
//
// One-shot semantics: skips when timezoneMgr.IsSet() == true (operator or
// prior capture already configured), and serializes concurrent racers via a
// CAS-guarded "currently setting" flag. Non-blocking — set runs in a
// goroutine so the request never waits on `timedatectl`.
//
// Validation: malformed zones (rejected by time.LoadLocation) are dropped
// silently. The header is a hint, not a contract — bad input doesn't fail
// the request.
func (s *GinServer) timezoneCaptureMiddleware() gin.HandlerFunc {
	var setting atomic.Bool
	return func(c *gin.Context) {
		c.Next()
		if s.timezoneMgr == nil {
			return
		}
		zone := strings.TrimSpace(c.Request.Header.Get("X-Browser-Timezone"))
		if zone == "" {
			return
		}
		if s.timezoneMgr.IsSet() {
			return
		}
		if _, err := time.LoadLocation(zone); err != nil {
			return
		}
		if !setting.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer setting.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.timezoneMgr.Set(ctx, zone); err != nil {
				log.Printf("WARN: timezone capture from X-Browser-Timezone (%q): %v", zone, err)
				return
			}
			log.Printf("INFO: timezone captured from browser: %s", zone)
		}()
	}
}

// requireUnlocked blocks state-changing operations when the system is not in
// a Ready state. Composite readiness via lifecycle, not the narrow SDEK
// check — this closes the window where the SDEK is loaded but persistence
// is still loading and a request would observe ErrLocked from any DB query.
func (s *GinServer) requireUnlocked() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.lifecycle == nil || !s.lifecycle.IsReady() {
			c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// requireSession ensures a valid portal session cookie is present and not expired.
// RFC 20260122 §6.2: Validates audience="portal" and origin binding.
// Implements sliding sessions: extends TTL only when >25% of the lifetime has
// been consumed to avoid Set-Cookie overhead on every response.
func (s *GinServer) requireSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := s.getSession(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			c.Abort()
			return
		}
		origin := s.computeCanonicalOrigin(c)
		sess, ok := s.sessions.ValidatePortalSession(id, origin)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			c.Abort()
			return
		}

		// Sliding session: extend only when >25% of TTL consumed (threshold = 75%).
		// ExtendTTLIfNeeded performs the check and write atomically under one lock.
		if s.sessions.ExtendTTLIfNeeded(id, portalSessionTTL, 3*portalSessionTTL/4) {
			s.setSessionCookie(c, id, portalSessionCookieTTL)
		}

		// Enforce MustRegisterPasskey: block dashboard access until the user
		// registers a passkey. Allowlist covers setup-flow endpoints that run
		// between session creation and passkey registration, plus the inline
		// "remove legacy entry then register" flow (C6, D-10).
		//
		// Allowed under MustRegisterPasskey:
		//   - /api/v1/auth/passkey/register/*       — ceremony
		//   - /api/v1/auth/session, /logout, /csrf
		//   - GET /api/v1/auth/passkeys             — list, so the bootscreen can
		//                                              show what to delete
		//   - DELETE /api/v1/auth/passkeys/:id      — inline legacy removal
		//   - /api/v1/crypto/recovery-key/...      — admitted by prefix; today
		//     the only route under requireSession with this prefix is the
		//     mutator below, denied at the handler. The prefix admission is
		//     defensive: any future read-only recovery endpoint registered
		//     under requireSession is reachable for the bootstrap-screen view
		//     flow without re-touching this allowlist.
		//
		// Notably DENIED even though under /crypto/recovery-key/ prefix:
		//   - POST /api/v1/crypto/recovery-key/generate — state-mutating; the
		//     handler enforces this with an explicit 403 (C7) belt-and-
		//     suspenders against future allowlist regressions.
		if sess.MustRegisterPasskey.Load() {
			path := c.Request.URL.Path
			method := c.Request.Method
			allowed := false
			switch {
			case strings.HasPrefix(path, "/api/v1/auth/passkey/register/"):
				allowed = true
			case strings.HasPrefix(path, "/api/v1/auth/session"),
				strings.HasPrefix(path, "/api/v1/auth/logout"),
				strings.HasPrefix(path, "/api/v1/auth/csrf"):
				allowed = true
			case path == "/api/v1/auth/passkeys" && method == http.MethodGet:
				allowed = true
			case strings.HasPrefix(path, "/api/v1/auth/passkeys/") && method == http.MethodDelete:
				allowed = true
			case strings.HasPrefix(path, "/api/v1/crypto/recovery-key/"):
				// Read paths permitted; mutators denied at handler level (C7).
				allowed = true
			case path == "/api/v1/telemetry/log" && method == http.MethodPost:
				// Admit error telemetry: the forced-registration window is
				// precisely when passkey-related errors are most likely, and
				// blocking telemetry here means the operator loses visibility
				// into exactly the failures that matter. Telemetry is append-
				// only; it grants no additional capability to an attacker.
				allowed = true
			}
			if !allowed {
				c.JSON(http.StatusForbidden, gin.H{
					"error":   "passkey_registration_required",
					"message": "You must register a passkey before accessing the dashboard.",
				})
				c.Abort()
				return
			}
		}

		c.Next()
	}
}

// requireAdmin ensures the session user has admin role.
// Must be used after requireSession middleware.
func (s *GinServer) requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := s.getSession(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			c.Abort()
			return
		}
		sess, ok := s.sessions.Get(id)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			c.Abort()
			return
		}
		if sess.Role != "admin" {
			c.JSON(http.StatusForbidden, gin.H{"error": "admin role required"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// csrfMiddleware enforces X-CSRF-Token on state-changing requests when session exists
func (s *GinServer) csrfMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Only enforce on non-GET/HEAD/OPTIONS
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		// Telemetry POST is exempt: navigator.sendBeacon (the durable path
		// for end-of-life flushes) has no header-customization API and so
		// can't carry X-CSRF-Token. Telemetry is append-only journal
		// logging — it grants no additional capability to an attacker who
		// already has the session cookie, and it's already gated by
		// requireSession + requireAdmin upstream. The cookie alone is the
		// authentication boundary we care about here.
		if c.Request.Method == http.MethodPost && c.Request.URL.Path == "/api/v1/telemetry/log" {
			c.Next()
			return
		}
		id, ok := s.getSession(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			c.Abort()
			return
		}
		sess, ok := s.sessions.Get(id)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			c.Abort()
			return
		}
		token := c.GetHeader("X-CSRF-Token")
		if token == "" || token != sess.CSRF {
			c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// requireUnhealthy blocks requests unless the system is in error state.
// Used to gate the unauthenticated emergency diagnostic endpoint.
func (s *GinServer) requireUnhealthy() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.healthTracker == nil || s.healthTracker.Overall() < health.LevelError {
			c.JSON(http.StatusForbidden, gin.H{"error": "system is operational"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// isLANRequest returns true if the request comes from a non-loopback IP (LAN).
// Loopback implies Nexus remote proxy (dials 127.0.0.1) or local script.
func isLANRequest(c *gin.Context) bool {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && !ip.IsLoopback()
}

// This prevents remote access via Nexus (which proxies via localhost) while allowing LAN access.
func (s *GinServer) allowLANOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isLANRequest(c) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden: Remote/Loopback access denied"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// relayClientIP extracts the real client IP from a Nexus-relayed request.
// Returns empty string for direct LAN connections or when Nexus didn't provide one.
func relayClientIP(c *gin.Context) string {
	if ip, ok := c.Request.Context().Value(relayClientIPKeyInstance).(string); ok {
		return ip
	}
	return ""
}

// requireSetupState blocks requests if crypto is already initialized.
// Gates endpoints that should only be accessible during first-time setup.
func (s *GinServer) requireSetupState() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.cryptoManager != nil && s.cryptoManager.IsInitialized() {
			c.JSON(http.StatusForbidden, gin.H{"error": "setup already complete"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// allowLANOrSamePublicIP allows requests from LAN (non-loopback) IPs or from
// remote callers whose public IP matches the device's STUN-discovered public IP.
// This enables same-network access via Nexus relay for endpoints where the user
// needs physical proximity but may be accessing via the remote domain.
//
// Used by: WiFi management routes (/api/v1/wifi/*).
// Trust model: "same public IP" ≈ "same network". Note that CGNAT environments
// may share a public IP across unrelated users — accepted risk since admin auth
// is also required.
func (s *GinServer) allowLANOrSamePublicIP() gin.HandlerFunc {
	return func(c *gin.Context) {
		if isLANRequest(c) {
			c.Next()
			return
		}

		// Remote: match Nexus-relayed client IP against device public IP.
		// Uses cached value (refreshed every 5 min by background loop).
		// Falls back to a synchronous query if cache is empty (fresh boot).
		if s.stunService != nil {
			clientIP := relayClientIP(c)
			deviceIP := s.stunService.PublicIP()
			if deviceIP == "" {
				deviceIP = s.stunService.Refresh() // cold-cache fill, at most once
			}
			normClient := stun.NormalizeIP(clientIP)
			normDevice := stun.NormalizeIP(deviceIP)
			if clientIP != "" && deviceIP != "" && normClient == normDevice {
				c.Next()
				return
			}
			log.Printf("WARN: allowLANOrSamePublicIP denied %s %s: relayClientIP=%q (norm=%q) stunDeviceIP=%q (norm=%q)",
				c.Request.Method, c.Request.URL.Path, clientIP, normClient, deviceIP, normDevice)
		} else {
			log.Printf("WARN: allowLANOrSamePublicIP denied %s %s: stunService is nil", c.Request.Method, c.Request.URL.Path)
		}

		c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden: LAN or same network required"})
		c.Abort()
	}
}

// lanHostRoutingMiddleware implements LAN host-based routing per RFC 20260122.
// It routes requests to app 2-level hostnames to the appropriate backend.
//
// Routing logic:
// - piccolo.local / localhost / IP → continue to portal routes
// - <app>-piccolo.local → app primary listener (2-level format)
// - <listener>-<app>-piccolo.local → specific listener (2-level format)
// - Unknown -piccolo.local subdomain → 404
func (s *GinServer) lanHostRoutingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.serviceManager == nil {
			c.Next()
			return
		}

		// Get the LAN base hostname
		lanBase := "piccolo.local"
		if s.mdnsManager != nil {
			lanBase = s.mdnsManager.Hostname()
		}

		// Normalize the request host (strip port if present)
		reqHost := c.Request.Host
		if h, _, err := net.SplitHostPort(reqHost); err == nil {
			reqHost = h
		}
		reqHost = strings.TrimSuffix(strings.ToLower(reqHost), ".")

		// Portal hosts: base domain, localhost, or IP addresses
		// These continue to normal portal routing
		if reqHost == strings.ToLower(lanBase) ||
			reqHost == "localhost" ||
			reqHost == "127.0.0.1" ||
			net.ParseIP(reqHost) != nil {
			c.Next()
			return
		}

		// Check for app hostname: <label>-<base> (2-level format per RFC 20260122)
		baseSuffix := "-" + strings.ToLower(lanBase)
		if !strings.HasSuffix(reqHost, baseSuffix) {
			// Not a 2-level piccolo.local hostname, continue to portal
			c.Next()
			return
		}

		// Extract the host label (app name or listener-app)
		hostLabel := hostname.NormalizeHostLabel(reqHost, lanBase)
		if hostLabel == "" {
			// Apex domain, continue to portal
			c.Next()
			return
		}

		// Resolve the endpoint by host label
		// LAN host-based routing is independent of remote port publishing.
		// Pass 0 to ignore remote port filtering (RFC 20260114 §5.2).
		ep, ok := s.serviceManager.ResolveByHostLabel(hostLabel, 0)
		if !ok {
			// Unknown subdomain
			c.JSON(http.StatusNotFound, gin.H{
				"error": "no_route",
				"host":  reqHost,
			})
			c.Abort()
			return
		}

		// Only HTTP/Websocket-on-flow:tcp endpoints can be proxied via gin's
		// host-based router — everything else (flow:tls passthrough,
		// tcp+raw added in plan §D8) is reachable via the TLS mux SNI route
		// or port-based access. Per RFC 20260316 + plan §D9.
		if !api.LanHostBasedEligible(ep.Flow, ep.Protocol) {
			c.Next()
			return
		}

		// Proxy the request to the resolved endpoint
		s.proxyToEndpoint(c, ep)
		c.Abort()
	}
}

// proxyToEndpoint proxies the HTTP request to the backend service endpoint.
// Used for LAN host-based routing per RFC 20260114.
//
// Routes to the PublicPort listener which goes through the existing ProxyManager
// with full auth, cookie handling, and security features.
//
// Uses the LocalAddr (interface IP that received the request) to route, ensuring
// ProxyManager can distinguish LAN traffic from trusted loopback sources (Nexus).
func (s *GinServer) proxyToEndpoint(c *gin.Context, ep services.ServiceEndpoint) {
	// Extract the local interface IP that received this request.
	// This ensures ProxyManager sees a non-loopback LocalAddr for LAN traffic,
	// preventing header spoofing while still trusting Nexus traffic via loopback.
	localAddr, ok := c.Request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "internal_error",
			"message": "Could not determine local address",
		})
		return
	}
	targetHost, _, err := net.SplitHostPort(localAddr.String())
	if err != nil || targetHost == "" {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "internal_error",
			"message": "Could not parse local address",
		})
		return
	}

	target := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(targetHost, strconv.Itoa(ep.PublicPort)),
	}

	// Provide per-request, non-spoofable forwarding hints to ProxyManager without relying on
	// loopback trust. This preserves real LAN client IP while keeping forwarded headers
	// zero-trust by default (ProxyManager still overwrites X-Forwarded-* for untrusted hops).
	hintToken := ""
	if s != nil && s.serviceManager != nil {
		if pm := s.serviceManager.ProxyManager(); pm != nil {
			clientIP := ""
			if host, _, err := net.SplitHostPort(c.Request.RemoteAddr); err == nil {
				clientIP = host
			} else if ip := net.ParseIP(strings.Trim(c.Request.RemoteAddr, "[]")); ip != nil {
				clientIP = ip.String()
			}
			isTLS := s.isSecureRequest(c.Request)
			remotePort := 80
			if isTLS {
				remotePort = 443
			}
			if _, port, err := net.SplitHostPort(c.Request.Host); err == nil {
				if p, err := strconv.Atoi(port); err == nil && p > 0 {
					remotePort = p
				}
			}
			if tok, ok := pm.IssueRequestHint(ep.PublicPort, clientIP, isTLS, remotePort); ok {
				hintToken = tok
			}
		}
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = c.Request.Host // Preserve original Host for ProxyManager to use

		// Attach a one-time hint token so ProxyManager can recover the real client address
		// even when the portal is the TCP peer for the PublicPort hop.
		req.Header.Del(services.HeaderPiccoloHintToken)
		if hintToken != "" {
			req.Header.Set(services.HeaderPiccoloHintToken, hintToken)
		}
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   "backend_unavailable",
			"message": "The backend service is not responding",
		})
	}

	proxy.ServeHTTP(c.Writer, c.Request)
}

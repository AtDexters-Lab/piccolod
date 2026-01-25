package services

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"piccolod/internal/api"
)

// startEchoBackend starts a simple TCP echo server on 127.0.0.1:0 and returns its port and a shutdown func
func startEchoBackend(t *testing.T) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	stop := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-stop:
					return
				default:
					return
				}
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				w := bufio.NewWriter(c)
				for {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					if _, err := w.Write(line); err != nil {
						return
					}
					_ = w.Flush()
				}
			}(conn)
		}
	}()

	shutdown := func() {
		close(stop)
		_ = ln.Close()
	}
	return addr.Port, shutdown
}

func getFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func TestProxy_PassthroughTCP(t *testing.T) {
	hb, stop := startEchoBackend(t)
	defer stop()

	pm := NewProxyManager()
	public := getFreePort(t)
	ep := ServiceEndpoint{App: "test", Name: "echo", GuestPort: 0, HostBind: hb, PublicPort: public, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw}
	pm.StartListener(ep)
	defer pm.StopAll()

	// Give the proxy time to bind
	time.Sleep(100 * time.Millisecond)

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(public)))
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello\n")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	buf := make([]byte, len(msg))
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read failed: %v", err)
	}

	if string(buf) != string(msg) {
		t.Fatalf("unexpected echo: got %q want %q", string(buf), string(msg))
	}
}

func TestApplyForwardHeadersUsesTLSHint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://web.example.com", nil)
	req = req.WithContext(context.WithValue(req.Context(), hintContextKey{}, connectionHint{isTLS: true}))
	ep := ServiceEndpoint{Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP}

	applyForwardHeaders(req, ep)

	if got := req.Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Fatalf("expected X-Forwarded-Proto=https, got %q", got)
	}
	if req.URL.Scheme != "https" {
		t.Fatalf("expected request scheme https, got %s", req.URL.Scheme)
	}
}

func TestApplyForwardHeadersUsesClientIPHint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	req.Host = "example.test"
	req.RemoteAddr = "10.0.0.2:1234"
	req = req.WithContext(context.WithValue(req.Context(), hintContextKey{}, connectionHint{clientIP: "203.0.113.9"}))
	ep := ServiceEndpoint{Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP}

	applyForwardHeaders(req, ep)

	if got := req.Header.Get("X-Forwarded-For"); got != "203.0.113.9" {
		t.Fatalf("expected X-Forwarded-For=203.0.113.9, got %q", got)
	}
	if got := req.Header.Get("X-Real-IP"); got != "203.0.113.9" {
		t.Fatalf("expected X-Real-IP=203.0.113.9, got %q", got)
	}
	if fwd := req.Header.Get("Forwarded"); !strings.Contains(fwd, "for=203.0.113.9") {
		t.Fatalf("expected Forwarded to include for=203.0.113.9, got %q", fwd)
	}
}

func TestApplyForwardHeaders_ForwardedHeaderQuotesIPv6For(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	req.Host = "example.test"
	req.RemoteAddr = "[2001:db8::1]:1234"
	ep := ServiceEndpoint{Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP}

	applyForwardHeaders(req, ep)

	fwd := req.Header.Get("Forwarded")
	if !strings.Contains(fwd, `for="[2001:db8::1]"`) {
		t.Fatalf("expected Forwarded to include quoted IPv6 for, got %q", fwd)
	}
}

func TestApplyForwardHeaders_ForwardedHeaderQuotesHostWithPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:35000/", nil)
	req.Host = "127.0.0.1:35000"
	req.RemoteAddr = "192.0.2.1:1234"
	ep := ServiceEndpoint{Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP}

	applyForwardHeaders(req, ep)

	fwd := req.Header.Get("Forwarded")
	if !strings.Contains(fwd, `host="127.0.0.1:35000"`) {
		t.Fatalf("expected Forwarded to include quoted host with port, got %q", fwd)
	}
}

func getNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("interface addrs: %v", err)
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil {
			continue
		}
		ip = ip.To4()
		if ip == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		// Prefer private addresses; fall back to any non-loopback v4.
		if ip.IsPrivate() || ip.IsGlobalUnicast() {
			return ip.String()
		}
	}
	t.Skip("no non-loopback IPv4 address available")
	return ""
}

func TestProxyManagerRequestHintTokenSingleUse(t *testing.T) {
	pm := NewProxyManager()
	token, ok := pm.IssueRequestHint(12345, "203.0.113.10", true, 443)
	if !ok || token == "" {
		t.Fatalf("expected hint token issuance to succeed")
	}
	hint, ok := pm.consumeRequestHint(12345, token)
	if !ok {
		t.Fatalf("expected hint token to be consumable")
	}
	if hint.clientIP != "203.0.113.10" {
		t.Fatalf("expected hinted clientIP preserved, got %q", hint.clientIP)
	}
	if _, ok := pm.consumeRequestHint(12345, token); ok {
		t.Fatalf("expected hint token to be single-use")
	}
}

func TestHTTPProxyForwardHeadersRespectHintTokenClientIP(t *testing.T) {
	backendReqs := make(chan map[string]string, 2)

	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backendLn.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headers := map[string]string{
				"xff":       r.Header.Get("X-Forwarded-For"),
				"realip":    r.Header.Get("X-Real-IP"),
				"forwarded": r.Header.Get("Forwarded"),
				"token":     r.Header.Get(HeaderPiccoloHintToken),
			}
			select {
			case backendReqs <- headers:
			default:
			}
			w.WriteHeader(http.StatusOK)
		}),
	}
	go srv.Serve(backendLn)
	defer srv.Shutdown(context.Background())

	backendPort := backendLn.Addr().(*net.TCPAddr).Port

	pm := NewProxyManager()
	public := getFreePort(t)
	ep := ServiceEndpoint{
		App:        "test",
		Name:       "web",
		GuestPort:  0,
		HostBind:   backendPort,
		PublicPort: public,
		Flow:       api.FlowTCP,
		Protocol:   api.ListenerProtocolHTTP,
		Auth: &api.ListenerAuth{Rules: []api.ListenerAuthRule{{
			Path:     "/",
			Type:     "prefix",
			Strategy: "public",
		}}},
	}
	pm.StartListener(ep)
	defer pm.StopAll()
	time.Sleep(100 * time.Millisecond)

	hostIP := getNonLoopbackIPv4(t)
	target := fmt.Sprintf("http://%s:%d/", hostIP, public)

	token, ok := pm.IssueRequestHint(ep.PublicPort, "203.0.113.99", false, 80)
	if !ok || token == "" {
		t.Fatalf("expected hint token issuance to succeed")
	}
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	req.Header.Set(HeaderPiccoloHintToken, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("dial %s failed: %v", target, err)
	}
	resp.Body.Close()

	select {
	case headers := <-backendReqs:
		// ReverseProxy appends its own hop to X-Forwarded-For; the hinted client IP must be first.
		if !strings.HasPrefix(headers["xff"], "203.0.113.99") {
			t.Fatalf("expected X-Forwarded-For to start with hinted client IP, got %q", headers["xff"])
		}
		if headers["realip"] != "203.0.113.99" {
			t.Fatalf("expected X-Real-IP to use hinted client IP, got %q", headers["realip"])
		}
		if !strings.Contains(headers["forwarded"], "for=203.0.113.99") {
			t.Fatalf("expected Forwarded to include hinted for, got %q", headers["forwarded"])
		}
		if headers["token"] != "" {
			t.Fatalf("expected hint token header to not reach backend, got %q", headers["token"])
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for backend request (hint token)")
	}

	// Reusing the same token should have no effect (single-use consumption).
	req, _ = http.NewRequest(http.MethodGet, target, nil)
	req.Header.Set(HeaderPiccoloHintToken, token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("second dial %s failed: %v", target, err)
	}
	resp.Body.Close()

	select {
	case headers := <-backendReqs:
		if headers["xff"] == "203.0.113.99" {
			t.Fatalf("expected X-Forwarded-For to not reuse consumed hint token")
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for backend request (reused token)")
	}
}

func TestHTTPProxyForwardHeadersRespectTLSHints(t *testing.T) {
	backendReqs := make(chan map[string]string, 2)

	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backendLn.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headers := map[string]string{
				"proto":     r.Header.Get("X-Forwarded-Proto"),
				"host":      r.Header.Get("X-Forwarded-Host"),
				"forwarded": r.Header.Get("Forwarded"),
				"port":      r.Header.Get("X-Forwarded-Port"),
			}
			select {
			case backendReqs <- headers:
			default:
			}
			w.WriteHeader(http.StatusOK)
		}),
	}
	go srv.Serve(backendLn)
	defer srv.Shutdown(context.Background())

	backendPort := backendLn.Addr().(*net.TCPAddr).Port

	pm := NewProxyManager()
	public := getFreePort(t)
	ep := ServiceEndpoint{
		App:        "test",
		Name:       "web",
		GuestPort:  0,
		HostBind:   backendPort,
		PublicPort: public,
		Flow:       api.FlowTCP,
		Protocol:   api.ListenerProtocolHTTP,
		Auth: &api.ListenerAuth{Rules: []api.ListenerAuthRule{{
			Path:     "/",
			Type:     "prefix",
			Strategy: "public",
		}}},
	}
	pm.StartListener(ep)
	defer pm.StopAll()

	// Give proxy time to bind
	time.Sleep(100 * time.Millisecond)

	var (
		nextHint connectionHint
		hintMu   sync.Mutex
	)
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if tcpAddr, ok := conn.LocalAddr().(*net.TCPAddr); ok {
				hintMu.Lock()
				hint := nextHint
				hintMu.Unlock()
				pm.registerHint(ep.PublicPort, tcpAddr.Port, hint)
			}
			return conn, nil
		},
	}
	defer transport.CloseIdleConnections()
	client := http.Client{Timeout: 2 * time.Second, Transport: transport}
	target := fmt.Sprintf("http://127.0.0.1:%d/", public)

	// Plain HTTP request (no TLS hint)
	hintMu.Lock()
	nextHint = connectionHint{}
	hintMu.Unlock()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("plain http get: %v", err)
	}
	resp.Body.Close()

	select {
	case headers := <-backendReqs:
		if headers["proto"] != "http" {
			t.Fatalf("expected proto=http for plain request, got %q", headers["proto"])
		}
		if strings.Contains(headers["forwarded"], "proto=https") {
			t.Fatalf("unexpected forwarded proto=https for plain request: %q", headers["forwarded"])
		}
		expectedHost := fmt.Sprintf("127.0.0.1:%d", public)
		if headers["host"] != expectedHost {
			t.Fatalf("expected X-Forwarded-Host=%s for plain request, got %q", expectedHost, headers["host"])
		}
		if headers["port"] != strconv.Itoa(public) {
			t.Fatalf("expected X-Forwarded-Port=%d for plain request, got %q", public, headers["port"])
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for backend request (plain http)")
	}
	// Mark upcoming request as TLS-terminated at Piccolo and originating from remote port 8443
	hintMu.Lock()
	nextHint = connectionHint{isTLS: true, remotePort: 8443}
	hintMu.Unlock()

	resp, err = client.Get(target)
	if err != nil {
		t.Fatalf("tls hint get: %v", err)
	}
	resp.Body.Close()

	select {
	case headers := <-backendReqs:
		if headers["proto"] != "https" {
			t.Fatalf("expected proto=https when hint present, got %q", headers["proto"])
		}
		if !strings.Contains(strings.ToLower(headers["forwarded"]), "proto=https") {
			t.Fatalf("expected forwarded proto=https, got %q", headers["forwarded"])
		}
		expectedHost := fmt.Sprintf("127.0.0.1:%d", public)
		if headers["host"] != expectedHost {
			t.Fatalf("expected X-Forwarded-Host=%s when hint present, got %q", expectedHost, headers["host"])
		}
		if headers["port"] != "8443" {
			t.Fatalf("expected X-Forwarded-Port=8443 when hint present, got %q", headers["port"])
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for backend request (tls hint)")
	}
}

func TestProxy_SecurityHeaders(t *testing.T) {
	// 1. Start a backend that sets restrictive headers
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backendLn.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Frame-Options", "SAMEORIGIN")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
			w.WriteHeader(http.StatusOK)
		}),
	}
	go srv.Serve(backendLn)
	defer srv.Shutdown(context.Background())

	backendPort := backendLn.Addr().(*net.TCPAddr).Port

	// 2. Start Proxy
	pm := NewProxyManager()
	public := getFreePort(t)
	ep := ServiceEndpoint{
		App:        "test",
		Name:       "web",
		GuestPort:  0,
		HostBind:   backendPort,
		PublicPort: public,
		Flow:       api.FlowTCP,
		Protocol:   api.ListenerProtocolHTTP,
		Auth: &api.ListenerAuth{Rules: []api.ListenerAuthRule{{
			Path:     "/",
			Type:     "prefix",
			Strategy: "public",
		}}},
	}
	pm.StartListener(ep)
	defer pm.StopAll()
	time.Sleep(100 * time.Millisecond)

	target := fmt.Sprintf("http://127.0.0.1:%d/", public)

	// 3. Test Default (Allowed for localhost with wildcard ports, no 127.0.0.1)
	req, _ := http.NewRequest("GET", target, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("default get: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Frame-Options"); got != "" {
		t.Errorf("default: expected X-Frame-Options to be stripped, got %q", got)
	}

	csp := resp.Header.Get("Content-Security-Policy")
	// Check for expanded default list (without 127.0.0.1 and non-wildcards)
	requiredDefaults := []string{
		"http://localhost:*",
		"http://*.local:*", "https://*.local:*",
	}
	for _, d := range requiredDefaults {
		if !strings.Contains(csp, d) {
			t.Errorf("default: expected CSP to contain %q, got %q", d, csp)
		}
	}
	if strings.Contains(csp, "127.0.0.1") {
		t.Errorf("default: expected 127.0.0.1 to be removed from CSP, got %q", csp)
	}
	if strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("default: expected backend frame-ancestors 'none' to be removed, got %q", csp)
	}

	// 4. Test adding an allowed ancestor (only wildcard variant)
	pm.SetAllowedAncestors([]string{"portal.example.com"})

	req, _ = http.NewRequest("GET", target, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("allowed get: %v", err)
	}
	defer resp.Body.Close()

	csp = strings.Join(resp.Header.Values("Content-Security-Policy"), ", ")
	if !strings.Contains(csp, "https://portal.example.com:*") {
		t.Errorf("allowed: expected CSP to contain portal.example.com:*, got %q", csp)
	}
}

func TestHTTPProxy_RewriteHttpOnlySetCookiePreservesMultibyteValue(t *testing.T) {
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backendLn.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Set-Cookie", "session=✓; Path=/; HttpOnly")
			w.WriteHeader(http.StatusOK)
		}),
	}
	go srv.Serve(backendLn)
	defer srv.Shutdown(context.Background())

	backendPort := backendLn.Addr().(*net.TCPAddr).Port

	pm := NewProxyManager()
	public := getFreePort(t)
	ep := ServiceEndpoint{
		App:        "demo",
		Name:       "web",
		GuestPort:  0,
		HostBind:   backendPort,
		PublicPort: public,
		Flow:       api.FlowTCP,
		Protocol:   api.ListenerProtocolHTTP,
		Auth: &api.ListenerAuth{Rules: []api.ListenerAuthRule{{
			Path:     "/",
			Type:     "prefix",
			Strategy: "public",
		}}},
	}
	pm.StartListener(ep)
	defer pm.StopAll()

	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", public))
	if err != nil {
		t.Fatalf("http get: %v", err)
	}
	resp.Body.Close()

	cookies := resp.Header.Values("Set-Cookie")
	if len(cookies) != 1 {
		t.Fatalf("expected 1 Set-Cookie, got %v", cookies)
	}
	want := "__piccolo_demo_session=✓; Path=/; HttpOnly"
	if cookies[0] != want {
		t.Fatalf("unexpected Set-Cookie: got %q want %q", cookies[0], want)
	}
}

func TestProxy_ACMEChallengeBypassesAuth(t *testing.T) {
	// Setup: backend that should NOT be reached for ACME challenges
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backendLn.Close()

	backendHit := make(chan struct{}, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case backendHit <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusOK)
		}),
	}
	go srv.Serve(backendLn)
	defer srv.Shutdown(context.Background())

	backendPort := backendLn.Addr().(*net.TCPAddr).Port

	pm := NewProxyManager()

	// Register mock ACME handler that returns a known challenge response
	acmeChallengeToken := "test-challenge-token-12345"
	acmeChallengeResponse := "test-challenge-response.key-authorization"
	pm.SetAcmeHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, acmeChallengeToken) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(acmeChallengeResponse))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))

	public := getFreePort(t)
	ep := ServiceEndpoint{
		App:        "test",
		Name:       "web",
		GuestPort:  0,
		HostBind:   backendPort,
		PublicPort: public,
		Flow:       api.FlowTCP,
		Protocol:   api.ListenerProtocolHTTP,
		// Protected auth strategy: requires session, which ACME verifiers don't have
		Auth: &api.ListenerAuth{Rules: []api.ListenerAuthRule{{
			Path:     "/",
			Type:     "prefix",
			Strategy: "protected",
		}}},
	}
	pm.StartListener(ep)
	defer pm.StopAll()

	time.Sleep(100 * time.Millisecond)

	// Test 1: ACME challenge request should bypass auth and return 200
	challengeURL := fmt.Sprintf("http://127.0.0.1:%d/.well-known/acme-challenge/%s", public, acmeChallengeToken)
	resp, err := http.Get(challengeURL)
	if err != nil {
		t.Fatalf("ACME challenge request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected ACME challenge to return 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read ACME challenge response: %v", err)
	}
	if string(body) != acmeChallengeResponse {
		t.Fatalf("expected ACME challenge response %q, got %q", acmeChallengeResponse, string(body))
	}

	// Verify backend was NOT hit (ACME handler intercepted the request)
	select {
	case <-backendHit:
		t.Fatalf("backend was hit for ACME challenge - should have been intercepted")
	default:
		// Expected: backend not hit
	}

	// Test 2: Regular protected path should still require auth (return 401 or 503)
	// 503 is returned when sessionGetter is nil (auth infra not configured for test)
	regularURL := fmt.Sprintf("http://127.0.0.1:%d/api/data", public)
	resp2, err := http.Get(regularURL)
	if err != nil {
		t.Fatalf("regular request failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusUnauthorized && resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected protected path to return 401 or 503, got %d", resp2.StatusCode)
	}
}

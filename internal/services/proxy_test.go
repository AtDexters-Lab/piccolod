package services

import (
	"bufio"
	"context"
	"fmt"
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

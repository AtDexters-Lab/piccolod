package captive

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer() *Server {
	scanFn := func(forceRefresh bool) ([]ScanResult, error) {
		return []ScanResult{
			{SSID: "TestNet", Security: "wpa2", SignalDBm: -55, SignalTier: "good", FrequencyMHz: 5180, Band: "5GHz"},
			{SSID: "OpenNet", Security: "open", SignalDBm: -70, SignalTier: "weak", FrequencyMHz: 2437, Band: "2.4GHz"},
		}, nil
	}
	connectFn := func(ssid, passphrase string) {}
	s, _ := NewServer(scanFn, connectFn, func() {})
	return s
}

func TestServer_IndexPage(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	s.handleIndex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Piccolo Setup") {
		t.Fatal("page should contain 'Piccolo Setup'")
	}
	if !strings.Contains(body, s.keypair.PublicKeyBase64()) {
		t.Fatal("page should contain the server public key")
	}
}

func TestServer_IndexPage_Redirect(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/some/random/path", nil)
	w := httptest.NewRecorder()

	s.handleIndex(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (redirect for non-root path)", w.Code, http.StatusFound)
	}
}

func TestServer_CaptivePortalDetection_Android(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/generate_204", nil)
	w := httptest.NewRecorder()

	s.handleRedirect(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
}

func TestServer_CaptivePortalDetection_Apple(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/hotspot-detect.html", nil)
	w := httptest.NewRecorder()

	s.handleAppleDetect(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	// Apple expects a non-"Success" body to trigger the captive portal popup
	if strings.Contains(body, "Success") {
		t.Fatal("Apple detect page should not contain 'Success'")
	}
}

func TestServer_Scan(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/scan", nil)
	w := httptest.NewRecorder()

	s.handleScan(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("JSON decode: %v", err)
	}
	networks, ok := resp["networks"].([]interface{})
	if !ok {
		t.Fatal("response should have 'networks' array")
	}
	if len(networks) != 2 {
		t.Fatalf("networks count = %d, want 2", len(networks))
	}
}

func TestServer_Scan_MethodNotAllowed(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/scan", nil)
	w := httptest.NewRecorder()

	s.handleScan(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestServer_Connect_RateLimiting(t *testing.T) {
	s := newTestServer()

	// Create a valid-looking connect request (will fail decryption, but rate limiter fires first)
	body := `{"ssid":"Test","client_public_key":"AAAA","nonce":"BBBB","encrypted_passphrase":"CCCC"}`

	// Exhaust rate limit (5 attempts)
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/connect", strings.NewReader(body))
		req.RemoteAddr = "192.168.1.100:12345"
		w := httptest.NewRecorder()
		s.handleConnect(w, req)
		// These will fail with 400 (bad base64) but still count toward rate limit
	}

	// 6th attempt should be rate-limited
	req := httptest.NewRequest(http.MethodPost, "/api/connect", strings.NewReader(body))
	req.RemoteAddr = "192.168.1.100:54321"
	w := httptest.NewRecorder()
	s.handleConnect(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d (rate limit)", w.Code, http.StatusTooManyRequests)
	}
}

func TestServer_Connect_DifferentIPsNotRateLimited(t *testing.T) {
	s := newTestServer()
	body := `{"ssid":"Test","client_public_key":"AAAA","nonce":"BBBB","encrypted_passphrase":"CCCC"}`

	// 5 attempts from one IP
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/connect", strings.NewReader(body))
		req.RemoteAddr = "192.168.1.100:12345"
		w := httptest.NewRecorder()
		s.handleConnect(w, req)
	}

	// Different IP should not be rate-limited
	req := httptest.NewRequest(http.MethodPost, "/api/connect", strings.NewReader(body))
	req.RemoteAddr = "192.168.1.200:12345"
	w := httptest.NewRecorder()
	s.handleConnect(w, req)

	if w.Code == http.StatusTooManyRequests {
		t.Fatal("different IP should not be rate-limited")
	}
}

func TestServer_Connect_MissingFields(t *testing.T) {
	s := newTestServer()

	tests := []struct {
		name string
		body string
	}{
		{"missing ssid", `{"client_public_key":"A","nonce":"B","encrypted_passphrase":"C"}`},
		{"missing key", `{"ssid":"X","nonce":"B","encrypted_passphrase":"C"}`},
		{"missing nonce", `{"ssid":"X","client_public_key":"A","encrypted_passphrase":"C"}`},
		{"missing passphrase", `{"ssid":"X","client_public_key":"A","nonce":"B"}`},
		{"empty body", `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/connect", strings.NewReader(tt.body))
			req.RemoteAddr = "10.42.0.10:9999"
			w := httptest.NewRecorder()
			s.handleConnect(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestServer_ConnectError_ShownOnNextIndex(t *testing.T) {
	s := newTestServer()
	s.SetConnectError("Authentication failed")

	// First request should show the error
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	w1 := httptest.NewRecorder()
	s.handleIndex(w1, req1)

	if !strings.Contains(w1.Body.String(), "Authentication failed") {
		t.Fatal("first index load should show the connection error")
	}

	// Second request should not (error shown once)
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	w2 := httptest.NewRecorder()
	s.handleIndex(w2, req2)

	if strings.Contains(w2.Body.String(), "Authentication failed") {
		t.Fatal("second index load should not show the error (shown once)")
	}
}

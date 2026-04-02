package captive

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ScanFunc is called by the portal to scan for WiFi networks.
type ScanFunc func(forceRefresh bool) ([]ScanResult, error)

// ConnectFunc is called by the portal to initiate a WiFi connection.
// It is called asynchronously after the HTTP response is sent.
type ConnectFunc func(ssid, passphrase string)

// ScanResult mirrors the network-level ScanResult for the portal API.
type ScanResult struct {
	SSID         string `json:"ssid"`
	Security     string `json:"security"`
	SignalDBm    int    `json:"signal_dbm"`
	SignalTier   string `json:"signal_tier"`
	FrequencyMHz uint32 `json:"frequency_mhz"`
	Band         string `json:"band"`
}

// Server is the captive portal HTTP server. It runs on the AP interface IP
// on a high port (e.g., 8080), with firewalld NAT redirecting port 80 to it.
type Server struct {
	httpSrv    *http.Server
	keypair    *ECDHKeypair
	scanFn     ScanFunc
	connectFn  ConnectFunc
	onActivity func() // called on each HTTP request to update activity timestamp

	mu         sync.Mutex
	connectErr string // error from last connection attempt, shown after AP reactivation
	limiter    *rateLimiter
}

// NewServer creates a captive portal server.
func NewServer(scanFn ScanFunc, connectFn ConnectFunc, onActivity func()) (*Server, error) {
	kp, err := NewECDHKeypair()
	if err != nil {
		return nil, err
	}

	return &Server{
		keypair:    kp,
		scanFn:     scanFn,
		connectFn:  connectFn,
		onActivity: onActivity,
		limiter:    newRateLimiter(5, time.Minute),
	}, nil
}

// Start begins serving on the given address (e.g., "10.42.0.1:8080").
func (s *Server) Start(ctx context.Context, listenAddr string) error {
	mux := http.NewServeMux()

	// Portal page
	mux.HandleFunc("/", s.handleIndex)

	// Captive portal detection probes
	mux.HandleFunc("/generate_204", s.handleRedirect)        // Android
	mux.HandleFunc("/hotspot-detect.html", s.handleAppleDetect) // Apple
	mux.HandleFunc("/connecttest.txt", s.handleRedirect)     // Windows
	mux.HandleFunc("/canonical.html", s.handleRedirect)      // Firefox

	// API endpoints
	mux.HandleFunc("/api/scan", s.handleScan)
	mux.HandleFunc("/api/connect", s.handleConnect)

	s.httpSrv = &http.Server{
		Addr:    listenAddr,
		Handler: mux,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 20 * time.Second, // scan can take up to 10s + D-Bus overhead
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("captive: listen %s: %w", listenAddr, err)
	}

	go func() {
		log.Printf("INFO: captive: portal server listening on %s", listenAddr)
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("ERROR: captive: server error: %v", err)
		}
	}()

	return nil
}

// Stop gracefully shuts down the server and zeros the keypair.
func (s *Server) Stop() {
	if s.httpSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.httpSrv.Shutdown(ctx)
	}
	if s.keypair != nil {
		s.keypair.Zero()
	}
}

// SetConnectError sets the error message shown after AP reactivation
// following a failed connection attempt.
func (s *Server) SetConnectError(msg string) {
	s.mu.Lock()
	s.connectErr = msg
	s.mu.Unlock()
}

// handleIndex serves the single-page captive portal HTML with the server's
// public key and any error message injected via template.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if s.onActivity != nil {
		s.onActivity()
	}

	// If the request isn't for "/" exactly, redirect (catch-all for DNS redirect)
	if r.URL.Path != "/" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	s.mu.Lock()
	errMsg := s.connectErr
	s.connectErr = "" // show once
	s.mu.Unlock()

	data := portalTemplateData{
		ErrorMessage: errMsg,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	// Two-pass rendering:
	// 1. html/template escapes ErrorMessage (prevents XSS via malicious SSIDs)
	// 2. strings.Replace injects the server public key (base64, server-controlled,
	//    safe — html/template's JS context escaper mangles + and / in base64)
	var buf strings.Builder
	if err := portalTmpl.Execute(&buf, data); err != nil {
		log.Printf("WARN: captive: template execute: %v", err)
		return
	}
	html := strings.Replace(buf.String(), "PUBKEY_PLACEHOLDER", s.keypair.PublicKeyBase64(), 1)
	fmt.Fprint(w, html)
}

// handleRedirect sends a 302 to "/" for captive portal detection probes.
func (s *Server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	if s.onActivity != nil {
		s.onActivity()
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleAppleDetect returns a non-"Success" HTML page to trigger Apple's
// captive portal popup.
func (s *Server) handleAppleDetect(w http.ResponseWriter, r *http.Request) {
	if s.onActivity != nil {
		s.onActivity()
	}
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "<HTML><HEAD><TITLE>Piccolo Setup</TITLE></HEAD><BODY>Redirecting...</BODY></HTML>")
}

// handleScan triggers a WiFi scan and returns JSON results.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if s.onActivity != nil {
		s.onActivity()
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	results, err := s.scanFn(true) // forceRefresh for captive portal
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"networks": []ScanResult{},
			"error":    err.Error(),
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"networks": results,
	})
}

// handleConnect accepts encrypted WiFi credentials and initiates a connection.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if s.onActivity != nil {
		s.onActivity()
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Rate limiting — record every attempt, not just failures
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !s.limiter.allow(clientIP) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "too_many_attempts",
			"message": "Too many attempts. Wait 1 minute.",
		})
		return
	}
	s.limiter.record(clientIP) // count all submissions, not just failures

	// Limit request body size (defense-in-depth on resource-constrained device)
	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req struct {
		SSID                string `json:"ssid"`
		ClientPublicKey     string `json:"client_public_key"`
		Nonce               string `json:"nonce"`
		EncryptedPassphrase string `json:"encrypted_passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}

	if req.SSID == "" || req.ClientPublicKey == "" || req.Nonce == "" || req.EncryptedPassphrase == "" {
		http.Error(w, `{"error":"missing_fields"}`, http.StatusBadRequest)
		return
	}

	// Decrypt passphrase
	passphrase, err := decryptConnectRequest(s.keypair, req.ClientPublicKey, req.Nonce, req.EncryptedPassphrase)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "decryption_failed"})
		return
	}

	// Return interstitial before AP teardown
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "connecting",
		"message": fmt.Sprintf("Connecting to %s… This may take up to 30 seconds.", req.SSID),
	})

	// Async: the AP will tear down after this response is sent
	go s.connectFn(req.SSID, string(passphrase))
}

// portalTemplateData is injected into the portal HTML template.
// ErrorMessage is auto-escaped by html/template (prevents XSS via malicious SSIDs).
type portalTemplateData struct {
	ErrorMessage string
}

// portalTmpl is parsed once from the embedded HTML.
var portalTmpl *template.Template

func init() {
	portalTmpl = template.Must(template.New("portal").Parse(portalHTML))
}

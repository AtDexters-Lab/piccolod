package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"piccolod/internal/api"
)

func TestTunnelCertificateIssueRequiresMTLSTarget(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)
	srv.remoteResolver.SetRemoteBases("self-hosted", []remoteBase{
		{source: "self-hosted", portalHost: "portal.example.com", domain: "portal.example.com"},
	})

	listeners := []api.AppListener{
		{Name: "web", GuestPort: 8080, Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTCP, Primary: true},
		{
			Name:      "ssh",
			GuestPort: 22,
			Protocol:  api.ListenerProtocolRaw,
			Flow:      api.FlowTCP,
			TLSWrap:   true,
			ConnectionAuth: &api.ConnectionAuth{MTLS: &api.ConnectionAuthMTLS{
				Verifier: api.ConnectionAuthMTLSVerifier{Type: "piccolo_session"},
			}},
		},
	}
	if _, err := srv.serviceManager.AllocateForApp("demo", listeners); err != nil {
		t.Fatalf("allocate: %v", err)
	}

	body := tunnelCertIssueBody(t, "ssh-demo.portal.example.com")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tunnels/certificates", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("issue status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		CertificatePEM string `json:"certificate_pem"`
		Serial         string `json:"serial"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.CertificatePEM == "" || resp.Serial == "" {
		t.Fatalf("expected cert and serial, got %#v", resp)
	}

	body = tunnelCertIssueBody(t, "web-demo.portal.example.com")
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tunnels/certificates", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-mTLS target status=%d body=%s", w.Code, w.Body.String())
	}
}

func tunnelCertIssueBody(t *testing.T, host string) []byte {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	req := tunnelCertificateRequest{
		Host:                host,
		RemotePort:          443,
		PublicKeyPEM:        string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
		RequestedTTLSeconds: 3600,
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return body
}

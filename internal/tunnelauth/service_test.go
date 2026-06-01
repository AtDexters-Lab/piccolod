package tunnelauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/services"
)

func TestIssueAndVerifyTunnelClient(t *testing.T) {
	pub, pubPEM := testEd25519PublicKeyPEM(t)
	svc := New(t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)

	issued, err := svc.Issue(context.Background(), IssueRequest{
		Host:         "ssh-demo.example.com",
		RemotePort:   443,
		App:          "demo",
		Listener:     "ssh",
		UserID:       "user-1",
		Username:     "admin",
		Role:         "admin",
		PublicKeyPEM: pubPEM,
		TTL:          time.Hour,
		Now:          now,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issued.Serial == "" || issued.CertificatePEM == "" {
		t.Fatalf("expected issued serial and cert, got %#v", issued)
	}
	cert := parseIssuedCert(t, issued.CertificatePEM)
	if !cert.PublicKey.(ed25519.PublicKey).Equal(pub) {
		t.Fatalf("issued cert public key mismatch")
	}

	result, err := svc.VerifyTunnelClient(context.Background(), services.TunnelClientVerification{
		PeerCertificates: []*x509.Certificate{cert},
		Host:             "ssh-demo.example.com",
		RemotePort:       443,
		App:              "demo",
		Listener:         "ssh",
		ClientIP:         "203.0.113.10",
		ConnectionAuth:   mtlsAuth(),
		Now:              now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("VerifyTunnelClient: %v", err)
	}
	if result.UserID != "user-1" || result.Serial != issued.Serial {
		t.Fatalf("unexpected verification result: %#v", result)
	}

	_, err = svc.VerifyTunnelClient(context.Background(), services.TunnelClientVerification{
		PeerCertificates: []*x509.Certificate{cert},
		Host:             "other.example.com",
		RemotePort:       443,
		App:              "demo",
		Listener:         "ssh",
		ClientIP:         "203.0.113.10",
		ConnectionAuth:   mtlsAuth(),
		Now:              now.Add(time.Minute),
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong host should be denied, got %v", err)
	}
	var verifyErr *services.TunnelClientVerificationError
	if !errors.As(err, &verifyErr) {
		t.Fatalf("wrong host error type = %T, want TunnelClientVerificationError", err)
	}
	if verifyErr.Reason != services.TunnelAuthReasonAudienceMismatch || verifyErr.Identity.UserID != "user-1" || verifyErr.Identity.Serial != issued.Serial {
		t.Fatalf("unexpected wrong-host verification error: %+v", verifyErr)
	}

	_, err = svc.VerifyTunnelClient(context.Background(), services.TunnelClientVerification{
		PeerCertificates: []*x509.Certificate{cert},
		Host:             "ssh-demo.example.com",
		RemotePort:       8443,
		App:              "demo",
		Listener:         "ssh",
		ClientIP:         "203.0.113.10",
		ConnectionAuth:   mtlsAuth(),
		Now:              now.Add(time.Minute),
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong remote port should be denied, got %v", err)
	}
}

func TestVerifyTunnelClientAppliesIPRules(t *testing.T) {
	_, pubPEM := testEd25519PublicKeyPEM(t)
	svc := New(t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	issued, err := svc.Issue(context.Background(), IssueRequest{
		Host:         "ssh-demo.example.com",
		RemotePort:   443,
		App:          "demo",
		Listener:     "ssh",
		UserID:       "user-1",
		Username:     "admin",
		Role:         "admin",
		PublicKeyPEM: pubPEM,
		TTL:          time.Hour,
		Now:          now,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	cert := parseIssuedCert(t, issued.CertificatePEM)
	auth := mtlsAuth()
	auth.Default = "deny"
	auth.Rules = []api.ConnectionAuthRule{{Match: "203.0.113.0/24", Strategy: "allow"}}

	_, err = svc.VerifyTunnelClient(context.Background(), services.TunnelClientVerification{
		PeerCertificates: []*x509.Certificate{cert},
		Host:             "ssh-demo.example.com",
		RemotePort:       443,
		App:              "demo",
		Listener:         "ssh",
		ClientIP:         "198.51.100.10",
		ConnectionAuth:   auth,
		Now:              now.Add(time.Minute),
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("source outside allowlist should be denied, got %v", err)
	}
}

func TestIssueRejectsUnsupportedPublicKey(t *testing.T) {
	svc := New(t.TempDir())
	_, err := svc.Issue(context.Background(), IssueRequest{
		Host:         "ssh-demo.example.com",
		RemotePort:   443,
		App:          "demo",
		Listener:     "ssh",
		UserID:       "user-1",
		Username:     "admin",
		Role:         "admin",
		PublicKeyPEM: "not pem",
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected invalid request, got %v", err)
	}
}

func mtlsAuth() *api.ConnectionAuth {
	return &api.ConnectionAuth{MTLS: &api.ConnectionAuthMTLS{
		Verifier: api.ConnectionAuthMTLSVerifier{Type: "piccolo_session"},
	}}
}

func testEd25519PublicKeyPEM(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return pub, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func parseIssuedCert(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatalf("missing certificate PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert
}

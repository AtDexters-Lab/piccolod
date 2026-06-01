package services_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/services"
	"piccolod/internal/tunnelauth"
)

func TestTLSMuxRequiresPiccoloSessionClientCert(t *testing.T) {
	svcMgr := services.NewServiceManager()
	defer svcMgr.Stop()
	defer svcMgr.ProxyManager().StopAll()

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
	eps, err := svcMgr.AllocateForApp("demo", listeners)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	var sshEP services.ServiceEndpoint
	for _, ep := range eps {
		if ep.Name == "ssh" {
			sshEP = ep
		}
	}
	if sshEP.HostBind == 0 {
		t.Fatalf("missing ssh endpoint")
	}
	backend := startOneShotTCPBackend(t, sshEP.HostBind)

	tunnelAuth := tunnelauth.New(t.TempDir())
	clientCert := issueClientTLSCert(t, tunnelAuth, "ssh-demo.example.com", "demo", "ssh")

	mux := services.NewTlsMux(svcMgr)
	mux.SetCertProvider(&staticTLSCertProvider{cert: mustServerCert(t, "ssh-demo.example.com")})
	mux.SetTunnelClientVerifier(tunnelAuth)
	decisions := make(chan services.TunnelAuthDecision, 4)
	mux.SetTunnelAuthDecisionRecorder(func(decision services.TunnelAuthDecision) {
		decisions <- decision
	})
	mux.UpdateConfig("portal.example.com", "example.com", 0)
	port, err := mux.Start()
	if err != nil {
		t.Fatalf("start mux: %v", err)
	}
	defer mux.Stop()

	if conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{
		ServerName:         "ssh-demo.example.com",
		InsecureSkipVerify: true,
	}); err == nil {
		_, _ = conn.Write([]byte("ping"))
		buf := make([]byte, 1)
		if _, readErr := conn.Read(buf); readErr == nil {
			t.Fatalf("expected connection without client cert to fail")
		}
		_ = conn.Close()
	}
	deny := waitTunnelAuthDecision(t, decisions)
	if deny.Allowed {
		t.Fatalf("expected denied decision without client cert, got %+v", deny)
	}
	if deny.App != "demo" || deny.Listener != "ssh" || deny.VerifierType != "piccolo_session" || deny.DenyReason != services.TunnelAuthReasonMissingClientCertificate {
		t.Fatalf("unexpected deny decision: %+v", deny)
	}

	conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{
		ServerName:         "ssh-demo.example.com",
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{clientCert},
	})
	if err != nil {
		t.Fatalf("dial with client cert: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(buf, []byte("pong")) {
		t.Fatalf("unexpected backend response %q", string(buf))
	}
	allow := waitTunnelAuthDecision(t, decisions)
	if !allow.Allowed {
		t.Fatalf("expected allowed decision with client cert, got %+v", allow)
	}
	if allow.App != "demo" || allow.Listener != "ssh" || allow.UserID != "user-1" || allow.Serial == "" || allow.VerifierType != "piccolo_session" {
		t.Fatalf("unexpected allow decision: %+v", allow)
	}
	snap := mux.TunnelAuthMetricsSnapshot()
	allowSample := services.TunnelAuthMetricsSample{App: "demo", Listener: "ssh", SourceIP: "127.0.0.1", VerifierType: "piccolo_session"}
	if got := snap.Allowed[allowSample]; got != 1 {
		t.Fatalf("allowed metrics = %d, want 1 (snapshot=%+v)", got, snap.Allowed)
	}
	denySample := services.TunnelAuthMetricsSample{App: "demo", Listener: "ssh", SourceIP: "127.0.0.1", VerifierType: "piccolo_session", DenyReason: services.TunnelAuthReasonMissingClientCertificate}
	if got := snap.Denied[denySample]; got != 1 {
		t.Fatalf("denied metrics = %d, want 1 (snapshot=%+v)", got, snap.Denied)
	}
	<-backend
}

func waitTunnelAuthDecision(t *testing.T, decisions <-chan services.TunnelAuthDecision) services.TunnelAuthDecision {
	t.Helper()
	select {
	case decision := <-decisions:
		return decision
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for tunnel auth decision")
		return services.TunnelAuthDecision{}
	}
}

type staticTLSCertProvider struct {
	cert *tls.Certificate
}

func (p *staticTLSCertProvider) GetCertificate(string) (*tls.Certificate, error) {
	return p.cert, nil
}

func startOneShotTCPBackend(t *testing.T, port int) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4)
		if _, err := conn.Read(buf); err != nil {
			return
		}
		_, _ = conn.Write([]byte("pong"))
	}()
	return done
}

func issueClientTLSCert(t *testing.T, svc *tunnelauth.Service, host, app, listener string) tls.Certificate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal client public key: %v", err)
	}
	issued, err := svc.Issue(t.Context(), tunnelauth.IssueRequest{
		Host:         host,
		RemotePort:   443,
		App:          app,
		Listener:     listener,
		UserID:       "user-1",
		Username:     "admin",
		Role:         "admin",
		PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
	})
	if err != nil {
		t.Fatalf("issue client cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal client private key: %v", err)
	}
	cert, err := tls.X509KeyPair([]byte(issued.CertificatePEM), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatalf("client keypair: %v", err)
	}
	return cert
}

func mustServerCert(t *testing.T, host string) *tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		t.Fatalf("server serial: %v", err)
	}
	now := time.Now().Add(-time.Minute)
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             now,
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create server cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatalf("server keypair: %v", err)
	}
	return &cert
}

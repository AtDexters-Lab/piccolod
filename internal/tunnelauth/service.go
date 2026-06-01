package tunnelauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/fsutil"
	"piccolod/internal/services"
)

const (
	DefaultTTL = time.Hour
	MaxTTL     = 4 * time.Hour
	MinTTL     = time.Minute

	caCertFile     = "client-ca.crt"
	caKeyFile      = "client-ca.key"
	ledgerFileName = "issued-certificates.json"
	ledgerVersion  = 1
	ledgerGrace    = 15 * time.Minute
)

var (
	ErrInvalidRequest = errors.New("tunnel auth: invalid request")
	ErrUnauthorized   = errors.New("tunnel auth: unauthorized")
	ErrUnavailable    = errors.New("tunnel auth: unavailable")
	ErrDenied         = errors.New("tunnel auth: denied")
)

// Service owns the Piccolo tunnel-client CA and issued certificate ledger.
// Its store directory must live in protected control-plane state.
type Service struct {
	mu sync.Mutex

	dir    string
	loaded bool
	caCert *x509.Certificate
	caKey  crypto.Signer
	ledger map[string]Record
}

type IssueRequest struct {
	Host         string
	RemotePort   int
	App          string
	Listener     string
	UserID       string
	Username     string
	Role         string
	PublicKeyPEM string
	TTL          time.Duration
	Now          time.Time
}

type IssueResponse struct {
	CertificatePEM           string    `json:"certificate_pem"`
	Serial                   string    `json:"serial"`
	NotAfter                 time.Time `json:"not_after"`
	MaxTunnelLifetimeSeconds int64     `json:"max_tunnel_lifetime_seconds"`
}

type ledgerFile struct {
	Version int               `json:"version"`
	Records map[string]Record `json:"records"`
}

type Record struct {
	Serial     string    `json:"serial"`
	UserID     string    `json:"user_id"`
	Username   string    `json:"username,omitempty"`
	Role       string    `json:"role"`
	Host       string    `json:"host"`
	RemotePort int       `json:"remote_port"`
	App        string    `json:"app"`
	Listener   string    `json:"listener"`
	NotBefore  time.Time `json:"not_before"`
	NotAfter   time.Time `json:"not_after"`
	CreatedAt  time.Time `json:"created_at"`
}

func New(dir string) *Service {
	return &Service{dir: dir}
}

func (s *Service) Issue(ctx context.Context, req IssueRequest) (IssueResponse, error) {
	if err := ctx.Err(); err != nil {
		return IssueResponse{}, err
	}
	host := normalizeHost(req.Host)
	if host == "" || req.RemotePort <= 0 || req.App == "" || req.Listener == "" || req.UserID == "" {
		return IssueResponse{}, fmt.Errorf("%w: missing target or user scope", ErrInvalidRequest)
	}
	if req.Role != "admin" {
		return IssueResponse{}, ErrUnauthorized
	}
	pub, err := parsePublicKeyPEM(req.PublicKeyPEM)
	if err != nil {
		return IssueResponse{}, err
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC().Truncate(time.Second)
	ttl := req.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < MinTTL {
		return IssueResponse{}, fmt.Errorf("%w: requested ttl below %s", ErrInvalidRequest, MinTTL)
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}
	notBefore := now.Add(-1 * time.Minute)
	notAfter := now.Add(ttl).UTC().Truncate(time.Second)

	serial, err := randomSerial()
	if err != nil {
		return IssueResponse{}, err
	}
	serialText := serialString(serial)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoadedLocked(true); err != nil {
		return IssueResponse{}, err
	}
	s.pruneLocked(now)

	aud := &url.URL{Scheme: "piccolo", Host: "tunnel", Path: "/" + req.App + "/" + req.Listener}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "piccolo tunnel " + req.UserID},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{host},
		URIs:                  []*url.URL{aud},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.caCert, pub, s.caKey)
	if err != nil {
		return IssueResponse{}, fmt.Errorf("create tunnel client cert: %w", err)
	}
	s.ledger[serialText] = Record{
		Serial:     serialText,
		UserID:     req.UserID,
		Username:   req.Username,
		Role:       req.Role,
		Host:       host,
		RemotePort: req.RemotePort,
		App:        req.App,
		Listener:   req.Listener,
		NotBefore:  notBefore,
		NotAfter:   notAfter,
		CreatedAt:  now,
	}
	if err := s.writeLedgerLocked(); err != nil {
		return IssueResponse{}, err
	}

	return IssueResponse{
		CertificatePEM:           string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		Serial:                   serialText,
		NotAfter:                 notAfter,
		MaxTunnelLifetimeSeconds: int64(MaxTTL.Seconds()),
	}, nil
}

func (s *Service) VerifyTunnelClient(ctx context.Context, req services.TunnelClientVerification) (services.TunnelClientVerificationResult, error) {
	if err := ctx.Err(); err != nil {
		return services.TunnelClientVerificationResult{}, err
	}
	if req.ConnectionAuth == nil || req.ConnectionAuth.MTLS == nil || req.ConnectionAuth.MTLS.Verifier.Type != "piccolo_session" {
		return services.TunnelClientVerificationResult{}, denied(services.TunnelAuthReasonUnsupportedVerifier, fmt.Errorf("%w: unsupported verifier", ErrDenied), nil)
	}
	if len(req.PeerCertificates) == 0 {
		return services.TunnelClientVerificationResult{}, denied(services.TunnelAuthReasonMissingClientCertificate, fmt.Errorf("%w: missing client certificate", ErrDenied), nil)
	}
	cert := req.PeerCertificates[0]
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	host := normalizeHost(req.Host)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoadedLocked(false); err != nil {
		return services.TunnelClientVerificationResult{}, err
	}

	roots := x509.NewCertPool()
	roots.AddCert(s.caCert)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return services.TunnelClientVerificationResult{}, denied(services.TunnelAuthReasonInvalidClientCertificate, fmt.Errorf("%w: invalid client certificate: %v", ErrDenied, err), nil)
	}

	serial := serialString(cert.SerialNumber)
	rec, ok := s.ledger[serial]
	if !ok {
		return services.TunnelClientVerificationResult{}, denied(services.TunnelAuthReasonUnknownCertificateSerial, fmt.Errorf("%w: unknown certificate serial", ErrDenied), nil)
	}
	if now.Before(rec.NotBefore) || !now.Before(rec.NotAfter) {
		return services.TunnelClientVerificationResult{}, denied(services.TunnelAuthReasonCertificateExpired, fmt.Errorf("%w: certificate ledger entry expired", ErrDenied), &rec)
	}
	if rec.Host != host || rec.RemotePort != req.RemotePort || rec.App != req.App || rec.Listener != req.Listener {
		return services.TunnelClientVerificationResult{}, denied(services.TunnelAuthReasonAudienceMismatch, fmt.Errorf("%w: audience mismatch", ErrDenied), &rec)
	}
	if !connectionAuthAllows(req.ConnectionAuth, req.ClientIP) {
		return services.TunnelClientVerificationResult{}, denied(services.TunnelAuthReasonSourceIPDenied, fmt.Errorf("%w: source ip denied", ErrDenied), &rec)
	}

	return services.TunnelClientVerificationResult{
		UserID:   rec.UserID,
		Username: rec.Username,
		Role:     rec.Role,
		Serial:   rec.Serial,
		NotAfter: minTime(cert.NotAfter, rec.NotAfter),
	}, nil
}

func denied(reason string, err error, rec *Record) error {
	var identity services.TunnelClientVerificationResult
	if rec != nil {
		identity = services.TunnelClientVerificationResult{
			UserID:   rec.UserID,
			Username: rec.Username,
			Role:     rec.Role,
			Serial:   rec.Serial,
			NotAfter: rec.NotAfter,
		}
	}
	return services.NewTunnelClientVerificationError(reason, err, identity)
}

func (s *Service) ensureLoadedLocked(create bool) error {
	if s.loaded {
		return nil
	}
	if strings.TrimSpace(s.dir) == "" {
		return fmt.Errorf("%w: missing store directory", ErrUnavailable)
	}
	certPath := filepath.Join(s.dir, caCertFile)
	keyPath := filepath.Join(s.dir, caKeyFile)
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr != nil || keyErr != nil {
		if create && errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) {
			if err := s.generateCALocked(); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("%w: load tunnel client ca", ErrUnavailable)
		}
	} else {
		cert, key, err := parseCA(certPEM, keyPEM)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		s.caCert = cert
		s.caKey = key
	}

	ledger, err := s.readLedgerLocked()
	if err != nil {
		if create && errors.Is(err, os.ErrNotExist) {
			ledger = make(map[string]Record)
		} else {
			return fmt.Errorf("%w: load certificate ledger", ErrUnavailable)
		}
	}
	s.ledger = ledger
	if s.ledger == nil {
		s.ledger = make(map[string]Record)
	}
	s.loaded = true
	return nil
}

func (s *Service) generateCALocked() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("%w: create tunnel auth store: %v", ErrUnavailable, err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate tunnel ca key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Second)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Piccolo tunnel client CA"},
		NotBefore:             now.Add(-1 * time.Minute),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return fmt.Errorf("create tunnel ca cert: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal tunnel ca key: %w", err)
	}
	if err := fsutil.AtomicWriteFile(filepath.Join(s.dir, caCertFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return fmt.Errorf("%w: write tunnel ca cert: %v", ErrUnavailable, err)
	}
	if err := fsutil.AtomicWriteFile(filepath.Join(s.dir, caKeyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return fmt.Errorf("%w: write tunnel ca key: %v", ErrUnavailable, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	s.caCert = cert
	s.caKey = priv
	return nil
}

func (s *Service) readLedgerLocked() (map[string]Record, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, ledgerFileName))
	if err != nil {
		return nil, err
	}
	var lf ledgerFile
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, err
	}
	if lf.Version != ledgerVersion {
		return nil, fmt.Errorf("unsupported ledger version %d", lf.Version)
	}
	return lf.Records, nil
}

func (s *Service) writeLedgerLocked() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("%w: create tunnel auth store: %v", ErrUnavailable, err)
	}
	data, err := json.MarshalIndent(ledgerFile{Version: ledgerVersion, Records: s.ledger}, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(filepath.Join(s.dir, ledgerFileName), data, 0o600)
}

func (s *Service) pruneLocked(now time.Time) {
	for serial, rec := range s.ledger {
		if now.After(rec.NotAfter.Add(ledgerGrace)) {
			delete(s.ledger, serial)
		}
	}
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, crypto.Signer, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, nil, errors.New("invalid ca certificate pem")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, errors.New("invalid ca key pem")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("ca key is not a signer")
	}
	return cert, signer, nil
}

func parsePublicKeyPEM(publicKeyPEM string) (any, error) {
	block, rest := pem.Decode([]byte(strings.TrimSpace(publicKeyPEM)))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("%w: public_key_pem must contain a PUBLIC KEY block", ErrInvalidRequest)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf("%w: public_key_pem must contain one key", ErrInvalidRequest)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed public key", ErrInvalidRequest)
	}
	switch k := pub.(type) {
	case ed25519.PublicKey:
		if len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: invalid ed25519 public key", ErrInvalidRequest)
		}
		return k, nil
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() {
			return nil, fmt.Errorf("%w: only ECDSA P-256 public keys are supported", ErrInvalidRequest)
		}
		return k, nil
	default:
		return nil, fmt.Errorf("%w: unsupported public key type", ErrInvalidRequest)
	}
}

func connectionAuthAllows(cfg *api.ConnectionAuth, clientIP string) bool {
	if cfg == nil || !cfg.HasIPRules() {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(clientIP))
	if ip == nil {
		return false
	}
	def := cfg.Default
	if def == "" {
		def = "allow"
	}
	if def != "allow" && def != "deny" {
		return false
	}
	for _, r := range cfg.Rules {
		if r.Strategy != "allow" && r.Strategy != "deny" {
			return false
		}
		_, cidr, err := net.ParseCIDR(r.Match)
		if err != nil {
			return false
		}
		if cidr.Contains(ip) {
			return r.Strategy == "allow"
		}
	}
	return def == "allow"
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	for {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, err
		}
		if n.Sign() > 0 {
			return n, nil
		}
	}
}

func serialString(n *big.Int) string {
	if n == nil {
		return ""
	}
	return strings.ToLower(n.Text(16))
}

func minTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() || a.Before(b) {
		return a
	}
	return b
}

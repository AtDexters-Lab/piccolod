package crypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// InternalCA manages a self-signed CA for internal HTTPS communication.
// This CA is used for OIDC back-channel communication between containers
// and the Piccolo server.
type InternalCA struct {
	certPath string
	keyPath  string

	mu          sync.RWMutex
	certificate *x509.Certificate
	privateKey  *ecdsa.PrivateKey
	certPEM     []byte
}

// NewInternalCA creates or loads an internal CA from the given directory.
func NewInternalCA(stateDir string) (*InternalCA, error) {
	caDir := filepath.Join(stateDir, "certs")
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		return nil, fmt.Errorf("create CA directory: %w", err)
	}

	ca := &InternalCA{
		certPath: filepath.Join(caDir, "internal-ca.crt"),
		keyPath:  filepath.Join(caDir, "internal-ca.key"),
	}

	// Try to load existing CA
	if err := ca.load(); err == nil {
		return ca, nil
	}

	// Generate new CA if loading fails
	if err := ca.generate(); err != nil {
		return nil, fmt.Errorf("generate CA: %w", err)
	}

	return ca, nil
}

// CertPath returns the path to the CA certificate file.
func (ca *InternalCA) CertPath() string {
	return ca.certPath
}

// CertPEM returns the PEM-encoded CA certificate.
func (ca *InternalCA) CertPEM() []byte {
	ca.mu.RLock()
	defer ca.mu.RUnlock()
	return ca.certPEM
}

// Certificate returns the CA certificate.
func (ca *InternalCA) Certificate() *x509.Certificate {
	ca.mu.RLock()
	defer ca.mu.RUnlock()
	return ca.certificate
}

// IssueCertificate generates a new certificate signed by this CA.
func (ca *InternalCA) IssueCertificate(cn string, dnsNames []string, validFor time.Duration) (certPEM, keyPEM []byte, err error) {
	ca.mu.RLock()
	defer ca.mu.RUnlock()

	if ca.certificate == nil || ca.privateKey == nil {
		return nil, nil, errors.New("CA not initialized")
	}

	// Generate key for the new certificate
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{"Piccolo"},
		},
		NotBefore:             now,
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, &privateKey.PublicKey, ca.privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}

	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyDER,
	})

	return certPEM, keyPEM, nil
}

// load attempts to load an existing CA from disk.
func (ca *InternalCA) load() error {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	// Load certificate
	certPEM, err := os.ReadFile(ca.certPath)
	if err != nil {
		return fmt.Errorf("read certificate: %w", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return errors.New("failed to decode certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse certificate: %w", err)
	}

	// Check if certificate is still valid
	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return errors.New("certificate expired or not yet valid")
	}

	// Load private key
	keyPEM, err := os.ReadFile(ca.keyPath)
	if err != nil {
		return fmt.Errorf("read private key: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return errors.New("failed to decode private key PEM")
	}

	privateKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}

	ca.certificate = cert
	ca.privateKey = privateKey
	ca.certPEM = certPEM

	return nil
}

// generate creates a new self-signed CA certificate.
func (ca *InternalCA) generate() error {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	// Generate private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate private key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate serial number: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:         "Piccolo Internal CA",
			Organization:       []string{"Piccolo"},
			OrganizationalUnit: []string{"Internal"},
		},
		NotBefore:             now,
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour), // 10 years
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	// Self-sign the certificate
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return fmt.Errorf("parse generated certificate: %w", err)
	}

	// Encode to PEM
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyDER,
	})

	// Write to disk
	if err := os.WriteFile(ca.certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write certificate: %w", err)
	}

	if err := os.WriteFile(ca.keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}

	ca.certificate = cert
	ca.privateKey = privateKey
	ca.certPEM = certPEM

	return nil
}

// ServerCertPath returns the path to the server certificate.
// This is a convenience for the most common use case.
func (ca *InternalCA) ServerCertPath() string {
	return filepath.Join(filepath.Dir(ca.certPath), "server.crt")
}

// ServerKeyPath returns the path to the server private key.
func (ca *InternalCA) ServerKeyPath() string {
	return filepath.Join(filepath.Dir(ca.certPath), "server.key")
}

// EnsureServerCertificate generates and saves a server certificate for piccolo.local
// if it doesn't exist or is expired.
func (ca *InternalCA) EnsureServerCertificate() error {
	certPath := ca.ServerCertPath()
	keyPath := ca.ServerKeyPath()

	// Check if certificate exists and is valid
	if certPEM, err := os.ReadFile(certPath); err == nil {
		block, _ := pem.Decode(certPEM)
		if block != nil {
			if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
				// Check if certificate is valid for at least 30 more days
				if time.Now().Add(30 * 24 * time.Hour).Before(cert.NotAfter) {
					return nil // Certificate is still valid
				}
			}
		}
	}

	// Generate new certificate
	certPEM, keyPEM, err := ca.IssueCertificate(
		"piccolo.local",
		[]string{"piccolo.local", "localhost"},
		365*24*time.Hour, // 1 year
	)
	if err != nil {
		return fmt.Errorf("issue server certificate: %w", err)
	}

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write server certificate: %w", err)
	}

	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write server key: %w", err)
	}

	return nil
}

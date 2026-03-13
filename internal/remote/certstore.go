package remote

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"piccolod/internal/services"
	"piccolod/internal/state/paths"
)

// FileCertProvider loads certificates from an on-disk store under the encrypted
// control volume. It implements services.CertProvider.
type FileCertProvider struct {
	base         string
	mu           sync.RWMutex
	cache        map[string]*tls.Certificate
	missing      func(host string)
	fallbackDirs []string                       // additional cert directories (e.g., network-bootstrap)
	portalMaps   map[string]map[string]string   // source → (hostname → certName) for multi-portal cert resolution
}

// NewFileCertProvider constructs a provider rooted at <control>/remote/certs when
// base is empty, or at the provided base otherwise.
func NewFileCertProvider(base string) *FileCertProvider {
	if strings.TrimSpace(base) == "" {
		base = filepath.Join(paths.CoreJoin("mounts", "control-plane"), "remote", "certs")
	}
	return &FileCertProvider{base: base, cache: make(map[string]*tls.Certificate)}
}

func (p *FileCertProvider) GetCertificate(host string) (*tls.Certificate, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return nil, services.ErrNoCert
	}
	p.mu.RLock()
	portalMaps := p.portalMaps
	p.mu.RUnlock()

	// Check portal cert mappings across all sources (multi-portal support)
	for _, m := range portalMaps {
		if certName, ok := m[host]; ok {
			if cert := p.tryLoad(certName); cert != nil {
				p.toCache(certName, cert)
				return cert, nil
			}
			if cert := p.fromCache(certName); cert != nil {
				return cert, nil
			}
		}
	}

	// Always prefer fresh load from disk, then fall back to cache.
	if cert := p.tryLoad(host); cert != nil {
		p.toCache(host, cert)
		return cert, nil
	}
	if cert := p.fromCache(host); cert != nil {
		return cert, nil
	}
	// Wildcard fallback: *.domain
	if i := strings.Index(host, "."); i != -1 {
		domain := host[i+1:]
		if domain != "" {
			star := "*." + domain
			if cert := p.tryLoad(star); cert != nil {
				p.toCache(star, cert)
				return cert, nil
			}
			if cert := p.fromCache(star); cert != nil {
				return cert, nil
			}
		}
	}
	p.mu.RLock()
	missing := p.missing
	p.mu.RUnlock()
	if missing != nil {
		go missing(host)
	}
	return nil, services.ErrNoCert
}

func (p *FileCertProvider) fromCache(key string) *tls.Certificate {
	p.mu.RLock()
	c := p.cache[key]
	p.mu.RUnlock()
	return c
}

func (p *FileCertProvider) toCache(key string, cert *tls.Certificate) {
	p.mu.Lock()
	p.cache[key] = cert
	p.mu.Unlock()
}

// SetMissingHandler registers a callback invoked when a cert is requested but not found.
// The handler is called asynchronously.
func (p *FileCertProvider) SetMissingHandler(fn func(host string)) {
	p.mu.Lock()
	p.missing = fn
	p.mu.Unlock()
}

// AddFallbackDir adds an additional directory to search for certs (e.g., network-bootstrap certs).
func (p *FileCertProvider) AddFallbackDir(dir string) {
	if dir == "" {
		return
	}
	p.mu.Lock()
	// Avoid duplicates
	for _, d := range p.fallbackDirs {
		if d == dir {
			p.mu.Unlock()
			return
		}
	}
	p.fallbackDirs = append(p.fallbackDirs, dir)
	p.mu.Unlock()
}

// SetPortalMappings configures source-tagged hostname→certName mappings for multi-portal cert resolution.
// Uses copy-on-write to avoid concurrent map read/write with GetCertificate.
func (p *FileCertProvider) SetPortalMappings(source string, mappings []services.PortalCertMapping) {
	m := make(map[string]string, len(mappings))
	for _, pm := range mappings {
		m[pm.Hostname] = pm.CertName
	}
	p.mu.Lock()
	newMaps := make(map[string]map[string]string, len(p.portalMaps)+1)
	for k, v := range p.portalMaps {
		if k != source {
			newMaps[k] = v
		}
	}
	if len(m) > 0 {
		newMaps[source] = m
	}
	p.portalMaps = newMaps
	p.mu.Unlock()
}

func (p *FileCertProvider) tryLoad(name string) *tls.Certificate {
	if cert := tryLoadFromDir(p.base, name); cert != nil {
		return cert
	}
	// Check fallback dirs (e.g., network-bootstrap certs)
	p.mu.RLock()
	dirs := p.fallbackDirs
	p.mu.RUnlock()
	for _, dir := range dirs {
		if cert := tryLoadFromDir(dir, name); cert != nil {
			return cert
		}
	}
	return nil
}

func tryLoadFromDir(dir, name string) *tls.Certificate {
	// Prefer separate CRT/KEY pair
	crt := filepath.Join(dir, name+".crt")
	key := filepath.Join(dir, name+".key")
	if fileExists(crt) && fileExists(key) {
		if c, err := tls.LoadX509KeyPair(crt, key); err == nil {
			return &c
		}
	}
	// Fallback to PEM bundle (cert + key in one file)
	pemPath := filepath.Join(dir, name+".pem")
	if fileExists(pemPath) {
		if c, err := loadPEMBundle(pemPath); err == nil {
			return c
		}
	}
	return nil
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func loadPEMBundle(path string) (*tls.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var certs [][]byte
	var keyBlock *pem.Block
	rest := data
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		switch b.Type {
		case "CERTIFICATE":
			certs = append(certs, pem.EncodeToMemory(b))
		case "PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY":
			if keyBlock == nil {
				keyBlock = b
			}
		}
	}
	if len(certs) == 0 || keyBlock == nil {
		return nil, errors.New("pem bundle missing cert or key")
	}
	// Parse key to validate
	if _, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err != nil {
		// pkcs1/rsa or ec? try generic parse
		if _, err2 := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err2 != nil {
			if _, err3 := x509.ParseECPrivateKey(keyBlock.Bytes); err3 != nil {
				// key is still acceptable for tls.Certificate as raw PEM; continue
			}
		}
	}
	// Concatenate certs
	var certPEM []byte
	for _, c := range certs {
		certPEM = append(certPEM, c...)
	}
	keyPEM := pem.EncodeToMemory(keyBlock)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("x509 keypair: %w", err)
	}
	return &pair, nil
}

var _ services.CertProvider = (*FileCertProvider)(nil)

package acme

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	lego "github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	gandiv5 "github.com/go-acme/lego/v4/providers/dns/gandiv5"
	"github.com/go-acme/lego/v4/providers/dns/route53"
	"github.com/go-acme/lego/v4/registration"
	acmepkg "golang.org/x/crypto/acme"
)

// ChallengeSink exposes Present/CleanUp to publish HTTP-01 tokens.
type ChallengeSink interface {
	Handler() http.Handler
	Put(token, value string)
	Delete(token string)
}

// Manager orchestrates ACME account and issuance via lego with HTTP-01.
type Manager struct {
	baseDir   string
	directory string
	email     string
	sink      ChallengeSink

	mu             sync.RWMutex
	solver         string
	dnsProvider    string
	dnsCredentials map[string]string
}

// NewManager constructs a lego-backed ACME manager.
func NewManager(stateDir string, sink ChallengeSink, email string, directoryURL string) *Manager {
	if stateDir == "" {
		stateDir = "."
	}
	if email == "" {
		email = "admin@local"
	}
	if directoryURL == "" {
		if v := os.Getenv("PICCOLO_ACME_DIR_URL"); v != "" {
			directoryURL = v
		} else {
			directoryURL = "https://acme-v02.api.letsencrypt.org/directory"
		}
	}
	log.Printf("INFO: ACME directory configured: %s", directoryURL)
	return &Manager{
		baseDir:        filepath.Join(stateDir, "remote", "acme"),
		directory:      directoryURL,
		email:          email,
		sink:           sink,
		solver:         "http-01",
		dnsCredentials: map[string]string{},
		dnsProvider:    "",
	}
}

type account struct {
	Email        string                 `json:"email"`
	Registration *registration.Resource `json:"registration"`
	key          *ecdsa.PrivateKey      `json:"-"`
}

func (a *account) GetEmail() string                        { return a.Email }
func (a *account) GetRegistration() *registration.Resource { return a.Registration }
func (a *account) GetPrivateKey() crypto.PrivateKey        { return a.key }

func (m *Manager) ensureDirs() error { return os.MkdirAll(m.baseDir, 0o700) }

func (m *Manager) accountPaths() (keyPath, regPath string) {
	return filepath.Join(m.baseDir, "account.key"), filepath.Join(m.baseDir, "account.json")
}

func (m *Manager) loadAccount() (*account, error) {
	if err := m.ensureDirs(); err != nil {
		return nil, err
	}
	keyPath, regPath := m.accountPaths()
	b, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	key, err := certcrypto.ParsePEMPrivateKey(b)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	curEmail := m.email
	m.mu.RUnlock()
	a := &account{Email: curEmail}
	if data, err := os.ReadFile(regPath); err == nil {
		var res registration.Resource
		if e := json.Unmarshal(data, &res); e == nil {
			a.Registration = &res
		}
	}
	// key may be any; cast to *ecdsa.PrivateKey if possible, else wrap
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		a.key = k
	default:
		// regenerate to be safe
		return nil, errors.New("unsupported key format")
	}
	return a, nil
}

func (m *Manager) saveAccount(a *account) error {
	if err := m.ensureDirs(); err != nil {
		return err
	}
	keyPath, regPath := m.accountPaths()
	b, err := pemEncodeEC(a.key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, b, 0o600); err != nil {
		return err
	}
	if a.Registration != nil {
		data, _ := json.MarshalIndent(a.Registration, "", "  ")
		_ = os.WriteFile(regPath, data, 0o600)
	}
	return nil
}

func (m *Manager) resetAccountCache() error {
	keyPath, regPath := m.accountPaths()
	if err := os.Remove(keyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(regPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func isAccountDoesNotExist(err error) bool {
	if err == nil {
		return false
	}
	if acmeErr, ok := err.(*acmepkg.Error); ok {
		return acmeErr.ProblemType == "urn:ietf:params:acme:error:accountDoesNotExist"
	}
	return strings.Contains(err.Error(), "accountDoesNotExist")
}

func hostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// SetEmail updates the preferred contact email used for ACME registration.
func (m *Manager) SetEmail(email string) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return
	}
	m.mu.Lock()
	m.email = email
	m.mu.Unlock()
}

// SetSolver configures the challenge solver and optional DNS provider credentials.
// Solver should be "http-01" or "dns-01".
// For dns-01, dnsProvider must be one of: cloudflare, route53, gandi.
// Credentials are provider-specific and stored in-memory only.
func (m *Manager) SetSolver(solver, dnsProvider string, creds map[string]string) error {
	if m == nil {
		return nil
	}
	s := strings.ToLower(strings.TrimSpace(solver))
	if s == "" {
		s = "http-01"
	}
	p := strings.ToLower(strings.TrimSpace(dnsProvider))
	if s == "dns-01" && p != "" {
		switch p {
		case "cloudflare", "route53", "gandi", "gandi.net", "gandiv5":
		default:
			return fmt.Errorf("acme: unsupported dns provider %q", p)
		}
	}
	m.mu.Lock()
	m.solver = s
	m.dnsProvider = p
	m.dnsCredentials = cloneCredentials(creds)
	m.mu.Unlock()
	return nil
}

func cloneCredentials(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (m *Manager) configureChallenge(cli *lego.Client) error {
	if m == nil || cli == nil {
		return errors.New("acme: client unavailable")
	}
	m.mu.RLock()
	solver := m.solver
	m.mu.RUnlock()
	if strings.EqualFold(solver, "dns-01") {
		prov, err := m.buildDNSProvider()
		if err != nil {
			return err
		}
		return cli.Challenge.SetDNS01Provider(prov)
	}
	prov := &http01Provider{sink: m.sink}
	return cli.Challenge.SetHTTP01Provider(prov)
}

func (m *Manager) buildDNSProvider() (challenge.Provider, error) {
	m.mu.RLock()
	p := m.dnsProvider
	creds := cloneCredentials(m.dnsCredentials)
	m.mu.RUnlock()
	switch p {
	case "cloudflare":
		cfg := cloudflare.NewDefaultConfig()
		cfg.AuthToken = strings.TrimSpace(creds["api_token"])
		if cfg.AuthToken == "" {
			cfg.AuthToken = strings.TrimSpace(creds["dns_api_token"])
		}
		cfg.ZoneToken = strings.TrimSpace(creds["zone_api_token"])
		cfg.AuthEmail = strings.TrimSpace(creds["email"])
		cfg.AuthKey = strings.TrimSpace(creds["api_key"])
		return cloudflare.NewDNSProviderConfig(cfg)
	case "route53":
		cfg := route53.NewDefaultConfig()
		cfg.AccessKeyID = strings.TrimSpace(creds["access_key"])
		cfg.SecretAccessKey = strings.TrimSpace(creds["secret_key"])
		cfg.Region = strings.TrimSpace(creds["region"])
		cfg.HostedZoneID = strings.TrimSpace(creds["hosted_zone_id"])
		return route53.NewDNSProviderConfig(cfg)
	case "gandi", "gandi.net", "gandiv5":
		cfg := gandiv5.NewDefaultConfig()
		cfg.APIKey = strings.TrimSpace(creds["api_key"])
		if cfg.APIKey == "" {
			cfg.APIKey = strings.TrimSpace(creds["api_token"])
		}
		return gandiv5.NewDNSProviderConfig(cfg)
	default:
		return nil, fmt.Errorf("acme: dns provider not configured")
	}
}

// EnsureAccount loads or creates a new ACME account (P-256), accepts TOS.
func (m *Manager) EnsureAccount() (*lego.Client, *account, error) {
	if acc, err := m.loadAccount(); err == nil {
		if acc != nil && acc.Registration != nil && acc.Registration.URI != "" {
			if regHost, dirHost := hostFromURL(acc.Registration.URI), hostFromURL(m.directory); regHost != "" && dirHost != "" && !strings.EqualFold(regHost, dirHost) {
				log.Printf("INFO: ACME cached account host %s differs from directory %s; resetting", regHost, dirHost)
				if err := m.resetAccountCache(); err != nil {
					return nil, nil, err
				}
				acc = nil
			}
		}
		if acc != nil {
			cfg := lego.NewConfig(acc)
			cfg.CADirURL = m.directory
			cfg.Certificate.KeyType = certcrypto.EC256
			cli, err := lego.NewClient(cfg)
			if err != nil {
				return nil, nil, err
			}
			if err := m.configureChallenge(cli); err != nil {
				return nil, nil, err
			}
			if acc.Registration != nil {
				log.Printf("INFO: ACME loaded cached account %s", acc.Registration.URI)
			}
			return cli, acc, nil
		}
	}
	// Create new
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	m.mu.RLock()
	curEmail := m.email
	m.mu.RUnlock()
	acc := &account{Email: curEmail, key: key}
	cfg := lego.NewConfig(acc)
	cfg.CADirURL = m.directory
	cfg.Certificate.KeyType = certcrypto.EC256
	cli, err := lego.NewClient(cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := m.configureChallenge(cli); err != nil {
		return nil, nil, err
	}
	// New registration with TOS
	reg, err := cli.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return nil, nil, err
	}
	acc.Registration = reg
	if err := m.saveAccount(acc); err != nil {
		return nil, nil, err
	}
	if acc.Registration != nil {
		log.Printf("INFO: ACME registered new account %s", acc.Registration.URI)
	}
	return cli, acc, nil
}

// Issue writes certificate and key files for the given commonName and SANs.
func (m *Manager) Issue(commonName string, sans []string, outName string, certDir string) (*tls.Certificate, error) {
	for attempt := 0; attempt < 2; attempt++ {
		cli, _, err := m.EnsureAccount()
		if err != nil {
			return nil, err
		}
		req := certificate.ObtainRequest{Domains: append([]string{commonName}, sans...), Bundle: true}
		certRes, err := cli.Certificate.Obtain(req)
		if err != nil {
			if attempt == 0 && isAccountDoesNotExist(err) {
				log.Printf("WARN: ACME account invalid, resetting cache and retrying: %v", err)
				if err := m.resetAccountCache(); err != nil {
					log.Printf("WARN: failed to reset ACME cache: %v", err)
				}
				continue
			}
			return nil, err
		}
		if err := os.MkdirAll(certDir, 0o700); err != nil {
			return nil, err
		}
		crtPath := filepath.Join(certDir, outName+".crt")
		keyPath := filepath.Join(certDir, outName+".key")
		if err := os.WriteFile(crtPath, certRes.Certificate, 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(keyPath, certRes.PrivateKey, 0o600); err != nil {
			return nil, err
		}
		pair, err := tls.X509KeyPair(certRes.Certificate, certRes.PrivateKey)
		if err != nil {
			return nil, err
		}
		return &pair, nil
	}
	return nil, errors.New("acme: failed to obtain certificate after retry")
}

// http01Provider bridges lego HTTP-01 to our ChallengeSink.
type http01Provider struct{ sink ChallengeSink }

func (p *http01Provider) Present(domain, token, keyAuth string) error {
	if p.sink == nil {
		return errors.New("acme: sink unavailable")
	}
	p.sink.Put(token, keyAuth)
	return nil
}
func (p *http01Provider) CleanUp(domain, token, keyAuth string) error {
	if p.sink != nil {
		p.sink.Delete(token)
	}
	return nil
}
func (p *http01Provider) GetType() string { return "http-01" }

// PEM encode helper for EC keys
func pemEncodeEC(key *ecdsa.PrivateKey) ([]byte, error) {
	b, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	block := &pem.Block{Type: "EC PRIVATE KEY", Bytes: b}
	return pem.EncodeToMemory(block), nil
}

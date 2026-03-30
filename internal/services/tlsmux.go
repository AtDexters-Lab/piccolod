package services

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CertProvider returns a certificate for the given SNI hostname.
// Implementations should read from the encrypted cert store.
type CertProvider interface {
	GetCertificate(host string) (*tls.Certificate, error)
}

type portalAwareProvider interface {
	SetPortalMappings(source string, mappings []PortalCertMapping)
}

// PortalCertMapping maps a portal hostname to a cert name for TLS mux portal awareness.
type PortalCertMapping struct {
	Hostname string
	CertName string
}

// portalHostLabel mirrors nexusclient.PortalHostLabel — the sentinel for portal-targeted aliases.
// Defined locally to avoid coupling the services package to nexusclient.
const portalHostLabel = "__portal"

// TlsMuxBase represents a remote base for TLS mux routing (RFC 20260312).
type TlsMuxBase struct {
	Source     string // source identifier for targeted removal (e.g., "self-hosted")
	PortalHost string
	Domain     string
}

// TlsMux terminates TLS (remote-only) on loopback and forwards HTTP to a local public_port.
// It does not expose any TLS listener on the LAN.
type TlsMux struct {
	mu      sync.RWMutex
	ln      net.Listener
	port    int
	running bool
	stopCh  chan struct{}

	// Routing config
	portalHost string
	portalPort int
	domain     string // e.g., example.com (no trailing dot)

	// Alias routing: hostname → DerivedHostLabel (or "__portal")
	aliases map[string]string

	// Additional remote bases (RFC 20260312)
	remoteBases []TlsMuxBase

	services *ServiceManager
	certs    CertProvider
}

func NewTlsMux(svc *ServiceManager) *TlsMux {
	return &TlsMux{services: svc, stopCh: make(chan struct{})}
}

// UpdateConfig sets portal hostname, domain, and portal upstream port.
func (m *TlsMux) UpdateConfig(portalHost, domain string, portalPort int) {
	m.mu.Lock()
	m.portalHost = strings.TrimSuffix(strings.ToLower(portalHost), ".")
	m.domain = strings.TrimSuffix(strings.ToLower(domain), ".")
	m.portalPort = portalPort
	if prov, ok := m.certs.(portalAwareProvider); ok && m.portalHost != "" {
		prov.SetPortalMappings("self-hosted", []PortalCertMapping{
			{Hostname: m.portalHost, CertName: "portal"},
		})
	}
	m.mu.Unlock()
}

func (m *TlsMux) SetCertProvider(p CertProvider) { m.mu.Lock(); m.certs = p; m.mu.Unlock() }

// UpdateAliases sets the alias hostname→hostLabel mapping for routing.
// hostLabel is a DerivedHostLabel or "__portal" for portal-targeted aliases.
func (m *TlsMux) UpdateAliases(aliases map[string]string) {
	m.mu.Lock()
	m.aliases = aliases
	m.mu.Unlock()
}

// SetRemoteBases replaces all remote bases for a given source (RFC 20260312).
func (m *TlsMux) SetRemoteBases(source string, bases []TlsMuxBase) {
	m.mu.Lock()
	var kept []TlsMuxBase
	for _, b := range m.remoteBases {
		if b.Source != source {
			kept = append(kept, b)
		}
	}
	m.remoteBases = append(kept, bases...)
	m.mu.Unlock()
}

// Start binds on 127.0.0.1:0 (ephemeral) unless already running. Returns the selected port.
func (m *TlsMux) Start() (int, error) {
	m.mu.Lock()
	if m.running {
		p := m.port
		m.mu.Unlock()
		return p, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		m.mu.Unlock()
		return 0, err
	}
	m.ln = ln
	addr := ln.Addr().(*net.TCPAddr)
	m.port = addr.Port
	m.running = true
	stopCh := m.stopCh
	services := m.services
	m.mu.Unlock()

	go func() {
		// Build tls.Config with SNI callback and hardened cipher suite policy.
		tlsCfg := &tls.Config{
			MinVersion:   tls.VersionTLS12,
			CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305},
			CurvePreferences: []tls.CurveID{
				tls.X25519,
				tls.CurveP256,
				tls.CurveP384,
			},
			PreferServerCipherSuites: true,
			GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
				host := strings.TrimSuffix(strings.ToLower(chi.ServerName), ".")
				// Ask provider for certificate
				m.mu.RLock()
				prov := m.certs
				m.mu.RUnlock()
				if prov == nil {
					return nil, errors.New("cert provider unavailable")
				}
				return prov.GetCertificate(host)
			},
		}
		tlsLn := tls.NewListener(ln, tlsCfg)
		for {
			conn, err := tlsLn.Accept()
			if err != nil {
				// Check for shutdown
				select {
				case <-stopCh:
					return
				default:
				}
				if ne, ok := err.(net.Error); ok && ne.Temporary() {
					time.Sleep(50 * time.Millisecond)
					continue
				}
				return
			}
			go m.serveTLSConn(conn, services)
		}
	}()

	log.Printf("INFO: TLS mux listening on 127.0.0.1:%d (remote-only)", m.port)
	return m.port, nil
}

func (m *TlsMux) Stop() {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	close(m.stopCh)
	_ = m.ln.Close()
	m.running = false
	m.port = 0
	m.stopCh = make(chan struct{})
	m.mu.Unlock()
}

func (m *TlsMux) Port() int { m.mu.RLock(); defer m.mu.RUnlock(); return m.port }

func (m *TlsMux) serveTLSConn(c net.Conn, services *ServiceManager) {
	// Extract SNI from tls.Conn
	tlsConn, ok := c.(*tls.Conn)
	if !ok {
		c.Close()
		return
	}
	// Ensure handshake is complete so SNI is available.
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("WARN: tlsmux handshake failed: %v", err)
		_ = tlsConn.Close()
		return
	}
	state := tlsConn.ConnectionState()
	host := ""
	if state.ServerName != "" {
		host = strings.TrimSuffix(strings.ToLower(state.ServerName), ".")
	}
	var (
		hint     connectionHint
		haveHint bool
	)
	if services != nil {
		if addr, ok := tlsConn.RemoteAddr().(*net.TCPAddr); ok {
			hint, haveHint = services.consumeProxyHint(m.Port(), addr.Port)
		}
	}
	if host == "" {
		m.mu.RLock()
		host = m.portalHost
		m.mu.RUnlock()
	}
	upstream := m.resolveUpstream(host)
	if upstream == 0 {
		log.Printf("WARN: tlsmux: unknown host %q", host)
		c.Close()
		return
	}
	backendAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(upstream))
	backend, err := net.DialTimeout("tcp", backendAddr, 5*time.Second)
	if err != nil {
		log.Printf("WARN: tlsmux upstream dial %s failed: %v", backendAddr, err)
		// Hint already consumed; nothing further to clean up on failure.
		c.Close()
		return
	}
	if services != nil {
		if addr, ok := backend.LocalAddr().(*net.TCPAddr); ok {
			remotePort := 0
			clientIP := ""
			if haveHint && hint.remotePort > 0 {
				remotePort = hint.remotePort
			}
			if haveHint {
				clientIP = hint.clientIP
			}
			isTLS := true
			if haveHint {
				isTLS = hint.isTLS || isTLS
			}
			services.RegisterProxyHint(upstream, addr.Port, remotePort, isTLS, clientIP)
		}
	}
	// Bi-directional copy: cleartext HTTP over TLS to upstream HTTP
	go func() {
		io.Copy(backend, tlsConn)
		if tc, ok := backend.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	io.Copy(tlsConn, backend)
	_ = tlsConn.Close()
	_ = backend.Close()
}

func (m *TlsMux) resolveUpstream(host string) int {
	m.mu.RLock()
	portal := m.portalHost
	domain := m.domain
	portalPort := m.portalPort
	aliases := m.aliases
	bases := m.remoteBases
	m.mu.RUnlock()

	if host == "" {
		return 0
	}
	if host == portal {
		return portalPort
	}
	// Alias domains: route by the listener they target.
	if hostLabel, ok := aliases[host]; ok {
		if hostLabel == portalHostLabel || hostLabel == "" {
			return portalPort
		}
		if m.services != nil {
			if ep, found := m.services.ResolveByHostLabel(hostLabel, 443); found {
				return ep.PublicPort
			}
		}
		return 0
	}
	// <app>.<domain> or <listener>-<app>.<domain> → map to ServiceManager public_port
	// Per RFC 20260114: use DerivedHostLabel for routing (primary=<app>, others=<listener>-<app>)
	if domain != "" && strings.HasSuffix(host, "."+domain) {
		if port := m.resolveByDomain(host, domain); port != 0 {
			return port
		}
	}
	// Check additional remote bases (RFC 20260312).
	// Two passes: exact portal match first, then subdomain — ensures a more-specific
	// portal hostname is not misclassified as a subdomain of a less-specific base.
	for _, rb := range bases {
		if host == rb.PortalHost {
			return portalPort
		}
	}
	// Longest domain match (consistent with resolver's PortalHostForRequest).
	var bestDomain string
	var bestLen int
	for _, rb := range bases {
		if rb.Domain != "" && host != rb.PortalHost && strings.HasSuffix(host, "."+rb.Domain) && len(rb.Domain) > bestLen {
			bestDomain = rb.Domain
			bestLen = len(rb.Domain)
		}
	}
	if bestDomain != "" {
		if port := m.resolveByDomain(host, bestDomain); port != 0 {
			return port
		}
	}
	return 0
}

func (m *TlsMux) resolveByDomain(host, domain string) int {
	label := strings.TrimSuffix(host, "."+domain)
	if i := strings.Index(label, "."); i != -1 {
		label = label[:i]
	}
	if label != "" && m.services != nil {
		if ep, ok := m.services.ResolveByHostLabel(label, 443); ok {
			return ep.PublicPort
		}
	}
	return 0
}

// ErrNoCert is returned by a CertProvider when no certificate is available.
var ErrNoCert = fmt.Errorf("no certificate available")

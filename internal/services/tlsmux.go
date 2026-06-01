package services

import (
	"context"
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
	verifier TunnelClientVerifier

	decisionRecorder  TunnelAuthDecisionRecorder
	tunnelAuthMetrics *TunnelAuthMetricsRegistry
}

func NewTlsMux(svc *ServiceManager) *TlsMux {
	return &TlsMux{services: svc, stopCh: make(chan struct{}), tunnelAuthMetrics: NewTunnelAuthMetricsRegistry()}
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

func (m *TlsMux) SetTunnelClientVerifier(v TunnelClientVerifier) {
	m.mu.Lock()
	m.verifier = v
	m.mu.Unlock()
}

func (m *TlsMux) SetTunnelAuthDecisionRecorder(recorder TunnelAuthDecisionRecorder) {
	m.mu.Lock()
	m.decisionRecorder = recorder
	m.mu.Unlock()
}

func (m *TlsMux) TunnelAuthMetricsSnapshot() TunnelAuthMetricsSnapshot {
	if m == nil || m.tunnelAuthMetrics == nil {
		return TunnelAuthMetricsSnapshot{
			Allowed: map[TunnelAuthMetricsSample]uint64{},
			Denied:  map[TunnelAuthMetricsSample]uint64{},
		}
	}
	return m.tunnelAuthMetrics.Snapshot()
}

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
		tlsCfg := tlsMuxBaseConfig()
		tlsCfg.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			host := strings.TrimSuffix(strings.ToLower(chi.ServerName), ".")
			m.mu.RLock()
			prov := m.certs
			m.mu.RUnlock()
			if prov == nil {
				return nil, errors.New("cert provider unavailable")
			}
			cert, err := prov.GetCertificate(host)
			if err != nil {
				return nil, err
			}
			cfg := tlsMuxBaseConfig()
			cfg.Certificates = []tls.Certificate{*cert}

			remotePort := 443
			if services != nil && chi.Conn != nil {
				if addr, ok := chi.Conn.RemoteAddr().(*net.TCPAddr); ok {
					if hint, haveHint := services.peekProxyHint(m.Port(), addr.Port); haveHint && hint.remotePort > 0 {
						remotePort = hint.remotePort
					}
				}
			}
			if route := m.resolveRoute(host, remotePort); route.requiresTLSMuxAuth {
				cfg.ClientAuth = tls.RequestClientCert
				cfg.SessionTicketsDisabled = true
			}
			return cfg, nil
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

func tlsMuxBaseConfig() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305},
		CurvePreferences: []tls.CurveID{
			tls.X25519,
			tls.CurveP256,
			tls.CurveP384,
		},
		PreferServerCipherSuites: true,
	}
}

func (m *TlsMux) serveTLSConn(c net.Conn, services *ServiceManager) {
	// Extract SNI from tls.Conn
	tlsConn, ok := c.(*tls.Conn)
	if !ok {
		c.Close()
		return
	}
	// Ensure handshake is complete so SNI is available.
	if err := tlsConn.Handshake(); err != nil {
		// INFO not WARN: internet scanner probes with obsolete TLS versions are
		// expected noise on public-facing devices, not actionable warnings.
		log.Printf("INFO: tlsmux handshake failed: %v", err)
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
	effectiveRemotePort := 443
	if haveHint && hint.remotePort > 0 {
		effectiveRemotePort = hint.remotePort
	}
	route := m.resolveRoute(host, effectiveRemotePort)
	if route.upstream == 0 {
		log.Printf("WARN: tlsmux: unknown host %q", host)
		c.Close()
		return
	}
	if route.requiresTLSMuxAuth {
		clientIP := peerClientIP(tlsConn, hint, haveHint)
		decision := m.tunnelAuthDecision(route, host, effectiveRemotePort, clientIP)
		result, err := m.verifyTunnelClient(state, route, host, effectiveRemotePort, clientIP)
		if err != nil {
			decision.applyVerificationError(err)
			m.recordTunnelAuthDecision(decision)
			log.Printf("WARN: tlsmux mTLS denied host=%q app=%q listener=%q remote_port=%d client_ip=%q: %v", host, route.app, route.listener, effectiveRemotePort, clientIP, err)
			c.Close()
			return
		}
		if !result.NotAfter.IsZero() {
			until := time.Until(result.NotAfter)
			if until <= 0 {
				decision.applyVerificationResult(result)
				decision.DenyReason = TunnelAuthReasonCertificateExpired
				m.recordTunnelAuthDecision(decision)
				log.Printf("WARN: tlsmux mTLS denied host=%q app=%q listener=%q: expired tunnel certificate", host, route.app, route.listener)
				c.Close()
				return
			}
			timer := time.AfterFunc(until, func() { _ = tlsConn.Close() })
			defer timer.Stop()
		}
		decision.Allowed = true
		decision.applyVerificationResult(result)
		m.recordTunnelAuthDecision(decision)
		log.Printf("INFO: tlsmux mTLS allowed host=%q app=%q listener=%q user=%q serial=%q", host, route.app, route.listener, result.UserID, result.Serial)
	}
	backendAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(route.upstream))
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
			clientIP = hintClientIP(hint, haveHint)
			isTLS := true
			if haveHint {
				isTLS = hint.isTLS || isTLS
			}
			services.RegisterProxyHint(route.upstream, addr.Port, remotePort, isTLS, clientIP)
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

type tlsMuxRoute struct {
	upstream           int
	app                string
	listener           string
	requiresTLSMuxAuth bool
	endpoint           *ServiceEndpoint
}

func (m *TlsMux) verifyTunnelClient(state tls.ConnectionState, route tlsMuxRoute, host string, remotePort int, clientIP string) (TunnelClientVerificationResult, error) {
	m.mu.RLock()
	verifier := m.verifier
	m.mu.RUnlock()
	if verifier == nil {
		return TunnelClientVerificationResult{}, NewTunnelClientVerificationError(TunnelAuthReasonVerifierUnavailable, fmt.Errorf("tunnel verifier unavailable"), TunnelClientVerificationResult{})
	}
	if route.endpoint == nil {
		return TunnelClientVerificationResult{}, NewTunnelClientVerificationError(TunnelAuthReasonMissingRouteMetadata, fmt.Errorf("missing route endpoint"), TunnelClientVerificationResult{})
	}
	return verifier.VerifyTunnelClient(context.Background(), TunnelClientVerification{
		PeerCertificates: state.PeerCertificates,
		Host:             host,
		RemotePort:       remotePort,
		App:              route.endpoint.App,
		Listener:         route.endpoint.Name,
		ClientIP:         clientIP,
		ConnectionAuth:   route.endpoint.ConnectionAuth,
		Now:              time.Now(),
	})
}

func (m *TlsMux) tunnelAuthDecision(route tlsMuxRoute, host string, remotePort int, clientIP string) TunnelAuthDecision {
	return TunnelAuthDecision{
		Host:         host,
		RemotePort:   remotePort,
		App:          route.app,
		Listener:     route.listener,
		ClientIP:     clientIP,
		VerifierType: tunnelAuthVerifierType(route),
		Time:         time.Now().UTC(),
	}
}

func (m *TlsMux) recordTunnelAuthDecision(decision TunnelAuthDecision) {
	if decision.Time.IsZero() {
		decision.Time = time.Now().UTC()
	}
	if m.tunnelAuthMetrics != nil {
		m.tunnelAuthMetrics.Record(decision)
	}
	m.mu.RLock()
	recorder := m.decisionRecorder
	m.mu.RUnlock()
	if recorder != nil {
		recorder(decision)
	}
}

func tunnelAuthVerifierType(route tlsMuxRoute) string {
	if route.endpoint == nil || route.endpoint.ConnectionAuth == nil || route.endpoint.ConnectionAuth.MTLS == nil {
		return ""
	}
	return route.endpoint.ConnectionAuth.MTLS.Verifier.Type
}

func (d *TunnelAuthDecision) applyVerificationResult(result TunnelClientVerificationResult) {
	d.UserID = result.UserID
	d.Username = result.Username
	d.Role = result.Role
	d.Serial = result.Serial
}

func (d *TunnelAuthDecision) applyVerificationError(err error) {
	d.DenyReason = tunnelAuthDenyReason(err)
	var vErr *TunnelClientVerificationError
	if errors.As(err, &vErr) {
		if vErr.Reason != "" {
			d.DenyReason = vErr.Reason
		}
		d.applyVerificationResult(vErr.Identity)
	}
}

func tunnelAuthDenyReason(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unsupported verifier"):
		return TunnelAuthReasonUnsupportedVerifier
	case strings.Contains(msg, "missing client certificate"):
		return TunnelAuthReasonMissingClientCertificate
	case strings.Contains(msg, "invalid client certificate"):
		return TunnelAuthReasonInvalidClientCertificate
	case strings.Contains(msg, "unknown certificate serial"):
		return TunnelAuthReasonUnknownCertificateSerial
	case strings.Contains(msg, "expired"):
		return TunnelAuthReasonCertificateExpired
	case strings.Contains(msg, "audience mismatch"):
		return TunnelAuthReasonAudienceMismatch
	case strings.Contains(msg, "source ip denied"):
		return TunnelAuthReasonSourceIPDenied
	case strings.Contains(msg, "verifier unavailable"):
		return TunnelAuthReasonVerifierUnavailable
	case strings.Contains(msg, "missing route endpoint"):
		return TunnelAuthReasonMissingRouteMetadata
	default:
		return TunnelAuthReasonVerificationFailed
	}
}

func peerClientIP(c net.Conn, hint connectionHint, haveHint bool) string {
	if haveHint {
		return hintClientIP(hint, true)
	}
	if addr, ok := c.RemoteAddr().(*net.TCPAddr); ok && addr.IP != nil {
		return addr.IP.String()
	}
	return ""
}

func hintClientIP(hint connectionHint, haveHint bool) string {
	if !haveHint {
		return ""
	}
	return normalizeClientIPHint(hint.clientIP)
}

func normalizeClientIPHint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if ip := net.ParseIP(raw); ip != nil {
		return ip.String()
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		if ip := net.ParseIP(strings.Trim(raw, "[]")); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(raw)
	if err != nil {
		return raw
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.String()
	}
	return raw
}

func (m *TlsMux) resolveUpstream(host string, remotePort int) int {
	return m.resolveRoute(host, remotePort).upstream
}

func (m *TlsMux) resolveRoute(host string, remotePort int) tlsMuxRoute {
	m.mu.RLock()
	portal := m.portalHost
	domain := m.domain
	portalPort := m.portalPort
	aliases := m.aliases
	bases := m.remoteBases
	m.mu.RUnlock()

	if host == "" {
		return tlsMuxRoute{}
	}
	if host == portal {
		return tlsMuxRoute{upstream: portalPort}
	}
	// Alias domains: route by the listener they target.
	if hostLabel, ok := aliases[host]; ok {
		if hostLabel == portalHostLabel || hostLabel == "" {
			return tlsMuxRoute{upstream: portalPort}
		}
		if m.services != nil {
			if ep, found := m.services.ResolveByHostLabel(hostLabel, remotePort); found {
				return endpointTLSMuxRoute(ep)
			}
		}
		return tlsMuxRoute{}
	}
	// <app>.<domain> or <listener>-<app>.<domain> → map to ServiceManager public_port
	// Per RFC 20260114: use DerivedHostLabel for routing (primary=<app>, others=<listener>-<app>)
	if domain != "" && strings.HasSuffix(host, "."+domain) {
		if route := m.resolveByDomainRoute(host, domain, remotePort); route.upstream != 0 {
			return route
		}
	}
	// Check additional remote bases (RFC 20260312).
	// Two passes: exact portal match first, then subdomain — ensures a more-specific
	// portal hostname is not misclassified as a subdomain of a less-specific base.
	for _, rb := range bases {
		if host == rb.PortalHost {
			return tlsMuxRoute{upstream: portalPort}
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
		if route := m.resolveByDomainRoute(host, bestDomain, remotePort); route.upstream != 0 {
			return route
		}
	}
	return tlsMuxRoute{}
}

func (m *TlsMux) resolveByDomain(host, domain string, remotePort int) int {
	return m.resolveByDomainRoute(host, domain, remotePort).upstream
}

func (m *TlsMux) resolveByDomainRoute(host, domain string, remotePort int) tlsMuxRoute {
	label := strings.TrimSuffix(host, "."+domain)
	if i := strings.Index(label, "."); i != -1 {
		label = label[:i]
	}
	if label != "" && m.services != nil {
		if ep, ok := m.services.ResolveByHostLabel(label, remotePort); ok {
			return endpointTLSMuxRoute(ep)
		}
	}
	return tlsMuxRoute{}
}

func endpointTLSMuxRoute(ep ServiceEndpoint) tlsMuxRoute {
	epCopy := ep
	return tlsMuxRoute{
		upstream:           ep.PublicPort,
		app:                ep.App,
		listener:           ep.Name,
		requiresTLSMuxAuth: ep.RequiresTLSMuxAuth,
		endpoint:           &epCopy,
	}
}

// ErrNoCert is returned by a CertProvider when no certificate is available.
var ErrNoCert = fmt.Errorf("no certificate available")

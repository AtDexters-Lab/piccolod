package server

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"piccolod/internal/hostname"
	"piccolod/internal/services"
)

func (s *GinServer) handleAuthValidateNext(c *gin.Context) {
	next := strings.TrimSpace(c.Query("next"))
	if next == "" {
		c.JSON(http.StatusOK, gin.H{"valid": false})
		return
	}
	if validated, ok := s.validateNextURL(next); ok {
		c.JSON(http.StatusOK, gin.H{"valid": true, "redirect_url": validated})
		return
	}
	c.JSON(http.StatusOK, gin.H{"valid": false})
}

func (s *GinServer) validateNextURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}

	// Relative paths
	if strings.TrimSpace(u.Scheme) == "" {
		if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
			return "", false
		}
		for _, seg := range strings.Split(u.Path, "/") {
			if seg == ".." {
				return "", false
			}
		}
		return raw, true
	}

	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme != "http" && scheme != "https" {
		return "", false
	}

	host := hostname.Normalize(u.Hostname())
	if host == "" {
		return "", false
	}

	port := 0
	if p := strings.TrimSpace(u.Port()); p != "" {
		v, err := strconv.Atoi(p)
		if err != nil || v < 1 || v > 65535 {
			return "", false
		}
		port = v
	}

	localHostname := ""
	if s.mdnsManager != nil {
		localHostname = hostname.Normalize(s.mdnsManager.Hostname())
	}

	allowedPorts := make(map[int]struct{})
	remoteHosts := make(map[string]struct{})
	localHosts := make(map[string]struct{})
	aliasHosts := make(map[string]struct{})

	if localHostname != "" {
		localHosts[localHostname] = struct{}{}
	}

	// Add all remote portal hostnames from the resolver (source-agnostic).
	portalHosts := s.portalHosts()
	for _, h := range portalHosts {
		remoteHosts[strings.ToLower(h)] = struct{}{}
	}

	if s.serviceManager != nil {
		for _, ep := range s.serviceManager.GetAll() {
			if ep.PublicPort != 0 {
				allowedPorts[ep.PublicPort] = struct{}{}
			}
			// Derive app hostnames for all portal hosts (source-agnostic).
			for _, portal := range portalHosts {
				if remoteHost := services.RemoteServiceHostname(ep.DerivedHostLabel, portal); remoteHost != "" {
					remoteHosts[strings.ToLower(remoteHost)] = struct{}{}
				}
			}
			// LAN host-based routing (future RFC) - safe allowlist entry even if not enabled yet.
			if localHostname != "" && strings.TrimSpace(ep.Name) != "" {
				localHosts[strings.ToLower(ep.Name)+"."+localHostname] = struct{}{}
			}
		}
	}

	if s.remoteResolver != nil {
		for aliasHost := range s.remoteResolver.AliasHostLabels() {
			aliasHosts[aliasHost] = struct{}{}
		}
	}

	// Remote hosts + aliases require HTTPS and default port.
	if _, ok := remoteHosts[host]; ok {
		if scheme != "https" {
			return "", false
		}
		if port != 0 && port != 443 {
			return "", false
		}
		return raw, true
	}
	if _, ok := aliasHosts[host]; ok {
		if scheme != "https" {
			return "", false
		}
		if port != 0 && port != 443 {
			return "", false
		}
		return raw, true
	}

	// LAN portal hostname and host-based app hostname (preferred): allow on http(s), default port.
	if _, ok := localHosts[host]; ok {
		if port != 0 {
			// Only allow listener ports for legacy LAN routing.
			if _, ok := allowedPorts[port]; !ok {
				return "", false
			}
		}
		return raw, true
	}

	// LAN port-based legacy: allow local hostname/IP/localhost with an allocated listener port.
	if port == 0 {
		// Host-only portal origins.
		if host == "localhost" {
			return raw, true
		}
		if ip := net.ParseIP(host); ip != nil && isLocalMachineIP(ip) {
			return raw, true
		}
		return "", false
	}
	if _, ok := allowedPorts[port]; !ok {
		return "", false
	}
	if host == "localhost" {
		return raw, true
	}
	if ip := net.ParseIP(host); ip != nil && isLocalMachineIP(ip) {
		return raw, true
	}
	if localHostname != "" && strings.EqualFold(host, localHostname) {
		return raw, true
	}

	return "", false
}

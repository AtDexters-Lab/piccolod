package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/tunnelauth"
)

type tunnelCertificateRequest struct {
	Host                string `json:"host"`
	RemotePort          int    `json:"remote_port"`
	PublicKeyPEM        string `json:"public_key_pem"`
	RequestedTTLSeconds int64  `json:"requested_ttl_seconds"`
}

func (s *GinServer) handleTunnelCertificateIssue(c *gin.Context) {
	if s == nil || s.tunnelAuth == nil || s.remoteResolver == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tunnel auth unavailable"})
		return
	}
	var req tunnelCertificateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.Host)), ".")
	remotePort := req.RemotePort
	if remotePort == 0 {
		remotePort = 443
	}
	if host == "" || remotePort <= 0 || strings.TrimSpace(req.PublicKeyPEM) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "host, remote_port, and public_key_pem are required"})
		return
	}
	ep, ok := s.remoteResolver.ResolveTunnelTarget(host, remotePort)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "tunnel target not found"})
		return
	}
	id, ok := s.getSession(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	sess, ok := s.sessions.Get(id)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := sess.UserID
	if userID == "" {
		userID = sess.User
	}
	var ttl time.Duration
	if req.RequestedTTLSeconds > 0 {
		ttl = time.Duration(req.RequestedTTLSeconds) * time.Second
	}
	resp, err := s.tunnelAuth.Issue(c.Request.Context(), tunnelauth.IssueRequest{
		Host:         host,
		RemotePort:   remotePort,
		App:          ep.App,
		Listener:     ep.Name,
		UserID:       userID,
		Username:     sess.User,
		Role:         sess.Role,
		PublicKeyPEM: req.PublicKeyPEM,
		TTL:          ttl,
	})
	if err != nil {
		switch {
		case errors.Is(err, tunnelauth.ErrInvalidRequest):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, tunnelauth.ErrUnauthorized):
			c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden"})
		case errors.Is(err, tunnelauth.ErrUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tunnel auth unavailable"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue tunnel certificate"})
		}
		return
	}
	c.JSON(http.StatusOK, resp)
}

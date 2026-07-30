package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"piccolod/internal/auth"
)

func TestIsEmergencyAllowed(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// Always allowed
		{"/api/v1/health/live", true},
		{"/api/v1/health/ready", true},
		{"/api/v1/health/detail", true},
		{"/api/v1/system/emergency", true},
		{"/api/v1/system/boot", true},
		{"/api/v1/system/diagnostic-log", true},
		{"/api/v1/system/admin/diagnostic-log", true},
		{"/api/v1/system/ca.crt", true},
		{"/api/v1/auth/session", true},
		{"/api/v1/auth/initialized", true},
		{"/api/v1/auth/login", true},
		{"/api/v1/crypto/status", true},
		{"/api/v1/network/peers", true},
		{"/version", true},

		// Static assets (non-API, non-OAuth)
		{"/", true},
		{"/index.html", true},
		{"/assets/main.js", true},

		// Blocked
		{"/api/v1/apps", false},
		{"/api/v1/crypto/setup", false},
		{"/api/v1/crypto/unlock", false},
		{"/api/v1/users", false},
		{"/oauth/authorize", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isEmergencyAllowed(tt.path); got != tt.want {
				t.Errorf("isEmergencyAllowed(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsEmergencySoftAllowed(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// Allowed in soft emergency
		{"/api/v1/crypto/unlock", true},
		{"/api/v1/crypto/reset-password", true},
		{"/api/v1/crypto/recovery-key", true},

		// Allowed: setup is idempotent and needed for partial-setup recovery
		{"/api/v1/crypto/setup", true},

		// Unrelated paths
		{"/api/v1/apps", false},
		{"/api/v1/users", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isEmergencySoftAllowed(tt.path); got != tt.want {
				t.Errorf("isEmergencySoftAllowed(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestPublicManualLockRouteRemoved(t *testing.T) {
	sessions := auth.NewSessionStore()
	session := sessions.CreatePortalSession("user-1", "admin", "admin", "http://example.com", 60)
	srv := &GinServer{sessions: sessions}
	srv.setupGinRoutes()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/crypto/lock", nil)
	req.Host = "example.com"
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session.ID})
	req.Header.Set("X-CSRF-Token", session.CSRF)

	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("POST /api/v1/crypto/lock status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

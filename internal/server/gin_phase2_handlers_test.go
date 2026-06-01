package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	apppkg "piccolod/internal/app"
	"piccolod/internal/remote"
	"strings"
	"testing"
)

func setupBasicServer(t *testing.T) *GinServer {
	t.Helper()
	tempDir := t.TempDir()
	srv := createGinTestServer(t, tempDir)
	if srv.remoteManager == nil {
		rm, err := remote.NewManager(tempDir)
		if err != nil {
			t.Fatalf("remote mgr: %v", err)
		}
		srv.remoteManager = rm
		srv.setupGinRoutes()
	}
	return srv
}

func TestOSUpdateStatus_OK(t *testing.T) {
	srv := setupBasicServer(t)
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/updates/os", nil)
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if _, ok := m["current_version"]; !ok {
		t.Fatalf("missing current_version")
	}
}

func TestOSUpdateRollbackBlocksEnabledMTLSApps(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	updater := &fakeOSUpdateMgr{}
	srv.updateManager = updater

	def, err := apppkg.ParseAppDefinition([]byte(`type: user
listeners:
  - name: __primary
    guest_port: 22
    flow: tcp
    protocol: raw
    tls_wrap: true
    connection_auth:
      mtls:
        verifier:
          type: piccolo_session
services:
  main:
    image: docker.io/library/alpine:latest
    bind_ports: [22]
x-piccolo:
  mode: service
  requires_features:
    - connection_auth_mtls_v1
`))
	if err != nil {
		t.Fatalf("parse app: %v", err)
	}
	def.Listeners[0].Name = "sshdev"
	def.Listeners[0].Primary = true
	if _, err := srv.appManager.Install(context.Background(), def); err != nil {
		t.Fatalf("install app: %v", err)
	}

	sessionCookie, csrfToken := setupTestAdminSession(t, srv)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/updates/os/rollback", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	if updater.rollbacked != 0 {
		t.Fatalf("rollback manager called %d times", updater.rollbacked)
	}
	if !strings.Contains(w.Body.String(), "sshdev") {
		t.Fatalf("response does not name blocking app: %s", w.Body.String())
	}
}

func TestRemoteStatus_RequiresAuth(t *testing.T) {
	srv := setupBasicServer(t)

	// Unauthenticated request should be rejected.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/remote/status", nil)
	srv.router.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("expected auth rejection, got 200")
	}

	// Authenticated request should succeed.
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/remote/status", nil)
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
}

// TestLANOnly_RejectsLoopback verifies that LAN-only endpoints reject
// requests from loopback (which is how the Nexus remote tunnel arrives).
func TestLANOnly_RejectsLoopback(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())

	lanOnlyPaths := []struct {
		method string
		path   string
	}{
		{"GET", "/api/v1/system/onboarding"},
		{"GET", "/api/v1/system/emergency"},
		{"GET", "/api/v1/openapi.yaml"},
		{"GET", "/api/v1/crypto/recovery-key"},
		{"POST", "/api/v1/crypto/reset-password"},
		{"POST", "/api/v1/system/pcv/import"},
	}

	for _, tc := range lanOnlyPaths {
		t.Run(tc.method+"_"+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(tc.method, tc.path, nil)
			req.RemoteAddr = "127.0.0.1:12345"
			srv.router.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Errorf("expected 403 for loopback, got %d", w.Code)
			}
		})
	}
}

// TestCryptoSetup_NonceGatedFromLoopback verifies that /crypto/setup is no
// longer LAN-only network-gated — instead, loopback (Nexus-tunneled) requests
// are required to present a valid setup nonce, which proves the requester held
// LAN access at the time of /identity/setup-hostname (where the nonce is minted).
// The nonce-as-trust-anchor is a stronger primitive than per-request LAN check
// because it works across reboots for partial-setup recovery.
func TestCryptoSetup_NonceGatedFromLoopback(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/crypto/setup",
		strings.NewReader(`{"password":"some-strong-password-123"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 from nonce check on loopback, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestStorageDisks_OK is an integration test that requires lsblk/findmnt;
// skipped in unit-test environments. The endpoint is now backed by onboarding
// disk discovery rather than the old empty-list stub.
func TestStorageDisks_OK(t *testing.T) {
	t.Skip("requires real lsblk/findmnt for disk discovery — run as integration test")
}

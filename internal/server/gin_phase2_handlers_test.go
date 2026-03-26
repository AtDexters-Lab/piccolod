package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"piccolod/internal/remote"
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
		{"POST", "/api/v1/crypto/setup"},
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

// TestStorageDisks_OK is an integration test that requires lsblk/findmnt;
// skipped in unit-test environments. The endpoint is now backed by onboarding
// disk discovery rather than the old empty-list stub.
func TestStorageDisks_OK(t *testing.T) {
	t.Skip("requires real lsblk/findmnt for disk discovery — run as integration test")
}

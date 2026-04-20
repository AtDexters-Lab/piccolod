package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBoot_FreshServer(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["screen"] != "setup" {
		t.Fatalf("expected screen=setup, got %v", resp["screen"])
	}
}

func TestBoot_AuthenticatedDesktop(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, _ := setupTestAdminSession(t, srv)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	req.AddCookie(sessionCookie)
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["screen"] != "desktop" {
		t.Fatalf("expected screen=desktop, got %v", resp["screen"])
	}
	if resp["user"] != "admin" {
		t.Fatalf("expected user=admin, got %v", resp["user"])
	}
	if _, ok := resp["has_passkey"]; !ok {
		t.Fatalf("expected has_passkey field in desktop response")
	}
	if _, ok := resp["must_register_passkey"]; !ok {
		t.Fatalf("expected must_register_passkey field in desktop response")
	}
}

func TestBoot_NoSession(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	setupTestAdminSession(t, srv) // initialize crypto+auth but don't use the session

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	// No session cookie
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["screen"] != "login" {
		t.Fatalf("expected screen=login, got %v", resp["screen"])
	}
}

func TestBoot_MustRegisterPasskey(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, _ := setupTestAdminSession(t, srv)

	// Set MustRegisterPasskey on the session
	sess, ok := srv.sessions.Get(sessionCookie.Value)
	if !ok {
		t.Fatal("session not found")
	}
	sess.MustRegisterPasskey.Store(true)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	req.AddCookie(sessionCookie)
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["screen"] != "passkey_required" {
		t.Fatalf("expected screen=passkey_required, got %v", resp["screen"])
	}
}

func TestBoot_Locked(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	setupTestAdminSession(t, srv) // initializes crypto

	// Lock crypto
	srv.cryptoManager.Lock()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/boot", nil)
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["screen"] != "unlock" {
		t.Fatalf("expected screen=unlock, got %v", resp["screen"])
	}
}

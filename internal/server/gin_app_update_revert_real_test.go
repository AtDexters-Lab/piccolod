package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAppServicesDiscovery_RealHandlers(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)

	// RFC 20260130: all apps with listeners must use __primary marker
	body := []byte(`{
		"app_definition": "type: user\nlisteners:\n  - name: __primary\n    guest_port: 80\n    flow: tcp\n    protocol: http\n    auth:\n      rules:\n        - path: \"/\"\n          type: prefix\n          strategy: public\nservices:\n  main:\n    image: alpine:3.18\n    bind_ports: [80]\nx-piccolo:\n  mode: service\n",
		"inputs": {
			"__app_address__": "demo"
		}
	}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/apps", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("install status %d body=%s", w.Code, w.Body.String())
	}

	// Global services listing should include the app listener
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/services", nil)
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("services status %d", w.Code)
	}
	var servicesResp struct {
		Services []map[string]any `json:"services"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &servicesResp); err != nil {
		t.Fatal(err)
	}
	if len(servicesResp.Services) == 0 {
		t.Fatalf("expected at least one service, got 0")
	}

	// Per-app services
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/apps/demo/services", nil)
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("app services status %d", w.Code)
	}
	var appServicesResp struct {
		Services []map[string]any `json:"services"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &appServicesResp); err != nil {
		t.Fatal(err)
	}
	if len(appServicesResp.Services) == 0 {
		t.Fatalf("expected at least one app service, got 0")
	}
}

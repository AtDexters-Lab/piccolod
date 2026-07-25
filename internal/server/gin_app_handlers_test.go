package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/api"
	"piccolod/internal/app"
	"piccolod/internal/app/catalog"
	authpkg "piccolod/internal/auth"
	"piccolod/internal/cluster"
	"piccolod/internal/container"
	crypt "piccolod/internal/crypt"
	"piccolod/internal/events"
	"piccolod/internal/health"
	"piccolod/internal/lifecycle"
	"piccolod/internal/mdns"
	"piccolod/internal/persistence"
	"piccolod/internal/provisioning"
	"piccolod/internal/remote"
	"piccolod/internal/remote/nexusclient"
	"piccolod/internal/runtime/commands"
	"piccolod/internal/services"
	"piccolod/internal/state/paths"
	"piccolod/internal/tunnelauth"

	webassets "piccolod"
)

// testLANAddr is a private LAN address used in tests to simulate a request
// arriving from the local network (as opposed to loopback, which signals the
// Nexus remote tunnel).
const testLANAddr = "192.168.1.100:54321"

func TestArtifactAwareDefinitionTimeout(t *testing.T) {
	if got := artifactAwareDefinitionTimeout(&api.AppDefinition{}, time.Minute); got != time.Minute {
		t.Fatalf("ordinary definition timeout = %s", got)
	}
	withArtifact := &api.AppDefinition{
		Artifacts: map[string]api.AppArtifact{"model": {}},
	}
	if got := artifactAwareDefinitionTimeout(withArtifact, time.Minute); got != app.ArtifactOperationTimeout {
		t.Fatalf("artifact definition timeout = %s, want %s", got, app.ArtifactOperationTimeout)
	}
}

func requireMountBypassAllowed(t *testing.T) {
	t.Helper()
	if os.Getenv("PICCOLO_ALLOW_UNMOUNTED_TESTS") != "1" {
		t.Skip("set PICCOLO_ALLOW_UNMOUNTED_TESTS=1 to run without mounted volumes")
	}
}

func ensureTestControlMetadata(t *testing.T, root string) {
	t.Helper()
	metaDir := filepath.Join(root, "volumes", "control")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir meta dir: %v", err)
	}
	meta := `{"version":1,"wrapped_key":"stub","nonce":"stub"}`
	if err := os.WriteFile(filepath.Join(metaDir, "piccolo.volume.json"), []byte(meta), 0o600); err != nil {
		t.Fatalf("write volume metadata: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "mounts", "control"), 0o700); err != nil {
		t.Fatalf("mkdir mount dir: %v", err)
	}
}

// TestGinAppAPI_Install tests POST /api/v1/apps endpoint with Gin
func TestGinAppAPI_Install(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()

	// Create test server with Gin
	server := createGinTestServer(t, tempDir)
	sessionCookie, csrfToken := setupTestAdminSession(t, server)

	tests := []struct {
		name           string
		method         string
		contentType    string
		body           string
		expectedStatus int
		expectError    bool
	}{
		{
			// RFC 20260130: All apps with listeners must use __primary marker
			// Use JSON format with inputs to provide __app_address__
			name:        "install valid nginx app",
			method:      "POST",
			contentType: "application/json",
			body: `{
				"app_definition": "type: user\nlisteners:\n  - name: __primary\n    guest_port: 80\n    flow: tcp\n    protocol: http\n    auth:\n      rules:\n        - path: \"/\"\n          type: prefix\n          strategy: public\nservices:\n  main:\n    image: docker.io/library/nginx:alpine\n    bind_ports: [80]\n    environment:\n      NGINX_HOST: localhost\n      NGINX_PORT: \"80\"\nx-piccolo:\n  mode: service",
				"inputs": {"__app_address__": "testnginx"}
			}`,
			expectedStatus: http.StatusCreated,
			expectError:    false,
		},
		{
			name:           "install with wrong content type",
			method:         "POST",
			contentType:    "text/plain",
			body:           "name: test",
			expectedStatus: http.StatusUnsupportedMediaType,
			expectError:    true,
		},
		{
			name:           "install with empty body",
			method:         "POST",
			contentType:    "application/x-yaml",
			body:           "",
			expectedStatus: http.StatusBadRequest,
			expectError:    true,
		},
		{
			name:           "install with invalid yaml",
			method:         "POST",
			contentType:    "application/x-yaml",
			body:           "invalid: yaml: content:",
			expectedStatus: http.StatusBadRequest,
			expectError:    true,
		},
		{
			name:           "wrong http method",
			method:         "PUT",
			contentType:    "application/x-yaml",
			body:           "name: test",
			expectedStatus: http.StatusNotFound, // Gin returns 404 for unregistered routes
			expectError:    false,               // 404 responses are plain text, not JSON
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()

			var req *http.Request
			if tt.body != "" {
				req, _ = http.NewRequest(tt.method, "/api/v1/apps", strings.NewReader(tt.body))
			} else {
				req, _ = http.NewRequest(tt.method, "/api/v1/apps", nil)
			}

			req.Header.Set("Content-Type", tt.contentType)
			attachAuth(req, sessionCookie, csrfToken)

			server.router.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, w.Code)
				t.Logf("Response body: %s", w.Body.String())
			}

			// Only check JSON for non-404 responses
			if w.Code != http.StatusNotFound {
				// Verify response is valid JSON
				var response GinAppResponse
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Errorf("Response is not valid JSON: %v", err)
				}

				// Check error field matches expectation
				if tt.expectError && response.Error == nil {
					t.Error("Expected error in response but got none")
				}

				if !tt.expectError && response.Error != nil {
					t.Errorf("Expected no error but got: %+v", response.Error)
				}
			}
		})
	}
}

func TestGinAppAPI_Install_JSON_WithDisplayName(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()
	server := createGinTestServer(t, tempDir)
	sessionCookie, csrfToken := setupTestAdminSession(t, server)

	// RFC 20260130: listener name is the app identity, no display_name
	payload := map[string]interface{}{
		"app_definition": `type: user
listeners:
  - name: __primary
    guest_port: 80
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/"
          type: prefix
          strategy: public
services:
  main:
    image: docker.io/library/nginx:alpine
    bind_ports: [80]
x-piccolo:
  mode: service`,
		"inputs": map[string]interface{}{
			"__app_address__": "testnginx",
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/apps", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d body=%s", http.StatusCreated, w.Code, w.Body.String())
	}

	var resp GinAppResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("response data not object: %#v", resp.Data)
	}
	// RFC 20260130: instance_id is the primary listener name
	if got := data["instance_id"]; got != "testnginx" {
		t.Fatalf("expected instance_id %q, got %#v", "testnginx", got)
	}
}

func TestGinAppAPI_CheckInstance(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()
	server := createGinTestServer(t, tempDir)
	sessionCookie, csrfToken := setupTestAdminSession(t, server)

	// RFC 20260130: check-instance only returns available, no suggested field
	check := func(id string) bool {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/apps/check-instance?id="+id, nil)
		attachAuth(req, sessionCookie, csrfToken)
		server.router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("check-instance: expected %d, got %d body=%s", http.StatusOK, w.Code, w.Body.String())
		}
		var resp struct {
			Available bool `json:"available"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode check-instance: %v", err)
		}
		return resp.Available
	}

	if !check("demo") {
		t.Fatalf("expected demo to be available")
	}

	// RFC 20260130: listener name is the app identity
	appDef := &api.AppDefinition{
		Type:      "user",
		Listeners: []api.AppListener{{Name: "demo", GuestPort: 80, Primary: true}},
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	if _, err := server.appManager.Install(context.Background(), appDef); err != nil {
		t.Fatalf("install app: %v", err)
	}

	if check("demo") {
		t.Fatalf("expected demo to be unavailable after install")
	}
}

// TestGinAppAPI_List tests GET /api/v1/apps endpoint with Gin
func TestGinAppAPI_List(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()

	// Create test server
	server := createGinTestServer(t, tempDir)
	sessionCookie, csrfToken := setupTestAdminSession(t, server)

	// Test empty list initially
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/apps", nil)
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	var response GinAppResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	// Should return empty array
	apps, ok := response.Data.([]interface{})
	if !ok {
		t.Fatalf("Expected array in response data")
	}

	if len(apps) != 0 {
		t.Errorf("Expected 0 apps, got %d", len(apps))
	}

	// RFC 20260130: listener name is the app identity
	appDef := &api.AppDefinition{
		Type:      "user",
		Listeners: []api.AppListener{{Name: "testapp", GuestPort: 80, Primary: true}},
		Services: map[string]api.AppService{
			"main": {Image: "nginx:alpine", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}

	if _, err := server.appManager.Install(context.Background(), appDef); err != nil {
		t.Fatalf("Failed to install app: %v", err)
	}

	// Test list with one app
	w = httptest.NewRecorder()
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	apps, ok = response.Data.([]interface{})
	if !ok {
		t.Fatalf("Expected array in response data")
	}

	if len(apps) != 1 {
		t.Errorf("Expected 1 app, got %d", len(apps))
	}
}

func TestGinAppServices_RemoteHost(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()
	srv := createGinTestServer(t, tempDir)
	sessionCookie, csrf := setupTestAdminSession(t, srv)

	if err := srv.remoteManager.Configure(remote.ConfigureRequest{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret-value",
		PortalHostname: "portal.example.com",
	}); err != nil {
		t.Fatalf("remote configure: %v", err)
	}
	status := srv.remoteManager.Status()
	if !status.Enabled {
		t.Fatalf("remote status not enabled: %+v", status)
	}
	if strings.TrimSpace(status.PortalHostname) == "" {
		t.Fatalf("remote status missing portal_hostname: %+v", status)
	}
	// Test hostname derivation with proper DerivedHostLabel (per RFC 20260114)
	if host := remoteHostForEndpoint(services.ServiceEndpoint{Name: "web", DerivedHostLabel: "testapp"}, []string{status.PortalHostname}); host == "" {
		t.Fatalf("remote hostname derivation failed")
	}
	srv.refreshRemoteRuntime()

	// RFC 20260130: listener name is the app identity
	_, err := srv.appManager.Install(context.Background(), &api.AppDefinition{
		Type: "user",
		Listeners: []api.AppListener{{
			Name:      "blog",
			GuestPort: 80,
			Flow:      api.FlowTCP,
			Protocol:  api.ListenerProtocolHTTP,
			Primary:   true,
		}},
		Services: map[string]api.AppService{
			"main": {Image: "docker.io/library/nginx:alpine", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	})
	if err != nil {
		t.Fatalf("install app: %v", err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/apps/blog", nil)
	attachAuth(req, sessionCookie, csrf)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp GinAppResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("response data not object: %#v", resp.Data)
	}
	rawListeners, ok := data["listeners"].([]interface{})
	if !ok || len(rawListeners) == 0 {
		t.Fatalf("expected listeners list in response: %#v", data)
	}
	first, ok := rawListeners[0].(map[string]interface{})
	if !ok {
		t.Fatalf("listener entry not object: %#v", rawListeners[0])
	}

	remoteHost, ok := first["remote_host"].(string)
	if !ok {
		t.Fatalf("expected remote_host field on service: %#v", first)
	}
	// Per RFC 20260114: primary listener gets <app>.<base> hostname where <base> is the portal hostname apex.
	if remoteHost != "blog.portal.example.com" {
		t.Fatalf("unexpected remote_host %q (expected blog.portal.example.com per RFC 20260114)", remoteHost)
	}

	rawContainers, ok := data["containers"].([]interface{})
	if !ok || len(rawContainers) == 0 {
		t.Fatalf("expected containers list in response: %#v", data)
	}
}

// TestGinAppAPI_GetApp tests GET /api/v1/apps/:name endpoint with Gin
func TestGinAppAPI_GetApp(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()

	// Create test server and install an app
	server := createGinTestServer(t, tempDir)
	sessionCookie, csrfToken := setupTestAdminSession(t, server)

	// RFC 20260130: listener name is the app identity
	appDef := &api.AppDefinition{
		Type:      "user",
		Listeners: []api.AppListener{{Name: "testapp", GuestPort: 80, Primary: true}},
		Services: map[string]api.AppService{
			"main": {Image: "nginx:alpine", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}

	if _, err := server.appManager.Install(context.Background(), appDef); err != nil {
		t.Fatalf("Failed to install app: %v", err)
	}

	tests := []struct {
		name           string
		appName        string
		expectedStatus int
		expectError    bool
	}{
		{
			name:           "get existing app",
			appName:        "testapp",
			expectedStatus: http.StatusOK,
			expectError:    false,
		},
		{
			name:           "get non-existent app",
			appName:        "nonexistent",
			expectedStatus: http.StatusNotFound,
			expectError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", "/api/v1/apps/"+tt.appName, nil)
			attachAuth(req, sessionCookie, csrfToken)
			server.router.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			var response GinAppResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Errorf("Response is not valid JSON: %v", err)
			}

			if tt.expectError && response.Error == nil {
				t.Error("Expected error in response but got none")
			}

			if !tt.expectError && response.Error != nil {
				t.Errorf("Expected no error but got: %+v", response.Error)
			}
		})
	}
}

// TestGinAppAPI_AppActions tests POST /api/v1/apps/:name/{action} endpoints with Gin
func TestGinAppAPI_AppActions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()

	// Create test server and install an app
	server := createGinTestServer(t, tempDir)
	sessionCookie, csrfToken := setupTestAdminSession(t, server)

	// RFC 20260130: listener name is the app identity
	appDef := &api.AppDefinition{
		Type:      "user",
		Listeners: []api.AppListener{{Name: "testapp", GuestPort: 80, Primary: true}},
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}

	if _, err := server.appManager.Install(context.Background(), appDef); err != nil {
		t.Fatalf("Failed to install app: %v", err)
	}

	tests := []struct {
		name           string
		method         string
		url            string
		expectedStatus int
		expectError    bool
	}{
		{
			name:           "start app",
			method:         "POST",
			url:            "/api/v1/apps/testapp/start",
			expectedStatus: http.StatusOK,
			expectError:    false,
		},
		{
			name:           "stop app",
			method:         "POST",
			url:            "/api/v1/apps/testapp/stop",
			expectedStatus: http.StatusOK,
			expectError:    false,
		},
		{
			name:           "wrong method for action",
			method:         "GET",
			url:            "/api/v1/apps/testapp/start",
			expectedStatus: http.StatusNotFound, // Gin returns 404 for unregistered routes
			expectError:    false,               // 404 responses are plain text, not JSON
		},
		{
			name:           "action on non-existent app",
			method:         "POST",
			url:            "/api/v1/apps/nonexistent/start",
			expectedStatus: http.StatusNotFound,
			expectError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(tt.method, tt.url, nil)
			attachAuth(req, sessionCookie, csrfToken)
			server.router.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, w.Code)
				t.Logf("Response body: %s", w.Body.String())
			}

			// Only check JSON for non-404 responses
			if w.Code != http.StatusNotFound {
				var response GinAppResponse
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Errorf("Response is not valid JSON: %v", err)
				}

				if tt.expectError && response.Error == nil {
					t.Error("Expected error in response but got none")
				}

				if !tt.expectError && response.Error != nil {
					t.Errorf("Expected no error but got: %+v", response.Error)
				}
			}
		})
	}
}

// TestGinAppAPI_FullLifecycle tests complete app lifecycle via Gin HTTP API
func TestGinAppAPI_FullLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()

	// Create test server
	server := createGinTestServer(t, tempDir)
	sessionCookie, csrfToken := setupTestAdminSession(t, server)

	// RFC 20260130: All apps with listeners must use __primary marker
	appJSON := `{
		"app_definition": "type: user\nlisteners:\n  - name: __primary\n    guest_port: 80\n    flow: tcp\n    protocol: http\n    auth:\n      rules:\n        - path: \"/\"\n          type: prefix\n          strategy: public\nservices:\n  main:\n    image: docker.io/library/nginx:alpine\n    bind_ports: [80]\n    environment:\n      TEST_ENV: \"lifecycle\"\nx-piccolo:\n  mode: service",
		"inputs": {"__app_address__": "lifecycletest"}
	}`

	// 1. Install app via HTTP API
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/apps", strings.NewReader(appJSON))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("Failed to install app: status %d, body: %s", w.Code, w.Body.String())
	}

	// 2. Verify app appears in list
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/apps", nil)
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Failed to list apps: status %d", w.Code)
	}

	// 3. Get specific app details
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/apps/lifecycletest", nil)
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Failed to get app details: status %d", w.Code)
	}

	// 4. Start the app
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/v1/apps/lifecycletest/start", nil)
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Failed to start app: status %d, body: %s", w.Code, w.Body.String())
	}

	// 5. Stop the app
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/v1/apps/lifecycletest/stop", nil)
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Failed to stop app: status %d", w.Code)
	}

	// 6. Uninstall the app
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/api/v1/apps/lifecycletest", nil)
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Failed to uninstall app: status %d", w.Code)
	}

	// 7. Verify app is gone
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/apps", nil)
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Failed to list apps after uninstall: status %d", w.Code)
	}

	var response GinAppResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse final response: %v", err)
	}

	apps, ok := response.Data.([]interface{})
	if !ok {
		t.Fatalf("Expected array in response data")
	}

	if len(apps) != 0 {
		t.Errorf("Expected 0 apps after full lifecycle, got %d", len(apps))
	}
}

func TestGinAppUpdatePersistentDataSnapshotBlockReturnsConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)
	def := &api.AppDefinition{
		Type:           "user",
		PrimaryService: "main",
		Listeners: []api.AppListener{{
			Name:      "persistapp",
			GuestPort: 80,
			Flow:      api.FlowTCP,
			Protocol:  api.ListenerProtocolHTTP,
			Primary:   true,
		}},
		Services: map[string]api.AppService{
			"main": {
				Image:     "alpine:3.18",
				BindPorts: []int{80},
				Storage: &api.AppStorage{Persistent: map[string]api.AppVolume{
					"data": {Container: "/data"},
				}},
			},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	if _, err := srv.appManager.Install(context.Background(), def); err != nil {
		t.Fatalf("install persistent app: %v", err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/apps/persistapp/update", nil)
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("update status = %d, want %d body=%s", w.Code, http.StatusConflict, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rollback snapshot required") {
		t.Fatalf("update body = %s, want snapshot requirement", w.Body.String())
	}
}

// TestGinAppAPI_Uninstall tests DELETE /api/v1/apps/:name endpoint with Gin
func TestGinAppAPI_Uninstall(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()

	// Create test server and install an app
	server := createGinTestServer(t, tempDir)
	sessionCookie, csrfToken := setupTestAdminSession(t, server)

	// RFC 20260130: listener name is the app identity
	appDef := &api.AppDefinition{
		Type:      "user",
		Listeners: []api.AppListener{{Name: "testapp", GuestPort: 80, Primary: true}},
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}

	if _, err := server.appManager.Install(context.Background(), appDef); err != nil {
		t.Fatalf("Failed to install app: %v", err)
	}

	// Test successful uninstall
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/apps/testapp", nil)
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	var response GinAppResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if response.Error != nil {
		t.Errorf("Expected no error but got: %+v", response.Error)
	}

	// Verify app is actually uninstalled
	apps, err := server.appManager.List(context.Background())
	if err != nil {
		t.Fatalf("Failed to list apps: %v", err)
	}

	if len(apps) != 0 {
		t.Errorf("Expected 0 apps after uninstall, got %d", len(apps))
	}

	// Test uninstall non-existent app
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/api/v1/apps/nonexistent", nil)
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status %d, got %d", http.StatusNotFound, w.Code)
	}
}

// TestInvalidRoutes tests invalid route handling with Gin
func TestInvalidRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()

	server := createGinTestServer(t, tempDir)
	sessionCookie, csrfToken := setupTestAdminSession(t, server)

	tests := []struct {
		name           string
		method         string
		url            string
		expectedStatus int
	}{
		{
			name:           "empty app name",
			method:         "GET",
			url:            "/api/v1/apps/",
			expectedStatus: http.StatusNotFound, // Trailing slash redirect disabled; expect 404
		},
		{
			name:           "too many path segments",
			method:         "POST",
			url:            "/api/v1/apps/test/start/extra",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(tt.method, tt.url, nil)
			attachAuth(req, sessionCookie, csrfToken)
			server.router.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, w.Code)
			}
		})
	}
}

type stubTestVolumeManager struct {
	root string
}

func (s *stubTestVolumeManager) EnsureVolume(ctx context.Context, req persistence.VolumeRequest) (persistence.VolumeHandle, error) {
	_ = ctx
	handle := persistence.VolumeHandle{
		ID:       req.ID,
		MountDir: filepath.Join(s.root, "mounts", req.ID),
	}
	if err := os.MkdirAll(handle.MountDir, 0o700); err != nil {
		return persistence.VolumeHandle{}, err
	}
	return handle, nil
}

func (s *stubTestVolumeManager) Attach(ctx context.Context, handle persistence.VolumeHandle, opts persistence.AttachOptions) error {
	_ = ctx
	_ = handle
	_ = opts
	return nil
}

func (s *stubTestVolumeManager) Detach(ctx context.Context, handle persistence.VolumeHandle) error {
	_ = ctx
	_ = handle
	return nil
}

func (s *stubTestVolumeManager) DestroyVolume(ctx context.Context, id string) error {
	_ = ctx
	return os.RemoveAll(filepath.Join(s.root, "mounts", id))
}

func (s *stubTestVolumeManager) RoleStream(id string) (<-chan persistence.VolumeRole, error) {
	_ = id
	ch := make(chan persistence.VolumeRole)
	close(ch)
	return ch, nil
}

func (s *stubTestVolumeManager) AttachStateOf(ctx context.Context, id string) (persistence.AttachState, error) {
	// Test stub: report the volume as attached when its mount dir was
	// created by this stub's filesystem-based EnsureVolume/Attach. Falls
	// back to Detached on probe failure.
	if _, err := os.Stat(filepath.Join(s.root, "mounts", id)); err == nil {
		return persistence.AttachStateAttached, nil
	}
	return persistence.AttachStateDetached, nil
}

func (s *stubTestVolumeManager) IsAttachedAdvisory(ctx context.Context, id string) bool {
	state, err := s.AttachStateOf(ctx, id)
	return err == nil && state == persistence.AttachStateAttached
}

// stubTestRootfsManager provides a minimal RootfsVolumeManager for server tests.
type stubTestRootfsManager struct {
	root string
}

func (s *stubTestRootfsManager) EnsureGoldenLV(_ context.Context, _ persistence.GoldenLVRequest) (string, error) {
	return "golden-test", nil
}
func (s *stubTestRootfsManager) CreateWorkspaceFromGolden(_ context.Context, req persistence.WorkspaceRootfsRequest) (persistence.RootfsHandle, error) {
	mp := filepath.Join(s.root, "rootfs", req.InstanceID)
	_ = os.MkdirAll(mp, 0o755)
	return persistence.RootfsHandle{MountPath: mp}, nil
}
func (s *stubTestRootfsManager) CreateServiceRootfs(_ context.Context, req persistence.ServiceRootfsRequest) (persistence.RootfsHandle, error) {
	vid := req.VolumeID
	if vid == "" {
		vid = persistence.ServiceRootfsVolumeID(req.InstanceID, req.ServiceName)
	}
	mp := filepath.Join(s.root, "rootfs", vid)
	_ = os.MkdirAll(mp, 0o755)
	return persistence.RootfsHandle{MountPath: mp, ReadOnly: true}, nil
}
func (s *stubTestRootfsManager) CloneWorkspace(_ context.Context, _, cloneID string, _ *persistence.IDMapConfig) (persistence.RootfsHandle, error) {
	mp := filepath.Join(s.root, "rootfs", cloneID)
	_ = os.MkdirAll(mp, 0o755)
	return persistence.RootfsHandle{MountPath: mp}, nil
}
func (s *stubTestRootfsManager) ListClones(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (s *stubTestRootfsManager) AttachRootfs(_ context.Context, volumeID string) (persistence.RootfsHandle, error) {
	mp := filepath.Join(s.root, "rootfs", volumeID)
	_ = os.MkdirAll(mp, 0o755)
	return persistence.RootfsHandle{MountPath: mp, ReadOnly: true}, nil
}
func (s *stubTestRootfsManager) DetachRootfs(_ context.Context, _ string) error  { return nil }
func (s *stubTestRootfsManager) DestroyRootfs(_ context.Context, _ string) error { return nil }
func (s *stubTestRootfsManager) GarbageCollectGoldenLVs(_ context.Context) error { return nil }
func (s *stubTestRootfsManager) ReconcileRootfsStates(_ context.Context) error   { return nil }
func (s *stubTestRootfsManager) ReadGoldenImageConfig(_ context.Context, _ string) (persistence.GoldenImageConfig, error) {
	return persistence.GoldenImageConfig{Entrypoint: []string{"/bin/sh"}}, nil
}
func (s *stubTestRootfsManager) RootfsVolumeID(mode, instanceID string) string {
	return "rootfs-" + instanceID
}
func (s *stubTestRootfsManager) RootfsExists(_ string) bool { return true }
func (s *stubTestRootfsManager) ReadRootfsImageIdentity(volumeID string) (persistence.RootfsImageIdentity, error) {
	return persistence.RootfsImageIdentity{
		VolumeID:        volumeID,
		BaseImageDigest: "sha256:mockdigest",
	}, nil
}
func (s *stubTestRootfsManager) FindGoldenByImageRef(_ string) (string, string, bool) {
	return "", "", false
}
func (s *stubTestRootfsManager) ResizeWorkspace(_ context.Context, _ string, _ int64) error {
	return nil
}
func (s *stubTestRootfsManager) ResizeApplication(_ context.Context, _ string, _ int64) error {
	return nil
}

func TestInstallInstanceIDForDefinition(t *testing.T) {
	tests := []struct {
		name    string
		def     *api.AppDefinition
		want    string
		wantErr bool
	}{
		{
			name: "primary listener",
			def: &api.AppDefinition{
				Listeners: []api.AppListener{
					{Name: "secondary"},
					{Name: "piclu", Primary: true},
				},
			},
			want: "piclu",
		},
		{
			name: "workspace without listeners",
			def: &api.AppDefinition{
				WorkspaceName: "code",
			},
			want: "code",
		},
		{
			name:    "nil definition",
			wantErr: true,
		},
		{
			name: "multiple primary listeners",
			def: &api.AppDefinition{
				Listeners: []api.AppListener{
					{Name: "one", Primary: true},
					{Name: "two", Primary: true},
				},
			},
			wantErr: true,
		},
		{
			name: "listeners without primary",
			def: &api.AppDefinition{
				Listeners: []api.AppListener{{Name: "one"}},
			},
			wantErr: true,
		},
		{
			name:    "no install identity",
			def:     &api.AppDefinition{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := installInstanceIDForDefinition(tt.def)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got id %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("installInstanceIDForDefinition() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnsureProxyOIDCClientBeforeInstall(t *testing.T) {
	ctx := context.Background()
	srv := &GinServer{
		persistence: &testOIDCPersistence{
			control: &testOIDCControl{oidcClients: newMemoryOIDCClientRepo()},
		},
	}
	def := &api.AppDefinition{
		Listeners: []api.AppListener{
			{Name: "piclu", Primary: true},
		},
	}

	appID, created, err := srv.ensureProxyOIDCClientBeforeInstall(ctx, def)
	if err != nil {
		t.Fatalf("ensureProxyOIDCClientBeforeInstall: %v", err)
	}
	if appID != "piclu" {
		t.Fatalf("appID = %q, want piclu", appID)
	}
	if !created {
		t.Fatalf("expected proxy OIDC client to be created")
	}

	clientMgr := srv.getOIDCClientManager()
	client, err := clientMgr.GetProxyClientByAppName(ctx, "piclu")
	if err != nil {
		t.Fatalf("GetProxyClientByAppName: %v", err)
	}
	if client.ID != "piccolo-piclu-proxy" {
		t.Fatalf("client ID = %q, want piccolo-piclu-proxy", client.ID)
	}
	if client.AppID != "piclu" {
		t.Fatalf("client AppID = %q, want piclu", client.AppID)
	}
	if client.Type != persistence.OIDCClientTypeProxy {
		t.Fatalf("client Type = %q, want %q", client.Type, persistence.OIDCClientTypeProxy)
	}

	appID, created, err = srv.ensureProxyOIDCClientBeforeInstall(ctx, def)
	if err != nil {
		t.Fatalf("second ensureProxyOIDCClientBeforeInstall: %v", err)
	}
	if appID != "piclu" {
		t.Fatalf("second appID = %q, want piclu", appID)
	}
	if created {
		t.Fatalf("expected second call to reuse existing proxy OIDC client")
	}
}

func TestEnsureProxyOIDCClientBeforeInstall_NoOpForPublicListener(t *testing.T) {
	def := &api.AppDefinition{
		Listeners: []api.AppListener{
			{
				Name:    "piclu",
				Primary: true,
				Auth: &api.ListenerAuth{Rules: []api.ListenerAuthRule{
					{Path: "/", Type: "prefix", Strategy: "public"},
				}},
			},
		},
	}

	appID, created, err := (&GinServer{}).ensureProxyOIDCClientBeforeInstall(context.Background(), def)
	if err != nil {
		t.Fatalf("ensureProxyOIDCClientBeforeInstall: %v", err)
	}
	if appID != "" {
		t.Fatalf("appID = %q, want empty", appID)
	}
	if created {
		t.Fatalf("expected no proxy OIDC client for public listener")
	}
}

func TestEnsureProxyOIDCClientBeforeInstall_RequiresOIDCManager(t *testing.T) {
	def := &api.AppDefinition{
		Listeners: []api.AppListener{
			{Name: "piclu", Primary: true},
		},
	}

	appID, created, err := (&GinServer{}).ensureProxyOIDCClientBeforeInstall(context.Background(), def)
	if err != errOIDCManagerUnavailable {
		t.Fatalf("error = %v, want %v", err, errOIDCManagerUnavailable)
	}
	if appID != "piclu" {
		t.Fatalf("appID = %q, want piclu", appID)
	}
	if created {
		t.Fatalf("expected no proxy OIDC client when manager is unavailable")
	}
}

func TestGinAppInstall_CleansPrecreatedProxyOIDCClientWhenAppOIDCPersistFails(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)

	repo := newMemoryOIDCClientRepo()
	repo.failAppClientCreate = true
	srv.persistence = &testOIDCPersistence{
		control: &testOIDCControl{oidcClients: repo},
	}

	payload := `{
		"app_definition": "type: user\nlisteners:\n  - name: __primary\n    guest_port: 80\n    flow: tcp\n    protocol: http\n    auth:\n      rules:\n        - path: \"/\"\n          type: prefix\n          strategy: protected\nservices:\n  main:\n    image: docker.io/library/nginx:alpine\n    bind_ports: [80]\n    oidc_client:\n      redirect_uri_paths:\n        - /callback\n      ca_mount_path: /etc/ssl/certs/piccolo-internal-ca.crt\n      env:\n        ISSUER_URL: \"{{ .System.Auth.Issuer }}\"\n        CLIENT_ID: \"{{ .System.Auth.ClientID }}\"\n        CLIENT_SECRET: \"{{ .System.Auth.ClientSecret }}\"\nx-piccolo:\n  mode: service\n",
		"inputs": {"__app_address__": "rollbackoidc"}
	}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/apps", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)

	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("install status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Failed to register OIDC client") {
		t.Fatalf("expected OIDC client registration failure, body=%s", w.Body.String())
	}
	if _, err := repo.Get(context.Background(), "piccolo-rollbackoidc-proxy"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("proxy OIDC client after rollback err=%v, want ErrNotFound", err)
	}
	if _, err := srv.appManager.Get(context.Background(), "rollbackoidc"); err == nil {
		t.Fatalf("expected app rollback to uninstall rollbackoidc")
	}
}

type memoryOIDCClientRepo struct {
	clients             map[string]persistence.OIDCClient
	failAppClientCreate bool
}

func newMemoryOIDCClientRepo() *memoryOIDCClientRepo {
	return &memoryOIDCClientRepo{clients: make(map[string]persistence.OIDCClient)}
}

func (r *memoryOIDCClientRepo) Create(_ context.Context, client persistence.OIDCClient) error {
	if client.ID == "" || client.Secret == "" || client.AppID == "" {
		return fmt.Errorf("client id, secret, and app_id required")
	}
	if r.failAppClientCreate && client.Type == persistence.OIDCClientTypeApp {
		return fmt.Errorf("forced app OIDC client create failure")
	}
	if _, ok := r.clients[client.ID]; ok {
		return fmt.Errorf("client already exists")
	}
	r.clients[client.ID] = client
	return nil
}

func (r *memoryOIDCClientRepo) Get(_ context.Context, clientID string) (persistence.OIDCClient, error) {
	client, ok := r.clients[clientID]
	if !ok {
		return persistence.OIDCClient{}, persistence.ErrNotFound
	}
	return client, nil
}

func (r *memoryOIDCClientRepo) GetByAppID(_ context.Context, appID string) (persistence.OIDCClient, error) {
	for _, client := range r.clients {
		if client.AppID == appID {
			return client, nil
		}
	}
	return persistence.OIDCClient{}, persistence.ErrNotFound
}

func (r *memoryOIDCClientRepo) Delete(_ context.Context, clientID string) error {
	if _, ok := r.clients[clientID]; !ok {
		return persistence.ErrNotFound
	}
	delete(r.clients, clientID)
	return nil
}

func (r *memoryOIDCClientRepo) DeleteByAppID(_ context.Context, appID string) error {
	deleted := false
	for id, client := range r.clients {
		if client.AppID == appID {
			delete(r.clients, id)
			deleted = true
		}
	}
	if !deleted {
		return persistence.ErrNotFound
	}
	return nil
}

func (r *memoryOIDCClientRepo) List(context.Context) ([]persistence.OIDCClient, error) {
	clients := make([]persistence.OIDCClient, 0, len(r.clients))
	for _, client := range r.clients {
		clients = append(clients, client)
	}
	return clients, nil
}

type testOIDCPersistence struct {
	control persistence.ControlStore
}

func (p *testOIDCPersistence) Control() persistence.ControlStore          { return p.control }
func (p *testOIDCPersistence) Volumes() persistence.VolumeManager         { return nil }
func (p *testOIDCPersistence) Rootfs() persistence.RootfsVolumeManager    { return nil }
func (p *testOIDCPersistence) Devices() persistence.DeviceManager         { return nil }
func (p *testOIDCPersistence) StorageAdapter() persistence.StorageAdapter { return nil }
func (p *testOIDCPersistence) Consensus() persistence.ConsensusManager    { return nil }
func (p *testOIDCPersistence) ControlVolume() persistence.VolumeHandle {
	return persistence.VolumeHandle{}
}
func (p *testOIDCPersistence) AttachAppLogs(context.Context)  {}
func (p *testOIDCPersistence) Shutdown(context.Context) error { return nil }

type testOIDCControl struct {
	oidcClients persistence.OIDCClientRepo
}

func (c *testOIDCControl) Auth() persistence.AuthRepo                              { return nil }
func (c *testOIDCControl) Remote() persistence.RemoteRepo                          { return nil }
func (c *testOIDCControl) AppState() persistence.AppStateRepo                      { return nil }
func (c *testOIDCControl) Users() persistence.UserRepo                             { return nil }
func (c *testOIDCControl) OIDCClients() persistence.OIDCClientRepo                 { return c.oidcClients }
func (c *testOIDCControl) OIDCKeys() persistence.OIDCKeyRepo                       { return nil }
func (c *testOIDCControl) OIDCAuthCodes() persistence.OIDCAuthCodeRepo             { return nil }
func (c *testOIDCControl) OIDCRefreshTokens() persistence.OIDCRefreshTokenRepo     { return nil }
func (c *testOIDCControl) OIDCConfig() persistence.OIDCConfigRepo                  { return nil }
func (c *testOIDCControl) WebAuthnCredentials() persistence.WebAuthnCredentialRepo { return nil }
func (c *testOIDCControl) InviteTokens() persistence.InviteTokenRepo               { return nil }
func (c *testOIDCControl) Close(context.Context) error                             { return nil }
func (c *testOIDCControl) Revision(context.Context) (uint64, string, error)        { return 0, "", nil }
func (c *testOIDCControl) QuickCheck(context.Context) (persistence.ControlHealthReport, error) {
	return persistence.ControlHealthReport{}, nil
}

// createGinTestServer creates a Gin test server instance with filesystem state management
func createGinTestServer(t *testing.T, tempDir string) *GinServer {
	t.Helper()
	server, _ := createGinTestServerWithContainerManager(t, tempDir)
	return server
}

func createGinTestServerWithContainerManager(
	t *testing.T,
	tempDir string,
) (*GinServer, *GinMockContainerManager) {
	t.Helper()
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	paths.SetCoreRootForTest(t, tempDir)
	t.Setenv("PICCOLO_PODMAN_RUNROOT_BASE", filepath.Join(tempDir, "run", "podman"))
	ensureTestControlMetadata(t, tempDir)
	// Create mock container manager for app manager
	mockContainer := &GinMockContainerManager{
		containers: make(map[string]*MockContainer),
		images:     make(map[string]struct{}),
		nextID:     1,
	}

	// Create filesystem app manager with service manager
	svcMgr := services.NewServiceManager()
	appMgr, err := app.NewAppManagerForTestWithServices(mockContainer, tempDir, svcMgr, nil)
	if err != nil {
		t.Fatalf("Failed to create app manager: %v", err)
	}
	requireMountBypassAllowed(t)
	appMgr.SetMountVerifier(func(string) error { return nil })
	appMgr.SetVolumeManager(&stubTestVolumeManager{root: tempDir})
	appMgr.SetRootfsManager(&stubTestRootfsManager{root: tempDir})
	// Create dummy workspace assets required by block-native install flow.
	assetsDir := filepath.Join(tempDir, "assets")
	_ = os.MkdirAll(assetsDir, 0o755)
	_ = os.WriteFile(filepath.Join(assetsDir, "boot.sh"), []byte("#!/bin/sh\n"), 0o755)
	_ = os.WriteFile(filepath.Join(assetsDir, "piccolo-startup"), []byte("#!/bin/sh\n"), 0o755)
	eventsBus := events.NewBus()
	appMgr.ObserveRuntimeEvents(eventsBus)
	eventsBus.Publish(events.Event{Topic: events.TopicLockStateChanged, Payload: events.LockStateChanged{Locked: false}})
	appMgr.ForceLockState(false)

	// Supporting managers for auth and crypto
	authMgr, err := authpkg.NewManager(tempDir)
	if err != nil {
		t.Fatalf("auth manager init: %v", err)
	}
	cryptoMgr, err := crypt.NewManager(tempDir)
	if err != nil {
		t.Fatalf("crypto manager init: %v", err)
	}

	dispatch := commands.NewDispatcher()
	dispatch.Register(persistence.CommandEnsureVolume, commands.HandlerFunc(func(ctx context.Context, cmd commands.Command) (commands.Response, error) {
		req, ok := cmd.(persistence.EnsureVolumeCommand)
		if !ok {
			return nil, fmt.Errorf("unexpected command type %T", cmd)
		}
		handle := persistence.VolumeHandle{
			ID:       req.Req.ID,
			MountDir: filepath.Join(tempDir, "mounts", req.Req.ID),
		}
		if err := os.MkdirAll(handle.MountDir, 0o700); err != nil {
			return nil, err
		}
		return persistence.EnsureVolumeResponse{Handle: handle}, nil
	}))
	dispatch.Register(persistence.CommandAttachVolume, commands.HandlerFunc(func(context.Context, commands.Command) (commands.Response, error) {
		return nil, nil
	}))
	dispatch.Register(persistence.CommandRecordLockState, commands.HandlerFunc(func(context.Context, commands.Command) (commands.Response, error) {
		return nil, nil
	}))

	// Create minimal server instance for testing
	rm, err := remote.NewManager(tempDir)
	if err != nil {
		t.Fatalf("remote mgr: %v", err)
	}
	t.Cleanup(func() {
		_ = rm.Close()
	})
	rm.SetNexusAdapter(nexusclient.NewStub())
	remote.RegisterHandlers(dispatch, rm)
	tlsMux := services.NewTlsMux(svcMgr)
	tunnelAuth := tunnelauth.New(filepath.Join(tempDir, "mounts", "control", "tunnel-auth"))
	tlsMux.SetTunnelClientVerifier(tunnelAuth)
	remoteResolver := newServiceRemoteResolver(svcMgr)

	// Catalog manager stub (avoid outbound network calls).
	catalogHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.yaml":
			w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
			_, _ = io.WriteString(w, `apps:
  - name: wordpress
    path: wordpress/app.yaml
    description: test
    category: test
`)
		case "/wordpress/app.yaml":
			// RFC 20260130: use __primary marker for catalog apps
			w.Header().Set("Content-Type", "application/x-yaml; charset=utf-8")
			_, _ = io.WriteString(w, `type: user
listeners:
  - name: __primary
    guest_port: 80
    flow: tcp
    protocol: http
services:
  main:
    image: docker.io/library/nginx:alpine
    bind_ports: [80]
x-piccolo:
  mode: service
`)
		default:
			http.NotFound(w, r)
		}
	})
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("catalog stub listen: %v", err)
	}
	catalogStub := &httptest.Server{
		Listener: ln,
		Config:   &http.Server{Handler: catalogHandler},
	}
	catalogStub.Start()
	t.Cleanup(catalogStub.Close)
	catalogMgr := catalog.NewManager(catalogStub.URL, filepath.Join(tempDir, "catalog-cache"))

	server := &GinServer{
		appManager:        appMgr,
		serviceManager:    svcMgr,
		mdnsManager:       mdns.NewManager(),
		dispatcher:        dispatch,
		remoteManager:     rm,
		catalogManager:    catalogMgr,
		authManager:       authMgr,
		sessions:          authpkg.NewSessionStore(),
		cryptoManager:     cryptoMgr,
		version:           "test-gin",
		healthTracker:     health.NewTracker(),
		tlsMux:            tlsMux,
		tunnelAuth:        tunnelAuth,
		remoteResolver:    remoteResolver,
		provisioningState: provisioning.New(nil),
		lifecycle:         lifecycle.New(initialLifecycleState(cryptoMgr)),
	}
	server.events = eventsBus
	server.healthTracker.Setf("app-manager", health.LevelOK, "test app manager ready")
	server.healthTracker.Setf("service-manager", health.LevelOK, "test service manager ready")
	server.healthTracker.Setf("mdns", health.LevelOK, "mdns stub")
	server.healthTracker.Setf("remote", health.LevelOK, "remote stub")
	server.healthTracker.Setf("persistence", health.LevelOK, "stub persistence ready")
	server.registerUnlockReloader(rm)
	server.observeRemoteConfig(eventsBus)
	rm.SetEventsBus(eventsBus)

	// Setup Gin routes
	server.staticCache = newStaticAssetCache(webassets.FS, "web")
	server.setupGinRoutes()
	if err := server.initSecureLoopback(); err != nil {
		t.Fatalf("secure loopback init: %v", err)
	}
	server.refreshRemoteRuntime()
	t.Cleanup(func() {
		if server.tlsMux != nil {
			server.tlsMux.Stop()
		}
		if server.serviceManager != nil && server.serviceManager.ProxyManager() != nil {
			server.serviceManager.ProxyManager().StopAll()
		}
	})

	return server, mockContainer
}

func TestLeadership_FollowerStopsApp(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrf := setupTestAdminSession(t, srv)

	// RFC 20260130: all apps with listeners must use __primary marker
	payload := `{
		"app_definition": "type: user\nlisteners:\n  - name: __primary\n    guest_port: 80\n    flow: tcp\n    protocol: http\n    auth:\n      rules:\n        - path: \"/\"\n          type: prefix\n          strategy: public\nservices:\n  main:\n    image: docker.io/library/nginx:alpine\n    bind_ports: [80]\nx-piccolo:\n  mode: service\n",
		"inputs": {"__app_address__": "blog"}
	}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/apps", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrf)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("install status=%d body=%s", w.Code, w.Body.String())
	}

	// Wait for the app to start (at least one container running) so the follower transition is meaningful.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		statuses, err := srv.appManager.ContainerStatuses(context.Background(), "blog")
		if err == nil {
			for _, st := range statuses {
				if st.Running {
					goto started
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected at least one blog container to be running after install")

started:
	// Publish follower role for this app
	srv.events.Publish(events.Event{Topic: events.TopicLeadershipRoleChanged, Payload: events.LeadershipChanged{Resource: cluster.ResourceForApp("blog"), Role: cluster.RoleFollower}})

	// Wait briefly for goroutine to act
	deadline = time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		statuses, err := srv.appManager.ContainerStatuses(context.Background(), "blog")
		if err == nil {
			anyRunning := false
			for _, st := range statuses {
				if st.Running {
					anyRunning = true
					break
				}
			}
			if !anyRunning {
				// Ensure proxies are also torn down.
				if _, err := srv.serviceManager.GetByApp("blog"); err == nil {
					time.Sleep(5 * time.Millisecond)
					continue
				}
				app, _ := srv.appManager.Get(context.Background(), "blog")
				// Per intent vs observed state ideology:
				// - Status reflects local container state (stopped on this machine)
				// - Enabled reflects user intent (remains true for restart when becoming leader)
				if app.Status != "stopped" {
					t.Fatalf("expected observed status to be stopped after follower transition, got %v", app.Status)
				}
				if !app.Enabled {
					t.Fatalf("expected Enabled intent to remain true after follower transition")
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	app, _ := srv.appManager.Get(context.Background(), "blog")
	t.Fatalf("expected follower transition to stop containers, got status=%v", app.Status)
}

// setupTestAdminSession provisions the admin password and returns session cookie/CSRF token.
func setupTestAdminSession(t *testing.T, server *GinServer) (*http.Cookie, string) {
	t.Helper()
	const password = "TestPass123!"

	// Use /crypto/setup which atomically sets up crypto, auth, and admin user.
	// Set a LAN RemoteAddr — crypto/setup is LAN-only.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/crypto/setup", strings.NewReader(fmt.Sprintf(`{"password":"%s"}`, password)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = testLANAddr
	server.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		// Allow already-initialized if tests re-use the helper on same server
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "already") {
			t.Fatalf("crypto setup failed: status=%d body=%s", w.Code, w.Body.String())
		}
		// If already initialized, login to get session
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(fmt.Sprintf(`{"username":"admin","password":"%s"}`, password)))
		req.Header.Set("Content-Type", "application/json")
		server.router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("auth login failed: status=%d body=%s", w.Code, w.Body.String())
		}
	}
	// crypto/setup returns session cookie directly
	var sessionCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatalf("missing session cookie in response")
	}

	// Fetch CSRF token
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/auth/csrf", nil)
	req.AddCookie(sessionCookie)
	server.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("csrf fetch failed: status=%d body=%s", w.Code, w.Body.String())
	}
	var csrfResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &csrfResp); err != nil {
		t.Fatalf("parse csrf response: %v", err)
	}
	if csrfResp.Token == "" {
		t.Fatalf("csrf token empty")
	}

	return sessionCookie, csrfResp.Token
}

// attachAuth applies session cookie and CSRF header when required for the request.
func attachAuth(req *http.Request, cookie *http.Cookie, csrfToken string) {
	if cookie != nil {
		req.AddCookie(cookie)
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return
	}
	if csrfToken != "" {
		req.Header.Set("X-CSRF-Token", csrfToken)
	}
}

// MockContainer represents a mock container for testing
type MockContainer struct {
	ID     string
	Name   string
	Image  string
	Status string
	Spec   container.ContainerCreateSpec
}

// generateMockContainerID generates a mock container ID for testing
func generateMockContainerID(id int) string {
	return fmt.Sprintf("mock-container-%d", id)
}

// GinMockContainerManager implements the ContainerManager interface for Gin testing
type GinMockContainerManager struct {
	mu          sync.RWMutex
	containers  map[string]*MockContainer
	images      map[string]struct{}
	nextID      int
	createError error
	startError  error
	stopError   error
	removeError error
}

func (m *GinMockContainerManager) CreateContainer(ctx context.Context, runtime container.PodmanRuntime, spec container.ContainerCreateSpec) (string, error) {
	_ = runtime
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createError != nil {
		return "", m.createError
	}

	// Initialize containers map if nil (safety check)
	if m.containers == nil {
		m.containers = make(map[string]*MockContainer)
	}

	containerID := generateMockContainerID(m.nextID)
	m.nextID++

	m.containers[containerID] = &MockContainer{
		ID:     containerID,
		Name:   spec.Name,
		Image:  spec.Image,
		Status: "created",
		Spec:   spec,
	}

	return containerID, nil
}

func (m *GinMockContainerManager) StartContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error {
	_ = runtime
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.startError != nil {
		return m.startError
	}

	if container, exists := m.containers[containerID]; exists {
		container.Status = "running"
		return nil
	}
	return container.ErrContainerNotFound(containerID)
}

func (m *GinMockContainerManager) StopContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error {
	_ = runtime
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopError != nil {
		return m.stopError
	}

	if container, exists := m.containers[containerID]; exists {
		container.Status = "stopped"
		return nil
	}
	return container.ErrContainerNotFound(containerID)
}

func (m *GinMockContainerManager) RemoveContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error {
	_ = runtime
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.removeError != nil {
		return m.removeError
	}

	if _, exists := m.containers[containerID]; exists {
		delete(m.containers, containerID)
		return nil
	}
	return container.ErrContainerNotFound(containerID)
}

func (m *GinMockContainerManager) ListContainersByLabel(ctx context.Context, runtime container.PodmanRuntime, labelKey, labelValue string) ([]container.ContainerListItem, error) {
	_ = ctx
	_ = runtime
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []container.ContainerListItem{}
	for id, c := range m.containers {
		if c == nil || c.Spec.Labels == nil {
			continue
		}
		if c.Spec.Labels[labelKey] == labelValue {
			out = append(out, container.ContainerListItem{ID: id, Name: c.Name})
		}
	}
	return out, nil
}

func (m *GinMockContainerManager) PullImage(ctx context.Context, runtime container.PodmanRuntime, image string) error {
	_ = runtime
	_ = image
	return nil
}

func (m *GinMockContainerManager) PullImageWithProgress(ctx context.Context, runtime container.PodmanRuntime, image string, callback container.ImagePullCallback) error {
	_ = runtime
	_ = image
	if callback != nil {
		callback(container.ImagePullReport{
			Image:          image,
			OverallPercent: 100,
			Phase:          "complete",
		})
	}
	return nil
}

func (m *GinMockContainerManager) ResetStorage(ctx context.Context, runtime container.PodmanRuntime) error {
	_ = ctx
	_ = runtime
	m.mu.Lock()
	defer m.mu.Unlock()
	m.containers = make(map[string]*MockContainer)
	m.images = make(map[string]struct{})
	return nil
}

func (m *GinMockContainerManager) ValidateAndRepairStorage(ctx context.Context, runtime container.PodmanRuntime) (bool, error) {
	_ = ctx
	_ = runtime
	return false, nil
}

func (m *GinMockContainerManager) CommitContainer(ctx context.Context, runtime container.PodmanRuntime, containerID, imageName string) error {
	_ = ctx
	_ = runtime
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.containers[containerID]; !ok {
		return container.ErrContainerNotFound(containerID)
	}
	if m.images == nil {
		m.images = make(map[string]struct{})
	}
	m.images[imageName] = struct{}{}
	return nil
}

func (m *GinMockContainerManager) ImageExists(ctx context.Context, runtime container.PodmanRuntime, imageName string) (bool, error) {
	_ = ctx
	_ = runtime
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.images == nil {
		return false, nil
	}
	_, ok := m.images[imageName]
	return ok, nil
}

func (m *GinMockContainerManager) RemoveImage(ctx context.Context, runtime container.PodmanRuntime, imageName string) error {
	_ = ctx
	_ = runtime
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.images == nil {
		return nil
	}
	delete(m.images, imageName)
	return nil
}

func (m *GinMockContainerManager) Logs(ctx context.Context, runtime container.PodmanRuntime, containerID string, lines int) ([]string, error) {
	_ = runtime
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.containers[containerID]; !ok {
		return nil, container.ErrContainerNotFound(containerID)
	}
	if lines <= 0 {
		lines = 2
	}
	out := []string{}
	for i := 0; i < lines; i++ {
		out = append(out, "demo log entry")
	}
	return out, nil
}

func (m *GinMockContainerManager) LogsStream(ctx context.Context, runtime container.PodmanRuntime, containerID string, lines int, timestamps bool) (io.ReadCloser, error) {
	_ = ctx
	_ = runtime
	_ = timestamps
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.containers[containerID]; !ok {
		return nil, container.ErrContainerNotFound(containerID)
	}
	if lines <= 0 {
		lines = 2
	}
	var b strings.Builder
	for i := 0; i < lines; i++ {
		b.WriteString("demo log entry\n")
	}
	return io.NopCloser(strings.NewReader(b.String())), nil
}

func (m *GinMockContainerManager) ResolveContainerIDByName(ctx context.Context, runtime container.PodmanRuntime, name string) (string, error) {
	_ = ctx
	_ = runtime
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, c := range m.containers {
		if c.Name == name {
			return id, nil
		}
	}
	return "", container.ErrContainerNotFound(name)
}

func (m *GinMockContainerManager) InspectContainerState(ctx context.Context, runtime container.PodmanRuntime, containerID string) (container.ContainerState, error) {
	_ = ctx
	_ = runtime
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.containers[containerID]
	if !ok {
		return container.ContainerState{Exists: false, Running: false}, nil
	}
	return container.ContainerState{Exists: true, Running: c.Status == "running"}, nil
}

// NOTE: duplicated in internal/app/mock_container_test.go (cross-package).
func (m *GinMockContainerManager) InspectPublishedPorts(ctx context.Context, runtime container.PodmanRuntime, containerID string) (map[string]int, error) {
	_ = ctx
	_ = runtime
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.containers[containerID]
	if !ok {
		return nil, container.ErrContainerNotFound(containerID)
	}
	out := make(map[string]int, len(c.Spec.Ports))
	for _, p := range c.Spec.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		key := fmt.Sprintf("%d/%s", p.Container, proto)
		out[key] = p.Host
	}
	return out, nil
}

func (m *GinMockContainerManager) UpdatePublishAdd(ctx context.Context, runtime container.PodmanRuntime, containerID string, hostBind, guestPort int) error {
	_ = ctx
	_ = runtime
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.containers[containerID]
	if !ok {
		return container.ErrContainerNotFound(containerID)
	}
	c.Spec.Ports = append(c.Spec.Ports, container.PortMapping{Host: hostBind, Container: guestPort})
	return nil
}

func (m *GinMockContainerManager) UpdatePublishRemove(ctx context.Context, runtime container.PodmanRuntime, containerID string, hostBind, guestPort int) error {
	_ = ctx
	_ = runtime
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.containers[containerID]
	if !ok {
		return container.ErrContainerNotFound(containerID)
	}
	out := make([]container.PortMapping, 0, len(c.Spec.Ports))
	for _, p := range c.Spec.Ports {
		if p.Host == hostBind && p.Container == guestPort {
			continue
		}
		out = append(out, p)
	}
	c.Spec.Ports = out
	return nil
}

func (m *GinMockContainerManager) InspectImage(ctx context.Context, runtime container.PodmanRuntime, imageName string) (*container.ImageConfig, error) {
	_ = ctx
	_ = runtime
	_ = imageName
	// Mock: return a typical image config with shell defaults
	return &container.ImageConfig{
		Entrypoint:  nil,
		Cmd:         []string{"/bin/sh"},
		Digest:      "sha256:mockdigest",
		RepoDigests: []string{imageName + "@sha256:mockdigest"},
		Size:        500 << 20,
	}, nil
}

func (m *GinMockContainerManager) SearchRegistry(ctx context.Context, runtime container.PodmanRuntime, query string, limit int) ([]container.ImageSearchResult, error) {
	_ = ctx
	_ = runtime
	_ = limit
	// Mock: return some example results based on query
	return []container.ImageSearchResult{
		{Index: "docker.io", Name: "library/" + query, Description: "Mock image", Stars: 100, Official: "[OK]"},
	}, nil
}

func (m *GinMockContainerManager) NetworkReload(ctx context.Context, runtime container.PodmanRuntime, containerNameOrID string) error {
	_ = ctx
	_ = runtime
	_ = containerNameOrID
	return nil
}

func (m *GinMockContainerManager) ExecShellCmd(runtime container.PodmanRuntime, containerID string) (*exec.Cmd, error) {
	_ = runtime
	// Mock: return a simple echo command for testing purposes
	return exec.Command("echo", "mock shell"), nil
}

func (m *GinMockContainerManager) ExecScript(ctx context.Context, runtime container.PodmanRuntime, containerID string, opts container.ExecScriptOptions) (int, string, error) {
	return 0, "mock exec script", nil
}

func TestServicesLocalURLGeneration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tempDir := t.TempDir()
	server := createGinTestServer(t, tempDir)
	sessionCookie, csrfToken := setupTestAdminSession(t, server)

	// RFC 20260130: listener name is the app identity
	appDef := &api.AppDefinition{
		Type:      "user",
		Listeners: []api.AppListener{{Name: "urltest", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true}},
		Services: map[string]api.AppService{
			"main": {Image: "alpine", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	_, err := server.appManager.Install(context.Background(), appDef)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	// Fetch endpoint to get allocated port
	eps, err := server.serviceManager.GetByApp("urltest")
	if err != nil || len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %v err=%v", eps, err)
	}
	ep := eps[0]

	// 1. Request with standard host
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/apps/urltest", nil)
	req.Host = "piccolo.local"
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp GinAppResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp.Data.(map[string]interface{})
	lis := data["listeners"].([]interface{})
	li := lis[0].(map[string]interface{})

	expected := fmt.Sprintf("http://piccolo.local:%d", ep.PublicPort)
	if got := li["local_url"].(string); got != expected {
		t.Errorf("Host=piccolo.local: expected %q, got %q", expected, got)
	}
	if containers, ok := data["containers"].([]interface{}); !ok || len(containers) == 0 {
		t.Fatalf("expected containers list in response: %#v", data)
	}

	// 2. Request with IP host
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/apps/urltest", nil)
	req.Host = "192.168.1.50:8080"
	attachAuth(req, sessionCookie, csrfToken)
	server.router.ServeHTTP(w, req)

	// Since we use the request host (minus port) for construction
	expected = fmt.Sprintf("http://192.168.1.50:%d", ep.PublicPort)
	json.Unmarshal(w.Body.Bytes(), &resp)
	data = resp.Data.(map[string]interface{})
	li = data["listeners"].([]interface{})[0].(map[string]interface{})
	if got := li["local_url"].(string); got != expected {
		t.Errorf("Host=192.168.1.50:8080: expected %q, got %q", expected, got)
	}
	if containers, ok := data["containers"].([]interface{}); !ok || len(containers) == 0 {
		t.Fatalf("expected containers list in response: %#v", data)
	}
}

func TestHandleGinAppClone_MissingName(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrf := setupTestAdminSession(t, srv)

	tests := []struct {
		name           string
		body           string
		expectedStatus int
	}{
		{"empty body", `{}`, http.StatusBadRequest},
		{"blank name", `{"name":""}`, http.StatusBadRequest},
		{"whitespace name", `{"name":"  "}`, http.StatusBadRequest},
		{"invalid json", `{bad`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/api/v1/apps/origin/clone", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			attachAuth(req, sessionCookie, csrf)
			srv.router.ServeHTTP(w, req)
			if w.Code != tt.expectedStatus {
				t.Errorf("status=%d, want %d; body=%s", w.Code, tt.expectedStatus, w.Body.String())
			}
		})
	}
}

func TestHandleGinAppClone_OriginNotFound(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrf := setupTestAdminSession(t, srv)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/apps/noexist/clone", strings.NewReader(`{"name":"myclone"}`))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrf)
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d, want %d; body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandleGinAppListClones_OriginNotFound(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, _ := setupTestAdminSession(t, srv)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/apps/noexist/clones", nil)
	attachAuth(req, sessionCookie, "")
	srv.router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d, want %d; body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

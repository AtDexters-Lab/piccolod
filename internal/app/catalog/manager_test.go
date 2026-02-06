package catalog

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		name    string
		ip      string
		blocked bool
	}{
		{"loopback_v4", "127.0.0.1", true},
		{"loopback_v4_other", "127.0.0.2", true},
		{"loopback_v6", "::1", true},
		{"private_10", "10.0.0.1", true},
		{"private_172", "172.16.0.1", true},
		{"private_192", "192.168.1.1", true},
		{"link_local", "169.254.1.1", true},
		{"unspecified_v4", "0.0.0.0", true},
		{"unspecified_v6", "::", true},
		{"public_google", "8.8.8.8", false},
		{"public_cloudflare", "1.1.1.1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("failed to parse IP: %s", tt.ip)
			}
			got := isBlockedIP(ip)
			if got != tt.blocked {
				t.Errorf("isBlockedIP(%s) = %v, want %v", tt.ip, got, tt.blocked)
			}
		})
	}
}

func TestGetIconByName_ValidPNG(t *testing.T) {
	pngData := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A} // PNG magic header

	// Use a variable to hold the icon server URL
	var iconServerURL string

	iconServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/icon.png" {
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngData)
			return
		}
		http.NotFound(w, r)
	}))
	defer iconServer.Close()
	iconServerURL = iconServer.URL

	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			w.Write([]byte(`apps:
  - name: testapp
    icon: ` + iconServerURL + `/icon.png
    description: Test app
`))
			return
		}
		http.NotFound(w, r)
	}))
	defer catalogServer.Close()

	tmpDir := t.TempDir()
	m := NewManager(catalogServer.URL, tmpDir)
	// Override the icon client to allow localhost for testing
	m.iconHTTPClient = &http.Client{Timeout: 10 * time.Second}

	ctx := context.Background()
	result, err := m.GetIconByName(ctx, "testapp")
	if err != nil {
		t.Fatalf("GetIconByName() error = %v", err)
	}

	if result.ContentType != "image/png" {
		t.Errorf("ContentType = %q, want image/png", result.ContentType)
	}
	if len(result.Data) != len(pngData) {
		t.Errorf("Data length = %d, want %d", len(result.Data), len(pngData))
	}
}

func TestGetIconByName_ValidSVG(t *testing.T) {
	svgData := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><circle cx="50" cy="50" r="40"/></svg>`)

	var iconServerURL string

	iconServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/icon.svg" {
			w.Header().Set("Content-Type", "image/svg+xml")
			w.Write(svgData)
			return
		}
		http.NotFound(w, r)
	}))
	defer iconServer.Close()
	iconServerURL = iconServer.URL

	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			w.Write([]byte(`apps:
  - name: testsvg
    icon: ` + iconServerURL + `/icon.svg
    description: Test SVG app
`))
			return
		}
		http.NotFound(w, r)
	}))
	defer catalogServer.Close()

	tmpDir := t.TempDir()
	m := NewManager(catalogServer.URL, tmpDir)
	m.iconHTTPClient = &http.Client{Timeout: 10 * time.Second}

	ctx := context.Background()
	result, err := m.GetIconByName(ctx, "testsvg")
	if err != nil {
		t.Fatalf("GetIconByName() error = %v", err)
	}

	if result.ContentType != "image/svg+xml" {
		t.Errorf("ContentType = %q, want image/svg+xml", result.ContentType)
	}
	if string(result.Data) != string(svgData) {
		t.Errorf("Data mismatch")
	}
}

func TestGetIconByName_AppNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			w.Write([]byte(`apps:
  - name: existingapp
    description: Existing app
`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	tmpDir := t.TempDir()
	m := NewManager(ts.URL, tmpDir)

	ctx := context.Background()
	_, err := m.GetIconByName(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent app")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want 'not found'", err)
	}
}

func TestGetIconByName_NoIconURL(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			w.Write([]byte(`apps:
  - name: noicon
    description: App without icon
`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	tmpDir := t.TempDir()
	m := NewManager(ts.URL, tmpDir)

	ctx := context.Background()
	_, err := m.GetIconByName(ctx, "noicon")
	if err == nil {
		t.Fatal("expected error for app without icon")
	}
	if !strings.Contains(err.Error(), "no icon URL") {
		t.Errorf("error = %v, want 'no icon URL'", err)
	}
}

func TestGetIconByName_OversizedResponse(t *testing.T) {
	// Create data larger than IconMaxSize (1MB)
	largeData := make([]byte, IconMaxSize+1000)

	var iconServerURL string

	iconServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/large.png" {
			w.Header().Set("Content-Type", "image/png")
			w.Write(largeData)
			return
		}
		http.NotFound(w, r)
	}))
	defer iconServer.Close()
	iconServerURL = iconServer.URL

	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			w.Write([]byte(`apps:
  - name: largeicon
    icon: ` + iconServerURL + `/large.png
    description: App with large icon
`))
			return
		}
		http.NotFound(w, r)
	}))
	defer catalogServer.Close()

	tmpDir := t.TempDir()
	m := NewManager(catalogServer.URL, tmpDir)
	m.iconHTTPClient = &http.Client{Timeout: 10 * time.Second}

	ctx := context.Background()
	_, err := m.GetIconByName(ctx, "largeicon")
	if err == nil {
		t.Fatal("expected error for oversized icon")
	}
	if !strings.Contains(err.Error(), "maximum size") {
		t.Errorf("error = %v, want 'maximum size'", err)
	}
}

func TestGetIconByName_InvalidContentType(t *testing.T) {
	var iconServerURL string

	iconServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/text.txt" {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("not an image"))
			return
		}
		http.NotFound(w, r)
	}))
	defer iconServer.Close()
	iconServerURL = iconServer.URL

	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			w.Write([]byte(`apps:
  - name: texticon
    icon: ` + iconServerURL + `/text.txt
    description: App with text file as icon
`))
			return
		}
		http.NotFound(w, r)
	}))
	defer catalogServer.Close()

	tmpDir := t.TempDir()
	m := NewManager(catalogServer.URL, tmpDir)
	m.iconHTTPClient = &http.Client{Timeout: 10 * time.Second}

	ctx := context.Background()
	_, err := m.GetIconByName(ctx, "texticon")
	if err == nil {
		t.Fatal("expected error for invalid content type")
	}
	if !strings.Contains(err.Error(), "MIME type") {
		t.Errorf("error = %v, want 'MIME type'", err)
	}
}

func TestGetIconByName_CacheHit(t *testing.T) {
	var fetchCount int32
	pngData := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

	var iconServerURL string

	iconServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/icon.png" {
			atomic.AddInt32(&fetchCount, 1)
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngData)
			return
		}
		http.NotFound(w, r)
	}))
	defer iconServer.Close()
	iconServerURL = iconServer.URL

	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			w.Write([]byte(`apps:
  - name: cachedapp
    icon: ` + iconServerURL + `/icon.png
    description: App for cache test
`))
			return
		}
		http.NotFound(w, r)
	}))
	defer catalogServer.Close()

	tmpDir := t.TempDir()
	m := NewManager(catalogServer.URL, tmpDir)
	m.iconHTTPClient = &http.Client{Timeout: 10 * time.Second}

	ctx := context.Background()

	// First fetch - should hit network
	_, err := m.GetIconByName(ctx, "cachedapp")
	if err != nil {
		t.Fatalf("first GetIconByName() error = %v", err)
	}
	if atomic.LoadInt32(&fetchCount) != 1 {
		t.Errorf("fetchCount = %d, want 1", fetchCount)
	}

	// Second fetch - should hit cache
	result2, err := m.GetIconByName(ctx, "cachedapp")
	if err != nil {
		t.Fatalf("second GetIconByName() error = %v", err)
	}
	if atomic.LoadInt32(&fetchCount) != 1 {
		t.Errorf("fetchCount = %d, want 1 (cache should have been used)", fetchCount)
	}
	if result2.ContentType != "image/png" {
		t.Errorf("cached ContentType = %q, want image/png", result2.ContentType)
	}
}

func TestGetIconByName_CacheExpiry(t *testing.T) {
	var fetchCount int32
	pngData := []byte{0x89, 'P', 'N', 'G'}

	var iconServerURL string

	iconServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/icon.png" {
			atomic.AddInt32(&fetchCount, 1)
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngData)
			return
		}
		http.NotFound(w, r)
	}))
	defer iconServer.Close()
	iconServerURL = iconServer.URL

	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			w.Write([]byte(`apps:
  - name: expiredapp
    icon: ` + iconServerURL + `/icon.png
    description: App for expiry test
`))
			return
		}
		http.NotFound(w, r)
	}))
	defer catalogServer.Close()

	tmpDir := t.TempDir()
	m := NewManager(catalogServer.URL, tmpDir)
	m.iconHTTPClient = &http.Client{Timeout: 10 * time.Second}

	ctx := context.Background()

	// First fetch
	_, err := m.GetIconByName(ctx, "expiredapp")
	if err != nil {
		t.Fatalf("first GetIconByName() error = %v", err)
	}
	if atomic.LoadInt32(&fetchCount) != 1 {
		t.Errorf("fetchCount = %d, want 1", fetchCount)
	}

	// Manually expire the cache by modifying the meta file
	metaPath := filepath.Join(tmpDir, "icons", "expiredapp.meta")
	oldMeta := `{"content_type":"image/png","fetched_at":"2000-01-01T00:00:00Z"}`
	if err := os.WriteFile(metaPath, []byte(oldMeta), 0644); err != nil {
		t.Fatalf("failed to modify meta file: %v", err)
	}

	// Third fetch - should hit network due to expired cache
	_, err = m.GetIconByName(ctx, "expiredapp")
	if err != nil {
		t.Fatalf("third GetIconByName() error = %v", err)
	}
	if atomic.LoadInt32(&fetchCount) != 2 {
		t.Errorf("fetchCount = %d, want 2 (cache should have been expired)", fetchCount)
	}
}

func TestSSRFBlocked_Loopback(t *testing.T) {
	// Create a catalog that points to a loopback icon URL
	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			w.Write([]byte(`apps:
  - name: ssrftest
    icon: http://127.0.0.1:9999/secret
    description: App with loopback icon URL
`))
			return
		}
		http.NotFound(w, r)
	}))
	defer catalogServer.Close()

	tmpDir := t.TempDir()
	m := NewManager(catalogServer.URL, tmpDir)
	// Use the default SSRF-safe client (don't override)

	ctx := context.Background()
	_, err := m.GetIconByName(ctx, "ssrftest")
	if err == nil {
		t.Fatal("expected SSRF error for loopback URL")
	}
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("expected ErrSSRFBlocked, got: %v", err)
	}
}

func TestSanitizeAppNameForCache(t *testing.T) {
	tests := []struct {
		name    string
		appName string
		wantErr bool
	}{
		{"valid_simple", "myapp", false},
		{"valid_with_hyphen", "my-app", false},
		{"valid_with_underscore", "my_app", false},
		{"valid_with_period", "my.app", false},
		{"valid_with_numbers", "app123", false},
		{"invalid_path_traversal", "../etc/passwd", true},
		{"invalid_path_traversal2", "..%2F..%2Fetc", true},
		{"invalid_hidden_file", ".hidden", true},
		{"invalid_double_dot", "app..name", true},
		{"invalid_slash", "app/name", true},
		{"invalid_backslash", "app\\name", true},
		{"invalid_space", "app name", true},
		{"invalid_empty", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sanitizeAppNameForCache(tt.appName)
			if (err != nil) != tt.wantErr {
				t.Errorf("sanitizeAppNameForCache(%q) error = %v, wantErr %v", tt.appName, err, tt.wantErr)
			}
		})
	}
}

package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	webassets "piccolod"

	"github.com/gin-gonic/gin"
)

func TestNewStaticAssetCache(t *testing.T) {
	cache := newStaticAssetCache(webassets.FS, "web")

	// Should have entries for known embedded files
	etag := cache.ETag("web/entry.html")
	if etag == "" {
		t.Fatal("expected ETag for web/entry.html, got empty")
	}
	// ETags must be quoted strings
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("expected quoted ETag, got %q", etag)
	}
	// Consistent across calls
	if cache.ETag("web/entry.html") != etag {
		t.Fatal("ETag not consistent across calls")
	}
	// Unknown path returns empty
	if cache.ETag("web/nonexistent.xyz") != "" {
		t.Fatal("expected empty ETag for nonexistent path")
	}
}

func TestCachePolicy(t *testing.T) {
	tests := []struct {
		path   string
		expect string
	}{
		// Code/data files: must revalidate
		{"web/entry.html", "no-cache"},
		{"web/flutter_bootstrap.js", "no-cache"},
		{"web/flutter.js", "no-cache"},
		{"web/main.dart.js", "no-cache"},
		{"web/sw.js", "no-cache"},
		{"web/version.json", "no-cache"},
		{"web/manifest.json", "no-cache"},
		{"web/assets/FontManifest.json", "no-cache"},
		{"web/assets/AssetManifest.bin", "no-cache"},
		{"web/assets/AssetManifest.bin.json", "no-cache"}, // path.Ext → ".json"
		{"web/canvaskit/canvaskit.js", "no-cache"},
		{"web/some/style.css", "no-cache"},
		{"web/canvaskit/canvaskit.wasm", "no-cache"},
		// Static assets: long cache
		{"web/favicon.png", "public, max-age=86400"},
		{"web/assets/assets/piccolo.svg", "public, max-age=86400"},
		{"web/assets/assets/fonts/Inter/Inter-Regular.ttf", "public, max-age=86400"},
		{"web/assets/fonts/MaterialIcons-Regular.otf", "public, max-age=86400"},
		{"web/assets/shaders/ink_sparkle.frag", "public, max-age=86400"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := cachePolicy(tt.path)
			if got != tt.expect {
				t.Errorf("cachePolicy(%q) = %q, want %q", tt.path, got, tt.expect)
			}
		})
	}
}

// setupStaticTestRouter creates a minimal gin engine with the NoRoute handler
// wired the same way as GinServer.setupGinRoutes.
func setupStaticTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	cache := newStaticAssetCache(webassets.FS, "web")

	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
			requestedPath := c.Request.URL.Path
			if strings.HasPrefix(requestedPath, "/api/") || strings.HasPrefix(requestedPath, "/oauth/") {
				c.Status(http.StatusNotFound)
				return
			}
			if strings.HasSuffix(requestedPath, "/") {
				requestedPath += "entry.html"
			}
			fspath := "web" + requestedPath
			if _, err := fs.Stat(webassets.FS, fspath); err != nil {
				fspath = "web/entry.html"
			}
			if etag := cache.ETag(fspath); etag != "" {
				c.Header("Cache-Control", cachePolicy(fspath))
				c.Header("ETag", etag)
			}
			c.FileFromFS(fspath, http.FS(webassets.FS))
		} else {
			c.Status(http.StatusNotFound)
		}
	})
	return r
}

func TestStaticAssetServing_ETag304(t *testing.T) {
	r := setupStaticTestRouter()

	// First request: get the ETag
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/entry.html", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag header on first request")
	}
	body := w.Body.Len()
	if body == 0 {
		t.Fatal("expected non-empty body on first request")
	}

	// Second request: conditional with If-None-Match
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/entry.html", nil)
	req2.Header.Set("If-None-Match", etag)
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", w2.Code)
	}
	if w2.Body.Len() != 0 {
		t.Fatalf("expected empty body on 304, got %d bytes", w2.Body.Len())
	}
}

func TestStaticAssetServing_ETagMismatch(t *testing.T) {
	r := setupStaticTestRouter()

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/entry.html", nil)
	req.Header.Set("If-None-Match", `"wrong-etag"`)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag header")
	}
	if w.Body.Len() == 0 {
		t.Fatal("expected non-empty body")
	}
}

func TestStaticAssetServing_CacheControlHeaders(t *testing.T) {
	r := setupStaticTestRouter()

	// entry.html should get no-cache
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/entry.html", nil)
	r.ServeHTTP(w, req)

	cc := w.Header().Get("Cache-Control")
	if cc != "no-cache" {
		t.Errorf("entry.html Cache-Control = %q, want %q", cc, "no-cache")
	}

	// An asset file should get public, max-age=86400
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/favicon.png", nil)
	r.ServeHTTP(w2, req2)

	cc2 := w2.Header().Get("Cache-Control")
	if cc2 != "public, max-age=86400" {
		t.Errorf("favicon.png Cache-Control = %q, want %q", cc2, "public, max-age=86400")
	}
}

func TestStaticAssetServing_SPAFallback(t *testing.T) {
	r := setupStaticTestRouter()

	// Request a path that doesn't exist — should serve entry.html with its ETag
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/some/spa/route", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag on SPA fallback")
	}
	cc := w.Header().Get("Cache-Control")
	if cc != "no-cache" {
		t.Errorf("SPA fallback Cache-Control = %q, want %q", cc, "no-cache")
	}

	// The ETag should match entry.html's ETag
	cache := newStaticAssetCache(webassets.FS, "web")
	entryETag := cache.ETag("web/entry.html")
	if etag != entryETag {
		t.Errorf("SPA fallback ETag %q != entry.html ETag %q", etag, entryETag)
	}
}

func TestStaticAssetServing_HEAD(t *testing.T) {
	r := setupStaticTestRouter()

	w := httptest.NewRecorder()
	req := httptest.NewRequest("HEAD", "/entry.html", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for HEAD, got %d", w.Code)
	}
	if w.Header().Get("ETag") == "" {
		t.Fatal("expected ETag header on HEAD request")
	}
	if w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("HEAD Cache-Control = %q, want %q", w.Header().Get("Cache-Control"), "no-cache")
	}
	if w.Body.Len() != 0 {
		t.Fatalf("expected empty body for HEAD, got %d bytes", w.Body.Len())
	}
}

package server

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// bootstrapFiles are Flutter Web files that must always revalidate.
// These are entry points and service worker scripts that reference
// other assets without cache-busting query params.
var bootstrapFiles = map[string]struct{}{
	"entry.html":               {},
	"flutter_service_worker.js": {},
	"flutter_bootstrap.js":     {},
	"flutter.js":               {},
	"version.json":             {},
	"manifest.json":            {},
}

// staticAssetCache precomputes ETags for embedded static assets at startup.
type staticAssetCache struct {
	etags map[string]string // fspath → quoted ETag (e.g. `"a1b2c3d4e5f6a7b8"`)
}

// newStaticAssetCache walks fsys under root and computes SHA-256 ETags
// (truncated to 16 hex chars) for every regular file.
func newStaticAssetCache(fsys fs.FS, root string) *staticAssetCache {
	cache := &staticAssetCache{etags: make(map[string]string)}
	fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		cache.etags[path] = fmt.Sprintf(`"%x"`, sum[:8]) // 16 hex chars
		return nil
	})
	return cache
}

// ETag returns the precomputed quoted ETag for the given fspath,
// or "" if the path is not in the cache.
func (c *staticAssetCache) ETag(fspath string) string {
	return c.etags[fspath]
}

// cachePolicy returns the Cache-Control value for a static asset path.
// Bootstrap-critical files get "no-cache" (must revalidate every time).
// All other files get "public, max-age=86400" (24h cache).
func cachePolicy(fspath string) string {
	base := path.Base(fspath)
	if _, ok := bootstrapFiles[strings.ToLower(base)]; ok {
		return "no-cache"
	}
	return "public, max-age=86400"
}

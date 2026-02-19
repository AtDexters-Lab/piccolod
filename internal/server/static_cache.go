package server

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// noCacheExtensions are file extensions for code and data assets that must
// revalidate on every request (Cache-Control: no-cache). ETags ensure
// revalidation is cheap (304 when unchanged). This prevents stale JS/WASM
// from persisting across binary updates.
var noCacheExtensions = map[string]struct{}{
	".html": {},
	".js":   {},
	".mjs":  {},
	".css":  {},
	".wasm": {},
	".json": {},
	".bin":  {},
	".map":  {},
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
// Code/data files (JS, WASM, JSON, HTML, BIN) get "no-cache" so they
// always revalidate via ETag. Static assets (fonts, images, shaders)
// get "public, max-age=86400" (24h).
func cachePolicy(fspath string) string {
	ext := strings.ToLower(path.Ext(fspath))
	if _, ok := noCacheExtensions[ext]; ok {
		return "no-cache"
	}
	return "public, max-age=86400"
}

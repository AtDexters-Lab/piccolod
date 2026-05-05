package oidc

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"piccolod/internal/services/middleware/l7"
)

// authorizeRewriteMaxBody is the maximum response body size for Layer 2
// body rewriting. Responses larger than this are passed through unmodified.
const authorizeRewriteMaxBody = 1 << 20 // 1 MB

// Snapshot holds OIDC rewrite state captured under lock in the request handler
// and threaded into ModifyResponse via request context.
type Snapshot struct {
	IssuerOrigin   string   // e.g., "http://piccolo-abc123.local"
	PortalOrigin   string   // e.g., "https://slug.piccolospace.com"
	AuthorizePaths []string // app's declared authorize_paths
}

type snapshotContextKey struct{}

// ContextWithSnapshot returns a copy of ctx carrying snap.
func ContextWithSnapshot(ctx context.Context, snap *Snapshot) context.Context {
	return context.WithValue(ctx, snapshotContextKey{}, snap)
}

// SnapshotFromContext retrieves a snapshot stored via ContextWithSnapshot.
func SnapshotFromContext(ctx context.Context) (*Snapshot, bool) {
	snap, ok := ctx.Value(snapshotContextKey{}).(*Snapshot)
	if !ok || snap == nil {
		return nil, false
	}
	return snap, true
}

// RewriteAuthorizeResponse performs dual-layer OIDC authorization URL
// rewriting for WAN requests. Layer 1 rewrites Location headers on 3xx
// responses. Layer 2 rewrites response bodies on declared authorize_paths.
func RewriteAuthorizeResponse(resp *http.Response, snap *Snapshot, appName string) {
	oldURL := snap.IssuerOrigin + "/oauth/authorize"
	newURL := snap.PortalOrigin + "/oauth/authorize"

	// Layer 1: Location header rewrite on 3xx responses.
	// Match the exact authorize URL or one followed by '?' (query string) or '/'
	// (subpath) to avoid matching unrelated paths like .../authorize_token.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); loc != "" {
			if loc == oldURL || strings.HasPrefix(loc, oldURL+"?") || strings.HasPrefix(loc, oldURL+"/") || strings.HasPrefix(loc, oldURL+"#") {
				resp.Header.Set("Location", newURL+loc[len(oldURL):])
				log.Printf("DEBUG: OIDC rewrite L1: app=%s location rewritten", appName)
			}
		}
	}

	// Layer 2: Body text replacement on declared authorize_paths.
	if len(snap.AuthorizePaths) == 0 {
		return
	}
	reqPath := ""
	if resp.Request != nil && resp.Request.URL != nil {
		reqPath = resp.Request.URL.Path
	}
	pathMatch := false
	for _, ap := range snap.AuthorizePaths {
		if reqPath == ap {
			pathMatch = true
			break
		}
	}
	if !pathMatch {
		return
	}

	// Only rewrite text-based content types. Reuses the proxy's existing
	// compressible-types definition (mime.ParseMediaType-based, handles
	// charset suffixes correctly).
	if !l7.IsCompressibleContentType(resp.Header.Get("Content-Type")) {
		return
	}

	// Skip compressed responses — we cannot rewrite without decompressing.
	if resp.Header.Get("Content-Encoding") != "" {
		log.Printf("WARN: OIDC rewrite L2 skipped (Content-Encoding present): app=%s path=%s", appName, reqPath)
		return
	}

	// Pre-check Content-Length to avoid consuming oversized responses.
	if resp.ContentLength > authorizeRewriteMaxBody {
		log.Printf("WARN: OIDC rewrite L2 skipped (Content-Length %d > %d): app=%s path=%s", resp.ContentLength, authorizeRewriteMaxBody, appName, reqPath)
		return
	}

	// Read up to maxBody+1 bytes to detect oversize from chunked responses.
	body, err := io.ReadAll(io.LimitReader(resp.Body, authorizeRewriteMaxBody+1))
	if err != nil {
		// Splice the read bytes back so the downstream client gets the full body.
		// Preserve the original Closer so resources are released correctly.
		log.Printf("WARN: OIDC rewrite L2 body read error: app=%s path=%s err=%v", appName, reqPath, err)
		resp.Body = &spliceCloser{
			Reader: io.MultiReader(bytes.NewReader(body), resp.Body),
			closer: resp.Body,
		}
		return
	}
	if int64(len(body)) > authorizeRewriteMaxBody {
		// Body exceeded the limit. Splice back the consumed portion plus the
		// rest of the original body so nothing is truncated.
		log.Printf("WARN: OIDC rewrite L2 skipped (body too large: >%d bytes): app=%s path=%s", authorizeRewriteMaxBody, appName, reqPath)
		resp.Body = &spliceCloser{
			Reader: io.MultiReader(bytes.NewReader(body), resp.Body),
			closer: resp.Body,
		}
		return
	}

	// Body fully read; safe to close the original.
	resp.Body.Close()

	if len(body) == 0 {
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		resp.ContentLength = 0
		resp.Header.Set("Content-Length", "0")
		return
	}

	// Replace the authorize URL pattern, matching only when followed by a
	// valid URL boundary to avoid false positives like `/oauth/authorize_token`.
	newBody, count := replaceAuthorizeURL(body, []byte(oldURL), []byte(newURL))
	if count == 0 {
		log.Printf("WARN: OIDC rewrite L2 matched path but zero replacements: app=%s path=%s", appName, reqPath)
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return
	}
	resp.Body = io.NopCloser(bytes.NewReader(newBody))
	resp.ContentLength = int64(len(newBody))
	resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	resp.Header.Del("Transfer-Encoding")
	log.Printf("DEBUG: OIDC rewrite L2: app=%s path=%s replacements=%d oldSize=%d newSize=%d", appName, reqPath, count, len(body), len(newBody))
}

// replaceAuthorizeURL replaces `old` with `new` in `body`, but only when the
// match is followed by a valid URL boundary (end-of-buffer or one of the
// characters that legitimately terminate a URL in HTML/JSON/JS contexts).
// This prevents false positives like `/oauth/authorize_token`.
func replaceAuthorizeURL(body, old, new []byte) ([]byte, int) {
	if len(old) == 0 {
		return body, 0
	}
	var out bytes.Buffer
	out.Grow(len(body))
	count := 0
	i := 0
	for {
		j := bytes.Index(body[i:], old)
		if j < 0 {
			out.Write(body[i:])
			break
		}
		matchStart := i + j
		matchEnd := matchStart + len(old)
		if matchEnd == len(body) || isURLBoundary(body[matchEnd]) {
			out.Write(body[i:matchStart])
			out.Write(new)
			count++
			i = matchEnd
		} else {
			// Not a boundary match — keep original bytes up to and including the prefix
			// and advance past the first byte of the match to allow overlapping searches.
			out.Write(body[i : matchStart+1])
			i = matchStart + 1
		}
	}
	return out.Bytes(), count
}

// isURLBoundary reports whether b is a character that legitimately terminates
// a URL in HTML/JSON/JS contexts.
func isURLBoundary(b byte) bool {
	switch b {
	case '?', '#', '/', '"', '\'', '<', '>', ' ', '\t', '\n', '\r', ',', ';', ')', '}', ']', '\\':
		return true
	}
	return false
}

// spliceCloser combines a Reader (typically a MultiReader) with the original
// body's Closer so the downstream HTTP transport closes the underlying body.
type spliceCloser struct {
	io.Reader
	closer io.Closer
}

func (s *spliceCloser) Close() error { return s.closer.Close() }

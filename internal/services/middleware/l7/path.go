package l7

import (
	"net/http"
	"path"
	"strings"

	"piccolod/internal/api"
)

// NormalizeAndSetRequestPath applies RFC 20260112 path handling rules.
//
// It returns a cleaned path that MUST be used for both rule matching and forwarding.
// On error, it returns an error code suitable for proxy JSON responses:
//   - "INVALID_PATH" for malformed/unsafe raw paths
//   - "PATH_INVALID" for normalization failures (e.g., traversal above root)
//
// Used by the path_normalize middleware factory in path_normalize.go to produce
// the in-place mutation per the §H path-representation contract.
func NormalizeAndSetRequestPath(r *http.Request) (string, string) {
	if r == nil || r.URL == nil {
		return "", "INVALID_PATH"
	}

	rawPath := rawPathFromRequestURI(r.RequestURI)
	rawLower := strings.ToLower(rawPath)

	// Step 1: Reject encoded separators and encoded percent (blocks double-encoding).
	if strings.Contains(rawLower, "%2f") || strings.Contains(rawLower, "%5c") || strings.Contains(rawLower, "%25") {
		return "", "INVALID_PATH"
	}
	// Step 1: Reject raw backslash after Go decoding (some stacks treat as separator).
	if strings.ContainsRune(r.URL.Path, '\\') {
		return "", "INVALID_PATH"
	}

	decodedPath := r.URL.Path
	if decodedPath == "" {
		decodedPath = "/"
	}

	// Step 2: Detect traversal above root (must reject, path.Clean would mask).
	if violatesRootTraversal(decodedPath) {
		return "", "PATH_INVALID"
	}

	// Step 3: Clean path (preserve trailing slash).
	hasTrailingSlash := strings.HasSuffix(decodedPath, "/")
	cleaned := path.Clean(decodedPath)
	if cleaned == "." {
		cleaned = "/"
	}
	if hasTrailingSlash && cleaned != "/" && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}

	// Step 4: Forward cleaned path.
	r.URL.Path = cleaned
	r.URL.RawPath = ""

	return cleaned, ""
}

func rawPathFromRequestURI(requestURI string) string {
	if requestURI == "" {
		return ""
	}
	// RequestURI may be origin-form ("/path?query") or absolute-form ("http://host/path?query").
	// We only need the raw path portion for encoded separator checks.
	uri := requestURI
	if i := strings.IndexByte(uri, '?'); i != -1 {
		uri = uri[:i]
	}
	// Strip scheme/host if absolute-form.
	if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
		// Minimal parse to avoid importing net/url; absolute-form is rare in server handlers.
		if i := strings.Index(uri, "://"); i != -1 {
			rest := uri[i+3:]
			if j := strings.IndexByte(rest, '/'); j != -1 {
				return rest[j:]
			}
			return "/"
		}
	}
	return uri
}

func violatesRootTraversal(p string) bool {
	depth := 0
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			depth--
			if depth < 0 {
				return true
			}
		default:
			depth++
		}
	}
	return false
}

// ListenerStrategyForPath resolves the listener auth strategy for a request path
// against the listener's auth rules. RFC 4.1.1: auth omitted → all "protected";
// no rule matches → "protected"; first matching rule wins.
//
// Used by the path_auth middleware to choose between protected, headers,
// oidc_passthrough, and public branches.
func ListenerStrategyForPath(auth *api.ListenerAuth, requestPath string) string {
	if auth == nil || len(auth.Rules) == 0 {
		return "protected"
	}

	for _, rule := range auth.Rules {
		ruleType := strings.TrimSpace(rule.Type)
		rulePath := strings.TrimSpace(rule.Path)
		ruleStrategy := strings.TrimSpace(rule.Strategy)

		switch ruleType {
		case "exact":
			if requestPath == rulePath {
				return ruleStrategy
			}
		case "prefix":
			// RFC 4.1.1: prefix paths must end with "/". With that invariant, a simple string prefix
			// match is element-boundary safe (e.g., "/api/" matches "/api/users" but not "/apikeys").
			if strings.HasPrefix(requestPath, rulePath) {
				return ruleStrategy
			}
		case "pattern":
			ok, err := path.Match(rulePath, requestPath)
			if err == nil && ok {
				return ruleStrategy
			}
		}
	}

	return "protected"
}

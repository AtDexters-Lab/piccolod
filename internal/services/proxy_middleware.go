package services

import (
	"net/http"
	"strings"
)

// piccoloHeaderPrefix is the prefix of all Piccolo-injected headers (e.g.,
// X-Piccolo-User defined alongside the path_auth middleware in l7/path_auth.go,
// and X-Piccolo-Hint-Token in proxy.go). stripPiccoloHeaders uses the prefix
// to remove client-spoofed values before any Piccolo-side injection runs.
const piccoloHeaderPrefix = "X-Piccolo-"

// StripHeadersFromRequest removes all X-Piccolo-* headers from the request to
// prevent client-side spoofing. Called by the proxy before any handler runs.
func StripHeadersFromRequest(r *http.Request) {
	stripPiccoloHeaders(r)
}

// stripPiccoloHeaders removes all X-Piccolo-* headers from the request.
func stripPiccoloHeaders(r *http.Request) {
	toDelete := make([]string, 0)
	for key := range r.Header {
		if strings.HasPrefix(strings.ToLower(key), strings.ToLower(piccoloHeaderPrefix)) {
			toDelete = append(toDelete, key)
		}
	}
	for _, key := range toDelete {
		r.Header.Del(key)
	}
}

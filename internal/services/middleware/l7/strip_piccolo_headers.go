package l7

import (
	"net/http"
	"strings"

	"piccolod/internal/services/middleware"
)

// piccoloHeaderPrefix is the prefix of all Piccolo-injected headers (e.g.,
// X-Piccolo-User defined alongside the path_auth middleware here in l7,
// X-Piccolo-Hint-Token in services/proxy.go pending step 9). The strip below
// uses the prefix to remove client-spoofed values before any handler that
// trusts X-Piccolo-* runs (per RFC 4.1.5).
const piccoloHeaderPrefix = "X-Piccolo-"

// StripPiccoloHeaders removes all X-Piccolo-* headers from the request to
// prevent client-side spoofing. Today's behavior at proxy.go's
// `StripHeadersFromRequest`. No dependencies.
//
// Composes BEFORE the path_auth headers strategy injects the trusted variants,
// and BEFORE the ACME HTTP-01 bypass so external verifiers never see an
// attacker-injected X-Piccolo-* header.
func StripPiccoloHeaders() middleware.L7Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stripPiccoloHeaders(r)
			next.ServeHTTP(w, r)
		})
	}
}

func stripPiccoloHeaders(r *http.Request) {
	var toDelete []string
	for key := range r.Header {
		if strings.HasPrefix(strings.ToLower(key), strings.ToLower(piccoloHeaderPrefix)) {
			toDelete = append(toDelete, key)
		}
	}
	for _, key := range toDelete {
		r.Header.Del(key)
	}
}

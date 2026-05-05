package services

import (
	"net/http"
	"strings"
)

// PiccoloHeaders are the trusted headers that piccolod injects when an app's
// auth strategy is "headers". Apps consume these to identify the authenticated
// user without re-implementing session validation.
const (
	HeaderPiccoloUser  = "X-Piccolo-User"
	HeaderPiccoloEmail = "X-Piccolo-Email"
	HeaderPiccoloName  = "X-Piccolo-Name"
	HeaderPiccoloRole  = "X-Piccolo-Role"
)

// piccoloHeaderPrefix is the prefix of all Piccolo-injected headers. Used by
// stripPiccoloHeaders to remove client-spoofed values before injection.
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

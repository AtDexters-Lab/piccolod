package l7

import (
	"net/http"

	"piccolod/internal/services/middleware"
)

// PathNormalizer mutates r.URL.Path in place per the plan §H path-representation
// contract. Returns the normalized path plus an error code string
// ("INVALID_PATH" / "PATH_INVALID" / "" on success). Typically bound to
// NormalizeAndSetRequestPath; kept as a func type so callers can substitute
// (e.g., for tests).
type PathNormalizer func(r *http.Request) (cleanedPath string, errCode string)

// ErrorWriter writes a JSON error response. Matches WriteJSONError so the
// extracted middleware preserves response formatting bit-for-bit. Kept as a
// func type for the same substitutability reason as PathNormalizer.
type ErrorWriter func(w http.ResponseWriter, status int, message, code string)

// PathNormalize runs the supplied normalizer on each incoming request,
// mutating r.URL.Path in place per the plan §H path-representation contract
// (downstream middlewares MUST read r.URL.Path).
//
// On normalization error, writes a 400 JSON response and short-circuits (next
// handler is not invoked).
func PathNormalize(normalize PathNormalizer, writeErr ErrorWriter) middleware.L7Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, errCode := normalize(r)
			if errCode != "" {
				if errCode == "INVALID_PATH" {
					writeErr(w, http.StatusBadRequest, "invalid_request_path", "INVALID_PATH")
					return
				}
				writeErr(w, http.StatusBadRequest, "path_normalization_failed", "PATH_INVALID")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

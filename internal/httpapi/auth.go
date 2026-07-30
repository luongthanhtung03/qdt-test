package httpapi

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// requireBearerToken guards the admin subtree.
//
// The brief waives real authentication, so this is a single static token. Two
// details are still worth getting right: the comparison is constant-time, so
// the endpoint does not leak the token one byte at a time under timing
// analysis, and the middleware is mounted on the /api/v1 subtree rather than
// globally, so the public routes cannot inherit it and the admin routes cannot
// lose it by accident.
func requireBearerToken(expected string) func(http.Handler) http.Handler {
	expectedBytes := []byte(expected)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(header, "Bearer ")
			if !ok {
				unauthorized(w)
				return
			}

			// ConstantTimeCompare returns 0 for differing lengths, which
			// already covers the empty-token case.
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), expectedBytes) != 1 {
				unauthorized(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func unauthorized(w http.ResponseWriter) {
	// RFC 9110 requires a challenge on a 401.
	w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
	writeError(w, http.StatusUnauthorized, CodeUnauthorized,
		"A valid bearer token is required.", nil)
}

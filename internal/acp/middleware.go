package acp

import (
	"crypto/subtle"
	"net/http"
	"path/filepath"
	"strings"
)

// bearerToken extracts the token from an Authorization: Bearer <token> header.
func bearerToken(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if !strings.HasPrefix(v, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(v, "Bearer ")
}

// authMiddleware wraps a handler to require a bearer token.
// If token is empty, all requests pass through (dev mode).
//
// A rejection is a JSON 401 with a WWW-Authenticate challenge, so a client can
// tell "wrong token" from "server unreachable" instead of guessing at an HTML
// error page.
func authMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tokenMatches(bearerToken(r), token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="harnessd"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenMatches compares in constant time so a wrong token cannot be recovered
// one byte at a time from response timing.
func tokenMatches(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// cwdAllowed reports whether cwd is under one of the allowed roots.
// An empty roots list allows everything.
func cwdAllowed(cwd string, roots []string) bool {
	if len(roots) == 0 {
		return true
	}
	clean := filepath.Clean(cwd)
	for _, root := range roots {
		r := filepath.Clean(root)
		if clean == r || strings.HasPrefix(clean, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

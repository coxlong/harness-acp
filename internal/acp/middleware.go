package acp

import (
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
func authMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearerToken(r) != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
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

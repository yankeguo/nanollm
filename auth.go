package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

func extractAPIKey(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if token, ok := cutBearer(v); ok {
			return token
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("Api-Key"); v != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

func cutBearer(v string) (string, bool) {
	const prefix = "bearer"
	if len(v) < len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return "", false
	}
	rest := v[len(prefix):]
	if rest == "" {
		return "", true
	}
	if rest[0] != ' ' && rest[0] != '\t' {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

func (c *Config) lookupAPIKey(value string) *APIKey {
	if c == nil || value == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(value))
	var found *APIKey
	for i := range c.APIKeys {
		want := sha256.Sum256([]byte(c.APIKeys[i].Value))
		if subtle.ConstantTimeCompare(sum[:], want[:]) == 1 {
			found = &c.APIKeys[i]
		}
	}
	return found
}

func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := s.Config.lookupAPIKey(extractAPIKey(r))
		if key == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nanollm"`)
			writeAPIError(w, http.StatusUnauthorized, "invalid_request_error", "invalid api key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

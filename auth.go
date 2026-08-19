package main

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

type ctxKey int

const apiKeyNameCtxKey ctxKey = 1

func withAPIKeyName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, apiKeyNameCtxKey, name)
}

func apiKeyNameFrom(ctx context.Context) string {
	name, _ := ctx.Value(apiKeyNameCtxKey).(string)
	return name
}

func extractAPIKey(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if token, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(token)
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

func (c *Config) lookupAPIKey(value string) *APIKey {
	if c == nil || value == "" {
		return nil
	}
	got := []byte(value)
	for i := range c.APIKeys {
		want := []byte(c.APIKeys[i].Value)
		if subtle.ConstantTimeCompare(got, want) == 1 {
			return &c.APIKeys[i]
		}
	}
	return nil
}

func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := s.Config.lookupAPIKey(extractAPIKey(r))
		if key == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nanollm"`)
			writeAPIError(w, http.StatusUnauthorized, "invalid_request_error", "invalid api key")
			return
		}
		next.ServeHTTP(w, r.WithContext(withAPIKeyName(r.Context(), key.Name)))
	})
}

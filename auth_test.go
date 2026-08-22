package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHealthzDoesNotRequireAPIKey(t *testing.T) {
	h := testProxy(t, cfgFast(Provider{Name: "x", Model: "x", OpenAICompletions: ep("http://example.invalid")}))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "OK", rec.Body.String())
}

func TestAPIKeyRequired(t *testing.T) {
	h := testProxy(t, cfgFast(Provider{Name: "x", Model: "x", OpenAICompletions: ep("http://example.invalid")}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid api key")
}

func TestAPIKeyRejectsUnknown(t *testing.T) {
	h := testProxy(t, cfgFast(Provider{Name: "x", Model: "x", OpenAICompletions: ep("http://example.invalid")}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAPIKeyAcceptsBearerAndXApiKey(t *testing.T) {
	h := testProxy(t, cfgFast(Provider{Name: "x", Model: "x", OpenAICompletions: ep("http://example.invalid")}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "bearer "+testAPIKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Api-Key", testAPIKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Api-Key", testAPIKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestExtractAPIKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer  sk-abc ")
	require.Equal(t, "sk-abc", extractAPIKey(req))

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "BEARER sk-abc")
	require.Equal(t, "sk-abc", extractAPIKey(req))

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Api-Key", " sk-abc ")
	require.Equal(t, "sk-abc", extractAPIKey(req))
}

func TestLookupAPIKeyScansAllKeys(t *testing.T) {
	cfg := &Config{APIKeys: []APIKey{
		{Name: "short", Value: "ab"},
		{Name: "long", Value: "sk-test-long"},
	}}
	require.Equal(t, "long", cfg.lookupAPIKey("sk-test-long").Name)
	require.Equal(t, "short", cfg.lookupAPIKey("ab").Name)
	require.Nil(t, cfg.lookupAPIKey("nope"))
}

func TestAnthropicAPIKeyUnauthorized(t *testing.T) {
	h := testProxy(t, cfgFast(Provider{Name: "x", Model: "x", Anthropic: ep("http://example.invalid")}))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", jsonBody(map[string]any{"model": "x"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Empty(t, rec.Header().Get("WWW-Authenticate"))
	require.Contains(t, rec.Body.String(), `"type":"error"`)
	require.Contains(t, rec.Body.String(), "authentication_error")
}

func TestAnthropicAPIKeyAcceptsXApiKey(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	t.Cleanup(up.Close)
	cfg := cfgFast(Provider{Name: "x", Model: "x", Anthropic: ep(up.URL)})
	cfg.Models[0].Name = "claude"
	h := NewServer(cfg, nil, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", jsonBody(map[string]any{"model": "claude"}))
	req.Header.Set("X-Api-Key", testAPIKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

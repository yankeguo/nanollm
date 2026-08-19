package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHealthzDoesNotRequireAPIKey(t *testing.T) {
	h := testProxy(t, cfgFast(Provider{Name: "x", URL: "http://example.invalid", Model: "x"}))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "OK", rec.Body.String())
}

func TestAPIKeyRequired(t *testing.T) {
	h := testProxy(t, cfgFast(Provider{Name: "x", URL: "http://example.invalid", Model: "x"}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid api key")
}

func TestAPIKeyRejectsUnknown(t *testing.T) {
	h := testProxy(t, cfgFast(Provider{Name: "x", URL: "http://example.invalid", Model: "x"}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAPIKeyAcceptsBearerAndXApiKey(t *testing.T) {
	h := testProxy(t, cfgFast(Provider{Name: "x", URL: "http://example.invalid", Model: "x"}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Api-Key", testAPIKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestExtractAPIKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer  sk-abc ")
	require.Equal(t, "sk-abc", extractAPIKey(req))
}

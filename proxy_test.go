package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

const testAPIKey = "sk-test"

func testProxy(t *testing.T, cfg *Config) http.Handler {
	t.Helper()
	if len(cfg.APIKeys) == 0 {
		cfg.APIKeys = []APIKey{{Name: "test", Value: testAPIKey}}
	}
	return NewServer(cfg, nil).Handler()
}

func cfgFast(providers ...Provider) *Config {
	return &Config{
		APIKeys: []APIKey{{Name: "test", Value: testAPIKey}},
		Models:  []ModelConfig{{Name: "fast", Providers: providers}},
	}
}

func authed(req *http.Request) *http.Request {
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	return req
}

func TestProxyRewritesModelAndHeaders(t *testing.T) {
	var gotBody []byte
	var gotAuth string
	var gotExtra string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		gotExtra = r.Header.Get("X-Extra")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"1","model":"gpt-4o-mini","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}))
	t.Cleanup(up.Close)

	h := testProxy(t, cfgFast(Provider{
		Name:  "primary",
		URL:   up.URL + "/v1/chat/completions",
		Model: "gpt-4o-mini",
		Headers: map[string]string{
			"Authorization": "Bearer sk-up",
			"X-Extra":       "yes",
		},
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{
		"model":    "fast",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))

	require.Equal(t, http.StatusOK, rec.Code)
	var sent map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sent))
	require.Equal(t, "gpt-4o-mini", sent["model"])
	require.Equal(t, "Bearer sk-up", gotAuth)
	require.Equal(t, "yes", gotExtra)
}

func TestProxyFailoversOnCatastrophicThenSucceeds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadURL := "http://" + ln.Addr().String() + "/v1/chat/completions"
	require.NoError(t, ln.Close())

	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"2","choices":[{"message":{"content":"backup"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(up.Close)

	h := testProxy(t, cfgFast(
		Provider{Name: "primary", URL: deadURL, Model: "primary"},
		Provider{Name: "backup", URL: up.URL + "/v1/chat/completions", Model: "backup"},
	))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{
		"model": "fast",
	}))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int32(1), hits.Load())
	require.Contains(t, rec.Body.String(), "backup")
}

func TestProxyDoesNotFailoverOnClientError(t *testing.T) {
	var secondHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"bad req"}}`)
	}))
	t.Cleanup(first.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(second.Close)

	h := testProxy(t, cfgFast(
		Provider{Name: "a", URL: first.URL, Model: "a"},
		Provider{Name: "b", URL: second.URL, Model: "b"},
	))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "fast"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, int32(0), secondHits.Load())
	require.Contains(t, rec.Body.String(), "bad req")
}

func TestProxyFailoversOn503(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `down`)
	}))
	t.Cleanup(first.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(second.Close)

	h := testProxy(t, cfgFast(
		Provider{Name: "a", URL: first.URL, Model: "a"},
		Provider{Name: "b", URL: second.URL, Model: "b"},
	))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "fast"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"ok"`)
}

func TestProxyUnknownModel(t *testing.T) {
	h := testProxy(t, cfgFast(Provider{Name: "x", URL: "http://example.invalid", Model: "x"}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "missing"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestProxyStreamUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)

	h := testProxy(t, cfgFast(Provider{Name: "m", URL: up.URL, Model: "m"}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{
		"model":  "fast",
		"stream": true,
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "[DONE]")
}

func TestModelsEndpoint(t *testing.T) {
	h := testProxy(t, &Config{Models: []ModelConfig{
		{Name: "fast", Providers: []Provider{{Name: "x", URL: "http://example.invalid", Model: "x"}}},
		{Name: "code", Providers: []Provider{{Name: "y", URL: "http://example.invalid", Model: "y"}}},
	}})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"fast"`)
	require.Contains(t, rec.Body.String(), `"code"`)
}

func TestProxyDoesNotFailoverOn429(t *testing.T) {
	var secondHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	t.Cleanup(first.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
	}))
	t.Cleanup(second.Close)

	h := testProxy(t, cfgFast(
		Provider{Name: "a", URL: first.URL, Model: "a"},
		Provider{Name: "b", URL: second.URL, Model: "b"},
	))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "fast"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, int32(0), secondHits.Load())
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

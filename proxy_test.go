package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testAPIKey = "sk-test"

func testProxy(t *testing.T, cfg *Config) http.Handler {
	t.Helper()
	if len(cfg.APIKeys) == 0 {
		cfg.APIKeys = []APIKey{{Name: "test", Value: testAPIKey}}
	}
	return NewServer(cfg, nil, nil).Handler()
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
	var gotXAPIKey string
	var gotAPIKey string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		gotExtra = r.Header.Get("X-Extra")
		gotXAPIKey = r.Header.Get("X-Api-Key")
		gotAPIKey = r.Header.Get("Api-Key")
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
	req.Header.Set("X-Api-Key", "client-x")
	req.Header.Set("Api-Key", "client-api")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))

	require.Equal(t, http.StatusOK, rec.Code)
	var sent map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sent))
	require.Equal(t, "gpt-4o-mini", sent["model"])
	require.Equal(t, "Bearer sk-up", gotAuth)
	require.Equal(t, "yes", gotExtra)
	require.Empty(t, gotXAPIKey)
	require.Empty(t, gotAPIKey)
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

func TestProxyStreamPassthrough(t *testing.T) {
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

func TestProxyLogsCanceledAfter200(t *testing.T) {
	started := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(up.Close)

	logger := &memoryCallLogger{}
	h := NewServer(cfgFast(Provider{Name: "m", URL: up.URL, Model: "m"}), logger, nil).Handler()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{
		"model":  "fast",
		"stream": true,
	})).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, authed(req))
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not start")
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return")
	}

	require.Len(t, logger.calls, 1)
	require.Equal(t, http.StatusOK, logger.calls[0].HTTPStatus)
	require.Equal(t, errCanceled, logger.calls[0].Error)
}

func TestProxyLogsCanceledBeforeResponse(t *testing.T) {
	started := make(chan struct{})
	block := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		// Wait for test end instead of r.Context(): the server cannot detect
		// a client disconnect while the request body is unread.
		<-block
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(block) })

	logger := &memoryCallLogger{}
	h := NewServer(cfgFast(
		Provider{Name: "m", URL: up.URL, Model: "m"},
		Provider{Name: "m2", URL: up.URL, Model: "m"},
	), logger, nil).Handler()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{
		"model": "fast",
	})).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, authed(req))
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not start")
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return")
	}

	// pre-response client cancel: logged as canceled, no failover attempt
	require.Len(t, logger.calls, 1)
	require.Equal(t, 0, logger.calls[0].HTTPStatus)
	require.Equal(t, errCanceled, logger.calls[0].Error)
}

func TestCopyErrText(t *testing.T) {
	require.Empty(t, copyErrText(nil))
	require.Equal(t, errCanceled, copyErrText(context.Canceled))
	require.Equal(t, errCanceled, copyErrText(fmt.Errorf("wrap: %w", context.Canceled)))
	require.Equal(t, "boom", copyErrText(errors.New("boom")))
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

func TestProxyDoesNotFailoverOn500(t *testing.T) {
	var secondHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"message":"boom"}}`)
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
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, int32(0), secondHits.Load())
	require.Contains(t, rec.Body.String(), "boom")
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

func TestProxyAllUpstreamsUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadURL := "http://" + ln.Addr().String() + "/v1/chat/completions"
	require.NoError(t, ln.Close())

	h := testProxy(t, cfgFast(Provider{Name: "a", URL: deadURL, Model: "a"}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "fast"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "all upstreams unavailable")
}

func TestProxyBodyTooLarge(t *testing.T) {
	orig := maxRequestBody
	maxRequestBody = 8
	t.Cleanup(func() { maxRequestBody = orig })

	h := testProxy(t, cfgFast(Provider{Name: "x", URL: "http://example.invalid", Model: "x"}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(bytes.Repeat([]byte("x"), 64)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Contains(t, rec.Body.String(), "request body too large")
}

func TestModelEndpoint(t *testing.T) {
	h := testProxy(t, &Config{Models: []ModelConfig{
		{Name: "fast", Providers: []Provider{{Name: "x", URL: "http://example.invalid", Model: "x"}}},
		{Name: "org/code", Providers: []Provider{{Name: "y", URL: "http://example.invalid", Model: "y"}}},
	}})

	req := httptest.NewRequest(http.MethodGet, "/v1/models/fast", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"fast"`)

	req = httptest.NewRequest(http.MethodGet, "/v1/models/org/code", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"org/code"`)

	req = httptest.NewRequest(http.MethodGet, "/v1/models/missing", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCopyRequestHeadersStripsSecretsAndHopByHop(t *testing.T) {
	src := make(http.Header)
	src.Set("Authorization", "Bearer client")
	src.Set("X-Api-Key", "client-x")
	src.Set("Api-Key", "client-api")
	src.Set("Cookie", "nanollm_admin=abc; other=1")
	src.Set("X-Extra", "keep")
	src.Set("Connection", "X-Tmp")
	src.Set("X-Tmp", "drop")
	src.Set("Trailer", "X-After")
	src.Set("Accept-Encoding", "gzip")
	dst := make(http.Header)
	copyRequestHeaders(dst, src)
	require.Empty(t, dst.Get("Authorization"))
	require.Empty(t, dst.Get("X-Api-Key"))
	require.Empty(t, dst.Get("Api-Key"))
	require.Empty(t, dst.Get("Cookie"))
	require.Empty(t, dst.Get("X-Tmp"))
	require.Empty(t, dst.Get("Trailer"))
	require.Empty(t, dst.Get("Accept-Encoding"))
	require.Equal(t, "keep", dst.Get("X-Extra"))
}

func TestCopyResponseHeadersStripsHopByHop(t *testing.T) {
	src := make(http.Header)
	src.Set("Content-Type", "application/json")
	src.Set("Connection", "close, X-Tmp")
	src.Set("X-Tmp", "drop")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("Set-Cookie", "session=x")
	dst := make(http.Header)
	copyResponseHeaders(dst, src)
	require.Equal(t, "application/json", dst.Get("Content-Type"))
	require.Empty(t, dst.Get("Connection"))
	require.Empty(t, dst.Get("X-Tmp"))
	require.Empty(t, dst.Get("Keep-Alive"))
	require.Empty(t, dst.Get("Set-Cookie"))
}

func TestProxyCatastrophicStatusReturnsError(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"down"}`)
	}))
	t.Cleanup(first.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(second.Close)

	logger := &memoryCallLogger{}
	h := NewServer(cfgFast(
		Provider{Name: "a", URL: first.URL},
		Provider{Name: "b", URL: second.URL},
	), logger, nil).Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "fast"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, logger.calls, 2)
	require.Equal(t, http.StatusServiceUnavailable, logger.calls[0].HTTPStatus)
	require.Contains(t, logger.calls[0].Error, "503")
	require.Equal(t, http.StatusOK, logger.calls[1].HTTPStatus)
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

type memoryCallLogger struct {
	calls []CallRecord
}

func (m *memoryCallLogger) Record(rec CallRecord) {
	m.calls = append(m.calls, rec)
}

func TestProxyLogsSuccess(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"1","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3}}}`)
	}))
	t.Cleanup(up.Close)

	logger := &memoryCallLogger{}
	cfg := cfgFast(Provider{Name: "primary", URL: up.URL, Model: "gpt-4o-mini"})
	h := NewServer(cfg, logger, nil).Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{
		"model":    "fast",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, logger.calls, 1)
	got := logger.calls[0]
	require.Equal(t, "fast", got.Model)
	require.Equal(t, "primary", got.Provider)
	require.Equal(t, "gpt-4o-mini", got.ProviderModel)
	require.Equal(t, "test", got.APIKeyName)
	require.Equal(t, int64(5), got.InputTokens)
	require.Equal(t, int64(2), got.OutputTokens)
	require.Equal(t, int64(3), got.CacheTokens)
	require.Equal(t, int64(2), got.UncachedTokens)
	require.Equal(t, http.StatusOK, got.HTTPStatus)
	require.Empty(t, got.Error)
	require.Contains(t, string(got.RequestJSON), `"model":"gpt-4o-mini"`)
	require.Contains(t, string(got.ResponseJSON), `"ok"`)
}

func TestProxyLogsFailoverAttempts(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"error":"bad gateway"}`)
	}))
	t.Cleanup(first.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(second.Close)

	logger := &memoryCallLogger{}
	cfg := cfgFast(
		Provider{Name: "a", URL: first.URL, Model: "a"},
		Provider{Name: "b", URL: second.URL, Model: "b"},
	)
	h := NewServer(cfg, logger, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "fast"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, logger.calls, 2)
	require.Equal(t, "a", logger.calls[0].Provider)
	require.Equal(t, http.StatusBadGateway, logger.calls[0].HTTPStatus)
	require.Contains(t, logger.calls[0].Error, "502")
	require.Contains(t, string(logger.calls[0].ResponseJSON), "bad gateway")
	require.Equal(t, "b", logger.calls[1].Provider)
	require.Equal(t, http.StatusOK, logger.calls[1].HTTPStatus)
	require.Empty(t, logger.calls[1].Error)
}

func TestProxyLogsDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadURL := "http://" + ln.Addr().String() + "/v1/chat/completions"
	require.NoError(t, ln.Close())

	logger := &memoryCallLogger{}
	h := NewServer(cfgFast(Provider{Name: "a", URL: deadURL, Model: "a"}), logger, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "fast"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Len(t, logger.calls, 1)
	require.Equal(t, 0, logger.calls[0].HTTPStatus)
	require.NotEmpty(t, logger.calls[0].Error)
	require.Contains(t, string(logger.calls[0].RequestJSON), `"model":"a"`)
}

func TestProxyDoesNotFailoverAfterResponseStarted(t *testing.T) {
	var secondHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"partial":true}`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	}))
	t.Cleanup(first.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"content":"backup"}}]}`)
	}))
	t.Cleanup(second.Close)

	logger := &memoryCallLogger{}
	h := NewServer(cfgFast(
		Provider{Name: "a", URL: first.URL, Model: "a"},
		Provider{Name: "b", URL: second.URL, Model: "b"},
	), logger, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "fast"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int32(0), secondHits.Load())
	require.Contains(t, rec.Body.String(), `"partial":true`)
	require.NotContains(t, rec.Body.String(), "backup")
	require.Len(t, logger.calls, 1)
	require.Equal(t, "a", logger.calls[0].Provider)
}

func TestProxyLogsStreamUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)

	logger := &memoryCallLogger{}
	h := NewServer(cfgFast(Provider{Name: "m", URL: up.URL, Model: "m"}), logger, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{
		"model":  "fast",
		"stream": true,
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, logger.calls, 1)
	require.Equal(t, int64(4), logger.calls[0].InputTokens)
	require.Equal(t, int64(1), logger.calls[0].OutputTokens)
	require.Contains(t, rec.Body.String(), "[DONE]")
}

func TestCapBuffer(t *testing.T) {
	buf := &capBuffer{max: 8}
	n, err := io.Copy(buf, bytes.NewReader(bytes.Repeat([]byte("x"), 32)))
	require.NoError(t, err)
	require.Equal(t, int64(32), n)
	require.Equal(t, 8, len(buf.Bytes()))

	var dst bytes.Buffer
	mw := io.MultiWriter(&dst, &capBuffer{max: 4})
	_, err = io.Copy(mw, bytes.NewReader([]byte("hello world")))
	require.NoError(t, err)
	require.Equal(t, "hello world", dst.String())
}

func TestProxySkipsOtherFormatProviders(t *testing.T) {
	var openaiHits, anthHits atomic.Int32
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"oai"}}]}`)
	}))
	t.Cleanup(openaiUp.Close)
	anthUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"anth"}]}`)
	}))
	t.Cleanup(anthUp.Close)

	cfg := &Config{
		APIKeys: []APIKey{{Name: "test", Value: testAPIKey}},
		Models: []ModelConfig{{Name: "claude", Providers: []Provider{
			{Name: "anthropic", Format: formatAnthropic, URL: anthUp.URL, Model: "claude-sonnet-4-5"},
			{Name: "openrouter", Format: formatOpenAI, URL: openaiUp.URL, Model: "anthropic/claude-sonnet-4-5"},
		}}},
	}
	h := NewServer(cfg, nil, nil).Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "claude"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "oai")
	require.Equal(t, int32(1), openaiHits.Load())
	require.Equal(t, int32(0), anthHits.Load())

	req = httptest.NewRequest(http.MethodPost, "/v1/messages", jsonBody(map[string]any{
		"model": "claude",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}))
	req.Header.Set("X-Api-Key", testAPIKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "anth")
	require.Equal(t, int32(1), openaiHits.Load())
	require.Equal(t, int32(1), anthHits.Load())
}

func TestProxyNestedProviderFormats(t *testing.T) {
	var openaiHits, anthHits atomic.Int32
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"oai"}}]}`)
	}))
	t.Cleanup(openaiUp.Close)
	anthUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"anth"}]}`)
	}))
	t.Cleanup(anthUp.Close)

	logger := &memoryCallLogger{}
	cfg := &Config{
		APIKeys: []APIKey{{Name: "test", Value: testAPIKey}},
		Models: []ModelConfig{{Name: "claude", Providers: []Provider{{
			Name:      "openrouter",
			Model:     "anthropic/claude-sonnet-4-5",
			OpenAI:    &ProviderEndpoint{URL: openaiUp.URL},
			Anthropic: &ProviderEndpoint{URL: anthUp.URL},
		}}}},
	}
	h := NewServer(cfg, logger, nil).Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "claude"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "oai")
	require.Equal(t, int32(1), openaiHits.Load())
	require.Equal(t, int32(0), anthHits.Load())

	req = httptest.NewRequest(http.MethodPost, "/v1/messages", jsonBody(map[string]any{
		"model": "claude",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}))
	req.Header.Set("X-Api-Key", testAPIKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "anth")
	require.Equal(t, int32(1), openaiHits.Load())
	require.Equal(t, int32(1), anthHits.Load())
	require.Len(t, logger.calls, 2)
	require.Equal(t, "openrouter", logger.calls[0].Provider)
	require.Equal(t, "openrouter", logger.calls[1].Provider)
}

func TestProxySkipsProviderWithoutMatchingBlock(t *testing.T) {
	var anthHits atomic.Int32
	anthUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"anth"}]}`)
	}))
	t.Cleanup(anthUp.Close)

	cfg := &Config{
		APIKeys: []APIKey{{Name: "test", Value: testAPIKey}},
		Models: []ModelConfig{{Name: "claude", Providers: []Provider{{
			Name:      "anthropic",
			Anthropic: &ProviderEndpoint{URL: anthUp.URL, Model: "claude-sonnet-4-5"},
		}}}},
	}
	h := NewServer(cfg, nil, nil).Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "claude"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "has no openai providers")
	require.Equal(t, int32(0), anthHits.Load())
}

func TestProxyAnthropicMissingProviders(t *testing.T) {
	h := testProxy(t, cfgFast(Provider{Name: "x", URL: "http://example.invalid", Model: "x"}))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", jsonBody(map[string]any{"model": "fast"}))
	req.Header.Set("X-Api-Key", testAPIKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), `"type":"error"`)
	require.Contains(t, rec.Body.String(), "has no anthropic providers")
	require.NotContains(t, rec.Body.String(), "invalid_request_error")
}

func TestProxyOpenAIMissingProviders(t *testing.T) {
	h := testProxy(t, &Config{
		APIKeys: []APIKey{{Name: "test", Value: testAPIKey}},
		Models: []ModelConfig{{Name: "claude", Providers: []Provider{
			{Name: "anthropic", Format: formatAnthropic, URL: "http://example.invalid", Model: "claude-sonnet-4-5"},
		}}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "claude"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "has no openai providers")
}

func TestProxyAnthropicRewritesModelAndStripsClientKey(t *testing.T) {
	var gotBody []byte
	var gotKey string
	var gotVersion string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotKey = r.Header.Get("X-Api-Key")
		gotVersion = r.Header.Get("Anthropic-Version")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","model":"claude-sonnet-4-5","usage":{"input_tokens":4,"output_tokens":2}}`)
	}))
	t.Cleanup(up.Close)

	logger := &memoryCallLogger{}
	cfg := &Config{
		APIKeys: []APIKey{{Name: "test", Value: testAPIKey}},
		Models: []ModelConfig{{Name: "claude", Providers: []Provider{{
			Name:   "anthropic",
			Format: formatAnthropic,
			URL:    up.URL,
			Model:  "claude-sonnet-4-5",
			Headers: map[string]string{
				"x-api-key":         "sk-up",
				"anthropic-version": "2023-06-01",
			},
		}}}},
	}
	h := NewServer(cfg, logger, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", jsonBody(map[string]any{
		"model":  "claude",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}))
	req.Header.Set("X-Api-Key", testAPIKey)
	req.Header.Set("Anthropic-Version", "2023-01-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var sent map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sent))
	require.Equal(t, "claude-sonnet-4-5", sent["model"])
	_, hasStreamOpts := sent["stream_options"]
	require.False(t, hasStreamOpts)
	require.Equal(t, "sk-up", gotKey)
	require.Equal(t, "2023-06-01", gotVersion)
	require.Len(t, logger.calls, 1)
}

func TestProxyAnthropicStreamUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":6}}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n")
	}))
	t.Cleanup(up.Close)

	logger := &memoryCallLogger{}
	cfg := &Config{
		APIKeys: []APIKey{{Name: "test", Value: testAPIKey}},
		Models: []ModelConfig{{Name: "claude", Providers: []Provider{{
			Name: "anthropic", Format: formatAnthropic, URL: up.URL, Model: "claude-sonnet-4-5",
		}}}},
	}
	h := NewServer(cfg, logger, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", jsonBody(map[string]any{
		"model":  "claude",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}))
	req.Header.Set("X-Api-Key", testAPIKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, logger.calls, 1)
	require.Equal(t, int64(6), logger.calls[0].InputTokens)
	require.Equal(t, int64(2), logger.calls[0].OutputTokens)
}

func TestProxyPassesResponseBytesUnchanged(t *testing.T) {
	upstream := []byte(`{"id":"1","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":2},"extra":{"n":9007199254740993,"html":"<x>"}}`)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(upstream)
	}))
	t.Cleanup(up.Close)

	logger := &memoryCallLogger{}
	h := NewServer(cfgFast(Provider{Name: "primary", URL: up.URL, Model: "gpt-4o-mini"}), logger, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{"model": "fast"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, upstream, rec.Body.Bytes())
	require.Len(t, logger.calls, 1)
	require.Equal(t, int64(5), logger.calls[0].InputTokens)
	require.Equal(t, int64(2), logger.calls[0].OutputTokens)
}

func TestProxyPassesSSEBytesUnchanged(t *testing.T) {
	upstream := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"extra\":1}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1},\"html\":\"<x>\"}\n\n" +
		"data: [DONE]\n\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(upstream)
	}))
	t.Cleanup(up.Close)

	logger := &memoryCallLogger{}
	h := NewServer(cfgFast(Provider{Name: "m", URL: up.URL, Model: "m"}), logger, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{
		"model":  "fast",
		"stream": true,
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, upstream, rec.Body.Bytes())
	require.Len(t, logger.calls, 1)
	require.Equal(t, int64(3), logger.calls[0].InputTokens)
	require.Equal(t, int64(1), logger.calls[0].OutputTokens)
}

func TestProxyLogsCompactedSSEDeltas(t *testing.T) {
	upstream := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(upstream)
	}))
	t.Cleanup(up.Close)

	logger := &memoryCallLogger{}
	h := NewServer(cfgFast(Provider{Name: "m", URL: up.URL, Model: "m"}), logger, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", jsonBody(map[string]any{
		"model":  "fast",
		"stream": true,
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, upstream, rec.Body.Bytes())
	require.Len(t, logger.calls, 1)
	require.Equal(t, int64(3), logger.calls[0].InputTokens)
	require.Equal(t, int64(2), logger.calls[0].OutputTokens)

	var logged string
	require.NoError(t, json.Unmarshal(logger.calls[0].ResponseJSON, &logged))
	events := splitSSEEvents([]byte(logged))
	require.Len(t, events, 3)
	require.Equal(t, []string{"Hello"}, openaiDeltaStrings(t, events[0], "content"))
	require.Contains(t, logged, "[DONE]")
}

func TestProxyResponsesRewritesModelWithoutStreamOptions(t *testing.T) {
	var gotBody []byte
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		require.Equal(t, "/v1/responses", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_1","object":"response","model":"gpt-4o","usage":{"input_tokens":5,"output_tokens":2,"input_tokens_details":{"cached_tokens":1}}}`)
	}))
	t.Cleanup(up.Close)

	logger := &memoryCallLogger{}
	cfg := &Config{
		APIKeys: []APIKey{{Name: "test", Value: testAPIKey}},
		Models: []ModelConfig{{Name: "fast", Providers: []Provider{{
			Name:  "openai",
			Model: "gpt-4o",
			Headers: map[string]string{
				"Authorization": "Bearer sk-up",
			},
			OpenAI:    &ProviderEndpoint{URL: up.URL + "/v1/chat/completions"},
			Responses: &ProviderEndpoint{URL: up.URL + "/v1/responses"},
		}}}},
	}
	h := NewServer(cfg, logger, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", jsonBody(map[string]any{
		"model":  "fast",
		"stream": true,
		"input":  "hi",
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)

	var sent map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sent))
	require.Equal(t, "gpt-4o", sent["model"])
	require.Equal(t, "hi", sent["input"])
	_, hasStreamOpts := sent["stream_options"]
	require.False(t, hasStreamOpts)
	require.Equal(t, "Bearer sk-up", gotAuth)
	require.Len(t, logger.calls, 1)
	require.Equal(t, "openai", logger.calls[0].Provider)
	require.Equal(t, "gpt-4o", logger.calls[0].ProviderModel)
}

func TestProxyResponsesStreamUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-4o\",\"usage\":null}}\n\n")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-4o\",\"usage\":{\"input_tokens\":6,\"output_tokens\":2}}}\n\n")
	}))
	t.Cleanup(up.Close)

	logger := &memoryCallLogger{}
	cfg := &Config{
		APIKeys: []APIKey{{Name: "test", Value: testAPIKey}},
		Models: []ModelConfig{{Name: "fast", Providers: []Provider{{
			Name:      "openai",
			Responses: &ProviderEndpoint{URL: up.URL, Model: "gpt-4o"},
		}}}},
	}
	h := NewServer(cfg, logger, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", jsonBody(map[string]any{
		"model":  "fast",
		"stream": true,
		"input":  "hi",
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, logger.calls, 1)
	require.Equal(t, int64(6), logger.calls[0].InputTokens)
	require.Equal(t, int64(2), logger.calls[0].OutputTokens)
	require.Contains(t, rec.Body.String(), "response.completed")
}

func TestProxyResponsesMissingProviders(t *testing.T) {
	h := testProxy(t, cfgFast(Provider{Name: "x", URL: "http://example.invalid", Model: "x"}))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", jsonBody(map[string]any{"model": "fast", "input": "hi"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "has no responses providers")
	require.Contains(t, rec.Body.String(), "invalid_request_error")
}

func TestProxyResponsesDoesNotUseOpenAIURL(t *testing.T) {
	var openaiHits atomic.Int32
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"oai"}}]}`)
	}))
	t.Cleanup(openaiUp.Close)

	cfg := &Config{
		APIKeys: []APIKey{{Name: "test", Value: testAPIKey}},
		Models: []ModelConfig{{Name: "fast", Providers: []Provider{{
			Name:   "openai",
			OpenAI: &ProviderEndpoint{URL: openaiUp.URL, Model: "gpt-4o"},
		}}}},
	}
	h := NewServer(cfg, nil, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", jsonBody(map[string]any{"model": "fast", "input": "hi"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, int32(0), openaiHits.Load())
}

func TestProxyResponsesLegacyFormat(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	t.Cleanup(up.Close)

	logger := &memoryCallLogger{}
	cfg := &Config{
		APIKeys: []APIKey{{Name: "test", Value: testAPIKey}},
		Models: []ModelConfig{{Name: "fast", Providers: []Provider{{
			Name: "openai", Format: formatResponses, URL: up.URL, Model: "gpt-4o",
		}}}},
	}
	h := NewServer(cfg, logger, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/responses", jsonBody(map[string]any{"model": "fast", "input": "hi"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, logger.calls, 1)
	require.Equal(t, int64(3), logger.calls[0].InputTokens)
	require.Equal(t, int64(1), logger.calls[0].OutputTokens)
}

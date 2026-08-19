package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

var maxRequestBody int64 = 64 << 20

var hopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Proxy-Connection":    true,
}

type Proxy struct {
	Config *Config
	Client *http.Client
	Logger CallLogger
}

func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func (p *Proxy) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

func (p *Proxy) logCall(rec CallRecord) {
	if p.Logger == nil {
		return
	}
	p.Logger.Record(rec)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	meta, err := parseRequest(body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	providers := p.Config.providers(meta.Model)
	if len(providers) == 0 {
		writeAPIError(w, http.StatusNotFound, "invalid_request_error", "the model `"+meta.Model+"` does not exist")
		return
	}

	var lastStatus int
	var lastErr error

	for _, provider := range providers {
		status, err := p.forward(w, r, meta, provider)
		if isCatastrophic(err, status) {
			log.Printf("model %q provider %q catastrophically unavailable (%v, status=%d), trying next", meta.Model, provider.Name, err, status)
			lastStatus, lastErr = status, err
			continue
		}

		if status == 0 && err != nil {
			if !errors.Is(err, context.Canceled) {
				writeAPIError(w, statusFromErr(err), "upstream_error", err.Error())
			}
		}
		return
	}

	if lastErr != nil && lastStatus == 0 {
		writeAPIError(w, http.StatusBadGateway, "upstream_error", "all upstreams unavailable: "+lastErr.Error())
		return
	}
	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	writeAPIError(w, lastStatus, "upstream_error", "all upstreams unavailable")
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, meta *requestMeta, provider Provider) (int, error) {
	rec := CallRecord{
		Model:         meta.Model,
		Provider:      provider.Name,
		ProviderModel: provider.Model,
		APIKeyName:    apiKeyNameFrom(r.Context()),
		RequestJSON:   meta.Body,
	}
	if rec.ProviderModel == "" {
		rec.ProviderModel = meta.Model
	}

	payload, err := rewriteRequest(meta.Body, provider.Model, meta.Stream)
	if err != nil {
		rec.HTTPStatus = http.StatusBadRequest
		rec.Error = err.Error()
		p.logCall(rec)
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return http.StatusBadRequest, err
	}
	rec.RequestJSON = payload

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, provider.URL, bytes.NewReader(payload))
	if err != nil {
		rec.Error = err.Error()
		p.logCall(rec)
		return 0, err
	}
	copyRequestHeaders(req.Header, r.Header)
	for k, v := range provider.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Del("Accept-Encoding")
	req.ContentLength = int64(len(payload))

	resp, err := p.client().Do(req)
	if err != nil {
		rec.Error = err.Error()
		p.logCall(rec)
		return 0, err
	}
	defer resp.Body.Close()
	rec.HTTPStatus = resp.StatusCode
	sse := meta.Stream || isSSE(resp.Header.Get("Content-Type"))

	if isCatastrophic(nil, resp.StatusCode) {
		body, _ := io.ReadAll(resp.Body)
		rec.ResponseJSON = encodeResponseBlob(body, sse)
		rec.Error = "upstream status " + resp.Status
		p.logCall(rec)
		return resp.StatusCode, nil
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	var buf bytes.Buffer
	dst := io.Writer(w)
	if sse {
		dst = flushWriter{w}
	}
	_, err = io.Copy(dst, io.TeeReader(resp.Body, &buf))
	body := buf.Bytes()
	rec.ResponseJSON = encodeResponseBlob(body, sse)
	var usage tokenUsage
	if sse {
		usage = parseUsageSSE(body)
	} else {
		usage = parseUsageJSON(body)
	}
	rec.InputTokens = usage.Input
	rec.OutputTokens = usage.Output
	rec.CacheTokens = usage.CacheRead
	rec.UncachedTokens = usage.Uncached
	if err != nil {
		rec.Error = err.Error()
	}
	p.logCall(rec)
	return resp.StatusCode, err
}

type flushWriter struct {
	http.ResponseWriter
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	_ = http.NewResponseController(w.ResponseWriter).Flush()
	return n, err
}

func hopByHop(h http.Header) map[string]bool {
	skip := make(map[string]bool, len(hopHeaders)+8)
	for k, v := range hopHeaders {
		if v {
			skip[k] = v
		}
	}
	for _, f := range h.Values("Connection") {
		for _, tok := range strings.Split(f, ",") {
			if name := http.CanonicalHeaderKey(strings.TrimSpace(tok)); name != "" {
				skip[name] = true
			}
		}
	}
	return skip
}

func copyRequestHeaders(dst, src http.Header) {
	skip := hopByHop(src)
	skip["Authorization"] = true
	skip["X-Api-Key"] = true
	skip["Api-Key"] = true
	skip["Host"] = true
	skip["Content-Length"] = true
	skip["Accept-Encoding"] = true
	for k, vs := range src {
		if skip[http.CanonicalHeaderKey(k)] {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

func copyResponseHeaders(dst, src http.Header) {
	skip := hopByHop(src)
	for k, vs := range src {
		if skip[http.CanonicalHeaderKey(k)] {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

func isSSE(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

func statusFromErr(err error) int {
	var ne net.Error
	if (errors.As(err, &ne) && ne.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

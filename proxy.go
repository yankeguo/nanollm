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
	Format string
}

func (p *Proxy) format() string {
	if p.Format == "" {
		return formatOpenAI
	}
	return p.Format
}

func (p *Proxy) writeError(w http.ResponseWriter, status int, typ, message string) {
	writeFormatError(w, p.format(), status, typ, message)
}

func (p *Proxy) clientErrorType(notFound bool) string {
	if p.format() != formatAnthropic {
		return "invalid_request_error"
	}
	if notFound {
		return "not_found_error"
	}
	return "invalid_request_error"
}

func (p *Proxy) upstreamErrorType() string {
	if p.format() == formatAnthropic {
		return "api_error"
	}
	return "upstream_error"
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
			p.writeError(w, http.StatusRequestEntityTooLarge, p.clientErrorType(false), "request body too large")
			return
		}
		p.writeError(w, http.StatusBadRequest, p.clientErrorType(false), "failed to read request body")
		return
	}

	meta, err := parseRequest(body)
	if err != nil {
		p.writeError(w, http.StatusBadRequest, p.clientErrorType(false), err.Error())
		return
	}

	if p.Config.model(meta.Model) == nil {
		p.writeError(w, http.StatusNotFound, p.clientErrorType(true), "the model `"+meta.Model+"` does not exist")
		return
	}
	providers := p.Config.providersFor(meta.Model, p.format())
	if len(providers) == 0 {
		p.writeError(w, http.StatusNotFound, p.clientErrorType(true), "the model `"+meta.Model+"` has no "+p.format()+" providers")
		return
	}

	tw := &writeTracker{ResponseWriter: w}
	var lastStatus int
	var lastErr error

	for _, provider := range providers {
		status, err := p.forward(tw, r, meta, provider)
		if tw.wrote {
			return
		}
		if isCatastrophic(err, status) {
			log.Printf("model %q provider %q catastrophically unavailable (%v, status=%d), trying next", meta.Model, provider.Name, err, status)
			lastStatus, lastErr = status, err
			continue
		}

		if status == 0 && err != nil {
			if !errors.Is(err, context.Canceled) {
				p.writeError(w, statusFromErr(err), p.upstreamErrorType(), err.Error())
			}
		}
		return
	}

	if lastErr != nil && lastStatus == 0 {
		p.writeError(w, http.StatusBadGateway, p.upstreamErrorType(), "all upstreams unavailable: "+lastErr.Error())
		return
	}
	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	p.writeError(w, lastStatus, p.upstreamErrorType(), "all upstreams unavailable")
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

	payload, err := rewriteRequest(meta.Body, provider.Model, meta.Stream, p.format())
	if err != nil {
		rec.HTTPStatus = http.StatusBadRequest
		rec.Error = err.Error()
		p.logCall(rec)
		p.writeError(w, http.StatusBadRequest, p.clientErrorType(false), err.Error())
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(maxMediumBlob)))
		rec.ResponseJSON = encodeResponseBlob(body, sse)
		rec.Error = "upstream status " + resp.Status
		p.logCall(rec)
		return resp.StatusCode, nil
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	buf := &capBuffer{max: maxMediumBlob}
	var usage tokenUsage
	if sse {
		usage, err = copyAndScanSSE(io.MultiWriter(newFlushWriter(w), buf), resp.Body)
	} else {
		_, err = io.Copy(io.MultiWriter(w, buf), resp.Body)
		usage = parseUsageJSON(buf.Bytes())
	}
	rec.ResponseJSON = encodeResponseBlob(buf.Bytes(), sse)
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

type writeTracker struct {
	http.ResponseWriter
	wrote bool
}

func (w *writeTracker) WriteHeader(status int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *writeTracker) Write(p []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(p)
}

func (w *writeTracker) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

type flushWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newFlushWriter(w http.ResponseWriter) flushWriter {
	return flushWriter{w: w, rc: http.NewResponseController(w)}
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	_ = w.rc.Flush()
	return n, err
}

// capBuffer keeps at most max bytes for call-log blobs; extra writes succeed
// so io.MultiWriter can still copy the rest of the upstream body to the client.
type capBuffer struct {
	b   bytes.Buffer
	max int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if c.max <= 0 || c.b.Len() >= c.max {
		return n, nil
	}
	left := c.max - c.b.Len()
	if len(p) > left {
		p = p[:left]
	}
	if _, err := c.b.Write(p); err != nil {
		return 0, err
	}
	return n, nil
}

func (c *capBuffer) Bytes() []byte {
	return c.b.Bytes()
}

func hopByHop(h http.Header) map[string]bool {
	skip := make(map[string]bool, len(hopHeaders)+8)
	for k := range hopHeaders {
		skip[k] = true
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

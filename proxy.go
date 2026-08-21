package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

var maxRequestBody int64 = 64 << 20

// maxStatusBlob caps how much of an upstream error body is read (and stored)
// before failing over on a catastrophic status; the full body is not worth
// waiting for when the next provider should be tried promptly.
const maxStatusBlob = 256 << 10

var hopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
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

// defaultClient is shared by all proxies so upstream connections pool together.
var defaultClient = &http.Client{
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
		ExpectContinueTimeout: 1 * time.Second,
		// No ResponseHeaderTimeout: thinking models may not send headers until
		// the first token. Wait as long as the client stays connected.
	},
	// No total Timeout: streaming responses stay open as long as the upstream
	// keeps sending. SSE comments keep the client from idle-timing out.
}

func (p *Proxy) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return defaultClient
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
	var lastErr error

	for _, provider := range providers {
		_, err := p.forward(tw, r, meta, provider)
		if tw.wrote {
			return
		}
		if err == nil || errors.Is(err, context.Canceled) {
			return
		}
		if isCatastrophic(err, 0) {
			log.Printf("model %q provider %q catastrophically unavailable (%v), trying next", meta.Model, provider.Name, err)
			lastErr = err
			continue
		}
		p.writeError(w, statusFromErr(err), p.upstreamErrorType(), err.Error())
		return
	}

	p.writeError(w, http.StatusBadGateway, p.upstreamErrorType(), "all upstreams unavailable: "+lastErr.Error())
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, meta *requestMeta, provider Provider) (int, error) {
	upstreamURL, providerModel, headers, ok := provider.resolve(p.format())
	if !ok {
		err := fmt.Errorf("provider %q has no %s endpoint", provider.Name, p.format())
		rec := CallRecord{
			Model:       meta.Model,
			Provider:    provider.Name,
			APIKeyName:  apiKeyNameFrom(r.Context()),
			RequestJSON: meta.Body,
			Error:       err.Error(),
		}
		p.logCall(rec)
		return 0, err
	}

	rec := CallRecord{
		Model:         meta.Model,
		Provider:      provider.Name,
		ProviderModel: providerModel,
		APIKeyName:    apiKeyNameFrom(r.Context()),
		RequestJSON:   meta.Body,
	}
	if rec.ProviderModel == "" {
		rec.ProviderModel = meta.Model
	}

	payload, err := rewriteRequest(meta.Body, providerModel, meta.Stream, p.format())
	if err != nil {
		rec.HTTPStatus = http.StatusBadRequest
		rec.Error = err.Error()
		p.logCall(rec)
		p.writeError(w, http.StatusBadRequest, p.clientErrorType(false), err.Error())
		return http.StatusBadRequest, err
	}
	rec.RequestJSON = payload

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(payload))
	if err != nil {
		rec.Error = err.Error()
		p.logCall(rec)
		return 0, err
	}
	copyRequestHeaders(req.Header, r.Header)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Del("Accept-Encoding")
	req.ContentLength = int64(len(payload))

	resp, err := p.client().Do(req)
	if err != nil {
		rec.Error = copyErrText(err)
		p.logCall(rec)
		return 0, err
	}
	defer resp.Body.Close()
	rec.HTTPStatus = resp.StatusCode
	sse := meta.Stream || isSSE(resp.Header.Get("Content-Type"))

	if isCatastrophic(nil, resp.StatusCode) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxStatusBlob))
		rec.ResponseJSON = encodeResponseBlob(body, sse)
		rec.Error = "upstream status " + resp.Status
		p.logCall(rec)
		return resp.StatusCode, fmt.Errorf("upstream status %s", resp.Status)
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	buf := &capBuffer{max: maxMediumBlob}
	var usage tokenUsage
	if sse {
		fw := newFlushWriter(w)
		usage, err = copySSE(io.MultiWriter(fw, buf), fw, resp.Body, sseKeepaliveInterval)
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
		rec.Error = copyErrText(err)
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
	// Never leak client cookies (e.g. nanollm_admin) to third-party upstreams.
	skip["Cookie"] = true
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
	// Do not let an upstream plant cookies on this proxy's origin.
	skip["Set-Cookie"] = true
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

const errCanceled = "canceled"

func copyErrText(err error) string {
	if err == nil {
		return ""
	}
	if isClientDisconnect(err) {
		return errCanceled
	}
	return err.Error()
}

func isClientDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) {
		if errors.Is(op.Err, syscall.EPIPE) || errors.Is(op.Err, syscall.ECONNRESET) {
			return true
		}
	}
	s := err.Error()
	return strings.Contains(s, "broken pipe") || strings.Contains(s, "connection reset by peer") || strings.Contains(s, "use of closed network connection")
}

func statusFromErr(err error) int {
	var ne net.Error
	if (errors.As(err, &ne) && ne.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

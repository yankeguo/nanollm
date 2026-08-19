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

const maxRequestBody = 64 << 20

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
	Config  *Config
	Metrics *Metrics
	Client  *http.Client
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

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
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
		status, usage, err := p.forward(w, r, meta, provider)
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
		p.Metrics.record(r.Context(), meta.Model, provider, usage)
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

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, meta *requestMeta, provider Provider) (int, tokenUsage, error) {
	payload, err := rewriteRequest(meta.Body, provider.Model, meta.Stream)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return http.StatusBadRequest, tokenUsage{}, err
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, provider.URL, bytes.NewReader(payload))
	if err != nil {
		return 0, tokenUsage{}, err
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
		return 0, tokenUsage{}, err
	}
	defer resp.Body.Close()

	if isCatastrophic(nil, resp.StatusCode) {
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, tokenUsage{}, nil
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	var usage tokenUsage
	if meta.Stream || isSSE(resp.Header.Get("Content-Type")) {
		usage, err = copyAndScanSSE(flushWriter{w}, resp.Body)
	} else {
		var buf bytes.Buffer
		_, err = io.Copy(w, io.TeeReader(resp.Body, &buf))
		usage = parseUsageJSON(buf.Bytes())
	}
	if err != nil {
		return resp.StatusCode, usage, err
	}
	return resp.StatusCode, usage, nil
}

type flushWriter struct {
	http.ResponseWriter
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	_ = http.NewResponseController(w.ResponseWriter).Flush()
	return n, err
}

func copyRequestHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		switch http.CanonicalHeaderKey(k) {
		case "Authorization", "Host", "Content-Length", "Accept-Encoding":
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
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

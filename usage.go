package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

var maxSSEScan = 1 << 20

type tokenUsage struct {
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
	Uncached      int64
	ResponseModel string
}

func (u tokenUsage) empty() bool {
	return u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheCreation == 0 && u.Uncached == 0
}

func mergeUsage(dst *tokenUsage, src tokenUsage) {
	if src.empty() && src.ResponseModel == "" {
		return
	}
	if src.Input != 0 {
		dst.Input = src.Input
	}
	if src.Output != 0 {
		dst.Output = src.Output
	}
	if src.CacheRead != 0 {
		dst.CacheRead = src.CacheRead
	}
	if src.CacheCreation != 0 {
		dst.CacheCreation = src.CacheCreation
	}
	if src.ResponseModel != "" {
		dst.ResponseModel = src.ResponseModel
	}
	if src.Uncached != 0 {
		dst.Uncached = src.Uncached
	} else {
		dst.fillUncached()
	}
}

func (u *tokenUsage) fillUncached() {
	if u.Input == 0 {
		return
	}
	left := u.Input - u.CacheRead - u.CacheCreation
	if left < 0 {
		left = 0
	}
	u.Uncached = left
}

type openaiUsage struct {
	PromptTokens             int64 `json:"prompt_tokens"`
	CompletionTokens         int64 `json:"completion_tokens"`
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CachedTokens             int64 `json:"cached_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	PromptCacheHitTokens     int64 `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens    int64 `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails      *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	CacheCreation *struct {
		Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u openaiUsage) asTokenUsage() tokenUsage {
	cacheRead := u.CacheReadInputTokens
	if cacheRead == 0 {
		cacheRead = u.PromptCacheHitTokens
	}
	if cacheRead == 0 && u.PromptTokensDetails != nil {
		cacheRead = u.PromptTokensDetails.CachedTokens
	}
	if cacheRead == 0 && u.InputTokensDetails != nil {
		cacheRead = u.InputTokensDetails.CachedTokens
	}
	if cacheRead == 0 {
		cacheRead = u.CachedTokens
	}

	cacheCreation := u.CacheCreationInputTokens
	if cacheCreation == 0 && u.CacheCreation != nil {
		cacheCreation = u.CacheCreation.Ephemeral5mInputTokens + u.CacheCreation.Ephemeral1hInputTokens
	}

	in := u.PromptTokens
	if in == 0 {
		in = u.InputTokens
		// Anthropic reports input_tokens excluding cache reads/creations,
		// while OpenAI's prompt_tokens includes them. Fold the Anthropic
		// cache parts back so Input is always the full prompt size.
		in += u.CacheReadInputTokens + cacheCreation
	}
	out := u.CompletionTokens
	if out == 0 {
		out = u.OutputTokens
	}

	usage := tokenUsage{
		Input:         in,
		Output:        out,
		CacheRead:     cacheRead,
		CacheCreation: cacheCreation,
		Uncached:      u.PromptCacheMissTokens,
	}
	if usage.Uncached == 0 {
		usage.fillUncached()
	}
	return usage
}

type openaiNestedUsage struct {
	Model string       `json:"model"`
	Usage *openaiUsage `json:"usage"`
}

type openaiResponse struct {
	Model    string             `json:"model"`
	Usage    *openaiUsage       `json:"usage"`
	Message  *openaiNestedUsage `json:"message"`
	Response *openaiNestedUsage `json:"response"`
}

func mergeNestedUsage(out *tokenUsage, nested *openaiNestedUsage) {
	if nested == nil {
		return
	}
	if nested.Model != "" {
		out.ResponseModel = nested.Model
	}
	if nested.Usage != nil {
		u := nested.Usage.asTokenUsage()
		u.ResponseModel = nested.Model
		if u.ResponseModel == "" {
			u.ResponseModel = out.ResponseModel
		}
		mergeUsage(out, u)
	}
}

func parseUsageJSON(body []byte) tokenUsage {
	var resp openaiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return tokenUsage{}
	}
	var out tokenUsage
	// Anthropic-style {"type":"message_start","message":{"model":...,"usage":...}}
	mergeNestedUsage(&out, resp.Message)
	// OpenAI Responses SSE {"type":"response.completed","response":{"model":...,"usage":...}}
	mergeNestedUsage(&out, resp.Response)
	if resp.Model != "" {
		out.ResponseModel = resp.Model
	}
	if resp.Usage != nil {
		mergeUsage(&out, resp.Usage.asTokenUsage())
	}
	return out
}

const sseComment = ":\n\n"

// sseKeepaliveInterval is how often to emit an SSE comment while the upstream
// body is idle. Thinking models can sit silent for minutes after HTTP 200;
// without comments, clients and middle proxies idle-timeout and cancel us.
var sseKeepaliveInterval = 15 * time.Second

func parseUsageSSELine(line string) tokenUsage {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return tokenUsage{}
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return tokenUsage{}
	}
	return parseUsageJSON([]byte(payload))
}

func copyAndScanSSE(dst io.Writer, src io.Reader) (tokenUsage, error) {
	return copySSE(dst, nil, src, 0)
}

func copySSE(dst, ping io.Writer, src io.Reader, keepalive time.Duration) (tokenUsage, error) {
	if ping == nil {
		ping = dst
	}
	var usage tokenUsage
	var carry []byte
	buf := make([]byte, 32*1024)

	consume := func(chunk []byte) error {
		if _, err := dst.Write(chunk); err != nil {
			return err
		}
		carry = append(carry, chunk...)
		for {
			i := bytes.IndexByte(carry, '\n')
			if i < 0 {
				break
			}
			line := string(bytes.TrimRight(carry[:i], "\r"))
			carry = carry[i+1:]
			mergeUsage(&usage, parseUsageSSELine(line))
		}
		if len(carry) > maxSSEScan {
			carry = carry[:0]
		}
		return nil
	}

	finish := func(err error) (tokenUsage, error) {
		if err == nil || errors.Is(err, io.EOF) {
			if len(carry) > 0 {
				mergeUsage(&usage, parseUsageSSELine(string(carry)))
			}
			return usage, nil
		}
		return usage, err
	}

	if keepalive <= 0 {
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if werr := consume(buf[:n]); werr != nil {
					return usage, werr
				}
			}
			if err != nil {
				return finish(err)
			}
		}
	}

	type readOp struct {
		n   int
		err error
	}
	readc := make(chan readOp, 1)
	startRead := func() {
		go func() {
			n, err := src.Read(buf)
			readc <- readOp{n, err}
		}()
	}

	ticker := time.NewTicker(keepalive)
	defer ticker.Stop()
	startRead()
	for {
		select {
		case op := <-readc:
			ticker.Reset(keepalive)
			if op.n > 0 {
				if err := consume(buf[:op.n]); err != nil {
					return usage, err
				}
			}
			if op.err != nil {
				return finish(op.err)
			}
			startRead()
		case <-ticker.C:
			if _, err := ping.Write([]byte(sseComment)); err != nil {
				return usage, err
			}
		}
	}
}

func encodeResponseBlob(body []byte, sse bool) []byte {
	if len(body) == 0 {
		return nil
	}
	if !sse {
		return body
	}
	b, err := json.Marshal(string(body))
	if err != nil {
		return nil
	}
	return b
}

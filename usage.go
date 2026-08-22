package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// maxSSEScan caps an incomplete SSE line held for usage parsing. Responses
// `response.completed` embeds the full output object on one data line, so this
// matches the call-log MEDIUMBLOB cap rather than a 1MiB thinking-delta guess.
var maxSSEScan = maxMediumBlob

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

// jsonInt64 accepts JSON integers, integer-valued floats, and numeric strings.
// Some compatible gateways serialize usage counts as 1.0 or "11".
type jsonInt64 int64

func (n *jsonInt64) UnmarshalJSON(b []byte) error {
	if jsonBlank(b) {
		*n = 0
		return nil
	}
	b = bytes.TrimSpace(b)
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		b = bytes.TrimSpace([]byte(s))
		if len(b) == 0 {
			*n = 0
			return nil
		}
	}
	var i int64
	if err := json.Unmarshal(b, &i); err == nil {
		*n = jsonInt64(i)
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*n = jsonInt64(f)
	return nil
}

type usageFields struct {
	PromptTokens             jsonInt64 `json:"prompt_tokens"`
	CompletionTokens         jsonInt64 `json:"completion_tokens"`
	TotalTokens              jsonInt64 `json:"total_tokens"`
	InputTokens              jsonInt64 `json:"input_tokens"`
	OutputTokens             jsonInt64 `json:"output_tokens"`
	CachedTokens             jsonInt64 `json:"cached_tokens"`
	CacheReadInputTokens     jsonInt64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens jsonInt64 `json:"cache_creation_input_tokens"`
	PromptCacheHitTokens     jsonInt64 `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens    jsonInt64 `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails      *struct {
		CachedTokens jsonInt64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens jsonInt64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	CacheCreation *struct {
		Ephemeral5mInputTokens jsonInt64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens jsonInt64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u usageFields) asTokenUsage() tokenUsage {
	cacheRead := int64(u.CacheReadInputTokens)
	if cacheRead == 0 {
		cacheRead = int64(u.PromptCacheHitTokens)
	}
	if cacheRead == 0 && u.PromptTokensDetails != nil {
		cacheRead = int64(u.PromptTokensDetails.CachedTokens)
	}
	if cacheRead == 0 && u.InputTokensDetails != nil {
		cacheRead = int64(u.InputTokensDetails.CachedTokens)
	}
	if cacheRead == 0 {
		cacheRead = int64(u.CachedTokens)
	}

	cacheCreation := int64(u.CacheCreationInputTokens)
	if cacheCreation == 0 && u.CacheCreation != nil {
		cacheCreation = int64(u.CacheCreation.Ephemeral5mInputTokens + u.CacheCreation.Ephemeral1hInputTokens)
	}

	in := int64(u.PromptTokens)
	if in == 0 {
		in = int64(u.InputTokens)
		// Anthropic input_tokens is tokens after the last cache breakpoint
		// (not the full prompt). OpenAI prompt_tokens / Responses
		// input_tokens already include cached tokens and use *tokens_details
		// instead of cache_read_input_tokens, so this add is a no-op there.
		in += int64(u.CacheReadInputTokens) + cacheCreation
	}
	out := int64(u.CompletionTokens)
	if out == 0 {
		out = int64(u.OutputTokens)
	}
	// Embeddings (and some gateways) report only total_tokens, or omit
	// prompt_tokens. Do not let total_tokens override an already-parsed input.
	if in == 0 {
		total := int64(u.TotalTokens)
		if total > 0 {
			in = total - out
			if in < 0 {
				in = 0
			}
		}
	}

	usage := tokenUsage{
		Input:         in,
		Output:        out,
		CacheRead:     cacheRead,
		CacheCreation: cacheCreation,
		Uncached:      int64(u.PromptCacheMissTokens),
	}
	if usage.Uncached == 0 {
		usage.fillUncached()
	}
	return usage
}

type nestedUsage struct {
	Model string       `json:"model"`
	Usage *usageFields `json:"usage"`
}

type usageEnvelope struct {
	Model    string       `json:"model"`
	Usage    *usageFields `json:"usage"`
	Message  *nestedUsage `json:"message"`
	Response *nestedUsage `json:"response"`
}

func mergeNestedUsage(out *tokenUsage, nested *nestedUsage) {
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
	var resp usageEnvelope
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
const sseKeepaliveInterval = 15 * time.Second

func parseUsageSSELine(line []byte) tokenUsage {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return tokenUsage{}
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return tokenUsage{}
	}
	return parseUsageJSON(payload)
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
			line := bytes.TrimRight(carry[:i], "\r")
			carry = carry[i+1:]
			mergeUsage(&usage, parseUsageSSELine(line))
		}
		if len(carry) > maxSSEScan {
			mergeUsage(&usage, parseUsageSSELine(carry))
			carry = carry[:0]
		}
		return nil
	}

	finish := func(err error) (tokenUsage, error) {
		if err == nil || errors.Is(err, io.EOF) {
			if len(carry) > 0 {
				mergeUsage(&usage, parseUsageSSELine(carry))
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

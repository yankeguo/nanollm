package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
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
	in := u.PromptTokens
	if in == 0 {
		in = u.InputTokens
	}
	out := u.CompletionTokens
	if out == 0 {
		out = u.OutputTokens
	}

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

type openaiResponse struct {
	Model string       `json:"model"`
	Usage *openaiUsage `json:"usage"`
}

func parseUsageJSON(body []byte) tokenUsage {
	var resp openaiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return tokenUsage{}
	}
	out := tokenUsage{ResponseModel: resp.Model}
	if resp.Usage != nil {
		u := resp.Usage.asTokenUsage()
		out.Input = u.Input
		out.Output = u.Output
		out.CacheRead = u.CacheRead
		out.CacheCreation = u.CacheCreation
		out.Uncached = u.Uncached
	}
	return out
}

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
	var usage tokenUsage
	buf := make([]byte, 32*1024)
	var carry []byte
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := dst.Write(chunk); werr != nil {
				return usage, werr
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
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(carry) > 0 {
					mergeUsage(&usage, parseUsageSSELine(string(carry)))
				}
				return usage, nil
			}
			return usage, err
		}
	}
}

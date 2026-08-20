package main

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseUsageJSONOpenAI(t *testing.T) {
	u := parseUsageJSON([]byte(`{"model":"gpt-4o-mini","usage":{"prompt_tokens":11,"completion_tokens":7}}`))
	require.Equal(t, int64(11), u.Input)
	require.Equal(t, int64(7), u.Output)
	require.Equal(t, int64(11), u.Uncached)
	require.Equal(t, "gpt-4o-mini", u.ResponseModel)
}

func TestParseUsageJSONInputOutput(t *testing.T) {
	u := parseUsageJSON([]byte(`{"usage":{"input_tokens":3,"output_tokens":4}}`))
	require.Equal(t, int64(3), u.Input)
	require.Equal(t, int64(4), u.Output)
}

func TestParseUsageJSONOpenAICached(t *testing.T) {
	u := parseUsageJSON([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":80}}}`))
	require.Equal(t, int64(100), u.Input)
	require.Equal(t, int64(20), u.Output)
	require.Equal(t, int64(80), u.CacheRead)
	require.Equal(t, int64(20), u.Uncached)
}

func TestParseUsageJSONDeepSeekCache(t *testing.T) {
	u := parseUsageJSON([]byte(`{"usage":{"prompt_tokens":50,"completion_tokens":8,"prompt_cache_hit_tokens":40,"prompt_cache_miss_tokens":10}}`))
	require.Equal(t, int64(50), u.Input)
	require.Equal(t, int64(40), u.CacheRead)
	require.Equal(t, int64(10), u.Uncached)
}

func TestParseUsageJSONAnthropicCache(t *testing.T) {
	u := parseUsageJSON([]byte(`{"usage":{"input_tokens":120,"output_tokens":9,"cache_read_input_tokens":90,"cache_creation_input_tokens":10}}`))
	require.Equal(t, int64(120), u.Input)
	require.Equal(t, int64(90), u.CacheRead)
	require.Equal(t, int64(10), u.CacheCreation)
	require.Equal(t, int64(20), u.Uncached)
}

func TestCopyAndScanSSE(t *testing.T) {
	src := bytes.NewBufferString("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"model\":\"gpt-4o\",\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":6}}}\n\n" +
		"data: [DONE]\n\n")
	var dst bytes.Buffer
	u, err := copyAndScanSSE(&dst, src)
	require.NoError(t, err)
	require.Equal(t, int64(9), u.Input)
	require.Equal(t, int64(2), u.Output)
	require.Equal(t, int64(6), u.CacheRead)
	require.Equal(t, int64(3), u.Uncached)
	require.Equal(t, "gpt-4o", u.ResponseModel)
	require.Contains(t, dst.String(), "[DONE]")
}

func TestCopySSEKeepalive(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })
	go func() {
		time.Sleep(40 * time.Millisecond)
		_, _ = pw.Write([]byte("data: {\"usage\":{\"prompt_tokens\":2}}\n\n"))
		_ = pw.Close()
	}()
	var data, pings bytes.Buffer
	u, err := copySSE(&data, &pings, pr, 10*time.Millisecond)
	require.NoError(t, err)
	require.Contains(t, pings.String(), ":")
	require.NotContains(t, data.String(), sseComment)
	require.Contains(t, data.String(), "prompt_tokens")
	require.Equal(t, int64(2), u.Input)
}

func TestCopyAndScanSSECapsScanBuffer(t *testing.T) {
	orig := maxSSEScan
	maxSSEScan = 16
	t.Cleanup(func() { maxSSEScan = orig })

	src := bytes.NewBuffer(bytes.Repeat([]byte("x"), 1000))
	var dst bytes.Buffer
	u, err := copyAndScanSSE(&dst, src)
	require.NoError(t, err)
	require.True(t, u.empty())
	require.Equal(t, 1000, dst.Len())
}

// parseUsageSSE parses an entire SSE transcript; the production path streams
// through copyAndScanSSE, so this helper lives only in tests.
func parseUsageSSE(body []byte) tokenUsage {
	var usage tokenUsage
	for {
		i := bytes.IndexByte(body, '\n')
		if i < 0 {
			if len(body) > 0 {
				mergeUsage(&usage, parseUsageSSELine(string(body)))
			}
			return usage
		}
		line := string(bytes.TrimRight(body[:i], "\r"))
		body = body[i+1:]
		mergeUsage(&usage, parseUsageSSELine(line))
	}
}

func TestParseUsageSSE(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n")
	u := parseUsageSSE(body)
	require.Equal(t, int64(4), u.Input)
	require.Equal(t, int64(1), u.Output)
}

func TestEncodeResponseBlob(t *testing.T) {
	require.Nil(t, encodeResponseBlob(nil, false))
	js := []byte(`{"ok":true}`)
	require.Equal(t, js, encodeResponseBlob(js, false))
	sse := []byte("data: hi\n\n")
	enc := encodeResponseBlob(sse, true)
	require.Equal(t, `"data: hi\n\n"`, string(enc))
}

func TestParseUsageJSONAnthropicMessageStart(t *testing.T) {
	u := parseUsageJSON([]byte(`{"type":"message_start","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":12,"cache_read_input_tokens":4,"cache_creation_input_tokens":1}}}`))
	require.Equal(t, int64(12), u.Input)
	require.Equal(t, int64(4), u.CacheRead)
	require.Equal(t, int64(1), u.CacheCreation)
	require.Equal(t, int64(7), u.Uncached)
	require.Equal(t, "claude-sonnet-4-5", u.ResponseModel)
}

func TestParseUsageSSEAnthropic(t *testing.T) {
	body := []byte("event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":2}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n")
	u := parseUsageSSE(body)
	require.Equal(t, int64(10), u.Input)
	require.Equal(t, int64(3), u.Output)
	require.Equal(t, int64(2), u.CacheRead)
	require.Equal(t, int64(8), u.Uncached)
	require.Equal(t, "claude-sonnet-4-5", u.ResponseModel)
}

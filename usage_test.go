package main

import (
	"bytes"
	"io"
	"strings"
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
	require.Equal(t, int64(220), u.Input)
	require.Equal(t, int64(90), u.CacheRead)
	require.Equal(t, int64(10), u.CacheCreation)
	require.Equal(t, int64(120), u.Uncached)
}

func TestCopyAndScanSSE(t *testing.T) {
	src := bytes.NewBufferString("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"model\":\"gpt-4o\",\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":6}}}\n\n" +
		"data: [DONE]\n\n")
	var dst bytes.Buffer
	u, first, err := copySSE(&dst, nil, src, 0)
	require.NoError(t, err)
	require.False(t, first.IsZero())
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
	start := time.Now()
	u, first, err := copySSE(&data, &pings, pr, 10*time.Millisecond)
	require.NoError(t, err)
	require.Contains(t, pings.String(), ":")
	require.NotContains(t, data.String(), sseComment)
	require.Contains(t, data.String(), "prompt_tokens")
	require.Equal(t, int64(2), u.Input)
	// Keepalive comments must not count as the first token.
	require.GreaterOrEqual(t, first.Sub(start), 25*time.Millisecond)
}

func TestCopyAndScanSSECapsScanBuffer(t *testing.T) {
	orig := maxSSEScan
	maxSSEScan = 16
	t.Cleanup(func() { maxSSEScan = orig })

	src := bytes.NewBuffer(bytes.Repeat([]byte("x"), 1000))
	var dst bytes.Buffer
	u, first, err := copySSE(&dst, nil, src, 0)
	require.NoError(t, err)
	require.True(t, u.empty())
	require.True(t, first.IsZero())
	require.Equal(t, 1000, dst.Len())
}

// parseUsageSSE parses an entire SSE transcript; the production path streams
// through copySSE, so this helper lives only in tests.
func parseUsageSSE(body []byte) tokenUsage {
	var usage tokenUsage
	for {
		i := bytes.IndexByte(body, '\n')
		if i < 0 {
			if len(body) > 0 {
				mergeUsage(&usage, parseUsageSSELine(body))
			}
			return usage
		}
		line := bytes.TrimRight(body[:i], "\r")
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
	require.Equal(t, int64(17), u.Input)
	require.Equal(t, int64(4), u.CacheRead)
	require.Equal(t, int64(1), u.CacheCreation)
	require.Equal(t, int64(12), u.Uncached)
	require.Equal(t, "claude-sonnet-4-5", u.ResponseModel)
}

func TestParseUsageJSONResponses(t *testing.T) {
	u := parseUsageJSON([]byte(`{"id":"resp_1","object":"response","model":"gpt-4o","usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":4}}}`))
	require.Equal(t, int64(11), u.Input)
	require.Equal(t, int64(7), u.Output)
	require.Equal(t, int64(4), u.CacheRead)
	require.Equal(t, int64(7), u.Uncached)
	require.Equal(t, "gpt-4o", u.ResponseModel)
}

func TestParseUsageSSEAnthropic(t *testing.T) {
	body := []byte("event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":2}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n")
	u := parseUsageSSE(body)
	require.Equal(t, int64(12), u.Input)
	require.Equal(t, int64(3), u.Output)
	require.Equal(t, int64(2), u.CacheRead)
	require.Equal(t, int64(10), u.Uncached)
	require.Equal(t, "claude-sonnet-4-5", u.ResponseModel)
}

func TestParseUsageSSEResponses(t *testing.T) {
	body := []byte("event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-4o\",\"usage\":null}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-4o\",\"usage\":{\"input_tokens\":9,\"output_tokens\":2,\"input_tokens_details\":{\"cached_tokens\":3}}}}\n\n")
	u := parseUsageSSE(body)
	require.Equal(t, int64(9), u.Input)
	require.Equal(t, int64(2), u.Output)
	require.Equal(t, int64(3), u.CacheRead)
	require.Equal(t, int64(6), u.Uncached)
	require.Equal(t, "gpt-4o", u.ResponseModel)
}

func TestParseUsageJSONFlexibleNumbers(t *testing.T) {
	u := parseUsageJSON([]byte(`{"usage":{"prompt_tokens":11.0,"completion_tokens":"7"}}`))
	require.Equal(t, int64(11), u.Input)
	require.Equal(t, int64(7), u.Output)
}

func TestParseUsageJSONCompletions(t *testing.T) {
	u := parseUsageJSON([]byte(`{"id":"cmpl_1","object":"text_completion","model":"gpt-3.5-turbo-instruct","choices":[{"text":"hi","index":0}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`))
	require.Equal(t, int64(4), u.Input)
	require.Equal(t, int64(1), u.Output)
	require.Equal(t, "gpt-3.5-turbo-instruct", u.ResponseModel)
}

func TestParseUsageJSONEmbeddings(t *testing.T) {
	u := parseUsageJSON([]byte(`{"object":"list","model":"text-embedding-3-small","data":[{"embedding":[0.1],"index":0}],"usage":{"prompt_tokens":8,"total_tokens":8}}`))
	require.Equal(t, int64(8), u.Input)
	require.Equal(t, int64(0), u.Output)
	require.Equal(t, int64(8), u.Uncached)
	require.Equal(t, "text-embedding-3-small", u.ResponseModel)
}

func TestParseUsageJSONBailianTongyiVision(t *testing.T) {
	// tongyi-embedding-vision-*: input_tokens already includes visual tokens;
	// input_tokens_details is a breakdown and must not be counted twice.
	u := parseUsageJSON([]byte(`{"output":{"embeddings":[{"index":0,"embedding":[0.1],"type":"text"}]},"usage":{"input_tokens":903,"input_tokens_details":{"image_tokens":896,"text_tokens":7},"output_tokens":3,"total_tokens":906},"request_id":"req-1"}`))
	require.Equal(t, int64(903), u.Input)
	require.Equal(t, int64(3), u.Output)
	require.Equal(t, int64(903), u.Uncached)
}

func TestParseUsageJSONBailianQwenImageTokens(t *testing.T) {
	// qwen3-vl-embedding: input_tokens is text-only; top-level image_tokens
	// is separate and total_tokens is their sum.
	u := parseUsageJSON([]byte(`{"usage":{"input_tokens":43,"image_tokens":1247,"total_tokens":1290}}`))
	require.Equal(t, int64(1290), u.Input)
	require.Equal(t, int64(0), u.Output)
}

func TestParseUsageJSONBailianV1(t *testing.T) {
	// multimodal-embedding-v1: no total_tokens, top-level image_tokens only.
	u := parseUsageJSON([]byte(`{"usage":{"input_tokens":7,"image_tokens":896,"image_count":1,"duration":0}}`))
	require.Equal(t, int64(903), u.Input)
	require.Equal(t, int64(0), u.Output)
}

func TestParseUsageJSONEmbeddingsTotalOnly(t *testing.T) {
	u := parseUsageJSON([]byte(`{"object":"list","usage":{"total_tokens":5}}`))
	require.Equal(t, int64(5), u.Input)
	require.Equal(t, int64(0), u.Output)
	require.Equal(t, int64(5), u.Uncached)
}

func TestParseUsageJSONTotalTokensDoesNotOverridePrompt(t *testing.T) {
	u := parseUsageJSON([]byte(`{"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
	require.Equal(t, int64(11), u.Input)
	require.Equal(t, int64(7), u.Output)
	require.Equal(t, int64(11), u.Uncached)
}

func TestParseUsageJSONTotalMinusCompletion(t *testing.T) {
	u := parseUsageJSON([]byte(`{"usage":{"completion_tokens":7,"total_tokens":18}}`))
	require.Equal(t, int64(11), u.Input)
	require.Equal(t, int64(7), u.Output)
}

func TestParseUsageJSONResponsesDoesNotFoldCacheRead(t *testing.T) {
	// Responses input_tokens already includes cached_tokens; cache_read_input_tokens is Anthropic-only.
	u := parseUsageJSON([]byte(`{"usage":{"input_tokens":11,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}}`))
	require.Equal(t, int64(11), u.Input)
	require.Equal(t, int64(4), u.CacheRead)
	require.Equal(t, int64(7), u.Uncached)
}

func TestCopyAndScanSSEResponsesLargeCompleted(t *testing.T) {
	payload := `{"type":"response.completed","response":{"model":"gpt-4o","output":[{"content":[{"text":"` + strings.Repeat("x", 64*1024) + `"}]}],"usage":{"input_tokens":9,"output_tokens":2}}}`
	src := bytes.NewBufferString("event: response.completed\ndata: " + payload + "\n\n")
	var dst bytes.Buffer
	u, first, err := copySSE(&dst, nil, src, 0)
	require.NoError(t, err)
	require.False(t, first.IsZero())
	require.Equal(t, int64(9), u.Input)
	require.Equal(t, int64(2), u.Output)
	require.Equal(t, "gpt-4o", u.ResponseModel)
}

func TestParseUsageJSONOllama(t *testing.T) {
	u := parseUsageJSON([]byte(`{"model":"qwen3:8b","message":{"role":"assistant","content":"ok"},"done":true,"prompt_eval_count":26,"eval_count":7}`))
	require.Equal(t, int64(26), u.Input)
	require.Equal(t, int64(7), u.Output)
	require.Equal(t, int64(26), u.Uncached)
	require.Equal(t, "qwen3:8b", u.ResponseModel)
}

func TestCopyAndScanNDJSON(t *testing.T) {
	body := "{\"model\":\"qwen3:8b\",\"message\":{\"role\":\"assistant\",\"content\":\"Hel\"},\"done\":false}\n" +
		"{\"model\":\"qwen3:8b\",\"message\":{\"role\":\"assistant\",\"content\":\"lo\"},\"done\":false}\n" +
		"{\"model\":\"qwen3:8b\",\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":true,\"prompt_eval_count\":26,\"eval_count\":7}\n"
	src := bytes.NewBufferString(body)
	var dst bytes.Buffer
	u, first, err := copyNDJSON(&dst, src)
	require.NoError(t, err)
	require.False(t, first.IsZero())
	require.Equal(t, int64(26), u.Input)
	require.Equal(t, int64(7), u.Output)
	require.Equal(t, "qwen3:8b", u.ResponseModel)
	require.Equal(t, body, dst.String())
}

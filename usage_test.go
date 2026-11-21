package main

import (
	"bytes"
	"encoding/json"
	"errors"
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
	u, first, completedAt, _, err := copySSE(&dst, nil, src, 0)
	require.NoError(t, err)
	require.False(t, first.IsZero())
	require.False(t, completedAt.IsZero())
	require.Equal(t, int64(9), u.Input)
	require.Equal(t, int64(2), u.Output)
	require.Equal(t, int64(6), u.CacheRead)
	require.Equal(t, int64(3), u.Uncached)
	require.Equal(t, "gpt-4o", u.ResponseModel)
	require.Contains(t, dst.String(), "[DONE]")
}

func TestCopySSESkipsNonTokenEvents(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "OpenAI usage and done",
			body: "data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "OpenAI metadata and done",
			body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "Anthropic lifecycle",
			body: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":6}}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
		{
			name: "Responses lifecycle",
			body: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"usage\":null}}\n\n" +
				"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{}}\n\n" +
				"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\"}}\n\n" +
				"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"item_id\":\"msg_1\",\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n" +
				"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"item_id\":\"msg_1\",\"text\":\"\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"output_tokens\":2}}}\n\n",
		},
		{
			name: "error",
			body: "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"boom\"}}\n\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst bytes.Buffer
			_, first, _, _, err := copySSE(&dst, nil, bytes.NewBufferString(tc.body), 0)
			require.NoError(t, err)
			require.True(t, first.IsZero())
			require.Equal(t, tc.body, dst.String())
		})
	}
}

func TestCopySSEKeepalive(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })
	go func() {
		time.Sleep(40 * time.Millisecond)
		_, _ = pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":2}}\n\n"))
		_ = pw.Close()
	}()
	var data, pings bytes.Buffer
	start := time.Now()
	u, first, _, _, err := copySSE(&data, &pings, pr, 10*time.Millisecond)
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
	u, first, _, _, err := copySSE(&dst, nil, src, 0)
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
	u, first, completedAt, _, err := copySSE(&dst, nil, src, 0)
	require.NoError(t, err)
	require.True(t, first.IsZero())
	require.False(t, completedAt.IsZero())
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

func TestCopyNDJSONSkipsNonTokenLines(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "error", body: "{\"error\":\"boom\"}\n"},
		{name: "done with usage", body: "{\"done\":true,\"prompt_eval_count\":2,\"eval_count\":10}\n"},
		{name: "role only", body: "{\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":false}\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst bytes.Buffer
			_, first, _, _, err := copyNDJSON(&dst, bytes.NewBufferString(tc.body))
			require.NoError(t, err)
			require.True(t, first.IsZero())
			require.Equal(t, tc.body, dst.String())
		})
	}
}

func TestCopySSEFirstTokenEvents(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "content delta", body: "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n", want: true},
		{name: "reasoning delta", body: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n", want: true},
		{name: "tool call delta", body: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0}]}}]}\n\n", want: true},
		{name: "legacy completions text", body: "data: {\"choices\":[{\"text\":\"a\"}]}\n\n", want: true},
		{name: "non-standard message wrapper", body: "data: {\"choices\":[{\"message\":{\"content\":\"a\"}}]}\n\n", want: true},
		{name: "non-json data", body: "data: hello\n\n", want: true},
		{name: "done only", body: "data: [DONE]\n\n", want: false},
		{name: "role only", body: "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n", want: false},
		{name: "finish only", body: "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst bytes.Buffer
			_, first, _, _, err := copySSE(&dst, nil, bytes.NewBufferString(tc.body), 0)
			require.NoError(t, err)
			require.Equal(t, tc.want, !first.IsZero())
		})
	}
}

func TestCopyNDJSONFirstTokenLines(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "content", body: "{\"message\":{\"content\":\"a\"},\"done\":false}\n", want: true},
		{name: "thinking", body: "{\"message\":{\"thinking\":\"hmm\"},\"done\":false}\n", want: true},
		{name: "tool call", body: "{\"message\":{\"tool_calls\":[{\"function\":{\"name\":\"x\"}}]},\"done\":false}\n", want: true},
		{name: "generate response", body: "{\"response\":\"a\",\"done\":false}\n", want: true},
		{name: "unknown envelope", body: "{\"delta\":\"a\"}\n", want: true},
		{name: "done", body: "{\"done\":true,\"eval_count\":1}\n", want: false},
		{name: "done false only", body: "{\"done\":false}\n", want: false},
		{name: "error", body: "{\"error\":\"boom\"}\n", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst bytes.Buffer
			_, first, _, _, err := copyNDJSON(&dst, bytes.NewBufferString(tc.body))
			require.NoError(t, err)
			require.Equal(t, tc.want, !first.IsZero())
		})
	}
}

func TestCopyAndScanNDJSON(t *testing.T) {
	body := "{\"model\":\"qwen3:8b\",\"message\":{\"role\":\"assistant\",\"content\":\"Hel\"},\"done\":false}\n" +
		"{\"model\":\"qwen3:8b\",\"message\":{\"role\":\"assistant\",\"content\":\"lo\"},\"done\":false}\n" +
		"{\"model\":\"qwen3:8b\",\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":true,\"prompt_eval_count\":26,\"eval_count\":7}\n"
	src := bytes.NewBufferString(body)
	var dst bytes.Buffer
	u, first, completedAt, _, err := copyNDJSON(&dst, src)
	require.NoError(t, err)
	require.False(t, first.IsZero())
	require.False(t, completedAt.IsZero())
	require.Equal(t, int64(26), u.Input)
	require.Equal(t, int64(7), u.Output)
	require.Equal(t, "qwen3:8b", u.ResponseModel)
	require.Equal(t, body, dst.String())
}

func TestIsSSEErrorLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{name: "event error", line: "event: error", want: true},
		{name: "event error no space", line: "event:error", want: true},
		{name: "event error trailing space", line: "event: error ", want: true},
		{name: "data error object", line: `data: {"type":"error","error":{"message":"boom"}}`, want: true},
		{name: "data error string", line: `data: {"error":"boom"}`, want: true},
		{name: "data error then done", line: `data: {"error":"boom","done":true}`, want: true},
		{name: "data error true", line: `data: {"error":true}`, want: true},
		{name: "data error non-empty array", line: `data: {"error":[{"message":"boom"}]}`, want: true},
		{name: "data type error no event line", line: `data: {"type":"error","code":"upstream_error","message":"boom"}`, want: true},
		{name: "data type error without error member", line: `data: {"type":"error"}`, want: true},
		{name: "data error null", line: `data: {"error":null}`, want: false},
		{name: "data error empty string", line: `data: {"error":""}`, want: false},
		{name: "data error empty object", line: `data: {"error":{}}`, want: false},
		{name: "data error spacey empty object", line: `data: {"error":{ }}`, want: false},
		{name: "data error empty array", line: `data: {"error":[]}`, want: false},
		{name: "data error false", line: `data: {"error":false}`, want: false},
		{name: "data error zero", line: `data: {"error":0}`, want: false},
		{name: "data error zero float", line: `data: {"error":0.0}`, want: false},
		{name: "normal delta", line: `data: {"choices":[{"delta":{"content":"a"}}]}`, want: false},
		{name: "normal chunk with null error", line: `data: {"choices":[{"delta":{"content":"a"}}],"error":null}`, want: false},
		{name: "normal chunk with empty error object", line: `data: {"choices":[{"delta":{"content":"a"}}],"error":{ }}`, want: false},
		{name: "data type message_stop", line: `data: {"type":"message_stop"}`, want: false},
		{name: "done", line: "data: [DONE]", want: false},
		{name: "comment", line: ":", want: false},
		{name: "event message_stop", line: "event: message_stop", want: false},
		{name: "non-json data", line: "data: hello", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isSSEErrorLine([]byte(tc.line)))
		})
	}
}

func TestIsNDJSONErrorLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{name: "ollama error", line: `{"error":"boom"}`, want: true},
		{name: "error with done", line: `{"error":"boom","done":true}`, want: true},
		{name: "error object", line: `{"error":{"message":"boom"}}`, want: true},
		{name: "normal chunk", line: `{"message":{"content":"a"},"done":false}`, want: false},
		{name: "done chunk", line: `{"done":true,"eval_count":2}`, want: false},
		{name: "null error", line: `{"error":null,"done":false}`, want: false},
		{name: "empty error", line: `{"error":"","done":false}`, want: false},
		{name: "empty object error", line: `{"error":{},"done":false}`, want: false},
		{name: "spacey empty object error", line: `{"error":{ },"done":false}`, want: false},
		{name: "empty array error", line: `{"error":[],"done":false}`, want: false},
		{name: "false error", line: `{"error":false,"done":false}`, want: false},
		{name: "zero error", line: `{"error":0,"done":false}`, want: false},
		{name: "non-json", line: `not-json`, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isNDJSONErrorLine([]byte(tc.line)))
		})
	}
}

// TestStreamErrorValue pins the JSON-semantic placeholder rule shared by the
// in-band-error and first-token detectors: formatting variants of empty values
// are placeholders, every non-empty or non-zero value is an error.
func TestStreamErrorValue(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "absent", raw: "", want: false},
		{name: "null", raw: "null", want: false},
		{name: "empty string", raw: `""`, want: false},
		{name: "empty object", raw: "{}", want: false},
		{name: "spacey empty object", raw: "{ }", want: false},
		{name: "empty array", raw: "[]", want: false},
		{name: "spacey empty array", raw: "[ ]", want: false},
		{name: "false", raw: "false", want: false},
		{name: "zero", raw: "0", want: false},
		{name: "negative zero", raw: "-0", want: false},
		{name: "zero float", raw: "0.0", want: false},
		{name: "zero exponent", raw: "0e0", want: false},
		{name: "string", raw: `"boom"`, want: true},
		{name: "true", raw: "true", want: true},
		{name: "object", raw: `{"message":"boom"}`, want: true},
		{name: "array", raw: `[{"message":"boom"}]`, want: true},
		{name: "non-zero number", raw: "1", want: true},
		{name: "negative number", raw: "-1", want: true},
		{name: "fraction", raw: "0.5", want: true},
		{name: "malformed", raw: "{", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, streamErrorValue(json.RawMessage(tc.raw)))
		})
	}
}

// TestCopySSEPlaceholderErrorKeepsFirstToken covers the case the placeholder
// rule exists for: a normal content chunk that carries an empty `error` member
// (in any JSON spelling) is still a token and not an in-band error, so TTFT
// survives.
func TestCopySSEPlaceholderErrorKeepsFirstToken(t *testing.T) {
	placeholders := []string{`""`, `{}`, `{ }`, `[]`, `false`, `0`, `null`}
	for _, placeholder := range placeholders {
		t.Run(placeholder, func(t *testing.T) {
			body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}],\"error\":" + placeholder + "}\n\n"
			var dst bytes.Buffer
			usg, firstToken, _, sawError, err := copySSE(&dst, nil, bytes.NewBufferString(body), 0)
			require.NoError(t, err)
			require.Equal(t, body, dst.String())
			require.True(t, usg.empty())
			require.False(t, firstToken.IsZero(), "placeholder error must not hide the first token")
			require.False(t, sawError, "placeholder error is not an in-band error event")
		})
	}
}

// TestCopyNDJSONPlaceholderErrorKeepsFirstToken is the NDJSON counterpart: an
// output chunk with an empty `error` member is still the first token.
func TestCopyNDJSONPlaceholderErrorKeepsFirstToken(t *testing.T) {
	placeholders := []string{`""`, `{}`, `{ }`, `[]`, `false`, `0`, `null`}
	for _, placeholder := range placeholders {
		t.Run(placeholder, func(t *testing.T) {
			body := "{\"message\":{\"role\":\"assistant\",\"content\":\"a\"},\"error\":" + placeholder + ",\"done\":false}\n"
			var dst bytes.Buffer
			_, firstToken, _, sawError, err := copyNDJSON(&dst, bytes.NewBufferString(body))
			require.NoError(t, err)
			require.Equal(t, body, dst.String())
			require.False(t, firstToken.IsZero(), "placeholder error must not hide the first token")
			require.False(t, sawError, "placeholder error is not an in-band error event")
		})
	}
}

// TestCopySSETracksStreamError covers the separate sawError signal: the copy
// itself succeeds (and the later marker is still seen) but the stream reported
// an error, so output speed must stay unmeasurable while TTFT survives.
func TestCopySSETracksStreamError(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "normal stream",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
				"data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":7}}\n\ndata: [DONE]\n\n",
			want: false,
		},
		{
			name: "error event then done",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
				"event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"boom\"}}\n\n" +
				"data: [DONE]\n\n",
			want: true,
		},
		{
			name: "inline error payload then done",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
				"data: {\"error\":{\"message\":\"boom\"}}\n\n" +
				"data: [DONE]\n\n",
			want: true,
		},
		{
			name: "data-only type error then done",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
				"data: {\"type\":\"error\",\"code\":\"upstream_error\",\"message\":\"boom\"}\n\n" +
				"data: [DONE]\n\n",
			want: true,
		},
		{
			name: "placeholder error then done",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"error\":{ }}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}],\"error\":0}\n\n" +
				"data: [DONE]\n\n",
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst bytes.Buffer
			_, firstToken, completedAt, sawError, err := copySSE(&dst, nil, bytes.NewBufferString(tc.body), 0)
			require.NoError(t, err)
			require.Equal(t, tc.body, dst.String())
			require.False(t, firstToken.IsZero(), "TTFT stays measurable for output before the error")
			require.False(t, completedAt.IsZero(), "the marker after the error is still observed")
			require.Equal(t, tc.want, sawError)
		})
	}
}

func TestCopyNDJSONTracksStreamError(t *testing.T) {
	first := "{\"message\":{\"role\":\"assistant\",\"content\":\"a\"},\"done\":false}\n"
	done := "{\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":true,\"prompt_eval_count\":2,\"eval_count\":7}\n"
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "normal stream", body: first + done, want: false},
		{name: "error line then done", body: first + "{\"error\":\"boom\"}\n" + done, want: true},
		{name: "error with done flag", body: first + "{\"error\":\"boom\",\"done\":true}\n" + done, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst bytes.Buffer
			u, firstToken, completedAt, sawError, err := copyNDJSON(&dst, bytes.NewBufferString(tc.body))
			require.NoError(t, err)
			require.Equal(t, tc.body, dst.String())
			require.False(t, firstToken.IsZero())
			require.False(t, completedAt.IsZero())
			require.Equal(t, tc.want, sawError)
			require.Equal(t, int64(7), u.Output)
		})
	}
}

// TestCopySSETerminalMarkerForms covers the accepted completion-marker shapes
// (and their false positives) end to end through the line pump: CRLF endings,
// a `data:` field without the optional space, a bare data `type`, `event:`
// lines, a final line with no trailing newline, and non-marker events.
func TestCopySSETerminalMarkerForms(t *testing.T) {
	first := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"
	tests := []struct {
		name       string
		body       string
		wantMarker bool
		wantError  bool
	}{
		{name: "done", body: first + "data: [DONE]\n\n", wantMarker: true},
		{name: "crlf endings", body: "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\r\n\r\ndata: [DONE]\r\n\r\n", wantMarker: true},
		{name: "done without space", body: first + "data:[DONE]\n", wantMarker: true},
		{name: "done at eof without newline", body: first + "data: [DONE]", wantMarker: true},
		{name: "data type message_stop", body: first + "data: {\"type\":\"message_stop\"}\n\n", wantMarker: true},
		{name: "data type response.completed", body: first + "data: {\"type\":\"response.completed\"}\n\n", wantMarker: true},
		{name: "event message_stop", body: first + "event: message_stop\n\n", wantMarker: true},
		{name: "event response.completed", body: first + "event: response.completed\n\n", wantMarker: true},
		{
			name:      "error event is not a marker",
			body:      first + "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"boom\"}}\n\n",
			wantError: true,
		},
		{name: "lifecycle event is not a marker", body: first + "data: {\"type\":\"message_delta\"}\n\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst bytes.Buffer
			_, firstToken, completedAt, sawError, err := copySSE(&dst, nil, bytes.NewBufferString(tc.body), 0)
			require.NoError(t, err)
			require.Equal(t, tc.body, dst.String())
			require.False(t, firstToken.IsZero())
			require.Equal(t, tc.wantMarker, !completedAt.IsZero())
			require.Equal(t, tc.wantError, sawError)
		})
	}
}

// recordingReader returns pre-set chunks and records when each Read starts, so
// stream-level "first marker wins" assertions need no timing tolerances.
type recordingReader struct {
	chunks []string
	delays []time.Duration
	reads  []time.Time
}

func (r *recordingReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	if d := r.delays[0]; d > 0 {
		time.Sleep(d)
	}
	r.reads = append(r.reads, time.Now())
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if len(r.chunks[0]) == 0 {
		r.chunks = r.chunks[1:]
		r.delays = r.delays[1:]
	}
	return n, nil
}

func TestCopySSEFirstTerminalMarkerWins(t *testing.T) {
	src := &recordingReader{
		chunks: []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n",
			"data: [DONE]\n\n",
			"data: [DONE]\n\n",
		},
		delays: []time.Duration{0, 0, 20 * time.Millisecond},
	}
	var dst bytes.Buffer
	_, _, completedAt, _, err := copySSE(&dst, nil, src, 0)
	require.NoError(t, err)
	require.Len(t, src.reads, 3)
	require.False(t, completedAt.IsZero())
	// The marker in chunk 2 is scanned before the pump asks for chunk 3, so
	// completedAt must sit in [reads[1], reads[2]) and never move to marker 2.
	require.False(t, completedAt.Before(src.reads[1]))
	require.True(t, completedAt.Before(src.reads[2]))
	require.Equal(t, 2, strings.Count(dst.String(), "[DONE]"))
}

func TestCopyNDJSONTerminalLineWithoutNewline(t *testing.T) {
	body := "{\"message\":{\"role\":\"assistant\",\"content\":\"a\"},\"done\":false}\n" +
		"{\"done\":true,\"prompt_eval_count\":2,\"eval_count\":5}" // no trailing newline
	var dst bytes.Buffer
	u, firstToken, completedAt, sawError, err := copyNDJSON(&dst, bytes.NewBufferString(body))
	require.NoError(t, err)
	require.Equal(t, body, dst.String())
	require.False(t, firstToken.IsZero())
	require.False(t, completedAt.IsZero())
	require.False(t, sawError)
	require.Equal(t, int64(5), u.Output)
}

// dataThenErrorReader delivers data and then fails, emulating an upstream that
// sends a complete-looking stream before the connection breaks.
type dataThenErrorReader struct {
	data []byte
	err  error
}

func (r *dataThenErrorReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestCopySSEMarkerThenCopyError(t *testing.T) {
	boom := errors.New("upstream reset")
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"usage\":{\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	var dst bytes.Buffer
	u, firstToken, completedAt, sawError, err := copySSE(&dst, nil, &dataThenErrorReader{data: []byte(body), err: boom}, 0)
	require.ErrorIs(t, err, boom)
	require.Equal(t, body, dst.String())
	require.False(t, firstToken.IsZero())
	require.False(t, completedAt.IsZero())
	require.False(t, sawError)
	require.Equal(t, int64(2), u.Output)
}

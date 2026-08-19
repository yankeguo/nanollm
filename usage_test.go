package main

import (
	"bytes"
	"testing"

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

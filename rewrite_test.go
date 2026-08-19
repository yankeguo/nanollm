package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRewriteRequestReplacesModelAndInjectsUsage(t *testing.T) {
	body := []byte(`{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := rewriteRequest(body, "gpt-4o-mini", true, formatOpenAI)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.Equal(t, "gpt-4o-mini", raw["model"])
	opts := raw["stream_options"].(map[string]any)
	require.Equal(t, true, opts["include_usage"])
}

func TestRewriteRequestKeepsExistingStreamOptions(t *testing.T) {
	body := []byte(`{"model":"fast","stream":true,"stream_options":{"include_usage":false}}`)
	out, err := rewriteRequest(body, "other", true, formatOpenAI)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	opts := raw["stream_options"].(map[string]any)
	require.Equal(t, false, opts["include_usage"])
}

func TestParseRequest(t *testing.T) {
	meta, err := parseRequest([]byte(`{"model":"fast","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, "fast", meta.Model)
	require.True(t, meta.Stream)

	_, err = parseRequest([]byte(`{"messages":[]}`))
	require.Error(t, err)

	_, err = parseRequest([]byte(`{"model":"fast"}{"x":1}`))
	require.Error(t, err)
}

func TestRewriteRequestPreservesLargeIntegersAndHTML(t *testing.T) {
	body := []byte(`{"model":"fast","seed":9007199254740993,"messages":[{"content":"<hi>"}]}`)
	out, err := rewriteRequest(body, "gpt", false, formatOpenAI)
	require.NoError(t, err)
	require.Contains(t, string(out), "9007199254740993")
	require.Contains(t, string(out), `"<hi>"`)
	require.NotContains(t, string(out), `\u003c`)
}

func TestRewriteRequestAnthropicDoesNotInjectStreamOptions(t *testing.T) {
	body := []byte(`{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := rewriteRequest(body, "claude-sonnet-4-5", true, formatAnthropic)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.Equal(t, "claude-sonnet-4-5", raw["model"])
	_, has := raw["stream_options"]
	require.False(t, has)
}

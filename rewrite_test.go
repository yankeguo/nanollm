package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRewriteRequestReplacesModelAndInjectsUsage(t *testing.T) {
	body := []byte(`{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := rewriteRequest(body, "gpt-4o-mini", true)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.Equal(t, "gpt-4o-mini", raw["model"])
	opts := raw["stream_options"].(map[string]any)
	require.Equal(t, true, opts["include_usage"])
}

func TestRewriteRequestKeepsExistingStreamOptions(t *testing.T) {
	body := []byte(`{"model":"fast","stream":true,"stream_options":{"include_usage":false}}`)
	out, err := rewriteRequest(body, "other", true)
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
}

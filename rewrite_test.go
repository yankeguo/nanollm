package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRewriteRequestReplacesModelAndInjectsUsage(t *testing.T) {
	body := []byte(`{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := rewriteRequest(body, "gpt-4o-mini", true, true)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.Equal(t, "gpt-4o-mini", raw["model"])
	opts := raw["stream_options"].(map[string]any)
	require.Equal(t, true, opts["include_usage"])
	require.Equal(t, rawObjectField(t, body, "messages"), rawObjectField(t, out, "messages"))
}

func TestRewriteRequestKeepsExistingStreamOptions(t *testing.T) {
	body := []byte(`{"model":"fast","stream":true,"stream_options":{"include_usage":false,"foo":{"n":1}}}`)
	out, err := rewriteRequest(body, "other", true, true)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	opts := raw["stream_options"].(map[string]any)
	require.Equal(t, false, opts["include_usage"])
	require.Equal(t, rawObjectField(t, body, "stream_options"), rawObjectField(t, out, "stream_options"))
}

func TestParseRequest(t *testing.T) {
	meta, err := parseRequest([]byte(`{"model":"fast","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, "fast", meta.Model)
	require.True(t, meta.Stream)
	require.Equal(t, []byte(`{"model":"fast","stream":true}`), meta.Body)

	_, err = parseRequest([]byte(`{"messages":[]}`))
	require.Error(t, err)

	_, err = parseRequest([]byte(`{"model":"fast"}{"x":1}`))
	require.Error(t, err)
}

func TestRewriteRequestPreservesLargeIntegersAndHTML(t *testing.T) {
	body := []byte(`{"model":"fast","seed":9007199254740993,"messages":[{"content":"<hi>"}]}`)
	out, err := rewriteRequest(body, "gpt", false, true)
	require.NoError(t, err)
	require.Equal(t, json.RawMessage(`9007199254740993`), rawObjectField(t, out, "seed"))
	require.Equal(t, rawObjectField(t, body, "messages"), rawObjectField(t, out, "messages"))
	require.Contains(t, string(out), `"<hi>"`)
	require.NotContains(t, string(out), `\u003c`)
}

func TestRewriteRequestPreservesUnknownFields(t *testing.T) {
	body := []byte(`{"model":"fast","foo":{"n":9007199254740993,"html":"<x>","arr":[1,2]}}`)
	out, err := rewriteRequest(body, "gpt-4o-mini", false, true)
	require.NoError(t, err)
	require.Equal(t, json.RawMessage(`"gpt-4o-mini"`), rawObjectField(t, out, "model"))
	require.Equal(t, rawObjectField(t, body, "foo"), rawObjectField(t, out, "foo"))
}

func TestRewriteRequestAnthropicDoesNotInjectStreamOptions(t *testing.T) {
	body := []byte(`{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := rewriteRequest(body, "claude-sonnet-4-5", true, false)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.Equal(t, "claude-sonnet-4-5", raw["model"])
	_, has := raw["stream_options"]
	require.False(t, has)
	require.Equal(t, rawObjectField(t, body, "messages"), rawObjectField(t, out, "messages"))
}

func TestRewriteRequestResponsesDoesNotInjectStreamOptions(t *testing.T) {
	body := []byte(`{"model":"fast","stream":true,"input":"hi"}`)
	out, err := rewriteRequest(body, "gpt-4o", true, false)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.Equal(t, "gpt-4o", raw["model"])
	_, has := raw["stream_options"]
	require.False(t, has)
	require.Equal(t, rawObjectField(t, body, "input"), rawObjectField(t, out, "input"))
}

func TestRewriteRequestCompletionsInjectsUsage(t *testing.T) {
	body := []byte(`{"model":"fast","stream":true,"prompt":"hi"}`)
	out, err := rewriteRequest(body, "gpt-3.5-turbo-instruct", true, true)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.Equal(t, "gpt-3.5-turbo-instruct", raw["model"])
	opts := raw["stream_options"].(map[string]any)
	require.Equal(t, true, opts["include_usage"])
	require.Equal(t, rawObjectField(t, body, "prompt"), rawObjectField(t, out, "prompt"))
}

func TestRewriteRequestEmbeddingsDoesNotInjectStreamOptions(t *testing.T) {
	body := []byte(`{"model":"fast","input":"hi","stream":true}`)
	out, err := rewriteRequest(body, "text-embedding-3-small", true, false)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	require.Equal(t, "text-embedding-3-small", raw["model"])
	_, has := raw["stream_options"]
	require.False(t, has)
	require.Equal(t, rawObjectField(t, body, "input"), rawObjectField(t, out, "input"))
}

func rawObjectField(t *testing.T, body []byte, key string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &m))
	return m[key]
}

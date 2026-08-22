package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func compactSSE(in []byte) []byte {
	if len(in) == 0 {
		return nil
	}
	var buf bytes.Buffer
	w := newSSELogWriter(&buf)
	_, _ = w.Write(in)
	w.Flush()
	return buf.Bytes()
}

func TestCompactSSEOpenAIContent(t *testing.T) {
	in := []byte("data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
		"data: {\"id\":\"2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n")
	out := compactSSE(in)
	events := splitSSEEvents(out)
	require.Len(t, events, 3)
	require.Equal(t, []string{"Hello"}, openaiDeltaStrings(t, events[0], "content"))
	obj := sseDataObject(t, events[0])
	require.Equal(t, `"1"`, string(bytes.TrimSpace(obj["id"])))
	choices, err := unmarshalRawArray(obj["choices"])
	require.NoError(t, err)
	ch, err := decodeJSONRawObject(choices[0])
	require.NoError(t, err)
	require.Equal(t, `"stop"`, string(bytes.TrimSpace(ch["finish_reason"])))
	require.Contains(t, string(events[1]), "prompt_tokens")
	require.Contains(t, string(events[2]), "[DONE]")
}

func TestCompactSSEOpenAIRoleThenContent(t *testing.T) {
	in := []byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	out := compactSSE(in)
	require.Len(t, splitSSEEvents(out), 2)
}

func TestCompactSSEOpenAIInterleavedChoices(t *testing.T) {
	in := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":1,\"delta\":{\"content\":\"x\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"b\"}}]}\n\n")
	out := compactSSE(in)
	events := splitSSEEvents(out)
	require.Len(t, events, 3)
	require.Equal(t, []string{"a"}, openaiDeltaStrings(t, events[0], "content"))
	require.Equal(t, []string{"x"}, openaiDeltaStrings(t, events[1], "content"))
	require.Equal(t, []string{"b"}, openaiDeltaStrings(t, events[2], "content"))
}

func TestCompactSSEOpenAIToolCallArguments(t *testing.T) {
	chunk1, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "foo", "arguments": `{"x":`},
			}},
		}}},
	})
	require.NoError(t, err)
	chunk2, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "function": map[string]any{"arguments": "1}"},
			}},
		}}},
	})
	require.NoError(t, err)
	in := []byte("data: " + string(chunk1) + "\n\ndata: " + string(chunk2) + "\n\n")
	out := compactSSE(in)
	events := splitSSEEvents(out)
	require.Len(t, events, 1)
	obj := sseDataObject(t, events[0])
	choices, err := unmarshalRawArray(obj["choices"])
	require.NoError(t, err)
	ch, err := decodeJSONRawObject(choices[0])
	require.NoError(t, err)
	delta, err := decodeJSONRawObject(ch["delta"])
	require.NoError(t, err)
	tcs, err := unmarshalRawArray(delta["tool_calls"])
	require.NoError(t, err)
	require.Len(t, tcs, 1)
	tc, err := decodeJSONRawObject(tcs[0])
	require.NoError(t, err)
	require.Equal(t, `"call_1"`, string(bytes.TrimSpace(tc["id"])))
	fn, err := decodeJSONRawObject(tc["function"])
	require.NoError(t, err)
	args, err := rawJSONString(fn["arguments"])
	require.NoError(t, err)
	require.Equal(t, `{"x":1}`, args)
	name, err := rawJSONString(fn["name"])
	require.NoError(t, err)
	require.Equal(t, "foo", name)
}

func TestCompactSSEOpenAIReasoning(t *testing.T) {
	in := []byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"th\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"ink\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
	out := compactSSE(in)
	events := splitSSEEvents(out)
	require.Len(t, events, 2)
	require.Equal(t, []string{"think"}, openaiDeltaStrings(t, events[0], "reasoning_content"))
	require.Equal(t, []string{"ok"}, openaiDeltaStrings(t, events[1], "content"))
}

func TestCompactSSEOpenAILegacyText(t *testing.T) {
	in := []byte("data: {\"choices\":[{\"index\":0,\"text\":\"Hel\"}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"text\":\"lo\"}]}\n\n")
	out := compactSSE(in)
	events := splitSSEEvents(out)
	require.Len(t, events, 1)
	obj := sseDataObject(t, events[0])
	choices, err := unmarshalRawArray(obj["choices"])
	require.NoError(t, err)
	ch, err := decodeJSONRawObject(choices[0])
	require.NoError(t, err)
	text, err := rawJSONString(ch["text"])
	require.NoError(t, err)
	require.Equal(t, "Hello", text)
}

func TestCompactSSEAnthropicTextDelta(t *testing.T) {
	in := []byte("event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
	out := compactSSE(in)
	events := splitSSEEvents(out)
	require.Len(t, events, 2)
	eventName, data := parseSSEFields(events[0])
	require.Equal(t, "content_block_delta", eventName)
	obj, err := decodeJSONRawObject([]byte(data))
	require.NoError(t, err)
	delta, err := decodeJSONRawObject(obj["delta"])
	require.NoError(t, err)
	text, err := rawJSONString(delta["text"])
	require.NoError(t, err)
	require.Equal(t, "Hello", text)
	require.Contains(t, string(events[1]), "message_delta")
}

func TestCompactSSEAnthropicInputJSON(t *testing.T) {
	chunk1, err := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"a":`},
	})
	require.NoError(t, err)
	chunk2, err := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": "1}"},
	})
	require.NoError(t, err)
	in := []byte("event: content_block_delta\ndata: " + string(chunk1) + "\n\n" +
		"event: content_block_delta\ndata: " + string(chunk2) + "\n\n")
	out := compactSSE(in)
	events := splitSSEEvents(out)
	require.Len(t, events, 1)
	obj := sseDataObject(t, events[0])
	delta, err := decodeJSONRawObject(obj["delta"])
	require.NoError(t, err)
	partial, err := rawJSONString(delta["partial_json"])
	require.NoError(t, err)
	require.Equal(t, `{"a":1}`, partial)
}

func TestCompactSSEAnthropicDifferentIndex(t *testing.T) {
	in := []byte("event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"a\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"b\"}}\n\n")
	out := compactSSE(in)
	require.Len(t, splitSSEEvents(out), 2)
}

func TestCompactSSEAnthropicThinkingVsText(t *testing.T) {
	in := []byte("event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
	out := compactSSE(in)
	require.Len(t, splitSSEEvents(out), 2)
}

func TestCompactSSEAnthropicCitationsUnmerged(t *testing.T) {
	in := []byte("event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"citations_delta\",\"citation\":{\"type\":\"web\"}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"citations_delta\",\"citation\":{\"type\":\"web\"}}}\n\n")
	out := compactSSE(in)
	require.Len(t, splitSSEEvents(out), 2)
}

func TestCompactSSEResponsesOutputText(t *testing.T) {
	in := []byte("event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hel\",\"sequence_number\":1}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"lo\",\"sequence_number\":2}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n")
	out := compactSSE(in)
	events := splitSSEEvents(out)
	require.Len(t, events, 2)
	eventName, data := parseSSEFields(events[0])
	require.Equal(t, "response.output_text.delta", eventName)
	obj, err := decodeJSONRawObject([]byte(data))
	require.NoError(t, err)
	delta, err := rawJSONString(obj["delta"])
	require.NoError(t, err)
	require.Equal(t, "Hello", delta)
	require.Equal(t, "2", string(bytes.TrimSpace(obj["sequence_number"])))
	require.Contains(t, string(events[1]), "response.completed")
}

func TestCompactSSEResponsesDifferentItem(t *testing.T) {
	in := []byte("event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"a\",\"delta\":\"x\"}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"b\",\"delta\":\"y\"}\n\n")
	out := compactSSE(in)
	require.Len(t, splitSSEEvents(out), 2)
}

func TestCompactSSEResponsesFunctionArgs(t *testing.T) {
	chunk1, err := json.Marshal(map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": `{"n":`,
	})
	require.NoError(t, err)
	chunk2, err := json.Marshal(map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": "1}",
	})
	require.NoError(t, err)
	in := []byte("event: response.function_call_arguments.delta\ndata: " + string(chunk1) + "\n\n" +
		"event: response.function_call_arguments.delta\ndata: " + string(chunk2) + "\n\n")
	out := compactSSE(in)
	events := splitSSEEvents(out)
	require.Len(t, events, 1)
	obj := sseDataObject(t, events[0])
	delta, err := rawJSONString(obj["delta"])
	require.NoError(t, err)
	require.Equal(t, `{"n":1}`, delta)
}

func TestCompactSSEResponsesAudioUnmerged(t *testing.T) {
	in := []byte("event: response.audio.delta\n" +
		"data: {\"type\":\"response.audio.delta\",\"delta\":\"AAAA\"}\n\n" +
		"event: response.audio.delta\n" +
		"data: {\"type\":\"response.audio.delta\",\"delta\":\"BBBB\"}\n\n")
	out := compactSSE(in)
	require.Len(t, splitSSEEvents(out), 2)
}

func TestCompactSSEResponsesCodeInterpreter(t *testing.T) {
	in := []byte("event: response.code_interpreter_call_code.delta\n" +
		"data: {\"type\":\"response.code_interpreter_call_code.delta\",\"item_id\":\"ci_1\",\"output_index\":0,\"delta\":\"import\"}\n\n" +
		"event: response.code_interpreter_call_code.delta\n" +
		"data: {\"type\":\"response.code_interpreter_call_code.delta\",\"item_id\":\"ci_1\",\"output_index\":0,\"delta\":\" os\"}\n\n")
	out := compactSSE(in)
	events := splitSSEEvents(out)
	require.Len(t, events, 1)
	obj := sseDataObject(t, events[0])
	delta, err := rawJSONString(obj["delta"])
	require.NoError(t, err)
	require.Equal(t, "import os", delta)
}

func TestCompactSSEResponsesCustomToolCallInput(t *testing.T) {
	in := []byte("event: response.custom_tool_call_input.delta\n" +
		"data: {\"type\":\"response.custom_tool_call_input.delta\",\"item_id\":\"ctc_1\",\"delta\":\"abc\"}\n\n" +
		"event: response.custom_tool_call_input.delta\n" +
		"data: {\"type\":\"response.custom_tool_call_input.delta\",\"item_id\":\"ctc_1\",\"delta\":\"def\"}\n\n")
	out := compactSSE(in)
	events := splitSSEEvents(out)
	require.Len(t, events, 1)
	obj := sseDataObject(t, events[0])
	delta, err := rawJSONString(obj["delta"])
	require.NoError(t, err)
	require.Equal(t, "abcdef", delta)
}

func TestCompactSSEPassThrough(t *testing.T) {
	in := []byte("data: not-json\n\n" +
		": comment\n\n" +
		"data: [DONE]\n\n")
	require.Equal(t, in, compactSSE(in))
}

func TestCompactSSEChunkedWrites(t *testing.T) {
	in := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
	var buf bytes.Buffer
	w := newSSELogWriter(&buf)
	for i := range in {
		n, err := w.Write(in[i : i+1])
		require.NoError(t, err)
		require.Equal(t, 1, n)
	}
	w.Flush()
	events := splitSSEEvents(buf.Bytes())
	require.Len(t, events, 1)
	require.Equal(t, []string{"Hello"}, openaiDeltaStrings(t, events[0], "content"))
}

func TestCompactSSEEmpty(t *testing.T) {
	require.Nil(t, compactSSE(nil))
	require.Nil(t, compactSSE([]byte{}))
}

func TestCompactSSESingleEventUnchanged(t *testing.T) {
	in := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"extra\":1}\n\n")
	require.Equal(t, in, compactSSE(in))
}

func TestCompactSSECRLF(t *testing.T) {
	in := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\r\n\r\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\r\n\r\n")
	out := compactSSE(in)
	events := splitSSEEvents(out)
	require.Len(t, events, 1)
	require.Equal(t, []string{"Hello"}, openaiDeltaStrings(t, events[0], "content"))
}

func splitSSEEvents(raw []byte) [][]byte {
	var events [][]byte
	var acc []byte
	for len(raw) > 0 {
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			acc = append(acc, raw...)
			break
		}
		line := raw[:i+1]
		raw = raw[i+1:]
		acc = append(acc, line...)
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			events = append(events, acc)
			acc = nil
		}
	}
	if len(acc) > 0 {
		events = append(events, acc)
	}
	return events
}

func sseDataObject(t *testing.T, event []byte) map[string]json.RawMessage {
	t.Helper()
	_, data := parseSSEFields(event)
	obj, err := decodeJSONRawObject([]byte(data))
	require.NoError(t, err)
	return obj
}

func openaiDeltaStrings(t *testing.T, event []byte, field string) []string {
	t.Helper()
	obj := sseDataObject(t, event)
	choices, err := unmarshalRawArray(obj["choices"])
	require.NoError(t, err)
	var out []string
	for _, raw := range choices {
		ch, err := decodeJSONRawObject(raw)
		require.NoError(t, err)
		delta, err := decodeJSONRawObject(ch["delta"])
		require.NoError(t, err)
		s, err := rawJSONString(delta[field])
		require.NoError(t, err)
		out = append(out, s)
	}
	return out
}

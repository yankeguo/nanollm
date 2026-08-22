package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// sseLogWriter copies an SSE transcript for the call log, coalescing
// consecutive same-type text/argument deltas. Write never returns a short
// write or an error so it is safe on io.MultiWriter beside the client copy.
type sseLogWriter struct {
	out     io.Writer
	carry   []byte
	acc     []byte
	pending *pendingSSE
}

type pendingSSE struct {
	raw    []byte
	event  string
	obj    map[string]json.RawMessage
	key    string
	merged bool
}

func newSSELogWriter(out io.Writer) *sseLogWriter {
	return &sseLogWriter{out: out}
}

func (w *sseLogWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	w.carry = append(w.carry, p...)
	w.consume()
	return n, nil
}

func (w *sseLogWriter) Flush() {
	if len(w.acc) > 0 || len(w.carry) > 0 {
		raw := make([]byte, 0, len(w.acc)+len(w.carry))
		raw = append(raw, w.acc...)
		raw = append(raw, w.carry...)
		w.acc = nil
		w.carry = nil
		w.handleEvent(raw)
	}
	w.flushPending()
}

func (w *sseLogWriter) consume() {
	for {
		i := bytes.IndexByte(w.carry, '\n')
		if i < 0 {
			break
		}
		line := w.carry[:i+1]
		w.carry = w.carry[i+1:]
		w.acc = append(w.acc, line...)
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			ev := w.acc
			w.acc = nil
			w.handleEvent(ev)
		}
	}
	if len(w.acc)+len(w.carry) > maxMediumBlob {
		w.flushPending()
		w.writeOut(w.acc)
		w.writeOut(w.carry)
		w.acc = nil
		w.carry = nil
	}
}

func (w *sseLogWriter) handleEvent(raw []byte) {
	eventName, data := parseSSEFields(raw)
	obj, key, ok := sseMergeable(data)
	if !ok {
		w.flushPending()
		w.writeOut(raw)
		return
	}
	// mergeSSE only mutates the pending object, never obj, so a failed merge
	// can still start a new pending run with the current event.
	if w.pending != nil && w.pending.key == key && mergeSSE(w.pending.obj, obj) {
		w.pending.merged = true
		return
	}
	w.flushPending()
	w.pending = &pendingSSE{raw: raw, event: eventName, obj: obj, key: key}
}

func (w *sseLogWriter) flushPending() {
	if w.pending == nil {
		return
	}
	p := w.pending
	w.pending = nil
	if !p.merged {
		w.writeOut(p.raw)
		return
	}
	data, err := encodeJSONValue(p.obj)
	if err != nil {
		w.writeOut(p.raw)
		return
	}
	var buf bytes.Buffer
	if p.event != "" {
		buf.WriteString("event: ")
		buf.WriteString(p.event)
		buf.WriteByte('\n')
	}
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	w.writeOut(buf.Bytes())
}

func (w *sseLogWriter) writeOut(p []byte) {
	if len(p) == 0 {
		return
	}
	_, _ = w.out.Write(p)
}

func parseSSEFields(raw []byte) (event, data string) {
	var dataParts []string
	for len(raw) > 0 {
		var line []byte
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line = raw[:i]
			raw = raw[i+1:]
		} else {
			line = raw
			raw = nil
		}
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		field, rest, found := bytes.Cut(line, []byte{':'})
		value := rest
		if found && len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch string(field) {
		case "event":
			event = string(value)
		case "data":
			dataParts = append(dataParts, string(value))
		}
	}
	return event, strings.Join(dataParts, "\n")
}

func sseMergeable(data string) (map[string]json.RawMessage, string, bool) {
	obj, err := decodeJSONRawObject([]byte(data))
	if err != nil {
		return nil, "", false
	}
	typ, _ := rawJSONString(obj["type"])
	var key string
	switch {
	case typ == "content_block_delta":
		key, err = anthropicMergeKey(obj)
	case strings.HasPrefix(typ, "response.") && strings.HasSuffix(typ, ".delta"):
		key, err = responsesMergeKey(obj)
	case obj["choices"] != nil:
		key, err = openaiMergeKey(obj)
	default:
		return nil, "", false
	}
	if err != nil || key == "" {
		return nil, "", false
	}
	return obj, key, true
}

func mergeSSE(dst, src map[string]json.RawMessage) bool {
	typ, _ := rawJSONString(dst["type"])
	switch {
	case typ == "content_block_delta":
		return mergeAnthropicDelta(dst, src)
	case strings.HasPrefix(typ, "response."):
		return mergeResponsesDelta(dst, src)
	default:
		return mergeOpenAIDelta(dst, src)
	}
}

func openaiMergeKey(obj map[string]json.RawMessage) (string, error) {
	choices, err := unmarshalRawArray(obj["choices"])
	if err != nil || len(choices) == 0 {
		return "", err
	}
	var key strings.Builder
	key.WriteString("openai")
	for _, ch := range choices {
		m, err := decodeJSONRawObject(ch)
		if err != nil {
			return "", err
		}
		key.WriteString("|i=")
		key.WriteString(rawIndex(m))
		fields, ok := openaiChoiceFields(m)
		if !ok {
			return "", nil
		}
		key.WriteString(":")
		key.WriteString(fields)
	}
	return key.String(), nil
}

func openaiChoiceFields(ch map[string]json.RawMessage) (string, bool) {
	if delta, ok := ch["delta"]; ok {
		d, err := decodeJSONRawObject(delta)
		if err != nil {
			return "", false
		}
		fields := concatenableDeltaFields(d)
		if fields == "" {
			return "", false
		}
		return fields, true
	}
	if isJSONString(ch["text"]) {
		return "text", true
	}
	return "", false
}

func concatenableDeltaFields(d map[string]json.RawMessage) string {
	var parts []string
	for _, name := range []string{"content", "reasoning_content", "reasoning"} {
		if isJSONString(d[name]) {
			parts = append(parts, name)
		}
	}
	if raw, ok := d["function_call"]; ok && functionCallHasArgs(raw) {
		parts = append(parts, "function_call")
	}
	if raw, ok := d["tool_calls"]; ok {
		indexes, ok := toolCallIndexes(raw)
		if !ok {
			return ""
		}
		parts = append(parts, "tool_calls="+strings.Join(indexes, ","))
	}
	return strings.Join(parts, "+")
}

func functionCallHasArgs(raw json.RawMessage) bool {
	m, err := decodeJSONRawObject(raw)
	return err == nil && isJSONString(m["arguments"])
}

func toolCallIndexes(raw json.RawMessage) ([]string, bool) {
	arr, err := unmarshalRawArray(raw)
	if err != nil || len(arr) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	seen := make(map[string]struct{}, len(arr))
	for _, item := range arr {
		m, err := decodeJSONRawObject(item)
		if err != nil {
			return nil, false
		}
		idx := rawIndex(m)
		if _, ok := seen[idx]; ok {
			continue
		}
		seen[idx] = struct{}{}
		out = append(out, idx)
	}
	return out, true
}

func mergeOpenAIDelta(dst, src map[string]json.RawMessage) bool {
	dstChoices, err := unmarshalRawArray(dst["choices"])
	if err != nil {
		return false
	}
	srcChoices, err := unmarshalRawArray(src["choices"])
	if err != nil {
		return false
	}
	srcByIdx := make(map[string]map[string]json.RawMessage, len(srcChoices))
	for _, raw := range srcChoices {
		m, err := decodeJSONRawObject(raw)
		if err != nil {
			return false
		}
		srcByIdx[rawIndex(m)] = m
	}
	out := make([]json.RawMessage, len(dstChoices))
	for i, raw := range dstChoices {
		dm, err := decodeJSONRawObject(raw)
		if err != nil {
			return false
		}
		if sm := srcByIdx[rawIndex(dm)]; sm != nil {
			if !mergeOpenAIChoice(dm, sm) {
				return false
			}
		}
		enc, err := encodeJSONValue(dm)
		if err != nil {
			return false
		}
		out[i] = enc
	}
	enc, err := encodeJSONValue(out)
	if err != nil {
		return false
	}
	dst["choices"] = enc
	copyLastField(dst, src, "sequence_number")
	copyLastField(dst, src, "usage")
	return true
}

func mergeOpenAIChoice(dst, src map[string]json.RawMessage) bool {
	if fr, ok := src["finish_reason"]; ok && !isJSONNull(fr) {
		dst["finish_reason"] = fr
	}
	if isJSONString(src["text"]) {
		concatJSONStringField(dst, "text", src["text"])
	}
	srcDelta, ok := src["delta"]
	if !ok {
		return true
	}
	sd, err := decodeJSONRawObject(srcDelta)
	if err != nil {
		return false
	}
	dd, err := decodeJSONRawObject(dst["delta"])
	if err != nil {
		dst["delta"] = srcDelta
		return true
	}
	for _, name := range []string{"content", "reasoning_content", "reasoning"} {
		if isJSONString(sd[name]) {
			concatJSONStringField(dd, name, sd[name])
		}
	}
	if raw, ok := sd["function_call"]; ok {
		if !mergeFunctionCall(dd, raw) {
			return false
		}
	}
	if raw, ok := sd["tool_calls"]; ok {
		if !mergeToolCalls(dd, raw) {
			return false
		}
	}
	enc, err := encodeJSONValue(dd)
	if err != nil {
		return false
	}
	dst["delta"] = enc
	return true
}

func mergeToolCalls(dstDelta map[string]json.RawMessage, srcRaw json.RawMessage) bool {
	srcList, err := unmarshalRawArray(srcRaw)
	if err != nil {
		return false
	}
	dstList, err := unmarshalRawArray(dstDelta["tool_calls"])
	if err != nil {
		dstDelta["tool_calls"] = srcRaw
		return true
	}
	dstMaps := make([]map[string]json.RawMessage, len(dstList))
	byIdx := make(map[string]int, len(dstList))
	for i, raw := range dstList {
		m, err := decodeJSONRawObject(raw)
		if err != nil {
			return false
		}
		dstMaps[i] = m
		byIdx[rawIndex(m)] = i
	}
	for _, raw := range srcList {
		sm, err := decodeJSONRawObject(raw)
		if err != nil {
			return false
		}
		idx := rawIndex(sm)
		di, ok := byIdx[idx]
		if !ok {
			dstMaps = append(dstMaps, sm)
			byIdx[idx] = len(dstMaps) - 1
			continue
		}
		if !mergeToolCall(dstMaps[di], sm) {
			return false
		}
	}
	out := make([]json.RawMessage, len(dstMaps))
	for i, m := range dstMaps {
		enc, err := encodeJSONValue(m)
		if err != nil {
			return false
		}
		out[i] = enc
	}
	enc, err := encodeJSONValue(out)
	if err != nil {
		return false
	}
	dstDelta["tool_calls"] = enc
	return true
}

func mergeFunctionCall(dstDelta map[string]json.RawMessage, srcRaw json.RawMessage) bool {
	sm, err := decodeJSONRawObject(srcRaw)
	if err != nil {
		return true
	}
	if !isJSONString(sm["arguments"]) {
		return true
	}
	dm, err := decodeJSONRawObject(dstDelta["function_call"])
	if err != nil {
		dstDelta["function_call"] = srcRaw
		return true
	}
	if _, ok := dm["name"]; !ok {
		if v, ok := sm["name"]; ok {
			dm["name"] = v
		}
	}
	concatJSONStringField(dm, "arguments", sm["arguments"])
	enc, err := encodeJSONValue(dm)
	if err != nil {
		return false
	}
	dstDelta["function_call"] = enc
	return true
}

func mergeToolCall(dst, src map[string]json.RawMessage) bool {
	sf, err := decodeJSONRawObject(src["function"])
	if err != nil {
		return true
	}
	if !isJSONString(sf["arguments"]) {
		return true
	}
	df, err := decodeJSONRawObject(dst["function"])
	if err != nil {
		dst["function"] = src["function"]
		return true
	}
	concatJSONStringField(df, "arguments", sf["arguments"])
	enc, err := encodeJSONValue(df)
	if err != nil {
		return false
	}
	dst["function"] = enc
	return true
}

var anthropicDeltaField = map[string]string{
	"text_delta":       "text",
	"thinking_delta":   "thinking",
	"input_json_delta": "partial_json",
	"signature_delta":  "signature",
}

func anthropicMergeKey(obj map[string]json.RawMessage) (string, error) {
	delta, err := decodeJSONRawObject(obj["delta"])
	if err != nil {
		return "", err
	}
	dtyp, _ := rawJSONString(delta["type"])
	field := anthropicDeltaField[dtyp]
	if field == "" || !isJSONString(delta[field]) {
		return "", nil
	}
	return "anthropic|" + rawIndex(obj) + "|" + dtyp, nil
}

func mergeAnthropicDelta(dst, src map[string]json.RawMessage) bool {
	dd, err := decodeJSONRawObject(dst["delta"])
	if err != nil {
		return false
	}
	sd, err := decodeJSONRawObject(src["delta"])
	if err != nil {
		return false
	}
	dtyp, _ := rawJSONString(dd["type"])
	field := anthropicDeltaField[dtyp]
	if field == "" || !isJSONString(sd[field]) {
		return false
	}
	concatJSONStringField(dd, field, sd[field])
	enc, err := encodeJSONValue(dd)
	if err != nil {
		return false
	}
	dst["delta"] = enc
	return true
}

var responsesTextDelta = map[string]bool{
	"response.output_text.delta":                true,
	"response.refusal.delta":                    true,
	"response.function_call_arguments.delta":    true,
	"response.reasoning_text.delta":             true,
	"response.reasoning.delta":                  true,
	"response.reasoning_summary_text.delta":     true,
	"response.mcp_call.arguments.delta":         true,
	"response.audio.transcript.delta":           true,
	"response.code_interpreter_call_code.delta": true,
	"response.custom_tool_call_input.delta":     true,
}

func responsesMergeKey(obj map[string]json.RawMessage) (string, error) {
	typ, _ := rawJSONString(obj["type"])
	if !responsesTextDelta[typ] || !isJSONString(obj["delta"]) {
		return "", nil
	}
	var key strings.Builder
	key.WriteString("responses|")
	key.WriteString(typ)
	for _, f := range []string{"item_id", "output_index", "content_index", "summary_index"} {
		if v, ok := obj[f]; ok {
			key.WriteByte('|')
			key.WriteString(f)
			key.WriteByte('=')
			key.Write(bytes.TrimSpace(v))
		}
	}
	return key.String(), nil
}

func mergeResponsesDelta(dst, src map[string]json.RawMessage) bool {
	if !isJSONString(src["delta"]) {
		return false
	}
	concatJSONStringField(dst, "delta", src["delta"])
	copyLastField(dst, src, "sequence_number")
	return true
}

func concatJSONStringField(obj map[string]json.RawMessage, key string, extra json.RawMessage) {
	a, err := rawJSONString(obj[key])
	if err != nil {
		a = ""
	}
	b, err := rawJSONString(extra)
	if err != nil {
		return
	}
	enc, err := encodeJSONValue(a + b)
	if err != nil {
		return
	}
	obj[key] = enc
}

func copyLastField(dst, src map[string]json.RawMessage, key string) {
	if v, ok := src[key]; ok {
		dst[key] = v
	}
}

func unmarshalRawArray(raw json.RawMessage) ([]json.RawMessage, error) {
	if jsonBlank(raw) {
		return nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, err
	}
	return arr, nil
}

func rawIndex(m map[string]json.RawMessage) string {
	v, ok := m["index"]
	if !ok {
		return "0"
	}
	s := string(bytes.TrimSpace(v))
	if s == "" || s == "null" {
		return "0"
	}
	return s
}

func isJSONString(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '"'
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

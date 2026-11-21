package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// maxSSEScan caps an incomplete SSE line held for usage parsing. Responses
// `response.completed` embeds the full output object on one data line, so this
// matches the call-log MEDIUMBLOB cap rather than a 1MiB thinking-delta guess.
var maxSSEScan = maxMediumBlob

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

// jsonInt64 accepts JSON integers, integer-valued floats, and numeric strings.
// Some compatible gateways serialize usage counts as 1.0 or "11".
type jsonInt64 int64

func (n *jsonInt64) UnmarshalJSON(b []byte) error {
	if jsonBlank(b) {
		*n = 0
		return nil
	}
	b = bytes.TrimSpace(b)
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		b = bytes.TrimSpace([]byte(s))
		if len(b) == 0 {
			*n = 0
			return nil
		}
	}
	var i int64
	if err := json.Unmarshal(b, &i); err == nil {
		*n = jsonInt64(i)
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*n = jsonInt64(f)
	return nil
}

type usageFields struct {
	PromptTokens             jsonInt64 `json:"prompt_tokens"`
	CompletionTokens         jsonInt64 `json:"completion_tokens"`
	TotalTokens              jsonInt64 `json:"total_tokens"`
	InputTokens              jsonInt64 `json:"input_tokens"`
	OutputTokens             jsonInt64 `json:"output_tokens"`
	CachedTokens             jsonInt64 `json:"cached_tokens"`
	CacheReadInputTokens     jsonInt64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens jsonInt64 `json:"cache_creation_input_tokens"`
	PromptCacheHitTokens     jsonInt64 `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens    jsonInt64 `json:"prompt_cache_miss_tokens"`
	// PromptEvalCount/EvalCount are Ollama /api/chat's flat top-level usage
	// fields (prompt and completion token counts).
	PromptEvalCount jsonInt64 `json:"prompt_eval_count"`
	EvalCount       jsonInt64 `json:"eval_count"`
	// ImageTokens is a top-level field only on DashScope multimodal embedding
	// responses (qwen*-vl-embedding, multimodal-embedding-v1), where
	// input_tokens excludes visual tokens.
	ImageTokens         jsonInt64 `json:"image_tokens"`
	PromptTokensDetails *struct {
		CachedTokens jsonInt64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens jsonInt64 `json:"cached_tokens"`
		// DashScope tongyi-embedding-vision-* breaks down input_tokens here;
		// those tokens are already counted in input_tokens, so these fields
		// are parsed for completeness but never added.
		ImageTokens jsonInt64 `json:"image_tokens"`
		TextTokens  jsonInt64 `json:"text_tokens"`
	} `json:"input_tokens_details"`
	CacheCreation *struct {
		Ephemeral5mInputTokens jsonInt64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens jsonInt64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u usageFields) asTokenUsage() tokenUsage {
	cacheRead := int64(u.CacheReadInputTokens)
	if cacheRead == 0 {
		cacheRead = int64(u.PromptCacheHitTokens)
	}
	if cacheRead == 0 && u.PromptTokensDetails != nil {
		cacheRead = int64(u.PromptTokensDetails.CachedTokens)
	}
	if cacheRead == 0 && u.InputTokensDetails != nil {
		cacheRead = int64(u.InputTokensDetails.CachedTokens)
	}
	if cacheRead == 0 {
		cacheRead = int64(u.CachedTokens)
	}

	cacheCreation := int64(u.CacheCreationInputTokens)
	if cacheCreation == 0 && u.CacheCreation != nil {
		cacheCreation = int64(u.CacheCreation.Ephemeral5mInputTokens + u.CacheCreation.Ephemeral1hInputTokens)
	}

	in := int64(u.PromptTokens)
	if in == 0 {
		in = int64(u.InputTokens)
		// Anthropic input_tokens is tokens after the last cache breakpoint
		// (not the full prompt). OpenAI prompt_tokens / Responses
		// input_tokens already include cached tokens and use *tokens_details
		// instead of cache_read_input_tokens, so this add is a no-op there.
		in += int64(u.CacheReadInputTokens) + cacheCreation
	}
	if in == 0 {
		// Ollama /api/chat reports a flat prompt_eval_count.
		in = int64(u.PromptEvalCount)
	}
	// DashScope qwen multimodal embeddings report visual tokens as a
	// top-level image_tokens not included in input_tokens. The tongyi
	// series puts them in input_tokens_details instead and already counts
	// them in input_tokens, so only the top-level field is added here.
	in += int64(u.ImageTokens)
	out := int64(u.CompletionTokens)
	if out == 0 {
		out = int64(u.OutputTokens)
	}
	if out == 0 {
		// Ollama /api/chat reports a flat eval_count.
		out = int64(u.EvalCount)
	}
	// Embeddings (and some gateways) report only total_tokens, or omit
	// prompt_tokens. Do not let total_tokens override an already-parsed input.
	if in == 0 {
		total := int64(u.TotalTokens)
		if total > 0 {
			in = total - out
			if in < 0 {
				in = 0
			}
		}
	}

	usage := tokenUsage{
		Input:         in,
		Output:        out,
		CacheRead:     cacheRead,
		CacheCreation: cacheCreation,
		Uncached:      int64(u.PromptCacheMissTokens),
	}
	if usage.Uncached == 0 {
		usage.fillUncached()
	}
	return usage
}

type nestedUsage struct {
	Model string       `json:"model"`
	Usage *usageFields `json:"usage"`
}

type usageEnvelope struct {
	Model    string       `json:"model"`
	Usage    *usageFields `json:"usage"`
	Message  *nestedUsage `json:"message"`
	Response *nestedUsage `json:"response"`
}

func mergeNestedUsage(out *tokenUsage, nested *nestedUsage) {
	if nested == nil {
		return
	}
	if nested.Model != "" {
		out.ResponseModel = nested.Model
	}
	if nested.Usage != nil {
		u := nested.Usage.asTokenUsage()
		u.ResponseModel = nested.Model
		if u.ResponseModel == "" {
			u.ResponseModel = out.ResponseModel
		}
		mergeUsage(out, u)
	}
}

func parseUsageJSON(body []byte) tokenUsage {
	var resp usageEnvelope
	if err := json.Unmarshal(body, &resp); err != nil {
		return tokenUsage{}
	}
	var out tokenUsage
	// Ollama /api/chat has no usage object: prompt_eval_count/eval_count sit
	// flat at the top level. Other protocols leave these fields zero, so this
	// merge is a no-op for them.
	var flat usageFields
	if err := json.Unmarshal(body, &flat); err == nil {
		mergeUsage(&out, flat.asTokenUsage())
	}
	// Anthropic-style {"type":"message_start","message":{"model":...,"usage":...}}
	mergeNestedUsage(&out, resp.Message)
	// OpenAI Responses SSE {"type":"response.completed","response":{"model":...,"usage":...}}
	mergeNestedUsage(&out, resp.Response)
	if resp.Model != "" {
		out.ResponseModel = resp.Model
	}
	if resp.Usage != nil {
		mergeUsage(&out, resp.Usage.asTokenUsage())
	}
	return out
}

const sseComment = ":\n\n"

// sseKeepaliveInterval is how often to emit an SSE comment while the upstream
// body is idle. Thinking models can sit silent for minutes after HTTP 200;
// without comments, clients and middle proxies idle-timeout and cancel us.
const sseKeepaliveInterval = 15 * time.Second

func parseUsageSSELine(line []byte) tokenUsage {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return tokenUsage{}
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return tokenUsage{}
	}
	return parseUsageJSON(payload)
}

// copySSE returns the parsed usage, the first output-eligible line time, the
// protocol completion-marker time, and whether the stream carried an in-band
// error event. The error return is only the copy error: a stream that reports
// an error event and then a completion marker copies cleanly, so sawError is
// the separate signal callers use to reject the measurement window.
func copySSE(dst, ping io.Writer, src io.Reader, keepalive time.Duration) (tokenUsage, time.Time, time.Time, bool, error) {
	var usage tokenUsage
	var firstToken, completedAt time.Time
	var sawError bool
	err := pumpStream(dst, ping, src, keepalive, func(line []byte) {
		// Metadata, usage-only, error, and terminal events do not represent
		// output. Keepalive comments never reach this point.
		if isSSEErrorLine(line) {
			sawError = true
		}
		if firstToken.IsZero() && isSSEFirstTokenLine(line) {
			firstToken = time.Now()
		}
		if completedAt.IsZero() && isSSETerminalLine(line) {
			completedAt = time.Now()
		}
		mergeUsage(&usage, parseUsageSSELine(line))
	})
	return usage, firstToken, completedAt, sawError, err
}

// copyNDJSON copies an Ollama-style newline-delimited JSON stream. NDJSON has
// no comment syntax, so no keepalive can be injected; the first output-bearing
// line is the TTFT signal and `"done":true` is the completion marker. Like
// copySSE, it reports an in-band error separately from the copy error.
func copyNDJSON(dst io.Writer, src io.Reader) (tokenUsage, time.Time, time.Time, bool, error) {
	var usage tokenUsage
	var firstToken, completedAt time.Time
	var sawError bool
	err := pumpStream(dst, dst, src, 0, func(line []byte) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			return
		}
		if isNDJSONErrorLine(line) {
			sawError = true
		}
		// Ollama sends role-only/empty message chunks before real output and a
		// final "done":true object; neither is a token.
		if firstToken.IsZero() && isNDJSONFirstTokenLine(line) {
			firstToken = time.Now()
		}
		if completedAt.IsZero() && isNDJSONTerminalLine(line) {
			completedAt = time.Now()
		}
		mergeUsage(&usage, parseUsageJSON(line))
	})
	return usage, firstToken, completedAt, sawError, err
}

// pumpStream copies src to dst while feeding each completed line to scan.
// While keepalive > 0 and src is idle, SSE comments are written to ping.
func pumpStream(dst, ping io.Writer, src io.Reader, keepalive time.Duration, scan func(line []byte)) error {
	if ping == nil {
		ping = dst
	}
	var carry []byte
	buf := make([]byte, 32*1024)

	consume := func(chunk []byte) error {
		if _, err := dst.Write(chunk); err != nil {
			return err
		}
		carry = append(carry, chunk...)
		for {
			i := bytes.IndexByte(carry, '\n')
			if i < 0 {
				break
			}
			line := bytes.TrimRight(carry[:i], "\r")
			carry = carry[i+1:]
			scan(line)
		}
		if len(carry) > maxSSEScan {
			scan(carry)
			carry = carry[:0]
		}
		return nil
	}

	finish := func(err error) error {
		if err == nil || errors.Is(err, io.EOF) {
			if len(carry) > 0 {
				scan(carry)
			}
			return nil
		}
		return err
	}

	if keepalive <= 0 {
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if werr := consume(buf[:n]); werr != nil {
					return werr
				}
			}
			if err != nil {
				return finish(err)
			}
		}
	}

	type readOp struct {
		n   int
		err error
	}
	readc := make(chan readOp, 1)
	startRead := func() {
		go func() {
			n, err := src.Read(buf)
			readc <- readOp{n, err}
		}()
	}

	ticker := time.NewTicker(keepalive)
	defer ticker.Stop()
	startRead()
	for {
		select {
		case op := <-readc:
			ticker.Reset(keepalive)
			if op.n > 0 {
				if err := consume(buf[:op.n]); err != nil {
					return err
				}
			}
			if op.err != nil {
				return finish(op.err)
			}
			startRead()
		case <-ticker.C:
			if _, err := ping.Write([]byte(sseComment)); err != nil {
				return err
			}
		}
	}
}

// sseDataPayload extracts a non-empty data payload and reports whether the
// line was an SSE data line. [DONE] remains a payload so the caller can
// recognize it as a terminal sentinel rather than streamed output.
func sseDataPayload(line []byte) ([]byte, bool) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	return payload, len(payload) > 0
}

// isSSEFirstTokenLine reports whether an SSE data line can carry the first
// streamed output token. OpenAI Chat/Completions metadata chunks (role-only or
// empty deltas), usage-only chunks, lifecycle events (Anthropic message_* and
// content_block_*, OpenAI Responses response.*), error events, and the [DONE]
// sentinel do not count. Unknown envelopes keep the historical data-line
// behavior so compatible vendors that do not follow the standard shape still
// get TTFT.
func isSSEFirstTokenLine(line []byte) bool {
	payload, ok := sseDataPayload(line)
	if !ok || bytes.Equal(payload, []byte("[DONE]")) {
		return false
	}
	var event struct {
		Type    string          `json:"type"`
		Choices json.RawMessage `json:"choices"`
		Error   json.RawMessage `json:"error"`
		Usage   json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return true
	}
	if !jsonBlank(event.Error) {
		return false
	}
	if !jsonBlank(event.Choices) {
		// OpenAI Chat Completions (choices[].delta) and Completions
		// (choices[].text).
		return choicesCarryOutput(event.Choices)
	}
	if event.Type != "" {
		return sseEventTypeCarriesOutput(event.Type)
	}
	// A data line without choices/type is output only if it is not a
	// usage-only terminal chunk.
	return jsonBlank(event.Usage)
}

// choicesCarryOutput reports whether an OpenAI choices array contains generated
// output: legacy Completions `text`, a Chat Completions delta with a
// non-empty output field (content, reasoning_content, tool_calls, ...), or a
// non-standard choice shape that is not clearly metadata. Role-only and
// finish_reason-only deltas are metadata, not output.
func choicesCarryOutput(raw json.RawMessage) bool {
	var choices []json.RawMessage
	if json.Unmarshal(raw, &choices) != nil {
		return true
	}
	for _, choice := range choices {
		if choiceCarriesOutput(choice) {
			return true
		}
	}
	return false
}

func choiceCarriesOutput(choice json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(choice, &fields) != nil {
		return true
	}
	if text, ok := fields["text"]; ok && !jsonZeroValue(text) {
		return true
	}
	if delta, ok := fields["delta"]; ok {
		return deltaCarriesOutput(delta)
	}
	// No delta: a non-standard wrapper (for example a per-chunk `message`)
	// counts as output; finish_reason/index/logprobs-only chunks do not.
	for key, value := range fields {
		switch key {
		case "index", "finish_reason", "logprobs":
			continue
		}
		if !jsonZeroValue(value) {
			return true
		}
	}
	return false
}

func deltaCarriesOutput(raw json.RawMessage) bool {
	if jsonBlank(raw) {
		return false
	}
	var delta map[string]json.RawMessage
	if json.Unmarshal(raw, &delta) != nil {
		return true
	}
	for key, value := range delta {
		if key == "role" || key == "finish_reason" {
			continue
		}
		if !jsonZeroValue(value) {
			return true
		}
	}
	return false
}

// sseEventTypeCarriesOutput reports whether a typed SSE event can carry output.
// Anthropic Messages emits output only in content_block_delta; OpenAI Responses
// emits it only in *.delta events (output_text.delta,
// function_call_arguments.delta, reasoning_summary_text.delta, ...). Lifecycle,
// metadata, error, and terminal events return false.
func sseEventTypeCarriesOutput(typ string) bool {
	switch typ {
	case "error", "ping", "message_start", "message_delta", "message_stop",
		"content_block_start", "content_block_stop":
		return false
	}
	if strings.HasPrefix(typ, "content_block_") {
		return typ == "content_block_delta"
	}
	if strings.HasPrefix(typ, "response.") {
		return strings.HasSuffix(typ, ".delta")
	}
	return true
}

// isSSETerminalLine recognizes the standard OpenAI Chat/Completions,
// Anthropic Messages, and OpenAI Responses completion markers. A clean EOF
// without one is treated as an incomplete stream.
func isSSETerminalLine(line []byte) bool {
	line = bytes.TrimSpace(line)
	if bytes.HasPrefix(line, []byte("event:")) {
		event := string(bytes.TrimSpace(line[len("event:"):]))
		return event == "message_stop" || event == "response.completed"
	}
	payload, ok := sseDataPayload(line)
	if !ok {
		return false
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		return true
	}
	var event struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(payload, &event) == nil &&
		(event.Type == "message_stop" || event.Type == "response.completed")
}

// isSSEErrorLine reports whether an SSE line announces an in-stream error:
// either the SSE `event: error` field or a JSON data payload with a non-empty
// top-level `error` member. Such a stream may still end with a completion
// marker (and a clean copy), but it is not a successful measurement window.
func isSSEErrorLine(line []byte) bool {
	line = bytes.TrimSpace(line)
	if bytes.HasPrefix(line, []byte("event:")) {
		return string(bytes.TrimSpace(line[len("event:"):])) == "error"
	}
	payload, ok := sseDataPayload(line)
	if !ok || bytes.Equal(payload, []byte("[DONE]")) {
		return false
	}
	var event struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return false
	}
	return streamErrorValue(event.Error)
}

// isNDJSONFirstTokenLine reports whether an Ollama-style NDJSON line carries
// generated output rather than an error, keepalive metadata, or the final
// `"done":true` object.
func isNDJSONFirstTokenLine(line []byte) bool {
	var event struct {
		Done     *bool           `json:"done"`
		Error    json.RawMessage `json:"error"`
		Message  json.RawMessage `json:"message"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(line, &event) != nil {
		return true // not a JSON object: preserve historical behavior
	}
	if !jsonBlank(event.Error) {
		return false
	}
	if event.Done != nil && *event.Done {
		return false
	}
	if !jsonBlank(event.Message) {
		var message struct {
			Content   json.RawMessage `json:"content"`
			Thinking  json.RawMessage `json:"thinking"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		}
		if json.Unmarshal(event.Message, &message) != nil {
			return true
		}
		return !jsonZeroValue(message.Content) ||
			!jsonZeroValue(message.Thinking) ||
			!jsonZeroValue(message.ToolCalls)
	}
	if !jsonBlank(event.Response) {
		// Ollama /api/generate uses a flat "response" string.
		return !jsonZeroValue(event.Response)
	}
	// No recognized field: a done:false metadata object is not output, but an
	// unknown envelope keeps the historical behavior for compatible vendors.
	return event.Done == nil
}

// isNDJSONTerminalLine reports Ollama's `"done":true` completion marker.
func isNDJSONTerminalLine(line []byte) bool {
	var event struct {
		Done bool `json:"done"`
	}
	return json.Unmarshal(line, &event) == nil && event.Done
}

// isNDJSONErrorLine reports whether an NDJSON line is an in-band error object
// (`{"error":"..."}`). A later `"done":true` line makes the copy look clean, so
// callers need this separate signal to reject the measurement window.
func isNDJSONErrorLine(line []byte) bool {
	var event struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &event) != nil {
		return false
	}
	return streamErrorValue(event.Error)
}

// streamErrorValue reports whether a streamed `error` field carries content.
// Healthy chunks usually omit the field or send `null`; empty placeholders do
// not count, so a normal chunk is never mistaken for an error event.
func streamErrorValue(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "", "null", `""`, "{}", "[]", "false":
		return false
	}
	return true
}

// jsonZeroValue reports whether raw is absent or an empty JSON value (null,
// "", {}, []). Streaming metadata deltas use these placeholder values.
func jsonZeroValue(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "", "null", `""`, "{}", "[]":
		return true
	}
	return false
}

func encodeResponseBlob(body []byte, sse bool) []byte {
	if len(body) == 0 {
		return nil
	}
	if !sse {
		return body
	}
	b, err := json.Marshal(string(body))
	if err != nil {
		return nil
	}
	return b
}

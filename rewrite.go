package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

type requestMeta struct {
	Model  string
	Stream bool
	Body   []byte
}

func parseRequest(body []byte) (*requestMeta, error) {
	raw, err := decodeJSONRawObject(body)
	if err != nil {
		return nil, fmt.Errorf("invalid json body: %w", err)
	}
	model, err := rawJSONString(raw["model"])
	if err != nil {
		return nil, fmt.Errorf("request body model must be a string")
	}
	if model == "" {
		return nil, fmt.Errorf("request body missing model")
	}
	stream, _ := rawJSONBool(raw["stream"])
	return &requestMeta{Model: model, Stream: stream, Body: body}, nil
}

func rewriteRequest(body []byte, upstreamModel string, stream bool, format string) ([]byte, error) {
	raw, err := decodeJSONRawObject(body)
	if err != nil {
		return nil, err
	}
	if upstreamModel != "" {
		b, err := encodeJSONValue(upstreamModel)
		if err != nil {
			return nil, err
		}
		raw["model"] = b
	}
	if stream && format == formatOpenAI {
		opts, err := ensureIncludeUsage(raw["stream_options"])
		if err != nil {
			return nil, err
		}
		raw["stream_options"] = opts
	}
	return encodeJSONRawObject(raw)
}

func ensureIncludeUsage(opts json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(opts)) == 0 || bytes.Equal(bytes.TrimSpace(opts), []byte("null")) {
		return json.RawMessage(`{"include_usage":true}`), nil
	}
	nested, err := decodeJSONRawObject(opts)
	if err != nil {
		return nil, err
	}
	if _, ok := nested["include_usage"]; ok {
		return opts, nil
	}
	nested["include_usage"] = json.RawMessage("true")
	return encodeJSONRawObject(nested)
}

func decodeJSONRawObject(body []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("json object required")
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing data")
		}
		return nil, err
	}
	return raw, nil
}

func encodeJSONRawObject(raw map[string]json.RawMessage) ([]byte, error) {
	return encodeJSONValue(raw)
}

func encodeJSONValue(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

func rawJSONString(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s, nil
}

func rawJSONBool(raw json.RawMessage) (bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, err
	}
	return b, nil
}

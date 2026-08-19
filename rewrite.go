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
	raw, err := decodeJSONObject(body)
	if err != nil {
		return nil, fmt.Errorf("invalid json body: %w", err)
	}
	model, _ := raw["model"].(string)
	if model == "" {
		return nil, fmt.Errorf("request body missing model")
	}
	stream, _ := raw["stream"].(bool)
	return &requestMeta{Model: model, Stream: stream, Body: body}, nil
}

func rewriteRequest(body []byte, upstreamModel string, stream bool, format string) ([]byte, error) {
	raw, err := decodeJSONObject(body)
	if err != nil {
		return nil, err
	}
	if upstreamModel != "" {
		raw["model"] = upstreamModel
	}
	if stream && format != formatAnthropic {
		opts, _ := raw["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		if _, ok := opts["include_usage"]; !ok {
			opts["include_usage"] = true
			raw["stream_options"] = opts
		}
	}
	return encodeJSONObject(raw)
}

func decodeJSONObject(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw map[string]any
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

func encodeJSONObject(raw map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(raw); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

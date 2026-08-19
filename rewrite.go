package main

import (
	"encoding/json"
	"fmt"
)

type requestMeta struct {
	Model  string
	Stream bool
	Body   []byte
}

func parseRequest(body []byte) (*requestMeta, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid json body: %w", err)
	}
	model, _ := raw["model"].(string)
	if model == "" {
		return nil, fmt.Errorf("request body missing model")
	}
	stream, _ := raw["stream"].(bool)
	return &requestMeta{Model: model, Stream: stream, Body: body}, nil
}

func rewriteRequest(body []byte, upstreamModel string, stream bool) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if upstreamModel != "" {
		raw["model"] = upstreamModel
	}
	if stream {
		opts, _ := raw["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		if _, ok := opts["include_usage"]; !ok {
			opts["include_usage"] = true
			raw["stream_options"] = opts
		}
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return out, nil
}

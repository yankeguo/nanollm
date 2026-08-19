package main

import (
	"encoding/json"
	"net/http"
	"time"
)

type Server struct {
	Config  *Config
	Metrics *Metrics
	Client  *http.Client
	started int64
}

func NewServer(cfg *Config, metrics *Metrics) *Server {
	return &Server{
		Config:  cfg,
		Metrics: metrics,
		Client:  defaultHTTPClient(),
		started: time.Now().Unix(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /v1/models/{model}", s.handleModel)
	proxy := &Proxy{Config: s.Config, Metrics: s.Metrics, Client: s.Client}
	mux.Handle("POST /v1/chat/completions", proxy)
	mux.Handle("POST /chat/completions", proxy)
	mux.Handle("POST /v1/completions", proxy)
	mux.Handle("POST /completions", proxy)
	mux.Handle("POST /v1/embeddings", proxy)
	mux.Handle("POST /embeddings", proxy)
	mux.Handle("POST /v1/responses", proxy)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("OK"))
}

type openaiModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	names := s.Config.modelNames()
	data := make([]openaiModel, 0, len(names))
	for _, name := range names {
		data = append(data, openaiModel{
			ID:      name,
			Object:  "model",
			Created: s.started,
			OwnedBy: "nanollm",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("model")
	if len(s.Config.providers(name)) == 0 {
		writeAPIError(w, http.StatusNotFound, "invalid_request_error", "the model `"+name+"` does not exist")
		return
	}
	writeJSON(w, http.StatusOK, openaiModel{
		ID:      name,
		Object:  "model",
		Created: s.started,
		OwnedBy: "nanollm",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    typ,
		},
	})
}

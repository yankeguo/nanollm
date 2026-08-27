package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"
)

type Server struct {
	Config   *Config
	Client   *http.Client
	Logger   CallLogger
	DB       *gorm.DB
	adminKey []byte
	started  int64
}

func NewServer(cfg *Config, logger CallLogger, db *gorm.DB) *Server {
	s := &Server{
		Config:  cfg,
		Client:  defaultClient,
		Logger:  logger,
		DB:      db,
		started: time.Now().Unix(),
	}
	if cfg != nil {
		s.adminKey = adminCookieKey(cfg.Admin.Username, cfg.Admin.Password)
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	mux.HandleFunc("GET /login", s.handleAdminLogin)
	mux.HandleFunc("POST /login", s.handleAdminLogin)
	mux.HandleFunc("POST /logout", s.handleAdminLogout)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/usage", http.StatusFound)
	})
	// The built JS/CSS bundles carry no secrets and the login page itself
	// links the stylesheet, so /static/ is unauthenticated.
	mux.Handle("GET /static/", staticHandler())
	mux.Handle("GET /usage", s.requireAdmin(http.HandlerFunc(s.handleAdminUsage)))
	mux.Handle("GET /calls", s.requireAdmin(http.HandlerFunc(s.handleAdminCalls)))
	mux.Handle("GET /calls/{id}", s.requireAdmin(http.HandlerFunc(s.handleAdminCall)))
	mux.Handle("GET /files/{sha}", s.requireAdmin(http.HandlerFunc(s.handleAdminFile)))

	// LLM routes mirror each vendor's official URL shape: the vendor host is
	// the first path segment, so clients only swap the endpoint host for this
	// proxy. The host prefix also isolates vendor namespaces (e.g. OpenAI and
	// Anthropic both have /v1/files). No unprefixed aliases are served.
	auth := func(h http.Handler) http.Handler { return s.requireAPIKey(h, protocolOpenAICompletions) }
	mux.Handle("GET /api.openai.com/v1/models", auth(http.HandlerFunc(s.handleModels)))
	mux.Handle("GET /api.openai.com/v1/models/{model...}", auth(http.HandlerFunc(s.handleModel)))
	chat := auth(&Proxy{Config: s.Config, Client: s.Client, Logger: s.Logger, Protocol: protocolOpenAICompletions, InjectStreamUsage: true})
	mux.Handle("POST /api.openai.com/v1/chat/completions", chat)
	mux.Handle("POST /api.openai.com/v1/completions", chat)
	embed := auth(&Proxy{Config: s.Config, Client: s.Client, Logger: s.Logger, Protocol: protocolOpenAIEmbeddings})
	mux.Handle("POST /api.openai.com/v1/embeddings", embed)
	resp := auth(&Proxy{Config: s.Config, Client: s.Client, Logger: s.Logger, Protocol: protocolOpenAIResponses})
	mux.Handle("POST /api.openai.com/v1/responses", resp)
	anth := s.requireAPIKey(&Proxy{Config: s.Config, Client: s.Client, Logger: s.Logger, Protocol: protocolAnthropicMessages}, protocolAnthropicMessages)
	mux.Handle("POST /api.anthropic.com/v1/messages", anth)
	mme := auth(&Proxy{Config: s.Config, Client: s.Client, Logger: s.Logger, Protocol: protocolBailianMultimodalEmbedding})
	mux.Handle("POST /dashscope.aliyuncs.com/api/v1/services/embeddings/multimodal-embedding/multimodal-embedding", mme)
	return withSecurityHeaders(mux)
}

// isAdminPage reports whether a path serves the cookie-authenticated
// dashboard, which gets no-store and a strict CSP.
func isAdminPage(path string) bool {
	if path == "/" {
		return true
	}
	for _, prefix := range []string{"/usage", "/calls", "/files", "/login", "/logout"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if isAdminPage(r.URL.Path) {
			h.Set("Cache-Control", "no-store")
			h.Set("Content-Security-Policy", strings.Join([]string{
				"default-src 'none'",
				"script-src 'self'",
				"style-src 'self' 'unsafe-inline'",
				"img-src 'self' data:",
				"media-src 'self'",
				"connect-src 'self'",
				"form-action 'self'",
				"base-uri 'none'",
				"frame-ancestors 'none'",
			}, "; "))
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("OK"))
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
	if s.Config.model(name) == nil {
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

func writeProtocolError(w http.ResponseWriter, protocol string, status int, typ, message string) {
	if protocol == protocolAnthropicMessages {
		writeJSON(w, status, map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    typ,
				"message": message,
			},
		})
		return
	}
	if protocol == protocolBailianMultimodalEmbedding {
		// DashScope errors are {"code": ..., "message": ...}.
		writeJSON(w, status, map[string]any{
			"code":    typ,
			"message": message,
		})
		return
	}
	writeAPIError(w, status, typ, message)
}

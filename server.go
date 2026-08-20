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

	mux.HandleFunc("GET /admin/login", s.handleAdminLogin)
	mux.HandleFunc("POST /admin/login", s.handleAdminLogin)
	mux.HandleFunc("POST /admin/logout", s.handleAdminLogout)
	mux.HandleFunc("GET /admin/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusFound)
	})
	mux.Handle("GET /admin", s.requireAdmin(http.HandlerFunc(s.handleAdminUsage)))
	mux.Handle("GET /admin/calls", s.requireAdmin(http.HandlerFunc(s.handleAdminCalls)))
	mux.Handle("GET /admin/calls/{id}", s.requireAdmin(http.HandlerFunc(s.handleAdminCall)))

	auth := func(h http.Handler) http.Handler { return s.requireAPIKey(h, formatOpenAI) }
	mux.Handle("GET /v1/models", auth(http.HandlerFunc(s.handleModels)))
	mux.Handle("GET /v1/models/{model...}", auth(http.HandlerFunc(s.handleModel)))
	proxy := auth(&Proxy{Config: s.Config, Client: s.Client, Logger: s.Logger, Format: formatOpenAI})
	mux.Handle("POST /v1/chat/completions", proxy)
	mux.Handle("POST /chat/completions", proxy)
	mux.Handle("POST /v1/completions", proxy)
	mux.Handle("POST /completions", proxy)
	mux.Handle("POST /v1/embeddings", proxy)
	mux.Handle("POST /embeddings", proxy)
	resp := auth(&Proxy{Config: s.Config, Client: s.Client, Logger: s.Logger, Format: formatResponses})
	mux.Handle("POST /v1/responses", resp)
	mux.Handle("POST /responses", resp)
	anth := s.requireAPIKey(&Proxy{Config: s.Config, Client: s.Client, Logger: s.Logger, Format: formatAnthropic}, formatAnthropic)
	mux.Handle("POST /v1/messages", anth)
	return withSecurityHeaders(mux)
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/admin") {
			h.Set("Cache-Control", "no-store")
			h.Set("Content-Security-Policy", strings.Join([]string{
				"default-src 'none'",
				"script-src https://cdn.jsdelivr.net 'unsafe-inline'",
				"style-src 'unsafe-inline'",
				"img-src 'self' data:",
				"connect-src 'self' https://api.iconify.design https://api.simplesvg.com https://api.unisvg.com",
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

func writeFormatError(w http.ResponseWriter, format string, status int, typ, message string) {
	if format == formatAnthropic {
		writeJSON(w, status, map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    typ,
				"message": message,
			},
		})
		return
	}
	writeAPIError(w, status, typ, message)
}

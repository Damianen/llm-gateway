// Package server wires the gateway's HTTP surface: OpenAI- and
// Anthropic-compatible inbound endpoints, the admin API, health, and metrics.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Damianen/llm-gateway/internal/cache"
	"github.com/Damianen/llm-gateway/internal/config"
	"github.com/Damianen/llm-gateway/internal/ratelimit"
	"github.com/Damianen/llm-gateway/internal/router"
	"github.com/Damianen/llm-gateway/internal/store"
)

// Options are the dependencies injected into New.
type Options struct {
	Config     *config.Config
	Logger     *slog.Logger
	Store      *store.Store
	Router     *router.Router
	Cache      *cache.Cache       // nil disables response caching
	Limiter    *ratelimit.Limiter // nil disables rate limiting
	AdminToken string
	Clock      func() time.Time // nil means time.Now
	Version    string
}

// Server handles all gateway HTTP traffic.
type Server struct {
	cfg        *config.Config
	logger     *slog.Logger
	store      *store.Store
	router     *router.Router
	cache      *cache.Cache
	limiter    *ratelimit.Limiter
	adminToken string
	clock      func() time.Time
	version    string
}

// New constructs a Server from its dependencies.
func New(opts Options) *Server {
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Server{
		cfg:        opts.Config,
		logger:     logger,
		store:      opts.Store,
		router:     opts.Router,
		cache:      opts.Cache,
		limiter:    opts.Limiter,
		adminToken: opts.AdminToken,
		clock:      clock,
		version:    opts.Version,
	}
}

// Handler builds the full route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)

	mux.Handle("POST /v1/chat/completions", s.requireKey(http.HandlerFunc(s.handleChatCompletions)))
	mux.Handle("POST /v1/messages", s.requireKey(http.HandlerFunc(s.handleMessages)))
	mux.Handle("GET /v1/models", s.requireKey(http.HandlerFunc(s.handleListModels)))

	mux.HandleFunc("POST /admin/projects", s.requireAdmin(s.handleCreateProject))
	mux.HandleFunc("GET /admin/projects", s.requireAdmin(s.handleListProjects))
	mux.HandleFunc("POST /admin/keys", s.requireAdmin(s.handleCreateKey))
	mux.HandleFunc("GET /admin/keys", s.requireAdmin(s.handleListKeys))
	mux.HandleFunc("POST /admin/keys/{id}/revoke", s.requireAdmin(s.handleRevokeKey))
	mux.HandleFunc("GET /admin/usage", s.requireAdmin(s.handleUsage))

	return s.recoverPanics(s.withRequestID(mux))
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		s.logger.Error("healthz: database unreachable", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "reason": "database unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

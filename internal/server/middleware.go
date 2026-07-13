package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/Damianen/llm-gateway/internal/auth"
	"github.com/Damianen/llm-gateway/internal/store"
)

type ctxKey int

const (
	ctxKeyAPIKey ctxKey = iota
	ctxKeyRequestID
)

// keyFromContext returns the authenticated API key, or nil.
func keyFromContext(ctx context.Context) *store.APIKey {
	k, _ := ctx.Value(ctxKeyAPIKey).(*store.APIKey)
	return k
}

// requestIDFromContext returns the per-request ID, or "".
func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// withRequestID tags every request with an ID, echoed in a response header.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Gateway-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

// recoverPanics turns handler panics into 500s without leaking internals.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic in handler",
					"request_id", requestIDFromContext(r.Context()),
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()))
				writeDialectError(w, dialectForPath(r.URL.Path), http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// dialectForPath picks the error dialect for a request path: Anthropic shape
// for /v1/messages, OpenAI shape elsewhere under /v1.
func dialectForPath(path string) string {
	if strings.HasPrefix(path, "/v1/messages") {
		return dialectAnthropic
	}
	return dialectOpenAI
}

// bearerOrAPIKey extracts the client credential from Authorization: Bearer
// (OpenAI convention) or x-api-key (Anthropic convention).
func bearerOrAPIKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if tok, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(tok)
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// requireKey authenticates a virtual key and attaches it to the context.
// Errors are shaped for the inbound dialect and never echo the presented key.
func (s *Server) requireKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialect := dialectForPath(r.URL.Path)
		presented := bearerOrAPIKey(r)
		if presented == "" {
			writeDialectError(w, dialect, http.StatusUnauthorized, "missing_api_key",
				"missing API key: pass a gateway key via Authorization: Bearer or x-api-key")
			return
		}
		key, err := s.store.GetKeyByHash(r.Context(), auth.HashKey(presented))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeDialectError(w, dialect, http.StatusUnauthorized, "invalid_api_key", "invalid API key")
				return
			}
			s.logger.Error("key lookup failed", "request_id", requestIDFromContext(r.Context()), "err", err)
			writeDialectError(w, dialect, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		if key.Revoked {
			writeDialectError(w, dialect, http.StatusUnauthorized, "invalid_api_key", "API key has been revoked")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyAPIKey, key)))
	})
}

// requireAdmin guards the admin API with GATEWAY_ADMIN_TOKEN.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.adminToken == "" {
			writeAdminError(w, http.StatusServiceUnavailable, "admin API disabled: GATEWAY_ADMIN_TOKEN is not set")
			return
		}
		presented := bearerOrAPIKey(r)
		if presented == "" || !auth.Equal(presented, s.adminToken) {
			writeAdminError(w, http.StatusUnauthorized, "invalid admin token")
			return
		}
		next(w, r)
	}
}

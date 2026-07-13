package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Damianen/llm-gateway/internal/provider"
	"github.com/Damianen/llm-gateway/internal/router"
	"github.com/Damianen/llm-gateway/internal/store"
)

const maxRequestBody = 32 << 20

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.handleCompletion(w, r, openaiDialect{})
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handleCompletion(w, r, anthropicDialect{})
}

// handleCompletion is the shared pipeline behind both inbound surfaces:
// parse dialect -> canonical -> resolve model -> route (with fallback) ->
// translate back -> account.
func (s *Server) handleCompletion(w http.ResponseWriter, r *http.Request, d dialect) {
	started := time.Now()
	key := keyFromContext(r.Context())

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		d.writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the gateway limit")
		return
	}
	req, opts, aerr := d.parseRequest(body)
	if aerr != nil {
		d.writeError(w, aerr.status, aerr.code, aerr.message)
		return
	}
	if s.cfg.Server.LogBodies {
		s.logger.Info("request body",
			"request_id", requestIDFromContext(r.Context()),
			"endpoint", d.endpoint(),
			"body", string(body))
	}

	entry, ok := s.router.Resolve(req.Model)
	if !ok {
		d.writeError(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("model %q is not available on this gateway", req.Model))
		return
	}

	if req.Stream {
		s.handleStreamingCompletion(w, r, d, req, opts, entry, key, started)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Server.RequestTimeout.Std())
	defer cancel()
	result, err := s.router.Complete(ctx, entry, req)
	latencyMS := time.Since(started).Milliseconds()
	if err != nil {
		status, code, msg := mapUpstreamError(err)
		s.record(r, requestRecord{
			dialect: d, key: key, model: entry.Name, served: entry,
			latencyMS: latencyMS, status: status,
		})
		s.logger.Warn("upstream request failed",
			"request_id", requestIDFromContext(r.Context()),
			"model", entry.Name, "status", status, "err", err)
		d.writeError(w, status, code, msg)
		return
	}

	resp := result.Response
	cost := result.Entry.Pricing.Cost(resp.Usage.InputTokens, resp.Usage.OutputTokens)
	s.record(r, requestRecord{
		dialect: d, key: key, model: entry.Name, served: result.Entry,
		usage: resp.Usage, cost: cost, latencyMS: latencyMS, status: http.StatusOK,
		fallbackUsed: result.FallbackUsed,
	})
	if s.cfg.Server.LogBodies {
		s.logResponseBody(r, d, resp)
	}
	d.writeResponse(w, opts.requestedModel, resp, s.clock())
}

// handleStreamingCompletion is implemented in Phase 4.
func (s *Server) handleStreamingCompletion(w http.ResponseWriter, r *http.Request, d dialect,
	req *provider.Request, opts *inboundOpts, entry *router.Entry, key *store.APIKey, started time.Time) {
	d.writeError(w, http.StatusNotImplemented, "streaming_unavailable", "streaming is not implemented yet")
}

// requestRecord carries everything the request log, slog line, and metrics
// need about one finished request.
type requestRecord struct {
	dialect      dialect
	key          *store.APIKey
	model        string        // resolved gateway model name
	served       *router.Entry // entry that served (or was last attempted)
	usage        provider.Usage
	cost         float64
	latencyMS    int64
	status       int
	cacheHit     bool
	fallbackUsed bool
	stream       bool
}

func (s *Server) record(r *http.Request, rec requestRecord) {
	row := &store.RequestLog{
		Time:          s.clock(),
		ProjectID:     rec.key.ProjectID,
		KeyID:         rec.key.ID,
		Endpoint:      rec.dialect.endpoint(),
		Model:         rec.model,
		Provider:      rec.served.ProviderType,
		UpstreamModel: rec.served.UpstreamModel,
		InputTokens:   rec.usage.InputTokens,
		OutputTokens:  rec.usage.OutputTokens,
		CostUSD:       rec.cost,
		LatencyMS:     rec.latencyMS,
		Status:        rec.status,
		CacheHit:      rec.cacheHit,
		FallbackUsed:  rec.fallbackUsed,
		Stream:        rec.stream,
	}
	if err := s.store.LogRequest(context.WithoutCancel(r.Context()), row); err != nil {
		s.logger.Error("failed to record request", "request_id", requestIDFromContext(r.Context()), "err", err)
	}
	s.logger.Info("request",
		"request_id", requestIDFromContext(r.Context()),
		"project", rec.key.ProjectName,
		"key_id", rec.key.ID,
		"endpoint", rec.dialect.endpoint(),
		"model", rec.model,
		"provider", rec.served.ProviderType,
		"upstream_model", rec.served.UpstreamModel,
		"status", rec.status,
		"latency_ms", rec.latencyMS,
		"input_tokens", rec.usage.InputTokens,
		"output_tokens", rec.usage.OutputTokens,
		"cost_usd", rec.cost,
		"cache_hit", rec.cacheHit,
		"fallback", rec.fallbackUsed,
		"stream", rec.stream,
	)
}

func (s *Server) logResponseBody(r *http.Request, d dialect, resp *provider.Response) {
	s.logger.Info("response body",
		"request_id", requestIDFromContext(r.Context()),
		"endpoint", d.endpoint(),
		"body", resp)
}

// mapUpstreamError converts a routing failure into a client-facing status.
// Upstream auth problems are the gateway's configuration fault and are never
// echoed as 401s to clients (and never include upstream key material).
func mapUpstreamError(err error) (status int, code, message string) {
	ue, ok := provider.AsUpstreamError(err)
	if !ok {
		return http.StatusBadGateway, "upstream_error", "upstream request failed"
	}
	msg := ue.Message
	switch {
	case ue.StatusCode == http.StatusBadRequest:
		if msg == "" {
			msg = "upstream rejected the request"
		}
		return http.StatusBadRequest, "upstream_invalid_request", msg
	case ue.StatusCode == http.StatusUnauthorized || ue.StatusCode == http.StatusForbidden:
		return http.StatusBadGateway, "upstream_auth_error",
			"upstream authentication failed (check the gateway's provider key configuration)"
	case ue.StatusCode == http.StatusNotFound:
		if msg == "" {
			msg = "upstream model or endpoint not found"
		}
		return http.StatusBadGateway, "upstream_not_found", msg
	case ue.StatusCode == http.StatusTooManyRequests:
		if msg == "" {
			msg = "upstream rate limit exceeded"
		}
		return http.StatusTooManyRequests, "upstream_rate_limited", msg
	case ue.StatusCode == 0:
		if errors.Is(err, context.DeadlineExceeded) {
			return http.StatusGatewayTimeout, "upstream_timeout", "upstream timed out"
		}
		return http.StatusBadGateway, "upstream_unreachable", "upstream request failed"
	default:
		if msg == "" {
			msg = "upstream error"
		}
		return http.StatusBadGateway, "upstream_error", msg
	}
}

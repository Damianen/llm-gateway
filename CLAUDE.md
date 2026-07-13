# llm-gateway — project conventions & build plan

Self-hosted LLM gateway in Go. One canonical request/response model; OpenAI- and Anthropic-compatible inbound surfaces; Anthropic and OpenAI-compatible outbound adapters; SQLite for everything persistent.

## Conventions

- **Dependencies:** stdlib first. Allowed third-party: `modernc.org/sqlite`, `gopkg.in/yaml.v3`, `github.com/prometheus/client_golang`. Anything else needs a justifying comment. No web framework, no ORM.
- **Routing:** `net/http` with Go 1.22+ pattern routing (`mux.HandleFunc("POST /v1/messages", ...)`).
- **Logging:** `log/slog` JSON. Never log `Authorization` headers, `x-api-key` headers, or key material — including error paths. Request/response bodies only when `log_bodies: true`.
- **Errors:** wrap with `%w`. Inbound errors are returned in the dialect of the inbound surface (OpenAI error JSON on `/v1/chat/completions`, Anthropic error JSON on `/v1/messages`) with correct status codes.
- **Design:** small interfaces (`provider.Provider` = `Complete` + `Stream`), constructor injection, no package-level mutable state. Context propagation everywhere; per-request and per-upstream timeouts; graceful shutdown on SIGTERM.
- **Tokens/cost:** token counts come only from provider-returned usage. Never tokenize locally. Streaming: OpenAI-compatible upstreams get `stream_options: {"include_usage": true}` forced; Anthropic usage is read from `message_start`/`message_delta`.
- **Secrets:** upstream provider keys come only from env vars named in config. Virtual keys are stored as SHA-256 hashes. Admin API guarded by `GATEWAY_ADMIN_TOKEN` (constant-time compare).
- **Tests:** `httptest` fake upstreams only (`internal/upstreamfake`) — no real network. Golden files for wire payloads, canned SSE fixtures for streams. Every phase ends `go vet ./...` clean and `go test -race ./...` green.
- **Commits:** conventional messages (`feat:`, `fix:`, `test:`, `docs:`, `ci:`, `build:`, `chore:`).

## Architecture (target)

```
inbound /v1/chat/completions (OpenAI dialect) ─┐
inbound /v1/messages (Anthropic dialect) ──────┤→ canonical provider.Request
                                               │
                    auth (virtual keys) → rate limit → cache → router (fallback chains)
                                               │
                        ┌──────────────────────┴──────────────────────┐
                 anthropic adapter                          openai-compat adapter
                 (api.anthropic.com)                (OpenRouter / Ollama / llama.cpp / …)
                                               │
              SQLite (projects, keys, request log, cache) + Prometheus /metrics
```

- `internal/provider` — canonical types + `Provider` interface + `UpstreamError`.
- `internal/provider/anthropic`, `internal/provider/openaicompat` — outbound adapters.
- `internal/server` — inbound dialects, shared pipeline, streaming writers, admin API.
- `internal/router` — model registry (names + aliases) and fallback chains.
- `internal/store` — SQLite + embedded migrations (`migrations/` package).
- `internal/cache`, `internal/ratelimit`, `internal/metrics`, `internal/auth`, `internal/config`, `internal/sse`.

### Adding a provider adapter

1. Implement `provider.Provider` (`Complete(ctx, *Request) (*Response, error)`, `Stream(ctx, *Request) (Stream, error)`) in `internal/provider/<name>/`.
2. Translate canonical ↔ wire types in both directions; map finish reasons; classify upstream failures as `*provider.UpstreamError` (429/5xx/transport ⇒ retryable, used by fallback).
3. For streams, emit canonical events in the guaranteed order: `start`, block starts before deltas, every `block_stop` before `finish`, then `done`; extract usage from the provider's stream frames.
4. Register the provider type in `internal/router` construction and in config validation.
5. Add a fake in `internal/upstreamfake` + golden/table tests mirroring the existing adapters.

## Build plan (tick per phase; each phase = vet clean, tests green with -race, conventional commit, push)

- [x] **Phase 0** — repo bootstrap: scaffold, module, CI (vet+test+build), first push, CI triggers.
- [x] **Phase 1** — canonical types; YAML config + validation + example; SQLite store + embedded migrations; auth (hashed virtual keys); admin API (projects/keys/usage) + healthz; auth middleware; unit tests.
- [x] **Phase 2** — outbound adapters non-streaming: anthropic + openai-compatible; fake upstreams; table-driven translation tests + golden wire payloads; tool-call round trips both directions.
- [x] **Phase 3** — inbound surfaces + routing: both dialect handlers (non-streaming), /v1/models, fallback chains with annotations; e2e matrix (2 dialects × 2 providers × plain/tool) asserting SQLite usage rows.
- [x] **Phase 4** — streaming: SSE plumbing, both-direction translation (text + tool-call deltas), terminal events, usage extraction from streams, mid-stream error handling; fixture-driven event-by-event tests. (Streaming tool calls fully working — no flag/skip needed.)
- [ ] **Phase 5** — exact-match cache (incl. replay-as-stream), RPM/TPM token buckets (fake clock tests), 429 + Retry-After in dialect, request log finalized, /admin/usage exact-totals test.
- [ ] **Phase 6** — Prometheus metrics, startup summary log, scripts/smoke.sh, CI integration job.
- [ ] **Phase 7** — Dockerfile (distroless, non-root), docker-compose, systemd unit, full README (architecture diagram, quickstart, config/metrics reference, security, roadmap), CLAUDE.md finalized.
- [ ] **Phase 8** — release workflow (multi-arch → GHCR), full green suite, optional live smoke, tag v0.1.0, confirm release, final report.

## Commands

```sh
go vet ./...
go test -race ./...
go build ./cmd/gateway
./scripts/smoke.sh          # end-to-end against fake upstreams (Phase 6+)
```

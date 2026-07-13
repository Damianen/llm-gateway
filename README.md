# llm-gateway

Self-hosted LLM gateway in Go: one API for Anthropic, OpenRouter, and local models — routing, fallback, cost tracking, caching, metrics.

> **Status:** under construction. Full documentation lands with v0.1.0.

- OpenAI-compatible `POST /v1/chat/completions` and Anthropic-compatible `POST /v1/messages` inbound; both translate to one canonical model.
- Outbound adapters for Anthropic and any OpenAI-compatible upstream (OpenRouter, Ollama, llama.cpp, LM Studio).
- Single SQLite file for keys, projects, request log, usage, and cache. No external services.

## License

MIT

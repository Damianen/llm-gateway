// Package metrics defines the gateway's Prometheus instrumentation. All
// collectors live on a private registry injected where needed — no
// package-level mutable state.
package metrics

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every gateway collector.
type Metrics struct {
	registry  *prometheus.Registry
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	tokens    *prometheus.CounterVec
	cost      *prometheus.CounterVec
	cacheHits *prometheus.CounterVec
	fallbacks *prometheus.CounterVec
}

// New builds a registry with go/process collectors and the gateway metrics.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	m := &Metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Completed gateway requests (rate-limited requests count with status 429).",
		}, []string{"project", "model", "provider", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_request_duration_seconds",
			Help: "Gateway request latency (streams: full stream duration).",
			// LLM requests routinely run tens of seconds.
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120},
		}, []string{"project", "model", "provider", "status"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_tokens_total",
			Help: "Provider-reported tokens by direction (cache hits contribute none).",
		}, []string{"project", "model", "provider", "direction"}),
		cost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_cost_usd_total",
			Help: "Accumulated cost in USD, computed from configured pricing.",
		}, []string{"project", "model", "provider"}),
		cacheHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_cache_hits_total",
			Help: "Requests served from the exact-match response cache.",
		}, []string{"project", "model"}),
		fallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_fallbacks_total",
			Help: "Requests served by a fallback entry, labeled by the provider that served.",
		}, []string{"model", "provider"}),
	}
	reg.MustRegister(m.requests, m.duration, m.tokens, m.cost, m.cacheHits, m.fallbacks)
	return m
}

// Handler serves the /metrics exposition endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ObserveRequest records one finished (or rejected) request.
func (m *Metrics) ObserveRequest(project, model, providerType string, status int, seconds float64,
	inputTokens, outputTokens int, costUSD float64, cacheHit, fallback bool) {
	s := strconv.Itoa(status)
	m.requests.WithLabelValues(project, model, providerType, s).Inc()
	m.duration.WithLabelValues(project, model, providerType, s).Observe(seconds)
	if inputTokens > 0 {
		m.tokens.WithLabelValues(project, model, providerType, "input").Add(float64(inputTokens))
	}
	if outputTokens > 0 {
		m.tokens.WithLabelValues(project, model, providerType, "output").Add(float64(outputTokens))
	}
	if costUSD > 0 {
		m.cost.WithLabelValues(project, model, providerType).Add(costUSD)
	}
	if cacheHit {
		m.cacheHits.WithLabelValues(project, model).Inc()
	}
	if fallback {
		m.fallbacks.WithLabelValues(model, providerType).Inc()
	}
}

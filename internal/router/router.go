// Package router resolves gateway model names (and aliases) to provider
// adapters and walks fallback chains on retryable upstream failures.
package router

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/Damianen/llm-gateway/internal/config"
	"github.com/Damianen/llm-gateway/internal/provider"
	"github.com/Damianen/llm-gateway/internal/provider/anthropic"
	"github.com/Damianen/llm-gateway/internal/provider/openaicompat"
)

// Entry is one model registry entry bound to its provider adapter.
type Entry struct {
	Name          string
	UpstreamModel string
	ProviderType  string
	Pricing       config.Pricing
	Enabled       bool
	Provider      provider.Provider
	Fallback      []*Entry
}

// Router holds the model registry.
type Router struct {
	byName map[string]*Entry // primary names and aliases
	names  []string          // enabled primary names, in config order
}

// New builds adapters for every model entry and wires fallback chains.
// Config validation has already guaranteed name uniqueness and that fallback
// references resolve.
func New(cfg *config.Config, hc *http.Client) (*Router, error) {
	r := &Router{byName: make(map[string]*Entry)}
	entries := make([]*Entry, len(cfg.Models))
	for i := range cfg.Models {
		m := &cfg.Models[i]
		var p provider.Provider
		switch m.Provider {
		case config.ProviderAnthropic:
			p = anthropic.New(m.BaseURL, m.APIKey(), hc, cfg.Server.DefaultMaxTokens, cfg.Server.UpstreamTimeout.Std())
		case config.ProviderOpenAI:
			p = openaicompat.New(m.BaseURL, m.APIKey(), hc, cfg.Server.UpstreamTimeout.Std())
		default:
			return nil, fmt.Errorf("model %q: unknown provider %q", m.Name, m.Provider)
		}
		e := &Entry{
			Name:          m.Name,
			UpstreamModel: m.UpstreamModel,
			ProviderType:  m.Provider,
			Pricing:       m.Pricing,
			Enabled:       m.IsEnabled(),
			Provider:      p,
		}
		entries[i] = e
		r.byName[m.Name] = e
		for _, a := range m.Aliases {
			r.byName[a] = e
		}
		if e.Enabled {
			r.names = append(r.names, m.Name)
		}
	}
	for i := range cfg.Models {
		for _, fb := range cfg.Models[i].Fallback {
			entries[i].Fallback = append(entries[i].Fallback, r.byName[fb])
		}
	}
	return r, nil
}

// Resolve maps a gateway model name or alias to its enabled entry.
func (r *Router) Resolve(name string) (*Entry, bool) {
	e, ok := r.byName[name]
	if !ok || !e.Enabled {
		return nil, false
	}
	return e, true
}

// ModelNames lists enabled primary model names in config order.
func (r *Router) ModelNames() []string { return slices.Clone(r.names) }

// Result is the outcome of a routed completion.
type Result struct {
	Response     *provider.Response
	Entry        *Entry // the entry that actually served the request
	FallbackUsed bool
	Attempts     int
}

// Complete tries the entry, then its fallback chain, advancing only on
// retryable upstream errors (429/5xx/transport/timeout).
func (r *Router) Complete(ctx context.Context, e *Entry, req *provider.Request) (*Result, error) {
	var lastErr error
	attempts := 0
	for _, cand := range e.chain() {
		if !cand.Enabled {
			continue
		}
		attempts++
		creq := *req
		creq.Model = cand.UpstreamModel
		resp, err := cand.Provider.Complete(ctx, &creq)
		if err == nil {
			return &Result{Response: resp, Entry: cand, FallbackUsed: cand != e, Attempts: attempts}, nil
		}
		lastErr = err
		if ue, ok := provider.AsUpstreamError(err); !ok || !ue.Retryable() {
			return nil, err
		}
		if ctx.Err() != nil {
			// The overall request deadline is spent; walking further would
			// only produce more context errors.
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("model %q: no enabled entry in fallback chain", e.Name)
	}
	return nil, lastErr
}

func (e *Entry) chain() []*Entry {
	return append([]*Entry{e}, e.Fallback...)
}

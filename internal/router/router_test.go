package router

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Damianen/llm-gateway/internal/config"
	"github.com/Damianen/llm-gateway/internal/provider"
	"github.com/Damianen/llm-gateway/internal/upstreamfake"
)

type routerEnv struct {
	router *Router
	ant    *upstreamfake.Fake
	oai    *upstreamfake.Fake
}

func newRouterEnv(t *testing.T) *routerEnv {
	t.Helper()
	ant := upstreamfake.NewAnthropic()
	oai := upstreamfake.NewOpenAI()
	antTS := httptest.NewServer(ant)
	oaiTS := httptest.NewServer(oai)
	t.Cleanup(antTS.Close)
	t.Cleanup(oaiTS.Close)

	disabled := false
	cfg := &config.Config{
		Server: config.Server{UpstreamTimeout: config.Duration(2 * time.Second), DefaultMaxTokens: 1024},
		Models: []config.Model{
			{
				Name: "sonnet", Provider: config.ProviderAnthropic, UpstreamModel: "claude-sonnet-4-6",
				BaseURL: antTS.URL, Pricing: config.Pricing{InputPerMTok: 3, OutputPerMTok: 15},
				Aliases: []string{"claude-sonnet"},
				// The disabled entry must be skipped without counting as an attempt.
				Fallback: []string{"disabled-or", "sonnet-or"},
			},
			{
				Name: "disabled-or", Provider: config.ProviderOpenAI, UpstreamModel: "or/disabled",
				BaseURL: oaiTS.URL + "/v1", Enabled: &disabled,
			},
			{
				Name: "sonnet-or", Provider: config.ProviderOpenAI, UpstreamModel: "or/claude-sonnet",
				BaseURL: oaiTS.URL + "/v1", Pricing: config.Pricing{InputPerMTok: 3, OutputPerMTok: 15},
			},
		},
	}
	r, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return &routerEnv{router: r, ant: ant, oai: oai}
}

func testReq() *provider.Request {
	return &provider.Request{
		Model:    "sonnet",
		Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("hi")}}},
	}
}

func TestResolve(t *testing.T) {
	e := newRouterEnv(t)
	if entry, ok := e.router.Resolve("sonnet"); !ok || entry.Name != "sonnet" {
		t.Errorf("Resolve(sonnet) = %+v, %v", entry, ok)
	}
	if entry, ok := e.router.Resolve("claude-sonnet"); !ok || entry.Name != "sonnet" {
		t.Errorf("Resolve(alias) = %+v, %v", entry, ok)
	}
	if _, ok := e.router.Resolve("ghost"); ok {
		t.Error("Resolve(ghost) should fail")
	}
	if _, ok := e.router.Resolve("disabled-or"); ok {
		t.Error("Resolve(disabled) should fail")
	}
	names := e.router.ModelNames()
	if len(names) != 2 || names[0] != "sonnet" || names[1] != "sonnet-or" {
		t.Errorf("ModelNames = %v", names)
	}
}

func TestFallbackOnRetryableError(t *testing.T) {
	e := newRouterEnv(t)
	e.ant.Enqueue(upstreamfake.Step{Status: 429})

	entry, _ := e.router.Resolve("sonnet")
	res, err := e.router.Complete(context.Background(), entry, testReq())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Entry.Name != "sonnet-or" || !res.FallbackUsed || res.Attempts != 2 {
		t.Errorf("result = served-by %q fallback=%v attempts=%d", res.Entry.Name, res.FallbackUsed, res.Attempts)
	}
	if res.Response.Text() != "Hello from fake openai." {
		t.Errorf("response text = %q", res.Response.Text())
	}

	// Each attempt must use its own entry's upstream model id.
	var antWire, oaiWire struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(e.ant.LastRequest().Body, &antWire); err != nil || antWire.Model != "claude-sonnet-4-6" {
		t.Errorf("anthropic attempt model = %q (%v)", antWire.Model, err)
	}
	if err := json.Unmarshal(e.oai.LastRequest().Body, &oaiWire); err != nil || oaiWire.Model != "or/claude-sonnet" {
		t.Errorf("openai attempt model = %q (%v)", oaiWire.Model, err)
	}
}

func TestFallbackChainFailsThrough(t *testing.T) {
	e := newRouterEnv(t)
	e.ant.Enqueue(upstreamfake.Step{Status: 529})
	e.oai.Enqueue(upstreamfake.Step{Status: 500})

	entry, _ := e.router.Resolve("sonnet")
	_, err := e.router.Complete(context.Background(), entry, testReq())
	ue, ok := provider.AsUpstreamError(err)
	if !ok {
		t.Fatalf("error = %v, want UpstreamError", err)
	}
	// The last attempt's error surfaces (openai 500).
	if ue.Provider != "openai" || ue.StatusCode != 500 {
		t.Errorf("final error = %+v", ue)
	}
}

func TestNonRetryableStopsChain(t *testing.T) {
	e := newRouterEnv(t)
	e.ant.Enqueue(upstreamfake.Step{Status: 400, Body: `{"type":"error","error":{"type":"invalid_request_error","message":"bad tool schema"}}`})

	entry, _ := e.router.Resolve("sonnet")
	_, err := e.router.Complete(context.Background(), entry, testReq())
	ue, ok := provider.AsUpstreamError(err)
	if !ok || ue.StatusCode != 400 || ue.Message != "bad tool schema" {
		t.Fatalf("error = %v", err)
	}
	if got := len(e.oai.Requests()); got != 0 {
		t.Errorf("fallback should not run after a non-retryable error; openai got %d requests", got)
	}
}

func TestSuccessWithoutFallback(t *testing.T) {
	e := newRouterEnv(t)
	entry, _ := e.router.Resolve("sonnet")
	res, err := e.router.Complete(context.Background(), entry, testReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.FallbackUsed || res.Entry.Name != "sonnet" || res.Attempts != 1 {
		t.Errorf("result = %+v", res)
	}
	if got := len(e.oai.Requests()); got != 0 {
		t.Errorf("openai fake should be untouched, got %d requests", got)
	}
}

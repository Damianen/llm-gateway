package config

import (
	"strings"
	"testing"
	"time"
)

const validYAML = `
server:
  listen: ":0"
models:
  - name: sonnet
    provider: anthropic
    upstream_model: claude-sonnet-4-6
    base_url: https://api.anthropic.com
    api_key_env: TEST_ANTHROPIC_KEY
    pricing: { input_usd_per_mtok: 3, output_usd_per_mtok: 15 }
    aliases: [claude-sonnet]
    fallback: [fast]
  - name: fast
    provider: openai
    upstream_model: gpt-fast
    base_url: https://example.com/v1
    api_key_env: TEST_OPENAI_KEY
`

func TestParseValidAppliesDefaults(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "k1")
	t.Setenv("TEST_OPENAI_KEY", "k2")

	cfg, err := Parse(strings.NewReader(validYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Server.RequestTimeout.Std() != 120*time.Second {
		t.Errorf("request_timeout default = %v", cfg.Server.RequestTimeout.Std())
	}
	if cfg.Server.UpstreamTimeout.Std() != 60*time.Second {
		t.Errorf("upstream_timeout default = %v", cfg.Server.UpstreamTimeout.Std())
	}
	if cfg.Server.DefaultMaxTokens != 1024 {
		t.Errorf("default_max_tokens default = %d", cfg.Server.DefaultMaxTokens)
	}
	if cfg.Server.LogLevel != "info" {
		t.Errorf("log_level default = %q", cfg.Server.LogLevel)
	}
	if cfg.Cache.TTL.Std() != 5*time.Minute {
		t.Errorf("cache ttl default = %v", cfg.Cache.TTL.Std())
	}
	if cfg.Database.Path != "data/gateway.db" {
		t.Errorf("database path default = %q", cfg.Database.Path)
	}

	m, ok := cfg.ModelByName("claude-sonnet")
	if !ok || m.Name != "sonnet" {
		t.Fatalf("alias resolution failed: %v %v", m, ok)
	}
	if !m.IsEnabled() {
		t.Error("model should default to enabled")
	}
	if m.APIKey() != "k1" {
		t.Errorf("APIKey() = %q", m.APIKey())
	}
	if got := m.Pricing.Cost(1_000_000, 1_000_000); got != 18 {
		t.Errorf("Cost(1M, 1M) = %v, want 18", got)
	}
	if got := m.Pricing.Cost(100, 10); got != 100*3.0/1e6+10*15.0/1e6 {
		t.Errorf("Cost(100, 10) = %v", got)
	}
}

func TestParseErrors(t *testing.T) {
	t.Setenv("TEST_KEY", "x")

	model := func(fields string) string {
		return "models:\n  - name: m1\n    provider: anthropic\n    upstream_model: claude\n    base_url: https://example.com\n" + fields
	}
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"no models", "server:\n  listen: \":0\"\n", "at least one model"},
		{"missing name", "models:\n  - provider: anthropic\n    upstream_model: x\n    base_url: https://e.com\n", "name is required"},
		{"bad provider", "models:\n  - name: m1\n    provider: gemini\n    upstream_model: x\n    base_url: https://e.com\n", `provider "gemini"`},
		{"missing upstream_model", "models:\n  - name: m1\n    provider: openai\n    base_url: https://e.com\n", "upstream_model is required"},
		{"missing base_url", "models:\n  - name: m1\n    provider: openai\n    upstream_model: x\n", "base_url is required"},
		{"relative base_url", "models:\n  - name: m1\n    provider: openai\n    upstream_model: x\n    base_url: example.com/v1\n", "absolute http(s)"},
		{"duplicate name", model("") + "  - name: m1\n    provider: openai\n    upstream_model: x\n    base_url: https://e.com\n", "collides"},
		{"alias collides with name", model("    aliases: [m2]\n") + "  - name: m2\n    provider: openai\n    upstream_model: x\n    base_url: https://e.com\n", "collides"},
		{"unknown fallback", model("    fallback: [ghost]\n"), `fallback "ghost" does not match`},
		{"self fallback", model("    fallback: [m1]\n"), "must not reference itself"},
		{"missing env", model("    api_key_env: LLM_GATEWAY_TEST_UNSET_ENV\n"), "is not set in the environment"},
		{"negative pricing", model("    pricing: { input_usd_per_mtok: -1, output_usd_per_mtok: 0 }\n"), "pricing must be >= 0"},
		{"unknown field", "serverz: {}\n" + model(""), "not found"},
		{"bad duration", "server:\n  request_timeout: fast\n" + model(""), "invalid duration"},
		{"negative duration", "server:\n  request_timeout: -5s\n" + model(""), "must not be negative"},
		{"bad log level", "server:\n  log_level: loud\n" + model(""), "log_level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestDisabledModelSkipsEnvCheck(t *testing.T) {
	yaml := `
models:
  - name: m1
    provider: openai
    upstream_model: x
    base_url: https://e.com
  - name: off
    provider: openai
    upstream_model: y
    base_url: https://e.com
    api_key_env: LLM_GATEWAY_TEST_UNSET_ENV
    enabled: false
`
	cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("disabled model must not require its env var: %v", err)
	}
	m, _ := cfg.ModelByName("off")
	if m.IsEnabled() {
		t.Error("enabled: false not honored")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("OPENROUTER_API_KEY", "test-key")

	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("config.example.yaml must load: %v", err)
	}
	if len(cfg.Models) < 3 {
		t.Errorf("example should register several models, got %d", len(cfg.Models))
	}
	if _, ok := cfg.ModelByName("sonnet"); !ok {
		t.Error("example must define model \"sonnet\"")
	}
}

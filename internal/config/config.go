// Package config loads and validates the gateway's YAML configuration.
// Validation is strict and fails fast at startup: unknown fields, dangling
// fallback references, and missing upstream key env vars are all errors.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider types accepted in model entries.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

// Config is the root configuration.
type Config struct {
	Server     Server     `yaml:"server"`
	Database   Database   `yaml:"database"`
	Cache      Cache      `yaml:"cache"`
	RateLimits RateLimits `yaml:"rate_limits"`
	Models     []Model    `yaml:"models"`
}

// Server holds HTTP server and request-handling settings.
type Server struct {
	Listen string `yaml:"listen"`
	// RequestTimeout caps a whole non-streaming request, including fallbacks.
	RequestTimeout Duration `yaml:"request_timeout"`
	// UpstreamTimeout caps one upstream attempt (full round trip when not
	// streaming; time to first response when streaming).
	UpstreamTimeout  Duration `yaml:"upstream_timeout"`
	ShutdownGrace    Duration `yaml:"shutdown_grace"`
	LogLevel         string   `yaml:"log_level"`
	LogBodies        bool     `yaml:"log_bodies"`
	DefaultMaxTokens int      `yaml:"default_max_tokens"`
}

// Database holds SQLite settings.
type Database struct {
	Path string `yaml:"path"`
}

// Cache holds response-cache settings.
type Cache struct {
	TTL Duration `yaml:"ttl"`
}

// RateLimits holds the per-key defaults, overridable per key in the store.
// Zero means unlimited.
type RateLimits struct {
	DefaultRPM int `yaml:"default_rpm"`
	DefaultTPM int `yaml:"default_tpm"`
}

// Pricing is USD per million tokens.
type Pricing struct {
	InputPerMTok  float64 `yaml:"input_usd_per_mtok"`
	OutputPerMTok float64 `yaml:"output_usd_per_mtok"`
}

// Cost returns the USD cost of a request at this pricing.
func (p Pricing) Cost(inputTokens, outputTokens int) float64 {
	return float64(inputTokens)*p.InputPerMTok/1e6 + float64(outputTokens)*p.OutputPerMTok/1e6
}

// Model is one entry in the model registry.
type Model struct {
	Name          string   `yaml:"name"`
	Provider      string   `yaml:"provider"`
	UpstreamModel string   `yaml:"upstream_model"`
	BaseURL       string   `yaml:"base_url"`
	APIKeyEnv     string   `yaml:"api_key_env"`
	Pricing       Pricing  `yaml:"pricing"`
	Enabled       *bool    `yaml:"enabled"`
	Aliases       []string `yaml:"aliases"`
	Fallback      []string `yaml:"fallback"`
}

// IsEnabled reports whether the entry is enabled (default true).
func (m *Model) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// APIKey resolves the upstream key from the configured env var. Empty
// APIKeyEnv (local upstreams like Ollama) yields an empty key.
func (m *Model) APIKey() string {
	if m.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(m.APIKeyEnv)
}

// Load reads, defaults, and validates the configuration file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	cfg, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes and validates configuration from r.
func Parse(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.RequestTimeout == 0 {
		c.Server.RequestTimeout = Duration(120 * time.Second)
	}
	if c.Server.UpstreamTimeout == 0 {
		c.Server.UpstreamTimeout = Duration(60 * time.Second)
	}
	if c.Server.ShutdownGrace == 0 {
		c.Server.ShutdownGrace = Duration(10 * time.Second)
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = "info"
	}
	if c.Server.DefaultMaxTokens == 0 {
		c.Server.DefaultMaxTokens = 1024
	}
	if c.Database.Path == "" {
		c.Database.Path = "data/gateway.db"
	}
	if c.Cache.TTL == 0 {
		c.Cache.TTL = Duration(5 * time.Minute)
	}
}

// Validate checks the whole configuration and returns every problem found.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	switch c.Server.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		fail("server.log_level: %q is not one of debug|info|warn|error", c.Server.LogLevel)
	}
	if c.Server.DefaultMaxTokens < 1 {
		fail("server.default_max_tokens: must be >= 1, got %d", c.Server.DefaultMaxTokens)
	}
	if c.RateLimits.DefaultRPM < 0 || c.RateLimits.DefaultTPM < 0 {
		fail("rate_limits: default_rpm and default_tpm must be >= 0")
	}
	if len(c.Models) == 0 {
		fail("models: at least one model entry is required")
	}

	names := map[string]string{} // name/alias -> where it was defined
	for i, m := range c.Models {
		at := fmt.Sprintf("models[%d] (%q)", i, m.Name)
		if m.Name == "" {
			fail("%s: name is required", at)
			continue
		}
		if prev, dup := names[m.Name]; dup {
			fail("%s: name collides with %s", at, prev)
		}
		names[m.Name] = at
		for _, a := range m.Aliases {
			if prev, dup := names[a]; dup {
				fail("%s: alias %q collides with %s", at, a, prev)
			}
			names[a] = at + " alias"
		}

		switch m.Provider {
		case ProviderAnthropic, ProviderOpenAI:
		default:
			fail("%s: provider %q is not one of %s|%s", at, m.Provider, ProviderAnthropic, ProviderOpenAI)
		}
		if m.UpstreamModel == "" {
			fail("%s: upstream_model is required", at)
		}
		if m.BaseURL == "" {
			fail("%s: base_url is required", at)
		} else if u, err := url.Parse(m.BaseURL); err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			fail("%s: base_url %q must be an absolute http(s) URL", at, m.BaseURL)
		}
		if m.Pricing.InputPerMTok < 0 || m.Pricing.OutputPerMTok < 0 {
			fail("%s: pricing must be >= 0 USD per Mtok", at)
		}
		if m.IsEnabled() && m.APIKeyEnv != "" && os.Getenv(m.APIKeyEnv) == "" {
			fail("%s: api_key_env %s is not set in the environment (required because the model is enabled)", at, m.APIKeyEnv)
		}
	}

	// Fallback references are validated after the full name set is known.
	for i, m := range c.Models {
		at := fmt.Sprintf("models[%d] (%q)", i, m.Name)
		seen := map[string]bool{}
		for _, fb := range m.Fallback {
			if fb == m.Name {
				fail("%s: fallback must not reference itself", at)
				continue
			}
			if seen[fb] {
				fail("%s: fallback %q listed twice", at, fb)
				continue
			}
			seen[fb] = true
			if !c.hasModelName(fb) {
				fail("%s: fallback %q does not match any model name", at, fb)
			}
		}
	}

	return errors.Join(errs...)
}

// hasModelName reports whether name matches a primary model name (aliases are
// not valid fallback targets: chains are defined between concrete entries).
func (c *Config) hasModelName(name string) bool {
	for _, m := range c.Models {
		if m.Name == name {
			return true
		}
	}
	return false
}

// ModelByName resolves a gateway model name or alias to its entry.
func (c *Config) ModelByName(name string) (*Model, bool) {
	for i := range c.Models {
		if c.Models[i].Name == name {
			return &c.Models[i], true
		}
		for _, a := range c.Models[i].Aliases {
			if a == name {
				return &c.Models[i], true
			}
		}
	}
	return nil, false
}

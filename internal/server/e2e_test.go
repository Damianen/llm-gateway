package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Damianen/llm-gateway/internal/auth"
	"github.com/Damianen/llm-gateway/internal/config"
	"github.com/Damianen/llm-gateway/internal/router"
	"github.com/Damianen/llm-gateway/internal/store"
	"github.com/Damianen/llm-gateway/internal/upstreamfake"
)

// e2eEnv is a full gateway stack over fake upstreams: real HTTP server, real
// SQLite store, real router.
type e2eEnv struct {
	srv    *Server
	store  *store.Store
	http   *httptest.Server
	logBuf *bytes.Buffer
	ant    *upstreamfake.Fake
	oai    *upstreamfake.Fake
	key    string // plaintext virtual key
	keyID  int64
}

const e2eAdminToken = "e2e-admin-token"

func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	ant := upstreamfake.NewAnthropic()
	oai := upstreamfake.NewOpenAI()
	antTS := httptest.NewServer(ant)
	oaiTS := httptest.NewServer(oai)
	t.Cleanup(antTS.Close)
	t.Cleanup(oaiTS.Close)

	cfg := &config.Config{
		Server: config.Server{
			RequestTimeout:   config.Duration(5 * time.Second),
			UpstreamTimeout:  config.Duration(2 * time.Second),
			ShutdownGrace:    config.Duration(time.Second),
			LogLevel:         "info",
			DefaultMaxTokens: 1024,
		},
		Cache: config.Cache{TTL: config.Duration(5 * time.Minute)},
		Models: []config.Model{
			{
				Name: "sonnet", Provider: config.ProviderAnthropic, UpstreamModel: "claude-sonnet-4-6",
				BaseURL: antTS.URL, Pricing: config.Pricing{InputPerMTok: 3, OutputPerMTok: 15},
				Aliases: []string{"claude-sonnet"}, Fallback: []string{"sonnet-or"},
			},
			{
				Name: "sonnet-or", Provider: config.ProviderOpenAI, UpstreamModel: "anthropic/claude-sonnet",
				BaseURL: oaiTS.URL + "/v1", Pricing: config.Pricing{InputPerMTok: 3, OutputPerMTok: 15},
			},
			{
				Name: "fast", Provider: config.ProviderOpenAI, UpstreamModel: "gpt-fast",
				BaseURL: oaiTS.URL + "/v1", Pricing: config.Pricing{InputPerMTok: 1, OutputPerMTok: 5},
			},
		},
	}
	rt, err := router.New(cfg, nil)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "gw.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// A fixed clock keeps stream goldens (created timestamps) deterministic.
	fixedNow := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	srv := New(Options{
		Config: cfg, Logger: logger, Store: st, Router: rt,
		AdminToken: e2eAdminToken, Version: "e2e",
		Clock: func() time.Time { return fixedNow },
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	p, err := st.CreateProject(ctx, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	plaintext, hash, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	k, err := st.InsertKey(ctx, hash, "e2e", p.ID, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	return &e2eEnv{srv: srv, store: st, http: ts, logBuf: &logBuf, ant: ant, oai: oai, key: plaintext, keyID: k.ID}
}

// post sends an authenticated JSON request and returns status, parsed body,
// and the raw body.
func (e *e2eEnv) post(t *testing.T, path string, body map[string]any, headers map[string]string) (int, map[string]any, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, e.http.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.key)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rawResp, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if len(rawResp) > 0 {
		if err := json.Unmarshal(rawResp, &parsed); err != nil {
			t.Fatalf("response is not JSON (%v): %s", err, rawResp)
		}
	}
	return resp.StatusCode, parsed, rawResp
}

func (e *e2eEnv) lastRow(t *testing.T) *store.RequestLog {
	t.Helper()
	row, err := e.store.LastRequest(context.Background())
	if err != nil {
		t.Fatalf("LastRequest: %v", err)
	}
	return row
}

func costCloseTo(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestE2ENonStreamingMatrix(t *testing.T) {
	cases := []struct {
		name         string
		dialect      string
		model        string
		wantProvider string
		wantText     string
		wantUpstream string
		wantCost     float64 // 25 input, 7 output tokens at the entry's pricing
	}{
		{"openai_dialect_to_anthropic_upstream", dialectOpenAI, "sonnet", "anthropic", "Hello from fake anthropic.", "claude-sonnet-4-6", 25*3.0/1e6 + 7*15.0/1e6},
		{"openai_dialect_to_openai_upstream", dialectOpenAI, "fast", "openai", "Hello from fake openai.", "gpt-fast", 25*1.0/1e6 + 7*5.0/1e6},
		{"anthropic_dialect_to_anthropic_upstream", dialectAnthropic, "sonnet", "anthropic", "Hello from fake anthropic.", "claude-sonnet-4-6", 25*3.0/1e6 + 7*15.0/1e6},
		{"anthropic_dialect_to_openai_upstream", dialectAnthropic, "fast", "openai", "Hello from fake openai.", "gpt-fast", 25*1.0/1e6 + 7*5.0/1e6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newE2EEnv(t)
			var status int
			var body map[string]any
			if tc.dialect == dialectOpenAI {
				status, body, _ = e.post(t, "/v1/chat/completions", map[string]any{
					"model":    tc.model,
					"messages": []map[string]any{{"role": "user", "content": "hi"}},
				}, nil)
				if status != http.StatusOK {
					t.Fatalf("status = %d body=%v", status, body)
				}
				choices := body["choices"].([]any)
				msg := choices[0].(map[string]any)["message"].(map[string]any)
				if msg["content"] != tc.wantText {
					t.Errorf("content = %v", msg["content"])
				}
				if choices[0].(map[string]any)["finish_reason"] != "stop" {
					t.Errorf("finish_reason = %v", choices[0].(map[string]any)["finish_reason"])
				}
				usage := body["usage"].(map[string]any)
				if usage["prompt_tokens"].(float64) != 25 || usage["completion_tokens"].(float64) != 7 || usage["total_tokens"].(float64) != 32 {
					t.Errorf("usage = %v", usage)
				}
				if body["model"] != tc.model {
					t.Errorf("echoed model = %v, want %v", body["model"], tc.model)
				}
			} else {
				status, body, _ = e.post(t, "/v1/messages", map[string]any{
					"model":      tc.model,
					"max_tokens": 100,
					"messages":   []map[string]any{{"role": "user", "content": "hi"}},
				}, nil)
				if status != http.StatusOK {
					t.Fatalf("status = %d body=%v", status, body)
				}
				if body["type"] != "message" || body["role"] != "assistant" {
					t.Errorf("envelope = %v/%v", body["type"], body["role"])
				}
				content := body["content"].([]any)
				if content[0].(map[string]any)["text"] != tc.wantText {
					t.Errorf("content = %v", content)
				}
				if body["stop_reason"] != "end_turn" {
					t.Errorf("stop_reason = %v", body["stop_reason"])
				}
				usage := body["usage"].(map[string]any)
				if usage["input_tokens"].(float64) != 25 || usage["output_tokens"].(float64) != 7 {
					t.Errorf("usage = %v", usage)
				}
				if body["model"] != tc.model {
					t.Errorf("echoed model = %v", body["model"])
				}
			}

			row := e.lastRow(t)
			wantEndpoint := "chat_completions"
			if tc.dialect == dialectAnthropic {
				wantEndpoint = "messages"
			}
			if row.Model != tc.model || row.Provider != tc.wantProvider || row.UpstreamModel != tc.wantUpstream {
				t.Errorf("row model/provider/upstream = %q/%q/%q", row.Model, row.Provider, row.UpstreamModel)
			}
			if row.Endpoint != wantEndpoint || row.Status != 200 || row.Stream || row.CacheHit || row.FallbackUsed {
				t.Errorf("row = %+v", row)
			}
			if row.InputTokens != 25 || row.OutputTokens != 7 {
				t.Errorf("row tokens = %d/%d", row.InputTokens, row.OutputTokens)
			}
			if !costCloseTo(row.CostUSD, tc.wantCost) {
				t.Errorf("row cost = %v, want %v", row.CostUSD, tc.wantCost)
			}
			if row.KeyID != e.keyID {
				t.Errorf("row key = %d, want %d", row.KeyID, e.keyID)
			}
		})
	}
}

const antToolUseResponse = `{
	"id":"msg_tool","type":"message","role":"assistant","model":"claude-sonnet-4-6",
	"content":[
		{"type":"text","text":"Checking."},
		{"type":"tool_use","id":"toolu_09","name":"get_weather","input":{"location":"Amsterdam"}}
	],
	"stop_reason":"tool_use","stop_sequence":null,
	"usage":{"input_tokens":100,"output_tokens":25}
}`

const oaiToolCallsResponse = `{
	"id":"chatcmpl-tool","object":"chat.completion","model":"gpt-fast",
	"choices":[{"index":0,"message":{
		"role":"assistant","content":null,
		"tool_calls":[{"id":"call_7","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Amsterdam\"}"}}]},
		"finish_reason":"tool_calls"}],
	"usage":{"prompt_tokens":90,"completion_tokens":20,"total_tokens":110}
}`

// OpenAI-dialect client <-> model served by Anthropic: the full tool loop.
func TestE2EToolRoundTripOpenAIDialectToAnthropic(t *testing.T) {
	e := newE2EEnv(t)
	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "Get weather",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"location": map[string]any{"type": "string"}}},
		},
	}}

	// Turn 1: the model asks for a tool call.
	e.ant.Enqueue(upstreamfake.Step{Body: antToolUseResponse})
	status, body, _ := e.post(t, "/v1/chat/completions", map[string]any{
		"model":    "sonnet",
		"messages": []map[string]any{{"role": "user", "content": "Weather in Amsterdam?"}},
		"tools":    tools,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	choice := body["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	toolCalls := choice["message"].(map[string]any)["tool_calls"].([]any)
	tc0 := toolCalls[0].(map[string]any)
	if tc0["id"] != "toolu_09" || tc0["function"].(map[string]any)["name"] != "get_weather" {
		t.Errorf("tool call = %v", tc0)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc0["function"].(map[string]any)["arguments"].(string)), &args); err != nil || args["location"] != "Amsterdam" {
		t.Errorf("arguments = %v (%v)", tc0["function"], err)
	}
	// The anthropic upstream must have received the tool definitions.
	if !strings.Contains(string(e.ant.LastRequest().Body), `"input_schema"`) {
		t.Errorf("anthropic wire missing tool definitions: %s", e.ant.LastRequest().Body)
	}

	// Turn 2: the client returns the tool result (OpenAI shapes).
	status, body, _ = e.post(t, "/v1/chat/completions", map[string]any{
		"model": "sonnet",
		"messages": []map[string]any{
			{"role": "user", "content": "Weather in Amsterdam?"},
			{"role": "assistant", "tool_calls": []map[string]any{{
				"id": "toolu_09", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": `{"location":"Amsterdam"}`},
			}}},
			{"role": "tool", "tool_call_id": "toolu_09", "content": "17C, cloudy"},
		},
		"tools": tools,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("turn 2 status = %d body=%v", status, body)
	}

	// Assert the Anthropic wire translation of the OpenAI-shaped history.
	var wire struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   string          `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(e.ant.LastRequest().Body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 3 {
		t.Fatalf("anthropic wire messages = %+v", wire.Messages)
	}
	asst := wire.Messages[1]
	if asst.Role != "assistant" || asst.Content[0].Type != "tool_use" || asst.Content[0].ID != "toolu_09" ||
		string(asst.Content[0].Input) != `{"location":"Amsterdam"}` {
		t.Errorf("assistant wire turn = %+v", asst)
	}
	result := wire.Messages[2]
	if result.Role != "user" || result.Content[0].Type != "tool_result" ||
		result.Content[0].ToolUseID != "toolu_09" || result.Content[0].Content != "17C, cloudy" {
		t.Errorf("tool_result wire turn = %+v", result)
	}
}

// Anthropic-dialect client <-> model served by an OpenAI-compatible upstream.
func TestE2EToolRoundTripAnthropicDialectToOpenAI(t *testing.T) {
	e := newE2EEnv(t)
	tools := []map[string]any{{
		"name":         "get_weather",
		"description":  "Get weather",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{"location": map[string]any{"type": "string"}}},
	}}

	// Turn 1: the model asks for a tool call.
	e.oai.Enqueue(upstreamfake.Step{Body: oaiToolCallsResponse})
	status, body, _ := e.post(t, "/v1/messages", map[string]any{
		"model":      "fast",
		"max_tokens": 200,
		"messages":   []map[string]any{{"role": "user", "content": "Weather in Amsterdam?"}},
		"tools":      tools,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", body["stop_reason"])
	}
	content := body["content"].([]any)
	tu := content[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_7" || tu["name"] != "get_weather" {
		t.Errorf("tool_use block = %v", tu)
	}
	if input, ok := tu["input"].(map[string]any); !ok || input["location"] != "Amsterdam" {
		t.Errorf("tool_use input = %v (must be an object, not a string)", tu["input"])
	}

	// Turn 2: the client returns the tool result (Anthropic shapes).
	status, body, _ = e.post(t, "/v1/messages", map[string]any{
		"model":      "fast",
		"max_tokens": 200,
		"messages": []map[string]any{
			{"role": "user", "content": "Weather in Amsterdam?"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "call_7", "name": "get_weather", "input": map[string]any{"location": "Amsterdam"}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "call_7", "content": "17C, cloudy"},
			}},
		},
		"tools": tools,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("turn 2 status = %d body=%v", status, body)
	}

	// Assert the OpenAI wire translation of the Anthropic-shaped history.
	var wire struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []any `json:"tools"`
	}
	if err := json.Unmarshal(e.oai.LastRequest().Body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 3 {
		t.Fatalf("openai wire messages = %+v", wire.Messages)
	}
	asst := wire.Messages[1]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_7" ||
		asst.ToolCalls[0].Function.Arguments != `{"location":"Amsterdam"}` {
		t.Errorf("assistant wire turn = %+v", asst)
	}
	toolMsg := wire.Messages[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_7" || toolMsg.Content != "17C, cloudy" {
		t.Errorf("tool wire turn = %+v", toolMsg)
	}
	if len(wire.Tools) != 1 {
		t.Errorf("tools = %+v", wire.Tools)
	}
}

func TestE2EFallbackAnnotatesRow(t *testing.T) {
	e := newE2EEnv(t)
	e.ant.Enqueue(upstreamfake.Step{Status: 429})

	status, body, _ := e.post(t, "/v1/chat/completions", map[string]any{
		"model":    "sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	msg := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello from fake openai." {
		t.Errorf("content = %v (should be served by fallback)", msg["content"])
	}
	row := e.lastRow(t)
	if !row.FallbackUsed || row.Provider != "openai" || row.UpstreamModel != "anthropic/claude-sonnet" || row.Model != "sonnet" {
		t.Errorf("row = %+v", row)
	}
}

func TestE2EErrorShapes(t *testing.T) {
	e := newE2EEnv(t)

	t.Run("unknown model openai dialect", func(t *testing.T) {
		status, body, _ := e.post(t, "/v1/chat/completions", map[string]any{
			"model": "ghost", "messages": []map[string]any{{"role": "user", "content": "hi"}},
		}, nil)
		errObj := body["error"].(map[string]any)
		if status != 404 || errObj["code"] != "model_not_found" {
			t.Errorf("status=%d body=%v", status, body)
		}
	})

	t.Run("unknown model anthropic dialect", func(t *testing.T) {
		status, body, _ := e.post(t, "/v1/messages", map[string]any{
			"model": "ghost", "max_tokens": 10, "messages": []map[string]any{{"role": "user", "content": "hi"}},
		}, nil)
		if status != 404 || body["type"] != "error" || body["error"].(map[string]any)["type"] != "not_found_error" {
			t.Errorf("status=%d body=%v", status, body)
		}
	})

	t.Run("upstream 400 passes through message", func(t *testing.T) {
		e.oai.Enqueue(upstreamfake.Step{Status: 400, Body: `{"error":{"message":"context length exceeded","type":"invalid_request_error"}}`})
		status, body, _ := e.post(t, "/v1/chat/completions", map[string]any{
			"model": "fast", "messages": []map[string]any{{"role": "user", "content": "hi"}},
		}, nil)
		if status != 400 || !strings.Contains(body["error"].(map[string]any)["message"].(string), "context length exceeded") {
			t.Errorf("status=%d body=%v", status, body)
		}
	})

	t.Run("upstream 500 without fallback becomes 502 and is recorded", func(t *testing.T) {
		e.oai.Enqueue(upstreamfake.Step{Status: 500})
		status, _, _ := e.post(t, "/v1/chat/completions", map[string]any{
			"model": "fast", "messages": []map[string]any{{"role": "user", "content": "hi"}},
		}, nil)
		if status != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", status)
		}
		row := e.lastRow(t)
		if row.Status != 502 || row.InputTokens != 0 || row.CostUSD != 0 {
			t.Errorf("row = %+v", row)
		}
	})

	t.Run("upstream auth failure is masked as 502", func(t *testing.T) {
		e.oai.Enqueue(upstreamfake.Step{Status: 401})
		status, body, raw := e.post(t, "/v1/chat/completions", map[string]any{
			"model": "fast", "messages": []map[string]any{{"role": "user", "content": "hi"}},
		}, nil)
		if status != http.StatusBadGateway {
			t.Errorf("status = %d, want 502 (upstream auth is a gateway config problem)", status)
		}
		if strings.Contains(string(raw), "fake openai error") {
			t.Errorf("upstream auth error body leaked: %v", body)
		}
	})

	t.Run("n greater than one rejected", func(t *testing.T) {
		status, _, _ := e.post(t, "/v1/chat/completions", map[string]any{
			"model": "fast", "n": 2, "messages": []map[string]any{{"role": "user", "content": "hi"}},
		}, nil)
		if status != 400 {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("multimodal content rejected", func(t *testing.T) {
		status, _, _ := e.post(t, "/v1/chat/completions", map[string]any{
			"model": "fast",
			"messages": []map[string]any{{"role": "user", "content": []map[string]any{
				{"type": "image_url", "image_url": map[string]any{"url": "http://x/img.png"}},
			}}},
		}, nil)
		if status != 400 {
			t.Errorf("status = %d, want 400", status)
		}
	})
}

func TestE2EAliasResolution(t *testing.T) {
	e := newE2EEnv(t)
	status, body, _ := e.post(t, "/v1/chat/completions", map[string]any{
		"model":    "claude-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if body["model"] != "claude-sonnet" {
		t.Errorf("echoed model = %v, want the alias the client used", body["model"])
	}
	if row := e.lastRow(t); row.Model != "sonnet" {
		t.Errorf("row model = %q, want resolved primary name", row.Model)
	}
}

func TestE2EDefaultMaxTokensApplied(t *testing.T) {
	e := newE2EEnv(t)
	status, _, _ := e.post(t, "/v1/messages", map[string]any{
		"model":    "sonnet", // no max_tokens: gateway must default it for Anthropic upstreams
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	var wire struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(e.ant.LastRequest().Body, &wire); err != nil || wire.MaxTokens != 1024 {
		t.Errorf("anthropic wire max_tokens = %d (%v), want default 1024", wire.MaxTokens, err)
	}
}

func TestE2EModelsEndpoint(t *testing.T) {
	e := newE2EEnv(t)
	req, _ := http.NewRequest(http.MethodGet, e.http.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+e.key)
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Object != "list" || len(body.Data) != 3 ||
		body.Data[0].ID != "sonnet" || body.Data[1].ID != "sonnet-or" || body.Data[2].ID != "fast" {
		t.Errorf("models = %+v", body)
	}

	// Unauthenticated access is refused.
	unauth, err := e.http.Client().Get(e.http.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d", unauth.StatusCode)
	}
}

func TestE2ENoSecretsInLogs(t *testing.T) {
	e := newE2EEnv(t)
	status, _, _ := e.post(t, "/v1/chat/completions", map[string]any{
		"model": "fast", "messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, nil)
	if status != http.StatusOK {
		t.Fatal("request failed")
	}
	logs := e.logBuf.String()
	if strings.Contains(logs, e.key) {
		t.Error("virtual key leaked into logs")
	}
	if strings.Contains(logs, e2eAdminToken) {
		t.Error("admin token leaked into logs")
	}
}

package openaicompat

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Damianen/llm-gateway/internal/provider"
	"github.com/Damianen/llm-gateway/internal/testutil"
	"github.com/Damianen/llm-gateway/internal/upstreamfake"
)

func f64(v float64) *float64 { return &v }

func newTestClient(t *testing.T, fake *upstreamfake.Fake, apiKey string) *Client {
	t.Helper()
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	return New(ts.URL+"/v1", apiKey, ts.Client(), 5*time.Second)
}

func TestBuildRequestGolden(t *testing.T) {
	weatherSchema := json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}`)
	cases := []struct {
		name string
		req  *provider.Request
	}{
		{
			name: "plain",
			req: &provider.Request{
				Model:  "gpt-fast",
				System: "You are terse.",
				Messages: []provider.Message{
					{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("Hello!")}},
				},
				// MaxTokens omitted: chat-completions allows absence, so the
				// field must not appear on the wire.
				Temperature: f64(0.2),
				Stop:        []string{"END"},
			},
		},
		{
			name: "tools",
			req: &provider.Request{
				Model: "gpt-fast",
				Messages: []provider.Message{
					{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("Weather in Amsterdam?")}},
				},
				Tools:      []provider.Tool{{Name: "get_weather", Description: "Get current weather", InputSchema: weatherSchema}},
				ToolChoice: provider.ToolChoice{Mode: provider.ToolChoiceTool, Name: "get_weather"},
				MaxTokens:  512,
			},
		},
		{
			name: "tool_round_trip",
			req: &provider.Request{
				Model: "gpt-fast",
				Messages: []provider.Message{
					{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("Weather in Amsterdam?")}},
					{Role: provider.RoleAssistant, Blocks: []provider.Block{
						provider.TextBlock("Let me check."),
						provider.ToolUseBlock("toolu_01", "get_weather", json.RawMessage(`{"location":"Amsterdam"}`)),
					}},
					{Role: provider.RoleUser, Blocks: []provider.Block{
						provider.ToolResultBlock("toolu_01", "17°C, cloudy", true), // is_error surfaces in-band
						provider.TextBlock("Summarize that."),
					}},
				},
				Tools:     []provider.Tool{{Name: "get_weather", InputSchema: weatherSchema}},
				MaxTokens: 512,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := upstreamfake.NewOpenAI()
			client := newTestClient(t, fake, "test-openai-key")
			if _, err := client.Complete(context.Background(), tc.req); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			wire := fake.LastRequest()
			if wire.Path != "/v1/chat/completions" {
				t.Errorf("path = %q, want /v1/chat/completions", wire.Path)
			}
			testutil.GoldenJSON(t, wire.Body, "testdata/"+tc.name+".golden.json")
		})
	}
}

func TestRequestHeaders(t *testing.T) {
	req := &provider.Request{Model: "m", Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("hi")}}}}

	fake := upstreamfake.NewOpenAI()
	client := newTestClient(t, fake, "test-openai-key")
	if _, err := client.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := fake.LastRequest().Header.Get("Authorization"); got != "Bearer test-openai-key" {
		t.Errorf("Authorization = %q", got)
	}

	// Local upstreams (Ollama) have no key: no Authorization header at all.
	fakeLocal := upstreamfake.NewOpenAI()
	local := newTestClient(t, fakeLocal, "")
	if _, err := local.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := fakeLocal.LastRequest().Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization for keyless upstream = %q, want empty", got)
	}
}

func TestParseResponseWithToolCalls(t *testing.T) {
	fake := upstreamfake.NewOpenAI()
	fake.Enqueue(upstreamfake.Step{Body: `{
		"id":"chatcmpl-42","object":"chat.completion","model":"gpt-fast",
		"choices":[{"index":0,"message":{
			"role":"assistant","content":"Checking.",
			"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Amsterdam\"}"}},
				{"id":"call_2","type":"function","function":{"name":"noop","arguments":""}},
				{"id":"call_3","type":"function","function":{"name":"broken","arguments":"{not json"}}
			]},
			"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":80,"completion_tokens":30,"total_tokens":110}
	}`})
	client := newTestClient(t, fake, "k")

	resp, err := client.Complete(context.Background(), &provider.Request{
		Model:    "gpt-fast",
		Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != provider.FinishToolUse {
		t.Errorf("finish = %q", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 80 || resp.Usage.OutputTokens != 30 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if len(resp.Blocks) != 4 {
		t.Fatalf("blocks = %+v", resp.Blocks)
	}
	if resp.Blocks[0].Type != provider.BlockText || resp.Blocks[0].Text != "Checking." {
		t.Errorf("block 0 = %+v", resp.Blocks[0])
	}
	if string(resp.Blocks[1].Input) != `{"location":"Amsterdam"}` {
		t.Errorf("block 1 input = %s", resp.Blocks[1].Input)
	}
	if string(resp.Blocks[2].Input) != `{}` {
		t.Errorf("empty arguments should normalize to {}, got %s", resp.Blocks[2].Input)
	}
	if !strings.Contains(string(resp.Blocks[3].Input), `"_raw"`) || !json.Valid(resp.Blocks[3].Input) {
		t.Errorf("invalid arguments should be wrapped valid JSON, got %s", resp.Blocks[3].Input)
	}
}

func TestParseResponseWithoutUsage(t *testing.T) {
	fake := upstreamfake.NewOpenAI()
	fake.Enqueue(upstreamfake.Step{Body: `{
		"id":"chatcmpl-43","object":"chat.completion","model":"local",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]
	}`})
	client := newTestClient(t, fake, "")
	resp, err := client.Complete(context.Background(), &provider.Request{
		Model:    "local",
		Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 0 || resp.Usage.OutputTokens != 0 {
		t.Errorf("usage without upstream data = %+v, want zeros (never tokenize locally)", resp.Usage)
	}
}

func TestNoChoicesIsError(t *testing.T) {
	fake := upstreamfake.NewOpenAI()
	fake.Enqueue(upstreamfake.Step{Body: `{"id":"x","object":"chat.completion","model":"m","choices":[]}`})
	client := newTestClient(t, fake, "k")
	_, err := client.Complete(context.Background(), &provider.Request{
		Model:    "m",
		Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("hi")}}},
	})
	if _, ok := provider.AsUpstreamError(err); !ok {
		t.Fatalf("error = %v, want *UpstreamError", err)
	}
}

func TestFinishReasonMapping(t *testing.T) {
	cases := map[string]string{
		"stop":           provider.FinishStop,
		"length":         provider.FinishLength,
		"tool_calls":     provider.FinishToolUse,
		"function_call":  provider.FinishToolUse,
		"content_filter": provider.FinishContentFilter,
		"":               provider.FinishStop,
	}
	for in, want := range cases {
		if got := FinishFromOpenAI(in); got != want {
			t.Errorf("FinishFromOpenAI(%q) = %q, want %q", in, got, want)
		}
	}
	back := map[string]string{
		provider.FinishStop:          "stop",
		provider.FinishLength:        "length",
		provider.FinishToolUse:       "tool_calls",
		provider.FinishContentFilter: "content_filter",
	}
	for in, want := range back {
		if got := OpenAIFromFinish(in); got != want {
			t.Errorf("OpenAIFromFinish(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestErrorClassification(t *testing.T) {
	req := &provider.Request{Model: "m", Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("hi")}}}}
	cases := []struct {
		name      string
		status    int
		body      string
		retryable bool
		message   string
	}{
		{"bad request", 400, `{"error":{"message":"unknown parameter","type":"invalid_request_error"}}`, false, "unknown parameter"},
		{"unauthorized", 401, "", false, ""},
		{"not found", 404, "", false, ""},
		{"rate limited", 429, "", true, ""},
		{"bad gateway", 502, "", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := upstreamfake.NewOpenAI()
			fake.Enqueue(upstreamfake.Step{Status: tc.status, Body: tc.body})
			client := newTestClient(t, fake, "k")
			_, err := client.Complete(context.Background(), req)
			ue, ok := provider.AsUpstreamError(err)
			if !ok {
				t.Fatalf("error = %v, want *UpstreamError", err)
			}
			if ue.StatusCode != tc.status || ue.Retryable() != tc.retryable {
				t.Errorf("status=%d retryable=%v, want %d/%v", ue.StatusCode, ue.Retryable(), tc.status, tc.retryable)
			}
			if tc.message != "" && ue.Message != tc.message {
				t.Errorf("message = %q, want %q", ue.Message, tc.message)
			}
		})
	}
}

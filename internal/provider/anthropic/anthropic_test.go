package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Damianen/llm-gateway/internal/provider"
	"github.com/Damianen/llm-gateway/internal/testutil"
	"github.com/Damianen/llm-gateway/internal/upstreamfake"
)

func f64(v float64) *float64 { return &v }

func newTestClient(t *testing.T, fake *upstreamfake.Fake) *Client {
	t.Helper()
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	return New(ts.URL, "test-anthropic-key", ts.Client(), 1024, 5*time.Second)
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
				Model: "claude-sonnet-4-6",
				Messages: []provider.Message{
					{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("Hello!")}},
				},
				MaxTokens:   256,
				Temperature: f64(0.5),
				TopP:        f64(0.9),
				Stop:        []string{"END"},
			},
		},
		{
			name: "defaults_and_system",
			req: &provider.Request{
				Model:  "claude-sonnet-4-6",
				System: "You are terse.",
				Messages: []provider.Message{
					{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("Hi")}},
				},
				// MaxTokens omitted: the adapter must apply its default (1024)
				// because the Messages API requires max_tokens.
			},
		},
		{
			name: "tools",
			req: &provider.Request{
				Model: "claude-sonnet-4-6",
				Messages: []provider.Message{
					{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("Weather in Amsterdam?")}},
				},
				Tools:      []provider.Tool{{Name: "get_weather", Description: "Get current weather", InputSchema: weatherSchema}},
				ToolChoice: provider.ToolChoice{Mode: provider.ToolChoiceRequired},
				MaxTokens:  512,
			},
		},
		{
			name: "tool_round_trip",
			req: &provider.Request{
				Model: "claude-sonnet-4-6",
				Messages: []provider.Message{
					{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("Weather in Amsterdam?")}},
					{Role: provider.RoleAssistant, Blocks: []provider.Block{
						provider.TextBlock("Let me check."),
						provider.ToolUseBlock("toolu_01", "get_weather", json.RawMessage(`{"location":"Amsterdam"}`)),
					}},
					{Role: provider.RoleUser, Blocks: []provider.Block{
						provider.ToolResultBlock("toolu_01", "17°C, cloudy", false),
					}},
					// Consecutive user turns must merge into one wire message.
					{Role: provider.RoleUser, Blocks: []provider.Block{
						provider.TextBlock("Summarize that."),
						provider.TextBlock(""), // empty text blocks are dropped
					}},
				},
				Tools:     []provider.Tool{{Name: "get_weather", InputSchema: weatherSchema}},
				MaxTokens: 512,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := upstreamfake.NewAnthropic()
			client := newTestClient(t, fake)
			if _, err := client.Complete(context.Background(), tc.req); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			wire := fake.LastRequest()
			if wire.Path != "/v1/messages" {
				t.Errorf("path = %q, want /v1/messages", wire.Path)
			}
			testutil.GoldenJSON(t, wire.Body, "testdata/"+tc.name+".golden.json")
		})
	}
}

func TestRequestHeaders(t *testing.T) {
	fake := upstreamfake.NewAnthropic()
	client := newTestClient(t, fake)
	req := &provider.Request{Model: "m", Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("hi")}}}}
	if _, err := client.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	h := fake.LastRequest().Header
	if got := h.Get("x-api-key"); got != "test-anthropic-key" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := h.Get("anthropic-version"); got != Version {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := h.Get("Authorization"); got != "" {
		t.Errorf("unexpected Authorization header %q", got)
	}
}

func TestParseResponseWithToolUse(t *testing.T) {
	fake := upstreamfake.NewAnthropic()
	fake.Enqueue(upstreamfake.Step{Body: `{
		"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-6",
		"content":[
			{"type":"text","text":"Checking."},
			{"type":"tool_use","id":"toolu_09","name":"get_weather","input":{"location":"Amsterdam"}}
		],
		"stop_reason":"tool_use","stop_sequence":null,
		"usage":{"input_tokens":100,"output_tokens":25}
	}`})
	client := newTestClient(t, fake)

	resp, err := client.Complete(context.Background(), &provider.Request{
		Model:    "claude-sonnet-4-6",
		Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "msg_01" || resp.Model != "claude-sonnet-4-6" {
		t.Errorf("id/model = %q/%q", resp.ID, resp.Model)
	}
	if resp.FinishReason != provider.FinishToolUse {
		t.Errorf("finish = %q, want tool_use", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 100 || resp.Usage.OutputTokens != 25 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if len(resp.Blocks) != 2 {
		t.Fatalf("blocks = %+v", resp.Blocks)
	}
	if resp.Blocks[0].Type != provider.BlockText || resp.Blocks[0].Text != "Checking." {
		t.Errorf("block 0 = %+v", resp.Blocks[0])
	}
	tu := resp.Blocks[1]
	if tu.Type != provider.BlockToolUse || tu.ID != "toolu_09" || tu.Name != "get_weather" ||
		string(tu.Input) != `{"location":"Amsterdam"}` {
		t.Errorf("block 1 = %+v input=%s", tu, tu.Input)
	}
}

func TestStopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"end_turn":      provider.FinishStop,
		"stop_sequence": provider.FinishStop,
		"max_tokens":    provider.FinishLength,
		"tool_use":      provider.FinishToolUse,
		"refusal":       provider.FinishContentFilter,
		"pause_turn":    provider.FinishStop,
	}
	for stop, want := range cases {
		if got := FinishFromStopReason(stop); got != want {
			t.Errorf("FinishFromStopReason(%q) = %q, want %q", stop, got, want)
		}
	}
	back := map[string]string{
		provider.FinishStop:          "end_turn",
		provider.FinishLength:        "max_tokens",
		provider.FinishToolUse:       "tool_use",
		provider.FinishContentFilter: "refusal",
	}
	for finish, want := range back {
		if got := StopReasonFromFinish(finish); got != want {
			t.Errorf("StopReasonFromFinish(%q) = %q, want %q", finish, got, want)
		}
	}
}

func TestErrorClassification(t *testing.T) {
	req := &provider.Request{Model: "m", Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("hi")}}}}

	cases := []struct {
		name      string
		step      upstreamfake.Step
		status    int
		retryable bool
		message   string
	}{
		{"bad request", upstreamfake.Step{Status: 400, Body: `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens is too large"}}`}, 400, false, "max_tokens is too large"},
		{"unauthorized", upstreamfake.Step{Status: 401}, 401, false, ""},
		{"rate limited", upstreamfake.Step{Status: 429}, 429, true, ""},
		{"server error", upstreamfake.Step{Status: 500}, 500, true, ""},
		{"overloaded", upstreamfake.Step{Status: 529}, 529, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := upstreamfake.NewAnthropic()
			fake.Enqueue(tc.step)
			client := newTestClient(t, fake)
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

	t.Run("transport error", func(t *testing.T) {
		fake := upstreamfake.NewAnthropic()
		ts := httptest.NewServer(fake)
		client := New(ts.URL, "k", ts.Client(), 1024, time.Second)
		ts.Close()
		_, err := client.Complete(context.Background(), req)
		ue, ok := provider.AsUpstreamError(err)
		if !ok || ue.StatusCode != 0 || !ue.Retryable() {
			t.Fatalf("transport error = %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		fake := upstreamfake.NewAnthropic()
		fake.Enqueue(upstreamfake.Step{Delay: 500 * time.Millisecond})
		ts := httptest.NewServer(fake)
		t.Cleanup(ts.Close)
		client := New(ts.URL, "k", ts.Client(), 1024, 50*time.Millisecond)
		_, err := client.Complete(context.Background(), req)
		ue, ok := provider.AsUpstreamError(err)
		if !ok || ue.StatusCode != 0 || !ue.Retryable() {
			t.Fatalf("timeout error = %v", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("timeout should unwrap to context.DeadlineExceeded, got %v", err)
		}
	})
}

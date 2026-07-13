package anthropic

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/Damianen/llm-gateway/internal/provider"
	"github.com/Damianen/llm-gateway/internal/upstreamfake"
)

func streamReq() *provider.Request {
	return &provider.Request{
		Model:    "claude-sonnet-4-6",
		Stream:   true,
		Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("hi")}}},
	}
}

// collect drains a stream into events, returning the terminal error (nil for
// a clean io.EOF end).
func collect(t *testing.T, s provider.Stream) ([]provider.StreamEvent, error) {
	t.Helper()
	defer s.Close()
	var out []provider.StreamEvent
	for {
		ev, err := s.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
		out = append(out, *ev)
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestStreamDefaultText(t *testing.T) {
	fake := upstreamfake.NewAnthropic()
	client := newTestClient(t, fake)
	st, err := client.Stream(context.Background(), streamReq())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events, err := collect(t, st)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	want := []provider.StreamEvent{
		{Type: provider.EventStart, ID: "msg_fake_001", Model: "claude-sonnet-4-6", Usage: provider.Usage{InputTokens: 25}},
		{Type: provider.EventTextStart, Index: 0},
		{Type: provider.EventTextDelta, Index: 0, Text: "Hello from "},
		{Type: provider.EventTextDelta, Index: 0, Text: "fake anthropic."},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventFinish, FinishReason: provider.FinishStop, Usage: provider.Usage{InputTokens: 25, OutputTokens: 7}},
		{Type: provider.EventDone},
	}
	assertEvents(t, events, want)
}

func TestStreamToolUse(t *testing.T) {
	fake := upstreamfake.NewAnthropic()
	fake.Enqueue(upstreamfake.Step{SSE: fixture(t, "tool_stream.sse")})
	client := newTestClient(t, fake)
	st, err := client.Stream(context.Background(), streamReq())
	if err != nil {
		t.Fatal(err)
	}
	events, err := collect(t, st)
	if err != nil {
		t.Fatal(err)
	}

	want := []provider.StreamEvent{
		{Type: provider.EventStart, ID: "msg_str_tool", Model: "claude-sonnet-4-6", Usage: provider.Usage{InputTokens: 120}},
		{Type: provider.EventTextStart, Index: 0},
		{Type: provider.EventTextDelta, Index: 0, Text: "Let me check."},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventToolUseStart, Index: 1, ToolID: "toolu_str_1", ToolName: "get_weather"},
		{Type: provider.EventToolInputDelta, Index: 1, Text: `{"location":`},
		{Type: provider.EventToolInputDelta, Index: 1, Text: `"Amsterdam"}`},
		{Type: provider.EventBlockStop, Index: 1},
		{Type: provider.EventFinish, FinishReason: provider.FinishToolUse, Usage: provider.Usage{InputTokens: 120, OutputTokens: 30}},
		{Type: provider.EventDone},
	}
	assertEvents(t, events, want)
}

func TestStreamToolUseEmptyInputSynthesizesBraces(t *testing.T) {
	fake := upstreamfake.NewAnthropic()
	fake.Enqueue(upstreamfake.Step{SSE: fixture(t, "tool_empty_input.sse")})
	client := newTestClient(t, fake)
	st, err := client.Stream(context.Background(), streamReq())
	if err != nil {
		t.Fatal(err)
	}
	events, err := collect(t, st)
	if err != nil {
		t.Fatal(err)
	}

	want := []provider.StreamEvent{
		{Type: provider.EventStart, ID: "msg_str_empty", Model: "claude-sonnet-4-6", Usage: provider.Usage{InputTokens: 40}},
		{Type: provider.EventToolUseStart, Index: 0, ToolID: "toolu_str_2", ToolName: "refresh"},
		{Type: provider.EventToolInputDelta, Index: 0, Text: "{}"},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventFinish, FinishReason: provider.FinishToolUse, Usage: provider.Usage{InputTokens: 40, OutputTokens: 8}},
		{Type: provider.EventDone},
	}
	assertEvents(t, events, want)
}

func TestStreamMidStreamError(t *testing.T) {
	fake := upstreamfake.NewAnthropic()
	fake.Enqueue(upstreamfake.Step{SSE: fixture(t, "error_midstream.sse")})
	client := newTestClient(t, fake)
	st, err := client.Stream(context.Background(), streamReq())
	if err != nil {
		t.Fatal(err)
	}
	events, err := collect(t, st)
	ue, ok := provider.AsUpstreamError(err)
	if !ok || ue.StatusCode != 529 || ue.Message != "Overloaded" {
		t.Fatalf("terminal error = %v", err)
	}
	// The deltas before the error must have been delivered.
	if len(events) != 3 || events[2].Text != "Hel" {
		t.Errorf("events before error = %+v", events)
	}
}

func TestStreamHTTPErrorBeforeEvents(t *testing.T) {
	fake := upstreamfake.NewAnthropic()
	fake.Enqueue(upstreamfake.Step{Status: 429})
	client := newTestClient(t, fake)
	_, err := client.Stream(context.Background(), streamReq())
	ue, ok := provider.AsUpstreamError(err)
	if !ok || ue.StatusCode != 429 || !ue.Retryable() {
		t.Fatalf("error = %v, want retryable 429", err)
	}
}

func assertEvents(t *testing.T, got, want []provider.StreamEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d\ngot: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d:\ngot  %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

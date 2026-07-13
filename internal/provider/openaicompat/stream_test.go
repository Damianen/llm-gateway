package openaicompat

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Damianen/llm-gateway/internal/provider"
	"github.com/Damianen/llm-gateway/internal/upstreamfake"
)

func streamReq() *provider.Request {
	return &provider.Request{
		Model:    "gpt-fast",
		Stream:   true,
		Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("hi")}}},
	}
}

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

func TestStreamDefaultText(t *testing.T) {
	fake := upstreamfake.NewOpenAI()
	client := newTestClient(t, fake, "k")
	st, err := client.Stream(context.Background(), streamReq())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events, err := collect(t, st)
	if err != nil {
		t.Fatal(err)
	}

	want := []provider.StreamEvent{
		{Type: provider.EventStart, ID: "chatcmpl-fake-001", Model: "gpt-fast"},
		{Type: provider.EventTextStart, Index: 0},
		{Type: provider.EventTextDelta, Index: 0, Text: "Hello from "},
		{Type: provider.EventTextDelta, Index: 0, Text: "fake openai."},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventFinish, FinishReason: provider.FinishStop, Usage: provider.Usage{InputTokens: 25, OutputTokens: 7}},
		{Type: provider.EventDone},
	}
	assertEvents(t, events, want)

	// The outbound request must force stream_options.include_usage.
	body := string(fake.LastRequest().Body)
	if want := `"stream_options":{"include_usage":true}`; !strings.Contains(body, want) {
		t.Errorf("outbound stream request missing %s: %s", want, body)
	}
}

func TestStreamToolCalls(t *testing.T) {
	fake := upstreamfake.NewOpenAI()
	fake.Enqueue(upstreamfake.Step{SSE: fixture(t, "tool_stream.sse")})
	client := newTestClient(t, fake, "k")
	st, err := client.Stream(context.Background(), streamReq())
	if err != nil {
		t.Fatal(err)
	}
	events, err := collect(t, st)
	if err != nil {
		t.Fatal(err)
	}

	want := []provider.StreamEvent{
		{Type: provider.EventStart, ID: "chatcmpl-str-tool", Model: "gpt-fast"},
		{Type: provider.EventTextStart, Index: 0},
		{Type: provider.EventTextDelta, Index: 0, Text: "Let me check."},
		{Type: provider.EventToolUseStart, Index: 1, ToolID: "call_str_1", ToolName: "get_weather"},
		{Type: provider.EventToolInputDelta, Index: 1, Text: `{"location":`},
		{Type: provider.EventToolInputDelta, Index: 1, Text: `"Amsterdam"}`},
		{Type: provider.EventToolUseStart, Index: 2, ToolID: "call_str_2", ToolName: "refresh"},
		// Closing sequence: blocks close in open order; the tool call that
		// never received arguments gets synthesized "{}".
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventBlockStop, Index: 1},
		{Type: provider.EventToolInputDelta, Index: 2, Text: "{}"},
		{Type: provider.EventBlockStop, Index: 2},
		{Type: provider.EventFinish, FinishReason: provider.FinishToolUse, Usage: provider.Usage{InputTokens: 90, OutputTokens: 22}},
		{Type: provider.EventDone},
	}
	assertEvents(t, events, want)
}

func TestStreamWithoutUsageChunk(t *testing.T) {
	fake := upstreamfake.NewOpenAI()
	fake.Enqueue(upstreamfake.Step{SSE: fixture(t, "no_usage.sse")})
	client := newTestClient(t, fake, "")
	st, err := client.Stream(context.Background(), streamReq())
	if err != nil {
		t.Fatal(err)
	}
	events, err := collect(t, st)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-2] // finish precedes done
	if last.Type != provider.EventFinish || last.Usage.InputTokens != 0 || last.Usage.OutputTokens != 0 {
		t.Errorf("finish = %+v, want zero usage when the upstream reports none", last)
	}
}

func TestStreamMidStreamError(t *testing.T) {
	fake := upstreamfake.NewOpenAI()
	fake.Enqueue(upstreamfake.Step{SSE: fixture(t, "error_midstream.sse")})
	client := newTestClient(t, fake, "k")
	st, err := client.Stream(context.Background(), streamReq())
	if err != nil {
		t.Fatal(err)
	}
	events, err := collect(t, st)
	ue, ok := provider.AsUpstreamError(err)
	if !ok || ue.Message == "" {
		t.Fatalf("terminal error = %v", err)
	}
	if len(events) != 3 || events[2].Text != "Hel" {
		t.Errorf("events before error = %+v", events)
	}
}

func TestStreamHTTPErrorBeforeEvents(t *testing.T) {
	fake := upstreamfake.NewOpenAI()
	fake.Enqueue(upstreamfake.Step{Status: 500})
	client := newTestClient(t, fake, "k")
	_, err := client.Stream(context.Background(), streamReq())
	ue, ok := provider.AsUpstreamError(err)
	if !ok || ue.StatusCode != 500 || !ue.Retryable() {
		t.Fatalf("error = %v, want retryable 500", err)
	}
}

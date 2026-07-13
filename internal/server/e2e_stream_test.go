package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Damianen/llm-gateway/internal/sse"
	"github.com/Damianen/llm-gateway/internal/testutil"
	"github.com/Damianen/llm-gateway/internal/upstreamfake"
)

type sseEv struct {
	name string
	data string
}

func parseSSE(t *testing.T, raw []byte) []sseEv {
	t.Helper()
	sc := sse.NewScanner(bytes.NewReader(raw))
	var out []sseEv
	for {
		ev, err := sc.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("parse SSE: %v", err)
		}
		out = append(out, sseEv{ev.Name, ev.Data})
	}
}

// goldenSSE renders events one per line with normalized (key-sorted) JSON so
// translated streams can be asserted event-by-event via golden files.
func goldenSSE(t *testing.T, events []sseEv) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range events {
		name := ev.name
		if name == "" {
			name = "-"
		}
		data := ev.data
		var v any
		if json.Unmarshal([]byte(data), &v) == nil {
			nb, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			data = string(nb)
		}
		b.WriteString("event=" + name + " data=" + data + "\n")
	}
	return b.String()
}

// streamPost sends a streaming request and returns status, content type, and
// the full SSE body.
func (e *e2eEnv) streamPost(t *testing.T, path string, body map[string]any) (int, string, []byte) {
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
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rawResp, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), rawResp
}

func adapterFixture(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestE2EStreamTextMatrix(t *testing.T) {
	cases := []struct {
		name     string
		dialect  string
		model    string
		wantCost float64
	}{
		{"openai_dialect_anthropic_upstream", dialectOpenAI, "sonnet", 25*3.0/1e6 + 7*15.0/1e6},
		{"openai_dialect_openai_upstream", dialectOpenAI, "fast", 25*1.0/1e6 + 7*5.0/1e6},
		{"anthropic_dialect_anthropic_upstream", dialectAnthropic, "sonnet", 25*3.0/1e6 + 7*15.0/1e6},
		{"anthropic_dialect_openai_upstream", dialectAnthropic, "fast", 25*1.0/1e6 + 7*5.0/1e6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newE2EEnv(t)
			var status int
			var ctype string
			var raw []byte
			if tc.dialect == dialectOpenAI {
				status, ctype, raw = e.streamPost(t, "/v1/chat/completions", map[string]any{
					"model":          tc.model,
					"stream":         true,
					"stream_options": map[string]any{"include_usage": true},
					"messages":       []map[string]any{{"role": "user", "content": "hi"}},
				})
			} else {
				status, ctype, raw = e.streamPost(t, "/v1/messages", map[string]any{
					"model":      tc.model,
					"max_tokens": 100,
					"stream":     true,
					"messages":   []map[string]any{{"role": "user", "content": "hi"}},
				})
			}
			if status != http.StatusOK || !strings.HasPrefix(ctype, "text/event-stream") {
				t.Fatalf("status=%d content-type=%q body=%s", status, ctype, raw)
			}
			testutil.GoldenText(t, goldenSSE(t, parseSSE(t, raw)), "testdata/stream_"+tc.name+".golden.txt")

			// Streamed requests are costed identically to non-streamed ones.
			row := e.lastRow(t)
			if !row.Stream || row.Status != 200 || row.InputTokens != 25 || row.OutputTokens != 7 {
				t.Errorf("row = %+v", row)
			}
			if !costCloseTo(row.CostUSD, tc.wantCost) {
				t.Errorf("row cost = %v, want %v", row.CostUSD, tc.wantCost)
			}
		})
	}
}

func TestE2EStreamToolCallTranslationToOpenAIDialect(t *testing.T) {
	e := newE2EEnv(t)
	e.ant.Enqueue(upstreamfake.Step{SSE: adapterFixture(t, "../provider/anthropic/testdata/tool_stream.sse")})

	status, _, raw := e.streamPost(t, "/v1/chat/completions", map[string]any{
		"model":          "sonnet",
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       []map[string]any{{"role": "user", "content": "Weather?"}},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, raw)
	}
	testutil.GoldenText(t, goldenSSE(t, parseSSE(t, raw)), "testdata/stream_tool_openai_dialect.golden.txt")

	row := e.lastRow(t)
	if row.InputTokens != 120 || row.OutputTokens != 30 || !row.Stream {
		t.Errorf("row = %+v", row)
	}
}

func TestE2EStreamToolCallTranslationToAnthropicDialect(t *testing.T) {
	e := newE2EEnv(t)
	e.oai.Enqueue(upstreamfake.Step{SSE: adapterFixture(t, "../provider/openaicompat/testdata/tool_stream.sse")})

	status, _, raw := e.streamPost(t, "/v1/messages", map[string]any{
		"model":      "fast",
		"max_tokens": 200,
		"stream":     true,
		"messages":   []map[string]any{{"role": "user", "content": "Weather?"}},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, raw)
	}
	testutil.GoldenText(t, goldenSSE(t, parseSSE(t, raw)), "testdata/stream_tool_anthropic_dialect.golden.txt")

	row := e.lastRow(t)
	if row.InputTokens != 90 || row.OutputTokens != 22 || !row.Stream {
		t.Errorf("row = %+v", row)
	}
}

func TestE2EStreamNoUsageChunkWithoutOptIn(t *testing.T) {
	e := newE2EEnv(t)
	status, _, raw := e.streamPost(t, "/v1/chat/completions", map[string]any{
		"model":    "fast",
		"stream":   true, // no stream_options.include_usage
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if strings.Contains(string(raw), "prompt_tokens") {
		t.Error("usage chunk sent although the client did not opt in")
	}
	// The gateway still costs the request from the forced upstream usage.
	row := e.lastRow(t)
	if row.InputTokens != 25 || row.OutputTokens != 7 || row.CostUSD == 0 {
		t.Errorf("row = %+v", row)
	}
}

func TestE2EStreamMidStreamUpstreamError(t *testing.T) {
	t.Run("openai dialect", func(t *testing.T) {
		e := newE2EEnv(t)
		e.oai.Enqueue(upstreamfake.Step{SSE: adapterFixture(t, "../provider/openaicompat/testdata/error_midstream.sse")})
		status, _, raw := e.streamPost(t, "/v1/chat/completions", map[string]any{
			"model": "fast", "stream": true,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK { // headers were already sent when the error hit
			t.Fatalf("status = %d", status)
		}
		events := parseSSE(t, raw)
		last := events[len(events)-1]
		if !strings.Contains(last.data, `"error"`) || strings.Contains(last.data, "[DONE]") {
			t.Errorf("last event = %+v, want error frame and no [DONE]", last)
		}
		if row := e.lastRow(t); row.Status != 502 {
			t.Errorf("row status = %d, want 502", row.Status)
		}
	})

	t.Run("anthropic dialect", func(t *testing.T) {
		e := newE2EEnv(t)
		e.ant.Enqueue(upstreamfake.Step{SSE: adapterFixture(t, "../provider/anthropic/testdata/error_midstream.sse")})
		status, _, raw := e.streamPost(t, "/v1/messages", map[string]any{
			"model": "sonnet", "max_tokens": 50, "stream": true,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		events := parseSSE(t, raw)
		last := events[len(events)-1]
		if last.name != "error" || !strings.Contains(last.data, "Overloaded") {
			t.Errorf("last event = %+v, want anthropic error event", last)
		}
		if row := e.lastRow(t); row.Status != 502 {
			t.Errorf("row status = %d, want 502", row.Status)
		}
	})
}

func TestE2EStreamFallbackBeforeFirstByte(t *testing.T) {
	e := newE2EEnv(t)
	e.ant.Enqueue(upstreamfake.Step{Status: 529})

	status, _, raw := e.streamPost(t, "/v1/chat/completions", map[string]any{
		"model": "sonnet", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, raw)
	}
	if !strings.Contains(string(raw), "fake openai.") {
		t.Errorf("stream should be served by the openai fallback: %s", raw)
	}
	row := e.lastRow(t)
	if !row.FallbackUsed || row.Provider != "openai" || !row.Stream {
		t.Errorf("row = %+v", row)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestE2EStreamClientDisconnectCancelsUpstream(t *testing.T) {
	e := newE2EEnv(t)
	// The fake writes two SSE chunks, then blocks until its request context
	// is canceled — which only happens if the gateway propagates the client
	// disconnect upstream.
	e.oai.Enqueue(upstreamfake.Step{StallAfter: 2})

	body, _ := json.Marshal(map[string]any{
		"model": "fast", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.http.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first stream read: %v", err)
	}
	cancel()
	resp.Body.Close()

	waitFor(t, "upstream cancellation", e.oai.Aborted)
	waitFor(t, "499 request row", func() bool {
		row, err := e.store.LastRequest(context.Background())
		return err == nil && row.Status == 499 && row.Stream
	})
}

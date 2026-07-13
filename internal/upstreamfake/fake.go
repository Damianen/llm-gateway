// Package upstreamfake provides scriptable fake Anthropic and
// OpenAI-compatible upstreams for tests and the smoke script. Tests never
// touch the real network: they point adapters at an httptest.Server wrapping
// one of these fakes.
package upstreamfake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Kinds of fake upstream.
const (
	KindAnthropic = "anthropic"
	KindOpenAI    = "openai"
)

// Recorded is one captured upstream request.
type Recorded struct {
	Path   string
	Header http.Header
	Body   []byte
}

// Step scripts one response. The zero value serves the dialect's default
// success body (or default stream for streaming requests).
type Step struct {
	Status     int           // 0 means 200
	Body       string        // JSON body; empty = dialect default (or default error body when Status >= 400)
	SSE        string        // SSE payload served when the request has "stream": true
	Delay      time.Duration // wait before responding (aborts if the client disconnects)
	ChunkDelay time.Duration // wait between SSE chunks
	StallAfter int           // >0: after writing this many SSE chunks, block until the client disconnects
}

// Fake is a scriptable upstream. Responses are consumed from the enqueued
// script in order; when the script is empty, defaults are served.
type Fake struct {
	kind string

	mu       sync.Mutex
	requests []Recorded
	script   []Step
	aborted  bool
}

// NewAnthropic returns a fake Anthropic Messages API upstream.
func NewAnthropic() *Fake { return &Fake{kind: KindAnthropic} }

// NewOpenAI returns a fake OpenAI-compatible chat-completions upstream.
func NewOpenAI() *Fake { return &Fake{kind: KindOpenAI} }

// Kind returns which dialect this fake speaks.
func (f *Fake) Kind() string { return f.kind }

// Enqueue appends scripted responses, served one per request.
func (f *Fake) Enqueue(steps ...Step) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, steps...)
}

// Requests returns a copy of all captured requests.
func (f *Fake) Requests() []Recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Recorded, len(f.requests))
	copy(out, f.requests)
	return out
}

// LastRequest returns the most recent captured request, or nil.
func (f *Fake) LastRequest() *Recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	r := f.requests[len(f.requests)-1]
	return &r
}

// Aborted reports whether a scripted response observed a client disconnect.
func (f *Fake) Aborted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aborted
}

func (f *Fake) setAborted() {
	f.mu.Lock()
	f.aborted = true
	f.mu.Unlock()
}

// ServeHTTP implements http.Handler.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.requests = append(f.requests, Recorded{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	var step Step
	if len(f.script) > 0 {
		step = f.script[0]
		f.script = f.script[1:]
	}
	f.mu.Unlock()

	if step.Delay > 0 {
		select {
		case <-time.After(step.Delay):
		case <-r.Context().Done():
			f.setAborted()
			return
		}
	}

	var parsed struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &parsed)

	status := step.Status
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		errBody := step.Body
		if errBody == "" {
			errBody = f.defaultErrorBody(status)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, errBody)
		return
	}

	if parsed.Stream {
		sse := step.SSE
		if sse == "" {
			sse = f.DefaultSSE(parsed.Model)
		}
		f.writeSSE(w, r, sse, step)
		return
	}

	out := step.Body
	if out == "" {
		out = f.defaultJSON(parsed.Model)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, out)
}

func (f *Fake) writeSSE(w http.ResponseWriter, r *http.Request, sse string, step Step) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)

	chunks := strings.Split(strings.TrimRight(sse, "\n"), "\n\n")
	for i, chunk := range chunks {
		if step.StallAfter > 0 && i >= step.StallAfter {
			<-r.Context().Done()
			f.setAborted()
			return
		}
		if step.ChunkDelay > 0 {
			select {
			case <-time.After(step.ChunkDelay):
			case <-r.Context().Done():
				f.setAborted()
				return
			}
		}
		io.WriteString(w, chunk+"\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
}

func (f *Fake) defaultJSON(model string) string {
	if f.kind == KindAnthropic {
		return fmt.Sprintf(`{"id":"msg_fake_001","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"Hello from fake anthropic."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":25,"output_tokens":7}}`, model)
	}
	return fmt.Sprintf(`{"id":"chatcmpl-fake-001","object":"chat.completion","created":1700000000,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"Hello from fake openai."},"finish_reason":"stop"}],"usage":{"prompt_tokens":25,"completion_tokens":7,"total_tokens":32}}`, model)
}

func (f *Fake) defaultErrorBody(status int) string {
	if f.kind == KindAnthropic {
		errType := "api_error"
		switch status {
		case http.StatusBadRequest:
			errType = "invalid_request_error"
		case http.StatusUnauthorized:
			errType = "authentication_error"
		case http.StatusTooManyRequests:
			errType = "rate_limit_error"
		case 529:
			errType = "overloaded_error"
		}
		return fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":"fake anthropic error (status %d)"}}`, errType, status)
	}
	return fmt.Sprintf(`{"error":{"message":"fake openai error (status %d)","type":"api_error","param":null,"code":null}}`, status)
}

// DefaultSSE returns the canned happy-path stream for this dialect.
func (f *Fake) DefaultSSE(model string) string {
	if f.kind == KindAnthropic {
		return fmt.Sprintf(`event: message_start
data: {"type":"message_start","message":{"id":"msg_fake_001","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"usage":{"input_tokens":25,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello from "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"fake anthropic."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}
`, model)
	}
	head := fmt.Sprintf(`{"id":"chatcmpl-fake-001","object":"chat.completion.chunk","created":1700000000,"model":%q`, model)
	return `data: ` + head + `,"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: ` + head + `,"choices":[{"index":0,"delta":{"content":"Hello from "},"finish_reason":null}]}

data: ` + head + `,"choices":[{"index":0,"delta":{"content":"fake openai."},"finish_reason":null}]}

data: ` + head + `,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: ` + head + `,"choices":[],"usage":{"prompt_tokens":25,"completion_tokens":7,"total_tokens":32}}

data: [DONE]
`
}

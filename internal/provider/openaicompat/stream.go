package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Damianen/llm-gateway/internal/provider"
	"github.com/Damianen/llm-gateway/internal/sse"
)

// Stream performs a streaming completion. stream_options.include_usage is
// always requested so streamed requests are costed identically to
// non-streamed ones. The configured timeout applies to time-to-response only.
func (c *Client) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	ctx, cancel := context.WithCancel(ctx)
	httpReq, err := c.newHTTPRequest(ctx, req, true)
	if err != nil {
		cancel()
		return nil, err
	}
	var ttfb *time.Timer
	if c.timeout > 0 {
		ttfb = time.AfterFunc(c.timeout, cancel)
	}
	resp, err := c.http.Do(httpReq)
	if ttfb != nil && !ttfb.Stop() {
		if resp != nil {
			resp.Body.Close()
		}
		cancel()
		return nil, provider.NewTransportError(providerType, context.DeadlineExceeded)
	}
	if err != nil {
		cancel()
		return nil, provider.NewTransportError(providerType, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		cancel()
		return nil, readError(resp)
	}
	return &stream{
		body:      resp.Body,
		scanner:   sse.NewScanner(resp.Body),
		cancel:    cancel,
		textIndex: -1,
		toolIndex: make(map[int]int),
		toolInput: make(map[int]bool),
	}, nil
}

// stream translates a chat-completions chunk stream into canonical events.
// OpenAI has no explicit start/stop framing, so this adapter synthesizes
// EventStart, EventTextStart, and EventBlockStop, and assigns sequential
// canonical block indexes (text first if present, then tool calls in order
// of appearance).
type stream struct {
	body    io.ReadCloser
	scanner *sse.Scanner
	cancel  context.CancelFunc

	pending   []provider.StreamEvent
	started   bool
	textIndex int         // canonical index of the open text block, -1 if none
	toolIndex map[int]int // upstream tool_calls index -> canonical block index
	toolInput map[int]bool
	openOrder []int // canonical indexes in open order
	nextIndex int
	finish    string
	usage     *provider.Usage
	closed    bool // terminal sequence queued
}

type chunkFrame struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (s *stream) Recv() (*provider.StreamEvent, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return &ev, nil
		}
		if s.closed {
			return nil, io.EOF
		}

		raw, err := s.scanner.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if s.finish != "" {
					// Some servers omit [DONE] but did send finish_reason:
					// treat the stream as complete.
					s.queueClosing()
					continue
				}
				return nil, &provider.UpstreamError{Provider: providerType, StatusCode: 0,
					Message: "stream ended unexpectedly (no finish_reason)"}
			}
			return nil, provider.NewTransportError(providerType, err)
		}
		if raw.Data == "[DONE]" {
			s.queueClosing()
			continue
		}

		var chunk chunkFrame
		if err := json.Unmarshal([]byte(raw.Data), &chunk); err != nil {
			return nil, &provider.UpstreamError{Provider: providerType, StatusCode: 0,
				Message: "invalid stream chunk JSON", Err: err}
		}
		if chunk.Error != nil {
			return nil, &provider.UpstreamError{Provider: providerType, StatusCode: 500, Message: chunk.Error.Message}
		}

		var events []provider.StreamEvent
		if !s.started {
			s.started = true
			events = append(events, provider.StreamEvent{Type: provider.EventStart, ID: chunk.ID, Model: chunk.Model})
		}
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			if choice.Delta.Content != "" {
				if s.textIndex < 0 {
					s.textIndex = s.nextIndex
					s.nextIndex++
					s.openOrder = append(s.openOrder, s.textIndex)
					events = append(events, provider.StreamEvent{Type: provider.EventTextStart, Index: s.textIndex})
				}
				events = append(events, provider.StreamEvent{Type: provider.EventTextDelta, Index: s.textIndex, Text: choice.Delta.Content})
			}
			for _, tc := range choice.Delta.ToolCalls {
				ci, known := s.toolIndex[tc.Index]
				if !known {
					ci = s.nextIndex
					s.nextIndex++
					s.toolIndex[tc.Index] = ci
					s.openOrder = append(s.openOrder, ci)
					events = append(events, provider.StreamEvent{
						Type: provider.EventToolUseStart, Index: ci,
						ToolID: tc.ID, ToolName: tc.Function.Name,
					})
				}
				if tc.Function.Arguments != "" {
					s.toolInput[ci] = true
					events = append(events, provider.StreamEvent{Type: provider.EventToolInputDelta, Index: ci, Text: tc.Function.Arguments})
				}
			}
			if choice.FinishReason != "" {
				s.finish = FinishFromOpenAI(choice.FinishReason)
			}
		}
		if chunk.Usage != nil {
			s.usage = &provider.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		s.pending = append(s.pending, events...)
	}
}

// queueClosing emits the canonical terminal sequence: every open block is
// closed (tool blocks that never saw arguments get "{}"), then finish with
// whatever usage the upstream reported, then done.
func (s *stream) queueClosing() {
	if s.closed {
		return
	}
	s.closed = true
	isTool := make(map[int]bool, len(s.toolIndex))
	for _, ci := range s.toolIndex {
		isTool[ci] = true
	}
	for _, ci := range s.openOrder {
		if isTool[ci] && !s.toolInput[ci] {
			s.pending = append(s.pending, provider.StreamEvent{Type: provider.EventToolInputDelta, Index: ci, Text: "{}"})
		}
		s.pending = append(s.pending, provider.StreamEvent{Type: provider.EventBlockStop, Index: ci})
	}
	finish := s.finish
	if finish == "" {
		finish = provider.FinishStop
	}
	var usage provider.Usage
	if s.usage != nil {
		usage = *s.usage
	}
	s.pending = append(s.pending,
		provider.StreamEvent{Type: provider.EventFinish, FinishReason: finish, Usage: usage},
		provider.StreamEvent{Type: provider.EventDone},
	)
}

func (s *stream) Close() error {
	s.cancel()
	return s.body.Close()
}

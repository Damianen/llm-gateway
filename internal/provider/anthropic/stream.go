package anthropic

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

// Stream performs a streaming completion. The configured timeout applies to
// time-to-response only; once the event stream is open, its lifetime is
// bounded by ctx (i.e. the client connection).
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
		// The header-phase timer already fired: surface it as a timeout even
		// if a response squeaked through the race.
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
		body:       resp.Body,
		scanner:    sse.NewScanner(resp.Body),
		cancel:     cancel,
		blockTypes: make(map[int]string),
		toolInput:  make(map[int]bool),
	}, nil
}

// stream translates the Anthropic event stream into canonical events.
type stream struct {
	body    io.ReadCloser
	scanner *sse.Scanner
	cancel  context.CancelFunc

	pending    []provider.StreamEvent
	blockTypes map[int]string // index -> "text" | "tool_use" | "skip"
	toolInput  map[int]bool   // index -> saw at least one input_json_delta
	usageIn    int
	usageOut   int
	finish     string
	done       bool
}

type streamFrame struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		ID    string    `json:"id"`
		Model string    `json:"model"`
		Usage wireUsage `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *wireUsage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *stream) Recv() (*provider.StreamEvent, error) {
	if len(s.pending) > 0 {
		ev := s.pending[0]
		s.pending = s.pending[1:]
		return &ev, nil
	}
	if s.done {
		return nil, io.EOF
	}
	for {
		raw, err := s.scanner.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, &provider.UpstreamError{Provider: providerType, StatusCode: 0,
					Message: "stream ended before message_stop"}
			}
			return nil, provider.NewTransportError(providerType, err)
		}
		var frame streamFrame
		if err := json.Unmarshal([]byte(raw.Data), &frame); err != nil {
			return nil, &provider.UpstreamError{Provider: providerType, StatusCode: 0,
				Message: "invalid stream frame JSON", Err: err}
		}
		switch frame.Type {
		case "message_start":
			s.usageIn = frame.Message.Usage.InputTokens
			return &provider.StreamEvent{
				Type: provider.EventStart, ID: frame.Message.ID, Model: frame.Message.Model,
				Usage: provider.Usage{InputTokens: s.usageIn},
			}, nil
		case "content_block_start":
			switch frame.ContentBlock.Type {
			case "text":
				s.blockTypes[frame.Index] = "text"
				return &provider.StreamEvent{Type: provider.EventTextStart, Index: frame.Index}, nil
			case "tool_use":
				s.blockTypes[frame.Index] = "tool_use"
				return &provider.StreamEvent{
					Type: provider.EventToolUseStart, Index: frame.Index,
					ToolID: frame.ContentBlock.ID, ToolName: frame.ContentBlock.Name,
				}, nil
			default:
				// thinking and other block types: drop the whole block.
				s.blockTypes[frame.Index] = "skip"
				continue
			}
		case "content_block_delta":
			if s.blockTypes[frame.Index] == "skip" {
				continue
			}
			switch frame.Delta.Type {
			case "text_delta":
				return &provider.StreamEvent{Type: provider.EventTextDelta, Index: frame.Index, Text: frame.Delta.Text}, nil
			case "input_json_delta":
				s.toolInput[frame.Index] = true
				return &provider.StreamEvent{Type: provider.EventToolInputDelta, Index: frame.Index, Text: frame.Delta.PartialJSON}, nil
			default:
				continue // signature_delta etc.
			}
		case "content_block_stop":
			if s.blockTypes[frame.Index] == "skip" {
				continue
			}
			if s.blockTypes[frame.Index] == "tool_use" && !s.toolInput[frame.Index] {
				// No input deltas were sent (empty tool input): synthesize
				// "{}" so every consumer accumulates valid JSON arguments.
				s.pending = append(s.pending, provider.StreamEvent{Type: provider.EventBlockStop, Index: frame.Index})
				return &provider.StreamEvent{Type: provider.EventToolInputDelta, Index: frame.Index, Text: "{}"}, nil
			}
			return &provider.StreamEvent{Type: provider.EventBlockStop, Index: frame.Index}, nil
		case "message_delta":
			if frame.Delta.StopReason != "" {
				s.finish = FinishFromStopReason(frame.Delta.StopReason)
			}
			if frame.Usage != nil {
				s.usageOut = frame.Usage.OutputTokens
				if frame.Usage.InputTokens > 0 {
					s.usageIn = frame.Usage.InputTokens
				}
			}
			continue
		case "message_stop":
			s.done = true
			finish := s.finish
			if finish == "" {
				finish = provider.FinishStop
			}
			s.pending = append(s.pending, provider.StreamEvent{Type: provider.EventDone})
			return &provider.StreamEvent{
				Type: provider.EventFinish, FinishReason: finish,
				Usage: provider.Usage{InputTokens: s.usageIn, OutputTokens: s.usageOut},
			}, nil
		case "error":
			status := 500
			if frame.Error.Type == "overloaded_error" {
				status = 529
			}
			return nil, &provider.UpstreamError{Provider: providerType, StatusCode: status, Message: frame.Error.Message}
		default: // ping and future event types
			continue
		}
	}
}

func (s *stream) Close() error {
	s.cancel()
	return s.body.Close()
}

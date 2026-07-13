package server

import (
	"net/http"
	"time"

	"github.com/Damianen/llm-gateway/internal/provider"
	"github.com/Damianen/llm-gateway/internal/provider/anthropic"
	"github.com/Damianen/llm-gateway/internal/provider/openaicompat"
)

// streamWriter translates canonical stream events into one inbound dialect's
// SSE convention. Implementations write (and flush) directly to the client.
type streamWriter interface {
	event(ev *provider.StreamEvent) error
	// terminalError surfaces a mid-stream failure in the dialect's
	// convention; the connection closes afterwards.
	terminalError(status int, code, message string)
}

// --- OpenAI dialect: chat.completion.chunk stream ---

type oaiStreamWriter struct {
	sse          *sseWriter
	id           string
	created      int64
	model        string
	includeUsage bool
	toolIdx      map[int]int // canonical block index -> openai tool_calls index
	usage        *provider.Usage
}

func (d openaiDialect) newStreamWriter(w http.ResponseWriter, model string, opts *inboundOpts, now time.Time) streamWriter {
	return &oaiStreamWriter{
		sse:          newSSEWriter(w),
		id:           "chatcmpl-" + newRequestID(),
		created:      now.Unix(),
		model:        model,
		includeUsage: opts.includeUsage,
		toolIdx:      make(map[int]int),
	}
}

func (o *oaiStreamWriter) chunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id":      o.id,
		"object":  "chat.completion.chunk",
		"created": o.created,
		"model":   o.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
}

func (o *oaiStreamWriter) event(ev *provider.StreamEvent) error {
	switch ev.Type {
	case provider.EventStart:
		if ev.ID != "" {
			o.id = ev.ID
		}
		return o.sse.event("", o.chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	case provider.EventTextDelta:
		return o.sse.event("", o.chunk(map[string]any{"content": ev.Text}, nil))
	case provider.EventToolUseStart:
		oi := len(o.toolIdx)
		o.toolIdx[ev.Index] = oi
		return o.sse.event("", o.chunk(map[string]any{"tool_calls": []map[string]any{{
			"index": oi,
			"id":    ev.ToolID,
			"type":  "function",
			"function": map[string]any{
				"name":      ev.ToolName,
				"arguments": "",
			},
		}}}, nil))
	case provider.EventToolInputDelta:
		oi, ok := o.toolIdx[ev.Index]
		if !ok {
			return nil // defensive: delta for an unknown block
		}
		return o.sse.event("", o.chunk(map[string]any{"tool_calls": []map[string]any{{
			"index": oi,
			"function": map[string]any{
				"arguments": ev.Text,
			},
		}}}, nil))
	case provider.EventFinish:
		u := ev.Usage
		o.usage = &u
		return o.sse.event("", o.chunk(map[string]any{}, openaicompat.OpenAIFromFinish(ev.FinishReason)))
	case provider.EventDone:
		if o.includeUsage && o.usage != nil {
			usageChunk := map[string]any{
				"id":      o.id,
				"object":  "chat.completion.chunk",
				"created": o.created,
				"model":   o.model,
				"choices": []map[string]any{},
				"usage": map[string]any{
					"prompt_tokens":     o.usage.InputTokens,
					"completion_tokens": o.usage.OutputTokens,
					"total_tokens":      o.usage.InputTokens + o.usage.OutputTokens,
				},
			}
			if err := o.sse.event("", usageChunk); err != nil {
				return err
			}
		}
		return o.sse.raw("[DONE]")
	default: // text_start, block_stop: no OpenAI equivalent
		return nil
	}
}

func (o *oaiStreamWriter) terminalError(status int, code, message string) {
	var codeVal any
	if code != "" {
		codeVal = code
	}
	// OpenAI streams surface errors as a data frame with an error object,
	// then the connection closes (no [DONE]).
	_ = o.sse.event("", map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    openaiErrorType(status),
			"param":   nil,
			"code":    codeVal,
		},
	})
}

// --- Anthropic dialect: Messages event stream ---

type antStreamWriter struct {
	sse         *sseWriter
	model       string
	inputTokens int
}

func (d anthropicDialect) newStreamWriter(w http.ResponseWriter, model string, opts *inboundOpts, now time.Time) streamWriter {
	return &antStreamWriter{sse: newSSEWriter(w), model: model}
}

func (a *antStreamWriter) event(ev *provider.StreamEvent) error {
	switch ev.Type {
	case provider.EventStart:
		id := ev.ID
		if id == "" {
			id = "msg_" + newRequestID()
		}
		a.inputTokens = ev.Usage.InputTokens
		return a.sse.event("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            id,
				"type":          "message",
				"role":          "assistant",
				"model":         a.model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage": map[string]any{
					"input_tokens":  ev.Usage.InputTokens,
					"output_tokens": 0,
				},
			},
		})
	case provider.EventTextStart:
		return a.sse.event("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         ev.Index,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	case provider.EventTextDelta:
		return a.sse.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": ev.Index,
			"delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})
	case provider.EventToolUseStart:
		return a.sse.event("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": ev.Index,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    ev.ToolID,
				"name":  ev.ToolName,
				"input": map[string]any{},
			},
		})
	case provider.EventToolInputDelta:
		return a.sse.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": ev.Index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.Text},
		})
	case provider.EventBlockStop:
		return a.sse.event("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": ev.Index,
		})
	case provider.EventFinish:
		in := ev.Usage.InputTokens
		if in == 0 {
			in = a.inputTokens
		}
		return a.sse.event("message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   anthropic.StopReasonFromFinish(ev.FinishReason),
				"stop_sequence": nil,
			},
			"usage": map[string]any{
				"input_tokens":  in,
				"output_tokens": ev.Usage.OutputTokens,
			},
		})
	case provider.EventDone:
		return a.sse.event("message_stop", map[string]any{"type": "message_stop"})
	default:
		return nil
	}
}

func (a *antStreamWriter) terminalError(status int, code, message string) {
	_ = a.sse.event("error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicErrorType(status),
			"message": message,
		},
	})
}

// replayAsStream synthesizes a canonical event stream from a complete
// response — used to serve cached responses to streaming requests.
func replayAsStream(resp *provider.Response, sw streamWriter) error {
	events := []provider.StreamEvent{{
		Type: provider.EventStart, ID: resp.ID, Model: resp.Model,
		Usage: provider.Usage{InputTokens: resp.Usage.InputTokens},
	}}
	for i, b := range resp.Blocks {
		switch b.Type {
		case provider.BlockText:
			events = append(events,
				provider.StreamEvent{Type: provider.EventTextStart, Index: i},
				provider.StreamEvent{Type: provider.EventTextDelta, Index: i, Text: b.Text},
				provider.StreamEvent{Type: provider.EventBlockStop, Index: i},
			)
		case provider.BlockToolUse:
			input := "{}"
			if len(b.Input) > 0 {
				input = string(b.Input)
			}
			events = append(events,
				provider.StreamEvent{Type: provider.EventToolUseStart, Index: i, ToolID: b.ID, ToolName: b.Name},
				provider.StreamEvent{Type: provider.EventToolInputDelta, Index: i, Text: input},
				provider.StreamEvent{Type: provider.EventBlockStop, Index: i},
			)
		}
	}
	events = append(events,
		provider.StreamEvent{Type: provider.EventFinish, FinishReason: resp.FinishReason, Usage: resp.Usage},
		provider.StreamEvent{Type: provider.EventDone},
	)
	for _, ev := range events {
		if err := sw.event(&ev); err != nil {
			return err
		}
	}
	return nil
}

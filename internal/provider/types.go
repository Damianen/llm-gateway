// Package provider defines the canonical request/response model shared by
// every inbound surface and outbound adapter, plus the Provider interface
// adapters implement. Inbound handlers translate their dialect into these
// types; outbound adapters translate them to provider wire formats.
package provider

import (
	"context"
	"encoding/json"
)

// Canonical message roles. Tool results are represented as blocks inside
// user messages, so only two roles exist.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Block types.
const (
	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// Canonical finish reasons.
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolUse       = "tool_use"
	FinishContentFilter = "content_filter"
)

// ToolChoice modes.
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required"
	ToolChoiceTool     = "tool"
)

// Request is the canonical completion request. The JSON encoding is used for
// cache keys and (when enabled) body logging; it never goes over the wire.
type Request struct {
	Model       string     `json:"model"`
	System      string     `json:"system,omitempty"`
	Messages    []Message  `json:"messages"`
	Tools       []Tool     `json:"tools,omitempty"`
	ToolChoice  ToolChoice `json:"tool_choice,omitzero"`
	MaxTokens   int        `json:"max_tokens,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
	TopP        *float64   `json:"top_p,omitempty"`
	Stop        []string   `json:"stop,omitempty"`
	Stream      bool       `json:"stream,omitempty"`
}

// ToolChoice constrains which tool the model may call. Mode is one of "",
// ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired, or ToolChoiceTool
// (Name set). Empty means the provider default.
type ToolChoice struct {
	Mode string `json:"mode,omitempty"`
	Name string `json:"name,omitempty"`
}

// Message is one conversation turn.
type Message struct {
	Role   string  `json:"role"`
	Blocks []Block `json:"blocks"`
}

// Block is one content block within a message. Type selects which fields are
// meaningful: text blocks use Text; tool_use blocks use ID/Name/Input;
// tool_result blocks use ToolUseID/Content/IsError.
type Block struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// TextBlock returns a text content block.
func TextBlock(text string) Block {
	return Block{Type: BlockText, Text: text}
}

// ToolUseBlock returns a tool invocation block. input must be a JSON object.
func ToolUseBlock(id, name string, input json.RawMessage) Block {
	return Block{Type: BlockToolUse, ID: id, Name: name, Input: input}
}

// ToolResultBlock returns a tool result block referencing a prior tool_use ID.
func ToolResultBlock(toolUseID, content string, isError bool) Block {
	return Block{Type: BlockToolResult, ToolUseID: toolUseID, Content: content, IsError: isError}
}

// Tool is a tool definition offered to the model.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// Usage is the token consumption reported by the upstream provider. Counts
// always come from the provider; the gateway never tokenizes locally.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is the canonical completion response.
type Response struct {
	ID           string  `json:"id"`
	Model        string  `json:"model"`
	Blocks       []Block `json:"blocks"`
	FinishReason string  `json:"finish_reason"`
	Usage        Usage   `json:"usage"`
}

// Text returns the concatenated text of all text blocks.
func (r *Response) Text() string {
	var out string
	for _, b := range r.Blocks {
		if b.Type == BlockText {
			out += b.Text
		}
	}
	return out
}

// EventType identifies a canonical stream event.
type EventType string

// Canonical stream event types. Adapters guarantee ordering: EventStart
// first; each block's start precedes its deltas; every opened block emits
// EventBlockStop before EventFinish; EventDone is terminal.
const (
	EventStart          EventType = "start"
	EventTextStart      EventType = "text_start"
	EventTextDelta      EventType = "text_delta"
	EventToolUseStart   EventType = "tool_use_start"
	EventToolInputDelta EventType = "tool_input_delta"
	EventBlockStop      EventType = "block_stop"
	EventFinish         EventType = "finish"
	EventDone           EventType = "done"
)

// StreamEvent is one canonical streaming event.
type StreamEvent struct {
	Type EventType

	// EventStart: upstream response ID and model.
	ID    string
	Model string

	// Block events: Index is the content block index (sequential from 0).
	// Text carries text_delta text or tool_input_delta partial JSON.
	Index    int
	Text     string
	ToolID   string
	ToolName string

	// EventFinish.
	FinishReason string

	// EventStart carries InputTokens when the upstream reports it up front
	// (Anthropic); EventFinish carries OutputTokens and, when reported late
	// (OpenAI usage chunk), InputTokens as well.
	Usage Usage
}

// Stream yields canonical events from an in-flight upstream response.
// Recv returns io.EOF after the EventDone event has been consumed.
type Stream interface {
	Recv() (*StreamEvent, error)
	Close() error
}

// Provider is an outbound adapter for one upstream endpoint. Implementations
// must honor ctx cancellation and classify upstream failures as *UpstreamError.
type Provider interface {
	Complete(ctx context.Context, req *Request) (*Response, error)
	Stream(ctx context.Context, req *Request) (Stream, error)
}

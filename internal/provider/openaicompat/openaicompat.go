// Package openaicompat is the outbound adapter for any OpenAI-compatible
// chat-completions endpoint: OpenRouter, OpenAI itself, and local servers
// such as Ollama, llama.cpp, or LM Studio (differing only in base URL and
// key). It translates the canonical model to the chat/completions wire
// format and back.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Damianen/llm-gateway/internal/provider"
)

const (
	providerType     = "openai"
	maxResponseBytes = 32 << 20
	maxErrorBytes    = 64 << 10
)

// Client is a provider.Provider speaking the OpenAI chat-completions API.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	timeout time.Duration
}

var _ provider.Provider = (*Client)(nil)

// New returns a Client. baseURL includes the API prefix (e.g.
// https://openrouter.ai/api/v1 or http://localhost:11434/v1), matching the
// OpenAI SDK convention; the client appends /chat/completions. An empty
// apiKey sends no Authorization header (local servers). timeout caps one
// upstream attempt: the full round trip for Complete, time-to-response for
// Stream.
func New(baseURL, apiKey string, hc *http.Client, timeout time.Duration) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    hc,
		timeout: timeout,
	}
}

// --- wire types ---

type wireRequest struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Tools         []wireTool     `json:"tools,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"` // string or {"type":"function",...}
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string      `json:"type"`
	Function wireToolDef `json:"function"`
}

type wireToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type wireResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []wireChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage"`
}

type wireChoice struct {
	Index        int             `json:"index"`
	Message      wireRespMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type wireRespMessage struct {
	Role      string         `json:"role"`
	Content   *string        `json:"content"`
	ToolCalls []wireToolCall `json:"tool_calls"`
}

type wireErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// --- request building ---

func strPtr(s string) *string { return &s }

func (c *Client) buildRequest(req *provider.Request, stream bool) *wireRequest {
	wr := &wireRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
		Stream:      stream,
	}
	if stream {
		// Locked decision: always request usage on streams so streamed
		// requests are costed identically to non-streamed ones.
		wr.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if req.System != "" {
		wr.Messages = append(wr.Messages, wireMessage{Role: "system", Content: strPtr(req.System)})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case provider.RoleAssistant:
			msg := wireMessage{Role: "assistant"}
			var texts []string
			for _, b := range m.Blocks {
				switch b.Type {
				case provider.BlockText:
					if b.Text != "" {
						texts = append(texts, b.Text)
					}
				case provider.BlockToolUse:
					args := "{}"
					if len(b.Input) > 0 {
						args = string(b.Input)
					}
					msg.ToolCalls = append(msg.ToolCalls, wireToolCall{
						ID: b.ID, Type: "function",
						Function: wireFunction{Name: b.Name, Arguments: args},
					})
				}
			}
			if len(texts) > 0 {
				msg.Content = strPtr(strings.Join(texts, "\n\n"))
			}
			if msg.Content != nil || len(msg.ToolCalls) > 0 {
				wr.Messages = append(wr.Messages, msg)
			}
		default: // user turn: tool results become role:"tool" messages first
			var texts []string
			for _, b := range m.Blocks {
				switch b.Type {
				case provider.BlockToolResult:
					content := b.Content
					if b.IsError {
						// The chat-completions format has no error flag on
						// tool results; surface it in-band.
						content = "[tool error] " + content
					}
					wr.Messages = append(wr.Messages, wireMessage{
						Role: "tool", ToolCallID: b.ToolUseID, Content: strPtr(content),
					})
				case provider.BlockText:
					if b.Text != "" {
						texts = append(texts, b.Text)
					}
				}
			}
			if len(texts) > 0 {
				wr.Messages = append(wr.Messages, wireMessage{Role: "user", Content: strPtr(strings.Join(texts, "\n\n"))})
			}
		}
	}
	for _, t := range req.Tools {
		wr.Tools = append(wr.Tools, wireTool{Type: "function", Function: wireToolDef{
			Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
		}})
	}
	switch req.ToolChoice.Mode {
	case provider.ToolChoiceAuto:
		wr.ToolChoice = "auto"
	case provider.ToolChoiceNone:
		wr.ToolChoice = "none"
	case provider.ToolChoiceRequired:
		wr.ToolChoice = "required"
	case provider.ToolChoiceTool:
		wr.ToolChoice = map[string]any{
			"type":     "function",
			"function": map[string]string{"name": req.ToolChoice.Name},
		}
	}
	return wr
}

func (c *Client) newHTTPRequest(ctx context.Context, req *provider.Request, stream bool) (*http.Request, error) {
	body, err := json.Marshal(c.buildRequest(req, stream))
	if err != nil {
		return nil, fmt.Errorf("marshal openai request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build openai request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	return httpReq, nil
}

// Complete performs a non-streaming completion.
func (c *Client) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	httpReq, err := c.newHTTPRequest(ctx, req, false)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, provider.NewTransportError(providerType, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readError(resp)
	}
	var out wireResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return nil, &provider.UpstreamError{Provider: providerType, StatusCode: resp.StatusCode,
			Message: "invalid response JSON", Err: err}
	}
	return toCanonical(&out)
}

func toCanonical(w *wireResponse) (*provider.Response, error) {
	if len(w.Choices) == 0 {
		return nil, &provider.UpstreamError{Provider: providerType, StatusCode: http.StatusOK,
			Message: "response contained no choices"}
	}
	choice := w.Choices[0]
	resp := &provider.Response{
		ID:           w.ID,
		Model:        w.Model,
		FinishReason: FinishFromOpenAI(choice.FinishReason),
	}
	if w.Usage != nil {
		resp.Usage = provider.Usage{InputTokens: w.Usage.PromptTokens, OutputTokens: w.Usage.CompletionTokens}
	}
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		resp.Blocks = append(resp.Blocks, provider.TextBlock(*choice.Message.Content))
	}
	for _, tc := range choice.Message.ToolCalls {
		resp.Blocks = append(resp.Blocks, provider.ToolUseBlock(tc.ID, tc.Function.Name, normalizeArgs(tc.Function.Arguments)))
	}
	return resp, nil
}

// normalizeArgs turns a tool-call arguments string into a valid JSON value so
// it can be re-embedded in any wire format. Empty means no arguments; invalid
// JSON (models occasionally emit it) is preserved under a "_raw" wrapper
// rather than corrupting the downstream payload.
func normalizeArgs(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	wrapped, err := json.Marshal(map[string]string{"_raw": args})
	if err != nil {
		return json.RawMessage("{}")
	}
	return wrapped
}

func readError(resp *http.Response) *provider.UpstreamError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	msg := strings.TrimSpace(string(raw))
	var we wireErrorBody
	if json.Unmarshal(raw, &we) == nil && we.Error.Message != "" {
		msg = we.Error.Message
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return &provider.UpstreamError{Provider: providerType, StatusCode: resp.StatusCode, Message: msg}
}

// FinishFromOpenAI maps an OpenAI finish_reason to a canonical finish reason.
func FinishFromOpenAI(s string) string {
	switch s {
	case "length":
		return provider.FinishLength
	case "tool_calls", "function_call":
		return provider.FinishToolUse
	case "content_filter":
		return provider.FinishContentFilter
	default: // stop, empty, unknown
		return provider.FinishStop
	}
}

// OpenAIFromFinish maps a canonical finish reason to an OpenAI finish_reason.
// Used by the OpenAI-dialect inbound surface.
func OpenAIFromFinish(f string) string {
	switch f {
	case provider.FinishLength:
		return "length"
	case provider.FinishToolUse:
		return "tool_calls"
	case provider.FinishContentFilter:
		return "content_filter"
	default:
		return "stop"
	}
}

// Package anthropic is the outbound adapter for the Anthropic Messages API.
// It translates the canonical model to the /v1/messages wire format and back.
package anthropic

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
	// Version is the anthropic-version header sent upstream.
	Version = "2023-06-01"

	providerType     = "anthropic"
	maxResponseBytes = 32 << 20
	maxErrorBytes    = 64 << 10
)

// Client is a provider.Provider speaking the Anthropic Messages API.
type Client struct {
	baseURL          string
	apiKey           string
	http             *http.Client
	defaultMaxTokens int
	timeout          time.Duration
}

var _ provider.Provider = (*Client)(nil)

// New returns a Client. baseURL is the host root (the client appends
// /v1/messages), matching the ANTHROPIC_BASE_URL convention. timeout caps one
// upstream attempt: the full round trip for Complete, time-to-response for
// Stream. defaultMaxTokens is applied when the canonical request omits
// max_tokens, because the Messages API requires it.
func New(baseURL, apiKey string, hc *http.Client, defaultMaxTokens int, timeout time.Duration) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{
		baseURL:          strings.TrimRight(baseURL, "/"),
		apiKey:           apiKey,
		http:             hc,
		defaultMaxTokens: defaultMaxTokens,
		timeout:          timeout,
	}
}

// --- wire types ---

type wireRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        string          `json:"system,omitempty"`
	Messages      []wireMessage   `json:"messages"`
	Tools         []wireTool      `json:"tools,omitempty"`
	ToolChoice    *wireToolChoice `json:"tool_choice,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

type wireBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type wireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type wireResponse struct {
	ID         string      `json:"id"`
	Type       string      `json:"type"`
	Role       string      `json:"role"`
	Model      string      `json:"model"`
	Content    []wireBlock `json:"content"`
	StopReason string      `json:"stop_reason"`
	Usage      wireUsage   `json:"usage"`
}

type wireError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// --- request building ---

func (c *Client) buildRequest(req *provider.Request, stream bool) *wireRequest {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.defaultMaxTokens
	}
	wr := &wireRequest{
		Model:         req.Model,
		MaxTokens:     maxTokens,
		System:        req.System,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.Stop,
		Stream:        stream,
	}
	for _, t := range req.Tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		wr.Tools = append(wr.Tools, wireTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	switch req.ToolChoice.Mode {
	case provider.ToolChoiceAuto:
		wr.ToolChoice = &wireToolChoice{Type: "auto"}
	case provider.ToolChoiceNone:
		wr.ToolChoice = &wireToolChoice{Type: "none"}
	case provider.ToolChoiceRequired:
		wr.ToolChoice = &wireToolChoice{Type: "any"}
	case provider.ToolChoiceTool:
		wr.ToolChoice = &wireToolChoice{Type: "tool", Name: req.ToolChoice.Name}
	}
	for _, m := range req.Messages {
		blocks := make([]wireBlock, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			switch b.Type {
			case provider.BlockText:
				if b.Text == "" {
					continue // the Messages API rejects empty text blocks
				}
				blocks = append(blocks, wireBlock{Type: "text", Text: b.Text})
			case provider.BlockToolUse:
				input := b.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, wireBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: input})
			case provider.BlockToolResult:
				blocks = append(blocks, wireBlock{Type: "tool_result", ToolUseID: b.ToolUseID, Content: b.Content, IsError: b.IsError})
			}
		}
		if len(blocks) == 0 {
			continue
		}
		// Merge consecutive same-role messages: the Messages API expects
		// alternating turns.
		if n := len(wr.Messages); n > 0 && wr.Messages[n-1].Role == m.Role {
			wr.Messages[n-1].Content = append(wr.Messages[n-1].Content, blocks...)
			continue
		}
		wr.Messages = append(wr.Messages, wireMessage{Role: m.Role, Content: blocks})
	}
	return wr
}

func (c *Client) newHTTPRequest(ctx context.Context, req *provider.Request, stream bool) (*http.Request, error) {
	body, err := json.Marshal(c.buildRequest(req, stream))
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build anthropic request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", Version)
	if c.apiKey != "" {
		httpReq.Header.Set("x-api-key", c.apiKey)
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
	return toCanonical(&out), nil
}

func toCanonical(w *wireResponse) *provider.Response {
	resp := &provider.Response{
		ID:           w.ID,
		Model:        w.Model,
		FinishReason: FinishFromStopReason(w.StopReason),
		Usage:        provider.Usage{InputTokens: w.Usage.InputTokens, OutputTokens: w.Usage.OutputTokens},
	}
	for _, b := range w.Content {
		switch b.Type {
		case "text":
			resp.Blocks = append(resp.Blocks, provider.TextBlock(b.Text))
		case "tool_use":
			resp.Blocks = append(resp.Blocks, provider.ToolUseBlock(b.ID, b.Name, b.Input))
		}
	}
	return resp
}

func readError(resp *http.Response) *provider.UpstreamError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	msg := strings.TrimSpace(string(raw))
	var we wireError
	if json.Unmarshal(raw, &we) == nil && we.Error.Message != "" {
		msg = we.Error.Message
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return &provider.UpstreamError{Provider: providerType, StatusCode: resp.StatusCode, Message: msg}
}

// FinishFromStopReason maps an Anthropic stop_reason to a canonical finish
// reason.
func FinishFromStopReason(s string) string {
	switch s {
	case "max_tokens":
		return provider.FinishLength
	case "tool_use":
		return provider.FinishToolUse
	case "refusal":
		return provider.FinishContentFilter
	default: // end_turn, stop_sequence, pause_turn, unknown
		return provider.FinishStop
	}
}

// StopReasonFromFinish maps a canonical finish reason to an Anthropic
// stop_reason. Used by the Anthropic-dialect inbound surface.
func StopReasonFromFinish(f string) string {
	switch f {
	case provider.FinishLength:
		return "max_tokens"
	case provider.FinishToolUse:
		return "tool_use"
	case provider.FinishContentFilter:
		return "refusal"
	default:
		return "end_turn"
	}
}

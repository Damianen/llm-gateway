package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Damianen/llm-gateway/internal/provider"
	"github.com/Damianen/llm-gateway/internal/provider/anthropic"
)

// anthropicDialect implements the Anthropic Messages inbound surface.
type anthropicDialect struct{}

func (anthropicDialect) name() string     { return dialectAnthropic }
func (anthropicDialect) endpoint() string { return "messages" }

func (anthropicDialect) writeError(w http.ResponseWriter, status int, code, message string) {
	writeAnthropicError(w, status, message)
}

// --- inbound wire types ---

type antInRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system"`
	Messages      []antInMessage  `json:"messages"`
	Tools         []antInTool     `json:"tools"`
	ToolChoice    *antToolChoice  `json:"tool_choice"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
}

type antInMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type antInBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type antInTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type antToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

func (d anthropicDialect) parseRequest(body []byte) (*provider.Request, *inboundOpts, *apiError) {
	var in antInRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, nil, badRequest("invalid_json", "invalid request JSON: %v", err)
	}
	if in.Model == "" {
		return nil, nil, badRequest("missing_model", "model is required")
	}
	if len(in.Messages) == 0 {
		return nil, nil, badRequest("missing_messages", "messages must not be empty")
	}

	req := &provider.Request{
		Model:       in.Model,
		MaxTokens:   in.MaxTokens, // 0 is allowed here: the gateway applies default_max_tokens
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stop:        in.StopSequences,
		Stream:      in.Stream,
	}

	system, err := antSystemText(in.System)
	if err != nil {
		return nil, nil, badRequest("invalid_system", "%v", err)
	}
	req.System = system

	for i, m := range in.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, nil, badRequest("invalid_role", "messages[%d]: role must be user or assistant, got %q", i, m.Role)
		}
		blocks, err := antContentBlocks(m.Content)
		if err != nil {
			return nil, nil, badRequest("invalid_content", "messages[%d]: %v", i, err)
		}
		req.Messages = append(req.Messages, provider.Message{Role: m.Role, Blocks: blocks})
	}

	for _, t := range in.Tools {
		req.Tools = append(req.Tools, provider.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}

	if in.ToolChoice != nil {
		switch in.ToolChoice.Type {
		case "auto":
			req.ToolChoice = provider.ToolChoice{Mode: provider.ToolChoiceAuto}
		case "none":
			req.ToolChoice = provider.ToolChoice{Mode: provider.ToolChoiceNone}
		case "any":
			req.ToolChoice = provider.ToolChoice{Mode: provider.ToolChoiceRequired}
		case "tool":
			if in.ToolChoice.Name == "" {
				return nil, nil, badRequest("invalid_tool_choice", "tool_choice type \"tool\" requires a name")
			}
			req.ToolChoice = provider.ToolChoice{Mode: provider.ToolChoiceTool, Name: in.ToolChoice.Name}
		default:
			return nil, nil, badRequest("invalid_tool_choice", "tool_choice type %q is not auto|any|tool|none", in.ToolChoice.Type)
		}
	}

	return req, &inboundOpts{requestedModel: in.Model}, nil
}

// antSystemText accepts a system prompt as a string or an array of text
// blocks.
func antSystemText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("system must be a string or an array of text blocks")
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			return "", fmt.Errorf("system block type %q is not supported", b.Type)
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n\n"), nil
}

// antContentBlocks accepts message content as a string or an array of blocks.
// Unsupported block types that carry no conversational state (e.g. thinking)
// are dropped; media blocks are rejected as out of scope.
func antContentBlocks(raw json.RawMessage) ([]provider.Block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("content is required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []provider.Block{provider.TextBlock(s)}, nil
	}
	var wire []antInBlock
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("content must be a string or an array of blocks")
	}
	var blocks []provider.Block
	for _, b := range wire {
		switch b.Type {
		case "text":
			blocks = append(blocks, provider.TextBlock(b.Text))
		case "tool_use":
			blocks = append(blocks, provider.ToolUseBlock(b.ID, b.Name, b.Input))
		case "tool_result":
			content, err := antToolResultText(b.Content)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, provider.ToolResultBlock(b.ToolUseID, content, b.IsError))
		case "image", "document":
			return nil, fmt.Errorf("block type %q is not supported (multimodal is out of scope)", b.Type)
		default:
			// thinking, redacted_thinking, server_tool_use, etc.: dropped.
			continue
		}
	}
	return blocks, nil
}

// antToolResultText accepts tool_result content as a string or an array of
// text blocks.
func antToolResultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("tool_result content must be a string or an array of text blocks")
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			return "", fmt.Errorf("tool_result block type %q is not supported", b.Type)
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n"), nil
}

// --- response writing ---

func (d anthropicDialect) writeResponse(w http.ResponseWriter, model string, resp *provider.Response, _ time.Time) {
	content := []map[string]any{}
	for _, b := range resp.Blocks {
		switch b.Type {
		case provider.BlockText:
			content = append(content, map[string]any{"type": "text", "text": b.Text})
		case provider.BlockToolUse:
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    b.ID,
				"name":  b.Name,
				"input": input,
			})
		}
	}
	id := resp.ID
	if id == "" {
		id = "msg_" + newRequestID()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   anthropic.StopReasonFromFinish(resp.FinishReason),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  resp.Usage.InputTokens,
			"output_tokens": resp.Usage.OutputTokens,
		},
	})
}

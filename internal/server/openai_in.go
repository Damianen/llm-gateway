package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Damianen/llm-gateway/internal/provider"
	"github.com/Damianen/llm-gateway/internal/provider/openaicompat"
)

// openaiDialect implements the OpenAI chat-completions inbound surface.
type openaiDialect struct{}

func (openaiDialect) name() string     { return dialectOpenAI }
func (openaiDialect) endpoint() string { return "chat_completions" }

func (openaiDialect) writeError(w http.ResponseWriter, status int, code, message string) {
	writeOpenAIError(w, status, code, message)
}

// --- inbound wire types ---

type oaiInRequest struct {
	Model               string          `json:"model"`
	Messages            []oaiInMessage  `json:"messages"`
	Tools               []oaiInTool     `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                json.RawMessage `json:"stop"`
	Stream              bool            `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	N int `json:"n"`
}

type oaiInMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []oaiToolCall   `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiInTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func (d openaiDialect) parseRequest(body []byte) (*provider.Request, *inboundOpts, *apiError) {
	var in oaiInRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, nil, badRequest("invalid_json", "invalid request JSON: %v", err)
	}
	if in.Model == "" {
		return nil, nil, badRequest("missing_model", "model is required")
	}
	if len(in.Messages) == 0 {
		return nil, nil, badRequest("missing_messages", "messages must not be empty")
	}
	if in.N > 1 {
		return nil, nil, badRequest("unsupported_parameter", "n > 1 is not supported by this gateway")
	}

	req := &provider.Request{
		Model:       in.Model,
		MaxTokens:   in.MaxTokens,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stream:      in.Stream,
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = in.MaxCompletionTokens
	}

	if len(in.Stop) > 0 {
		var one string
		var many []string
		switch {
		case json.Unmarshal(in.Stop, &one) == nil:
			req.Stop = []string{one}
		case json.Unmarshal(in.Stop, &many) == nil:
			req.Stop = many
		default:
			return nil, nil, badRequest("invalid_stop", "stop must be a string or array of strings")
		}
	}

	var systemParts []string
	for i, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			text, err := oaiContentText(m.Content)
			if err != nil {
				return nil, nil, badRequest("invalid_content", "messages[%d]: %v", i, err)
			}
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "user":
			text, err := oaiContentText(m.Content)
			if err != nil {
				return nil, nil, badRequest("invalid_content", "messages[%d]: %v", i, err)
			}
			req.Messages = append(req.Messages, provider.Message{
				Role:   provider.RoleUser,
				Blocks: []provider.Block{provider.TextBlock(text)},
			})
		case "assistant":
			var blocks []provider.Block
			text, err := oaiContentText(m.Content)
			if err != nil {
				return nil, nil, badRequest("invalid_content", "messages[%d]: %v", i, err)
			}
			if text != "" {
				blocks = append(blocks, provider.TextBlock(text))
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, provider.ToolUseBlock(tc.ID, tc.Function.Name,
					openaicompat.NormalizeArgs(tc.Function.Arguments)))
			}
			req.Messages = append(req.Messages, provider.Message{Role: provider.RoleAssistant, Blocks: blocks})
		case "tool":
			if m.ToolCallID == "" {
				return nil, nil, badRequest("invalid_message", "messages[%d]: tool message requires tool_call_id", i)
			}
			text, err := oaiContentText(m.Content)
			if err != nil {
				return nil, nil, badRequest("invalid_content", "messages[%d]: %v", i, err)
			}
			req.Messages = append(req.Messages, provider.Message{
				Role:   provider.RoleUser,
				Blocks: []provider.Block{provider.ToolResultBlock(m.ToolCallID, text, false)},
			})
		default:
			return nil, nil, badRequest("invalid_role", "messages[%d]: unknown role %q", i, m.Role)
		}
	}
	req.System = strings.Join(systemParts, "\n\n")

	for _, t := range in.Tools {
		req.Tools = append(req.Tools, provider.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}

	if len(in.ToolChoice) > 0 {
		tc, err := parseOAIToolChoice(in.ToolChoice)
		if err != nil {
			return nil, nil, badRequest("invalid_tool_choice", "%v", err)
		}
		req.ToolChoice = tc
	}

	opts := &inboundOpts{
		requestedModel: in.Model,
		includeUsage:   in.StreamOptions != nil && in.StreamOptions.IncludeUsage,
	}
	return req, opts, nil
}

// oaiContentText extracts text from a chat-completions content value: null,
// a string, or an array of {"type":"text"} parts. Multimodal parts are
// rejected (out of scope for v0.1).
func oaiContentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("content must be a string or an array of content parts")
	}
	var texts []string
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("content part type %q is not supported (multimodal is out of scope)", p.Type)
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n\n"), nil
}

func parseOAIToolChoice(raw json.RawMessage) (provider.ToolChoice, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return provider.ToolChoice{Mode: provider.ToolChoiceAuto}, nil
		case "none":
			return provider.ToolChoice{Mode: provider.ToolChoiceNone}, nil
		case "required":
			return provider.ToolChoice{Mode: provider.ToolChoiceRequired}, nil
		default:
			return provider.ToolChoice{}, fmt.Errorf("tool_choice %q is not auto|none|required or a function object", s)
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Function.Name == "" {
		return provider.ToolChoice{}, fmt.Errorf("tool_choice object must be {\"type\":\"function\",\"function\":{\"name\":...}}")
	}
	return provider.ToolChoice{Mode: provider.ToolChoiceTool, Name: obj.Function.Name}, nil
}

// --- response writing ---

func (d openaiDialect) writeResponse(w http.ResponseWriter, model string, resp *provider.Response, now time.Time) {
	msg := map[string]any{"role": "assistant"}
	var texts []string
	var toolCalls []map[string]any
	for _, b := range resp.Blocks {
		switch b.Type {
		case provider.BlockText:
			texts = append(texts, b.Text)
		case provider.BlockToolUse:
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": args,
				},
			})
		}
	}
	if len(texts) > 0 {
		msg["content"] = strings.Join(texts, "\n\n")
	} else if len(toolCalls) > 0 {
		msg["content"] = nil
	} else {
		msg["content"] = ""
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	id := resp.ID
	if id == "" {
		id = "chatcmpl-" + newRequestID()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": now.Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": openaicompat.OpenAIFromFinish(resp.FinishReason),
		}},
		"usage": map[string]any{
			"prompt_tokens":     resp.Usage.InputTokens,
			"completion_tokens": resp.Usage.OutputTokens,
			"total_tokens":      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	})
}

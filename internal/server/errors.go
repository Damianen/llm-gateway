package server

import (
	"encoding/json"
	"net/http"
)

// Inbound dialects. Every error a client sees is shaped like the API it spoke.
const (
	dialectOpenAI    = "openai"
	dialectAnthropic = "anthropic"
)

// writeDialectError writes an error response in the given inbound dialect.
// code is the OpenAI-style machine-readable code (ignored by the Anthropic
// shape, which derives its type from the status).
func writeDialectError(w http.ResponseWriter, dialect string, status int, code, message string) {
	switch dialect {
	case dialectAnthropic:
		writeAnthropicError(w, status, message)
	default:
		writeOpenAIError(w, status, code, message)
	}
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	var codeVal any
	if code != "" {
		codeVal = code
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    openaiErrorType(status),
			"param":   nil,
			"code":    codeVal,
		},
	})
}

func writeAnthropicError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicErrorType(status),
			"message": message,
		},
	})
}

func openaiErrorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

func anthropicErrorType(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeAdminError writes a plain admin-API error (not dialect-shaped).
func writeAdminError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

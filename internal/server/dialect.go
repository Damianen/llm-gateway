package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Damianen/llm-gateway/internal/provider"
)

// dialect translates between one inbound API surface and the canonical model.
// Everything a client sees — responses, errors, streams — is shaped like the
// API it spoke.
type dialect interface {
	name() string     // dialectOpenAI or dialectAnthropic
	endpoint() string // request-log label: "chat_completions" or "messages"

	// parseRequest translates an inbound body to the canonical request.
	parseRequest(body []byte) (*provider.Request, *inboundOpts, *apiError)

	// writeResponse translates a canonical response back to the dialect.
	// model is echoed as the client requested it (alias included).
	writeResponse(w http.ResponseWriter, model string, resp *provider.Response, now time.Time)

	writeError(w http.ResponseWriter, status int, code, message string)
}

// inboundOpts carries dialect-specific request details that are not part of
// the canonical model.
type inboundOpts struct {
	requestedModel string // raw model string as sent, echoed back in responses
	includeUsage   bool   // OpenAI stream_options.include_usage
}

// apiError is a client-facing request error in canonical form; dialects shape
// it on the way out.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func badRequest(code, format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, code: code, message: fmt.Sprintf(format, args...)}
}

package provider

import (
	"context"
	"errors"
	"fmt"
)

// UpstreamError describes a failed upstream call. StatusCode 0 means the
// request never produced an HTTP response (transport error or timeout).
type UpstreamError struct {
	Provider   string // adapter type: "anthropic" or "openai"
	StatusCode int
	Message    string
	Err        error
}

func (e *UpstreamError) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("%s upstream: %s", e.Provider, e.Message)
	}
	return fmt.Sprintf("%s upstream: status %d: %s", e.Provider, e.StatusCode, e.Message)
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// Retryable reports whether the next entry in a fallback chain should be
// tried: rate limits, server errors, and transport failures/timeouts qualify.
func (e *UpstreamError) Retryable() bool {
	return e.StatusCode == 0 || e.StatusCode == 429 || e.StatusCode >= 500
}

// AsUpstreamError unwraps err to an *UpstreamError if there is one.
func AsUpstreamError(err error) (*UpstreamError, bool) {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// NewTransportError wraps a failure that never produced an HTTP response
// (connection refused, timeout, canceled). Always retryable.
func NewTransportError(providerType string, err error) *UpstreamError {
	msg := "request failed"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		msg = "upstream timeout"
	case errors.Is(err, context.Canceled):
		msg = "request canceled"
	}
	return &UpstreamError{Provider: providerType, StatusCode: 0, Message: msg, Err: err}
}

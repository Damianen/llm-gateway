package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// sseWriter writes server-sent events with a flush after every event so
// deltas reach the client immediately.
type sseWriter struct {
	w  http.ResponseWriter
	fl http.Flusher
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	fl, _ := w.(http.Flusher)
	s := &sseWriter{w: w, fl: fl}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // defeat reverse-proxy buffering
	w.WriteHeader(http.StatusOK)
	s.flush()
	return s
}

// event writes an optionally named event with a JSON payload.
func (s *sseWriter) event(name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal SSE payload: %w", err)
	}
	if name != "" {
		if _, err := io.WriteString(s.w, "event: "+name+"\n"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(s.w, "data: "+string(data)+"\n\n"); err != nil {
		return err
	}
	s.flush()
	return nil
}

// raw writes a literal data line (e.g. the OpenAI "[DONE]" sentinel).
func (s *sseWriter) raw(data string) error {
	if _, err := io.WriteString(s.w, "data: "+data+"\n\n"); err != nil {
		return err
	}
	s.flush()
	return nil
}

func (s *sseWriter) flush() {
	if s.fl != nil {
		s.fl.Flush()
	}
}

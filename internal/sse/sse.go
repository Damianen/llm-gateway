// Package sse provides a minimal server-sent-events scanner used by the
// outbound adapters to read upstream streams.
package sse

import (
	"bufio"
	"io"
	"strings"
)

// Event is one server-sent event.
type Event struct {
	Name string // the "event:" field; empty when the stream only uses data lines
	Data string // concatenated "data:" lines, joined with newlines
}

// Scanner reads events from an SSE byte stream.
type Scanner struct {
	r *bufio.Reader
}

// NewScanner wraps r for event reading.
func NewScanner(r io.Reader) *Scanner {
	return &Scanner{r: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns the next event, or io.EOF when the stream ends cleanly.
// Comment lines (":keepalive") and unknown fields are skipped per the SSE
// spec; a partial event truncated by EOF is discarded.
func (s *Scanner) Next() (*Event, error) {
	var ev Event
	var dataLines []string
	hasFields := false
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			if err == io.EOF && line == "" {
				return nil, io.EOF
			}
			if err == io.EOF {
				// Truncated final line without a blank-line terminator:
				// treat as end of stream.
				return nil, io.EOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if !hasFields {
				continue // leading blank lines between events
			}
			ev.Data = strings.Join(dataLines, "\n")
			return &ev, nil
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keepalive
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.Name = value
			hasFields = true
		case "data":
			dataLines = append(dataLines, value)
			hasFields = true
		default:
			// id, retry, unknown fields: ignored.
		}
	}
}

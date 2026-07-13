package sse

import (
	"io"
	"strings"
	"testing"
)

func TestScanner(t *testing.T) {
	in := strings.Join([]string{
		": keepalive comment",
		"",
		"event: message_start",
		"data: {\"a\":1}",
		"",
		"data: line one",
		"data: line two",
		"",
		"id: 42",
		"retry: 100",
		"data: after-ignored-fields",
		"",
		"data: [DONE]",
		"",
		"",
	}, "\n")

	sc := NewScanner(strings.NewReader(in))

	ev, err := sc.Next()
	if err != nil || ev.Name != "message_start" || ev.Data != `{"a":1}` {
		t.Fatalf("event 1 = %+v, %v", ev, err)
	}
	ev, err = sc.Next()
	if err != nil || ev.Name != "" || ev.Data != "line one\nline two" {
		t.Fatalf("event 2 = %+v, %v (multi-line data must join)", ev, err)
	}
	ev, err = sc.Next()
	if err != nil || ev.Data != "after-ignored-fields" {
		t.Fatalf("event 3 = %+v, %v", ev, err)
	}
	ev, err = sc.Next()
	if err != nil || ev.Data != "[DONE]" {
		t.Fatalf("event 4 = %+v, %v", ev, err)
	}
	if _, err = sc.Next(); err != io.EOF {
		t.Fatalf("want io.EOF at end, got %v", err)
	}
}

func TestScannerCRLFAndTruncation(t *testing.T) {
	sc := NewScanner(strings.NewReader("data: hello\r\n\r\ndata: truncated-no-terminator"))
	ev, err := sc.Next()
	if err != nil || ev.Data != "hello" {
		t.Fatalf("CRLF event = %+v, %v", ev, err)
	}
	if _, err := sc.Next(); err != io.EOF {
		t.Fatalf("truncated tail should yield io.EOF, got %v", err)
	}
}

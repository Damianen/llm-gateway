// Package testutil holds small helpers shared by tests.
package testutil

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// GoldenJSON compares got (a JSON document) against the golden file at path,
// ignoring formatting differences. Run tests with UPDATE_GOLDEN=1 to
// (re)write golden files.
func GoldenJSON(t *testing.T, got []byte, path string) {
	t.Helper()
	normGot := normalizeJSON(t, got)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create golden dir: %v", err)
		}
		if err := os.WriteFile(path, append(normGot, '\n'), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with UPDATE_GOLDEN=1 to create it)", path, err)
	}
	normWant := normalizeJSON(t, want)
	if !bytes.Equal(normGot, normWant) {
		t.Errorf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", path, normGot, normWant)
	}
}

// GoldenText compares got against the golden file at path, byte-for-byte.
// Run tests with UPDATE_GOLDEN=1 to (re)write golden files.
func GoldenText(t *testing.T, got string, path string) {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create golden dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with UPDATE_GOLDEN=1 to create it)", path, err)
	}
	if got != string(want) {
		t.Errorf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func normalizeJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("re-marshal JSON: %v", err)
	}
	return out
}

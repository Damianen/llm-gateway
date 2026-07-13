package cache

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Damianen/llm-gateway/internal/provider"
	"github.com/Damianen/llm-gateway/internal/store"
)

func testReq(temp float64) *provider.Request {
	return &provider.Request{
		Model:       "requested-alias",
		Temperature: &temp,
		Messages:    []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{provider.TextBlock("hi")}}},
	}
}

func TestKey(t *testing.T) {
	a := testReq(0.5)
	b := testReq(0.5)
	b.Stream = true
	if Key("sonnet", a) != Key("sonnet", b) {
		t.Error("stream flag must not affect the cache key")
	}

	c := testReq(0.7)
	if Key("sonnet", a) == Key("sonnet", c) {
		t.Error("different temperature must change the key")
	}

	// The resolved model name is hashed, not the requested alias, so aliases
	// share cache entries.
	if Key("sonnet", a) == Key("other-model", a) {
		t.Error("different resolved model must change the key")
	}
	if len(Key("sonnet", a)) != 64 {
		t.Errorf("key length = %d, want 64 hex chars", len(Key("sonnet", a)))
	}
}

func TestGetSetTTL(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	c := New(st, time.Minute, clock, nil)
	ctx := context.Background()

	key := Key("sonnet", testReq(0.5))
	if _, ok := c.Get(ctx, key); ok {
		t.Fatal("empty cache must miss")
	}

	resp := &provider.Response{
		ID: "msg_1", Model: "claude-sonnet-4-6",
		Blocks:       []provider.Block{provider.TextBlock("cached answer")},
		FinishReason: provider.FinishStop,
		Usage:        provider.Usage{InputTokens: 25, OutputTokens: 7},
	}
	c.Set(ctx, key, "sonnet", resp)

	got, ok := c.Get(ctx, key)
	if !ok || got.Text() != "cached answer" || got.Usage.InputTokens != 25 {
		t.Fatalf("Get = %+v, %v", got, ok)
	}

	now = now.Add(59 * time.Second)
	if _, ok := c.Get(ctx, key); !ok {
		t.Error("entry should still be fresh before TTL")
	}
	now = now.Add(2 * time.Second)
	if _, ok := c.Get(ctx, key); ok {
		t.Error("entry must expire after TTL")
	}
}

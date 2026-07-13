package store

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func closeTo(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gw.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen with applied migrations: %v", err)
	}
	s2.Close()
}

func TestProjects(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	p, err := s.CreateProject(ctx, "agents")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if p.ID == 0 || p.Name != "agents" || p.CreatedAt.IsZero() {
		t.Errorf("unexpected project: %+v", p)
	}

	if _, err := s.CreateProject(ctx, "agents"); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate project error = %v, want ErrConflict", err)
	}

	got, err := s.GetProjectByName(ctx, "agents")
	if err != nil || got.ID != p.ID {
		t.Errorf("GetProjectByName = %+v, %v", got, err)
	}
	if _, err := s.GetProjectByName(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing project error = %v, want ErrNotFound", err)
	}

	if _, err := s.CreateProject(ctx, "bots"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListProjects(ctx)
	if err != nil || len(list) != 2 {
		t.Errorf("ListProjects = %v, %v", list, err)
	}
}

func TestKeys(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, err := s.CreateProject(ctx, "agents")
	if err != nil {
		t.Fatal(err)
	}

	k, err := s.InsertKey(ctx, "hash-1", "ci key", p.ID, 10, 5000, true)
	if err != nil {
		t.Fatalf("InsertKey: %v", err)
	}
	if k.ProjectName != "agents" || k.RPM != 10 || k.TPM != 5000 || !k.CacheDefault || k.Revoked {
		t.Errorf("unexpected key: %+v", k)
	}

	if _, err := s.InsertKey(ctx, "hash-1", "dup", p.ID, 0, 0, false); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate hash error = %v, want ErrConflict", err)
	}

	got, err := s.GetKeyByHash(ctx, "hash-1")
	if err != nil || got.ID != k.ID {
		t.Fatalf("GetKeyByHash = %+v, %v", got, err)
	}
	if _, err := s.GetKeyByHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown hash error = %v, want ErrNotFound", err)
	}

	if err := s.RevokeKey(ctx, k.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	got, err = s.GetKeyByHash(ctx, "hash-1")
	if err != nil || !got.Revoked {
		t.Errorf("revoked key lookup = %+v, %v (want Revoked=true)", got, err)
	}
	if err := s.RevokeKey(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke unknown = %v, want ErrNotFound", err)
	}

	if _, err := s.InsertKey(ctx, "hash-2", "second", p.ID, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListKeys(ctx, "agents")
	if err != nil || len(keys) != 2 {
		t.Errorf("ListKeys(agents) = %d keys, %v", len(keys), err)
	}
	keys, err = s.ListKeys(ctx, "ghost")
	if err != nil || len(keys) != 0 {
		t.Errorf("ListKeys(ghost) = %d keys, %v", len(keys), err)
	}
}

func TestUsageAggregation(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p1, _ := s.CreateProject(ctx, "p1")
	p2, _ := s.CreateProject(ctx, "p2")
	k1, _ := s.InsertKey(ctx, "h1", "", p1.ID, 0, 0, false)
	k2, _ := s.InsertKey(ctx, "h2", "", p2.ID, 0, 0, false)

	day1 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)

	rows := []RequestLog{
		{Time: day1, ProjectID: p1.ID, KeyID: k1.ID, Endpoint: "chat_completions", Model: "sonnet", Provider: "anthropic", UpstreamModel: "claude", InputTokens: 100, OutputTokens: 10, CostUSD: 0.5, LatencyMS: 120, Status: 200},
		{Time: day1.Add(time.Hour), ProjectID: p1.ID, KeyID: k1.ID, Endpoint: "messages", Model: "sonnet", Provider: "openai", UpstreamModel: "or/claude", InputTokens: 50, OutputTokens: 5, CostUSD: 0.25, LatencyMS: 80, Status: 200, FallbackUsed: true},
		{Time: day1.Add(2 * time.Hour), ProjectID: p1.ID, KeyID: k1.ID, Endpoint: "chat_completions", Model: "sonnet", Provider: "anthropic", UpstreamModel: "claude", InputTokens: 0, OutputTokens: 0, CostUSD: 0, LatencyMS: 2, Status: 200, CacheHit: true},
		{Time: day2, ProjectID: p1.ID, KeyID: k1.ID, Endpoint: "chat_completions", Model: "fast", Provider: "anthropic", UpstreamModel: "haiku", InputTokens: 10, OutputTokens: 1, CostUSD: 0.01, LatencyMS: 40, Status: 200},
		{Time: day1, ProjectID: p2.ID, KeyID: k2.ID, Endpoint: "chat_completions", Model: "sonnet", Provider: "anthropic", UpstreamModel: "claude", InputTokens: 999, OutputTokens: 999, CostUSD: 9.99, LatencyMS: 10, Status: 200},
	}
	for i := range rows {
		if err := s.LogRequest(ctx, &rows[i]); err != nil {
			t.Fatalf("LogRequest[%d]: %v", i, err)
		}
	}

	from := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)

	byModel, totals, err := s.Usage(ctx, "p1", from, to, "model")
	if err != nil {
		t.Fatalf("Usage(model): %v", err)
	}
	if len(byModel) != 2 {
		t.Fatalf("groups = %+v, want 2 (fast, sonnet)", byModel)
	}
	fast, sonnet := byModel[0], byModel[1]
	if fast.Group != "fast" || fast.Requests != 1 || fast.InputTokens != 10 || !closeTo(fast.CostUSD, 0.01) {
		t.Errorf("fast row = %+v", fast)
	}
	if sonnet.Group != "sonnet" || sonnet.Requests != 3 || sonnet.InputTokens != 150 ||
		sonnet.OutputTokens != 15 || !closeTo(sonnet.CostUSD, 0.75) || sonnet.CacheHits != 1 || sonnet.Fallbacks != 1 {
		t.Errorf("sonnet row = %+v", sonnet)
	}
	if totals.Requests != 4 || totals.InputTokens != 160 || totals.OutputTokens != 16 ||
		!closeTo(totals.CostUSD, 0.76) || totals.CacheHits != 1 || totals.Fallbacks != 1 {
		t.Errorf("totals = %+v", totals)
	}

	byDay, _, err := s.Usage(ctx, "p1", from, to, "day")
	if err != nil {
		t.Fatalf("Usage(day): %v", err)
	}
	if len(byDay) != 2 || byDay[0].Group != "2026-07-01" || byDay[0].Requests != 3 ||
		byDay[1].Group != "2026-07-02" || byDay[1].Requests != 1 {
		t.Errorf("byDay = %+v", byDay)
	}

	// Window bounds: `to` is exclusive.
	_, windowTotals, err := s.Usage(ctx, "p1", day1, day2, "model")
	if err != nil {
		t.Fatal(err)
	}
	if windowTotals.Requests != 3 {
		t.Errorf("window totals = %+v, want 3 requests (day2 excluded)", windowTotals)
	}

	// No project filter: all projects included.
	_, allTotals, err := s.Usage(ctx, "", from, to, "model")
	if err != nil {
		t.Fatal(err)
	}
	if allTotals.Requests != 5 || !closeTo(allTotals.CostUSD, 10.75) {
		t.Errorf("all-projects totals = %+v", allTotals)
	}

	if _, _, err := s.Usage(ctx, "", from, to, "hour"); err == nil {
		t.Error("Usage with bad group_by should error")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	ttl := time.Minute

	if err := s.CacheSet(ctx, "k1", "sonnet", []byte(`{"a":1}`), now, ttl); err != nil {
		t.Fatalf("CacheSet: %v", err)
	}

	body, hit, err := s.CacheGet(ctx, "k1", now.Add(30*time.Second))
	if err != nil || !hit || string(body) != `{"a":1}` {
		t.Errorf("CacheGet fresh = %q, %v, %v", body, hit, err)
	}

	if _, hit, _ := s.CacheGet(ctx, "k1", now.Add(ttl)); hit {
		t.Error("CacheGet at TTL boundary should miss")
	}
	if _, hit, _ := s.CacheGet(ctx, "ghost", now); hit {
		t.Error("CacheGet unknown key should miss")
	}

	// Replacement updates body and TTL.
	if err := s.CacheSet(ctx, "k1", "sonnet", []byte(`{"a":2}`), now.Add(time.Hour), ttl); err != nil {
		t.Fatal(err)
	}
	body, hit, _ = s.CacheGet(ctx, "k1", now.Add(time.Hour+30*time.Second))
	if !hit || string(body) != `{"a":2}` {
		t.Errorf("CacheGet after replace = %q, %v", body, hit)
	}

	// Purge removes only expired rows.
	if err := s.CacheSet(ctx, "k2", "fast", []byte(`{}`), now, ttl); err != nil {
		t.Fatal(err)
	}
	n, err := s.CachePurgeExpired(ctx, now.Add(2*time.Minute))
	if err != nil || n != 1 {
		t.Errorf("CachePurgeExpired = %d, %v (want 1: k2 expired, k1 refreshed)", n, err)
	}
}

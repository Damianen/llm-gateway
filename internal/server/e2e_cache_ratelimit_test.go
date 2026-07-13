package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Damianen/llm-gateway/internal/upstreamfake"
)

func chatBody(model, text string) map[string]any {
	return map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": text}},
	}
}

func jsonBody(v any) (io.Reader, error) {
	raw, err := json.Marshal(v)
	return bytes.NewReader(raw), err
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// adminUsage calls GET /admin/usage with the admin token.
func (e *e2eEnv) adminUsage(t *testing.T, query string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.http.URL+"/admin/usage"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func TestE2ECacheHitMissAndTTL(t *testing.T) {
	e := newE2EEnv(t)
	hdr := map[string]string{"X-Gateway-Cache": "true"}

	// Miss: served upstream, costed normally.
	status, body1, _ := e.post(t, "/v1/chat/completions", chatBody("fast", "cache me"), hdr)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	row := e.lastRow(t)
	if row.CacheHit || row.InputTokens != 25 || row.CostUSD == 0 {
		t.Errorf("miss row = %+v", row)
	}

	// Hit: identical request, $0, zero tokens, upstream untouched.
	status, body2, _ := e.post(t, "/v1/chat/completions", chatBody("fast", "cache me"), hdr)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	row = e.lastRow(t)
	if !row.CacheHit || row.CostUSD != 0 || row.InputTokens != 0 || row.OutputTokens != 0 {
		t.Errorf("hit row = %+v", row)
	}
	if len(e.oai.Requests()) != 1 {
		t.Errorf("upstream calls = %d, want 1 (second request served from cache)", len(e.oai.Requests()))
	}
	c1 := body1["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"]
	c2 := body2["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"]
	if c1 != c2 {
		t.Errorf("cached content differs: %v vs %v", c1, c2)
	}

	// Different request body: miss.
	if _, _, _ = e.post(t, "/v1/chat/completions", chatBody("fast", "different"), hdr); len(e.oai.Requests()) != 2 {
		t.Errorf("different request must miss the cache")
	}

	// TTL expiry: after the configured 5m TTL passes, the entry is stale.
	e.clock.Advance(5*time.Minute + time.Second)
	if _, _, _ = e.post(t, "/v1/chat/completions", chatBody("fast", "cache me"), hdr); len(e.oai.Requests()) != 3 {
		t.Errorf("expired entry must be refetched upstream")
	}
	if e.lastRow(t).CacheHit {
		t.Error("expired entry must not count as a hit")
	}
}

func TestE2ECacheOptIn(t *testing.T) {
	e := newE2EEnv(t)

	// Without the header (and cache_default=false), nothing is cached.
	e.post(t, "/v1/chat/completions", chatBody("fast", "no cache"), nil)
	e.post(t, "/v1/chat/completions", chatBody("fast", "no cache"), nil)
	if len(e.oai.Requests()) != 2 {
		t.Errorf("upstream calls = %d, want 2 (caching is opt-in)", len(e.oai.Requests()))
	}

	// A key with cache_default=true caches without the header.
	cachingKey := e.newKey(t, 0, 0, true)
	hdr := map[string]string{"Authorization": "Bearer " + cachingKey}
	e.post(t, "/v1/chat/completions", chatBody("fast", "key default"), hdr)
	e.post(t, "/v1/chat/completions", chatBody("fast", "key default"), hdr)
	if len(e.oai.Requests()) != 3 {
		t.Errorf("upstream calls = %d, want 3 (cache_default key hits cache)", len(e.oai.Requests()))
	}
	if row := e.lastRow(t); !row.CacheHit {
		t.Errorf("row = %+v, want cache hit", row)
	}
}

func TestE2ECachedResponseReplaysAsStream(t *testing.T) {
	e := newE2EEnv(t)
	hdr := map[string]string{"X-Gateway-Cache": "true"}

	// Prime the cache with a non-streaming request.
	if status, _, _ := e.post(t, "/v1/chat/completions", chatBody("fast", "replay me"), hdr); status != http.StatusOK {
		t.Fatal("prime failed")
	}

	// The same request with stream=true is served from cache as a valid
	// stream: same events a live stream would carry, ending in [DONE].
	body := chatBody("fast", "replay me")
	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}
	raw, err := jsonBody(body)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, e.http.URL+"/v1/chat/completions", raw)
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("X-Gateway-Cache", "true")
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rawBody := readAll(t, resp.Body)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type = %q", resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(rawBody, "Hello from fake openai.") || !strings.Contains(rawBody, "[DONE]") {
		t.Errorf("replayed stream incomplete:\n%s", rawBody)
	}
	if !strings.Contains(rawBody, `"finish_reason":"stop"`) {
		t.Errorf("replayed stream missing finish chunk:\n%s", rawBody)
	}

	if len(e.oai.Requests()) != 1 {
		t.Errorf("upstream calls = %d, want 1 (stream served from cache)", len(e.oai.Requests()))
	}
	row := e.lastRow(t)
	if !row.CacheHit || !row.Stream || row.CostUSD != 0 {
		t.Errorf("row = %+v", row)
	}
}

func TestE2EStreamingRequestsDoNotWriteCache(t *testing.T) {
	e := newE2EEnv(t)
	hdr := map[string]string{"X-Gateway-Cache": "true"}

	body := chatBody("fast", "stream first")
	body["stream"] = true
	if status, _, _ := e.streamPost(t, "/v1/chat/completions", body); status != http.StatusOK {
		t.Fatal("stream failed")
	}
	// The identical non-streaming request must still go upstream: streams
	// never write cache entries.
	e.post(t, "/v1/chat/completions", chatBody("fast", "stream first"), hdr)
	if len(e.oai.Requests()) != 2 {
		t.Errorf("upstream calls = %d, want 2", len(e.oai.Requests()))
	}
}

func TestE2ERateLimitRPM(t *testing.T) {
	e := newE2EEnv(t)
	limited := e.newKey(t, 2, 0, false)
	hdr := map[string]string{"Authorization": "Bearer " + limited}

	for i := range 2 {
		if status, _, _ := e.post(t, "/v1/chat/completions", chatBody("fast", "r"+strconv.Itoa(i)), hdr); status != http.StatusOK {
			t.Fatalf("request %d should pass", i)
		}
	}

	// Third request: 429 with Retry-After, OpenAI error shape.
	rawReq, _ := jsonBody(chatBody("fast", "r3"))
	req, _ := http.NewRequest(http.MethodPost, e.http.URL+"/v1/chat/completions", rawReq)
	req.Header.Set("Authorization", "Bearer "+limited)
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || ra < 1 {
		t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(body, `"rate_limit_error"`) {
		t.Errorf("429 body = %s", body)
	}

	// Anthropic dialect gets its own error shape.
	rawReq2, _ := jsonBody(map[string]any{
		"model": "fast", "max_tokens": 10,
		"messages": []map[string]any{{"role": "user", "content": "x"}},
	})
	req2, _ := http.NewRequest(http.MethodPost, e.http.URL+"/v1/messages", rawReq2)
	req2.Header.Set("x-api-key", limited)
	resp2, err := e.http.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	body2 := readAll(t, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests || !strings.Contains(body2, `"type":"error"`) {
		t.Errorf("anthropic 429 = %d %s", resp2.StatusCode, body2)
	}

	// After the refill interval, requests pass again (rpm=2 -> 30s/token).
	e.clock.Advance(31 * time.Second)
	if status, _, _ := e.post(t, "/v1/chat/completions", chatBody("fast", "r4"), hdr); status != http.StatusOK {
		t.Errorf("request after refill = %d", status)
	}
}

func TestE2ERateLimitTPM(t *testing.T) {
	e := newE2EEnv(t)
	// tpm=10: the first request is admitted (balance positive), its 32
	// actual tokens push the balance negative, blocking the second.
	limited := e.newKey(t, 0, 10, false)
	hdr := map[string]string{"Authorization": "Bearer " + limited}

	if status, _, _ := e.post(t, "/v1/chat/completions", chatBody("fast", "t1"), hdr); status != http.StatusOK {
		t.Fatal("first request should pass")
	}
	if status, _, _ := e.post(t, "/v1/chat/completions", chatBody("fast", "t2"), hdr); status != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429 (TPM debt)", status)
	}
}

// A known sequence of requests must produce exact usage totals.
func TestE2EUsageAccountingExactTotals(t *testing.T) {
	e := newE2EEnv(t)
	hdr := map[string]string{"X-Gateway-Cache": "true"}

	// 1+2: sonnet plain (second one cached).
	e.post(t, "/v1/chat/completions", chatBody("sonnet", "same"), hdr)
	e.post(t, "/v1/chat/completions", chatBody("sonnet", "same"), hdr)
	// 3: sonnet with fallback to the openrouter entry (429 upstream).
	e.ant.Enqueue(upstreamfake.Step{Status: 429})
	e.post(t, "/v1/chat/completions", chatBody("sonnet", "fallback"), nil)
	// 4: fast.
	e.post(t, "/v1/chat/completions", chatBody("fast", "cheap"), nil)

	status, body := e.adminUsage(t, "?project=e2e&group_by=model")
	if status != http.StatusOK {
		t.Fatalf("usage status = %d", status)
	}
	totals := body["totals"].(map[string]any)
	wantCost := 2*(25*3.0/1e6+7*15.0/1e6) /* sonnet upstream x2 (one direct, one fallback at same price) */ +
		(25*1.0/1e6 + 7*5.0/1e6) /* fast */
	if got := totals["requests"].(float64); got != 4 {
		t.Errorf("requests = %v", got)
	}
	if got := totals["input_tokens"].(float64); got != 75 { // 3 upstream-served x 25; cache hit contributes 0
		t.Errorf("input_tokens = %v", got)
	}
	if got := totals["output_tokens"].(float64); got != 21 {
		t.Errorf("output_tokens = %v", got)
	}
	if got := totals["cost_usd"].(float64); !costCloseTo(got, wantCost) {
		t.Errorf("cost = %v, want %v", got, wantCost)
	}
	if got := totals["cache_hits"].(float64); got != 1 {
		t.Errorf("cache_hits = %v", got)
	}
	if got := totals["fallbacks"].(float64); got != 1 {
		t.Errorf("fallbacks = %v", got)
	}

	groups := body["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("groups = %v", groups)
	}
	fast := groups[0].(map[string]any)
	sonnet := groups[1].(map[string]any)
	if fast["group"] != "fast" || fast["requests"].(float64) != 1 {
		t.Errorf("fast group = %v", fast)
	}
	if sonnet["group"] != "sonnet" || sonnet["requests"].(float64) != 3 || sonnet["cache_hits"].(float64) != 1 || sonnet["fallbacks"].(float64) != 1 {
		t.Errorf("sonnet group = %v", sonnet)
	}
}

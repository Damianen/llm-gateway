package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Damianen/llm-gateway/internal/upstreamfake"
)

func TestE2EMetricsExposition(t *testing.T) {
	e := newE2EEnv(t)

	// One plain request, one cache hit, one fallback, one rate-limited.
	hdr := map[string]string{"X-Gateway-Cache": "true"}
	e.post(t, "/v1/chat/completions", chatBody("fast", "m"), hdr)
	e.post(t, "/v1/chat/completions", chatBody("fast", "m"), hdr)
	e.ant.Enqueue(upstreamfake.Step{Status: 429})
	e.post(t, "/v1/chat/completions", chatBody("sonnet", "fb"), nil)
	limited := e.newKey(t, 1, 0, false)
	lhdr := map[string]string{"Authorization": "Bearer " + limited}
	e.post(t, "/v1/chat/completions", chatBody("fast", "one"), lhdr)
	e.post(t, "/v1/chat/completions", chatBody("fast", "two"), lhdr) // 429

	resp, err := e.http.Client().Get(e.http.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d", resp.StatusCode)
	}
	body := readAll(t, resp.Body)

	wantLines := []string{
		// 2 upstream-served fast requests (one per key) + rate-limited 429.
		`gateway_requests_total{model="fast",project="e2e",provider="openai",status="200"} 3`,
		`gateway_requests_total{model="fast",project="e2e",provider="openai",status="429"} 1`,
		// The fallback request served by the openai entry.
		`gateway_requests_total{model="sonnet",project="e2e",provider="openai",status="200"} 1`,
		`gateway_fallbacks_total{model="sonnet",provider="openai"} 1`,
		`gateway_cache_hits_total{model="fast",project="e2e"} 1`,
		// Tokens: 3 upstream-served requests x 25 in / 7 out; cache hit adds none.
		`gateway_tokens_total{direction="input",model="fast",project="e2e",provider="openai"} 50`,
		`gateway_tokens_total{direction="output",model="fast",project="e2e",provider="openai"} 14`,
		`gateway_cost_usd_total{model="fast",project="e2e",provider="openai"}`,
		`gateway_request_duration_seconds_bucket{model="fast",project="e2e",provider="openai",status="200",le="+Inf"} 3`,
	}
	for _, want := range wantLines {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

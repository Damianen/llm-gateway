package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Damianen/llm-gateway/internal/config"
	"github.com/Damianen/llm-gateway/internal/store"
)

const testAdminToken = "test-admin-token-do-not-log"

type testEnv struct {
	srv    *Server
	store  *store.Store
	http   *httptest.Server
	logBuf *bytes.Buffer
}

func newTestEnv(t *testing.T, adminToken string) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gw.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	srv := New(Options{
		Config:     &config.Config{},
		Logger:     logger,
		Store:      st,
		AdminToken: adminToken,
		Version:    "test",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{srv: srv, store: st, http: ts, logBuf: &logBuf}
}

// adminReq performs an admin API call and returns status + parsed body.
func (e *testEnv) adminReq(t *testing.T, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.http.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("response %q is not JSON: %v", raw, err)
		}
	}
	return resp.StatusCode, parsed
}

func TestAdminAuthGuard(t *testing.T) {
	e := newTestEnv(t, testAdminToken)

	status, _ := e.adminReq(t, "GET", "/admin/projects", "", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", status)
	}
	status, _ = e.adminReq(t, "GET", "/admin/projects", "wrong-token", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", status)
	}
	status, _ = e.adminReq(t, "GET", "/admin/projects", testAdminToken, nil)
	if status != http.StatusOK {
		t.Errorf("valid token: status = %d, want 200", status)
	}
}

func TestAdminDisabledWithoutToken(t *testing.T) {
	e := newTestEnv(t, "")
	status, body := e.adminReq(t, "GET", "/admin/projects", "anything", nil)
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body=%v", status, body)
	}
}

func TestAdminProjectAndKeyFlow(t *testing.T) {
	e := newTestEnv(t, testAdminToken)

	status, body := e.adminReq(t, "POST", "/admin/projects", testAdminToken, map[string]any{"name": "agents"})
	if status != http.StatusCreated || body["name"] != "agents" {
		t.Fatalf("create project: %d %v", status, body)
	}
	status, _ = e.adminReq(t, "POST", "/admin/projects", testAdminToken, map[string]any{"name": "agents"})
	if status != http.StatusConflict {
		t.Errorf("duplicate project: status = %d, want 409", status)
	}
	status, _ = e.adminReq(t, "POST", "/admin/projects", testAdminToken, map[string]any{"name": ""})
	if status != http.StatusBadRequest {
		t.Errorf("empty name: status = %d, want 400", status)
	}

	status, body = e.adminReq(t, "POST", "/admin/keys", testAdminToken,
		map[string]any{"project": "agents", "name": "ci", "rpm": 10, "tpm": 5000, "cache_default": true})
	if status != http.StatusCreated {
		t.Fatalf("create key: %d %v", status, body)
	}
	plaintext, _ := body["key"].(string)
	if !strings.HasPrefix(plaintext, "sk-gw-") {
		t.Fatalf("key = %q, want sk-gw- prefix", plaintext)
	}
	details, _ := body["details"].(map[string]any)
	if details["project"] != "agents" || details["rpm"].(float64) != 10 || details["cache_default"] != true {
		t.Errorf("key details = %v", details)
	}
	keyID := int64(details["id"].(float64))

	status, _ = e.adminReq(t, "POST", "/admin/keys", testAdminToken, map[string]any{"project": "ghost"})
	if status != http.StatusNotFound {
		t.Errorf("key for missing project: status = %d, want 404", status)
	}

	// Listing keys must never expose plaintext or hashes.
	req, _ := http.NewRequest("GET", e.http.URL+"/admin/keys?project=agents", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rawList, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list keys: %d %s", resp.StatusCode, rawList)
	}
	if strings.Contains(string(rawList), "sk-gw-") || strings.Contains(string(rawList), "key_hash") {
		t.Errorf("key listing leaks secrets: %s", rawList)
	}

	// Revoke flow.
	status, body = e.adminReq(t, "POST", fmt.Sprintf("/admin/keys/%d/revoke", keyID), testAdminToken, nil)
	if status != http.StatusOK || body["revoked"] != true {
		t.Errorf("revoke: %d %v", status, body)
	}
	status, _ = e.adminReq(t, "POST", "/admin/keys/9999/revoke", testAdminToken, nil)
	if status != http.StatusNotFound {
		t.Errorf("revoke unknown: status = %d, want 404", status)
	}

	// The plaintext key and admin token must never appear in logs.
	logs := e.logBuf.String()
	if strings.Contains(logs, plaintext) {
		t.Error("plaintext virtual key leaked into logs")
	}
	if strings.Contains(logs, testAdminToken) {
		t.Error("admin token leaked into logs")
	}
}

func TestRequireKeyMiddleware(t *testing.T) {
	e := newTestEnv(t, testAdminToken)
	ctx := context.Background()

	p, err := e.store.CreateProject(ctx, "agents")
	if err != nil {
		t.Fatal(err)
	}
	_, body := e.adminReq(t, "POST", "/admin/keys", testAdminToken, map[string]any{"project": "agents"})
	plaintext := body["key"].(string)
	keyID := int64(body["details"].(map[string]any)["id"].(float64))

	var gotKey *store.APIKey
	probe := e.srv.requireKey(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = keyFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	do := func(path, authHeader, apiKeyHeader string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, nil)
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		if apiKeyHeader != "" {
			req.Header.Set("x-api-key", apiKeyHeader)
		}
		rec := httptest.NewRecorder()
		probe.ServeHTTP(rec, req)
		return rec
	}

	// Bearer works (OpenAI convention).
	if rec := do("/v1/chat/completions", "Bearer "+plaintext, ""); rec.Code != http.StatusOK {
		t.Errorf("bearer auth: %d %s", rec.Code, rec.Body)
	}
	if gotKey == nil || gotKey.ProjectID != p.ID {
		t.Errorf("context key = %+v", gotKey)
	}

	// x-api-key works (Anthropic convention).
	if rec := do("/v1/messages", "", plaintext); rec.Code != http.StatusOK {
		t.Errorf("x-api-key auth: %d %s", rec.Code, rec.Body)
	}

	// Missing key: dialect-shaped 401s.
	rec := do("/v1/chat/completions", "", "")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("openai 401 = %d %s", rec.Code, rec.Body)
	}
	rec = do("/v1/messages", "", "")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"type":"error"`) ||
		!strings.Contains(rec.Body.String(), "authentication_error") {
		t.Errorf("anthropic 401 = %d %s", rec.Code, rec.Body)
	}

	// Wrong key rejected without echoing it.
	rec = do("/v1/chat/completions", "Bearer sk-gw-totally-wrong", "")
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "sk-gw-totally-wrong") {
		t.Errorf("wrong key = %d %s", rec.Code, rec.Body)
	}

	// Revoked key rejected.
	if err := e.store.RevokeKey(ctx, keyID); err != nil {
		t.Fatal(err)
	}
	if rec := do("/v1/chat/completions", "Bearer "+plaintext, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked key: %d %s", rec.Code, rec.Body)
	}

	if strings.Contains(e.logBuf.String(), plaintext) {
		t.Error("plaintext key leaked into logs")
	}
}

func TestAdminUsageEndpoint(t *testing.T) {
	e := newTestEnv(t, testAdminToken)
	ctx := context.Background()
	p, _ := e.store.CreateProject(ctx, "agents")
	k, _ := e.store.InsertKey(ctx, "h1", "", p.ID, 0, 0, false)

	now := time.Now().UTC()
	for i, row := range []store.RequestLog{
		{Time: now, ProjectID: p.ID, KeyID: k.ID, Endpoint: "chat_completions", Model: "sonnet", Provider: "anthropic", UpstreamModel: "claude", InputTokens: 100, OutputTokens: 20, CostUSD: 0.6, LatencyMS: 100, Status: 200},
		{Time: now, ProjectID: p.ID, KeyID: k.ID, Endpoint: "messages", Model: "sonnet", Provider: "anthropic", UpstreamModel: "claude", InputTokens: 40, OutputTokens: 10, CostUSD: 0.27, LatencyMS: 90, Status: 200},
	} {
		if err := e.store.LogRequest(ctx, &row); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	status, body := e.adminReq(t, "GET", "/admin/usage?project=agents&group_by=model", testAdminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("usage: %d %v", status, body)
	}
	totals := body["totals"].(map[string]any)
	if totals["requests"].(float64) != 2 || totals["input_tokens"].(float64) != 140 ||
		totals["output_tokens"].(float64) != 30 {
		t.Errorf("totals = %v", totals)
	}
	groups := body["groups"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["group"] != "sonnet" {
		t.Errorf("groups = %v", groups)
	}

	status, _ = e.adminReq(t, "GET", "/admin/usage?group_by=hour", testAdminToken, nil)
	if status != http.StatusBadRequest {
		t.Errorf("bad group_by: status = %d, want 400", status)
	}
	status, _ = e.adminReq(t, "GET", "/admin/usage?from=yesterday", testAdminToken, nil)
	if status != http.StatusBadRequest {
		t.Errorf("bad from: status = %d, want 400", status)
	}
}

func TestHealthz(t *testing.T) {
	e := newTestEnv(t, "")
	resp, err := e.http.Client().Get(e.http.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Gateway-Request-Id") == "" {
		t.Error("missing X-Gateway-Request-Id header")
	}
}

package store

import (
	"context"
	"fmt"
	"time"
)

// RequestLog is one row of the per-request usage/cost log.
type RequestLog struct {
	Time          time.Time
	ProjectID     int64
	KeyID         int64
	Endpoint      string // inbound surface: "chat_completions" or "messages"
	Model         string // gateway-facing model name (after alias resolution)
	Provider      string // provider type that actually served the request
	UpstreamModel string // upstream model id actually used
	InputTokens   int
	OutputTokens  int
	CostUSD       float64
	LatencyMS     int64
	Status        int
	CacheHit      bool
	FallbackUsed  bool
	Stream        bool
}

// LogRequest appends one request row.
func (s *Store) LogRequest(ctx context.Context, r *RequestLog) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO requests
		 (ts, project_id, key_id, endpoint, model, provider, upstream_model,
		  input_tokens, output_tokens, cost_usd, latency_ms, status,
		  cache_hit, fallback_used, stream)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		FormatTime(r.Time), r.ProjectID, r.KeyID, r.Endpoint, r.Model, r.Provider, r.UpstreamModel,
		r.InputTokens, r.OutputTokens, r.CostUSD, r.LatencyMS, r.Status,
		boolToInt(r.CacheHit), boolToInt(r.FallbackUsed), boolToInt(r.Stream))
	if err != nil {
		return fmt.Errorf("log request: %w", err)
	}
	return nil
}

// LastRequest returns the most recently logged request row (highest id).
// Used by tests and diagnostics.
func (s *Store) LastRequest(ctx context.Context) (*RequestLog, error) {
	var r RequestLog
	var ts string
	var cacheHit, fallbackUsed, stream int
	err := s.db.QueryRowContext(ctx,
		`SELECT ts, project_id, key_id, endpoint, model, provider, upstream_model,
		        input_tokens, output_tokens, cost_usd, latency_ms, status,
		        cache_hit, fallback_used, stream
		 FROM requests ORDER BY id DESC LIMIT 1`,
	).Scan(&ts, &r.ProjectID, &r.KeyID, &r.Endpoint, &r.Model, &r.Provider, &r.UpstreamModel,
		&r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.LatencyMS, &r.Status,
		&cacheHit, &fallbackUsed, &stream)
	if err != nil {
		return nil, fmt.Errorf("last request: %w", err)
	}
	r.Time = parseTime(ts)
	r.CacheHit = cacheHit != 0
	r.FallbackUsed = fallbackUsed != 0
	r.Stream = stream != 0
	return &r, nil
}

// UsageRow is one aggregated usage bucket.
type UsageRow struct {
	Group        string  `json:"group"`
	Requests     int64   `json:"requests"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	CacheHits    int64   `json:"cache_hits"`
	Fallbacks    int64   `json:"fallbacks"`
}

// Usage aggregates the request log between from (inclusive) and to
// (exclusive), optionally filtered by project name. groupBy is "model" or
// "day"; rows are ordered by group key.
func (s *Store) Usage(ctx context.Context, projectName string, from, to time.Time, groupBy string) (rows []UsageRow, totals UsageRow, err error) {
	var groupExpr string
	switch groupBy {
	case "model":
		groupExpr = "r.model"
	case "day":
		groupExpr = "substr(r.ts, 1, 10)"
	default:
		return nil, totals, fmt.Errorf("usage: group_by %q is not one of model|day", groupBy)
	}

	q := `SELECT ` + groupExpr + ` AS grp,
	             COUNT(*),
	             COALESCE(SUM(r.input_tokens), 0),
	             COALESCE(SUM(r.output_tokens), 0),
	             COALESCE(SUM(r.cost_usd), 0),
	             COALESCE(SUM(r.cache_hit), 0),
	             COALESCE(SUM(r.fallback_used), 0)
	      FROM requests r JOIN projects p ON p.id = r.project_id
	      WHERE r.ts >= ? AND r.ts < ?`
	args := []any{FormatTime(from), FormatTime(to)}
	if projectName != "" {
		q += ` AND p.name = ?`
		args = append(args, projectName)
	}
	q += ` GROUP BY grp ORDER BY grp`

	res, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, totals, fmt.Errorf("usage query: %w", err)
	}
	defer res.Close()
	totals.Group = "total"
	for res.Next() {
		var u UsageRow
		if err := res.Scan(&u.Group, &u.Requests, &u.InputTokens, &u.OutputTokens, &u.CostUSD, &u.CacheHits, &u.Fallbacks); err != nil {
			return nil, totals, fmt.Errorf("scan usage: %w", err)
		}
		rows = append(rows, u)
		totals.Requests += u.Requests
		totals.InputTokens += u.InputTokens
		totals.OutputTokens += u.OutputTokens
		totals.CostUSD += u.CostUSD
		totals.CacheHits += u.CacheHits
		totals.Fallbacks += u.Fallbacks
	}
	return rows, totals, res.Err()
}

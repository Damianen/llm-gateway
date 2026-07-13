package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CacheGet returns the cached response body for key if present and not
// expired at now.
func (s *Store) CacheGet(ctx context.Context, key string, now time.Time) ([]byte, bool, error) {
	var body []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT response FROM cache WHERE key = ? AND expires_at > ?`,
		key, now.Unix(),
	).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cache get: %w", err)
	}
	return body, true, nil
}

// CacheSet stores (or replaces) a cached response body under key.
func (s *Store) CacheSet(ctx context.Context, key, model string, response []byte, now time.Time, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO cache (key, model, response, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		key, model, response, now.Unix(), now.Add(ttl).Unix())
	if err != nil {
		return fmt.Errorf("cache set: %w", err)
	}
	return nil
}

// CachePurgeExpired deletes rows whose TTL has passed and reports how many.
func (s *Store) CachePurgeExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM cache WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("cache purge: %w", err)
	}
	return res.RowsAffected()
}

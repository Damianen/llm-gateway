package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// APIKey is a gateway virtual key. Only the SHA-256 hash of the plaintext is
// stored; the hash is never serialized in API responses.
type APIKey struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	ProjectID    int64     `json:"project_id"`
	ProjectName  string    `json:"project"`
	RPM          int       `json:"rpm"`
	TPM          int       `json:"tpm"`
	CacheDefault bool      `json:"cache_default"`
	Revoked      bool      `json:"revoked"`
	CreatedAt    time.Time `json:"created_at"`
}

// InsertKey stores a new key hash for a project and returns the row.
func (s *Store) InsertKey(ctx context.Context, keyHash, name string, projectID int64, rpm, tpm int, cacheDefault bool) (*APIKey, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys (key_hash, name, project_id, rpm, tpm, cache_default, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		keyHash, name, projectID, rpm, tpm, boolToInt(cacheDefault), FormatTime(time.Now()))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("api key: %w", ErrConflict)
		}
		return nil, fmt.Errorf("insert key: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("insert key: %w", err)
	}
	return s.getKey(ctx, `k.id = ?`, id)
}

// GetKeyByHash resolves a presented key hash. Revoked keys are still returned
// (with Revoked set) so callers can distinguish and reject them.
func (s *Store) GetKeyByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	return s.getKey(ctx, `k.key_hash = ?`, keyHash)
}

func (s *Store) getKey(ctx context.Context, where string, arg any) (*APIKey, error) {
	var k APIKey
	var created string
	var cacheDefault, revoked int
	err := s.db.QueryRowContext(ctx,
		`SELECT k.id, k.name, k.project_id, p.name, k.rpm, k.tpm, k.cache_default, k.revoked, k.created_at
		 FROM api_keys k JOIN projects p ON p.id = k.project_id
		 WHERE `+where, arg,
	).Scan(&k.ID, &k.Name, &k.ProjectID, &k.ProjectName, &k.RPM, &k.TPM, &cacheDefault, &revoked, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("api key: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get key: %w", err)
	}
	k.CacheDefault = cacheDefault != 0
	k.Revoked = revoked != 0
	k.CreatedAt = parseTime(created)
	return &k, nil
}

// RevokeKey marks a key revoked. Returns ErrNotFound for unknown IDs.
func (s *Store) RevokeKey(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE api_keys SET revoked = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke key: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("api key %d: %w", id, ErrNotFound)
	}
	return nil
}

// ListKeys returns keys, optionally filtered by project name.
func (s *Store) ListKeys(ctx context.Context, projectName string) ([]APIKey, error) {
	q := `SELECT k.id, k.name, k.project_id, p.name, k.rpm, k.tpm, k.cache_default, k.revoked, k.created_at
	      FROM api_keys k JOIN projects p ON p.id = k.project_id`
	var args []any
	if projectName != "" {
		q += ` WHERE p.name = ?`
		args = append(args, projectName)
	}
	q += ` ORDER BY k.id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var created string
		var cacheDefault, revoked int
		if err := rows.Scan(&k.ID, &k.Name, &k.ProjectID, &k.ProjectName, &k.RPM, &k.TPM, &cacheDefault, &revoked, &created); err != nil {
			return nil, fmt.Errorf("scan key: %w", err)
		}
		k.CacheDefault = cacheDefault != 0
		k.Revoked = revoked != 0
		k.CreatedAt = parseTime(created)
		out = append(out, k)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

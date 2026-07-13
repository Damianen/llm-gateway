package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Project groups API keys and usage accounting.
type Project struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateProject inserts a new project. Returns ErrConflict if the name exists.
func (s *Store) CreateProject(ctx context.Context, name string) (*Project, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (name, created_at) VALUES (?, ?)`,
		name, FormatTime(time.Now()))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("project %q: %w", name, ErrConflict)
		}
		return nil, fmt.Errorf("create project: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create project: %w", err)
	}
	return s.getProject(ctx, `WHERE id = ?`, id)
}

// GetProjectByName looks a project up by name.
func (s *Store) GetProjectByName(ctx context.Context, name string) (*Project, error) {
	return s.getProject(ctx, `WHERE name = ?`, name)
}

func (s *Store) getProject(ctx context.Context, where string, arg any) (*Project, error) {
	var p Project
	var created string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, created_at FROM projects `+where, arg,
	).Scan(&p.ID, &p.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("project: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	p.CreatedAt = parseTime(created)
	return &p, nil
}

// ListProjects returns all projects ordered by name.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at FROM projects ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var created string
		if err := rows.Scan(&p.ID, &p.Name, &created); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		p.CreatedAt = parseTime(created)
		out = append(out, p)
	}
	return out, rows.Err()
}

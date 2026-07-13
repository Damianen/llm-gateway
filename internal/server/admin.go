package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Damianen/llm-gateway/internal/auth"
	"github.com/Damianen/llm-gateway/internal/store"
)

const maxAdminBody = 1 << 20 // 1 MiB

func decodeAdminBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeAdminBody(w, r, &req); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeAdminError(w, http.StatusBadRequest, "name is required")
		return
	}
	p, err := s.store.CreateProject(r.Context(), req.Name)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeAdminError(w, http.StatusConflict, fmt.Sprintf("project %q already exists", req.Name))
			return
		}
		s.adminInternalError(w, r, "create project", err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		s.adminInternalError(w, r, "list projects", err)
		return
	}
	if projects == nil {
		projects = []store.Project{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Project      string `json:"project"`
		Name         string `json:"name"`
		RPM          int    `json:"rpm"`
		TPM          int    `json:"tpm"`
		CacheDefault bool   `json:"cache_default"`
	}
	if err := decodeAdminBody(w, r, &req); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Project == "" {
		writeAdminError(w, http.StatusBadRequest, "project is required")
		return
	}
	if req.RPM < 0 || req.TPM < 0 {
		writeAdminError(w, http.StatusBadRequest, "rpm and tpm must be >= 0")
		return
	}
	project, err := s.store.GetProjectByName(r.Context(), req.Project)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAdminError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", req.Project))
			return
		}
		s.adminInternalError(w, r, "resolve project", err)
		return
	}
	plaintext, hash, err := auth.GenerateKey()
	if err != nil {
		s.adminInternalError(w, r, "generate key", err)
		return
	}
	key, err := s.store.InsertKey(r.Context(), hash, req.Name, project.ID, req.RPM, req.TPM, req.CacheDefault)
	if err != nil {
		s.adminInternalError(w, r, "insert key", err)
		return
	}
	// The plaintext key is returned exactly once, here; only its hash is stored.
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":     plaintext,
		"details": key,
	})
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListKeys(r.Context(), r.URL.Query().Get("project"))
	if err != nil {
		s.adminInternalError(w, r, "list keys", err)
		return
	}
	if keys == nil {
		keys = []store.APIKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "key id must be an integer")
		return
	}
	if err := s.store.RevokeKey(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAdminError(w, http.StatusNotFound, fmt.Sprintf("key %d not found", id))
			return
		}
		s.adminInternalError(w, r, "revoke key", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "revoked": true})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	groupBy := q.Get("group_by")
	if groupBy == "" {
		groupBy = "model"
	}
	if groupBy != "model" && groupBy != "day" {
		writeAdminError(w, http.StatusBadRequest, "group_by must be model or day")
		return
	}
	now := s.clock()
	from := time.Time{} // beginning of time
	to := now.Add(time.Minute)
	var err error
	if v := q.Get("from"); v != "" {
		if from, err = parseTimeParam(v); err != nil {
			writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("from: %v", err))
			return
		}
	}
	if v := q.Get("to"); v != "" {
		if to, err = parseTimeParam(v); err != nil {
			writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("to: %v", err))
			return
		}
	}
	rows, totals, err := s.store.Usage(r.Context(), q.Get("project"), from, to, groupBy)
	if err != nil {
		s.adminInternalError(w, r, "usage query", err)
		return
	}
	if rows == nil {
		rows = []store.UsageRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project":  q.Get("project"),
		"from":     from.UTC().Format(time.RFC3339),
		"to":       to.UTC().Format(time.RFC3339),
		"group_by": groupBy,
		"groups":   rows,
		"totals":   totals,
	})
}

// parseTimeParam accepts RFC3339 timestamps or bare dates (UTC midnight).
func parseTimeParam(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not RFC3339 or YYYY-MM-DD", v)
}

func (s *Server) adminInternalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	s.logger.Error("admin API error", "request_id", requestIDFromContext(r.Context()), "op", op, "err", err)
	writeAdminError(w, http.StatusInternalServerError, "internal error")
}

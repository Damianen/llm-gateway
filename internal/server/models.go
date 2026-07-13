package server

import "net/http"

// handleListModels serves GET /v1/models with the gateway's enabled model
// names, in OpenAI list format (the shape every client library expects).
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{}
	for _, name := range s.router.ModelNames() {
		data = append(data, map[string]any{
			"id":       name,
			"object":   "model",
			"created":  0,
			"owned_by": "llm-gateway",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

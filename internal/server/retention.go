package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/boxes-oss/boxes/internal/store"
)

const retentionConfirmation = "delete archived sandbox data"

func (s *Server) getRetention(w http.ResponseWriter, r *http.Request) {
	if currentUser(r).Role != "owner" {
		writeError(w, http.StatusForbidden, "owner_required", "only an owner can view retention settings")
		return
	}
	enabled, err := s.store.ZeroDataRetention(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": enabled})
}

func (s *Server) putRetention(w http.ResponseWriter, r *http.Request) {
	if currentUser(r).Role != "owner" {
		writeError(w, http.StatusForbidden, "owner_required", "only an owner can change retention settings")
		return
	}
	var input struct {
		Enabled bool   `json:"enabled"`
		Confirm string `json:"confirm"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid retention settings")
		return
	}
	if input.Enabled && strings.TrimSpace(input.Confirm) != retentionConfirmation {
		writeError(w, http.StatusBadRequest, "confirmation_required", "type the required confirmation phrase to enable zero data retention")
		return
	}
	if err := s.store.SetZeroDataRetention(r.Context(), input.Enabled); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": input.Enabled})
}

func (s *Server) queueRetentionDelete(ctx context.Context, record store.BoxRecord) {
	enabled, err := s.store.ZeroDataRetention(ctx)
	if err != nil || !enabled {
		return
	}
	go func() { _ = s.deleteDetachedSandbox(context.Background(), record) }()
}

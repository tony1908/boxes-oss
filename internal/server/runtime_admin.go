package server

import (
	"context"
	"net/http"
	"time"

	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
)

func (s *Server) stopRuntimeBoxesFromAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requireLocalAdmin(w, r) {
		return
	}
	boxes, err := s.store.ListBoxes(r.Context(), "")
	if err != nil {
		s.apiError(w, err)
		return
	}
	stopped := make([]string, 0)
	failed := make([]string, 0)
	for _, box := range boxes {
		if box.Box.State == types.StateStopped {
			continue
		}
		switch box.Box.State {
		case types.StateProvisioning, types.StateStopping, types.StateResuming, types.StateDeleting:
			failed = append(failed, box.Box.ID)
			continue
		}
		if err := s.stopRuntimeBox(r.Context(), box); err != nil {
			failed = append(failed, box.Box.ID)
			continue
		}
		stopped = append(stopped, box.Box.ID)
	}
	status := http.StatusOK
	if len(failed) > 0 {
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"stopped": stopped, "failed": failed})
}

func (s *Server) stopRuntimeBox(ctx context.Context, box store.BoxRecord) error {
	box, client, err := s.loadBoxAgent(ctx, box.Box.ID)
	if err != nil {
		return err
	}
	if err := s.waitSnapshot(ctx, box.Box.ID); err != nil {
		return err
	}
	defer s.endSnapshot(box.Box.ID)
	var response struct {
		Snapshot struct {
			SnapshotID string    `json:"snapshotId"`
			SizeBytes  int64     `json:"sizeBytes"`
			CreatedAt  time.Time `json:"createdAt"`
		} `json:"snapshot"`
	}
	if err := client.json(ctx, http.MethodPost, agentPath("v1", "boxes", box.Box.ID, "stop"), map[string]bool{"snapshot": true}, &response); err != nil {
		return err
	}
	if response.Snapshot.SnapshotID == "" {
		return errSnapshotRequired
	}
	now := time.Now().UTC()
	box.Box.State = types.StateStopped
	box.Box.StoppedAt = &now
	closeRunningInterval(&box, now)
	createdAt := response.Snapshot.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	if _, err := s.recordSnapshot(ctx, box.Box.ID, response.Snapshot.SnapshotID, response.Snapshot.SizeBytes, createdAt, s.storageConfig()); err != nil {
		return err
	}
	if err := s.store.UpdateBox(ctx, box); err != nil {
		return err
	}
	s.queueRetentionDelete(ctx, box)
	s.stopEventPump(box.Box.ID)
	s.emitWebhookEvent(context.Background(), "sandbox.stopped", map[string]any{"sandbox": box.Box})
	return nil
}

func (s *Server) resumeRuntimeBoxesFromAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requireLocalAdmin(w, r) {
		return
	}
	var input struct {
		BoxIDs []string `json:"boxIds"`
	}
	if err := decodeJSON(r, &input); err != nil || len(input.BoxIDs) > 4096 {
		writeError(w, http.StatusBadRequest, "invalid_request", "a valid boxIds list is required")
		return
	}
	resumed := make([]string, 0, len(input.BoxIDs))
	failed := make([]string, 0)
	seen := make(map[string]struct{}, len(input.BoxIDs))
	for _, id := range input.BoxIDs {
		if id == "" {
			failed = append(failed, id)
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		box, err := s.store.GetBox(r.Context(), id)
		if err != nil {
			failed = append(failed, id)
			continue
		}
		if box.Box.State == types.StateReady || box.Box.State == types.StateIdle {
			resumed = append(resumed, id)
			continue
		}
		if box.Box.State != types.StateStopped {
			failed = append(failed, id)
			continue
		}
		box, err = s.resumeBox(r.Context(), box, resumeSandboxRequest{})
		if err != nil || (box.Box.State != types.StateReady && box.Box.State != types.StateIdle) {
			failed = append(failed, id)
			continue
		}
		resumed = append(resumed, id)
	}
	status := http.StatusOK
	if len(failed) > 0 {
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"resumed": resumed, "failed": failed})
}

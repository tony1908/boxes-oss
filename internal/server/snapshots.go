package server

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/boxes-oss/boxes/internal/types"
)

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	snapshots, err := s.store.ListSnapshots(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	for left, right := 0, len(snapshots)-1; left < right; left, right = left+1, right-1 {
		snapshots[left], snapshots[right] = snapshots[right], snapshots[left]
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snapshots})
}

func (s *Server) latestSnapshot(w http.ResponseWriter, r *http.Request) {
	boxID := pathID(r)
	if _, err := s.store.GetBox(r.Context(), boxID); err != nil {
		s.apiError(w, err)
		return
	}
	snapshot, err := s.store.LatestSnapshot(r.Context(), boxID)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"snapshot": nil})
		return
	}
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot})
}

func (s *Server) listAllSnapshots(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			writeError(w, http.StatusBadRequest, "invalid_query", "limit must be between 1 and 100")
			return
		}
		limit = value
	}
	var beforeCreatedAt, beforeID string
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(cursor)
		var parsed struct {
			CreatedAt string `json:"createdAt"`
			ID        string `json:"id"`
		}
		if err != nil || json.Unmarshal(data, &parsed) != nil || parsed.CreatedAt == "" || parsed.ID == "" {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
			return
		}
		beforeCreatedAt, beforeID = parsed.CreatedAt, parsed.ID
	}
	page, err := s.store.ListSnapshotPage(r.Context(), beforeCreatedAt, beforeID, limit+1)
	if err != nil {
		s.apiError(w, err)
		return
	}
	nextCursor := ""
	if len(page) > limit {
		page = page[:limit]
		last := page[len(page)-1]
		data, _ := json.Marshal(map[string]string{"createdAt": last.CreatedAt.UTC().Format(time.RFC3339Nano), "id": last.ID})
		nextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": page, "nextCursor": nextCursor})
}

func (s *Server) downloadSnapshot(w http.ResponseWriter, r *http.Request) {
	s.proxySnapshot(w, r, "download")
}

func (s *Server) snapshotTree(w http.ResponseWriter, r *http.Request) {
	s.proxySnapshot(w, r, "tree")
}

func (s *Server) snapshotFile(w http.ResponseWriter, r *http.Request) {
	s.proxySnapshot(w, r, "file")
}

func (s *Server) proxySnapshot(w http.ResponseWriter, r *http.Request, action string) {
	snapshot, err := s.store.GetSnapshot(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	box, client, err := s.loadBoxAgent(r.Context(), snapshot.BoxID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "invalid query parameters")
		return
	}
	endpoint := agentPath("v1", "snapshots", snapshot.ResticID, action)
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	storageHeaders, err := s.snapshotStorageHeader(snapshot)
	if err != nil {
		s.apiError(w, err)
		return
	}
	response, err := client.rawWithHeaders(r.Context(), http.MethodGet, endpoint, nil, "", storageHeaders)
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent_unavailable", "agent request failed")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		writeError(w, response.StatusCode, "agent_error", "snapshot could not be read")
		return
	}
	for key, values := range response.Header {
		if strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Connection") || strings.EqualFold(key, "Transfer-Encoding") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
	for key, values := range response.Trailer {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	_ = box
}

func (s *Server) listNamedSnapshots(w http.ResponseWriter, r *http.Request) {
	snapshots, err := s.store.ListNamedSnapshots(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"namedSnapshots": snapshots})
}

func (s *Server) createNamedSnapshot(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name       string `json:"name"`
		BoxID      string `json:"boxId"`
		SnapshotID string `json:"snapshotId"`
	}
	if err := decodeJSON(r, &input); err != nil || strings.TrimSpace(input.Name) == "" || (input.SnapshotID == "" && input.BoxID == "") {
		writeError(w, http.StatusBadRequest, "invalid_request", "name and either boxId or snapshotId are required")
		return
	}
	if _, err := s.store.GetNamedSnapshot(r.Context(), strings.TrimSpace(input.Name)); err == nil {
		writeError(w, http.StatusConflict, "name_exists", "a named snapshot with this name already exists")
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		s.apiError(w, err)
		return
	}
	capturedBoxID := ""
	if input.BoxID != "" {
		if input.SnapshotID != "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "specify either boxId or snapshotId")
			return
		}
		box, client, err := s.loadBoxAgent(r.Context(), input.BoxID)
		if err != nil {
			s.apiError(w, err)
			return
		}
		if err := s.waitSnapshot(r.Context(), box.Box.ID); err != nil {
			writeError(w, http.StatusConflict, "snapshot_in_progress", "sandbox snapshot capture did not finish before the wait expired")
			return
		}
		defer s.endSnapshot(box.Box.ID)
		var response struct {
			SnapshotID string    `json:"snapshotId"`
			SizeBytes  int64     `json:"sizeBytes"`
			CreatedAt  time.Time `json:"createdAt"`
		}
		if err := client.json(r.Context(), http.MethodPost, agentPath("v1", "boxes", box.Box.ID, "snapshot"), nil, &response); err != nil {
			s.apiError(w, err)
			return
		}
		if response.SnapshotID == "" {
			writeError(w, http.StatusBadGateway, "snapshot_failed", "agent returned no snapshot identifier")
			return
		}
		if response.CreatedAt.IsZero() {
			response.CreatedAt = time.Now().UTC()
		}
		snapshot, err := s.recordSnapshot(r.Context(), box.Box.ID, response.SnapshotID, response.SizeBytes, response.CreatedAt, s.storageConfig())
		if err != nil {
			s.apiError(w, err)
			return
		}
		capturedBoxID = box.Box.ID
		s.recordActivity(r.Context(), box.Box.ID, "snapshot", "manual capture", map[string]any{"sizeBytes": response.SizeBytes}, "ok")
		input.SnapshotID = snapshot.ID
	}
	// Do not hold snapshotRefs while waiting for a per-box capture: periodic
	// capture pruning also takes snapshotRefs before it can release that slot.
	s.snapshotRefs.Lock()
	defer s.snapshotRefs.Unlock()
	if _, err := s.store.GetNamedSnapshot(r.Context(), strings.TrimSpace(input.Name)); err == nil {
		writeError(w, http.StatusConflict, "name_exists", "a named snapshot with this name already exists")
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		s.apiError(w, err)
		return
	}
	snapshot, err := s.store.GetSnapshot(r.Context(), input.SnapshotID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	named := types.NamedSnapshot{Name: strings.TrimSpace(input.Name), SnapshotID: input.SnapshotID, CreatedAt: time.Now().UTC()}
	if err := s.store.CreateNamedSnapshot(r.Context(), named); err != nil {
		writeError(w, http.StatusConflict, "name_exists", "a named snapshot with this name already exists")
		return
	}
	if capturedBoxID == "" {
		capturedBoxID = snapshot.BoxID
	}
	s.recordActivity(r.Context(), capturedBoxID, "snapshot", "named snapshot saved", map[string]any{"name": named.Name}, "ok")
	writeJSON(w, http.StatusCreated, map[string]any{"namedSnapshot": named})
}

func (s *Server) getNamedSnapshot(w http.ResponseWriter, r *http.Request) {
	named, err := s.store.GetNamedSnapshot(r.Context(), r.PathValue("name"))
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"namedSnapshot": named})
}

func (s *Server) deleteNamedSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteNamedSnapshot(r.Context(), r.PathValue("name")); err != nil {
		s.apiError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	box, err := s.store.GetBox(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	snapshot, err := s.store.GetSnapshot(r.Context(), r.PathValue("snapshotId"))
	if err != nil {
		s.apiError(w, err)
		return
	}
	if snapshot.BoxID != box.Box.ID {
		writeError(w, http.StatusNotFound, "not_found", "snapshot not found")
		return
	}
	named, err := s.store.ListNamedSnapshots(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	for _, pin := range named {
		if pin.SnapshotID == snapshot.ID {
			writeError(w, http.StatusConflict, "snapshot_pinned", "named snapshots cannot be deleted")
			return
		}
	}
	force := r.URL.Query().Get("force") == "true"
	snapshots, err := s.store.ListSnapshots(r.Context(), box.Box.ID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	if box.Box.State == types.StateStopped && len(snapshots) > 0 && snapshots[len(snapshots)-1].ID == snapshot.ID && !force {
		writeError(w, http.StatusConflict, "resume_snapshot", "the newest snapshot of a stopped sandbox is required to resume; pass force=true to delete it")
		return
	}
	client, ok := newAgentClient(s.hub, box.Box.NodeID)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "agent_unavailable", "snapshot agent is unavailable")
		return
	}
	headers, err := s.snapshotStorageHeader(snapshot)
	if err != nil {
		s.apiError(w, err)
		return
	}
	if err := client.jsonWithHeaders(r.Context(), http.MethodPost, agentPath("v1", "snapshots", "forget"), map[string][]string{"ids": {snapshot.ResticID}}, nil, headers); err != nil {
		writeError(w, http.StatusBadGateway, "snapshot_delete_failed", "agent could not forget the snapshot")
		return
	}
	if err := s.store.DeleteSnapshot(r.Context(), snapshot.ID); err != nil {
		s.apiError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

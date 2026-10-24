package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/tony1908/boxes-oss/internal/store"
	"github.com/tony1908/boxes-oss/internal/types"
)

// driveError is a client-facing drive or mount validation failure.
type driveError struct {
	status  int
	code    string
	message string
}

func (e *driveError) Error() string { return e.message }

// driveUse is one box's mount of a drive.
type driveUse struct {
	boxID    string
	path     string
	readOnly bool
	running  bool
}

// driveAgent returns the agent that owns drive volumes.
// ponytail: drives live on the single VM's Docker; multi-node needs per-node volumes and placement.
func (s *Server) driveAgent() (*agentClient, error) {
	nodes := s.hub.Connected()
	if len(nodes) == 0 {
		return nil, errors.New("local agent disconnected")
	}
	client, ok := newAgentClient(s.hub, nodes[0])
	if !ok {
		return nil, errors.New("local agent disconnected")
	}
	return client, nil
}

// driveUses maps each drive name to the boxes that mount it.
func (s *Server) driveUses(ctx context.Context) (map[string][]driveUse, error) {
	boxes, err := s.store.ListBoxes(ctx, "")
	if err != nil {
		return nil, err
	}
	uses := map[string][]driveUse{}
	for _, record := range boxes {
		for _, mount := range record.Box.Mounts {
			uses[mount.Drive] = append(uses[mount.Drive], driveUse{boxID: record.Box.ID, path: mount.Path,
				readOnly: mount.ReadOnly, running: record.Box.State != types.StateStopped})
		}
	}
	return uses, nil
}

// checkMounts validates a box's mounts: shape, existing drives, and one read-write mount per drive.
// boxID is the box being changed (empty for a new box) so its own mounts are not counted as conflicts.
func (s *Server) checkMounts(ctx context.Context, mounts []types.Mount, boxID string) error {
	if err := types.ValidateMounts(mounts); err != nil {
		return &driveError{http.StatusBadRequest, "invalid_mounts", err.Error()}
	}
	if len(mounts) == 0 {
		return nil
	}
	uses, err := s.driveUses(ctx)
	if err != nil {
		return err
	}
	readWrite := map[string]int{}
	for _, mount := range mounts {
		if _, err := s.store.GetDrive(ctx, mount.Drive); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &driveError{http.StatusBadRequest, "invalid_mounts", fmt.Sprintf("drive %q does not exist", mount.Drive)}
			}
			return err
		}
		if mount.ReadOnly {
			continue
		}
		readWrite[mount.Drive]++
		busy := readWrite[mount.Drive] > 1
		for _, use := range uses[mount.Drive] {
			busy = busy || (!use.readOnly && use.boxID != boxID)
		}
		if busy {
			return &driveError{http.StatusConflict, "drive_busy", fmt.Sprintf("drive %q is already mounted read-write", mount.Drive)}
		}
	}
	return nil
}

func (s *Server) writeDriveError(w http.ResponseWriter, err error) {
	var driveErr *driveError
	if errors.As(err, &driveErr) {
		writeError(w, driveErr.status, driveErr.code, driveErr.message)
		return
	}
	s.apiError(w, err)
}

func (s *Server) listDrives(w http.ResponseWriter, r *http.Request) {
	drives, err := s.store.ListDrives(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	uses, err := s.driveUses(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	type mountedBy struct {
		SandboxID string `json:"sandboxId"`
		Path      string `json:"path"`
		ReadOnly  bool   `json:"readOnly"`
	}
	type driveView struct {
		types.Drive
		MountedBy []mountedBy `json:"mountedBy"`
	}
	views := make([]driveView, 0, len(drives))
	for _, drive := range drives {
		view := driveView{Drive: drive, MountedBy: []mountedBy{}}
		for _, use := range uses[drive.Name] {
			view.MountedBy = append(view.MountedBy, mountedBy{SandboxID: use.boxID, Path: use.path, ReadOnly: use.readOnly})
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"drives": views})
}

func (s *Server) createDrive(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid drive request")
		return
	}
	if err := types.ValidateDriveName(input.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_drive", err.Error())
		return
	}
	if _, err := s.store.GetDrive(r.Context(), input.Name); err == nil {
		writeError(w, http.StatusConflict, "drive_exists", "drive already exists")
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		s.apiError(w, err)
		return
	}
	client, err := s.driveAgent()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "node_unavailable", "node is not connected")
		return
	}
	if err := client.json(r.Context(), http.MethodPut, agentPath("v1", "drives", input.Name), nil, nil); err != nil {
		writeError(w, agentErrorStatus(err), "drive_create_failed", withAgentError("drive could not be created", err))
		return
	}
	drive := types.Drive{Name: input.Name, CreatedAt: time.Now().UTC()}
	if err := s.store.CreateDrive(r.Context(), drive); err != nil {
		if errors.Is(err, store.ErrDriveExists) {
			writeError(w, http.StatusConflict, "drive_exists", "drive already exists")
			return
		}
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, drive)
}

func (s *Server) deleteDrive(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.store.GetDrive(r.Context(), name); err != nil {
		s.apiError(w, err)
		return
	}
	uses, err := s.driveUses(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	if len(uses[name]) > 0 {
		writeError(w, http.StatusConflict, "drive_in_use", "drive is mounted by a sandbox")
		return
	}
	client, err := s.driveAgent()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "node_unavailable", "node is not connected")
		return
	}
	if err := client.json(r.Context(), http.MethodDelete, agentPath("v1", "drives", name), nil, nil); err != nil {
		writeError(w, agentErrorStatus(err), "drive_delete_failed", withAgentError("drive could not be deleted", err))
		return
	}
	if err := s.store.DeleteDrive(r.Context(), name); err != nil {
		s.apiError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) forkDrive(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("name")
	var input struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid drive fork request")
		return
	}
	if err := types.ValidateDriveName(input.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_drive", err.Error())
		return
	}
	if _, err := s.store.GetDrive(r.Context(), source); err != nil {
		s.apiError(w, err)
		return
	}
	if _, err := s.store.GetDrive(r.Context(), input.Name); err == nil {
		writeError(w, http.StatusConflict, "drive_exists", "drive already exists")
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		s.apiError(w, err)
		return
	}
	uses, err := s.driveUses(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	for _, use := range uses[source] {
		if !use.readOnly && use.running {
			writeError(w, http.StatusConflict, "drive_busy", "drive is mounted read-write by a running sandbox")
			return
		}
	}
	client, err := s.driveAgent()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "node_unavailable", "node is not connected")
		return
	}
	payload := map[string]any{"target": input.Name, "image": s.defaultImages().Box}
	if err := client.json(r.Context(), http.MethodPost, agentPath("v1", "drives", source, "fork"), payload, nil); err != nil {
		writeError(w, agentErrorStatus(err), "drive_fork_failed", withAgentError("drive could not be copied", err))
		return
	}
	drive := types.Drive{Name: input.Name, CreatedAt: time.Now().UTC()}
	if err := s.store.CreateDrive(r.Context(), drive); err != nil {
		if errors.Is(err, store.ErrDriveExists) {
			writeError(w, http.StatusConflict, "drive_exists", "drive already exists")
			return
		}
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, drive)
}

// readOnlyMounts returns the mounts a fork keeps. Read-write mounts are dropped because a fork
// must not share a writable volume with its source.
func readOnlyMounts(mounts []types.Mount) []types.Mount {
	var kept []types.Mount
	for _, mount := range mounts {
		if mount.ReadOnly {
			kept = append(kept, mount)
		}
	}
	return kept
}

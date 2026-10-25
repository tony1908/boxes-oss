package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/tony1908/boxes-oss/internal/types"
)

type operationStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	s.operationMu.RLock()
	operation, ok := s.operations[r.PathValue("id")]
	s.operationMu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "operation not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"operation": operation})
}

func (s *Server) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	if box, err := s.store.GetBox(r.Context(), pathID(r)); err == nil {
		local := false
		if box.State == types.StateStopped {
			if node, nodeErr := s.store.GetNode(r.Context(), box.NodeID); nodeErr == nil {
				local = node.Node.Status == "offline"
			}
		}
		if local {
			s.deleteSandboxNow(w, r)
			return
		}
	}
	op := operationStatus{ID: types.NewID("op_"), Status: "pending"}
	s.operationMu.Lock()
	s.operations[op.ID] = op
	s.operationMu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": op})
	asyncRequest := r.WithContext(context.WithoutCancel(r.Context()))
	go func() {
		recorder := httptest.NewRecorder()
		s.deleteSandboxNow(recorder, asyncRequest)
		updated := op
		if recorder.Code >= 200 && recorder.Code < 300 {
			updated.Status = "done"
		} else {
			updated.Status = "failed"
			var payload struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(recorder.Body.Bytes(), &payload) == nil {
				updated.Error = payload.Error.Message
			}
			if updated.Error == "" {
				updated.Error = http.StatusText(recorder.Code)
			}
		}
		s.operationMu.Lock()
		s.operations[op.ID] = updated
		s.operationMu.Unlock()
	}()
}

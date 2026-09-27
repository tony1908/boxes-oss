package server

import (
	"net/http"
	"strings"

	"golang.org/x/crypto/ssh"
)

func (s *Server) addSSHKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		PublicKey string `json:"publicKey"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "publicKey is required")
		return
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(input.PublicKey))
	if err != nil || len(strings.Fields(input.PublicKey)) < 2 {
		writeError(w, http.StatusBadRequest, "invalid_ssh_key", "publicKey must be a valid authorized key")
		return
	}
	boxID := pathID(r)
	client, err := s.boxAgent(r.Context(), boxID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	request := map[string]any{"cmd": []string{"sh", "-c", "mkdir -p /config/.ssh && chmod 700 /config/.ssh && printf '%s\\n' \"$1\" >> /config/.ssh/authorized_keys && chmod 600 /config/.ssh/authorized_keys", "boxes", canonical}}
	if err := client.json(r.Context(), http.MethodPost, agentPath("v1", "boxes", boxID, "exec"), request, nil); err != nil {
		s.apiError(w, err)
		return
	}
	s.recordActivity(r.Context(), boxID, "access", "SSH key added", map[string]any{"keyType": key.Type()}, "ok")
	w.WriteHeader(http.StatusNoContent)
}

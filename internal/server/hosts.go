package server

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/boxes-oss/boxes/internal/store"
)

func (s *Server) listHosts(w http.ResponseWriter, r *http.Request) {
	box, err := s.store.GetBox(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	node, err := s.store.GetNode(r.Context(), box.Box.NodeID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	base := nodeHostBase(node.Node)
	if base == "" {
		writeError(w, http.StatusConflict, "node_host_missing", "node requires host or hostBase to expose ports")
		return
	}
	hosts, err := s.store.ListHosts(r.Context(), box.Box.ID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	items := make([]map[string]any, 0, len(hosts))
	for _, host := range hosts {
		items = append(items, map[string]any{"port": host.Port, "private": host.Private, "url": hostURLFor(box.Box.ID, host.Port, base)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": items})
}

func (s *Server) createHost(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Port    int  `json:"port"`
		Private bool `json:"private"`
	}
	if err := decodeJSON(r, &input); err != nil || input.Port < 1 || input.Port > 65535 {
		writeError(w, http.StatusBadRequest, "invalid_request", "port must be between 1 and 65535")
		return
	}
	box, client, err := s.loadBoxAgent(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	node, err := s.store.GetNode(r.Context(), box.Box.NodeID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	base := nodeHostBase(node.Node)
	if base == "" {
		writeError(w, http.StatusConflict, "node_host_missing", "node requires host or hostBase to expose ports")
		return
	}
	var rawToken string
	var tokenHashValue string
	if input.Private {
		rawToken, err = randomToken(32)
		if err != nil {
			s.apiError(w, err)
			return
		}
		tokenHashValue = tokenHash(rawToken)
	}
	host := store.HostRecord{BoxID: box.Box.ID, Port: input.Port, Private: input.Private, TokenHash: tokenHashValue}
	if err := s.store.CreateHost(r.Context(), host); err != nil {
		writeError(w, http.StatusConflict, "host_exists", "this port is already exposed")
		return
	}
	if err := s.pushCurrentBoxRoutes(r.Context(), client, box.Box); err != nil {
		_ = s.store.DeleteHost(r.Context(), box.Box.ID, input.Port)
		s.apiError(w, err)
		return
	}
	s.recordActivity(r.Context(), box.Box.ID, "access", "host exposed", map[string]any{"port": input.Port, "public": !input.Private, "private": input.Private}, "ok")
	publicURL := hostURLFor(box.Box.ID, input.Port, base)
	if rawToken != "" {
		parsed, _ := url.Parse(publicURL)
		query := parsed.Query()
		query.Set("token", rawToken)
		parsed.RawQuery = query.Encode()
		publicURL = parsed.String()
	}
	writeJSON(w, http.StatusCreated, map[string]any{"host": map[string]any{"port": input.Port, "private": input.Private, "url": publicURL}})
}

func (s *Server) deleteHost(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeError(w, http.StatusBadRequest, "invalid_port", "port must be between 1 and 65535")
		return
	}
	box, client, err := s.loadBoxAgent(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	private := false
	if hosts, listErr := s.store.ListHosts(r.Context(), box.Box.ID); listErr == nil {
		for _, host := range hosts {
			if host.Port == port {
				private = host.Private
				break
			}
		}
	}
	if err := s.store.DeleteHost(r.Context(), box.Box.ID, port); err != nil {
		s.apiError(w, err)
		return
	}
	s.recordActivity(r.Context(), box.Box.ID, "access", "host removed", map[string]any{"port": port, "public": !private, "private": private}, "ok")
	if err := s.reconcileCurrentBoxRoutes(r.Context(), client, box.Box); err != nil {
		s.apiError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

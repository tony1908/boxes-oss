package server

import (
	"net/http"
	"sort"
	"strings"

	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
)

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.ListNodes(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	result := make([]types.Node, 0, len(nodes))
	for _, node := range nodes {
		if h, ok := s.getHealth(node.Node.ID); ok {
			node.Node.MemAvailableBytes = h.Health.MemAvailableBytes
			node.Node.SwapFreeBytes = h.Health.SwapFreeBytes
			node.Node.MemoryPressureSomeAvg10 = h.Health.MemoryPressureSomeAvg10
		}
		result = append(result, node.Node)
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": result})
}

func (s *Server) createNode(w http.ResponseWriter, r *http.Request) {
	if currentUser(r).Role != "owner" {
		writeError(w, http.StatusForbidden, "owner_required", "only an owner can install nodes")
		return
	}
	var input struct {
		Name     string `json:"name"`
		Host     string `json:"host"`
		HostBase string `json:"hostBase"`
	}
	if err := decodeJSON(r, &input); err != nil || strings.TrimSpace(input.Name) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "node name is required")
		return
	}
	joinToken, err := randomToken(32)
	if err != nil {
		s.apiError(w, err)
		return
	}
	node := types.Node{ID: types.NewID("nd_"), Name: strings.TrimSpace(input.Name), Host: input.Host, HostBase: input.HostBase, Status: "offline"}
	if err := s.store.CreateNode(r.Context(), store.NodeRecord{Node: node, TokenHash: tokenHash(joinToken)}); err != nil {
		s.apiError(w, err)
		return
	}
	publicURL := strings.TrimRight(s.cfg.PublicURL, "/")
	tunnelURL := publicURL
	switch {
	case strings.HasPrefix(publicURL, "https://"):
		tunnelURL = "wss://" + strings.TrimPrefix(publicURL, "https://")
	case strings.HasPrefix(publicURL, "http://"):
		tunnelURL = "ws://" + strings.TrimPrefix(publicURL, "http://")
	}
	tunnelURL += "/api/v1/agent/connect"
	installCommand := nodeInstallCommand(publicURL, publicURL+"/install/node-install.sh", tunnelURL, joinToken, s.storageConfig(), s.registryConfig(), s.defaultImages())
	writeJSON(w, http.StatusCreated, map[string]any{"node": node, "joinToken": joinToken, "installCommand": installCommand})
}

func nodeInstallCommand(publicURL, scriptURL, tunnelURL, token string, storage storageConfig, registry registryConfig, images defaultImageConfig) string {
	environment, args := nodeInstallEnvironment(publicURL, tunnelURL, token, storage, registry, images)
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	command := "curl -fsSL " + shellQuote(scriptURL) + " | sudo env"
	for _, key := range keys {
		command += " " + key + "=" + shellQuote(environment[key])
	}
	command += " bash -s --"
	for _, arg := range args {
		command += " " + shellQuote(arg)
	}
	return command
}

func nodeInstallEnvironment(publicURL, tunnelURL, token string, storage storageConfig, registry registryConfig, images defaultImageConfig) (map[string]string, []string) {
	environment := map[string]string{"BOXES_PUBLIC_URL": strings.TrimRight(publicURL, "/"), "BOXES_SERVER_URL": tunnelURL, "BOXES_NODE_TOKEN": token}
	if images.Box != "" {
		environment["BOXES_BOX_IMAGE"] = images.Box
	}
	if images.Tools != "" {
		environment["BOXES_TOOLS_IMAGE"] = images.Tools
	}
	if registry.URL != "" {
		environment["BOXES_REGISTRY_URL"] = registry.URL
		environment["BOXES_REGISTRY_USERNAME"] = registry.Username
		environment["BOXES_REGISTRY_PASSWORD"] = registry.Password
	}
	args := []string{}
	if storage.Repository != "" {
		environment["RESTIC_REPOSITORY"] = storage.Repository
	}
	if password := storage.Env["RESTIC_PASSWORD"]; password != "" {
		environment["RESTIC_PASSWORD"] = password
	}
	if storage.Type == "s3" {
		environment["BOXES_S3_ACCESS_KEY"] = storage.Env["AWS_ACCESS_KEY_ID"]
		environment["BOXES_S3_SECRET_KEY"] = storage.Env["AWS_SECRET_ACCESS_KEY"]
		args = append(args, "--s3-endpoint", storage.Endpoint, "--s3-bucket", storage.Bucket)
		if storage.Prefix != "" {
			args = append(args, "--s3-prefix", storage.Prefix)
		}
		if storage.Region != "" {
			args = append(args, "--s3-region", storage.Region)
		}
	}
	return environment, args
}

func (s *Server) drainNode(w http.ResponseWriter, r *http.Request) {
	node, err := s.store.GetNode(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	node.Node.Drained = true
	if err := s.store.UpdateNode(r.Context(), node); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": node.Node})
}

func (s *Server) undrainNode(w http.ResponseWriter, r *http.Request) {
	if currentUser(r).Role != "owner" {
		writeError(w, http.StatusForbidden, "owner_required", "only an owner can undrain nodes")
		return
	}
	node, err := s.store.GetNode(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	node.Node.Drained = false
	if err := s.store.UpdateNode(r.Context(), node); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": node.Node})
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteNode(r.Context(), pathID(r)); err != nil {
		s.apiError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

package server

import (
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/boxes-oss/boxes/install"
	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
	"golang.org/x/crypto/ssh"
)

func (s *Server) addNodeSSH(w http.ResponseWriter, r *http.Request) {
	if currentUser(r).Role != "owner" {
		writeError(w, http.StatusForbidden, "owner_required", "only an owner can install nodes")
		return
	}
	var input struct {
		Name       string `json:"name"`
		Host       string `json:"host"`
		Port       int    `json:"port"`
		User       string `json:"user"`
		PrivateKey string `json:"privateKey"`
	}
	if err := decodeJSON(r, &input); err != nil || strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Host) == "" || strings.TrimSpace(input.User) == "" || input.PrivateKey == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name, host, user, and privateKey are required")
		return
	}
	if input.Port == 0 {
		input.Port = 22
	}
	if input.Port < 1 || input.Port > 65535 {
		writeError(w, http.StatusBadRequest, "invalid_port", "port must be between 1 and 65535")
		return
	}
	key, err := ssh.ParsePrivateKey([]byte(input.PrivateKey))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_private_key", "privateKey is not a supported SSH private key")
		return
	}
	joinToken, err := randomToken(32)
	if err != nil {
		s.apiError(w, err)
		return
	}
	node := types.Node{ID: types.NewID("nd_"), Name: strings.TrimSpace(input.Name), Host: strings.TrimSpace(input.Host), Status: "offline"}
	if err := s.store.CreateNode(r.Context(), store.NodeRecord{Node: node, TokenHash: tokenHash(joinToken)}); err != nil {
		s.apiError(w, err)
		return
	}
	address := net.JoinHostPort(strings.Trim(input.Host, "[]"), strconv.Itoa(input.Port))
	config := &ssh.ClientConfig{User: input.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 15 * time.Second}
	client, err := ssh.Dial("tcp", address, config)
	if err != nil {
		_ = s.store.DeleteNode(r.Context(), node.ID)
		writeError(w, http.StatusBadGateway, "ssh_connect_failed", "could not connect to node over SSH")
		return
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		_ = s.store.DeleteNode(r.Context(), node.ID)
		writeError(w, http.StatusBadGateway, "ssh_install_failed", "could not start remote installer")
		return
	}
	defer session.Close()
	session.Stdin = strings.NewReader(string(install.NodeScript))
	session.Stdout, session.Stderr = io.Discard, io.Discard
	storage := s.storageConfig()
	publicURL := strings.TrimRight(s.cfg.PublicURL, "/")
	environment, args := nodeInstallEnvironment(publicURL, publicURL, joinToken, storage, s.registryConfig(), s.defaultImages())
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	command := "sudo env"
	for _, key := range keys {
		command += " " + key + "=" + shellQuote(environment[key])
	}
	command += " bash -s --"
	for _, arg := range args {
		command += " " + shellQuote(arg)
	}
	if err := session.Run(command); err != nil {
		_ = s.store.DeleteNode(r.Context(), node.ID)
		writeError(w, http.StatusBadGateway, "ssh_install_failed", "node installation failed")
		return
	}
	installCommand := "ssh " + shellQuote(input.User+"@"+input.Host) + " 'sudo bash -s'"
	writeJSON(w, http.StatusCreated, map[string]any{"node": node, "joinToken": joinToken, "installCommand": installCommand})
}

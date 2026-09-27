package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/boxes-oss/boxes/internal/provider"
	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
)

func (s *Server) reportPoolBake(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	s.bakeReportMu.Lock()
	want := s.bakeReportTokens[id]
	s.bakeReportMu.Unlock()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(tokenHash(token))) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid_bake_token", "invalid or expired bake token")
		return
	}
	var report struct {
		BoxDigest   string `json:"boxDigest"`
		ToolsDigest string `json:"toolsDigest"`
	}
	if err := decodeJSON(r, &report); err != nil || !validRepoDigest(report.BoxDigest) || (report.ToolsDigest != "" && !validRepoDigest(report.ToolsDigest)) {
		writeError(w, http.StatusBadRequest, "invalid_bake_report", "valid boxDigest and toolsDigest are required")
		return
	}
	pool, err := s.store.GetPool(r.Context(), id)
	if err != nil {
		s.apiError(w, err)
		return
	}
	pool.Pool.BakedBoxDigest = report.BoxDigest
	pool.Pool.BakedToolsDigest = report.ToolsDigest
	if err := s.store.UpdatePool(r.Context(), pool); err != nil {
		s.apiError(w, err)
		return
	}
	s.bakeReportMu.Lock()
	delete(s.bakeReportTokens, id)
	s.bakeReportMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func validRepoDigest(value string) bool {
	parts := strings.SplitN(value, "@sha256:", 2)
	if len(parts) != 2 || parts[0] == "" || len(parts[1]) != 64 {
		return false
	}
	return regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(parts[1])
}

func (s *Server) bakePool(w http.ResponseWriter, r *http.Request) {
	if !s.requirePoolOwner(w, r) {
		return
	}
	pool, err := s.store.GetPool(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	if pool.Pool.BakeStatus == "baking" {
		writeError(w, http.StatusConflict, "bake_in_progress", "pool bake is already running")
		return
	}
	pool.Pool.BakeStatus = "baking"
	if err = s.store.UpdatePool(r.Context(), pool); err != nil {
		s.apiError(w, err)
		return
	}
	go func() {
		if err := s.bakePoolImage(s.lifecycleCtx, pool); err != nil {
			log.Printf("bake pool %s failed: %v", pool.Pool.ID, err)
			latest, e := s.store.GetPool(context.Background(), pool.Pool.ID)
			if e == nil {
				latest.Pool.BakeStatus = "none"
				_ = s.store.UpdatePool(context.Background(), latest)
			}
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"pool": publicPool(pool)})
}

type poolInput struct {
	Name               string   `json:"name"`
	Provider           string   `json:"provider"`
	Credentials        string   `json:"credentials"`
	Region             string   `json:"region"`
	Size               string   `json:"size"`
	Image              string   `json:"image"`
	SSHKeys            []string `json:"sshKeys"`
	MinNodes           *int     `json:"minNodes"`
	MaxNodes           *int     `json:"maxNodes"`
	IdleMinutes        *int     `json:"idleMinutes"`
	BootTimeoutMinutes *int     `json:"bootTimeoutMinutes"`
	HeadroomSlots      *int     `json:"headroomSlots"`
	WarmSchedule       *string  `json:"warmSchedule"`
	AutoRebake         *bool    `json:"autoRebake"`
}

func (s *Server) listPools(w http.ResponseWriter, r *http.Request) {
	if !s.requirePoolOwner(w, r) {
		return
	}
	pools, err := s.store.ListPools(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	result := make([]types.Pool, 0, len(pools))
	for _, record := range pools {
		result = append(result, publicPool(record))
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": result})
}

func (s *Server) getPool(w http.ResponseWriter, r *http.Request) {
	if !s.requirePoolOwner(w, r) {
		return
	}
	record, err := s.store.GetPool(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pool": publicPool(record)})
}

func (s *Server) createPool(w http.ResponseWriter, r *http.Request) {
	if !s.requirePoolOwner(w, r) {
		return
	}
	var input poolInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid pool configuration")
		return
	}
	pool, credentials, err := normalizePoolInput(input, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_pool", err.Error())
		return
	}
	encrypted, err := s.secrets.encrypt([]byte(credentials))
	if err != nil {
		s.apiError(w, err)
		return
	}
	pool.ID = types.NewID("pl_")
	pool.CreatedAt = time.Now().UTC()
	if err := s.store.CreatePool(r.Context(), store.PoolRecord{Pool: pool, CredentialsEnc: encrypted}); err != nil {
		s.apiError(w, err)
		return
	}
	// Warm nodes are requested asynchronously so a slow provider never delays pool CRUD.
	if pool.MinNodes > 0 {
		go s.ensurePoolMinimum(s.lifecycleCtx, pool.ID)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"pool": publicPool(store.PoolRecord{Pool: pool, CredentialsEnc: encrypted})})
}

func (s *Server) updatePool(w http.ResponseWriter, r *http.Request) {
	if !s.requirePoolOwner(w, r) {
		return
	}
	current, err := s.store.GetPool(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	var input poolInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid pool configuration")
		return
	}
	pool, credentials, err := normalizePoolInput(input, &current.Pool)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_pool", err.Error())
		return
	}
	credentialsEnc := current.CredentialsEnc
	if credentials != "" {
		credentialsEnc, err = s.secrets.encrypt([]byte(credentials))
		if err != nil {
			s.apiError(w, err)
			return
		}
	}
	pool.ID, pool.CreatedAt = current.Pool.ID, current.Pool.CreatedAt
	if err := s.store.UpdatePool(r.Context(), store.PoolRecord{Pool: pool, CredentialsEnc: credentialsEnc}); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pool": publicPool(store.PoolRecord{Pool: pool, CredentialsEnc: credentialsEnc})})
}

func (s *Server) deletePool(w http.ResponseWriter, r *http.Request) {
	if !s.requirePoolOwner(w, r) {
		return
	}
	pool, err := s.store.GetPool(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	nodes, err := s.store.ListNodes(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	for _, node := range nodes {
		if node.Node.PoolID != pool.Pool.ID {
			continue
		}
		boxes, err := s.store.ListBoxes(r.Context(), node.Node.ID)
		if err != nil {
			s.apiError(w, err)
			return
		}
		if len(boxes) != 0 {
			writeError(w, http.StatusConflict, "pool_in_use", "pool nodes still have sandboxes; delete those sandboxes before removing the pool")
			return
		}
	}
	cloud, err := s.poolProvider(pool)
	if err != nil {
		s.apiError(w, err)
		return
	}
	instances, err := cloud.List(r.Context(), poolTag(pool.Pool.ID))
	if err != nil {
		s.apiError(w, err)
		return
	}
	for _, instance := range instances {
		if err := cloud.Delete(r.Context(), instance.ID); err != nil && !errors.Is(err, provider.ErrNotFound) {
			s.apiError(w, err)
			return
		}
	}
	for _, node := range nodes {
		if node.Node.PoolID == pool.Pool.ID {
			if err := s.store.DeleteNode(r.Context(), node.Node.ID); err != nil {
				s.apiError(w, err)
				return
			}
		}
	}
	if pool.Pool.BakedImageID != "" {
		if err := cloud.DeleteImage(r.Context(), pool.Pool.BakedImageID); err != nil {
			log.Printf("delete baked image %s for pool %s failed: %v", pool.Pool.BakedImageID, pool.Pool.ID, err)
		}
	}
	if err := s.store.DeletePool(r.Context(), pool.Pool.ID); err != nil {
		s.apiError(w, err)
		return
	}
	s.cleanupDetachedNodes(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requirePoolOwner(w http.ResponseWriter, r *http.Request) bool {
	if currentUser(r).Role == "owner" {
		return true
	}
	writeError(w, http.StatusForbidden, "owner_required", "only an owner can manage autoscaling pools")
	return false
}

func publicPool(record store.PoolRecord) types.Pool {
	pool := record.Pool
	pool.HasCredentials = len(record.CredentialsEnc) != 0
	return pool
}

func normalizePoolInput(input poolInput, current *types.Pool) (types.Pool, string, error) {
	pool := types.Pool{SSHKeys: input.SSHKeys, MaxNodes: 1, IdleMinutes: 10, BootTimeoutMinutes: 15}
	if current != nil {
		pool = *current
	}
	if input.Name != "" {
		pool.Name = strings.TrimSpace(input.Name)
	}
	if input.Provider != "" {
		pool.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	}
	if input.Region != "" {
		pool.Region = strings.TrimSpace(input.Region)
	}
	if input.Size != "" {
		pool.Size = strings.TrimSpace(input.Size)
	}
	if input.Image != "" {
		pool.Image = strings.TrimSpace(input.Image)
	}
	if input.SSHKeys != nil {
		pool.SSHKeys = input.SSHKeys
	}
	if input.MinNodes != nil {
		pool.MinNodes = *input.MinNodes
	}
	if input.MaxNodes != nil {
		pool.MaxNodes = *input.MaxNodes
	}
	if input.IdleMinutes != nil {
		pool.IdleMinutes = *input.IdleMinutes
	}
	if input.BootTimeoutMinutes != nil {
		pool.BootTimeoutMinutes = *input.BootTimeoutMinutes
	}
	if input.HeadroomSlots != nil {
		pool.HeadroomSlots = *input.HeadroomSlots
	}
	if input.WarmSchedule != nil {
		pool.WarmSchedule = strings.TrimSpace(*input.WarmSchedule)
	}
	if input.AutoRebake != nil {
		pool.AutoRebake = *input.AutoRebake
	}
	if pool.IdleMinutes == 0 {
		pool.IdleMinutes = 10
	}
	if pool.BootTimeoutMinutes == 0 {
		pool.BootTimeoutMinutes = 15
	}
	if pool.Name == "" || pool.Region == "" || pool.Size == "" || pool.Image == "" {
		return types.Pool{}, "", errors.New("name, region, size, and image are required")
	}
	switch pool.Provider {
	case "digitalocean", "hetzner", "script":
	default:
		return types.Pool{}, "", errors.New("provider must be digitalocean, hetzner, or script")
	}
	if pool.MinNodes < 0 || pool.MaxNodes < 1 || pool.MinNodes > pool.MaxNodes || pool.MaxNodes > 100 || pool.HeadroomSlots < 0 || pool.HeadroomSlots > 10000 {
		return types.Pool{}, "", errors.New("node limits must satisfy 0 <= minNodes <= maxNodes <= 100")
	}
	if pool.IdleMinutes < 1 || pool.IdleMinutes > 10080 || pool.BootTimeoutMinutes < 1 || pool.BootTimeoutMinutes > 1440 {
		return types.Pool{}, "", errors.New("idle and boot timeout values are out of range")
	}
	if _, err := parseWarmSchedule(pool.WarmSchedule); err != nil {
		return types.Pool{}, "", err
	}
	credentials := strings.TrimSpace(input.Credentials)
	if credentials == "" && current == nil {
		return types.Pool{}, "", errors.New("provider credentials are required")
	}
	if strings.ContainsAny(credentials, "\x00\r\n") {
		return types.Pool{}, "", errors.New("provider credentials contain invalid control characters")
	}
	return pool, credentials, nil
}

func (s *Server) poolProvider(record store.PoolRecord) (provider.Provider, error) {
	plain, err := s.secrets.decrypt(record.CredentialsEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt provider credentials: %w", err)
	}
	name, credentials := record.Pool.Provider, string(plain)
	if s.cfg.ProviderFactory != nil {
		cloud := s.cfg.ProviderFactory(name, credentials)
		if cloud == nil {
			return nil, fmt.Errorf("provider %q is not configured", name)
		}
		return cloud, nil
	}
	switch name {
	case "digitalocean":
		return &provider.DigitalOcean{Token: credentials}, nil
	case "hetzner":
		return &provider.Hetzner{Token: credentials}, nil
	case "script":
		return provider.Script{Executable: credentials}, nil
	default:
		return nil, fmt.Errorf("unsupported provider %q", name)
	}
}

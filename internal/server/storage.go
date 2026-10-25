package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/tony1908/boxes-oss/internal/types"
)

const storageSettingKey = "snapshot_storage"

type StorageBootstrap struct {
	Repository string
	Endpoint   string
	Bucket     string
	Prefix     string
	Region     string
	AccessKey  string
	SecretKey  string
	Password   string
}

type storageConfig struct {
	Type       string            `json:"type"`
	Repository string            `json:"repository"`
	Endpoint   string            `json:"endpoint,omitempty"`
	Bucket     string            `json:"bucket,omitempty"`
	Prefix     string            `json:"prefix,omitempty"`
	Region     string            `json:"region,omitempty"`
	Env        map[string]string `json:"-"`
}

type storedStorageConfig struct {
	Type       string `json:"type"`
	Repository string `json:"repository"`
	Endpoint   string `json:"endpoint,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
	Region     string `json:"region,omitempty"`
	EnvEnc     []byte `json:"envEnc"`
}

func (s *Server) initializeStorage(bootstrap StorageBootstrap) error {
	data, err := s.store.GetSetting(context.Background(), storageSettingKey)
	if err == nil {
		var stored storedStorageConfig
		if err := json.Unmarshal(data, &stored); err != nil {
			return fmt.Errorf("decode snapshot storage settings: %w", err)
		}
		plain, err := s.secrets.decrypt(stored.EnvEnc)
		if err != nil {
			return fmt.Errorf("decrypt snapshot storage settings: %w", err)
		}
		config := storageConfig{Type: stored.Type, Repository: stored.Repository, Endpoint: stored.Endpoint,
			Bucket: stored.Bucket, Prefix: stored.Prefix, Region: stored.Region}
		if err := json.Unmarshal(plain, &config.Env); err != nil {
			return fmt.Errorf("decode snapshot storage credentials: %w", err)
		}
		s.storageMu.Lock()
		s.storage = config
		s.storageMu.Unlock()
		return s.backfillSnapshotStorage(context.Background(), config)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load snapshot storage settings: %w", err)
	}
	config := storageConfig{Type: "local", Repository: bootstrap.Repository, Env: make(map[string]string)}
	if config.Repository == "" {
		config.Repository = "/var/lib/boxes/restic"
	}
	if bootstrap.Password != "" {
		config.Env["RESTIC_PASSWORD"] = bootstrap.Password
	} else {
		password, err := randomToken(32)
		if err != nil {
			return fmt.Errorf("generate snapshot storage password: %w", err)
		}
		config.Env["RESTIC_PASSWORD"] = password
	}
	if bootstrap.Endpoint != "" || bootstrap.Bucket != "" {
		var err error
		config, err = makeS3Storage(bootstrap.Endpoint, bootstrap.Bucket, bootstrap.Prefix, bootstrap.Region,
			bootstrap.AccessKey, bootstrap.SecretKey, config.Env["RESTIC_PASSWORD"])
		if err != nil {
			return fmt.Errorf("bootstrap S3 snapshot storage: %w", err)
		}
	}
	if err := s.saveStorage(context.Background(), config); err != nil {
		return err
	}
	return s.backfillSnapshotStorage(context.Background(), config)
}

func (s *Server) backfillSnapshotStorage(ctx context.Context, config storageConfig) error {
	plain, err := json.Marshal(config.Env)
	if err != nil {
		return err
	}
	encrypted, err := s.secrets.encrypt(plain)
	if err != nil {
		return err
	}
	return s.store.BackfillSnapshotStorage(ctx, config.Repository, encrypted)
}

func (s *Server) storageConfig() storageConfig {
	s.storageMu.RLock()
	defer s.storageMu.RUnlock()
	return cloneStorage(s.storage)
}

func cloneStorage(config storageConfig) storageConfig {
	copy := config
	copy.Env = make(map[string]string, len(config.Env))
	for key, value := range config.Env {
		copy.Env[key] = value
	}
	return copy
}

func (s *Server) saveStorage(ctx context.Context, config storageConfig) error {
	if config.Env == nil {
		config.Env = make(map[string]string)
	}
	plain, err := json.Marshal(config.Env)
	if err != nil {
		return fmt.Errorf("encode snapshot storage credentials: %w", err)
	}
	encrypted, err := s.secrets.encrypt(plain)
	if err != nil {
		return err
	}
	stored, err := json.Marshal(storedStorageConfig{Type: config.Type, Repository: config.Repository,
		Endpoint: config.Endpoint, Bucket: config.Bucket, Prefix: config.Prefix, Region: config.Region, EnvEnc: encrypted})
	if err != nil {
		return err
	}
	if err := s.store.SetSetting(ctx, storageSettingKey, stored); err != nil {
		return err
	}
	s.storageMu.Lock()
	s.storage = cloneStorage(config)
	s.storageMu.Unlock()
	return nil
}

func makeS3Storage(endpoint, bucket, prefix, region, accessKey, secretKey, password string) (storageConfig, error) {
	if accessKey == "" || secretKey == "" {
		return storageConfig{}, errors.New("BOXES_S3_ACCESS_KEY and BOXES_S3_SECRET_KEY are required for S3 storage")
	}
	if strings.ContainsAny(accessKey+secretKey, "\r\n\x00") {
		return storageConfig{}, errors.New("S3 credentials contain an invalid value")
	}
	if strings.ContainsAny(region, "\r\n\x00") {
		return storageConfig{}, errors.New("S3 region contains an invalid value")
	}
	repository, err := s3Repository(endpoint, bucket, prefix)
	if err != nil {
		return storageConfig{}, err
	}
	if strings.Trim(prefix, "/") == "" {
		prefix = "boxes"
		repository, err = s3Repository(endpoint, bucket, prefix)
		if err != nil {
			return storageConfig{}, err
		}
	}
	config := storageConfig{Type: "s3", Repository: repository, Endpoint: strings.TrimRight(endpoint, "/"),
		Bucket: bucket, Prefix: strings.Trim(prefix, "/"), Region: region, Env: map[string]string{
			"AWS_ACCESS_KEY_ID": accessKey, "AWS_SECRET_ACCESS_KEY": secretKey, "RESTIC_PASSWORD": password,
		}}
	if region != "" {
		config.Env["AWS_DEFAULT_REGION"] = region
	}
	return config, nil
}

func s3Repository(endpoint, bucket, prefix string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return "", errors.New("S3 endpoint must be an http(s) URL origin")
	}
	if bucket == "" || strings.ContainsAny(bucket, "/\\\x00") || bucket == "." || bucket == ".." {
		return "", errors.New("S3 bucket must be a single non-empty path segment")
	}
	prefix = strings.Trim(prefix, "/")
	if strings.ContainsRune(prefix, '\x00') || prefix == ".." || strings.HasPrefix(path.Clean(prefix), "../") {
		return "", errors.New("S3 prefix must be a relative path")
	}
	base := "s3:" + strings.TrimRight(endpoint, "/") + "/" + bucket
	if prefix != "" && prefix != "." {
		base += "/" + path.Clean(prefix)
	}
	return base, nil
}

func (s *Server) storageSettings(w http.ResponseWriter, r *http.Request) {
	if currentUser(r).Role != "owner" {
		writeError(w, http.StatusForbidden, "owner_required", "only an owner can view snapshot storage settings")
		return
	}
	config := s.storageConfig()
	writeJSON(w, http.StatusOK, map[string]any{"type": config.Type, "endpoint": config.Endpoint, "bucket": config.Bucket, "prefix": config.Prefix})
}

func (s *Server) setStorageFromAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requireLocalAdmin(w, r) {
		return
	}
	var input struct {
		Endpoint  string `json:"endpoint"`
		Bucket    string `json:"bucket"`
		Prefix    string `json:"prefix"`
		Region    string `json:"region"`
		AccessKey string `json:"accessKey"`
		SecretKey string `json:"secretKey"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid S3 storage configuration")
		return
	}
	previous := s.storageConfig()
	password := previous.Env["RESTIC_PASSWORD"]
	config, err := makeS3Storage(input.Endpoint, input.Bucket, input.Prefix, input.Region, input.AccessKey, input.SecretKey, password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_storage", err.Error())
		return
	}
	if err := s.pushStorageToConnectedAgents(r.Context(), config); err != nil {
		_ = s.pushStorageToConnectedAgents(context.Background(), previous)
		writeError(w, http.StatusBadGateway, "storage_sync_failed", "connected nodes could not switch snapshot storage")
		return
	}
	if err := s.saveStorage(r.Context(), config); err != nil {
		_ = s.pushStorageToConnectedAgents(context.Background(), previous)
		s.apiError(w, err)
		return
	}
	go s.captureRunningBoxes(config)
	writeJSON(w, http.StatusOK, map[string]any{"type": config.Type, "endpoint": config.Endpoint, "bucket": config.Bucket, "prefix": config.Prefix})
}

func (s *Server) pushStorageToConnectedAgents(ctx context.Context, config storageConfig) error {
	var firstErr error
	for _, nodeID := range s.hub.Connected() {
		client, ok := newAgentClient(s.hub, nodeID)
		if !ok {
			continue
		}
		if err := client.json(ctx, http.MethodPut, "/v1/storage", map[string]any{"repository": config.Repository, "env": config.Env}, nil); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("node %s did not accept snapshot storage settings", nodeID)
			}
		}
	}
	return firstErr
}

func (s *Server) captureRunningBoxes(config storageConfig) {
	ctx := context.Background()
	boxes, err := s.store.ListBoxes(ctx, "")
	if err != nil {
		return
	}
	for _, box := range boxes {
		if box.State != types.StateReady && box.State != types.StateIdle {
			continue
		}
		client, ok := newAgentClient(s.hub, box.NodeID)
		if !ok {
			continue
		}
		if s.waitSnapshot(ctx, box.ID) != nil {
			continue
		}
		var result struct {
			SnapshotID string    `json:"snapshotId"`
			SizeBytes  int64     `json:"sizeBytes"`
			CreatedAt  time.Time `json:"createdAt"`
		}
		err := client.json(ctx, http.MethodPost, agentPath("v1", "boxes", box.ID, "snapshot"), nil, &result)
		s.endSnapshot(box.ID)
		if err != nil || result.SnapshotID == "" {
			continue
		}
		createdAt := result.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		_, _ = s.recordSnapshot(ctx, box.ID, result.SnapshotID, result.SizeBytes, createdAt, config)
	}
}

func (s *Server) recordSnapshot(ctx context.Context, boxID, resticID string, size int64, createdAt time.Time, config storageConfig) (types.Snapshot, error) {
	encodedEnv, err := json.Marshal(config.Env)
	if err != nil {
		return types.Snapshot{}, err
	}
	encryptedEnv, err := s.secrets.encrypt(encodedEnv)
	if err != nil {
		return types.Snapshot{}, err
	}
	snapshot := types.Snapshot{ID: types.NewID("sn_"), BoxID: boxID, ResticID: resticID,
		SizeBytes: size, CreatedAt: createdAt, Repository: config.Repository, StorageEnvEnc: encryptedEnv}
	if err := s.store.CreateSnapshot(ctx, snapshot); err != nil {
		return types.Snapshot{}, err
	}
	// Retention runs for every recorded snapshot (manual, stop, reaper, periodic). Periodic capture alone
	// prunes only when it captures a change, so rows added here would otherwise exceed keepSnapshots.
	if box, err := s.store.GetBox(ctx, boxID); err == nil {
		if client, ok := newAgentClient(s.hub, box.NodeID); ok {
			if err := s.pruneSnapshots(ctx, boxID, client); err != nil {
				log.Printf("prune snapshots for %s after recording: %v", boxID, err)
			}
		}
	}
	return snapshot, nil
}

func (s *Server) snapshotStorage(snapshot types.Snapshot) (map[string]any, error) {
	if snapshot.Repository == "" {
		config := s.storageConfig()
		return map[string]any{"repository": config.Repository, "env": config.Env}, nil
	}
	plain, err := s.secrets.decrypt(snapshot.StorageEnvEnc)
	if err != nil {
		return nil, err
	}
	var env map[string]string
	if err := json.Unmarshal(plain, &env); err != nil {
		return nil, err
	}
	return map[string]any{"repository": snapshot.Repository, "env": env}, nil
}

func (s *Server) snapshotStorageHeader(snapshot types.Snapshot) (http.Header, error) {
	storage, err := s.snapshotStorage(snapshot)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(storage)
	if err != nil {
		return nil, err
	}
	return http.Header{"X-Boxes-Snapshot-Storage": {base64.RawURLEncoding.EncodeToString(data)}}, nil
}

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tony1908/boxes-oss/internal/store"
	"github.com/tony1908/boxes-oss/internal/types"
)

const (
	defaultTTLSeconds      = int64(3600)
	minimumTTLSeconds      = int64(60)
	maximumTTLSeconds      = int64(2592000)
	minimumIdleStopSeconds = int64(300)
	maximumIdleStopSeconds = int64(2592000)
	maximumSetupBytes      = 64 << 10
	maximumEnvBytes        = 64 << 10
	maximumEnvVars         = 100
)

type sandboxResponse struct {
	types.Box
	AutoStop            bool       `json:"autoStop"`
	TTLSeconds          *int64     `json:"ttlSeconds"`
	ExpiresAt           *time.Time `json:"expiresAt"`
	TTLRemainingSeconds *int64     `json:"ttlRemainingSeconds"`
	IdleStopSeconds     *int64     `json:"idleStopSeconds"`
	LastSnapshotAt      *time.Time `json:"lastSnapshotAt"`
	DesktopAvailable    bool       `json:"desktopAvailable"`
	RunningSeconds      int64      `json:"runningSeconds"`
	SetupStatus         string     `json:"setupStatus"`
}

// sandboxView converts persisted lifecycle accounting to the public sandbox representation.
func sandboxView(record store.BoxRecord, lastSnapshot *time.Time, now time.Time) sandboxResponse {
	view := sandboxResponse{
		Box: record.Box, AutoStop: record.Box.TTLSeconds > 0, ExpiresAt: record.Box.ExpiresAt, LastSnapshotAt: lastSnapshot,
		DesktopAvailable: record.Box.State == types.StateReady || record.Box.State == types.StateIdle,
		RunningSeconds:   record.RunningSeconds, SetupStatus: record.SetupStatus,
	}
	if view.SetupStatus == "" {
		view.SetupStatus = "none"
	}
	if record.Box.TTLSeconds > 0 {
		ttl := record.Box.TTLSeconds
		view.TTLSeconds = &ttl
	}
	if record.Box.IdleStopSeconds > 0 {
		seconds := record.Box.IdleStopSeconds
		view.IdleStopSeconds = &seconds
	}
	if record.Box.ExpiresAt != nil {
		remaining := int64(record.Box.ExpiresAt.Sub(now).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		view.TTLRemainingSeconds = &remaining
	}
	if record.RunningSince != nil && now.After(*record.RunningSince) {
		view.RunningSeconds += int64(now.Sub(*record.RunningSince).Seconds())
	}
	return view
}

func parseIdleStopSeconds(raw json.RawMessage, defaultValue int64) (int64, error) {
	if len(raw) == 0 {
		return defaultValue, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return 0, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, errors.New("idleStopSeconds must be an integer or null")
	}
	if value == 0 {
		return 0, nil
	}
	if value < minimumIdleStopSeconds {
		return 0, fmt.Errorf("idleStopSeconds must be at least %d seconds or null", minimumIdleStopSeconds)
	}
	if value > maximumIdleStopSeconds {
		return 0, fmt.Errorf("idleStopSeconds must not exceed %d seconds", maximumIdleStopSeconds)
	}
	return value, nil
}

func parseTTLSeconds(raw json.RawMessage, defaultValue int64) (int64, error) {
	if len(raw) == 0 {
		return defaultValue, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return 0, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, errors.New("ttlSeconds must be an integer or null")
	}
	if value == 0 {
		return 0, nil
	}
	if value < minimumTTLSeconds {
		return 0, fmt.Errorf("ttlSeconds must be at least %d seconds or null", minimumTTLSeconds)
	}
	if value > maximumTTLSeconds {
		value = maximumTTLSeconds
	}
	return value, nil
}

func validateSandboxEnv(env map[string]string) error {
	if len(env) > maximumEnvVars {
		return fmt.Errorf("env must contain no more than %d variables", maximumEnvVars)
	}
	var total int
	for key, value := range env {
		if !validEnvName(key) || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("env contains an invalid variable %q", key)
		}
		total += len(key) + len(value) + 2
	}
	if total > maximumEnvBytes {
		return fmt.Errorf("env must not exceed %d bytes", maximumEnvBytes)
	}
	return nil
}

// provisionRepo and provisionSetup mirror the setup.json document read by boxd.
type provisionRepo struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Branch      string `json:"branch,omitempty"`
	Revision    string `json:"revision,omitempty"`
	Depth       int    `json:"depth,omitempty"`
	TokenSecret string `json:"tokenSecret,omitempty"`
}

type provisionTarball struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type provisionSetup struct {
	Repos       []provisionRepo    `json:"repos"`
	Tarballs    []provisionTarball `json:"tarballs,omitempty"`
	SetupScript string             `json:"setupScript"`
}

func mergeProvisionFiles(files []map[string]any, env map[string]string, setupScript *string, source *sandboxSource) ([]map[string]any, error) {
	byPath := make(map[string]map[string]any, len(files)+2)
	for _, file := range files {
		name, _ := file["path"].(string)
		byPath[name] = file
	}
	decodeFile := func(name string) ([]byte, error) {
		file := byPath[name]
		if file == nil {
			return nil, nil
		}
		encoded, _ := file["content"].(string)
		return base64.StdEncoding.DecodeString(encoded)
	}
	writeFile := func(name string, mode int, data []byte) {
		byPath[name] = encodedBoxFile(name, mode, data)
	}

	if len(env) != 0 {
		values := make(map[string]string)
		data, err := decodeFile("/run/boxes/env")
		if err != nil {
			return nil, fmt.Errorf("decode environment file: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok || key == "" {
				continue
			}
			values[key] = strings.Trim(value, "'")
		}
		for key, value := range env {
			values[key] = value
		}
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var dotenv strings.Builder
		for _, key := range keys {
			dotenv.WriteString(key)
			dotenv.WriteByte('=')
			dotenv.WriteString(dotenvValue(values[key]))
			dotenv.WriteByte('\n')
		}
		writeFile("/run/boxes/env", 0600, []byte(dotenv.String()))
	}
	if setupScript != nil || source != nil {
		if setupScript != nil && len(*setupScript) > maximumSetupBytes {
			return nil, fmt.Errorf("setupScript must not exceed %d bytes", maximumSetupBytes)
		}
		setup := provisionSetup{Repos: []provisionRepo{}}
		data, err := decodeFile("/run/boxes/setup.json")
		if err != nil {
			return nil, fmt.Errorf("decode setup configuration: %w", err)
		}
		if len(data) != 0 {
			if err := json.Unmarshal(data, &setup); err != nil {
				return nil, fmt.Errorf("decode setup configuration: %w", err)
			}
		}
		if setupScript != nil {
			setup.SetupScript = *setupScript
		}
		if source != nil {
			// Source runs before the setup script; its token travels as a secret file that boxd removes after cloning.
			if source.Type == "tarball" {
				setup.Tarballs = append(setup.Tarballs, provisionTarball{Name: source.Path, URL: source.URL})
			} else {
				repo := provisionRepo{Name: source.Path, URL: source.URL, Revision: source.Revision}
				if source.Depth != nil {
					repo.Depth = *source.Depth
				}
				if source.Token != "" {
					writeFile(sourceTokenPath, 0600, []byte(source.Token))
					repo.TokenSecret = sourceTokenSecret
				}
				setup.Repos = append(setup.Repos, repo)
			}
		}
		data, err = json.Marshal(setup)
		if err != nil {
			return nil, err
		}
		writeFile("/run/boxes/setup.json", 0600, data)
	}
	if len(env) != 0 || setupScript != nil || source != nil {
		if byPath["/run/boxes/env"] == nil {
			writeFile("/run/boxes/env", 0600, nil)
		}
		if byPath["/run/boxes/setup.json"] == nil {
			data, _ := json.Marshal(provisionSetup{Repos: []provisionRepo{}})
			writeFile("/run/boxes/setup.json", 0600, data)
		}
	}
	result := make([]map[string]any, 0, len(byPath))
	for _, file := range files {
		name, _ := file["path"].(string)
		result = append(result, byPath[name])
		delete(byPath, name)
	}
	for _, file := range byPath {
		result = append(result, file)
	}
	return result, nil
}

func setupConfigured(files []map[string]any) bool {
	for _, file := range files {
		name, _ := file["path"].(string)
		if name != "/run/boxes/setup.json" {
			continue
		}
		encoded, _ := file["content"].(string)
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return false
		}
		var setup struct {
			Repos       []json.RawMessage `json:"repos"`
			Tarballs    []json.RawMessage `json:"tarballs"`
			SetupScript string            `json:"setupScript"`
		}
		if json.Unmarshal(data, &setup) == nil {
			return len(setup.Repos) > 0 || len(setup.Tarballs) > 0 || setup.SetupScript != ""
		}
	}
	return false
}

func payloadFiles(payload map[string]any) []map[string]any {
	files, _ := payload["files"].([]map[string]any)
	return files
}

func (s *Server) sandboxView(ctx context.Context, record store.BoxRecord) (sandboxResponse, error) {
	snapshots, err := s.store.ListSnapshots(ctx, record.Box.ID)
	if err != nil {
		return sandboxResponse{}, err
	}
	var lastSnapshot *time.Time
	if len(snapshots) != 0 {
		createdAt := snapshots[len(snapshots)-1].CreatedAt
		lastSnapshot = &createdAt
	}
	return sandboxView(record, lastSnapshot, time.Now().UTC()), nil
}

func closeRunningInterval(record *store.BoxRecord, now time.Time) {
	if record.RunningSince == nil {
		return
	}
	if now.After(*record.RunningSince) {
		record.RunningSeconds += int64(now.Sub(*record.RunningSince).Seconds())
	}
	record.RunningSince = nil
}

func matchingSandboxFilter(state types.BoxState, filter string) bool {
	if filter == "" {
		return true
	}
	for _, category := range filter {
		switch category {
		case 'r':
			if state == types.StateProvisioning || state == types.StateReady || state == types.StateIdle || state == types.StateResuming {
				return true
			}
		case 's':
			if state == types.StateStopping || state == types.StateStopped {
				return true
			}
		case 'e':
			if state == types.StateError || state == types.StateUnreachable {
				return true
			}
		}
	}
	return false
}

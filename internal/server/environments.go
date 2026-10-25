package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/tony1908/boxes-oss/internal/store"
	"github.com/tony1908/boxes-oss/internal/types"
)

type environmentRepo struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Branch      string `json:"branch,omitempty"`
	TokenSecret string `json:"tokenSecret,omitempty"`
}

type environmentSecrets struct {
	Vars  map[string]string `json:"vars,omitempty"`
	Files map[string]string `json:"files,omitempty"`
}

type environmentInput struct {
	Name        string            `json:"name"`
	Vars        map[string]string `json:"vars"`
	Secrets     map[string]string `json:"secrets"`
	SecretFiles map[string]string `json:"secretFiles"`
	Repos       []environmentRepo `json:"repos"`
	SetupScript string            `json:"setupScript"`
	NoSecrets   bool              `json:"noSecrets"`
}

func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListEnvironments(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"environments": items})
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	var input environmentInput
	if err := decodeJSON(r, &input); err != nil || strings.TrimSpace(input.Name) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "environment name is required")
		return
	}
	if err := validateEnvironment(input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_environment", err.Error())
		return
	}
	document := environmentDocument(input)
	secrets, err := s.secrets.encrypt(encodeEnvironmentSecrets(input))
	if err != nil {
		s.apiError(w, err)
		return
	}
	now := time.Now().UTC()
	env := types.Environment{ID: types.NewID("env_"), Name: strings.TrimSpace(input.Name), Version: 1, Document: document, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateEnvironment(r.Context(), env, secrets); err != nil {
		writeError(w, http.StatusConflict, "environment_exists", "an environment with this name already exists")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"environment": env})
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request) {
	env, err := s.store.GetEnvironment(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"environment": env})
}

func (s *Server) updateEnvironment(w http.ResponseWriter, r *http.Request) {
	env, err := s.store.GetEnvironment(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	current, err := s.store.GetEnvironmentVersion(r.Context(), env.ID, env.Version)
	if err != nil {
		s.apiError(w, err)
		return
	}
	plain, err := s.secrets.decrypt(current.SecretsEnc)
	if err != nil {
		s.apiError(w, err)
		return
	}
	secretValues := environmentSecrets{}
	if len(plain) != 0 {
		if err := json.Unmarshal(plain, &secretValues); err != nil {
			s.apiError(w, err)
			return
		}
	}
	var patch struct {
		Vars        map[string]string `json:"vars"`
		Secrets     map[string]string `json:"secrets"`
		SecretFiles map[string]string `json:"secretFiles"`
		Repos       []environmentRepo `json:"repos"`
		SetupScript *string           `json:"setupScript"`
		NoSecrets   *bool             `json:"noSecrets"`
	}
	if err := decodeJSON(r, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid environment update")
		return
	}
	input := environmentInput{Vars: patch.Vars, Secrets: patch.Secrets, SecretFiles: patch.SecretFiles, Repos: patch.Repos}
	if patch.SetupScript != nil {
		input.SetupScript = *patch.SetupScript
	}
	if input.Vars == nil {
		if raw, ok := env.Document["vars"]; ok {
			encoded, _ := json.Marshal(raw)
			_ = json.Unmarshal(encoded, &input.Vars)
		}
	}
	if input.Repos == nil {
		if encoded, ok := env.Document["repos"]; ok {
			data, _ := json.Marshal(encoded)
			_ = json.Unmarshal(data, &input.Repos)
		}
	}
	if patch.SetupScript == nil {
		input.SetupScript, _ = env.Document["setupScript"].(string)
	}
	if patch.NoSecrets != nil {
		input.NoSecrets = *patch.NoSecrets
	} else {
		input.NoSecrets, _ = env.Document["noSecrets"].(bool)
	}
	if input.Secrets == nil {
		input.Secrets = secretValues.Vars
	}
	if input.SecretFiles == nil {
		input.SecretFiles = secretValues.Files
	}
	if err := validateEnvironment(input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_environment", err.Error())
		return
	}
	encodedSecrets, err := s.secrets.encrypt(encodeEnvironmentSecrets(input))
	if err != nil {
		s.apiError(w, err)
		return
	}
	now := time.Now().UTC()
	env.Version++
	env.Document = environmentDocument(input)
	env.UpdatedAt = now
	version := store.EnvironmentVersion{EnvironmentID: env.ID, Version: env.Version, Document: env.Document, SecretsEnc: encodedSecrets, CreatedAt: now}
	if err := s.store.CreateEnvironmentVersion(r.Context(), version); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"environment": env})
}

func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteEnvironment(r.Context(), pathID(r)); err != nil {
		s.apiError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) upgradeEnvironment(w http.ResponseWriter, r *http.Request) {
	env, err := s.store.GetEnvironment(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	boxes, err := s.store.ListBoxes(r.Context(), "")
	if err != nil {
		s.apiError(w, err)
		return
	}
	upgraded := 0
	for _, box := range boxes {
		if box.EnvironmentID != env.ID || (box.State != types.StateReady && box.State != types.StateIdle) {
			continue
		}
		files, _, err := s.environmentFiles(r.Context(), env.ID, env.Version, false)
		if err != nil {
			s.apiError(w, err)
			return
		}
		client, ok := newAgentClient(s.hub, box.NodeID)
		if !ok {
			continue
		}
		if err := s.uploadEnvironmentFiles(r.Context(), client, box.ID, files); err != nil {
			continue
		}
		box.EnvironmentVersion = env.Version
		if err := s.store.UpdateBox(r.Context(), box); err != nil {
			continue
		}
		upgraded++
	}
	writeJSON(w, http.StatusOK, map[string]any{"environment": env, "upgraded": upgraded})
}

func validateEnvironment(input environmentInput) error {
	for key := range input.Vars {
		if !validEnvName(key) {
			return fmt.Errorf("invalid environment variable name %q", key)
		}
		if strings.ContainsAny(input.Vars[key], "\r\n\x00") {
			return fmt.Errorf("environment variable %q cannot contain a newline", key)
		}
	}
	for key := range input.Secrets {
		if !validEnvName(key) {
			return fmt.Errorf("invalid secret variable name %q", key)
		}
		if strings.ContainsAny(input.Secrets[key], "\r\n\x00") {
			return fmt.Errorf("secret variable %q cannot contain a newline", key)
		}
	}
	for name := range input.SecretFiles {
		if !safeSecretPath(name) {
			return fmt.Errorf("invalid secret file path %q", name)
		}
	}
	for _, repo := range input.Repos {
		if strings.TrimSpace(repo.Name) == "" || strings.TrimSpace(repo.URL) == "" {
			return errors.New("repositories require a name and url")
		}
		if repo.TokenSecret != "" && !safeSecretPath(repo.TokenSecret) {
			return fmt.Errorf("invalid repository token secret path %q", repo.TokenSecret)
		}
		if repo.TokenSecret != "" {
			if _, exists := input.SecretFiles[repo.TokenSecret]; !exists {
				return fmt.Errorf("repository token secret %q is not configured", repo.TokenSecret)
			}
		}
	}
	return nil
}

func environmentDocument(input environmentInput) map[string]any {
	vars := input.Vars
	if vars == nil {
		vars = map[string]string{}
	}
	repos := input.Repos
	if repos == nil {
		repos = []environmentRepo{}
	}
	return map[string]any{"vars": vars, "repos": repos, "setupScript": input.SetupScript, "noSecrets": input.NoSecrets}
}

func encodeEnvironmentSecrets(input environmentInput) []byte {
	encoded, _ := json.Marshal(environmentSecrets{Vars: input.Secrets, Files: input.SecretFiles})
	return encoded
}

func (s *Server) environmentFiles(ctx context.Context, envID string, version int, noSecrets bool) ([]map[string]any, int, error) {
	stored, err := s.store.GetEnvironmentVersion(ctx, envID, version)
	if err != nil {
		return nil, 0, err
	}
	plain, err := s.secrets.decrypt(stored.SecretsEnc)
	if err != nil {
		return nil, 0, err
	}
	secrets := environmentSecrets{}
	if len(plain) != 0 {
		if err := json.Unmarshal(plain, &secrets); err != nil {
			return nil, 0, err
		}
	}
	vars := map[string]string{}
	if raw, ok := stored.Document["vars"]; ok {
		encoded, _ := json.Marshal(raw)
		_ = json.Unmarshal(encoded, &vars)
	}
	if configuredNoSecrets, ok := stored.Document["noSecrets"].(bool); ok && configuredNoSecrets {
		noSecrets = true
	}
	if !noSecrets {
		for key, value := range secrets.Vars {
			vars[key] = value
		}
	}
	keys := make([]string, 0, len(vars))
	for key := range vars {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var dotenv strings.Builder
	for _, key := range keys {
		dotenv.WriteString(key)
		dotenv.WriteByte('=')
		dotenv.WriteString(dotenvValue(vars[key]))
		dotenv.WriteByte('\n')
	}
	files := []map[string]any{encodedBoxFile("/run/boxes/env", 0o600, []byte(dotenv.String()))}
	if !noSecrets {
		for name, content := range secrets.Files {
			files = append(files, encodedBoxFile(path.Join("/run/boxes/secrets", name), 0o600, []byte(content)))
		}
	}
	repos := []environmentRepo{}
	if raw, ok := stored.Document["repos"]; ok {
		encoded, _ := json.Marshal(raw)
		_ = json.Unmarshal(encoded, &repos)
	}
	if noSecrets {
		for index := range repos {
			repos[index].TokenSecret = ""
		}
	}
	setup := struct {
		Repos       []environmentRepo `json:"repos"`
		SetupScript string            `json:"setupScript"`
	}{}
	setup.Repos = repos
	setup.SetupScript, _ = stored.Document["setupScript"].(string)
	setupJSON, err := json.Marshal(setup)
	if err != nil {
		return nil, 0, err
	}
	files = append(files, encodedBoxFile("/run/boxes/setup.json", 0o600, setupJSON))
	return files, stored.Version, nil
}

func (s *Server) uploadEnvironmentFiles(ctx context.Context, client *agentClient, boxID string, files []map[string]any) error {
	for _, file := range files {
		name, _ := file["path"].(string)
		content, _ := file["content"].(string)
		mode, _ := file["mode"].(int)
		data, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return err
		}
		query := url.Values{"path": []string{name}, "mode": []string{fmt.Sprintf("%o", mode)}}
		response, err := client.raw(ctx, http.MethodPut, agentPath("v1", "boxes", boxID, "files")+"?"+query.Encode(), bytes.NewReader(data), "application/octet-stream")
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return fmt.Errorf("write environment file returned HTTP %d", response.StatusCode)
		}
	}
	return nil
}

func encodedBoxFile(name string, mode int, contents []byte) map[string]any {
	return map[string]any{"path": name, "mode": mode, "content": base64.StdEncoding.EncodeToString(contents)}
}

func dotenvValue(value string) string {
	if strings.TrimSpace(value) != value {
		return "'" + value + "'"
	}
	return value
}

func validEnvName(value string) bool {
	if value == "" {
		return false
	}
	for index, char := range value {
		if char != '_' && (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && (index <= 0 || char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func safeSecretPath(value string) bool {
	clean := path.Clean(value)
	return value != "" && !strings.HasPrefix(value, "/") && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../") && clean == value
}

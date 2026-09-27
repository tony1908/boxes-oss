package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const (
	registrySettingKey = "registry"
	imageSettingKey    = "default_images"
)

type registryConfig struct {
	URL      string
	Username string
	Password string
}

type storedRegistryConfig struct {
	URL         string `json:"url"`
	Username    string `json:"username"`
	PasswordEnc []byte `json:"passwordEnc"`
}

type defaultImageConfig struct {
	Box   string `json:"boxImage"`
	Tools string `json:"toolsImage"`
}

var imageReferencePattern = regexp.MustCompile(`^[A-Za-z0-9._/:@-]+$`)

func (s *Server) initializeRegistry() error {
	data, err := s.store.GetSetting(context.Background(), registrySettingKey)
	if err == nil {
		var stored storedRegistryConfig
		if err := json.Unmarshal(data, &stored); err != nil {
			return fmt.Errorf("decode registry settings: %w", err)
		}
		password, err := s.secrets.decrypt(stored.PasswordEnc)
		if err != nil {
			return fmt.Errorf("decrypt registry password: %w", err)
		}
		config := registryConfig{URL: stored.URL, Username: stored.Username, Password: string(password)}
		if err := validateRegistryConfig(config); err != nil {
			return fmt.Errorf("load registry settings: %w", err)
		}
		s.registry = config
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load registry settings: %w", err)
	}
	config := registryConfig{URL: os.Getenv("BOXES_REGISTRY_URL"), Username: os.Getenv("BOXES_REGISTRY_USERNAME"), Password: os.Getenv("BOXES_REGISTRY_PASSWORD")}
	if config.URL == "" && config.Username == "" && config.Password == "" {
		s.registry = config
		return nil
	}
	if err := validateRegistryConfig(config); err != nil {
		return fmt.Errorf("configure registry: %w", err)
	}
	return s.saveRegistry(context.Background(), config)
}

func validateRegistryConfig(config registryConfig) error {
	if config.URL == "" && config.Username == "" && config.Password == "" {
		return nil
	}
	if config.URL == "" || config.Username == "" || config.Password == "" {
		return errors.New("registry URL, username, and password must all be configured")
	}
	parsed, err := url.Parse("//" + config.URL)
	if err != nil || parsed.Host == "" || parsed.Host != config.URL || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("registry URL must be a hostname with an optional port")
	}
	if parsed.Port() == "0" || strings.ContainsAny(config.Username+config.Password, "\r\n\x00") {
		return errors.New("registry credentials contain an invalid value")
	}
	return nil
}

func (s *Server) registryConfig() registryConfig {
	s.registryMu.RLock()
	defer s.registryMu.RUnlock()
	return s.registry
}

func (s *Server) saveRegistry(ctx context.Context, config registryConfig) error {
	if err := validateRegistryConfig(config); err != nil {
		return err
	}
	password, err := s.secrets.encrypt([]byte(config.Password))
	if err != nil {
		return err
	}
	data, err := json.Marshal(storedRegistryConfig{URL: config.URL, Username: config.Username, PasswordEnc: password})
	if err != nil {
		return err
	}
	if err := s.store.SetSetting(ctx, registrySettingKey, data); err != nil {
		return err
	}
	s.registryMu.Lock()
	s.registry = config
	s.registryMu.Unlock()
	return nil
}

func (s *Server) registrySettings(w http.ResponseWriter, r *http.Request) {
	if currentUser(r).Role != "owner" {
		writeError(w, http.StatusForbidden, "owner_required", "only an owner can view registry settings")
		return
	}
	config := s.registryConfig()
	writeJSON(w, http.StatusOK, map[string]string{"url": config.URL, "username": config.Username, "password": config.Password})
}

func (s *Server) initializeDefaultImages(boxImage, toolsImage string) error {
	data, err := s.store.GetSetting(context.Background(), imageSettingKey)
	if err == nil {
		var images defaultImageConfig
		if err := json.Unmarshal(data, &images); err != nil {
			return fmt.Errorf("decode default image settings: %w", err)
		}
		if images.Box == "" || images.Tools == "" {
			return errors.New("default box and tools images must both be configured")
		}
		s.images = images
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load default image settings: %w", err)
	}
	return s.saveDefaultImages(context.Background(), defaultImageConfig{Box: boxImage, Tools: toolsImage})
}

func (s *Server) defaultImages() defaultImageConfig {
	s.imageMu.RLock()
	defer s.imageMu.RUnlock()
	return s.images
}

func (s *Server) saveDefaultImages(ctx context.Context, images defaultImageConfig) error {
	if !imageReferencePattern.MatchString(images.Box) || !imageReferencePattern.MatchString(images.Tools) {
		return errors.New("box and tools images must be valid container image references")
	}
	data, err := json.Marshal(images)
	if err != nil {
		return err
	}
	if err := s.store.SetSetting(ctx, imageSettingKey, data); err != nil {
		return err
	}
	s.imageMu.Lock()
	s.images = images
	s.imageMu.Unlock()
	return nil
}

func (s *Server) updateImageSettings(w http.ResponseWriter, r *http.Request) {
	if currentUser(r).Role != "owner" {
		writeError(w, http.StatusForbidden, "owner_required", "only an owner can update image settings")
		return
	}
	var input struct {
		BoxImage   string `json:"boxImage"`
		ToolsImage string `json:"toolsImage"`
	}
	if err := decodeJSON(r, &input); err != nil || (input.BoxImage == "" && input.ToolsImage == "") {
		writeError(w, http.StatusBadRequest, "invalid_request", "at least one default image is required")
		return
	}
	images := s.defaultImages()
	if input.BoxImage != "" {
		images.Box = input.BoxImage
	}
	if input.ToolsImage != "" {
		images.Tools = input.ToolsImage
	}
	if err := s.saveDefaultImages(r.Context(), images); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_image", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, images)
}

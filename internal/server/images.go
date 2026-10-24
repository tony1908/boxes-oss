package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
)

const imageSettingKey = "default_images"

type defaultImageConfig struct {
	Box   string `json:"boxImage"`
	Tools string `json:"toolsImage"`
}

var imageReferencePattern = regexp.MustCompile(`^[A-Za-z0-9._/:@-]+$`)

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

// validateImageReference checks a container image reference; default and per-box images share this rule.
func validateImageReference(images ...string) error {
	for _, image := range images {
		if !imageReferencePattern.MatchString(image) {
			return errors.New("image must be a valid container image reference")
		}
	}
	return nil
}

func (s *Server) saveDefaultImages(ctx context.Context, images defaultImageConfig) error {
	if err := validateImageReference(images.Box, images.Tools); err != nil {
		return err
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

package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/boxes-oss/boxes/internal/types"
)

type webhookEventDelivery struct {
	Name string
	Body []byte
}

type webhookRequest struct {
	URL    string
	Secret string
	Body   []byte
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListWebhooks(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	for i := range items {
		items[i].Secret = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": items})
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	var input struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
		Secret string   `json:"secret"`
	}
	if err := decodeJSON(r, &input); err != nil || !validWebhookURL(input.URL) || len(input.Events) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "a valid url and at least one event are required")
		return
	}
	secret := input.Secret
	if secret == "" {
		var err error
		secret, err = randomToken(32)
		if err != nil {
			s.apiError(w, err)
			return
		}
	}
	now := time.Now().UTC()
	webhook := types.Webhook{ID: types.NewID("wh_"), URL: input.URL, Events: cleanWebhookEvents(input.Events), Secret: secret, CreatedAt: now, UpdatedAt: now}
	if len(webhook.Events) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_event", "at least one supported event is required")
		return
	}
	if err := s.store.CreateWebhook(r.Context(), webhook); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"webhook": webhook, "secret": secret})
}

func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request) {
	webhook, err := s.store.GetWebhook(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	var input struct {
		URL    *string   `json:"url"`
		Events *[]string `json:"events"`
		Secret *string   `json:"secret"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid webhook update")
		return
	}
	if input.URL != nil {
		if !validWebhookURL(*input.URL) {
			writeError(w, http.StatusBadRequest, "invalid_url", "webhook URL must use http or https")
			return
		}
		webhook.URL = *input.URL
	}
	if input.Events != nil {
		webhook.Events = cleanWebhookEvents(*input.Events)
		if len(webhook.Events) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_event", "at least one supported event is required")
			return
		}
	}
	if input.Secret != nil {
		if strings.TrimSpace(*input.Secret) == "" {
			writeError(w, http.StatusBadRequest, "invalid_secret", "secret cannot be empty")
			return
		}
		webhook.Secret = *input.Secret
	}
	webhook.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateWebhook(r.Context(), webhook); err != nil {
		s.apiError(w, err)
		return
	}
	webhook.Secret = ""
	writeJSON(w, http.StatusOK, map[string]any{"webhook": webhook})
}

func (s *Server) rotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	webhook, err := s.store.GetWebhook(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	secret, err := randomToken(32)
	if err != nil {
		s.apiError(w, err)
		return
	}
	webhook.Secret = secret
	webhook.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateWebhook(r.Context(), webhook); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secret": secret})
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteWebhook(r.Context(), pathID(r)); err != nil {
		s.apiError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) emitWebhookEvent(_ context.Context, name string, data any) {
	body, err := json.Marshal(map[string]any{"event": name, "time": time.Now().UTC(), "data": data})
	if err != nil {
		return
	}
	item := webhookEventDelivery{Name: name, Body: body}
	select {
	case s.webhookQueue <- item:
	default:
		// Keep API handlers independent from slow webhook destinations.
	}
}

func (s *Server) runWebhookQueue(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-s.webhookQueue:
			s.deliverWebhookEvent(ctx, item)
		}
	}
}

func (s *Server) deliverWebhookEvent(ctx context.Context, item webhookEventDelivery) {
	webhooks, err := s.store.ListWebhooks(ctx)
	if err != nil {
		return
	}
	for _, webhook := range webhooks {
		if subscribed(webhook.Events, item.Name) {
			s.deliverWebhook(webhookRequest{URL: webhook.URL, Secret: webhook.Secret, Body: item.Body})
		}
	}
}

func (s *Server) deliverWebhook(item webhookRequest) {
	mac := hmac.New(sha256.New, []byte(item.Secret))
	_, _ = mac.Write(item.Body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	client := &http.Client{Timeout: 5 * time.Second}
	for attempt := 0; attempt < 4; attempt++ {
		request, err := http.NewRequest(http.MethodPost, item.URL, bytes.NewReader(item.Body))
		if err == nil {
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Boxes-Signature", signature)
			response, requestErr := client.Do(request)
			if requestErr == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
				_ = response.Body.Close()
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					return
				}
			}
		}
		if attempt < 3 {
			time.Sleep(time.Duration(1<<attempt) * 100 * time.Millisecond)
		}
	}
}

func validWebhookURL(raw string) bool {
	parsed, err := url.ParseRequestURI(raw)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func cleanWebhookEvents(events []string) []string {
	allowed := map[string]bool{"sandbox.ready": true, "sandbox.restored": true, "sandbox.error": true, "sandbox.stopped": true, "sandbox.deleted": true}
	result := make([]string, 0, len(events))
	seen := make(map[string]bool)
	for _, event := range events {
		event = strings.TrimSpace(event)
		if allowed[event] && !seen[event] {
			seen[event] = true
			result = append(result, event)
		}
	}
	return result
}

func subscribed(events []string, event string) bool {
	for _, candidate := range events {
		if candidate == event || candidate == "*" {
			return true
		}
	}
	return false
}

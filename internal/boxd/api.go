package boxd

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type promptRequest struct {
	Prompt         string `json:"prompt"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	ConversationID string `json:"conversationId"`
	New            bool   `json:"new"`
}
type conversationRequest struct {
	ConversationID string `json:"conversationId"`
	Message        string `json:"message"`
}

func (s *Service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/prompt", s.handlePrompt)
	mux.HandleFunc("/steer", s.handleSteer)
	mux.HandleFunc("/interrupt", s.handleInterrupt)
	mux.HandleFunc("/conversations", s.handleConversations)
	mux.HandleFunc("/runs/", s.handleRun)
	mux.HandleFunc("/events", s.handleEvents)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && !s.authorized(r) {
			writeAPIError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Service) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+s.config.Token
}

func (s *Service) handlePrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req promptRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_prompt", "prompt is required")
		return
	}
	runID, conversationID, err := s.enqueue(req)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "prompt_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"runId": runID, "conversationId": conversationID})
}

func (s *Service) handleSteer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req conversationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_message", "message is required")
		return
	}
	if req.ConversationID != "" {
		if err := s.steer(req.ConversationID, req.Message); err != nil {
			writeAPIError(w, http.StatusConflict, "steer_failed", err.Error())
			return
		}
	} else {
		s.mu.Lock()
		ids := make([]string, 0, len(s.runtimes))
		for id := range s.runtimes {
			ids = append(ids, id)
		}
		s.mu.Unlock()
		for _, id := range ids {
			_ = s.steer(id, req.Message)
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "steered"})
}

func (s *Service) steer(id, message string) error {
	s.mu.Lock()
	runtime := s.runtimes[id]
	s.mu.Unlock()
	if runtime == nil {
		return ErrNotFound
	}
	runtime.mu.Lock()
	session, active := runtime.session, runtime.active
	runtime.mu.Unlock()
	if session == nil || !active {
		return ErrNotFound
	}
	if err := session.Steer(message); err != nil {
		return err
	}
	_, err := s.emit(id, "", "message", map[string]any{"text": message, "steered": true})
	return err
}

func (s *Service) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ConversationID string `json:"conversationId"`
	}
	if r.Body != nil {
		if err := decodeJSON(r, &req); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
	}
	s.mu.Lock()
	var targets []Session
	if req.ConversationID != "" {
		if rt := s.runtimes[req.ConversationID]; rt != nil {
			rt.mu.Lock()
			if rt.active && rt.session != nil {
				targets = append(targets, rt.session)
			}
			rt.mu.Unlock()
		}
	} else {
		for _, rt := range s.runtimes {
			rt.mu.Lock()
			if rt.active && rt.session != nil {
				targets = append(targets, rt.session)
			}
			rt.mu.Unlock()
		}
	}
	s.mu.Unlock()
	for _, session := range targets {
		_ = session.Interrupt()
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "interrupted"})
}

func (s *Service) handleConversations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	items, err := s.outbox.Conversations()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "outbox_error", err.Error())
		return
	}
	if items == nil {
		items = []Conversation{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Service) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/runs/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	run, err := s.outbox.Run(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	if wait < 0 {
		wait = 0
	}
	if wait > 30 {
		wait = 30
	}
	conversationID := r.URL.Query().Get("conversation")
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		events, err := s.outbox.Events(after, conversationID, 1000)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "outbox_error", err.Error())
			return
		}
		if len(events) > 0 || time.Now().After(deadline) {
			cursor := after
			if len(events) > 0 {
				cursor = events[len(events)-1].Cursor
			}
			writeJSON(w, http.StatusOK, map[string]any{"events": events, "cursor": cursor})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func decodeJSON(r *http.Request, v any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(v)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func methodNotAllowed(w http.ResponseWriter) {
	writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

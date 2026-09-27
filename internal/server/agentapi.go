package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/boxes-oss/boxes/internal/types"
	"github.com/coder/websocket"
)

// providerModels is a static catalog; refresh it when harness model catalogs change.
func (s *Server) providerModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": []any{
		map[string]any{"id": "claude", "models": []string{"claude-sonnet-4-5", "claude-opus-4-1", "claude-haiku-4-5"}, "reasoning": []string{"low", "medium", "high"}},
		map[string]any{"id": "codex", "models": []string{"gpt-5-codex", "gpt-5"}, "reasoning": []string{"low", "medium", "high", "xhigh"}},
		map[string]any{"id": "opencode", "models": []string{"anthropic/claude-sonnet-4-5", "openai/gpt-5"}, "reasoning": []string{"low", "medium", "high"}},
		map[string]any{"id": "pi", "models": []string{"anthropic/claude-sonnet-4-5", "openai/gpt-5"}, "reasoning": []string{"off", "minimal", "low", "medium", "high"}},
		map[string]any{"id": "prime", "models": []string{"anthropic/claude-sonnet-4-5", "openai/gpt-5"}, "reasoning": []string{"off", "minimal", "low", "medium", "high"}},
		map[string]any{"id": "kimi", "models": []string{"kimi-k2.5", "kimi-k2-thinking"}, "reasoning": []string{"low", "medium", "high"}},
	}})
}

func (s *Server) prompt(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Prompt         string `json:"prompt"`
		Provider       string `json:"provider"`
		Model          string `json:"model"`
		ConversationID string `json:"conversationId"`
		New            bool   `json:"new"`
		StopWhenDone   bool   `json:"stopWhenDone"`
	}
	if err := decodeJSON(r, &input); err != nil || strings.TrimSpace(input.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "prompt is required")
		return
	}
	if input.Provider != "claude" && input.Provider != "codex" && input.Provider != "opencode" && input.Provider != "pi" && input.Provider != "prime" && input.Provider != "kimi" {
		writeError(w, http.StatusBadRequest, "invalid_provider", "provider must be claude, codex, opencode, pi, prime, or kimi")
		return
	}
	box, err := s.store.GetBox(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	if box.Box.State == types.StateStopped {
		box, err = s.resumeBox(r.Context(), box, resumeSandboxRequest{})
		if err != nil {
			s.writeResumeError(w, box.Box.ID, err)
			return
		}
	}
	if box.Box.State != types.StateReady && box.Box.State != types.StateIdle {
		writeError(w, http.StatusConflict, "sandbox_not_ready", "sandbox must be ready before prompting")
		return
	}
	client, ok := newAgentClient(s.hub, box.Box.NodeID)
	if !ok {
		writeError(w, http.StatusBadGateway, "node_unavailable", "sandbox node is offline")
		return
	}
	var response struct {
		RunID          string `json:"runId"`
		ConversationID string `json:"conversationId"`
	}
	boxdInput := struct {
		Prompt         string `json:"prompt"`
		Provider       string `json:"provider"`
		Model          string `json:"model"`
		ConversationID string `json:"conversationId"`
		New            bool   `json:"new"`
	}{input.Prompt, input.Provider, input.Model, input.ConversationID, input.New}
	queuedAt := time.Now()
	if err := client.json(r.Context(), http.MethodPost, agentPath("v1", "boxes", box.Box.ID, "boxd", "prompt"), boxdInput, &response); err != nil {
		duration := time.Since(queuedAt).Milliseconds()
		s.recordActivityWithMetrics(r.Context(), box.Box.ID, "agent", "prompt run failed", map[string]any{"provider": input.Provider, "model": input.Model, "promptPreview": safeActivityText(input.Prompt, 120)}, &duration, "error", time.Now().UTC())
		s.apiError(w, err)
		return
	}
	if response.RunID == "" || response.ConversationID == "" {
		duration := time.Since(queuedAt).Milliseconds()
		s.recordActivityWithMetrics(r.Context(), box.Box.ID, "agent", "prompt run failed", map[string]any{"provider": input.Provider, "model": input.Model, "promptPreview": safeActivityText(input.Prompt, 120), "reason": "incomplete prompt response"}, &duration, "error", time.Now().UTC())
		writeError(w, http.StatusBadGateway, "agent_error", "boxd returned an incomplete prompt response")
		return
	}
	now := time.Now().UTC()
	if _, activityErr := s.store.GetPromptRunQueuedActivity(r.Context(), response.RunID); errors.Is(activityErr, sql.ErrNoRows) {
		duration := time.Since(queuedAt).Milliseconds()
		s.recordActivityWithMetrics(r.Context(), box.Box.ID, "agent", "prompt run queued", map[string]any{"runId": response.RunID, "provider": input.Provider, "model": input.Model, "promptPreview": safeActivityText(input.Prompt, 120)}, &duration, "ok", now)
	} else if activityErr != nil {
		s.apiError(w, activityErr)
		return
	}
	conversation, getErr := s.store.GetConversation(r.Context(), response.ConversationID)
	if getErr != nil {
		conversation = types.Conversation{ID: response.ConversationID, BoxID: box.Box.ID, Provider: input.Provider, Model: input.Model, CreatedAt: now}
	}
	conversation.Provider, conversation.Model, conversation.LastPrompt = input.Provider, input.Model, input.Prompt
	conversation.Prompts++
	conversation.Running, conversation.Current, conversation.UpdatedAt = true, true, now
	if getErr == nil {
		_ = s.store.UpdateConversation(r.Context(), conversation)
	} else if err := s.store.CreateConversation(r.Context(), conversation); err != nil {
		s.apiError(w, err)
		return
	}
	run := types.Run{ID: response.RunID, ConversationID: response.ConversationID, Status: "queued", CreatedAt: now}
	if err := s.store.CreateRunWithStopWhenDone(r.Context(), run, input.StopWhenDone); err != nil {
		if _, lookupErr := s.store.GetRun(r.Context(), run.ID); lookupErr == nil {
			if input.StopWhenDone {
				if err := s.store.SetRunStopWhenDone(r.Context(), run.ID, true); err != nil {
					s.apiError(w, err)
					return
				}
			}
			writeJSON(w, http.StatusAccepted, map[string]string{"runId": response.RunID, "conversationId": response.ConversationID})
			return
		}
		duration := time.Since(queuedAt).Milliseconds()
		s.recordActivityWithMetrics(r.Context(), box.Box.ID, "agent", "prompt run failed", map[string]any{"runId": response.RunID, "provider": input.Provider, "model": input.Model, "promptPreview": safeActivityText(input.Prompt, 120), "reason": err.Error()}, &duration, "error", now)
		s.apiError(w, err)
		return
	}
	if input.StopWhenDone {
		if storedRun, err := s.store.GetRun(r.Context(), run.ID); err == nil && isTerminalRunStatus(storedRun.Status) {
			go s.stopBoxAfterRun(box.Box.ID)
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"runId": response.RunID, "conversationId": response.ConversationID})
}

func (s *Server) steer(w http.ResponseWriter, r *http.Request) {
	s.forwardBoxdJSON(w, r, http.MethodPost, "steer")
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	s.forwardBoxdJSON(w, r, http.MethodPost, "interrupt")
}

func (s *Server) forwardBoxdJSON(w http.ResponseWriter, r *http.Request, method, action string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read request")
		return
	}
	var value map[string]any
	if len(body) != 0 && json.Unmarshal(body, &value) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request")
		return
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	boxID := pathID(r)
	started := time.Now()
	client, err := s.boxAgent(r.Context(), boxID)
	if err != nil {
		duration := time.Since(started).Milliseconds()
		s.recordActivityWithMetrics(r.Context(), boxID, "agent", action, nil, &duration, "error", time.Now().UTC())
		s.apiError(w, err)
		return
	}
	response, err := client.raw(r.Context(), method, agentPath("v1", "boxes", boxID, "boxd", action), strings.NewReader(string(body)), "application/json")
	if err != nil {
		duration := time.Since(started).Milliseconds()
		s.recordActivityWithMetrics(r.Context(), boxID, "agent", action, nil, &duration, "error", time.Now().UTC())
		s.apiError(w, err)
		return
	}
	duration := time.Since(started).Milliseconds()
	outcome := "ok"
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		outcome = "error"
	}
	s.recordActivityWithMetrics(r.Context(), boxID, "agent", action, nil, &duration, outcome, time.Now().UTC())
	defer response.Body.Close()
	for key, values := range response.Header {
		if strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Connection") || strings.EqualFold(key, "Transfer-Encoding") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (s *Server) proxyBoxd(w http.ResponseWriter, r *http.Request) {
	suffix := strings.Trim(r.PathValue("path"), "/")
	if suffix == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "boxd path is required")
		return
	}
	segments := strings.Split(suffix, "/")
	for index, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			writeError(w, http.StatusBadRequest, "invalid_path", "boxd path contains an invalid segment")
			return
		}
		segments[index] = url.PathEscape(segment)
	}
	endpoint := agentPath("v1", "boxes", pathID(r), "boxd") + "/" + strings.Join(segments, "/")
	s.forward(w, r, r.Method, endpoint, r.Body, r.Header.Get("Content-Type"))
}

func (s *Server) tcp22(w http.ResponseWriter, r *http.Request) {
	r.SetPathValue("port", "22")
	s.tcpProxy(w, r)
}

func (s *Server) tcpProxy(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeError(w, http.StatusBadRequest, "invalid_port", "port must be between 1 and 65535")
		return
	}
	box, err := s.store.GetBox(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	client, ok := newAgentClient(s.hub, box.Box.NodeID)
	if !ok {
		writeError(w, http.StatusBadGateway, "node_unavailable", "sandbox node is offline")
		return
	}
	agentConn, _, err := websocket.Dial(r.Context(), "ws://agent"+agentPath("v1", "boxes", box.Box.ID, "tcp", strconv.Itoa(port)), &websocket.DialOptions{HTTPClient: client.http})
	if err != nil {
		s.apiError(w, err)
		return
	}
	defer agentConn.Close(websocket.StatusNormalClosure, "")
	browserConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer browserConn.Close(websocket.StatusNormalClosure, "")
	proxyType := "TCP"
	if port == 22 {
		proxyType = "SSH"
	}
	openedAt := time.Now()
	s.recordActivity(r.Context(), box.Box.ID, "access", proxyType+" proxy session opened", map[string]any{"port": port}, "ok")
	defer func() {
		duration := time.Since(openedAt).Milliseconds()
		s.recordActivityWithMetrics(r.Context(), box.Box.ID, "access", proxyType+" proxy session closed", map[string]any{"port": port}, &duration, "ok", time.Now().UTC())
	}()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	done := make(chan struct{}, 2)
	bridge := func(destination, source *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			messageType, data, err := source.Read(ctx)
			if err != nil {
				return
			}
			if err := destination.Write(ctx, messageType, data); err != nil {
				return
			}
		}
	}
	go bridge(agentConn, browserConn)
	go bridge(browserConn, agentConn)
	<-done
}

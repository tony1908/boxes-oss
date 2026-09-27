package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/boxes-oss/boxes/internal/diagnostics"
	"github.com/boxes-oss/boxes/internal/types"
)

type eventPump struct {
	ctx    context.Context
	cancel context.CancelFunc
}

const eventPumpLogInterval = 30 * time.Second
const terminalRunEventWait = 5 * time.Second

type boxdEventsResponse struct {
	Events []types.Event `json:"events"`
	Cursor int64         `json:"cursor"`
}

func (s *Server) syncEventPump(box types.Box) {
	if box.State != types.StateReady && box.State != types.StateIdle {
		s.stopEventPump(box.ID)
		return
	}
	s.platformMu.Lock()
	if s.closed {
		s.platformMu.Unlock()
		return
	}
	if _, exists := s.eventPumps[box.ID]; exists {
		s.platformMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.eventPumps[box.ID] = eventPump{ctx: ctx, cancel: cancel}
	s.eventWG.Add(1)
	s.platformMu.Unlock()
	go func() {
		defer s.eventWG.Done()
		s.runEventPump(ctx, box.ID)
	}()
}

func (s *Server) stopEventPump(boxID string) {
	s.platformMu.Lock()
	pump, exists := s.eventPumps[boxID]
	delete(s.eventPumps, boxID)
	s.platformMu.Unlock()
	if exists {
		pump.cancel()
	}
}

func (s *Server) runEventPump(ctx context.Context, boxID string) {
	defer func() {
		s.platformMu.Lock()
		if current, exists := s.eventPumps[boxID]; exists && current.ctx == ctx {
			delete(s.eventPumps, boxID)
		}
		s.platformMu.Unlock()
	}()
	for ctx.Err() == nil {
		box, err := s.store.GetBox(ctx, boxID)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, sql.ErrNoRows) {
				return
			}
			s.logEventPumpError(boxID, fmt.Errorf("load box: %w", err))
			if !sleepContext(ctx, time.Second) {
				return
			}
			continue
		}
		if box.Box.State != types.StateReady && box.Box.State != types.StateIdle {
			return
		}
		client, ok := newAgentClient(s.hub, box.Box.NodeID)
		if !ok {
			s.logEventPumpError(boxID, fmt.Errorf("node %s is not connected", box.Box.NodeID))
			if !sleepContext(ctx, time.Second) {
				return
			}
			continue
		}
		if err := s.fetchBoxdEvents(ctx, client, boxID, 25); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logEventPumpError(boxID, err)
			if !sleepContext(ctx, time.Second) {
				return
			}
		}
	}
}

func (s *Server) fetchBoxdEvents(ctx context.Context, client *agentClient, boxID string, wait int) error {
	after := s.boxdCursor(boxID)
	query := url.Values{}
	query.Set("after", strconv.FormatInt(after, 10))
	query.Set("wait", strconv.Itoa(wait))
	response, err := client.raw(ctx, http.MethodGet,
		agentPath("v1", "boxes", boxID, "boxd", "events")+"?"+query.Encode(), nil, "")
	if err != nil {
		return fmt.Errorf("request boxd events: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	_ = response.Body.Close()
	if readErr != nil {
		return fmt.Errorf("read boxd events response: %w", readErr)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(body))
		if len(message) > 512 {
			message = message[:512] + "..."
		}
		return fmt.Errorf("boxd events returned HTTP %d: %s", response.StatusCode, message)
	}
	var batch boxdEventsResponse
	if err := json.Unmarshal(body, &batch); err != nil {
		return fmt.Errorf("decode boxd events response: %w", err)
	}
	for _, event := range batch.Events {
		if event.Cursor <= after {
			continue
		}
		event.BoxID = boxID
		if event.ID == "" {
			event.ID = types.NewID("ev_")
		}
		if event.Time.IsZero() {
			event.Time = time.Now().UTC()
		}
		if event.Data == nil {
			event.Data = map[string]any{}
		}
		if err := s.ensureEventConversation(ctx, boxID, event); err != nil {
			s.logEventPumpError(boxID, fmt.Errorf("prepare conversation for event %s: %w", event.ID, err))
			continue
		}
		if _, err := s.store.CreateEvent(ctx, event); err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed: events.id") {
				after = event.Cursor
				s.setBoxdCursor(boxID, after)
			} else {
				s.logEventPumpError(boxID, fmt.Errorf("store event %s: %w", event.ID, err))
			}
			continue
		}
		s.applyBoxdEvent(ctx, event)
		after = event.Cursor
		s.setBoxdCursor(boxID, after)
	}
	if len(batch.Events) == 0 && batch.Cursor > after {
		s.setBoxdCursor(boxID, batch.Cursor)
	}
	return nil
}

func (s *Server) waitForTerminalRunEvent(ctx context.Context, boxID, runID string, client *agentClient) (types.Event, error) {
	event, err := s.store.GetTerminalRunEvent(ctx, boxID, runID)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return event, err
	}
	deadline := time.Now().Add(terminalRunEventWait)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		requestTimeout := time.Second
		if remaining < requestTimeout {
			requestTimeout = remaining
		}
		fetchCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		fetchErr := s.fetchBoxdEvents(fetchCtx, client, boxID, 0)
		cancel()
		if fetchErr != nil && ctx.Err() == nil {
			s.logEventPumpError(boxID, fetchErr)
		}
		event, err = s.store.GetTerminalRunEvent(ctx, boxID, runID)
		if err == nil || !errors.Is(err, sql.ErrNoRows) {
			return event, err
		}
		if ctx.Err() != nil {
			return types.Event{}, ctx.Err()
		}
		if !sleepContext(ctx, 100*time.Millisecond) {
			return types.Event{}, ctx.Err()
		}
	}
	return types.Event{}, sql.ErrNoRows
}

func (s *Server) logEventPumpError(boxID string, err error) {
	now := time.Now()
	s.platformMu.Lock()
	last := s.eventPumpLogAt[boxID]
	if !last.IsZero() && now.Sub(last) < eventPumpLogInterval {
		s.platformMu.Unlock()
		return
	}
	s.eventPumpLogAt[boxID] = now
	s.platformMu.Unlock()
	log.Printf("event pump for %s: %v", boxID, err)
}

func (s *Server) boxdCursor(boxID string) int64 {
	s.platformMu.Lock()
	defer s.platformMu.Unlock()
	return s.eventCursors[boxID]
}

func (s *Server) setBoxdCursor(boxID string, cursor int64) {
	s.platformMu.Lock()
	if cursor > s.eventCursors[boxID] {
		s.eventCursors[boxID] = cursor
	}
	s.platformMu.Unlock()
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	boxID := pathID(r)
	if _, err := s.store.GetBox(r.Context(), boxID); err != nil {
		s.apiError(w, err)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "invalid query parameters")
		return
	}
	after := int64(0)
	if value := query.Get("after"); value != "" {
		after, err = strconv.ParseInt(value, 10, 64)
		if err != nil || after < 0 {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "after must be a non-negative integer")
			return
		}
	}
	wait := 0
	if value := query.Get("wait"); value != "" {
		wait, err = strconv.Atoi(value)
		if err != nil || wait < 0 || wait > 30 {
			writeError(w, http.StatusBadRequest, "invalid_wait", "wait must be between 0 and 30 seconds")
			return
		}
	}
	conversationID := query.Get("conversation")
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		events, err := s.store.ListEvents(r.Context(), boxID, after, 500)
		if err != nil {
			s.apiError(w, err)
			return
		}
		filtered := make([]types.Event, 0, len(events))
		cursor := after
		for _, event := range events {
			cursor = event.Cursor
			if conversationID == "" || event.ConversationID == conversationID {
				filtered = append(filtered, event)
			}
		}
		if len(filtered) != 0 || wait == 0 || !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, map[string]any{"events": filtered, "cursor": cursor})
			return
		}
		if cursor > after {
			after = cursor
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.GetBox(r.Context(), pathID(r)); err != nil {
		s.apiError(w, err)
		return
	}
	client, err := s.boxAgent(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	var conversations []types.Conversation
	if err := client.json(r.Context(), http.MethodGet, agentPath("v1", "boxes", pathID(r), "boxd", "conversations"), nil, &conversations); err != nil {
		s.apiError(w, err)
		return
	}
	for index := range conversations {
		conversations[index].BoxID = pathID(r)
		if old, err := s.store.GetConversation(r.Context(), conversations[index].ID); err == nil {
			conversations[index].CreatedAt = old.CreatedAt
			_ = s.store.UpdateConversation(r.Context(), conversations[index])
		} else {
			_ = s.store.CreateConversation(r.Context(), conversations[index])
		}
	}
	writeJSON(w, http.StatusOK, conversations)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetRun(r.Context(), r.PathValue("runId"))
	if err != nil {
		s.apiError(w, err)
		return
	}
	conversation, err := s.store.GetConversation(r.Context(), run.ConversationID)
	if err != nil || conversation.BoxID != pathID(r) {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	client, err := s.boxAgent(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	var remote types.Run
	if err := client.json(r.Context(), http.MethodGet, agentPath("v1", "boxes", pathID(r), "boxd", "runs", run.ID), nil, &remote); err != nil {
		s.apiError(w, err)
		return
	}
	if remote.ID == "" {
		remote.ID = run.ID
	}
	if remote.ConversationID == "" {
		remote.ConversationID = run.ConversationID
	}
	if remote.CreatedAt.IsZero() {
		remote.CreatedAt = run.CreatedAt
	}
	if remote.StartedAt == nil {
		remote.StartedAt = run.StartedAt
	}
	if remote.FinishedAt == nil {
		remote.FinishedAt = run.FinishedAt
	}
	if remote.Status == "" {
		remote.Status = run.Status
	}
	event, eventErr := s.store.GetTerminalRunEvent(r.Context(), pathID(r), run.ID)
	if eventErr != nil && !errors.Is(eventErr, sql.ErrNoRows) {
		s.apiError(w, eventErr)
		return
	}
	if errors.Is(eventErr, sql.ErrNoRows) && isTerminalRunStatus(remote.Status) {
		event, eventErr = s.waitForTerminalRunEvent(r.Context(), pathID(r), run.ID, client)
	}
	if eventErr == nil {
		remote.Status = terminalEventRunStatus(event.Type)
		remote.FinishedAt = &event.Time
		remote.Error, _ = event.Data["error"].(string)
	} else if errors.Is(eventErr, sql.ErrNoRows) && isTerminalRunStatus(remote.Status) {
		remote.Status = "running"
		remote.Error = ""
		remote.FinishedAt = nil
	} else if eventErr != nil && !errors.Is(eventErr, sql.ErrNoRows) {
		s.apiError(w, eventErr)
		return
	}
	if err := s.store.UpdateRun(r.Context(), remote); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, remote)
}

func (s *Server) applyBoxdEvent(ctx context.Context, event types.Event) {
	if event.Type == "setup.status" {
		status, _ := event.Data["status"].(string)
		if status == "running" || status == "succeeded" || status == "failed" {
			if box, err := s.store.GetBox(ctx, event.BoxID); err == nil {
				box.SetupStatus = status
				_ = s.store.UpdateBox(ctx, box)
			}
		}
		return
	}
	conversationID := event.ConversationID
	if conversationID == "" {
		conversationID, _ = event.Data["conversationId"].(string)
	}
	if conversationID != "" {
		conversation, err := s.store.GetConversation(ctx, conversationID)
		if err == nil {
			switch event.Type {
			case "turn.started":
				conversation.Running = true
			case "turn.completed", "turn.failed", "turn.interrupted", "interrupted":
				conversation.Running = false
			default:
				return
			}
			conversation.UpdatedAt = event.Time
			_ = s.store.UpdateConversation(ctx, conversation)
		}
	}
	runID := event.RunID
	if runID == "" {
		runID, _ = event.Data["runId"].(string)
	}
	if runID == "" {
		return
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		conversationID := event.ConversationID
		if conversationID == "" {
			conversationID, _ = event.Data["conversationId"].(string)
		}
		if conversationID == "" {
			return
		}
		run = types.Run{ID: runID, ConversationID: conversationID, Status: "queued", CreatedAt: event.Time}
		if createErr := s.store.CreateRun(ctx, run); createErr != nil {
			return
		}
	}
	var finish *time.Time
	switch event.Type {
	case "turn.started":
		run.Status = "running"
		started := event.Time
		run.StartedAt = &started
	case "turn.completed":
		run.Status = "completed"
		finish = &event.Time
	case "turn.failed":
		run.Status = "failed"
		finish = &event.Time
		run.Error, _ = event.Data["error"].(string)
	case "turn.interrupted":
		run.Status = "interrupted"
		finish = &event.Time
	case "interrupted":
		run.Status = "interrupted"
		finish = &event.Time
	default:
		return
	}
	if finish != nil {
		run.FinishedAt = finish
	}
	if err := s.store.UpdateRun(ctx, run); err != nil {
		return
	}
	if finish != nil {
		queued, queuedErr := s.store.GetPromptRunQueuedActivity(ctx, run.ID)
		actor := actorFromContext(ctx)
		detail := map[string]any{"runId": run.ID}
		if queuedErr == nil {
			actor = activityActor{Type: queued.ActorType, ID: queued.ActorID, Label: queued.ActorLabel}
			var queuedDetail map[string]any
			if json.Unmarshal(queued.Detail, &queuedDetail) == nil {
				for _, key := range []string{"provider", "model", "promptPreview"} {
					if value, exists := queuedDetail[key]; exists {
						detail[key] = value
					}
				}
			}
		}
		start := run.CreatedAt
		if run.StartedAt != nil {
			start = *run.StartedAt
		}
		duration := int64(0)
		if event.Time.After(start) {
			duration = event.Time.Sub(start).Milliseconds()
		}
		action, outcome := "prompt run interrupted", "ok"
		switch event.Type {
		case "turn.completed":
			action = "prompt run completed"
		case "turn.failed":
			action, outcome = "prompt run failed", "error"
			if reason, ok := event.Data["error"].(string); ok && reason != "" {
				detail["reason"] = reason
			}
		}
		s.recordActivityAs(ctx, event.BoxID, actor, "agent", action, detail, &duration, outcome, event.Time)
		stopWhenDone, err := s.store.RunStopWhenDone(ctx, run.ID)
		if err == nil && stopWhenDone {
			go s.stopBoxAfterRun(event.BoxID)
		}
	}
}

func (s *Server) stopBoxAfterRun(boxID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	box, err := s.store.GetBox(ctx, boxID)
	if err != nil || (box.Box.State != types.StateReady && box.Box.State != types.StateIdle) {
		return
	}
	if err := s.waitSnapshot(ctx, boxID); err != nil {
		return
	}
	defer s.endSnapshot(boxID)
	previousState := box.Box.State
	box.Box.State = types.StateStopping
	if err := s.store.UpdateBox(ctx, box); err != nil {
		return
	}
	s.recordSystemActivity(ctx, boxID, "system", "lifecycle", "stop requested", map[string]any{"reason": "run completed"}, "ok")
	resumeAfterFailure := func() {
		if current, err := s.store.GetBox(context.Background(), boxID); err == nil && current.Box.State == types.StateStopping {
			current.Box.State = previousState
			_ = s.store.UpdateBox(context.Background(), current)
			s.syncEventPump(current.Box)
		}
	}
	client, ok := newAgentClient(s.hub, box.Box.NodeID)
	if !ok {
		resumeAfterFailure()
		return
	}
	var response struct {
		Snapshot struct {
			SnapshotID string    `json:"snapshotId"`
			SizeBytes  int64     `json:"sizeBytes"`
			CreatedAt  time.Time `json:"createdAt"`
		} `json:"snapshot"`
	}
	if err := client.json(ctx, http.MethodPost, agentPath("v1", "boxes", boxID, "stop"), map[string]bool{"snapshot": true}, &response); err != nil || response.Snapshot.SnapshotID == "" {
		reason := "agent returned no stop snapshot"
		if err != nil {
			reason = err.Error()
		}
		s.recordSystemActivity(ctx, boxID, "system", "lifecycle", "error", map[string]any{"reason": reason}, "error")
		resumeAfterFailure()
		return
	}
	now := time.Now().UTC()
	createdAt := response.Snapshot.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	if _, err := s.recordSnapshot(ctx, boxID, response.Snapshot.SnapshotID, response.Snapshot.SizeBytes, createdAt, s.storageConfig()); err != nil {
		now := time.Now().UTC()
		box.Box.State = types.StateError
		box.Box.ErrorCode = "snapshot_failed"
		box.Box.Error = diagnostics.Sanitize(err.Error())
		box.Box.StoppedAt = &now
		closeRunningInterval(&box, now)
		_ = s.store.UpdateBox(context.Background(), box)
		s.recordSystemActivity(ctx, boxID, "system", "lifecycle", "error", map[string]any{"reason": err.Error()}, "error")
		s.stopEventPump(boxID)
		return
	}
	box.Box.State = types.StateStopped
	box.Box.StoppedAt = &now
	closeRunningInterval(&box, now)
	if err := s.store.UpdateBox(ctx, box); err != nil {
		return
	}
	_ = s.store.CloseRunningInterval(ctx, boxID, now)
	s.recordSystemActivity(ctx, boxID, "system", "snapshot", "stop capture", map[string]any{"sizeBytes": response.Snapshot.SizeBytes}, "ok")
	s.recordSystemActivity(ctx, boxID, "system", "lifecycle", "stopped", map[string]any{"reason": "run completed"}, "ok")
	s.stopEventPump(boxID)
	s.emitWebhookEvent(context.Background(), "sandbox.stopped", map[string]any{"sandbox": box.Box, "reason": "run_completed"})
	s.queueRetentionDelete(ctx, box)
}

func (s *Server) ensureEventConversation(ctx context.Context, boxID string, event types.Event) error {
	conversationID := event.ConversationID
	if conversationID == "" {
		conversationID, _ = event.Data["conversationId"].(string)
	}
	if conversationID == "" {
		return nil
	}
	conversation, err := s.store.GetConversation(ctx, conversationID)
	if err == nil {
		if conversation.BoxID != boxID {
			return sql.ErrNoRows
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	provider, _ := event.Data["provider"].(string)
	if provider == "" {
		provider = "unknown"
	}
	conversation = types.Conversation{ID: conversationID, BoxID: boxID, Provider: provider, Current: true, CreatedAt: event.Time, UpdatedAt: event.Time}
	return s.store.CreateConversation(ctx, conversation)
}

func (s *Server) boxAgent(ctx context.Context, boxID string) (*agentClient, error) {
	box, err := s.store.GetBox(ctx, boxID)
	if err != nil {
		return nil, err
	}
	client, ok := newAgentClient(s.hub, box.Box.NodeID)
	if !ok {
		return nil, fmt.Errorf("box node is offline")
	}
	return client, nil
}

func isTerminalRunStatus(status string) bool {
	switch status {
	case "completed", "failed", "interrupted":
		return true
	default:
		return false
	}
}

func terminalEventRunStatus(eventType string) string {
	switch eventType {
	case "turn.completed":
		return "completed"
	case "turn.failed":
		return "failed"
	case "turn.interrupted", "interrupted":
		return "interrupted"
	default:
		return "running"
	}
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func eventPath(boxID, suffix string) string {
	return strings.TrimSuffix(agentPath("v1", "boxes", boxID, "boxd", suffix), "/")
}

// Package boxd implements the per-box agent API and provider conversation runtime.
package boxd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tony1908/paddock/internal/types"
)

// Config configures the local boxd service.
type Config struct {
	DBPath  string
	Token   string
	WorkDir string
	Harness Harness
}

type promptJob struct{ runID, prompt string }
type conversationRuntime struct {
	mu      sync.Mutex
	queue   chan promptJob
	session Session
	active  bool
}

// Service owns the durable outbox and active conversation workers.
type Service struct {
	config    Config
	outbox    *Outbox
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	enqueueMu sync.Mutex
	runtimes  map[string]*conversationRuntime
	closed    bool
}

// New opens the persistent outbox and starts an API service.
func New(config Config) (*Service, error) {
	if config.DBPath == "" {
		return nil, errors.New("boxd database path is required")
	}
	if config.Token == "" {
		return nil, errors.New("boxd token is required")
	}
	if config.Harness == nil {
		config.Harness = NewProviderRegistry(config.WorkDir)
	}
	store, err := OpenOutbox(config.DBPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{config: config, outbox: store, ctx: ctx, cancel: cancel, runtimes: make(map[string]*conversationRuntime)}, nil
}

// Close stops conversation workers and closes the outbox.
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cancel()
	for _, runtime := range s.runtimes {
		runtime.mu.Lock()
		session := runtime.session
		runtime.mu.Unlock()
		if session != nil {
			_ = session.Close()
		}
	}
	s.mu.Unlock()
	return s.outbox.Close()
}

// Handler returns the authenticated local boxd HTTP API.
func (s *Service) Handler() http.Handler { return s.handler() }

func (s *Service) enqueue(req promptRequest) (string, string, error) {
	s.enqueueMu.Lock()
	defer s.enqueueMu.Unlock()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return "", "", errors.New("boxd is shutting down")
	}
	provider := req.Provider
	if provider == "" {
		provider = "claude"
	}
	if !validAgentProvider(provider) {
		return "", "", fmt.Errorf("unsupported provider %q", provider)
	}
	var conversation Conversation
	if req.ConversationID != "" && !req.New {
		var err error
		conversation, err = s.outbox.Conversation(req.ConversationID)
		if err != nil {
			return "", "", fmt.Errorf("conversation not found")
		}
		if conversation.Provider != provider {
			return "", "", errors.New("provider does not match conversation")
		}
	} else {
		conversation = Conversation{ID: types.NewID("cv_"), Provider: provider, Model: req.Model, Current: true}
	}
	if req.Model != "" {
		conversation.Model = req.Model
	}
	conversation.Prompts++
	conversation.LastPrompt = req.Prompt
	if err := s.outbox.SaveConversation(conversation); err != nil {
		return "", "", err
	}
	run := Run{ID: types.NewID("run_"), ConversationID: conversation.ID, Status: "queued", CreatedAt: time.Now().UTC()}
	if err := s.outbox.SaveRun(run); err != nil {
		return "", "", err
	}
	if _, err := s.emit(conversation.ID, run.ID, "prompt", map[string]any{"prompt": req.Prompt, "provider": provider, "model": conversation.Model}); err != nil {
		return "", "", err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", "", errors.New("boxd is shutting down")
	}
	runtime := s.runtimes[conversation.ID]
	if runtime == nil {
		runtime = &conversationRuntime{queue: make(chan promptJob, 256)}
		s.runtimes[conversation.ID] = runtime
		go s.runConversation(conversation.ID, runtime)
	}
	s.mu.Unlock()
	select {
	case runtime.queue <- promptJob{runID: run.ID, prompt: req.Prompt}:
		return run.ID, conversation.ID, nil
	default:
		s.finish(run.ID, "failed", "conversation prompt queue is full")
		return "", "", errors.New("conversation prompt queue is full")
	}
}

func validAgentProvider(provider string) bool {
	switch provider {
	case "claude", "codex", "opencode", "pi", "prime", "kimi":
		return true
	}
	return false
}

func (s *Service) runConversation(id string, runtime *conversationRuntime) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case job := <-runtime.queue:
			s.runPrompt(id, runtime, job)
		}
	}
}

func (s *Service) runPrompt(id string, runtime *conversationRuntime, job promptJob) {
	conversation, err := s.outbox.Conversation(id)
	if err != nil {
		s.finish(job.runID, "failed", err.Error())
		return
	}
	runtime.mu.Lock()
	session := runtime.session
	if session == nil {
		session, err = s.config.Harness.Start(s.ctx, ConvSpec{ConversationID: id, Provider: conversation.Provider, Model: conversation.Model, SessionID: conversation.SessionID})
		if err != nil {
			runtime.mu.Unlock()
			s.finish(job.runID, "failed", err.Error())
			return
		}
		runtime.session = session
	}
	runtime.active = true
	runtime.mu.Unlock()
	now := time.Now().UTC()
	run, err := s.outbox.Run(job.runID)
	if err == nil {
		run.Status = "running"
		run.StartedAt = &now
		_ = s.outbox.SaveRun(run)
	}
	conversation.Running = true
	_ = s.outbox.SaveConversation(conversation)
	_, _ = s.emit(id, job.runID, "turn.started", map[string]any{})
	if err := session.Send(job.prompt); err != nil {
		runtime.mu.Lock()
		runtime.active = false
		runtime.mu.Unlock()
		s.finish(job.runID, "failed", err.Error())
		conversation.Running = false
		_ = s.outbox.SaveConversation(conversation)
		return
	}
	var message strings.Builder
	messageParts := make(map[string]string)
	var messagePartIDs []string
	for {
		select {
		case <-s.ctx.Done():
			return
		case event, ok := <-session.Events():
			if !ok {
				s.finish(job.runID, "failed", "harness event stream closed")
				runtime.mu.Lock()
				runtime.session = nil
				runtime.active = false
				runtime.mu.Unlock()
				conversation.Running = false
				_ = s.outbox.SaveConversation(conversation)
				return
			}
			if event.SessionID != "" {
				_ = s.outbox.SetSessionID(id, event.SessionID)
				conversation.SessionID = event.SessionID
			}
			if event.Type == "message.delta" {
				if text, ok := event.Data["text"].(string); ok {
					message.WriteString(text)
				}
			}
			if event.Type == "message.part" {
				partID, _ := event.Data["id"].(string)
				text, _ := event.Data["text"].(string)
				if partID != "" {
					if _, seen := messageParts[partID]; !seen {
						messagePartIDs = append(messagePartIDs, partID)
					}
					messageParts[partID] = text
				}
			}
			if event.Type == "turn.completed" && (message.Len() > 0 || len(messageParts) > 0) {
				fullMessage := message.String()
				for _, partID := range messagePartIDs {
					fullMessage += messageParts[partID]
				}
				_, _ = s.emitWithTime(id, job.runID, "message", map[string]any{"text": fullMessage}, event.Time)
			}
			if event.Type != "" {
				_, _ = s.emitWithTime(id, job.runID, event.Type, event.Data, event.Time)
			}
			switch event.Type {
			case "turn.completed":
				s.finish(job.runID, "completed", "")
				runtime.mu.Lock()
				runtime.active = false
				runtime.mu.Unlock()
				conversation.Running = false
				_ = s.outbox.SaveConversation(conversation)
				goto next
			case "turn.failed":
				message, _ := event.Data["error"].(string)
				s.finish(job.runID, "failed", message)
				runtime.mu.Lock()
				runtime.active = false
				runtime.mu.Unlock()
				conversation.Running = false
				_ = s.outbox.SaveConversation(conversation)
				goto next
			}
		}
	}
next:
}

func (s *Service) finish(runID, status, message string) {
	run, err := s.outbox.Run(runID)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	run.Status = status
	run.Error = message
	run.FinishedAt = &now
	_ = s.outbox.SaveRun(run)
}
func (s *Service) emit(conversationID, runID, eventType string, data map[string]any) (Event, error) {
	return s.emitWithTime(conversationID, runID, eventType, data, time.Time{})
}
func (s *Service) emitWithTime(conversationID, runID, eventType string, data map[string]any, at time.Time) (Event, error) {
	return s.outbox.AddEvent(Event{ID: types.NewID("ev_"), ConversationID: conversationID, RunID: runID, Type: eventType, Time: at, Data: data})
}

// ErrNotFound is returned for missing durable entities.
var ErrNotFound = errors.New("not found")

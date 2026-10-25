package boxd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CodexHarness runs Codex's app-server JSON-RPC protocol over stdio.
type CodexHarness struct {
	workDir  string
	executor ClaudeExecutor
}

func NewCodexHarness(workDir string) *CodexHarness {
	return &CodexHarness{workDir: workDir, executor: osClaudeExecutor{}}
}

func (h *CodexHarness) Start(ctx context.Context, spec ConvSpec) (Session, error) {
	workDir := h.workDir
	if workDir == "" {
		workDir = "/config"
	}
	env, err := loadHarnessEnv("/run/boxes/env")
	if err != nil {
		return nil, err
	}
	executor := h.executor
	if executor == nil {
		executor = osClaudeExecutor{}
	}
	process, err := executor.Start(ctx, codexArgs(), workDir, env)
	if err != nil {
		return nil, fmt.Errorf("start codex: %w", err)
	}
	session := &codexSession{
		process: process,
		stdin:   process.Stdin(),
		events:  make(chan HarnessEvent, 128),
		pending: make(map[uint64]chan codexResponse),
		queued:  make(chan string, 32),
		done:    make(chan struct{}),
	}
	go session.readMessages()
	if _, err := session.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "boxd", "version": "0.1.0"},
		"capabilities": map[string]any{},
	}); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("initialize codex: %w", err)
	}
	if err := session.notify("initialized", map[string]any{}); err != nil {
		_ = session.Close()
		return nil, err
	}
	method := "thread/start"
	params := map[string]any{"cwd": workDir, "approvalPolicy": "never", "sandbox": "danger-full-access"}
	if spec.Model != "" {
		params["model"] = spec.Model
	}
	if spec.SessionID != "" {
		method = "thread/resume"
		params["threadId"] = spec.SessionID
	}
	result, err := session.call(ctx, method, params)
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("start codex thread: %w", err)
	}
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &thread); err != nil || thread.Thread.ID == "" {
		_ = session.Close()
		return nil, errors.New("codex thread response has no id")
	}
	session.threadID = thread.Thread.ID
	session.emit(HarnessEvent{Type: "lifecycle", SessionID: session.threadID, Data: map[string]any{"provider": "codex"}})
	return session, nil
}

func codexArgs() []string { return []string{"codex", "app-server"} }

type codexResponse struct {
	result json.RawMessage
	err    error
}

type codexSession struct {
	process      ClaudeProcess
	stdin        io.WriteCloser
	events       chan HarnessEvent
	done         chan struct{}
	close        sync.Once
	doneOnce     sync.Once
	writeMu      sync.Mutex
	mu           sync.Mutex
	eventMu      sync.Mutex
	threadID     string
	turnID       string
	closed       bool
	eventsClosed bool
	nextID       atomic.Uint64
	pending      map[uint64]chan codexResponse
	queued       chan string
}

func (s *codexSession) Send(prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return errors.New("prompt is empty")
	}
	return s.startTurn(prompt)
}

func (s *codexSession) startTurn(prompt string) error {
	s.mu.Lock()
	threadID := s.threadID
	s.mu.Unlock()
	result, err := s.call(context.Background(), "turn/start", map[string]any{
		"threadId": threadID,
		"input":    []map[string]string{{"type": "text", "text": prompt}},
	})
	if err != nil {
		return err
	}
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(result, &response) == nil && response.Turn.ID != "" {
		s.mu.Lock()
		s.turnID = response.Turn.ID
		s.mu.Unlock()
	}
	return nil
}

func (s *codexSession) Steer(message string) error {
	s.mu.Lock()
	threadID, turnID := s.threadID, s.turnID
	s.mu.Unlock()
	if turnID == "" {
		return errors.New("codex turn is not active")
	}
	_, err := s.call(context.Background(), "turn/steer", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
		"input":    []map[string]string{{"type": "text", "text": message}},
	})
	if err == nil {
		return nil
	}
	select {
	case s.queued <- message:
		return nil
	case <-s.done:
		return errors.New("codex session is closed")
	default:
		return errors.New("codex steer queue is full")
	}
}

func (s *codexSession) Interrupt() error {
	s.mu.Lock()
	threadID, turnID := s.threadID, s.turnID
	s.mu.Unlock()
	if turnID == "" {
		return nil
	}
	_, err := s.call(context.Background(), "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
	return err
}

func (s *codexSession) Events() <-chan HarnessEvent { return s.events }
func (s *codexSession) SessionID() string           { s.mu.Lock(); defer s.mu.Unlock(); return s.threadID }

func (s *codexSession) Close() error {
	s.close.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.doneOnce.Do(func() { close(s.done) })
		_ = s.stdin.Close()
		_ = s.process.Kill()
	})
	return nil
}

func (s *codexSession) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextID.Add(1)
	response := make(chan codexResponse, 1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("codex session is closed")
	}
	s.pending[id] = response
	s.mu.Unlock()
	if err := s.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case result := <-response:
		return result.result, result.err
	case <-s.done:
		return nil, errors.New("codex session closed")
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	case <-timer.C:
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, errors.New("codex request timed out")
	}
}

func (s *codexSession) notify(method string, params any) error {
	return s.write(map[string]any{"method": method, "params": params})
}

func (s *codexSession) write(message any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.stdin.Write(encoded)
	return err
}

func (s *codexSession) readMessages() {
	defer s.closeEvents()
	defer s.doneOnce.Do(func() { close(s.done) })
	scanner := bufio.NewScanner(s.process.Stdout())
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var message map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			s.emit(HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": "invalid Codex JSON-RPC response"}})
			continue
		}
		_, hasResult := message["result"]
		_, hasError := message["error"]
		if id, ok := jsonNumberID(message["id"]); ok && (hasResult || hasError) {
			s.mu.Lock()
			response := s.pending[id]
			delete(s.pending, id)
			s.mu.Unlock()
			if response != nil {
				if rpcError, ok := message["error"].(map[string]any); ok {
					response <- codexResponse{err: fmt.Errorf("codex RPC %v: %v", rpcError["code"], rpcError["message"])}
				} else {
					result, _ := json.Marshal(message["result"])
					response <- codexResponse{result: result}
				}
			}
			continue
		}
		if method, ok := message["method"].(string); ok {
			if params, ok := message["params"].(map[string]any); ok {
				for _, event := range mapCodexNotification(method, params) {
					s.emit(event)
					if (event.Type == "turn.completed" || event.Type == "turn.failed") && event.Data["interrupted"] != true {
						select {
						case prompt := <-s.queued:
							go func() {
								if err := s.startTurn(prompt); err != nil {
									s.emit(HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}})
								}
							}()
						default:
						}
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		s.emit(HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}})
	}
	s.mu.Lock()
	for id, response := range s.pending {
		delete(s.pending, id)
		response <- codexResponse{err: errors.New("codex app-server closed")}
	}
	s.mu.Unlock()
	_ = s.process.Wait()
}

func (s *codexSession) emit(event HarnessEvent) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.eventsClosed {
		return
	}
	select {
	case s.events <- event:
	case <-s.done:
	}
}

func (s *codexSession) closeEvents() {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if !s.eventsClosed {
		s.eventsClosed = true
		close(s.events)
	}
}

func jsonNumberID(value any) (uint64, bool) {
	switch id := value.(type) {
	case float64:
		return uint64(id), true
	case json.Number:
		parsed, err := id.Int64()
		return uint64(parsed), err == nil
	default:
		return 0, false
	}
}

func mapCodexNotification(method string, params map[string]any) []HarnessEvent {
	text, _ := params["delta"].(string)
	switch method {
	case "item/agentMessage/delta":
		if text == "" {
			return nil
		}
		return []HarnessEvent{{Type: "message.delta", Data: map[string]any{"text": text}}}
	case "item/started", "item/completed":
		item, _ := params["item"].(map[string]any)
		id, _ := item["id"].(string)
		itemType, _ := item["type"].(string)
		if id == "" || itemType == "agentMessage" || itemType == "reasoning" || itemType == "plan" {
			return nil
		}
		title := map[string]string{"commandExecution": "Command", "fileChange": "File change", "mcpToolCall": "MCP tool", "webSearch": "Web search"}[itemType]
		if title == "" {
			title = "Tool"
		}
		var detail any
		for _, key := range []string{"command", "name", "path"} {
			if value, ok := item[key].(string); ok {
				detail = value
				break
			}
		}
		if method == "item/started" {
			input := map[string]any{}
			if detail != nil {
				input["command"] = detail
			}
			return []HarnessEvent{{Type: "tool.call", Data: map[string]any{"id": id, "name": title, "input": input}}}
		}
		status, _ := item["status"].(string)
		output := item["aggregatedOutput"]
		return []HarnessEvent{{Type: "tool.result", Data: map[string]any{"id": id, "output": output, "isError": status == "failed" || status == "declined"}}}
	case "turn/completed", "turn/failed":
		turn, _ := params["turn"].(map[string]any)
		status := turn["status"]
		if method == "turn/failed" || status == "failed" {
			message := ""
			if detail, ok := turn["error"].(map[string]any); ok {
				message, _ = detail["message"].(string)
			}
			return []HarnessEvent{{Type: "turn.failed", Data: map[string]any{"error": harnessErrorText("", message, "Codex turn failed")}}}
		}
		if status == "interrupted" {
			return []HarnessEvent{{Type: "turn.failed", Data: map[string]any{"error": "Codex turn interrupted", "interrupted": true}}}
		}
		if value, ok := status.(map[string]any); ok {
			if failure, exists := value["failed"]; exists {
				reason := ""
				if detail, ok := failure.(map[string]any); ok {
					reason, _ = detail["reason"].(string)
				}
				return []HarnessEvent{{Type: "turn.failed", Data: map[string]any{"error": harnessErrorText("", reason, "Codex turn failed")}}}
			}
		}
		return []HarnessEvent{{Type: "turn.completed"}}
	}
	return nil
}

type harnessRegistry struct{ providers map[string]Harness }

// NewProviderRegistry returns the default provider registry used by boxd.
func NewProviderRegistry(workDir string) Harness {
	return &harnessRegistry{providers: map[string]Harness{
		"claude":   NewClaudeHarness(workDir),
		"codex":    NewCodexHarness(workDir),
		"opencode": NewOpencodeHarness(workDir),
		"pi":       NewPiHarness(workDir),
		"kimi":     NewKimiHarness(workDir),
		"prime":    NewPrimeHarness(workDir),
	}}
}

func (r *harnessRegistry) Start(ctx context.Context, spec ConvSpec) (Session, error) {
	provider, ok := r.providers[spec.Provider]
	if !ok {
		return nil, fmt.Errorf("unsupported provider %q", spec.Provider)
	}
	return provider.Start(ctx, spec)
}

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
)

// PiHarness runs Pi or Prime Agent's shared RPC protocol over JSONL stdio.
type PiHarness struct {
	workDir, command string
	executor         ClaudeExecutor
}

func NewPiHarness(workDir string) *PiHarness {
	return &PiHarness{workDir: workDir, command: "pi", executor: osClaudeExecutor{}}
}
func NewPrimeHarness(workDir string) *PiHarness {
	return &PiHarness{workDir: workDir, command: "prime-agent", executor: osClaudeExecutor{}}
}
func (h *PiHarness) Start(ctx context.Context, spec ConvSpec) (Session, error) {
	dir := h.workDir
	if dir == "" {
		dir = "/config"
	}
	env, err := loadHarnessEnv("/run/boxes/env")
	if err != nil {
		return nil, err
	}
	args := piArgs(h.command, dir, spec)
	executor := h.executor
	if executor == nil {
		executor = osClaudeExecutor{}
	}
	p, err := executor.Start(ctx, args, dir, env)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", h.command, err)
	}
	s := &piSession{process: p, stdin: p.Stdin(), events: make(chan HarnessEvent, 128), done: make(chan struct{}), command: h.command}
	go s.readEvents()
	if err := s.write(map[string]any{"type": "get_state"}); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("read %s session state: %w", h.command, err)
	}
	return s, nil
}

func piArgs(command, dir string, spec ConvSpec) []string {
	args := []string{command, "--mode", "rpc", "--session-dir", dir + "/.boxd/" + command + "-sessions", "--extension", "npm:pi-mcp-extension"}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.SessionID != "" {
		args = append(args, "--session", spec.SessionID)
	}
	return args
}

type piSession struct {
	process ClaudeProcess
	stdin   io.WriteCloser
	events  chan HarnessEvent
	done    chan struct{}
	command string
	mu      sync.Mutex
	id      string
	closed  bool
}

func (s *piSession) write(v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session is closed")
	}
	_, e = s.stdin.Write(b)
	return e
}
func (s *piSession) Send(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("prompt is empty")
	}
	return s.write(map[string]any{"type": "prompt", "message": p})
}
func (s *piSession) Steer(p string) error {
	return s.write(map[string]any{"type": "steer", "message": p})
}
func (s *piSession) Interrupt() error            { return s.write(map[string]any{"type": "abort"}) }
func (s *piSession) Events() <-chan HarnessEvent { return s.events }
func (s *piSession) SessionID() string           { s.mu.Lock(); defer s.mu.Unlock(); return s.id }
func (s *piSession) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		_ = s.stdin.Close()
		_ = s.process.Kill()
	}
	s.mu.Unlock()
	return nil
}
func (s *piSession) readEvents() {
	defer close(s.events)
	scanner := bufio.NewScanner(s.process.Stdout())
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		ev, id, err := mapPiEvent(scanner.Bytes(), s.command)
		if err != nil {
			s.events <- HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}}
			continue
		}
		if id != "" {
			s.mu.Lock()
			s.id = id
			s.mu.Unlock()
		}
		for _, e := range ev {
			s.events <- e
		}
	}
	if err := scanner.Err(); err != nil {
		s.events <- HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}}
	}
	if err := s.process.Wait(); err != nil {
		s.events <- HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}}
	}
	close(s.done)
}
func mapPiEvent(line []byte, provider string) ([]HarnessEvent, string, error) {
	var item map[string]any
	if err := json.Unmarshal(line, &item); err != nil {
		return nil, "", fmt.Errorf("decode %s event: %w", provider, err)
	}
	id, _ := item["sessionId"].(string)
	if id == "" {
		if data, ok := item["data"].(map[string]any); ok {
			id, _ = data["sessionId"].(string)
		}
	}
	typ, _ := item["type"].(string)
	var out []HarnessEvent
	switch typ {
	case "agent_start":
		out = append(out, HarnessEvent{Type: "lifecycle", SessionID: id, Data: map[string]any{"provider": provider}})
	case "message_update":
		u, _ := item["assistantMessageEvent"].(map[string]any)
		if u != nil && u["type"] == "text_delta" {
			out = append(out, HarnessEvent{Type: "message.delta", SessionID: id, Data: map[string]any{"text": u["delta"]}})
		} else if u != nil && u["type"] == "toolcall_start" {
			tc, _ := u["toolCall"].(map[string]any)
			out = append(out, HarnessEvent{Type: "tool.call", SessionID: id, Data: map[string]any{"id": tc["id"], "name": tc["name"], "input": tc["arguments"]}})
		}
	case "tool_execution_end":
		out = append(out, HarnessEvent{Type: "tool.result", SessionID: id, Data: map[string]any{"id": item["toolCallId"], "output": item["result"], "isError": item["isError"]}})
	case "agent_end", "agent_settled":
		out = append(out, HarnessEvent{Type: "turn.completed", SessionID: id})
	case "response":
		if item["success"] == false {
			out = append(out, HarnessEvent{Type: "turn.failed", SessionID: id, Data: map[string]any{"error": item["error"]}})
		}
	}
	return out, id, nil
}

// KimiHarness uses Kimi Code's persistent stream-JSON print mode.
type KimiHarness struct {
	workDir  string
	executor ClaudeExecutor
}

func NewKimiHarness(workDir string) *KimiHarness {
	return &KimiHarness{workDir: workDir, executor: osClaudeExecutor{}}
}
func (h *KimiHarness) Start(ctx context.Context, spec ConvSpec) (Session, error) {
	dir := h.workDir
	if dir == "" {
		dir = "/config"
	}
	env, err := loadHarnessEnv("/run/boxes/env")
	if err != nil {
		return nil, err
	}
	args := kimiArgs(spec, dir)
	ex := h.executor
	if ex == nil {
		ex = osClaudeExecutor{}
	}
	p, err := ex.Start(ctx, args, dir, env)
	if err != nil {
		return nil, fmt.Errorf("start kimi: %w", err)
	}
	sessionID := spec.SessionID
	if sessionID == "" {
		sessionID = spec.ConversationID
	}
	s := &kimiSession{process: p, stdin: p.Stdin(), events: make(chan HarnessEvent, 128), id: sessionID}
	go s.readEvents()
	return s, nil
}

func kimiArgs(spec ConvSpec, dir string) []string {
	args := []string{"kimi", "--print", "--input-format=stream-json", "--output-format=stream-json", "--mcp-config-file", dir + "/.boxd/mcp.json"}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	sessionID := spec.SessionID
	if sessionID == "" {
		sessionID = spec.ConversationID
	}
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	return args
}

type kimiSession struct {
	process ClaudeProcess
	stdin   io.WriteCloser
	events  chan HarnessEvent
	id      string
	mu      sync.Mutex
	closed  bool
}

func (s *kimiSession) send(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("prompt is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("kimi session is closed")
	}
	b, _ := json.Marshal(map[string]string{"role": "user", "content": text})
	b = append(b, '\n')
	_, err := s.stdin.Write(b)
	return err
}
func (s *kimiSession) Send(v string) error         { return s.send(v) }
func (s *kimiSession) Steer(v string) error        { return s.send(v) }
func (s *kimiSession) Interrupt() error            { return s.process.Interrupt() }
func (s *kimiSession) Events() <-chan HarnessEvent { return s.events }
func (s *kimiSession) SessionID() string           { return s.id }
func (s *kimiSession) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		_ = s.stdin.Close()
	}
	s.mu.Unlock()
	return nil
}
func (s *kimiSession) readEvents() {
	defer close(s.events)
	sc := bufio.NewScanner(s.process.Stdout())
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		events, err := mapKimiEvent(sc.Bytes())
		if err != nil {
			s.events <- HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}}
			continue
		}
		for _, e := range events {
			s.events <- e
		}
	}
	if err := sc.Err(); err != nil {
		s.events <- HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}}
	}
	if err := s.process.Wait(); err != nil {
		s.events <- HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}}
	}
}
func mapKimiEvent(line []byte) ([]HarnessEvent, error) {
	var m map[string]any
	if e := json.Unmarshal(line, &m); e != nil {
		return nil, e
	}
	role, _ := m["role"].(string)
	content, _ := m["content"].(string)
	var out []HarnessEvent
	if role == "assistant" && content != "" {
		out = append(out, HarnessEvent{Type: "message.delta", Data: map[string]any{"text": content}})
	}
	if role == "assistant" {
		if calls, ok := m["tool_calls"].([]any); ok {
			for _, c := range calls {
				call, _ := c.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				out = append(out, HarnessEvent{Type: "tool.call", Data: map[string]any{"id": call["id"], "name": fn["name"], "input": fn["arguments"]}})
			}
			if len(calls) > 0 {
				return out, nil
			}
		}
	}
	if role == "tool" {
		out = append(out, HarnessEvent{Type: "tool.result", Data: map[string]any{"id": m["tool_call_id"], "output": m["content"], "isError": false}})
	}
	if role == "assistant" {
		out = append(out, HarnessEvent{Type: "turn.completed"})
	}
	return out, nil
}

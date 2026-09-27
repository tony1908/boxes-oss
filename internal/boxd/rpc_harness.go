package boxd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

// KimiHarness drives Kimi Code through its persistent ACP JSON-RPC interface.
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
	args := kimiArgs(spec)
	ex := h.executor
	if ex == nil {
		ex = osClaudeExecutor{}
	}
	p, err := ex.Start(ctx, args, dir, env)
	if err != nil {
		return nil, fmt.Errorf("start kimi: %w", err)
	}
	s := &kimiSession{process: p, stdin: p.Stdin(), events: make(chan HarnessEvent, 128), pending: make(map[int]chan kimiRPCMessage), promptIDs: make(map[int]bool), id: spec.SessionID}
	go s.readMessages()
	if _, err := s.request("initialize", map[string]any{"protocolVersion": 1, "clientInfo": map[string]string{"name": "boxes", "version": "1"}, "clientCapabilities": map[string]any{}}); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("initialize kimi ACP: %w", err)
	}
	if err := s.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("complete kimi ACP initialization: %w", err)
	}
	var mcp []map[string]any
	if data, readErr := os.ReadFile(dir + "/.boxd/mcp.json"); readErr == nil {
		var cfg struct {
			Servers map[string]map[string]any `json:"mcpServers"`
		}
		if json.Unmarshal(data, &cfg) == nil {
			for name, server := range cfg.Servers {
				server["name"] = name
				mcp = append(mcp, server)
			}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		_ = s.Close()
		return nil, readErr
	}
	if s.id != "" {
		_, err = s.request("session/load", map[string]any{"cwd": dir, "sessionId": s.id, "mcpServers": mcp})
	} else {
		var result json.RawMessage
		result, err = s.request("session/new", map[string]any{"cwd": dir, "mcpServers": mcp})
		if err == nil {
			var session struct {
				SessionID string `json:"sessionId"`
			}
			err = json.Unmarshal(result, &session)
			s.id = session.SessionID
		}
	}
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("create kimi ACP session: %w", err)
	}
	return s, nil
}

func kimiArgs(spec ConvSpec) []string {
	args := []string{"kimi"}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	return append(args, "acp")
}

type kimiRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type kimiSession struct {
	process   ClaudeProcess
	stdin     io.WriteCloser
	events    chan HarnessEvent
	id        string
	mu        sync.Mutex
	writeMu   sync.Mutex
	nextID    int
	pending   map[int]chan kimiRPCMessage
	promptIDs map[int]bool
	closed    bool
}

func (s *kimiSession) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err = s.stdin.Write([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(b)))); err != nil {
		return err
	}
	_, err = s.stdin.Write(b)
	return err
}
func (s *kimiSession) request(method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan kimiRPCMessage, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	if err := s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	msg, ok := <-ch
	if !ok {
		return nil, errors.New("kimi ACP process ended before replying")
	}
	if msg.Error != nil {
		return nil, errors.New(msg.Error.Message)
	}
	return msg.Result, nil
}
func (s *kimiSession) Send(text string) error  { return s.prompt(text) }
func (s *kimiSession) Steer(text string) error { return s.prompt(text) }
func (s *kimiSession) prompt(text string) error {
	s.mu.Lock()
	closed := s.closed
	sessionID := s.id
	s.mu.Unlock()
	if closed {
		return errors.New("kimi session is closed")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("prompt is empty")
	}
	id := s.nextRequestID()
	s.mu.Lock()
	s.pending[id] = make(chan kimiRPCMessage, 1)
	s.promptIDs[id] = true
	s.mu.Unlock()
	return s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/prompt", "params": map[string]any{"sessionId": sessionID, "prompt": []map[string]any{{"type": "text", "text": text}}}})
}
func (s *kimiSession) nextRequestID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return s.nextID
}
func (s *kimiSession) Interrupt() error {
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	return s.write(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": id}})
}
func (s *kimiSession) Events() <-chan HarnessEvent { return s.events }
func (s *kimiSession) SessionID() string           { s.mu.Lock(); defer s.mu.Unlock(); return s.id }
func (s *kimiSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.stdin.Close()
	_ = s.process.Interrupt()
	return nil
}
func (s *kimiSession) readMessages() {
	defer close(s.events)
	defer func() {
		s.mu.Lock()
		for id, ch := range s.pending {
			delete(s.pending, id)
			close(ch)
		}
		s.mu.Unlock()
	}()
	r := bufio.NewReader(s.process.Stdout())
	for {
		length := -1
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					s.events <- HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}}
				}
				goto done
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				fmt.Sscanf(strings.TrimSpace(strings.SplitN(line, ":", 2)[1]), "%d", &length)
			}
		}
		if length < 0 {
			continue
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(r, body); err != nil {
			s.events <- HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}}
			goto done
		}
		var msg kimiRPCMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			s.events <- HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}}
			continue
		}
		if msg.Method != "" {
			if msg.Method == "session/update" {
				events, _ := mapKimiACPEvent(msg.Params)
				for _, e := range events {
					e.SessionID = s.SessionID()
					s.events <- e
				}
			} else if msg.Method == "session/request_permission" {
				var req struct {
					Options []struct {
						OptionID string `json:"optionId"`
						Kind     string `json:"kind"`
					} `json:"options"`
				}
				_ = json.Unmarshal(msg.Params, &req)
				option := "allow_once"
				for _, o := range req.Options {
					if o.Kind == "allow_always" {
						option = o.OptionID
						break
					}
					if o.Kind == "allow_once" {
						option = o.OptionID
					}
				}
				_ = s.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg.ID), "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": option}}})
			}
			continue
		}
		if len(msg.ID) > 0 {
			var id int
			if json.Unmarshal(msg.ID, &id) == nil {
				s.mu.Lock()
				ch := s.pending[id]
				delete(s.pending, id)
				isPrompt := s.promptIDs[id]
				delete(s.promptIDs, id)
				s.mu.Unlock()
				if ch != nil {
					if isPrompt {
						if msg.Error != nil {
							s.events <- HarnessEvent{Type: "turn.failed", SessionID: s.SessionID(), Data: map[string]any{"error": msg.Error.Message}}
						} else {
							s.events <- HarnessEvent{Type: "turn.completed", SessionID: s.SessionID()}
						}
					} else {
						ch <- msg
					}
				}
			}
		}
	}
done:
	if err := s.process.Wait(); err != nil {
		s.events <- HarnessEvent{Type: "turn.failed", SessionID: s.SessionID(), Data: map[string]any{"error": err.Error(), "stderr": s.process.StderrTail()}}
	}
}

func mapKimiACPEvent(raw json.RawMessage) ([]HarnessEvent, error) {
	var p struct {
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Content       json.RawMessage `json:"content"`
			ToolCallID    string          `json:"toolCallId"`
			Title         string          `json:"title"`
			RawInput      any             `json:"rawInput"`
			Status        string          `json:"status"`
		} `json:"update"`
	}
	var envelope struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Params) != 0 {
		raw = envelope.Params
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	u := p.Update
	var out []HarnessEvent
	switch u.SessionUpdate {
	case "agent_message_chunk":
		var content struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(u.Content, &content) == nil && content.Text != "" {
			out = append(out, HarnessEvent{Type: "message.delta", Data: map[string]any{"text": content.Text}})
		}
	case "tool_call":
		out = append(out, HarnessEvent{Type: "tool.call", Data: map[string]any{"id": u.ToolCallID, "name": u.Title, "input": u.RawInput}})
	case "tool_call_update":
		if u.Status == "completed" || u.Status == "failed" {
			var content any
			_ = json.Unmarshal(u.Content, &content)
			out = append(out, HarnessEvent{Type: "tool.result", Data: map[string]any{"id": u.ToolCallID, "output": content, "isError": u.Status == "failed"}})
		}
	}
	return out, nil
}

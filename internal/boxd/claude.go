package boxd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ClaudeProcess is the process boundary used by the Claude stream-json adapter.
type ClaudeProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	StderrTail() string
	Wait() error
	Interrupt() error
	Kill() error
}

// ClaudeExecutor starts a harness command without a shell.
type ClaudeExecutor interface {
	Start(ctx context.Context, argv []string, dir string, env []string) (ClaudeProcess, error)
}

type osClaudeExecutor struct{}

func (osClaudeExecutor) Start(ctx context.Context, argv []string, dir string, env []string) (ClaudeProcess, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	stderrTail := &processStderrTail{}
	cmd.Stderr = stderrTail
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &osClaudeProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderrTail: stderrTail}, nil
}

type osClaudeProcess struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderrTail *processStderrTail
	waitOnce   sync.Once
	waitErr    error
}

func (p *osClaudeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *osClaudeProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *osClaudeProcess) StderrTail() string    { return p.stderrTail.String() }
func (p *osClaudeProcess) Wait() error {
	p.waitOnce.Do(func() { p.waitErr = p.cmd.Wait() })
	return p.waitErr
}

const processStderrTailLimit = 500

type processStderrTail struct {
	mu   sync.Mutex
	text []rune
}

func (b *processStderrTail) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.text = append(b.text, []rune(string(data))...)
	if len(b.text) > processStderrTailLimit {
		b.text = append([]rune(nil), b.text[len(b.text)-processStderrTailLimit:]...)
	}
	return len(data), nil
}

func (b *processStderrTail) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.text)
}
func (p *osClaudeProcess) Interrupt() error {
	if p.cmd.Process == nil {
		return errors.New("claude process is not running")
	}
	return p.cmd.Process.Signal(syscall.SIGINT)
}
func (p *osClaudeProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

// ClaudeHarness starts Claude Code using its persistent stream-json protocol.
type ClaudeHarness struct {
	workDir  string
	executor ClaudeExecutor
}

// NewClaudeHarness creates the production Claude adapter.
func NewClaudeHarness(workDir string) *ClaudeHarness {
	return &ClaudeHarness{workDir: workDir, executor: osClaudeExecutor{}}
}

// Start starts a Claude CLI process, resuming the persisted session when present.
func (h *ClaudeHarness) Start(ctx context.Context, spec ConvSpec) (Session, error) {
	workDir := h.workDir
	if workDir == "" {
		workDir = "/config"
	}
	configPath := filepath.Join(workDir, ".boxd", "mcp.json")
	args := claudeArgs(spec, configPath)
	env, err := loadHarnessEnv("/run/boxes/env")
	if err != nil {
		return nil, err
	}
	executor := h.executor
	if executor == nil {
		executor = osClaudeExecutor{}
	}
	process, err := executor.Start(ctx, args, workDir, env)
	if err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}
	session := &claudeSession{process: process, stdin: process.Stdin(), events: make(chan HarnessEvent, 64), done: make(chan struct{})}
	go session.readEvents()
	return session, nil
}

func claudeArgs(spec ConvSpec, mcpConfig string) []string {
	args := []string{"claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--mcp-config", mcpConfig}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.SessionID != "" {
		args = append(args, "--resume", spec.SessionID)
	}
	return args
}

type claudeSession struct {
	process   ClaudeProcess
	stdin     io.WriteCloser
	events    chan HarnessEvent
	done      chan struct{}
	mu        sync.Mutex
	sessionID string
	closed    bool
}

func (s *claudeSession) Send(prompt string) error   { return s.send(prompt) }
func (s *claudeSession) Steer(message string) error { return s.send(message) }
func (s *claudeSession) send(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("claude session is closed")
	}
	message := struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}{Type: "user"}
	message.Message.Role = "user"
	message.Message.Content = text
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = s.stdin.Write(encoded)
	return err
}
func (s *claudeSession) Interrupt() error            { return s.process.Interrupt() }
func (s *claudeSession) Events() <-chan HarnessEvent { return s.events }
func (s *claudeSession) SessionID() string           { s.mu.Lock(); defer s.mu.Unlock(); return s.sessionID }
func (s *claudeSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	_ = s.stdin.Close()
	s.mu.Unlock()
	_ = s.process.Interrupt()
	select {
	case <-s.done:
	case <-timeAfterSecond():
		_ = s.process.Kill()
	}
	return nil
}

func (s *claudeSession) readEvents() {
	defer close(s.done)
	defer close(s.events)
	scanner := bufio.NewScanner(s.process.Stdout())
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		events, err := parseClaudeLine(scanner.Bytes())
		if err != nil {
			s.events <- HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}}
			break
		}
		for _, event := range events {
			if event.SessionID != "" {
				s.mu.Lock()
				s.sessionID = event.SessionID
				s.mu.Unlock()
			}
			s.events <- event
		}
	}
	if err := scanner.Err(); err != nil {
		s.events <- HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}}
	}
	if err := s.process.Wait(); err != nil {
		s.events <- HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}}
	}
}

func parseClaudeLine(line []byte) ([]HarnessEvent, error) {
	var item struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		IsError   bool   `json:"is_error"`
		Result    any    `json:"result"`
		Message   struct {
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &item); err != nil {
		return nil, fmt.Errorf("decode claude stream event: %w", err)
	}
	switch item.Type {
	case "system":
		if item.Subtype == "init" {
			return []HarnessEvent{{Type: "lifecycle", SessionID: item.SessionID, Data: map[string]any{"subtype": item.Subtype}}}, nil
		}
	case "assistant":
		var events []HarnessEvent
		for _, raw := range item.Message.Content {
			var block struct {
				Type  string `json:"type"`
				Text  string `json:"text"`
				ID    string `json:"id"`
				Name  string `json:"name"`
				Input any    `json:"input"`
			}
			if err := json.Unmarshal(raw, &block); err != nil {
				return nil, err
			}
			switch block.Type {
			case "text":
				events = append(events, HarnessEvent{Type: "message.delta", Data: map[string]any{"text": block.Text}})
			case "tool_use":
				events = append(events, HarnessEvent{Type: "tool.call", Data: map[string]any{"id": block.ID, "name": block.Name, "input": block.Input}})
			}
		}
		return events, nil
	case "user":
		var events []HarnessEvent
		for _, raw := range item.Message.Content {
			var block struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Content   any    `json:"content"`
				IsError   bool   `json:"is_error"`
			}
			if err := json.Unmarshal(raw, &block); err != nil {
				return nil, err
			}
			if block.Type == "tool_result" {
				events = append(events, HarnessEvent{Type: "tool.result", Data: map[string]any{"toolUseID": block.ToolUseID, "content": block.Content, "isError": block.IsError}})
			}
		}
		return events, nil
	case "result":
		if item.IsError || item.Subtype != "success" {
			message, ok := item.Result.(string)
			if !ok || message == "" {
				message = item.Subtype
			}
			return []HarnessEvent{{Type: "turn.failed", Data: map[string]any{"error": message, "subtype": item.Subtype}}}, nil
		}
		return []HarnessEvent{{Type: "turn.completed", Data: map[string]any{"result": item.Result}}}, nil
	}
	return nil, nil
}

func loadHarnessEnv(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		result = append(result, key+"="+value)
	}
	return result, nil
}

func timeAfterSecond() <-chan struct{} {
	ch := make(chan struct{})
	go func() { time.Sleep(time.Second); close(ch) }()
	return ch
}

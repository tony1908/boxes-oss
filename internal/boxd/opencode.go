package boxd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OpencodeHarness runs an OpenCode HTTP server for each persistent session.
type OpencodeHarness struct {
	workDir  string
	executor ClaudeExecutor
	client   *http.Client
	baseURL  string
}

const opencodeReadyTimeout = 60 * time.Second

var errOpencodeNotReady = errors.New("opencode server did not become ready")

func NewOpencodeHarness(workDir string) *OpencodeHarness {
	return &OpencodeHarness{workDir: workDir, executor: osClaudeExecutor{}, client: &http.Client{}}
}

func (h *OpencodeHarness) Start(ctx context.Context, spec ConvSpec) (Session, error) {
	workDir := h.workDir
	if workDir == "" {
		workDir = "/config"
	}
	client := h.client
	if client == nil {
		client = &http.Client{}
	}
	baseURL := h.baseURL
	var process ClaudeProcess
	if baseURL == "" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("reserve opencode port: %w", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		env, err := loadHarnessEnv("/run/paddock/env")
		if err != nil {
			return nil, err
		}
		env = opencodeHarnessEnvironment(workDir, env)
		executor := h.executor
		if executor == nil {
			executor = osClaudeExecutor{}
		}
		process, err = executor.Start(ctx, []string{"opencode", "serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port)}, workDir, env)
		if err != nil {
			return nil, fmt.Errorf("start opencode: %w", err)
		}
		go func() {
			_, _ = io.Copy(io.Discard, process.Stdout())
			_ = process.Wait()
		}()
		baseURL = "http://127.0.0.1:" + strconv.Itoa(port)
		if err := waitForOpencode(ctx, client, baseURL); err != nil {
			_ = process.Kill()
			if errors.Is(err, errOpencodeNotReady) {
				_ = process.Wait()
				return nil, opencodeReadinessError(process.StderrTail())
			}
			return nil, err
		}
	}
	var sessionID = spec.SessionID
	if sessionID == "" {
		var created map[string]any
		if err := requestJSON(ctx, client, http.MethodPost, baseURL+"/session", map[string]any{"title": spec.ConversationID}, &created); err != nil {
			if process != nil {
				_ = process.Kill()
			}
			return nil, fmt.Errorf("create opencode session: %w", err)
		}
		sessionID, _ = created["id"].(string)
		if sessionID == "" {
			sessionID, _ = created["sessionID"].(string)
		}
		if sessionID == "" {
			if process != nil {
				_ = process.Kill()
			}
			return nil, errors.New("opencode session response has no id")
		}
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	session := &opencodeSession{
		ctx: sessionCtx, client: client, baseURL: strings.TrimRight(baseURL, "/"), sessionID: sessionID,
		model: spec.Model, process: process, cancel: cancel, events: make(chan HarnessEvent, 128),
		done: make(chan struct{}),
	}
	go session.readEvents(sessionCtx)
	session.emit(HarnessEvent{Type: "lifecycle", SessionID: sessionID, Data: map[string]any{"provider": "opencode"}})
	return session, nil
}

func opencodeHarnessEnvironment(workDir string, env []string) []string {
	return append(env, "HOME="+workDir, "XDG_CACHE_HOME=/tmp/paddock-opencode-cache")
}

func waitForOpencode(ctx context.Context, client *http.Client, baseURL string) error {
	deadline := time.NewTimer(opencodeReadyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		requestCtx, cancel := context.WithTimeout(ctx, time.Second)
		request, _ := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+"/global/health", nil)
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				cancel()
				return nil
			}
		}
		cancel()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errOpencodeNotReady
		case <-ticker.C:
		}
	}
}

func opencodeReadinessError(stderr string) error {
	tail := []rune(stderr)
	if len(tail) > processStderrTailLimit {
		tail = tail[len(tail)-processStderrTailLimit:]
	}
	if len(tail) == 0 {
		return errOpencodeNotReady
	}
	return fmt.Errorf("%w: %s", errOpencodeNotReady, string(tail))
}

func requestJSON(ctx context.Context, client *http.Client, method, target string, body any, output any) error {
	var encoded io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		encoded = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, encoded)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("OpenCode API returned %s: %s", response.Status, strings.TrimSpace(string(data)))
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

type opencodeSession struct {
	ctx          context.Context
	client       *http.Client
	baseURL      string
	sessionID    string
	model        string
	process      ClaudeProcess
	cancel       context.CancelFunc
	events       chan HarnessEvent
	done         chan struct{}
	close        sync.Once
	mu           sync.Mutex
	eventMu      sync.Mutex
	closed       bool
	eventsClosed bool
}

func (s *opencodeSession) Send(prompt string) error   { return s.send(prompt) }
func (s *opencodeSession) Steer(message string) error { return s.send(message) }
func (s *opencodeSession) send(prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return errors.New("prompt is empty")
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errors.New("opencode session is closed")
	}
	body := map[string]any{"parts": []map[string]string{{"type": "text", "text": prompt}}}
	if s.model != "" {
		if provider, model, ok := strings.Cut(s.model, "/"); ok {
			body["model"] = map[string]string{"providerID": provider, "modelID": model}
		} else {
			body["model"] = map[string]string{"modelID": s.model}
		}
	}
	go func() {
		// OpenCode returns the assistant message; a provider failure (expired key,
		// unknown model) is carried in info.error rather than as an HTTP error.
		var reply struct {
			Info struct {
				Error map[string]any `json:"error"`
			} `json:"info"`
		}
		target := s.baseURL + "/session/" + url.PathEscape(s.sessionID) + "/message"
		if err := requestJSON(s.ctx, s.client, http.MethodPost, target, body, &reply); err != nil {
			if s.ctx.Err() != nil {
				return
			}
			s.emit(HarnessEvent{Type: "turn.failed", SessionID: s.sessionID, Data: map[string]any{"error": harnessErrorText("", err.Error(), "OpenCode request failed")}})
		} else if reply.Info.Error != nil {
			s.emit(HarnessEvent{Type: "turn.failed", SessionID: s.sessionID, Data: map[string]any{"error": opencodeErrorText(reply.Info.Error)}})
		}
	}()
	return nil
}

func (s *opencodeSession) Interrupt() error {
	target := s.baseURL + "/session/" + url.PathEscape(s.sessionID) + "/abort"
	return requestJSON(s.ctx, s.client, http.MethodPost, target, map[string]any{}, nil)
}

func (s *opencodeSession) Events() <-chan HarnessEvent { return s.events }
func (s *opencodeSession) SessionID() string           { return s.sessionID }
func (s *opencodeSession) Close() error {
	s.close.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.cancel()
		close(s.done)
		if s.process != nil {
			_ = s.process.Kill()
		}
	})
	return nil
}

func (s *opencodeSession) readEvents(ctx context.Context) {
	defer s.closeEvents()
	mapper := newOpencodeEventMapper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/event", nil)
	if err != nil {
		s.emit(HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}})
		return
	}
	response, err := s.client.Do(request)
	if err != nil {
		if ctx.Err() == nil {
			s.emit(HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}})
		}
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.emit(HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": "OpenCode event stream returned " + response.Status}})
		return
	}
	reader := bufio.NewReader(response.Body)
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if strings.TrimSpace(line) == "" && data.Len() > 0 {
			for _, event := range mapper.mapEvent([]byte(data.String())) {
				if event.SessionID == "" || event.SessionID == s.sessionID {
					s.emit(event)
				}
			}
			data.Reset()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				s.emit(HarnessEvent{Type: "turn.failed", Data: map[string]any{"error": err.Error()}})
			}
			return
		}
	}
}

func (s *opencodeSession) emit(event HarnessEvent) {
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

func (s *opencodeSession) closeEvents() {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if !s.eventsClosed {
		s.eventsClosed = true
		close(s.events)
	}
}

func mapOpencodeEvent(line []byte) []HarnessEvent {
	return newOpencodeEventMapper().mapEvent(line)
}

type opencodeEventMapper struct {
	messageRoles map[string]string
	textParts    map[string]string
	toolCalls    map[string]bool
	toolResults  map[string]bool
}

func newOpencodeEventMapper() *opencodeEventMapper {
	return &opencodeEventMapper{
		messageRoles: make(map[string]string),
		textParts:    make(map[string]string),
		toolCalls:    make(map[string]bool),
		toolResults:  make(map[string]bool),
	}
}

func (m *opencodeEventMapper) mapEvent(line []byte) []HarnessEvent {
	var value map[string]any
	if json.Unmarshal(line, &value) != nil {
		return nil
	}
	if payload, ok := value["payload"].(map[string]any); ok {
		value = payload
	}
	properties, _ := value["properties"].(map[string]any)
	eventType, _ := value["type"].(string)
	sessionID, _ := value["sessionID"].(string)
	if sessionID == "" {
		sessionID, _ = properties["sessionID"].(string)
	}
	part, _ := value["part"].(map[string]any)
	if part == nil {
		part, _ = properties["part"].(map[string]any)
	}
	if sessionID == "" {
		sessionID, _ = part["sessionID"].(string)
	}
	partType, _ := part["type"].(string)
	if eventType == "message.updated" {
		info, _ := properties["info"].(map[string]any)
		if info == nil {
			info, _ = value["info"].(map[string]any)
		}
		messageID, _ := info["id"].(string)
		role, _ := info["role"].(string)
		if messageID != "" {
			m.messageRoles[messageID] = role
		}
		return nil
	}
	if eventType == "message.part.updated" && partType == "text" {
		messageID, _ := part["messageID"].(string)
		role := m.messageRoles[messageID]
		if role == "" {
			role, _ = part["role"].(string)
		}
		partID, _ := part["id"].(string)
		if role != "assistant" || partID == "" {
			return nil
		}
		partKey := sessionID + "\x00" + partID
		if text, ok := part["text"].(string); ok && m.textParts[partKey] != text {
			m.textParts[partKey] = text
			return []HarnessEvent{{Type: "message.part", SessionID: sessionID, Data: map[string]any{"id": partID, "text": text}}}
		}
		return nil
	}
	if eventType == "message.part.delta" {
		field, _ := properties["field"].(string)
		messageID, _ := properties["messageID"].(string)
		partID, _ := properties["partID"].(string)
		delta, _ := properties["delta"].(string)
		if field != "text" || m.messageRoles[messageID] != "assistant" || partID == "" || delta == "" {
			return nil
		}
		partKey := sessionID + "\x00" + partID
		m.textParts[partKey] += delta
		return []HarnessEvent{{Type: "message.part", SessionID: sessionID, Data: map[string]any{"id": partID, "text": m.textParts[partKey]}}}
	}
	switch eventType {
	case "text":
		text, _ := part["text"].(string)
		if text == "" {
			text, _ = properties["delta"].(string)
		}
		if text != "" {
			return []HarnessEvent{{Type: "message.delta", SessionID: sessionID, Data: map[string]any{"text": text}}}
		}
	case "tool_use", "message.part.updated":
		if eventType != "tool_use" && partType != "tool" {
			return nil
		}
		callID, _ := part["callID"].(string)
		if callID == "" {
			return nil
		}
		tool, _ := part["tool"].(string)
		state, _ := part["state"].(map[string]any)
		title, _ := state["title"].(string)
		if title == "" {
			title = tool
		}
		input := state["input"]
		callKey := sessionID + "\x00" + callID
		var result []HarnessEvent
		if !m.toolCalls[callKey] {
			m.toolCalls[callKey] = true
			result = append(result, HarnessEvent{Type: "tool.call", SessionID: sessionID, Data: map[string]any{"id": callID, "name": title, "input": input}})
		}
		if status, _ := state["status"].(string); (status == "completed" || status == "error") && !m.toolResults[callKey] {
			m.toolResults[callKey] = true
			result = append(result, HarnessEvent{Type: "tool.result", SessionID: sessionID, Data: map[string]any{"id": callID, "output": state["output"], "isError": status == "error"}})
		}
		return result
	case "session.idle":
		return []HarnessEvent{{Type: "turn.completed", SessionID: sessionID}}
	case "session.error", "error":
		detail, _ := properties["error"].(map[string]any)
		if detail == nil {
			detail, _ = value["error"].(map[string]any)
		}
		text := "OpenCode reported an error"
		if detail != nil {
			text = opencodeErrorText(detail)
		} else if message, ok := value["error"].(string); ok {
			text = harnessErrorText("", message, text)
		}
		return []HarnessEvent{{Type: "turn.failed", SessionID: sessionID, Data: map[string]any{"error": text}}}
	}
	return nil
}

// opencodeErrorText renders an OpenCode error object ({name, data: {message}}
// or {name, message}) as "name: message" via harnessErrorText.
func opencodeErrorText(detail map[string]any) string {
	name, _ := detail["name"].(string)
	message, _ := detail["message"].(string)
	if data, ok := detail["data"].(map[string]any); ok {
		if text, ok := data["message"].(string); ok && text != "" {
			message = text
		}
	}
	return harnessErrorText(name, message, "OpenCode reported an error")
}

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tony1908/paddock/internal/types"
)

func (s *Server) commands(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read command request")
		return
	}
	var input struct {
		Cmd            json.RawMessage   `json:"cmd"`
		CWD            string            `json:"cwd"`
		User           string            `json:"user"`
		Env            map[string]string `json:"env"`
		TimeoutSeconds int               `json:"timeoutSeconds"`
		Detached       bool              `json:"detached"`
	}
	if err := json.Unmarshal(body, &input); err != nil || len(input.Cmd) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "cmd is required")
		return
	}
	maxTimeout := 600
	if input.Detached {
		maxTimeout = 86400
	}
	if input.TimeoutSeconds < 0 || input.TimeoutSeconds > maxTimeout {
		writeError(w, http.StatusBadRequest, "invalid_timeout", fmt.Sprintf("timeoutSeconds must be between 0 and %d", maxTimeout))
		return
	}
	command := safeActivityText(commandString(input.Cmd), 200)
	s.forwardCommand(w, r, strings.NewReader(string(body)), command, input.CWD, input.Detached)
}

func commandString(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	switch command := value.(type) {
	case string:
		return command
	case []any:
		parts := make([]string, 0, len(command))
		for _, part := range command {
			parts = append(parts, fmt.Sprint(part))
		}
		return types.ShellJoin(parts)
	default:
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
}

type prefixBuffer struct {
	data []byte
	max  int
}

var commandExitCodeJSON = regexp.MustCompile(`"(?:exitCode|exit_code)"\s*:\s*(-?[0-9]+)`)

func (b *prefixBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	if len(b.data) < b.max {
		remaining := b.max - len(b.data)
		if len(value) > remaining {
			value = value[:remaining]
		}
		b.data = append(b.data, value...)
	}
	return originalLength, nil
}

func (s *Server) forwardCommand(w http.ResponseWriter, r *http.Request, body io.Reader, command, cwd string, detached bool) {
	boxID := pathID(r)
	started := time.Now()
	_, client, err := s.loadBoxAgent(r.Context(), boxID)
	if err != nil {
		duration := time.Since(started).Milliseconds()
		s.recordActivityWithMetrics(r.Context(), boxID, "command", "exec", map[string]any{"command": command, "cwd": cwd, "detached": detached, "reason": "agent unavailable"}, &duration, "error", time.Now().UTC())
		s.apiError(w, err)
		return
	}
	response, err := client.raw(r.Context(), http.MethodPost, agentPath("v1", "boxes", boxID, "exec"), body, "application/json")
	if err != nil {
		duration := time.Since(started).Milliseconds()
		s.recordActivityWithMetrics(r.Context(), boxID, "command", "exec", map[string]any{"command": command, "cwd": cwd, "detached": detached, "reason": "agent request failed"}, &duration, "error", time.Now().UTC())
		writeError(w, http.StatusBadGateway, "agent_unavailable", "agent request failed")
		return
	}
	defer func() { _ = response.Body.Close() }()
	copyResponseHeaders(w, response)
	w.WriteHeader(response.StatusCode)
	prefix := &prefixBuffer{max: 64 << 10}
	_, _ = io.Copy(w, io.TeeReader(response.Body, prefix))
	duration := time.Since(started).Milliseconds()
	detail := map[string]any{"command": command, "cwd": cwd, "detached": detached}
	var result map[string]any
	if json.Unmarshal(prefix.data, &result) == nil {
		for _, key := range []string{"exitCode", "exit_code"} {
			if value, ok := result[key].(float64); ok {
				detail["exitCode"] = int(value)
				break
			}
		}
		if processID, ok := result["processId"].(string); ok && processID != "" {
			detail["processId"] = processID
		}
	}
	if _, exists := detail["exitCode"]; !exists {
		if match := commandExitCodeJSON.FindSubmatch(prefix.data); len(match) == 2 {
			if code, parseErr := strconv.Atoi(string(match[1])); parseErr == nil {
				detail["exitCode"] = code
			}
		}
	}
	outcome := "ok"
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		outcome = "error"
	}
	if code, ok := detail["exitCode"].(int); ok && code != 0 {
		outcome = "error"
	}
	s.recordActivityWithMetrics(r.Context(), boxID, "command", "exec", detail, &duration, outcome, time.Now().UTC())
}

func (s *Server) commandResult(w http.ResponseWriter, r *http.Request) {
	boxID, processID := pathID(r), r.PathValue("pid")
	_, client, err := s.loadBoxAgent(r.Context(), boxID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	response, err := client.raw(r.Context(), http.MethodGet, agentPath("v1", "boxes", boxID, "exec", processID), nil, "")
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent_unavailable", "agent request failed")
		return
	}
	defer func() { _ = response.Body.Close() }()
	copyResponseHeaders(w, response)
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent_unavailable", "agent response could not be read")
		return
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return
	}
	var result struct {
		Running  bool `json:"running"`
		ExitCode *int `json:"exitCode"`
	}
	if json.Unmarshal(body, &result) == nil && !result.Running && result.ExitCode != nil {
		_ = s.store.UpdateCommandActivityResult(r.Context(), boxID, processID, *result.ExitCode, time.Now().UTC())
	}
}

// listCommands returns the detached commands the agent still remembers for the box.
func (s *Server) listCommands(w http.ResponseWriter, r *http.Request) {
	boxID := pathID(r)
	_, client, err := s.loadBoxAgent(r.Context(), boxID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	var commands json.RawMessage
	if err := client.json(r.Context(), http.MethodGet, agentPath("v1", "boxes", boxID, "exec"), nil, &commands); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]json.RawMessage{"commands": commands})
}

// commandLogs streams the agent's NDJSON log lines without buffering, flushing each chunk.
func (s *Server) commandLogs(w http.ResponseWriter, r *http.Request) {
	boxID, processID := pathID(r), r.PathValue("pid")
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "invalid query parameters")
		return
	}
	_, client, err := s.loadBoxAgent(r.Context(), boxID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	endpoint := agentPath("v1", "boxes", boxID, "exec", processID, "logs") + "?" + query.Encode()
	response, err := client.raw(r.Context(), http.MethodGet, endpoint, nil, "")
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent_unavailable", "agent request failed")
		return
	}
	defer func() { _ = response.Body.Close() }()
	copyResponseHeaders(w, response)
	w.WriteHeader(response.StatusCode)
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
			_ = controller.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

// killCommand relays a signal to a running detached command's process group.
func (s *Server) killCommand(w http.ResponseWriter, r *http.Request) {
	boxID, processID := pathID(r), r.PathValue("pid")
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read kill request")
		return
	}
	var input struct {
		Signal string `json:"signal"`
	}
	_ = json.Unmarshal(body, &input)
	signal := input.Signal
	if signal == "" {
		signal = "TERM"
	}
	_, client, err := s.loadBoxAgent(r.Context(), boxID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	response, err := client.raw(r.Context(), http.MethodPost, agentPath("v1", "boxes", boxID, "exec", processID, "kill"), strings.NewReader(string(body)), "application/json")
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent_unavailable", "agent request failed")
		return
	}
	defer func() { _ = response.Body.Close() }()
	copyResponseHeaders(w, response)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
	outcome := "ok"
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		outcome = "error"
	}
	s.recordActivity(r.Context(), boxID, "command", "kill", map[string]any{"processId": processID, "signal": safeActivityText(signal, 16)}, outcome)
}

// commandStdin streams the raw request body to a detached command's stdin. Stdin contents are never recorded.
func (s *Server) commandStdin(w http.ResponseWriter, r *http.Request) {
	boxID, processID := pathID(r), r.PathValue("pid")
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "invalid query parameters")
		return
	}
	_, client, err := s.loadBoxAgent(r.Context(), boxID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	const maxStdin = 1 << 20
	body := &byteCounter{reader: http.MaxBytesReader(w, r.Body, maxStdin)}
	endpoint := agentPath("v1", "boxes", boxID, "exec", processID, "stdin") + "?" + query.Encode()
	response, err := client.raw(r.Context(), http.MethodPost, endpoint, body, r.Header.Get("Content-Type"))
	if err != nil {
		if body.count >= maxStdin {
			writeError(w, http.StatusRequestEntityTooLarge, "stdin_too_large", "stdin write exceeds 1 MiB")
			return
		}
		writeError(w, http.StatusBadGateway, "agent_unavailable", "agent request failed")
		return
	}
	defer func() { _ = response.Body.Close() }()
	copyResponseHeaders(w, response)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
	outcome := "ok"
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		outcome = "error"
	}
	s.recordActivity(r.Context(), boxID, "command", "stdin", map[string]any{"processId": processID, "sizeBytes": body.count, "close": query.Get("close") == "1"}, outcome)
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	s.forwardFile(w, r, http.MethodGet)
}

func (s *Server) putFile(w http.ResponseWriter, r *http.Request) {
	s.forwardFile(w, r, http.MethodPut)
}

func (s *Server) putArchive(w http.ResponseWriter, r *http.Request) {
	boxID := pathID(r)
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "invalid query parameters")
		return
	}
	_, client, err := s.loadBoxAgent(r.Context(), boxID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	body := &byteCounter{reader: r.Body}
	response, err := client.raw(r.Context(), http.MethodPut, agentPath("v1", "boxes", boxID, "archive")+"?"+query.Encode(), body, "application/gzip")
	if err != nil {
		s.apiError(w, err)
		return
	}
	defer func() { _ = response.Body.Close() }()
	copyResponseHeaders(w, response)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
	outcome := "ok"
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		outcome = "error"
	}
	s.recordActivity(r.Context(), boxID, "access", "files pushed", map[string]any{"path": safeActivityText(query.Get("path"), 0), "sizeBytes": body.count}, outcome)
}

func (s *Server) getArtifacts(w http.ResponseWriter, r *http.Request) {
	boxID := pathID(r)
	artifactPath := r.URL.Query().Get("path")
	if artifactPath == "" || !strings.HasPrefix(artifactPath, "/") || strings.Contains(artifactPath, "..") {
		writeError(w, http.StatusBadRequest, "invalid_path", "path must be an absolute path without traversal")
		return
	}
	if record, err := s.store.GetBox(r.Context(), boxID); err == nil && record.State != types.StateReady && record.State != types.StateIdle {
		writeError(w, http.StatusConflict, "sandbox_not_running", "sandbox must be running to download artifacts")
		return
	}
	_, client, err := s.loadBoxAgent(r.Context(), boxID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	query := url.Values{"path": []string{artifactPath}}
	response, err := client.raw(r.Context(), http.MethodGet, agentPath("v1", "boxes", boxID, "artifacts")+"?"+query.Encode(), nil, "")
	if err != nil {
		s.apiError(w, err)
		return
	}
	defer func() { _ = response.Body.Close() }()
	copyResponseHeaders(w, response)
	w.WriteHeader(response.StatusCode)
	_, copyErr := io.Copy(w, response.Body)
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		outcome := "ok"
		if copyErr != nil {
			outcome = "error"
		}
		s.recordActivity(r.Context(), boxID, "access", "downloaded", map[string]any{"path": safeActivityText(artifactPath, 0)}, outcome)
	}
}

type byteCounter struct {
	reader io.Reader
	count  int64
}

func (c *byteCounter) Read(value []byte) (int, error) {
	n, err := c.reader.Read(value)
	c.count += int64(n)
	return n, err
}

func (s *Server) forwardFile(w http.ResponseWriter, r *http.Request, method string) {
	boxID := pathID(r)
	pathValue := safeActivityText(r.URL.Query().Get("path"), 0)
	endpoint := agentPath("v1", "boxes", boxID, "files")
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "invalid query parameters")
		return
	}
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	_, client, err := s.loadBoxAgent(r.Context(), boxID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	var body io.Reader
	var uploaded *byteCounter
	if method == http.MethodPut {
		uploaded = &byteCounter{reader: r.Body}
		body = uploaded
	}
	response, err := client.raw(r.Context(), method, endpoint, body, r.Header.Get("Content-Type"))
	if err != nil {
		s.apiError(w, err)
		return
	}
	defer func() { _ = response.Body.Close() }()
	copyResponseHeaders(w, response)
	w.WriteHeader(response.StatusCode)
	transferred := &byteCounter{reader: response.Body}
	_, _ = io.Copy(w, transferred)
	size := transferred.count
	if uploaded != nil {
		size = uploaded.count
	}
	action := "file read"
	if method == http.MethodPut {
		action = "file written"
	}
	outcome := "ok"
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		outcome = "error"
	}
	s.recordActivity(r.Context(), boxID, "access", action, map[string]any{"path": pathValue, "sizeBytes": size}, outcome)
}

func copyResponseHeaders(w http.ResponseWriter, response *http.Response) {
	for key, values := range response.Header {
		if strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Connection") || strings.EqualFold(key, "Transfer-Encoding") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, method, endpoint string, body io.Reader, contentType string) {
	_, client, err := s.loadBoxAgent(r.Context(), pathID(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "invalid query parameters")
		return
	}
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	response, err := client.raw(r.Context(), method, endpoint, body, contentType)
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent_unavailable", "agent request failed")
		return
	}
	defer func() { _ = response.Body.Close() }()
	for key, values := range response.Header {
		if strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Connection") || strings.EqualFold(key, "Transfer-Encoding") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if response.StatusCode != http.StatusNoContent {
		_, _ = io.Copy(w, response.Body)
	}
}

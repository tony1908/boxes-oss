package boxd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type testHarness struct {
	session *testSession
	script  []HarnessEvent
}

func (h *testHarness) Start(_ context.Context, spec ConvSpec) (Session, error) {
	h.session = &testSession{events: make(chan HarnessEvent, 8), sessionID: "claude-session-1", spec: spec, script: h.script}
	return h.session, nil
}

type testSession struct {
	events    chan HarnessEvent
	sessionID string
	spec      ConvSpec
	closed    bool
	script    []HarnessEvent
}

func (s *testSession) Send(prompt string) error {
	if len(s.script) == 0 {
		s.events <- HarnessEvent{Type: "message.delta", Data: map[string]any{"text": prompt}}
	} else {
		for _, event := range s.script {
			s.events <- event
		}
	}
	s.events <- HarnessEvent{Type: "turn.completed"}
	return nil
}

func TestServiceConsolidatesRepeatedFullMessagePartUpdates(t *testing.T) {
	harness := &testHarness{script: []HarnessEvent{
		{Type: "message.part", Data: map[string]any{"id": "part-1", "text": "paddock-"}},
		{Type: "message.part", Data: map[string]any{"id": "part-1", "text": "paddock-oops"}},
		{Type: "message.part", Data: map[string]any{"id": "part-1", "text": "paddock-ok"}},
		{Type: "message.part", Data: map[string]any{"id": "part-1", "text": "paddock-ok"}},
	}}
	service, err := New(Config{DBPath: t.TempDir() + "/boxd.db", Token: "box-secret", Harness: harness})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()

	request := httptest.NewRequest(http.MethodPost, "/prompt", strings.NewReader(`{"prompt":"Reply with exactly: paddock-ok","provider":"opencode"}`))
	request.Header.Set("Authorization", "Bearer box-secret")
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("POST /prompt status = %d: %s", response.Code, response.Body.String())
	}
	var created struct {
		ConversationID string `json:"conversationId"`
		RunID          string `json:"runId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := service.outbox.Run(created.RunID)
		if err == nil && run.Status == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, err := service.outbox.Run(created.RunID)
	if err != nil || run.Status != "completed" {
		t.Fatalf("run = %#v, err = %v; want completed", run, err)
	}
	events, err := service.outbox.Events(0, created.ConversationID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	for _, event := range events {
		if event.Type == "message" {
			text, _ := event.Data["text"].(string)
			messages = append(messages, text)
		}
	}
	if len(messages) != 1 || messages[0] != "paddock-ok" || strings.Contains(messages[0], "Reply with exactly") {
		t.Fatalf("consolidated messages = %#v, want one assistant message paddock-ok", messages)
	}
}

func TestOpencodeRealSSEReplayConsolidatesAssistantMessageOnce(t *testing.T) {
	trace, err := os.ReadFile("testdata/opencode_sse_real.txt")
	if err != nil {
		t.Fatal(err)
	}
	for lineNumber, line := range strings.Split(string(trace), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			t.Fatalf("invalid SSE event on line %d: %v", lineNumber+1, err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"ses_f2a57c319ffeohGB1q8yqML1nQ"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(trace)
		case r.Method == http.MethodPost && r.URL.Path == "/session/ses_f2a57c319ffeohGB1q8yqML1nQ/message":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	harness := NewOpencodeHarness(t.TempDir())
	harness.baseURL = server.URL
	harness.client = server.Client()
	service, err := New(Config{DBPath: t.TempDir() + "/boxd.db", Token: "box-secret", Harness: harness})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()

	request := httptest.NewRequest(http.MethodPost, "/prompt", strings.NewReader(`{"prompt":"reply with exactly: paddock-ok","provider":"opencode","model":"google/gemini-3.8-flash"}`))
	request.Header.Set("Authorization", "Bearer box-secret")
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("POST /prompt status = %d: %s", response.Code, response.Body.String())
	}
	var created struct {
		ConversationID string `json:"conversationId"`
		RunID          string `json:"runId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := service.outbox.Run(created.RunID)
		if err == nil && run.Status == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, err := service.outbox.Run(created.RunID)
	if err != nil || run.Status != "completed" {
		t.Fatalf("run = %#v, err = %v; want completed", run, err)
	}
	events, err := service.outbox.Events(0, created.ConversationID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	for _, event := range events {
		if event.Type == "message" {
			text, _ := event.Data["text"].(string)
			messages = append(messages, text)
		}
	}
	if len(messages) != 1 || messages[0] != "paddock-ok" || strings.Contains(messages[0], "reply with exactly") {
		t.Fatalf("consolidated messages = %#v, want one assistant message paddock-ok and no user text", messages)
	}
}

func (s *testSession) Steer(string) error          { return nil }
func (s *testSession) Interrupt() error            { return nil }
func (s *testSession) Events() <-chan HarnessEvent { return s.events }
func (s *testSession) SessionID() string           { return s.sessionID }
func (s *testSession) Close() error                { s.closed = true; return nil }

func TestServiceRequiresBearerExceptHealthAndPersistsConversation(t *testing.T) {
	dbPath := t.TempDir() + "/boxd.db"
	harness := &testHarness{}
	service, err := New(Config{DBPath: dbPath, Token: "box-secret", Harness: harness})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	defer func() { _ = service.Close() }()

	response, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", response.StatusCode)
	}
	response, err = http.Get(server.URL + "/conversations")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated conversations status = %d, want 401", response.StatusCode)
	}

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/prompt", strings.NewReader(`{"prompt":"inspect the page","provider":"claude"}`))
	request.Header.Set("Authorization", "Bearer box-secret")
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		RunID          string `json:"runId"`
		ConversationID string `json:"conversationId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted || !strings.HasPrefix(created.RunID, "run_") || !strings.HasPrefix(created.ConversationID, "cv_") {
		t.Fatalf("prompt response = status %d, %+v", response.StatusCode, created)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		request, _ = http.NewRequest(http.MethodGet, server.URL+"/runs/"+created.RunID, nil)
		request.Header.Set("Authorization", "Bearer box-secret")
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var run struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(response.Body).Decode(&run)
		_ = response.Body.Close()
		if run.Status == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	request, _ = http.NewRequest(http.MethodGet, server.URL+"/runs/"+created.RunID, nil)
	request.Header.Set("Authorization", "Bearer box-secret")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var run struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(response.Body).Decode(&run)
	_ = response.Body.Close()
	if run.Status != "completed" {
		t.Fatalf("run status = %q, want completed", run.Status)
	}
	eventsRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/events?after=0&conversation="+created.ConversationID, nil)
	eventsRequest.Header.Set("Authorization", "Bearer box-secret")
	eventsResponse, err := http.DefaultClient.Do(eventsRequest)
	if err != nil {
		t.Fatal(err)
	}
	var eventList struct {
		Events []struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		} `json:"events"`
	}
	if err := json.NewDecoder(eventsResponse.Body).Decode(&eventList); err != nil {
		_ = eventsResponse.Body.Close()
		t.Fatal(err)
	}
	_ = eventsResponse.Body.Close()
	messageFound := false
	for _, event := range eventList.Events {
		if event.Type == "message" && event.Data["text"] == "inspect the page" {
			messageFound = true
		}
	}
	if !messageFound {
		t.Fatalf("events = %#v, want completed message event", eventList.Events)
	}
	if harness.session.spec.ConversationID != created.ConversationID {
		t.Fatalf("harness conversation = %q, want %q", harness.session.spec.ConversationID, created.ConversationID)
	}
}

func TestServiceAcceptsCodexAndOpencodeProviders(t *testing.T) {
	for _, provider := range []string{"codex", "opencode"} {
		t.Run(provider, func(t *testing.T) {
			service, err := New(Config{DBPath: t.TempDir() + "/boxd.db", Token: "box-secret", Harness: &testHarness{}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = service.Close() }()

			request := httptest.NewRequest(http.MethodPost, "/prompt", strings.NewReader(`{"prompt":"say hi","provider":"`+provider+`"}`))
			request.Header.Set("Authorization", "Bearer box-secret")
			response := httptest.NewRecorder()
			service.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusAccepted {
				t.Fatalf("POST /prompt with %s returned status %d: %s", provider, response.Code, response.Body.String())
			}
		})
	}
}

func TestOutboxRetainsClaudeSessionIDAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/boxd.db"
	store, err := OpenOutbox(path)
	if err != nil {
		t.Fatal(err)
	}
	conversation := Conversation{ID: "cv_resume", Provider: "claude", Model: "sonnet"}
	if err := store.SaveConversation(conversation); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSessionID(conversation.ID, "session-after-restart"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenOutbox(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	got, err := store.Conversation(conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "session-after-restart" {
		t.Fatalf("session id = %q, want persisted session", got.SessionID)
	}
}

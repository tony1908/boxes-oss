package boxd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProviderRegistryRegistersCodexAndOpencode(t *testing.T) {
	registry, ok := NewProviderRegistry(t.TempDir()).(*harnessRegistry)
	if !ok {
		t.Fatal("default provider registry has unexpected type")
	}
	for _, provider := range []string{"claude", "codex", "opencode"} {
		if registry.providers[provider] == nil {
			t.Errorf("provider %q is not registered", provider)
		}
	}
}

func TestOpencodeHarnessUsesWritableCacheOutsideConfigHome(t *testing.T) {
	env := opencodeHarnessEnvironment("/config", []string{"BOXES_TEST=value"})
	if !containsEnvValue(env, "HOME", "/config") {
		t.Fatalf("OpenCode HOME env = %#v, want /config", env)
	}
	if !containsEnvValue(env, "XDG_CACHE_HOME", "/tmp/boxes-opencode-cache") {
		t.Fatalf("OpenCode XDG_CACHE_HOME env = %#v, want writable /tmp cache", env)
	}
}

func TestOpencodeReadinessErrorIncludesLast500CharactersOfStderr(t *testing.T) {
	stderr := strings.Repeat("x", 501)
	err := opencodeReadinessError(stderr)
	if err == nil {
		t.Fatal("opencodeReadinessError() returned nil")
	}
	message := err.Error()
	if !strings.HasPrefix(message, "opencode server did not become ready: ") {
		t.Fatalf("readiness error = %q, want startup error prefix", message)
	}
	tail := strings.TrimPrefix(message, "opencode server did not become ready: ")
	if len(tail) != 500 || tail != strings.Repeat("x", 500) {
		t.Fatalf("readiness stderr tail = %q (%d chars), want 500 trailing x characters", tail, len(tail))
	}
}

func TestOpencodeReadinessTimeoutIs60Seconds(t *testing.T) {
	if opencodeReadyTimeout != 60*time.Second {
		t.Fatalf("OpenCode readiness timeout = %s, want 60s", opencodeReadyTimeout)
	}
}

func containsEnvValue(env []string, key, want string) bool {
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == key && value == want {
			return true
		}
	}
	return false
}

func TestCodexRecordedAppServerNotificationsBecomeHarnessEvents(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []HarnessEvent
	}{
		{
			name: "assistant delta",
			line: `{"method":"item/agentMessage/delta","params":{"delta":"hello"}}`,
			want: []HarnessEvent{{Type: "message.delta", Data: map[string]any{"text": "hello"}}},
		},
		{
			name: "command item",
			line: `{"method":"item/started","params":{"item":{"id":"item-1","type":"commandExecution","command":"echo hi"}}}`,
			want: []HarnessEvent{{Type: "tool.call", Data: map[string]any{"id": "item-1", "name": "Command", "input": map[string]any{"command": "echo hi"}}}},
		},
		{
			name: "turn completed",
			line: `{"method":"turn/completed","params":{"turn":{"status":"completed"}}}`,
			want: []HarnessEvent{{Type: "turn.completed"}},
		},
		{
			name: "turn failed",
			line: `{"method":"turn/completed","params":{"turn":{"status":{"failed":{"reason":"error"}}}}}`,
			want: []HarnessEvent{{Type: "turn.failed", Data: map[string]any{"error": "error"}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var message map[string]any
			if err := json.Unmarshal([]byte(test.line), &message); err != nil {
				t.Fatal(err)
			}
			got := mapCodexNotification(message["method"].(string), message["params"].(map[string]any))
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("events = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestOpencodeRecordedEventLinesBecomeHarnessEvents(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []HarnessEvent
	}{
		{
			name: "assistant text",
			line: `{"type":"text","sessionID":"ses-1","part":{"text":"hello"}}`,
			want: []HarnessEvent{{Type: "message.delta", SessionID: "ses-1", Data: map[string]any{"text": "hello"}}},
		},
		{
			name: "completed tool",
			line: `{"type":"tool_use","sessionID":"ses-1","part":{"callID":"call-1","tool":"bash","state":{"status":"completed","title":"echo hi","input":{"command":"echo hi"},"output":"hi"}}}`,
			want: []HarnessEvent{{Type: "tool.call", SessionID: "ses-1", Data: map[string]any{"id": "call-1", "name": "echo hi", "input": map[string]any{"command": "echo hi"}}}, {Type: "tool.result", SessionID: "ses-1", Data: map[string]any{"id": "call-1", "output": "hi", "isError": false}}},
		},
		{
			name: "SSE tool update",
			line: `{"payload":{"type":"message.part.updated","properties":{"part":{"type":"tool","sessionID":"ses-1","callID":"call-2","tool":"bash","state":{"status":"completed","title":"pwd","input":{"command":"pwd"},"output":"/config"}}}}}`,
			want: []HarnessEvent{{Type: "tool.call", SessionID: "ses-1", Data: map[string]any{"id": "call-2", "name": "pwd", "input": map[string]any{"command": "pwd"}}}, {Type: "tool.result", SessionID: "ses-1", Data: map[string]any{"id": "call-2", "output": "/config", "isError": false}}},
		},
		{
			name: "session idle",
			line: `{"type":"session.idle","properties":{"sessionID":"ses-1"}}`,
			want: []HarnessEvent{{Type: "turn.completed", SessionID: "ses-1"}},
		},
		{
			name: "session error",
			line: `{"type":"session.error","properties":{"sessionID":"ses-1","error":{"message":"failed"}}}`,
			want: []HarnessEvent{{Type: "turn.failed", SessionID: "ses-1", Data: map[string]any{"error": "failed"}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := mapOpencodeEvent([]byte(test.line))
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("events = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestOpencodeReplayNormalizesAssistantPartsAndToolLifecycleOnce(t *testing.T) {
	lines := []string{
		`{"type":"message.updated","properties":{"info":{"id":"msg-user","sessionID":"ses-1","role":"user"}}}`,
		`{"type":"message.part.updated","properties":{"part":{"id":"part-user","sessionID":"ses-1","messageID":"msg-user","type":"text","text":"boxes-ok 1920"}}}`,
		`{"type":"message.updated","properties":{"info":{"id":"msg-assistant","sessionID":"ses-1","role":"assistant"}}}`,
		`{"type":"message.part.updated","properties":{"part":{"id":"part-text","sessionID":"ses-1","messageID":"msg-assistant","type":"text","text":"boxes-"}}}`,
		`{"type":"message.part.updated","properties":{"part":{"id":"part-text","sessionID":"ses-1","messageID":"msg-assistant","type":"text","text":"boxes-ok"}}}`,
		`{"type":"message.part.updated","properties":{"part":{"id":"part-text","sessionID":"ses-1","messageID":"msg-assistant","type":"text","text":"boxes-ok"}}}`,
		`{"type":"message.part.updated","properties":{"part":{"id":"part-tool","sessionID":"ses-1","messageID":"msg-assistant","type":"tool","callID":"call-1","tool":"bash","state":{"status":"running","title":"echo hi","input":{"command":"echo hi"}}}}}`,
		`{"type":"message.part.updated","properties":{"part":{"id":"part-tool","sessionID":"ses-1","messageID":"msg-assistant","type":"tool","callID":"call-1","tool":"bash","state":{"status":"running","title":"echo hi","input":{"command":"echo hi"}}}}}`,
		`{"type":"message.part.updated","properties":{"part":{"id":"part-tool","sessionID":"ses-1","messageID":"msg-assistant","type":"tool","callID":"call-1","tool":"bash","state":{"status":"completed","title":"echo hi","input":{"command":"echo hi"},"output":"ok"}}}}`,
		`{"type":"message.part.updated","properties":{"part":{"id":"part-tool","sessionID":"ses-1","messageID":"msg-assistant","type":"tool","callID":"call-1","tool":"bash","state":{"status":"completed","title":"echo hi","input":{"command":"echo hi"},"output":"ok"}}}}`,
	}
	mapper := newOpencodeEventMapper()
	var got []HarnessEvent
	for _, line := range lines {
		got = append(got, mapper.mapEvent([]byte(line))...)
	}
	want := []HarnessEvent{
		{Type: "message.part", SessionID: "ses-1", Data: map[string]any{"id": "part-text", "text": "boxes-"}},
		{Type: "message.part", SessionID: "ses-1", Data: map[string]any{"id": "part-text", "text": "boxes-ok"}},
		{Type: "tool.call", SessionID: "ses-1", Data: map[string]any{"id": "call-1", "name": "echo hi", "input": map[string]any{"command": "echo hi"}}},
		{Type: "tool.result", SessionID: "ses-1", Data: map[string]any{"id": "call-1", "output": "ok", "isError": false}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized replay events = %#v, want %#v", got, want)
	}
}

func TestOpencodeSessionUsesHTTPAPIAndConsumesSSE(t *testing.T) {
	promptReceived := make(chan map[string]any, 1)
	abortReceived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "ses-test"})
		case r.Method == http.MethodGet && r.URL.Path == "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"payload\":{\"type\":\"text\",\"sessionID\":\"ses-test\",\"part\":{\"text\":\"hello\"}}}\n\n"))
			_, _ = w.Write([]byte("data: {\"payload\":{\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"ses-test\"}}}\n\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.Method == http.MethodPost && r.URL.Path == "/session/ses-test/message":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			promptReceived <- body
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/session/ses-test/abort":
			abortReceived <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	harness := NewOpencodeHarness(t.TempDir())
	harness.baseURL = server.URL
	harness.client = server.Client()
	session, err := harness.Start(context.Background(), ConvSpec{ConversationID: "cv_test", Provider: "opencode", Model: "provider/model"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if session.SessionID() != "ses-test" {
		t.Fatalf("session id = %q, want ses-test", session.SessionID())
	}
	if err := session.Send("say hello"); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-promptReceived:
		if body["model"].(map[string]any)["providerID"] != "provider" {
			t.Fatalf("message model = %#v", body["model"])
		}
		parts := body["parts"].([]any)
		if parts[0].(map[string]any)["text"] != "say hello" {
			t.Fatalf("message parts = %#v", parts)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenCode message request was not sent")
	}
	seenText, seenDone := false, false
	deadline := time.After(2 * time.Second)
	for !seenDone {
		select {
		case event, open := <-session.Events():
			if !open {
				t.Fatal("OpenCode event stream closed before turn completion")
			}
			switch event.Type {
			case "message.delta":
				seenText = event.Data["text"] == "hello"
			case "turn.completed":
				seenDone = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for recorded OpenCode events")
		}
	}
	if !seenText {
		t.Fatal("assistant text event was not emitted")
	}
	if err := session.Interrupt(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-abortReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("OpenCode abort request was not sent")
	}
}

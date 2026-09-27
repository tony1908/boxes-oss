package boxd

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestParseClaudeStreamJSONMapsMessagesAndTerminalResults(t *testing.T) {
	fixtures := []struct {
		line string
		want []HarnessEvent
	}{
		{`{"type":"system","subtype":"init","session_id":"sid-123"}`, []HarnessEvent{{Type: "lifecycle", SessionID: "sid-123", Data: map[string]any{"subtype": "init"}}}},
		{`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"tool-1","name":"screenshot","input":{"detail":"full"}}]}}`, []HarnessEvent{{Type: "message.delta", Data: map[string]any{"text": "hello"}}, {Type: "tool.call", Data: map[string]any{"id": "tool-1", "name": "screenshot", "input": map[string]any{"detail": "full"}}}}},
		{`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"image captured","is_error":false}]}}`, []HarnessEvent{{Type: "tool.result", Data: map[string]any{"toolUseID": "tool-1", "content": "image captured", "isError": false}}}},
		{`{"type":"result","subtype":"success","is_error":false,"result":"done"}`, []HarnessEvent{{Type: "turn.completed", Data: map[string]any{"result": "done"}}}},
		{`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"limit"}`, []HarnessEvent{{Type: "turn.failed", Data: map[string]any{"error": "limit", "subtype": "error_max_turns"}}}},
	}
	for _, fixture := range fixtures {
		got, err := parseClaudeLine([]byte(fixture.line))
		if err != nil {
			t.Fatalf("parse %s: %v", fixture.line, err)
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(fixture.want)
		if !reflect.DeepEqual(gotJSON, wantJSON) {
			t.Errorf("events for %s = %s, want %s", fixture.line, gotJSON, wantJSON)
		}
	}
}

func TestClaudeArgumentsResumePersistedSession(t *testing.T) {
	args := claudeArgs(ConvSpec{Model: "claude-sonnet", SessionID: "sid-456"}, "/box/config/.boxd/mcp.json")
	want := []string{"claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--mcp-config", "/box/config/.boxd/mcp.json", "--model", "claude-sonnet", "--resume", "sid-456"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("claude argv = %#v, want %#v", args, want)
	}
}

func TestOSClaudeProcessCapturesStderrTail(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is unavailable")
	}
	process, err := (osClaudeExecutor{}).Start(context.Background(), []string{shell, "-c", `printf '%s' "$1" >&2`, "sh", strings.Repeat("x", 501)}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, process.Stdout()); err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	if got, want := process.StderrTail(), strings.Repeat("x", 500); got != want {
		t.Fatalf("stderr tail = %q, want last 500 characters %q", got, want)
	}
}

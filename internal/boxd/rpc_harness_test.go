package boxd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNewHarnessArgs(t *testing.T) {
	wantPi := []string{"pi", "--mode", "rpc", "--session-dir", "/config/.boxd/pi-sessions", "--extension", "npm:pi-mcp-extension", "--model", "anthropic/model", "--session", "sid"}
	if got := piArgs("pi", "/config", ConvSpec{Model: "anthropic/model", SessionID: "sid"}); !reflect.DeepEqual(got, wantPi) {
		t.Fatalf("pi args = %#v, want %#v", got, wantPi)
	}
	wantPrime := []string{"prime-agent", "--mode", "rpc", "--session-dir", "/config/.boxd/prime-agent-sessions", "--extension", "npm:pi-mcp-extension"}
	if got := piArgs("prime-agent", "/config", ConvSpec{}); !reflect.DeepEqual(got, wantPrime) {
		t.Fatalf("prime args = %#v, want %#v", got, wantPrime)
	}
	wantKimi := []string{"kimi", "--model", "kimi-code/kimi-for-coding", "acp"}
	if got := kimiArgs(ConvSpec{Model: "kimi-code/kimi-for-coding", SessionID: "sid"}); !reflect.DeepEqual(got, wantKimi) {
		t.Fatalf("kimi args = %#v, want %#v", got, wantKimi)
	}
}

func TestRecordedNewHarnessEvents(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return []byte(strings.TrimSpace(string(b)))
	}
	pi, id, err := mapPiEvent(read("pi_rpc_text_delta.jsonl"), "pi")
	if err != nil || id != "pi-session-1" || !reflect.DeepEqual(pi, []HarnessEvent{{Type: "message.delta", SessionID: id, Data: map[string]any{"text": "hello"}}}) {
		t.Fatalf("pi mapping = %#v, %q, %v", pi, id, err)
	}
	prime, id, err := mapPiEvent(read("prime_rpc_tool_call.jsonl"), "prime")
	wantPrime := []HarnessEvent{{Type: "tool.call", SessionID: "prime-session-1", Data: map[string]any{"id": "call-1", "name": "bash", "input": map[string]any{"command": "pwd"}}}}
	if err != nil || id != "prime-session-1" || !reflect.DeepEqual(prime, wantPrime) {
		t.Fatalf("prime mapping = %#v, %q, %v", prime, id, err)
	}
	var kimi []HarnessEvent
	for _, line := range strings.Split(string(read("kimi_acp_updates.jsonl")), "\n") {
		var mapped []HarnessEvent
		mapped, err = mapKimiACPEvent([]byte(line))
		if err != nil {
			break
		}
		kimi = append(kimi, mapped...)
	}
	wantKimi := []HarnessEvent{{Type: "message.delta", Data: map[string]any{"text": "hello"}}, {Type: "tool.call", Data: map[string]any{"id": "call-2", "name": "Shell", "input": map[string]any{"command": "pwd"}}}, {Type: "tool.result", Data: map[string]any{"id": "call-2", "output": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "/config"}}}, "isError": false}}}
	if err != nil || !reflect.DeepEqual(kimi, wantKimi) {
		t.Fatalf("kimi mapping = %#v, %v", kimi, err)
	}
}

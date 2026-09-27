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
	wantKimi := []string{"kimi", "--print", "--input-format=stream-json", "--output-format=stream-json", "--mcp-config-file", "/config/.boxd/mcp.json", "--model", "kimi-k2.5", "--session", "sid"}
	if got := kimiArgs(ConvSpec{Model: "kimi-k2.5", SessionID: "sid"}, "/config"); !reflect.DeepEqual(got, wantKimi) {
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
	kimi, err := mapKimiEvent(read("kimi_stream_json.jsonl"))
	wantKimi := []HarnessEvent{{Type: "message.delta", Data: map[string]any{"text": "hello"}}, {Type: "tool.call", Data: map[string]any{"id": "call-2", "name": "Shell", "input": "{\"command\":\"pwd\"}"}}}
	if err != nil || !reflect.DeepEqual(kimi, wantKimi) {
		t.Fatalf("kimi mapping = %#v, %v", kimi, err)
	}
}

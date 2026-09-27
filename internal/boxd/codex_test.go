package boxd

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"
)

type codexTestExecutor struct {
	argv    []string
	dir     string
	process *codexTestProcess
	methods chan string
}

func (e *codexTestExecutor) Start(_ context.Context, argv []string, dir string, _ []string) (ClaudeProcess, error) {
	e.argv = append([]string(nil), argv...)
	e.dir = dir
	e.methods = make(chan string, 16)
	e.process = newCodexTestProcess(e.methods)
	return e.process, nil
}

type codexTestProcess struct {
	stdinReader  *io.PipeReader
	stdinWriter  *io.PipeWriter
	stdoutReader *io.PipeReader
	stdoutWriter *io.PipeWriter
	done         chan struct{}
	kill         sync.Once
	write        sync.Mutex
}

func newCodexTestProcess(methods chan string) *codexTestProcess {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	process := &codexTestProcess{stdinReader: stdinReader, stdinWriter: stdinWriter, stdoutReader: stdoutReader, stdoutWriter: stdoutWriter, done: make(chan struct{})}
	go func() {
		defer close(process.done)
		defer process.stdoutWriter.Close()
		scanner := bufio.NewScanner(stdinReader)
		for scanner.Scan() {
			var request map[string]any
			if json.Unmarshal(scanner.Bytes(), &request) != nil {
				continue
			}
			method, _ := request["method"].(string)
			id, hasID := request["id"]
			if !hasID {
				continue
			}
			methods <- method
			result := map[string]any{}
			switch method {
			case "thread/start":
				result = map[string]any{"thread": map[string]string{"id": "thread-1"}}
			case "turn/start":
				result = map[string]any{"turn": map[string]string{"id": "turn-1"}}
			case "turn/steer", "turn/interrupt":
			default:
			}
			process.write.Lock()
			_ = json.NewEncoder(stdoutWriter).Encode(map[string]any{"id": id, "result": result})
			process.write.Unlock()
		}
	}()
	return process
}

func (p *codexTestProcess) Stdin() io.WriteCloser { return p.stdinWriter }
func (p *codexTestProcess) Stdout() io.ReadCloser { return p.stdoutReader }
func (p *codexTestProcess) StderrTail() string    { return "" }
func (p *codexTestProcess) Wait() error           { <-p.done; return nil }
func (p *codexTestProcess) Interrupt() error      { return nil }
func (p *codexTestProcess) Kill() error {
	p.kill.Do(func() {
		_ = p.stdinReader.Close()
		_ = p.stdinWriter.Close()
		_ = p.stdoutWriter.Close()
	})
	return nil
}

func TestCodexHarnessInitializesThreadAndUsesTurnControls(t *testing.T) {
	executor := &codexTestExecutor{}
	harness := NewCodexHarness(t.TempDir())
	harness.executor = executor
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := harness.Start(ctx, ConvSpec{ConversationID: "cv-1", Provider: "codex", Model: "model-x"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if !reflect.DeepEqual(executor.argv, []string{"codex", "app-server"}) {
		t.Fatalf("Codex argv = %#v", executor.argv)
	}
	if executor.dir != harness.workDir {
		t.Fatalf("Codex working directory = %q, want %q", executor.dir, harness.workDir)
	}
	if session.SessionID() != "thread-1" {
		t.Fatalf("session id = %q, want thread-1", session.SessionID())
	}
	select {
	case event := <-session.Events():
		if event.Type != "lifecycle" || event.SessionID != "thread-1" {
			t.Fatalf("initial event = %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("Codex lifecycle event was not emitted")
	}
	if err := session.Send("inspect this box"); err != nil {
		t.Fatal(err)
	}
	if err := session.Steer("focus on the config file"); err != nil {
		t.Fatal(err)
	}
	if err := session.Interrupt(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for range 5 {
		select {
		case method := <-executor.methods:
			got = append(got, method)
		case <-ctx.Done():
			t.Fatalf("Codex RPC methods = %#v, timed out", got)
		}
	}
	want := []string{"initialize", "thread/start", "turn/start", "turn/steer", "turn/interrupt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Codex RPC methods = %#v, want %#v", got, want)
	}
}

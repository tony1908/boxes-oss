package boxd

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tony1908/paddock/internal/computer"
)

type computerCommand struct {
	argv []string
}

type computerExecutor struct {
	calls []computerCommand
	png   []byte
	wm    []byte
}

func (e *computerExecutor) Run(_ context.Context, argv []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
	e.calls = append(e.calls, computerCommand{argv: append([]string(nil), argv...)})
	if stdout != nil {
		if argv[len(argv)-1] == "-" {
			_, _ = stdout.Write(e.png)
		} else {
			_, _ = stdout.Write(e.wm)
		}
	}
	return 0, nil
}

func TestComputerMCPServerExposesDesktopTools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	executor := &computerExecutor{png: []byte("png data"), wm: []byte("0x01 0 host Terminal\n")}
	server := NewMCPServer(computer.New(executor))
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "computer-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = session.Close()
		if err := <-serverDone; err != nil {
			t.Logf("mcp server stopped: %v", err)
		}
	}()

	tools, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	wantNames := []string{"screenshot", "click", "move", "drag", "scroll", "type", "key", "open_url", "launch_app", "list_windows"}
	sort.Strings(names)
	sort.Strings(wantNames)
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("tools = %v, want %v", names, wantNames)
	}

	screenshot, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "screenshot", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(screenshot.Content) != 1 {
		t.Fatalf("screenshot content count = %d, want 1", len(screenshot.Content))
	}
	image, ok := screenshot.Content[0].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("screenshot content = %T, want *mcp.ImageContent", screenshot.Content[0])
	}
	if image.MIMEType != "image/png" || !bytes.Equal(image.Data, executor.png) {
		t.Fatalf("screenshot image = (%q, %q), want image/png and PNG bytes", image.MIMEType, image.Data)
	}

	_, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "click",
		Arguments: map[string]any{"x": 12, "y": 34, "button": "right", "double": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantClick := []string{"env", "DISPLAY=:1", "xdotool", "mousemove", "12", "34", "click", "--repeat", "2", "--delay", "100", "3"}
	if len(executor.calls) != 2 || !reflect.DeepEqual(executor.calls[1].argv, wantClick) {
		t.Fatalf("click command = %#v, want %#v", executor.calls, wantClick)
	}

	windows, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_windows", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(windows.Content) != 1 {
		t.Fatalf("list_windows content count = %d, want 1", len(windows.Content))
	}
	text, ok := windows.Content[0].(*mcp.TextContent)
	if !ok || text.Text != `[{"id":"0x01","desktop":0,"host":"host","title":"Terminal"}]` {
		t.Fatalf("list_windows content = %#v", windows.Content)
	}

	calls := []struct {
		name string
		args map[string]any
		want []string
	}{
		{"move", map[string]any{"x": 5, "y": 6}, []string{"env", "DISPLAY=:1", "xdotool", "mousemove", "5", "6"}},
		{"drag", map[string]any{"from_x": 1, "from_y": 2, "to_x": 3, "to_y": 4}, []string{"env", "DISPLAY=:1", "xdotool", "mousemove", "1", "2", "mousedown", "1", "mousemove", "--sync", "3", "4", "mouseup", "1"}},
		{"scroll", map[string]any{"direction": "up", "amount": 2}, []string{"env", "DISPLAY=:1", "xdotool", "click", "--repeat", "2", "4"}},
		{"type", map[string]any{"text": "hello; not a shell"}, []string{"env", "DISPLAY=:1", "xdotool", "type", "--clearmodifiers", "--delay", "0", "--", "hello; not a shell"}},
		{"key", map[string]any{"combo": "ctrl+shift+t"}, []string{"env", "DISPLAY=:1", "xdotool", "key", "--clearmodifiers", "ctrl+shift+t"}},
		{"open_url", map[string]any{"url": "https://example.test/?a=1&b=2"}, []string{"env", "DISPLAY=:1", "xdg-open", "https://example.test/?a=1&b=2"}},
		{"launch_app", map[string]any{"name": "firefox"}, []string{"env", "DISPLAY=:1", "firefox"}},
	}
	for i, call := range calls {
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args}); err != nil {
			t.Fatalf("CallTool(%q): %v", call.name, err)
		}
		got := executor.calls[i+3].argv
		if !reflect.DeepEqual(got, call.want) {
			t.Errorf("%s command = %#v, want %#v", call.name, got, call.want)
		}
	}
}

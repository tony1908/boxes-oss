package computer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
)

type commandCall struct {
	argv   []string
	stdin  string
	stdout io.Writer
	stderr io.Writer
}

type fakeExecutor struct {
	calls []commandCall
	code  int
	err   error
	out   []byte
}

func (f *fakeExecutor) Run(_ context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	call := commandCall{argv: append([]string(nil), argv...), stdout: stdout, stderr: stderr}
	if stdin != nil {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return 0, err
		}
		call.stdin = string(data)
	}
	f.calls = append(f.calls, call)
	if stdout != nil && len(f.out) > 0 {
		_, _ = stdout.Write(f.out)
	}
	return f.code, f.err
}

func TestScreenshotReturnsPNGFromScrot(t *testing.T) {
	wantPNG := []byte("png bytes")
	exec := &fakeExecutor{out: wantPNG}
	computer := New(exec)

	got, err := computer.Screenshot()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, wantPNG) {
		t.Fatalf("Screenshot() = %q, want %q", got, wantPNG)
	}
	assertCommand(t, exec, []string{"env", "DISPLAY=:1", "scrot", "-"})
}

func TestMouseActionsUseDisplayAndExpectedArguments(t *testing.T) {
	exec := &fakeExecutor{}
	computer := New(exec)

	if err := computer.Click(12, 34, "right", true); err != nil {
		t.Fatal(err)
	}
	if err := computer.Move(56, 78); err != nil {
		t.Fatal(err)
	}
	if err := computer.Drag(1, 2, 3, 4); err != nil {
		t.Fatal(err)
	}
	if err := computer.Scroll("down", 3); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"env", "DISPLAY=:1", "xdotool", "mousemove", "12", "34", "click", "--repeat", "2", "--delay", "100", "3"},
		{"env", "DISPLAY=:1", "xdotool", "mousemove", "56", "78"},
		{"env", "DISPLAY=:1", "xdotool", "mousemove", "1", "2", "mousedown", "1", "mousemove", "--sync", "3", "4", "mouseup", "1"},
		{"env", "DISPLAY=:1", "xdotool", "click", "--repeat", "3", "5"},
	}
	if len(exec.calls) != len(want) {
		t.Fatalf("got %d commands, want %d", len(exec.calls), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(exec.calls[i].argv, want[i]) {
			t.Errorf("command %d = %#v, want %#v", i, exec.calls[i].argv, want[i])
		}
	}
}

func TestTextAndDesktopCommandsPassUserInputAsSingleArgument(t *testing.T) {
	exec := &fakeExecutor{}
	computer := New(exec)
	text := `hello; $(touch /tmp/not-run)`
	url := "https://example.test/search?q=a%20b&next=home"

	if err := computer.Type(text); err != nil {
		t.Fatal(err)
	}
	if err := computer.Key("ctrl+shift+t"); err != nil {
		t.Fatal(err)
	}
	if err := computer.OpenURL(url); err != nil {
		t.Fatal(err)
	}
	if err := computer.LaunchApp("firefox"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"env", "DISPLAY=:1", "xdotool", "type", "--clearmodifiers", "--delay", "0", "--", text},
		{"env", "DISPLAY=:1", "xdotool", "key", "--clearmodifiers", "ctrl+shift+t"},
		{"env", "DISPLAY=:1", "xdg-open", url},
		{"env", "DISPLAY=:1", "firefox"},
	}
	for i := range want {
		if !reflect.DeepEqual(exec.calls[i].argv, want[i]) {
			t.Errorf("command %d = %#v, want %#v", i, exec.calls[i].argv, want[i])
		}
	}
}

func TestListWindowsParsesWmctrlOutput(t *testing.T) {
	exec := &fakeExecutor{out: []byte("0x03e00007  0 host Firefox — Example\n0x04a00002 -1 host Terminal\n")}
	got, err := New(exec).ListWindows()
	if err != nil {
		t.Fatal(err)
	}
	want := []Window{
		{ID: "0x03e00007", Desktop: 0, Host: "host", Title: "Firefox — Example"},
		{ID: "0x04a00002", Desktop: -1, Host: "host", Title: "Terminal"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListWindows() = %#v, want %#v", got, want)
	}
	assertCommand(t, exec, []string{"env", "DISPLAY=:1", "wmctrl", "-l"})
}

func TestCommandErrorsAreReturned(t *testing.T) {
	wantErr := errors.New("execution failed")
	computer := New(&fakeExecutor{code: 1, err: wantErr})
	if err := computer.Move(1, 2); !errors.Is(err, wantErr) {
		t.Fatalf("Move() error = %v, want %v", err, wantErr)
	}
}

func assertCommand(t *testing.T, exec *fakeExecutor, want []string) {
	t.Helper()
	if len(exec.calls) != 1 || !reflect.DeepEqual(exec.calls[0].argv, want) {
		t.Fatalf("commands = %#v, want one command %#v", exec.calls, want)
	}
}

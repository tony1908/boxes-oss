package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tony1908/boxes-oss/internal/dockercli"
	"github.com/tony1908/boxes-oss/internal/snapshot"
)

func TestDesktopEnvFromEnviron(t *testing.T) {
	environ := []byte("HOME=/config\x00DISPLAY=:1\x00DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/dbus\x00XDG_RUNTIME_DIR=/config/.XDG\x00")
	got, ok := desktopEnvFromEnviron(environ)
	want := map[string]string{"DISPLAY": ":1", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/tmp/dbus", "XDG_RUNTIME_DIR": "/config/.XDG"}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("desktopEnvFromEnviron() = %#v, %v; want %#v", got, ok, want)
	}
	if _, ok := desktopEnvFromEnviron([]byte("DISPLAY=:0\x00HOME=/config\x00")); ok {
		t.Fatal("DISPLAY=:0 must not count as the desktop session")
	}
}

func TestDesktopExecUsesSessionEnvAndRejectsMissingSession(t *testing.T) {
	var dockerCalls []callRecord
	dockerExec := testExecutor{calls: &dockerCalls, command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 1 && argv[1] == "inspect" {
			return 1, nil
		}
		if containsArg(argv, desktopEnvScript) {
			if stdout != nil {
				_, _ = io.WriteString(stdout, "DISPLAY=:1\x00DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/dbus\x00XDG_RUNTIME_DIR=/config/.XDG\x00")
			}
		}
		return 0, nil
	}}
	agent := New(Config{NodeID: "nd_test"}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	response := httptest.NewRecorder()
	agent.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/boxes/bx_abcdefgh2345/exec", strings.NewReader(`{"cmd":["xeyes"],"desktop":true,"env":{"FOO":"1"}}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d/%s", response.Code, response.Body.String())
	}
	got := findDockerCall(dockerCalls, "xeyes")
	for _, want := range []string{"DISPLAY=:1", "DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/dbus", "XDG_RUNTIME_DIR=/config/.XDG", "FOO=1", "USER=abc"} {
		if !containsArg(got, want) {
			t.Fatalf("docker exec argv %#v missing --env %s", got, want)
		}
	}

	var noSession []callRecord
	missing := testExecutor{calls: &noSession, command: func(argv []string, _ []byte, _, _ io.Writer) (int, error) {
		if len(argv) > 1 && argv[1] == "inspect" {
			return 1, nil
		}
		if containsArg(argv, desktopEnvScript) {
			return 1, nil
		}
		return 0, nil
	}}
	agent = New(Config{NodeID: "nd_test"}, dockercli.New(missing), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	response = httptest.NewRecorder()
	agent.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/boxes/bx_abcdefgh2345/exec", strings.NewReader(`{"cmd":["xeyes"],"desktop":true}`)))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "desktop_not_running") {
		t.Fatalf("status/body = %d/%s, want 409 desktop_not_running", response.Code, response.Body.String())
	}
	if findDockerCall(noSession, "xeyes") != nil {
		t.Fatal("command ran without a desktop session")
	}
}

func TestDesktopExecRejectsOtherUser(t *testing.T) {
	var dockerCalls []callRecord
	agent := New(Config{NodeID: "nd_test"}, dockercli.New(boxUserExecutor(&dockerCalls)), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	response := httptest.NewRecorder()
	agent.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/boxes/bx_abcdefgh2345/exec", strings.NewReader(`{"cmd":["id"],"desktop":true,"user":"alice"}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d/%s, want 400", response.Code, response.Body.String())
	}
}

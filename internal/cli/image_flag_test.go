package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestNewImageFlagSendsCustomImageAndOmitsItByDefault(t *testing.T) {
	server, seen := recordCLIRequests(t)
	run := func(args ...string) {
		t.Helper()
		var out, stderr bytes.Buffer
		app := CLI{Out: &out, Err: &stderr, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token"}
		if err := app.Run(context.Background(), append([]string{"--json"}, args...)); err != nil {
			t.Fatalf("Run %v: %v (stderr: %s)", args, err, stderr.String())
		}
	}
	run("new", "--image", "ghcr.io/example/custom-box:1")
	if body := seen["POST /api/v1/sandboxes"]; body["image"] != "ghcr.io/example/custom-box:1" {
		t.Fatalf("new body = %#v, want image", body)
	}
	run("new")
	if _, sent := seen["POST /api/v1/sandboxes"]["image"]; sent {
		t.Fatal("new without --image must not send image")
	}
}

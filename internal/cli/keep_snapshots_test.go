package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// recordCLIRequests serves one fixed sandbox response and records every request body by method and path.
func recordCLIRequests(t *testing.T) (*httptest.Server, map[string]map[string]any) {
	t.Helper()
	seen := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		seen[r.Method+" "+r.URL.Path] = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sandbox":{"id":"bx_keepcli123456","state":"ready"}}`)
	}))
	t.Cleanup(server.Close)
	return server, seen
}

func TestKeepSnapshotsFlagSendsRetentionOnNewAndUpdate(t *testing.T) {
	server, seen := recordCLIRequests(t)
	run := func(args ...string) {
		t.Helper()
		var out, stderr bytes.Buffer
		app := CLI{Out: &out, Err: &stderr, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token"}
		if err := app.Run(context.Background(), append([]string{"--json"}, args...)); err != nil {
			t.Fatalf("Run %v: %v (stderr: %s)", args, err, stderr.String())
		}
	}
	run("new", "--keep-snapshots", "3")
	if body := seen["POST /api/v1/sandboxes"]; body["keepSnapshots"] != float64(3) {
		t.Fatalf("new body = %#v, want keepSnapshots 3", body)
	}
	run("new")
	if _, sent := seen["POST /api/v1/sandboxes"]["keepSnapshots"]; sent {
		t.Fatal("new without --keep-snapshots must not send keepSnapshots")
	}
	run("update", "bx_keepcli123456", "--keep-snapshots", "12")
	if body := seen["PATCH /api/v1/sandboxes/bx_keepcli123456"]; len(body) != 1 || body["keepSnapshots"] != float64(12) {
		t.Fatalf("update body = %#v, want only keepSnapshots 12", body)
	}
}

func TestRestoreSendsSnapshotToRollbackEndpoint(t *testing.T) {
	server, seen := recordCLIRequests(t)
	var out, stderr bytes.Buffer
	app := CLI{Out: &out, Err: &stderr, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token"}
	if err := app.Run(context.Background(), []string{"restore", "bx_keepcli123456", "sn_abc"}); err != nil {
		t.Fatalf("Run restore: %v (stderr: %s)", err, stderr.String())
	}
	if body, ok := seen["POST /api/v1/sandboxes/bx_keepcli123456/restore"]; !ok || body["snapshotId"] != "sn_abc" {
		t.Fatalf("restore request = %#v (seen %#v)", body, seen)
	}
	if err := app.Run(context.Background(), []string{"restore", "bx_keepcli123456"}); err == nil {
		t.Fatal("restore without a snapshot ID must fail")
	}
}

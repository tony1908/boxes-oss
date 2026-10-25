package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
)

// recordRequests runs args through the CLI against a fake API and returns each request's method, path and JSON body.
func recordRequests(t *testing.T, args ...string) []map[string]any {
	t.Helper()
	var seen []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := map[string]any{"method": r.Method, "path": r.URL.Path}
		var body map[string]any
		if data, _ := io.ReadAll(r.Body); len(data) > 0 {
			_ = json.Unmarshal(data, &body)
		}
		entry["body"] = body
		seen = append(seen, entry)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	var out, stderr bytes.Buffer
	app := CLI{Out: &out, Err: &stderr, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token"}
	if err := app.Run(context.Background(), args); err != nil {
		t.Fatalf("Run %v: %v", args, err)
	}
	return seen
}

func TestUserAndGroupCommandsHitTheRightEndpoints(t *testing.T) {
	const box = "bx_abcdefghijkl"
	const base = "/api/v1/sandboxes/" + box
	cases := []struct {
		args   []string
		method string
		path   string
		body   map[string]any
	}{
		{[]string{"user", "add", box, "alice"}, http.MethodPost, base + "/users", map[string]any{"username": "alice"}},
		{[]string{"user", "rm", box, "alice"}, http.MethodDelete, base + "/users/alice", nil},
		{[]string{"group", "add", box, "team"}, http.MethodPost, base + "/groups", map[string]any{"name": "team"}},
		{[]string{"group", "member", "add", box, "team", "alice"}, http.MethodPut, base + "/groups/team/members/alice", nil},
		{[]string{"group", "member", "rm", box, "team", "alice"}, http.MethodDelete, base + "/groups/team/members/alice", nil},
		{[]string{"exec", box, "--user", "alice", "--", "id"}, http.MethodPost, base + "/commands", map[string]any{"cmd": "id", "user": "alice"}},
	}
	for _, test := range cases {
		seen := recordRequests(t, test.args...)
		if len(seen) != 1 || seen[0]["method"] != test.method || seen[0]["path"] != test.path {
			t.Fatalf("%v: requests = %#v, want %s %s", test.args, seen, test.method, test.path)
		}
		if test.body != nil && !reflect.DeepEqual(seen[0]["body"], test.body) {
			t.Fatalf("%v: body = %#v, want %#v", test.args, seen[0]["body"], test.body)
		}
	}
}

func TestUserAndGroupCommandsRejectBadUsage(t *testing.T) {
	for _, args := range [][]string{
		{"user"},
		{"user", "add", "bx_abcdefghijkl"},
		{"user", "frobnicate", "bx_abcdefghijkl"},
		{"group", "member", "add", "bx_abcdefghijkl", "team"},
		{"group", "member", "move", "bx_abcdefghijkl", "team", "alice"},
	} {
		var out, stderr bytes.Buffer
		app := CLI{Out: &out, Err: &stderr, ConfigPath: filepath.Join(t.TempDir(), "config.json"), BaseURL: "http://127.0.0.1:1", Token: "api-token"}
		if err := app.Run(context.Background(), args); err == nil {
			t.Fatalf("Run %v accepted invalid usage", args)
		}
	}
}

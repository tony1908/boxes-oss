package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noRequestServer fails the test if any API request is made.
func noRequestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSubcommandHelpPrintsUsageWithoutAPICall(t *testing.T) {
	server := noRequestServer(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server":"`+server.URL+`","token":"t","current":"bx_abcdefghijkl"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"push", "--help"},
		{"exec", "bx_abcdefghijkl", "-h"},
		{"screenshot", "--help"},
		{"help", "exec"},
	} {
		var out bytes.Buffer
		app := CLI{Out: &out, Err: io.Discard, ConfigPath: configPath, BaseURL: server.URL, HTTPClient: server.Client()}
		if err := app.Run(context.Background(), args); err != nil {
			t.Fatalf("Run(%v) = %v", args, err)
		}
		if !strings.HasPrefix(out.String(), "usage: paddock ") {
			t.Fatalf("Run(%v) printed %q", args, out.String())
		}
	}
	data, _ := os.ReadFile(configPath)
	var config Config
	_ = json.Unmarshal(data, &config)
	if config.Current != "bx_abcdefghijkl" {
		t.Fatalf("current = %q after help calls", config.Current)
	}
}

func TestExecHelpAfterSeparatorIsRemoteArgument(t *testing.T) {
	var got map[string]any
	server := execServer(t, `{"exitCode":0,"stdout":"ok"}`, &got)
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "c.json"), BaseURL: server.URL, HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"exec", "bx_abcdefghijkl", "--", "tool", "--help"}); err != nil {
		t.Fatal(err)
	}
	if got["cmd"] == nil {
		t.Fatalf("command body = %#v", got)
	}
}

func TestDashPrefixedBoxArgumentIsNotAnID(t *testing.T) {
	server := noRequestServer(t)
	configPath := filepath.Join(t.TempDir(), "c.json")
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: configPath, BaseURL: server.URL, HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"info", "--bogus"}); err == nil || !strings.Contains(err.Error(), "invalid box ID") {
		t.Fatalf("err = %v, want invalid box ID", err)
	}
	if _, err := os.Stat(configPath); err == nil {
		t.Fatal("config written for rejected box argument")
	}
}

func TestCurrentIsSavedOnlyAfterSuccessAndOnlyForBoxIDs(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"bx_abcdefghijkl","state":"ready"}`)
	}))
	defer ok.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"code":"internal","message":"boom"}}`)
	}))
	defer failing.Close()
	configPath := filepath.Join(t.TempDir(), "c.json")
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: configPath, BaseURL: failing.URL, HTTPClient: failing.Client()}
	if err := app.Run(context.Background(), []string{"info", "bx_zzzzzzzzzzzz"}); err == nil {
		t.Fatal("expected server error")
	}
	if _, err := os.Stat(configPath); err == nil {
		t.Fatal("current saved although the request failed")
	}
	app = CLI{Out: io.Discard, Err: io.Discard, ConfigPath: configPath, BaseURL: ok.URL, HTTPClient: ok.Client()}
	if err := app.Run(context.Background(), []string{"info", "bx_abcdefghijkl"}); err != nil {
		t.Fatal(err)
	}
	if err := app.setCurrent("--help"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(configPath)
	var config Config
	_ = json.Unmarshal(data, &config)
	if config.Current != "bx_abcdefghijkl" {
		t.Fatalf("current = %q, want bx_abcdefghijkl", config.Current)
	}
}

func TestNotFoundNamesTheBox(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"resource not found"}}`)
	}))
	defer server.Close()
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "c.json"), BaseURL: server.URL, HTTPClient: server.Client()}
	err := app.Run(context.Background(), []string{"info", "bx_abcdefghijkl"})
	if err == nil || err.Error() != "box bx_abcdefghijkl not found" {
		t.Fatalf("err = %v, want box bx_abcdefghijkl not found", err)
	}
}

func TestExecDesktopFlagSendsDesktopTrue(t *testing.T) {
	var got map[string]any
	server := execServer(t, `{"exitCode":0,"stdout":""}`, &got)
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "c.json"), BaseURL: server.URL, HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"exec", "bx_abcdefghijkl", "--desktop", "--detach", "--", "./my-app"}); err != nil {
		t.Fatal(err)
	}
	if got["desktop"] != true || got["detached"] != true {
		t.Fatalf("body = %#v, want desktop and detached true", got)
	}
}

func TestScreenshotCapturesDownloadsAndCleansUp(t *testing.T) {
	png := []byte("\x89PNG-data")
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/sandboxes/bx_abcdefghijkl/artifacts":
			if r.URL.Query().Get("path") != "/tmp/paddock-screenshot.png" {
				t.Errorf("artifact path = %q", r.URL.Query().Get("path"))
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
		case "/api/v1/sandboxes/bx_abcdefghijkl/commands":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			cmd, _ := body["cmd"].([]any)
			if cmd[0] == "scrot" && (body["desktop"] != true || cmd[len(cmd)-1] != "/tmp/paddock-screenshot.png") {
				t.Errorf("scrot body = %#v", body)
			}
			_, _ = io.WriteString(w, `{"exitCode":0,"stdout":"","stderr":""}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	dest := filepath.Join(dir, "shot.png")
	var out bytes.Buffer
	app := CLI{Out: &out, Err: io.Discard, ConfigPath: filepath.Join(dir, "c.json"), BaseURL: server.URL, HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"screenshot", "bx_abcdefghijkl", "-o", dest}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != string(png) {
		t.Fatalf("saved file = %q, %v", data, err)
	}
	if strings.TrimSpace(out.String()) != dest {
		t.Fatalf("printed %q, want %q", out.String(), dest)
	}
	want := []string{
		"POST /api/v1/sandboxes/bx_abcdefghijkl/commands",
		"GET /api/v1/sandboxes/bx_abcdefghijkl/artifacts",
		"POST /api/v1/sandboxes/bx_abcdefghijkl/commands",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %q, want %q", calls, want)
	}
}

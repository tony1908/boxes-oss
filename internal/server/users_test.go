package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tony1908/boxes-oss/internal/tunnel"
	"github.com/tony1908/boxes-oss/internal/types"
)

func TestValidateAccountName(t *testing.T) {
	for _, name := range []string{"alice", "_svc", "dev-team_2", "a"} {
		if err := validateAccountName("user", name); err != nil {
			t.Fatalf("%q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"", "Alice", "1abc", "-x", "root", "abc", "has space", "a/b", "a.b", strings.Repeat("a", 33)} {
		if err := validateAccountName("user", name); err == nil {
			t.Fatalf("%q accepted", name)
		}
	}
}

func TestAccountCommandSequences(t *testing.T) {
	if got, want := createUserCommands("alice"), [][]string{
		{"useradd", "-m", "-s", "/bin/bash", "alice"},
		{"chmod", "770", "/home/alice"},
		{"usermod", "-aG", "alice", "abc"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("createUserCommands = %#v", got)
	}
	if got, want := deleteUserCommands("alice"), [][]string{{"userdel", "-r", "alice"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("deleteUserCommands = %#v", got)
	}
	if got, want := createGroupCommands("team"), [][]string{
		{"groupadd", "team"},
		{"mkdir", "-p", "/shared/team"},
		{"chgrp", "team", "/shared/team"},
		{"chmod", "2770", "/shared/team"},
		{"usermod", "-aG", "team", "abc"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("createGroupCommands = %#v", got)
	}
	if got, want := addMemberCommands("team", "alice"), [][]string{{"usermod", "-aG", "team", "alice"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("addMemberCommands = %#v", got)
	}
	if got, want := removeMemberCommands("team", "alice"), [][]string{{"gpasswd", "-d", "alice", "team"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("removeMemberCommands = %#v", got)
	}
}

func TestParseUsersKeepsHomeAccountsWithGroups(t *testing.T) {
	passwd := "root:x:0:0:root:/root:/bin/bash\nabc:x:1000:1000::/config:/bin/sh\nalice:x:1001:1001::/home/alice:/bin/bash\nbob:x:1002:1002::/home/bob:/bin/bash\n"
	group := "root:x:0:\nabc:x:1000:\nalice:x:1001:alice\nbob:x:1002:bob\nteam:x:1003:alice,bob\n"
	got := parseUsers(passwd, group)
	want := []boxUser{
		{Username: "alice", HomeDir: "/home/alice", Groups: []string{"alice", "team"}},
		{Username: "bob", HomeDir: "/home/bob", Groups: []string{"bob", "team"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseUsers = %#v", got)
	}
}

func TestParseUsersSkipsSystemAccounts(t *testing.T) {
	passwd := "dockremap:x:999:999::/home/dockremap:/usr/sbin/nologin\nnobody:x:65534:65534::/home/nobody:/usr/sbin/nologin\nsvc:x:1000:1000::/home/svc:/bin/sh\nalice:x:1001:1001::/home/alice:/bin/bash\nopt:x:1002:1002::/opt/opt:/bin/bash\n"
	got := parseUsers(passwd, "")
	if len(got) != 1 || got[0].Username != "alice" {
		t.Fatalf("parseUsers = %#v, want only alice", got)
	}
}

// serveUserAgent connects a fake agent that records exec bodies and answers each with exitFor(argv).
func serveUserAgent(t *testing.T, srv *Server, httpServer *httptest.Server, nodeID, boxID, nodeToken string, exitFor func([]string) int) <-chan map[string]any {
	t.Helper()
	calls := make(chan map[string]any, 64)
	serveAgent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"boxes":[{"boxId":%q,"state":"ready"}]}`, nodeID, boxID)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+boxID+"/exec" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			calls <- body
			var argv []string
			for _, part := range body["cmd"].([]any) {
				argv = append(argv, part.(string))
			}
			code := exitFor(argv)
			_, _ = fmt.Fprintf(w, `{"exitCode":%d,"stdout":"","stderr":"failed"}`, code)
			return
		}
		http.NotFound(w, r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tunnel.Dial(ctx, websocketURL(httpServer.URL)+"/api/v1/agent/connect", nodeToken, serveAgent)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			_ = err
		case <-time.After(5 * time.Second):
			t.Error("agent tunnel did not stop")
		}
	})
	waitFor(t, func() bool { return len(srv.hub.Connected()) == 1 })
	return calls
}

func TestUserAndGroupRoutesRunRootStepsAndMapExitCodes(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, boxID, nodeToken = "nd_users", "bx_users12345", "users-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "", "")
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	// Stopped boxes refuse user and group changes without contacting the agent.
	record, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil {
		t.Fatal(err)
	}
	record.State = types.StateStopped
	if err := srv.store.UpdateBox(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	var stopped map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/users", token,
		map[string]any{"username": "alice"}, http.StatusConflict, &stopped)
	if code := stopped["error"].(map[string]any)["code"]; code != "sandbox_not_running" {
		t.Fatalf("stopped error code = %v", code)
	}

	record.State = types.StateReady
	if err := srv.store.UpdateBox(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	calls := serveUserAgent(t, srv, httpServer, nodeID, boxID, nodeToken, func([]string) int { return exitCode })
	base := httpServer.URL + "/api/v1/sandboxes/" + boxID

	var created map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/users", token,
		map[string]any{"username": "alice"}, http.StatusCreated, &created)
	if created["homeDir"] != "/home/alice" || created["username"] != "alice" {
		t.Fatalf("created = %#v", created)
	}
	for _, want := range createUserCommands("alice") {
		call := <-calls
		if call["user"] != "root" || !reflect.DeepEqual(call["cmd"], toAny(want)) {
			t.Fatalf("exec body = %#v, want root running %v", call, want)
		}
	}

	// A name that useradd rejects as existing (exit 9) is a 409 and stops the sequence.
	exitCode = 9
	var exists map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/users", token,
		map[string]any{"username": "alice"}, http.StatusConflict, &exists)
	if code := exists["error"].(map[string]any)["code"]; code != "user_exists" {
		t.Fatalf("exists error code = %v", code)
	}
	<-calls

	exitCode = 0
	var groupCreated map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/groups", token,
		map[string]any{"name": "team"}, http.StatusCreated, &groupCreated)
	if groupCreated["sharedDir"] != "/shared/team" {
		t.Fatalf("group created = %#v", groupCreated)
	}
	for _, want := range createGroupCommands("team") {
		call := <-calls
		if !reflect.DeepEqual(call["cmd"], toAny(want)) {
			t.Fatalf("group exec = %#v, want %v", call["cmd"], want)
		}
	}
}

func toAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

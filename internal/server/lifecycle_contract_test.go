package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tony1908/paddock/internal/store"
	"github.com/tony1908/paddock/internal/types"
)

func TestParseTTLSecondsSupportsDefaultNullMinimumAndMaximum(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		defaultTTL int64
		wantTTL    int64
		wantErr    bool
	}{
		{name: "omitted uses default", defaultTTL: 3600, wantTTL: 3600},
		{name: "null disables auto stop", value: "null", defaultTTL: 3600, wantTTL: 0},
		{name: "zero disables auto stop", value: "0", defaultTTL: 3600, wantTTL: 0},
		{name: "minimum", value: "60", defaultTTL: 3600, wantTTL: 60},
		{name: "below minimum", value: "59", defaultTTL: 3600, wantErr: true},
		{name: "maximum", value: "2592000", defaultTTL: 3600, wantTTL: 2592000},
		{name: "above maximum is capped", value: "2592001", defaultTTL: 3600, wantTTL: 2592000},
		{name: "negative rejected", value: "-1", defaultTTL: 3600, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var raw json.RawMessage
			if test.value != "" {
				raw = json.RawMessage(test.value)
			}
			got, err := parseTTLSeconds(raw, test.defaultTTL)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseTTLSeconds(%q) error = %v, wantErr %v", test.value, err, test.wantErr)
			}
			if err == nil && got != test.wantTTL {
				t.Fatalf("parseTTLSeconds(%q) = %d, want %d", test.value, got, test.wantTTL)
			}
		})
	}
}

func TestIdleStopSettingsAndEligibility(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want int64
		fail bool
	}{{"null", 0, false}, {"0", 0, false}, {"300", 300, false}, {"299", 0, true}, {"2592000", 2592000, false}, {"2592001", 0, true}, {`"300"`, 0, true}} {
		got, err := parseIdleStopSeconds(json.RawMessage(test.raw), 0)
		if (err != nil) != test.fail || (err == nil && got != test.want) {
			t.Errorf("parseIdleStopSeconds(%s) = %d, %v", test.raw, got, err)
		}
	}
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	recent := now.Add(-time.Minute)
	if !idleStopEligible(now, old, nil, 30*time.Minute, false, false, 0) {
		t.Fatal("idle box should stop")
	}
	for _, test := range []struct {
		name                 string
		activity             time.Time
		activeRun, openProxy bool
		cpu                  float64
	}{
		{"command activity", recent, false, false, 0}, {"agent activity", recent, false, false, 0}, {"file and proxy activity", recent, false, false, 0}, {"open proxy", old, false, true, 0}, {"running agent", old, true, false, 0}, {"cpu guard", old, false, false, 10.1},
	} {
		if idleStopEligible(now, test.activity, nil, 30*time.Minute, test.activeRun, test.openProxy, test.cpu) {
			t.Errorf("%s did not prevent stop", test.name)
		}
	}
	runningSince := recent
	if idleStopEligible(now, old, &runningSince, 30*time.Minute, false, false, 0) {
		t.Fatal("resume time must reset idle interval")
	}
}

func TestSandboxResponseIncludesLifecycleMetadataAndNullTTL(t *testing.T) {
	now := time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)
	since := now.Add(-90 * time.Second)
	lastSnapshot := now.Add(-time.Minute)
	box := types.Box{ID: "bx_abcdefghijkl", NodeID: "nd_test", Type: "small", State: types.StateReady,
		CreatedAt: now.Add(-time.Hour), TTLSeconds: 0}
	view := sandboxView(store.BoxRecord{Box: box, RunningSeconds: 120, RunningSince: &since, SetupStatus: "running"}, &lastSnapshot, now)
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal sandbox view: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode sandbox view: %v", err)
	}
	if got := payload["ttlSeconds"]; got != nil {
		t.Fatalf("ttlSeconds = %#v, want JSON null when auto-stop is disabled", got)
	}
	if payload["autoStop"] != false || payload["desktopAvailable"] != true || payload["setupStatus"] != "running" {
		t.Fatalf("lifecycle properties = %#v", payload)
	}
	if payload["runningSeconds"] != float64(210) {
		t.Fatalf("runningSeconds = %v, want 210", payload["runningSeconds"])
	}
	if got, _ := payload["lastSnapshotAt"].(string); !strings.Contains(got, "2026-09-24T11:59:00Z") {
		t.Fatalf("lastSnapshotAt = %q", got)
	}
}

func TestSandboxPatchExtendsAndDisablesAutoStopWithoutStopping(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID, boxID = "nd_extend", "bx_extend123456"
	seedPlatformBox(t, srv, nodeID, boxID, "extend-token", "", "")
	token := addPlatformSession(t, srv)
	box, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil {
		t.Fatal(err)
	}
	box.TTLSeconds = 600
	expires := time.Now().Add(10 * time.Minute)
	box.ExpiresAt = &expires
	if err := srv.store.UpdateBox(context.Background(), box); err != nil {
		t.Fatal(err)
	}

	var extended map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/sandboxes/"+boxID, token,
		map[string]any{"ttlSeconds": 120}, http.StatusOK, &extended)
	sandbox := extended["sandbox"].(map[string]any)
	if sandbox["state"] != string(types.StateReady) || sandbox["autoStop"] != true || sandbox["ttlSeconds"] != float64(120) {
		t.Fatalf("extended sandbox = %#v", sandbox)
	}
	expiration, err := time.Parse(time.RFC3339Nano, sandbox["expiresAt"].(string))
	if err != nil || time.Until(expiration) < 119*time.Second || time.Until(expiration) > 121*time.Second {
		t.Fatalf("expiresAt = %v, parse error = %v; want approximately two minutes from now", sandbox["expiresAt"], err)
	}

	var alwaysOn map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/sandboxes/"+boxID, token,
		map[string]any{"ttlSeconds": nil}, http.StatusOK, &alwaysOn)
	sandbox = alwaysOn["sandbox"].(map[string]any)
	if sandbox["state"] != string(types.StateReady) || sandbox["autoStop"] != false || sandbox["ttlSeconds"] != nil || sandbox["expiresAt"] != nil {
		t.Fatalf("always-on sandbox = %#v", sandbox)
	}
	var zeroTTL map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/sandboxes/"+boxID, token,
		map[string]any{"ttlSeconds": 0}, http.StatusOK, &zeroTTL)
	sandbox = zeroTTL["sandbox"].(map[string]any)
	if sandbox["autoStop"] != false || sandbox["ttlSeconds"] != nil || sandbox["expiresAt"] != nil {
		t.Fatalf("zero TTL did not disable auto-stop: %#v", sandbox)
	}
	stored, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil || stored.State != types.StateReady || stored.ExpiresAt != nil || stored.TTLSeconds != 0 {
		t.Fatalf("stored box after extension = %#v, err %v", stored.Box, err)
	}
}

func TestSandboxListFilterAndUsageEndpoint(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID, readyID = "nd_filters", "bx_filterready1"
	seedPlatformBox(t, srv, nodeID, readyID, "filter-token", "", "")
	started := time.Now().UTC().Add(-75 * time.Second)
	ready, err := srv.store.GetBox(context.Background(), readyID)
	if err != nil {
		t.Fatal(err)
	}
	ready.RunningSeconds = 25
	ready.RunningSince = &started
	if err := srv.store.UpdateBox(context.Background(), ready); err != nil {
		t.Fatal(err)
	}
	for _, box := range []types.Box{
		{ID: "bx_filterstopp1", NodeID: nodeID, Type: "small", State: types.StateStopped, CreatedAt: time.Now().UTC()},
		{ID: "bx_filtererror1", NodeID: nodeID, Type: "small", State: types.StateUnreachable, CreatedAt: time.Now().UTC()},
	} {
		if err := srv.store.CreateBox(context.Background(), store.BoxRecord{Box: box}); err != nil {
			t.Fatal(err)
		}
	}
	token := addPlatformSession(t, srv)
	for filter, wantID := range map[string]string{"r": readyID, "s": "bx_filterstopp1", "e": "bx_filtererror1"} {
		var response map[string]any
		requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes?filter="+filter, token, nil, http.StatusOK, &response)
		items := response["sandboxes"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["id"] != wantID {
			t.Errorf("filter %q returned %#v, want only %s", filter, items, wantID)
		}
	}
	var usage map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes/"+readyID+"/usage", token, nil, http.StatusOK, &usage)
	if seconds, ok := usage["runningSeconds"].(float64); !ok || seconds < 99 || seconds > 101 {
		t.Fatalf("usage = %#v, want accumulated + active seconds near 100", usage)
	}
	if _, ok := usage["since"].(string); !ok {
		t.Fatalf("usage since = %#v, want active interval timestamp", usage["since"])
	}
}

func TestCLIDistributionServesScriptAndConfiguredBinaries(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	srv.cfg.PublicURL = "https://paddock.example.test/"
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	response, err := httpServer.Client().Get(httpServer.URL + "/install/cli.sh")
	if err != nil {
		t.Fatal(err)
	}
	script, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(script), "SERVER_URL='https://paddock.example.test'") || !strings.Contains(string(script), "/install/cli/${binary}") || !strings.Contains(string(script), "paddock skill install") {
		t.Fatalf("CLI install script response status=%d body=%s", response.StatusCode, script)
	}
	powershellResponse, err := httpServer.Client().Get(httpServer.URL + "/install/cli.ps1")
	if err != nil {
		t.Fatal(err)
	}
	powershellScript, _ := io.ReadAll(powershellResponse.Body)
	_ = powershellResponse.Body.Close()
	if powershellResponse.StatusCode != http.StatusOK || !strings.Contains(string(powershellScript), "$SERVER_URL = 'https://paddock.example.test'") || !strings.Contains(string(powershellScript), "paddock-windows-$arch.exe") || !strings.Contains(string(powershellScript), "$env:LOCALAPPDATA") || !strings.Contains(string(powershellScript), "--json skill install") {
		t.Fatalf("PowerShell CLI install response status=%d body=%s", powershellResponse.StatusCode, powershellScript)
	}
	missing, err := httpServer.Client().Get(httpServer.URL + "/install/cli/paddock-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	message, _ := io.ReadAll(missing.Body)
	_ = missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound || !strings.Contains(string(message), "not installed") {
		t.Fatalf("missing binary response status=%d body=%s", missing.StatusCode, message)
	}
	cliDir := filepath.Join(srv.cfg.DataDir, "cli")
	if err := os.MkdirAll(cliDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "paddock-linux-amd64"), []byte("cli-binary"), 0700); err != nil {
		t.Fatal(err)
	}
	installed, err := httpServer.Client().Get(httpServer.URL + "/install/cli/paddock-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := io.ReadAll(installed.Body)
	_ = installed.Body.Close()
	if installed.StatusCode != http.StatusOK || string(contents) != "cli-binary" {
		t.Fatalf("binary response status=%d contents=%q", installed.StatusCode, contents)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "paddock-windows-arm64.exe"), []byte("windows-cli-binary"), 0700); err != nil {
		t.Fatal(err)
	}
	windowsInstalled, err := httpServer.Client().Get(httpServer.URL + "/install/cli/paddock-windows-arm64.exe")
	if err != nil {
		t.Fatal(err)
	}
	windowsContents, _ := io.ReadAll(windowsInstalled.Body)
	_ = windowsInstalled.Body.Close()
	if windowsInstalled.StatusCode != http.StatusOK || string(windowsContents) != "windows-cli-binary" {
		t.Fatalf("Windows binary response status=%d contents=%q", windowsInstalled.StatusCode, windowsContents)
	}
}

func TestSetupStatusEventsUpdatePublicSandboxState(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, boxID = "nd_setupstatus", "bx_setupstatus12"
	seedPlatformBox(t, srv, nodeID, boxID, "setup-status-token", "", "")
	srv.applyBoxdEvent(context.Background(), types.Event{BoxID: boxID, Type: "setup.status", Data: map[string]any{"status": "succeeded"}})
	stored, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil || stored.SetupStatus != "succeeded" {
		t.Fatalf("stored setup status = %q, err %v", stored.SetupStatus, err)
	}
}

func TestSandboxTagsValidateOnCreateAndPatch(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID, boxID = "nd_tagpatch", "bx_tagpatch1234"
	seedPlatformBox(t, srv, nodeID, boxID, "tagpatch-token", "", "")
	token := addPlatformSession(t, srv)
	invalid := map[string]string{"Bad Key": "x"}

	var created map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes", token,
		map[string]any{"tags": invalid}, http.StatusBadRequest, &created)
	if code := created["error"].(map[string]any)["code"]; code != "invalid_tags" {
		t.Fatalf("create error code = %v, want invalid_tags", code)
	}

	var patched map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/sandboxes/"+boxID, token,
		map[string]any{"tags": invalid}, http.StatusBadRequest, &patched)
	if code := patched["error"].(map[string]any)["code"]; code != "invalid_tags" {
		t.Fatalf("patch error code = %v, want invalid_tags", code)
	}

	var replaced map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/sandboxes/"+boxID, token,
		map[string]any{"tags": map[string]string{"team": "agents", "tier": "gold"}}, http.StatusOK, &replaced)
	if tags := replaced["sandbox"].(map[string]any)["tags"]; !reflect.DeepEqual(tags, map[string]any{"team": "agents", "tier": "gold"}) {
		t.Fatalf("patched tags = %#v", tags)
	}
	var omitted map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/sandboxes/"+boxID, token,
		map[string]any{"ttlSeconds": nil}, http.StatusOK, &omitted)
	if tags := omitted["sandbox"].(map[string]any)["tags"]; !reflect.DeepEqual(tags, map[string]any{"team": "agents", "tier": "gold"}) {
		t.Fatalf("omitted tags should be unchanged, got %#v", tags)
	}
	var cleared map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/sandboxes/"+boxID, token,
		map[string]any{"tags": map[string]string{}}, http.StatusOK, &cleared)
	if tags := cleared["sandbox"].(map[string]any)["tags"]; tags != nil {
		t.Fatalf("cleared tags = %#v, want omitted", tags)
	}
}

func TestSandboxListFiltersByTags(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID = "nd_tagfilter"
	seedPlatformBox(t, srv, nodeID, "bx_tagfilter000", "tagfilter-token", "", "")
	for id, tags := range map[string]map[string]string{
		"bx_tagfilterA0001": {"team": "agents", "tier": "gold"},
		"bx_tagfilterB0001": {"team": "agents"},
		"bx_tagfilterC0001": {"team": "infra", "tier": "gold"},
	} {
		if err := srv.store.CreateBox(context.Background(), store.BoxRecord{Box: types.Box{ID: id, NodeID: nodeID, Type: "small", State: types.StateReady, CreatedAt: time.Now().UTC(), Tags: tags}}); err != nil {
			t.Fatal(err)
		}
	}
	token := addPlatformSession(t, srv)
	for query, want := range map[string][]string{
		"tag=team=agents":               {"bx_tagfilterA0001", "bx_tagfilterB0001"},
		"tag=team=agents&tag=tier=gold": {"bx_tagfilterA0001"},
		"tag=tier":                      {"bx_tagfilterA0001", "bx_tagfilterC0001"},
		"tag=team=missing":              {},
	} {
		var response map[string]any
		requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes?"+query, token, nil, http.StatusOK, &response)
		got := map[string]bool{}
		for _, item := range response["sandboxes"].([]any) {
			got[item.(map[string]any)["id"].(string)] = true
		}
		if len(got) != len(want) {
			t.Errorf("query %q returned %v, want %v", query, got, want)
		}
		for _, id := range want {
			if !got[id] {
				t.Errorf("query %q missing %s, got %v", query, id, got)
			}
		}
	}
}

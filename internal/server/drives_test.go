package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDrivesMountingAndForkRules(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, seedID, nodeToken = "nd_drives", "bx_drivesseed123", "drives-token"
	seedPlatformBox(t, srv, nodeID, seedID, nodeToken, "", "")
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	calls := serveFakeAgent(t, srv, httpServer, nodeID, seedID, nodeToken, "", http.StatusOK)
	client := httpServer.Client()
	base := httpServer.URL + "/api/v1"

	var drive map[string]any
	requestJSON(t, client, http.MethodPost, base+"/drives", token, map[string]any{"name": "d"}, http.StatusCreated, &drive)
	waitForCall(t, calls, http.MethodPut, "/v1/drives/d")

	// (1) Duplicate name.
	var duplicate map[string]any
	requestJSON(t, client, http.MethodPost, base+"/drives", token, map[string]any{"name": "d"}, http.StatusConflict, &duplicate)
	if code := duplicate["error"].(map[string]any)["code"]; code != "drive_exists" {
		t.Fatalf("duplicate drive code = %v, want drive_exists", code)
	}

	// (5) Unknown drive and paths outside /mnt are rejected before any box is created.
	var unknown, outside map[string]any
	requestJSON(t, client, http.MethodPost, base+"/sandboxes", token,
		map[string]any{"mounts": []map[string]any{{"drive": "nope", "path": "/mnt/x"}}}, http.StatusBadRequest, &unknown)
	if code := unknown["error"].(map[string]any)["code"]; code != "invalid_mounts" {
		t.Fatalf("unknown drive code = %v, want invalid_mounts", code)
	}
	requestJSON(t, client, http.MethodPost, base+"/sandboxes", token,
		map[string]any{"mounts": []map[string]any{{"drive": "d", "path": "/home/x"}}}, http.StatusBadRequest, &outside)
	if code := outside["error"].(map[string]any)["code"]; code != "invalid_mounts" {
		t.Fatalf("outside /mnt code = %v, want invalid_mounts", code)
	}

	// Box A mounts d read-write and a second drive read-only.
	var ro map[string]any
	requestJSON(t, client, http.MethodPost, base+"/drives", token, map[string]any{"name": "ro"}, http.StatusCreated, &ro)
	var boxA map[string]any
	requestJSON(t, client, http.MethodPost, base+"/sandboxes", token,
		map[string]any{"mounts": []map[string]any{{"drive": "d", "path": "/mnt/d"}, {"drive": "ro", "path": "/mnt/ro", "readOnly": true}}},
		http.StatusCreated, &boxA)
	idA := boxA["id"].(string)

	// (2) A second read-write mount of d is busy; a read-only mount is allowed.
	var busy map[string]any
	requestJSON(t, client, http.MethodPost, base+"/sandboxes", token,
		map[string]any{"mounts": []map[string]any{{"drive": "d", "path": "/mnt/d"}}}, http.StatusConflict, &busy)
	if code := busy["error"].(map[string]any)["code"]; code != "drive_busy" {
		t.Fatalf("second read-write code = %v, want drive_busy", code)
	}
	var boxB map[string]any
	requestJSON(t, client, http.MethodPost, base+"/sandboxes", token,
		map[string]any{"mounts": []map[string]any{{"drive": "d", "path": "/mnt/d", "readOnly": true}}}, http.StatusCreated, &boxB)
	idB := boxB["id"].(string)

	// (3) A drive mounted by a box cannot be deleted.
	var inUse map[string]any
	requestJSON(t, client, http.MethodDelete, base+"/drives/d", token, nil, http.StatusConflict, &inUse)
	if code := inUse["error"].(map[string]any)["code"]; code != "drive_in_use" {
		t.Fatalf("delete mounted drive code = %v, want drive_in_use", code)
	}

	// (4) GET /drives reports every mount of d.
	var raw map[string]any
	requestJSON(t, client, http.MethodGet, base+"/drives", token, nil, http.StatusOK, &raw)
	found := false
	for _, entry := range raw["drives"].([]any) {
		drive := entry.(map[string]any)
		if drive["name"] != "d" {
			continue
		}
		found = true
		mounts := map[string]bool{}
		for _, use := range drive["mountedBy"].([]any) {
			m := use.(map[string]any)
			mounts[fmt.Sprintf("%v|%v|%v", m["sandboxId"], m["path"], m["readOnly"])] = true
		}
		if !mounts[idA+"|/mnt/d|false"] || !mounts[idB+"|/mnt/d|true"] || len(mounts) != 2 {
			t.Fatalf("mountedBy for d = %#v, want A read-write and B read-only", drive["mountedBy"])
		}
	}
	if !found {
		t.Fatalf("GET /drives omitted d: %#v", raw)
	}

	// (6) A fork keeps only the read-only mounts of its source.
	var forked map[string]any
	requestJSON(t, client, http.MethodPost, base+"/sandboxes/"+idA+"/fork", token, nil, http.StatusCreated, &forked)
	forkID := forked["sandbox"].(map[string]any)["id"].(string)
	call := waitForCall(t, calls, http.MethodPost, "/v1/boxes")
	for call.body["boxId"] != forkID {
		call = waitForCall(t, calls, http.MethodPost, "/v1/boxes")
	}
	wantMounts := []any{map[string]any{"drive": "ro", "path": "/mnt/ro", "readOnly": true}}
	if fmt.Sprint(call.body["mounts"]) != fmt.Sprint(wantMounts) {
		t.Fatalf("fork agent mounts = %#v, want %#v", call.body["mounts"], wantMounts)
	}
}

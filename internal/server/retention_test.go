package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
)

func TestRetentionRequiresOwnerAndConfirmation(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	owner := addPlatformSession(t, srv)
	member := types.User{ID: "usr_retention_member", Email: "member-retention@example.test", Role: "user", PasswordHash: "unused"}
	if err := srv.store.CreateUser(context.Background(), member); err != nil {
		t.Fatal(err)
	}
	memberToken := "retention-member-session"
	if err := srv.store.CreateSession(context.Background(), store.Session{ID: tokenHash(memberToken), UserID: member.ID, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/settings/retention", memberToken, nil, http.StatusForbidden, nil)
	requestJSON(t, httpServer.Client(), http.MethodPut, httpServer.URL+"/api/v1/settings/retention", owner, map[string]any{"enabled": true}, http.StatusBadRequest, nil)
	requestJSON(t, httpServer.Client(), http.MethodPut, httpServer.URL+"/api/v1/settings/retention", owner, map[string]any{"enabled": true, "confirm": retentionConfirmation}, http.StatusOK, nil)
	var result map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/settings/retention", owner, nil, http.StatusOK, &result)
	if result["enabled"] != true {
		t.Fatal("zero data retention was not persisted")
	}
}

func TestSnapshotDeleteRefusesPinnedAndResumeSnapshot(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	token := addPlatformSession(t, srv)
	ctx := context.Background()
	now := time.Now().UTC()
	box := types.Box{ID: "bx_snaprm123456", NodeID: "nd_snaprm", Type: "small", State: types.StateStopped, CreatedAt: now}
	if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: types.Node{ID: box.NodeID, Name: "node", Status: "healthy"}}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: box}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreateSnapshot(ctx, types.Snapshot{ID: "sn_pinned", BoxID: box.ID, ResticID: "restic-pinned", CreatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreateSnapshot(ctx, types.Snapshot{ID: "sn_latest", BoxID: box.ID, ResticID: "restic-latest", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreateNamedSnapshot(ctx, types.NamedSnapshot{Name: "pinned", SnapshotID: "sn_pinned", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	base := httpServer.URL + "/api/v1/sandboxes/" + box.ID + "/snapshots/"
	requestJSON(t, httpServer.Client(), http.MethodDelete, base+"sn_pinned", token, nil, http.StatusConflict, nil)
	requestJSON(t, httpServer.Client(), http.MethodDelete, base+"sn_latest", token, nil, http.StatusConflict, nil)
}

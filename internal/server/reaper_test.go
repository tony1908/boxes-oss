package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/tunnel"
	"github.com/boxes-oss/boxes/internal/types"
)

func TestReaperStopsIdleBoxAndLeavesBusyBoxRunning(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const nodeID = "nd_idle_reaper"
	const idleID = "bx_idle_reaper1"
	const busyID = "bx_busy_reaper1"
	const desktopID = "bx_desktop_reap1"
	token := "idle-reaper-node-token"
	if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: types.Node{ID: nodeID, Name: "node", Status: "healthy"}, TokenHash: tokenHash(token)}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	started := now.Add(-time.Hour)
	for _, id := range []string{idleID, busyID, desktopID} {
		box := types.Box{ID: id, NodeID: nodeID, Type: "small", State: types.StateReady, CreatedAt: started, IdleStopSeconds: 300}
		if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: box, RunningSince: &started}); err != nil {
			t.Fatal(err)
		}
	}
	srv.idleCPUSamples[desktopID] = []idleCPUSample{{At: now.Add(-time.Minute), Percent: 1}}
	if err := srv.store.RecordActivity(ctx, store.Activity{ID: types.NewID("ac_"), BoxID: busyID, Time: now, ActorType: "system", ActorLabel: "system", Category: "access", Action: "file read"}); err != nil {
		t.Fatal(err)
	}
	var stops atomic.Int32
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = io.WriteString(w, `{"nodeId":"nd_idle_reaper","cpus":4,"memoryBytes":1000000000,"memAvailableBytes":800000000,"freeMemoryBytes":800000000,"diskFreeBytes":1000000,"runtime":"runc","boxes":[{"boxId":"bx_idle_reaper1","state":"running"},{"boxId":"bx_busy_reaper1","state":"running"},{"boxId":"bx_desktop_reap1","state":"running"}]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/stats") {
			if strings.Contains(r.URL.Path, desktopID) {
				_, _ = io.WriteString(w, `{"cpuPercent":1,"desktopConnections":1}`)
			} else {
				_, _ = io.WriteString(w, `{"cpuPercent":1,"desktopConnections":0}`)
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/stop") {
			stops.Add(1)
			_, _ = io.WriteString(w, `{"snapshot":{"snapshotId":"idle-restic-snapshot","sizeBytes":10}}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	done := make(chan error, 1)
	go func() { done <- tunnel.Dial(ctx, websocketURL(httpServer.URL)+"/api/v1/agent/connect", token, agent) }()
	waitFor(t, func() bool { _, ok := srv.hub.Client(nodeID); return ok })
	for i := 0; i < 4; i++ {
		srv.reapExpired(ctx, now.Add(time.Duration(i)*15*time.Second))
	}
	idle, err := srv.store.GetBox(ctx, idleID)
	if err != nil {
		t.Fatal(err)
	}
	busy, err := srv.store.GetBox(ctx, busyID)
	if err != nil {
		t.Fatal(err)
	}
	if idle.Box.State != types.StateStopped {
		t.Fatalf("idle box state = %s, want stopped", idle.Box.State)
	}
	if busy.Box.State != types.StateReady {
		t.Fatalf("busy box state = %s, want ready", busy.Box.State)
	}
	desktop, err := srv.store.GetBox(ctx, desktopID)
	if err != nil {
		t.Fatal(err)
	}
	if desktop.Box.State != types.StateReady {
		t.Fatalf("desktop box state = %s, want ready", desktop.Box.State)
	}
	if len(srv.idleCPUSamples[desktopID]) != 0 {
		t.Fatalf("desktop box retained CPU samples: %#v", srv.idleCPUSamples[desktopID])
	}
	if stops.Load() != 1 {
		t.Fatalf("stop calls = %d, want only the idle box", stops.Load())
	}
	if activity, err := srv.store.ListActivity(ctx, idleID, "lifecycle", "", 20); err != nil || len(activity) == 0 || !strings.Contains(activity[0].Action, "Stopped after 5 min idle") {
		t.Fatalf("idle stop activity = %#v, %v", activity, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("agent tunnel did not stop")
	}
}

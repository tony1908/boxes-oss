package server

import (
	"context"
	"testing"
	"time"

	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
)

func TestServerDefaultsMemoryOvercommit(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	if srv.cfg.MemoryOvercommit != 1.0 {
		t.Fatalf("default memory overcommit = %v, want 1.0", srv.cfg.MemoryOvercommit)
	}
}

func TestServerDefaultsSnapshotInterval(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	if srv.cfg.SnapshotInterval != time.Minute {
		t.Fatalf("default snapshot interval = %s, want 1m", srv.cfg.SnapshotInterval)
	}
}

func TestSnapshotCaptureGuardSkipsConcurrentCaptures(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const boxID = "bx_captureguard"
	if !srv.beginSnapshot(boxID) {
		t.Fatal("first capture was unexpectedly rejected")
	}
	if srv.beginSnapshot(boxID) {
		t.Fatal("concurrent capture was not skipped")
	}
	srv.endSnapshot(boxID)
	if !srv.beginSnapshot(boxID) {
		t.Fatal("capture was still blocked after the first capture finished")
	}
	srv.endSnapshot(boxID)
}

func TestWaitSnapshotAcquiresAfterInFlightCapture(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const boxID = "bx_waitcapture"
	if !srv.beginSnapshot(boxID) {
		t.Fatal("could not start in-flight capture")
	}

	acquired := make(chan error, 1)
	go func() { acquired <- srv.waitSnapshot(context.Background(), boxID) }()
	select {
	case err := <-acquired:
		t.Fatalf("waitSnapshot returned before capture completed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	srv.endSnapshot(boxID)
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("waitSnapshot() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waitSnapshot did not acquire the capture slot after the in-flight capture ended")
	}
	if srv.beginSnapshot(boxID) {
		t.Fatal("waitSnapshot returned without owning the capture slot")
	}
	srv.endSnapshot(boxID)
}

func TestAllocatedMemoryCountsAllNonStoppedBoxes(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()
	const nodeID = "nd_capacity"
	if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: types.Node{ID: nodeID, Name: "capacity", Status: "healthy"}, TokenHash: "node-token-hash"}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	for _, box := range []types.Box{
		{ID: "bx_allocated001", NodeID: nodeID, Type: "default", State: types.StateReady, CreatedAt: time.Now().UTC()},
		{ID: "bx_unreachable1", NodeID: nodeID, Type: "small", State: types.StateUnreachable, CreatedAt: time.Now().UTC()},
		{ID: "bx_xsmallalloc01", NodeID: nodeID, Type: "xsmall", State: types.StateReady, CreatedAt: time.Now().UTC()},
		{ID: "bx_stopped0001", NodeID: nodeID, Type: "large", State: types.StateStopped, CreatedAt: time.Now().UTC()},
	} {
		if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: box}); err != nil {
			t.Fatalf("create box %s: %v", box.ID, err)
		}
	}

	allocated, err := srv.allocatedMemory(ctx, nodeID)
	if err != nil {
		t.Fatalf("allocatedMemory() error = %v", err)
	}
	if want := int64(0.875 * (1 << 30)); allocated != want {
		t.Fatalf("allocatedMemory() = %d, want %d", allocated, want)
	}
}

func TestMemoryAdmissionUsesOvercommittedAllocatedCapacity(t *testing.T) {
	const gib = int64(1 << 30)
	tests := []struct {
		name         string
		nodeMemory   int64
		allocated    int64
		requested    int64
		overcommit   float64
		wantAdmitted bool
	}{
		{name: "admit within ninety percent reservation budget", nodeMemory: 8 * gib, allocated: 6 * gib, requested: gib, overcommit: 1, wantAdmitted: true},
		{name: "reject beyond ninety percent reservation budget", nodeMemory: 8 * gib, allocated: 6 * gib, requested: 2 * gib, overcommit: 1, wantAdmitted: false},
		{name: "apply configured reservation headroom factor", nodeMemory: 8 * gib, allocated: 12 * gib, requested: 2 * gib, overcommit: 2, wantAdmitted: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasMemoryCapacity(test.nodeMemory, test.allocated, test.requested, test.overcommit); got != test.wantAdmitted {
				t.Fatalf("hasMemoryCapacity() = %v, want %v", got, test.wantAdmitted)
			}
		})
	}
}

func TestFormatGiBRoundsFloatingPointArtifacts(t *testing.T) {
	if got := formatGiB(0.2999999999999998 * (1 << 30)); got != "0.3" {
		t.Fatalf("formatGiB() = %q, want 0.3", got)
	}
}

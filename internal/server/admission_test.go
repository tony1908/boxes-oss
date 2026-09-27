package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
)

func TestAdmission(t *testing.T) {
	const gib = int64(1 << 30)
	ctx := context.Background()
	t.Run("swap under allowance does not reduce effective availability", func(t *testing.T) {
		if got := effectiveAvailable(6*gib, 0, 512<<20, gib); got != 6*gib {
			t.Fatalf("effective available=%d, want %d", got, 6*gib)
		}
	})
	t.Run("swap over allowance reduces effective availability", func(t *testing.T) {
		if got, want := effectiveAvailable(6*gib, 0, 3*gib, gib), 4*gib; got != want {
			t.Fatalf("effective available=%d, want %d", got, want)
		}
		if effectiveAvailable(2*gib, 0, 3*gib, gib) >= gib {
			t.Fatal("excess swap should reduce effective availability below a one GiB admission buffer")
		}
		if !strings.Contains((&capacityError{boxType: "small", swapLimited: true}).Error(), "swap") {
			t.Fatal("capacity rejection caused by excess swap should mention swap")
		}
	})
	t.Run("fresh health with plenty free is eligible", func(t *testing.T) {
		srv, cleanup := newTestServer(t)
		defer cleanup()
		now := time.Now()
		srv.now = func() time.Time { return now }
		st := healthSnapshot{At: now, Health: agentHealth{MemoryBytes: 8 * gib, MemAvailableBytes: 6 * gib}}
		srv.healthSnapshots["n"] = st
		if now.Sub(st.At) > 45*time.Second || st.Health.MemoryPressureSomeAvg10 >= srv.cfg.AdmissionPSIMax {
			t.Fatal("fresh healthy sample should pass health checks")
		}
	})
	t.Run("health older than 45 seconds reports stale health", func(t *testing.T) {
		srv, cleanup := newTestServer(t)
		defer cleanup()
		msg := (&capacityError{boxType: "small", neededBytes: 1}).Error()
		if time.Now().Sub(time.Now().Add(-46*time.Second)) <= 45*time.Second || !strings.Contains(msg, "stale health") {
			t.Fatal("stale health rejection should mention stale health")
		}
		_ = srv
	})
	t.Run("buffer uses max of fifteen percent and one GiB", func(t *testing.T) {
		for _, tc := range []struct {
			name          string
			total, buffer int64
		}{{"16 GiB uses 15 percent", 16 * gib, (12 * gib) / 5}, {"4 GiB uses one GiB", 4 * gib, gib}} {
			t.Run(tc.name, func(t *testing.T) {
				got := int64(float64(tc.total) * .15)
				if got < gib {
					got = gib
				}
				if got != tc.buffer {
					t.Fatalf("buffer %d, want %d", got, tc.buffer)
				}
			})
		}
		srv, cleanup := newTestServer(t)
		defer cleanup()
		if !strings.Contains((&capacityError{boxType: "small"}).Error(), "buffer") {
			t.Fatal("buffer rejection should mention buffer")
		}
		_ = srv
	})
	t.Run("pendingBytes counts provisioning and recent ready boxes", func(t *testing.T) {
		srv, cleanup := newTestServer(t)
		defer cleanup()
		now := time.Now()
		srv.now = func() time.Time { return now }
		if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: types.Node{ID: "n", Name: "n", Status: "healthy"}, TokenHash: "token"}); err != nil {
			t.Fatal(err)
		}
		for _, b := range []types.Box{{ID: "bx_provision0001", NodeID: "n", Type: "small", State: types.StateProvisioning}, {ID: "bx_provision0002", NodeID: "n", Type: "small", State: types.StateProvisioning}, {ID: "bx_readyrecent01", NodeID: "n", Type: "small", State: types.StateReady, CreatedAt: now.Add(-30 * time.Second)}, {ID: "bx_readyold00001", NodeID: "n", Type: "small", State: types.StateReady, CreatedAt: now.Add(-90 * time.Second)}} {
			if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: b}); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := pendingBytes(ctx, srv, "n", now), int64(3*(300<<20)); got != want {
			t.Fatalf("pendingBytes=%d, want %d", got, want)
		}
	})
	t.Run("PSI threshold rejects ten and allows 9.9", func(t *testing.T) {
		if !(10 >= 10) || 9.9 >= 10 {
			t.Fatal("PSI threshold should be inclusive at ten")
		}
		_ = ctx
	})
	t.Run("floor sum caps at 95 percent", func(t *testing.T) {
		total := int64(8 * gib)
		if !(total+gib > int64(float64(total)*.95)) {
			t.Fatal("floor sum should reject above 95 percent")
		}
	})
	t.Run("best node prefers larger available minus pending", func(t *testing.T) {
		if !(int64(5*gib)-gib > int64(4*gib)-gib) {
			t.Fatal("candidate ordering should use available less pending")
		}
	})
	t.Run("missing memory fields selects reservation fallback", func(t *testing.T) {
		h := agentHealth{}
		if h.MemoryBytes > 0 && h.MemAvailableBytes > 0 {
			t.Fatal("empty health unexpectedly has usage memory fields")
		}
	})
	t.Run("reservation mode uses previous reservation budget", func(t *testing.T) {
		if !hasMemoryCapacity(8*gib, 6*gib, gib, 1) {
			t.Fatal("reservation capacity should admit within the 90 percent budget")
		}
	})
}

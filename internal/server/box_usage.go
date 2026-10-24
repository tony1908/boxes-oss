package server

import (
	"context"
	"net/http"
	"time"

	"github.com/tony1908/boxes-oss/internal/store"
	"github.com/tony1908/boxes-oss/internal/types"
)

// boxCounters is container usage: CPU seconds and network bytes.
type boxCounters struct {
	CPUSeconds float64 `json:"cpuSeconds"`
	RxBytes    int64   `json:"rxBytes"`
	TxBytes    int64   `json:"txBytes"`
}

func (c boxCounters) plus(other boxCounters) boxCounters {
	return boxCounters{CPUSeconds: c.CPUSeconds + other.CPUSeconds, RxBytes: c.RxBytes + other.RxBytes, TxBytes: c.TxBytes + other.TxBytes}
}

// liveCounters asks the box's agent for its running container's counters since the container was created.
// ok is false when the box is not running or the agent cannot answer; callers must not fail on that.
func (s *Server) liveCounters(ctx context.Context, box store.BoxRecord) (boxCounters, bool) {
	if box.Box.State != types.StateReady && box.Box.State != types.StateIdle {
		return boxCounters{}, false
	}
	client, ok := newAgentClient(s.hub, box.Box.NodeID)
	if !ok {
		return boxCounters{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var stats struct {
		CPUSeconds *float64 `json:"cpuSeconds"`
		RxBytes    *int64   `json:"rxBytes"`
		TxBytes    *int64   `json:"txBytes"`
	}
	if err := client.json(ctx, http.MethodGet, agentPath("v1", "boxes", box.Box.ID, "stats"), nil, &stats); err != nil ||
		stats.CPUSeconds == nil || stats.RxBytes == nil || stats.TxBytes == nil {
		return boxCounters{}, false
	}
	return boxCounters{CPUSeconds: *stats.CPUSeconds, RxBytes: *stats.RxBytes, TxBytes: *stats.TxBytes}, true
}

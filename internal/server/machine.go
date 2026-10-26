package server

import (
	"net/http"
	"time"

	"github.com/tony1908/paddock/internal/types"
)

type machineStatus struct {
	CPUs                    int       `json:"cpus"`
	MemoryBytes             int64     `json:"memoryBytes"`
	MemAvailableBytes       int64     `json:"memAvailableBytes"`
	DiskFreeBytes           int64     `json:"diskFreeBytes"`
	SwapFreeBytes           int64     `json:"swapFreeBytes"`
	MemoryPressureSomeAvg10 float64   `json:"memoryPressureSomeAvg10"`
	Status                  string    `json:"status"`
	RunningBoxes            int       `json:"runningBoxes"`
	LastSeen                time.Time `json:"lastSeen"`
}

func (s *Server) machine(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.ListNodes(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	if len(nodes) == 0 {
		writeError(w, http.StatusServiceUnavailable, "machine_unavailable", "local machine has not connected yet")
		return
	}
	node := nodes[0].Node
	if health, ok := s.getHealth(node.ID); ok {
		node.CPUs = health.Health.CPUs
		node.MemoryBytes = health.Health.MemoryBytes
		node.MemAvailableBytes = health.Health.MemAvailableBytes
		node.DiskFreeBytes = health.Health.DiskFreeBytes
		node.SwapFreeBytes = health.Health.SwapFreeBytes
		node.MemoryPressureSomeAvg10 = health.Health.MemoryPressureSomeAvg10
	}
	boxes, err := s.store.ListBoxes(r.Context(), node.ID)
	if err != nil {
		s.apiError(w, err)
		return
	}
	running := 0
	for _, box := range boxes {
		if box.State != types.StateStopped && box.State != types.StateError && box.State != types.StateDeleting {
			running++
		}
	}
	status := node.Status
	if status != "healthy" {
		status = "unhealthy"
	}
	writeJSON(w, http.StatusOK, map[string]any{"machine": machineStatus{CPUs: node.CPUs, MemoryBytes: node.MemoryBytes,
		MemAvailableBytes: node.MemAvailableBytes, DiskFreeBytes: node.DiskFreeBytes, SwapFreeBytes: node.SwapFreeBytes,
		MemoryPressureSomeAvg10: node.MemoryPressureSomeAvg10, Status: status, RunningBoxes: running, LastSeen: node.LastSeen.UTC()}})
}

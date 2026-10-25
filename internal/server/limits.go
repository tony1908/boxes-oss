package server

import (
	"net/http"

	"github.com/tony1908/boxes-oss/internal/types"
)

func (s *Server) limits(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.ListNodes(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	boxes, err := s.store.ListBoxes(r.Context(), "")
	if err != nil {
		s.apiError(w, err)
		return
	}
	totalCPUs, totalMemory, totalDisk, readyBoxes := 0, int64(0), int64(0), 0
	items := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		boxCount := 0
		for _, box := range boxes {
			if box.NodeID == node.Node.ID && box.State != types.StateStopped && box.State != types.StateError && box.State != types.StateDeleting {
				boxCount++
			}
		}
		totalCPUs += node.Node.CPUs
		totalMemory += node.Node.MemoryBytes
		totalDisk += node.Node.DiskFreeBytes
		readyBoxes += boxCount
		items = append(items, map[string]any{"nodeId": node.Node.ID, "status": node.Node.Status,
			"cpus": node.Node.CPUs, "memoryBytes": node.Node.MemoryBytes,
			"freeMemoryBytes": node.Node.FreeMemoryBytes, "diskFreeBytes": node.Node.DiskFreeBytes,
			"boxes": boxCount})
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": items, "totals": map[string]any{
		"cpus": totalCPUs, "memoryBytes": totalMemory, "diskFreeBytes": totalDisk, "boxes": readyBoxes,
	}})
}

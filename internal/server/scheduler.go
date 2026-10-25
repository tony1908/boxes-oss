package server

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/tony1908/boxes-oss/internal/store"
	"github.com/tony1908/boxes-oss/internal/types"
)

func (s *Server) schedule(ctx context.Context, size types.Size, boxType string) (store.NodeRecord, error) {
	return s.scheduleExcluding(ctx, size, boxType, "")
}

func (s *Server) scheduleExcluding(ctx context.Context, size types.Size, boxType, excludedBoxID string) (store.NodeRecord, error) {
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return store.NodeRecord{}, err
	}
	var localNode store.NodeRecord
	var eligible bool
	var largestFree float64
	var swapLimited bool
	for index, node := range nodes {
		if index != 0 {
			continue
		}
		if node.Node.Status != "healthy" || node.Node.CPUs < size.CPUs {
			continue
		}
		if _, connected := s.hub.Client(node.Node.ID); !connected {
			continue
		}
		allocated, err := s.allocatedMemoryExcept(ctx, node.Node.ID, excludedBoxID)
		if err != nil {
			return store.NodeRecord{}, err
		}
		var remaining float64
		snap, hasHealth := s.getHealth(node.Node.ID)
		usageHealth := hasHealth && snap.Health.MemoryBytes > 0 && snap.Health.MemAvailableBytes > 0
		if s.cfg.AdmissionMode == "reservation" || !usageHealth {
			remaining = memoryAdmissionBudget(node.Node.MemoryBytes, s.cfg.MemoryOvercommit) - float64(allocated)
			if remaining > largestFree {
				largestFree = remaining
			}
			if !hasMemoryCapacity(node.Node.MemoryBytes, allocated, size.MemoryReservationBytes*4, s.cfg.MemoryOvercommit) {
				continue
			}
		} else {
			ok := true
			if ok && s.now().Sub(snap.At) > 45*time.Second {
				ok = false
			}
			if ok {
				pending := pendingBytes(ctx, s, node.Node.ID, snap.At)
				rawRemaining := float64(snap.Health.MemAvailableBytes - pending - expectedBox(s, size))
				remaining = float64(effectiveAvailable(snap.Health.MemAvailableBytes, pending, snap.Health.SwapTotalBytes-snap.Health.SwapFreeBytes, s.cfg.AdmissionSwapAllowance) - expectedBox(s, size))
				if remaining > largestFree {
					largestFree = remaining
				}
				buffer := int64(math.Max(float64(s.cfg.AdmissionBufferMin), float64(snap.Health.MemoryBytes)*s.cfg.AdmissionBufferPercent))
				boxes, _ := s.store.ListBoxes(ctx, node.Node.ID)
				var floor int64
				for _, b := range boxes {
					if b.State != types.StateStopped {
						z, e := types.SizeFor(b.Type)
						if e == nil {
							floor += z.MemoryReservationBytes
						}
					}
				}
				ok = remaining >= float64(buffer) && snap.Health.MemoryPressureSomeAvg10 < s.cfg.AdmissionPSIMax && floor+size.MemoryReservationBytes <= int64(float64(snap.Health.MemoryBytes)*.95)
				if rawRemaining >= float64(buffer) && remaining < float64(buffer) && snap.Health.MemoryPressureSomeAvg10 < s.cfg.AdmissionPSIMax && floor+size.MemoryReservationBytes <= int64(float64(snap.Health.MemoryBytes)*.95) {
					swapLimited = true
				}
			}
			if !ok {
				continue
			}
		}
		localNode = node
		eligible = true
	}
	if !eligible {
		return store.NodeRecord{}, &capacityError{boxType: boxType, neededBytes: size.MemoryReservationBytes, largestFreeBytes: largestFree, swapLimited: swapLimited}
	}
	return localNode, nil
}

type capacityError struct {
	boxType          string
	neededBytes      int64
	largestFreeBytes float64
	swapLimited      bool
}

func (e *capacityError) Error() string {
	message := fmt.Sprintf("local machine has no room for a %s box (needs %s GiB, largest free %s GiB); check memory buffer, pressure, or stale health",
		e.boxType, formatGiB(float64(e.neededBytes)), formatGiB(e.largestFreeBytes))
	if e.swapLimited {
		message += "; swap use is limiting capacity"
	}
	return message
}

func effectiveAvailable(memAvailable, pending, swapUsed, swapAllowance int64) int64 {
	excessSwap := swapUsed - swapAllowance
	if excessSwap < 0 {
		excessSwap = 0
	}
	return memAvailable - pending - excessSwap
}

func formatGiB(bytes float64) string {
	if bytes < 0 {
		bytes = 0
	}
	return strconv.FormatFloat(math.Round(bytes/(1<<30)*10)/10, 'f', -1, 64)
}

func (s *Server) allocatedMemory(ctx context.Context, nodeID string) (int64, error) {
	return s.allocatedMemoryExcept(ctx, nodeID, "")
}

func (s *Server) allocatedMemoryExcept(ctx context.Context, nodeID, excludedBoxID string) (int64, error) {
	boxes, err := s.store.ListBoxes(ctx, nodeID)
	if err != nil {
		return 0, err
	}
	var allocated int64
	for _, box := range boxes {
		if box.State == types.StateStopped || box.ID == excludedBoxID {
			continue
		}
		size, err := types.SizeFor(box.Type)
		if err != nil {
			return 0, err
		}
		reservation := size.MemoryReservationBytes
		if s.cfg.AdmissionMode == "reservation" {
			reservation *= 4
		}
		allocated += reservation
	}
	return allocated, nil
}

func hasMemoryCapacity(memoryBytes, allocatedBytes, requestedBytes int64, overcommit float64) bool {
	if memoryBytes <= 0 || allocatedBytes < 0 || requestedBytes <= 0 || overcommit <= 0 {
		return false
	}
	return float64(allocatedBytes)+float64(requestedBytes) <= memoryAdmissionBudget(memoryBytes, overcommit)
}

func memoryAdmissionBudget(memoryBytes int64, overcommit float64) float64 {
	return float64(memoryBytes) * 0.9 * overcommit
}

type healthSnapshot struct {
	Health agentHealth
	At     time.Time
}

func (s *Server) getHealth(id string) (healthSnapshot, bool) {
	s.healthMu.RLock()
	defer s.healthMu.RUnlock()
	v, ok := s.healthSnapshots[id]
	return v, ok
}
func expectedBox(s *Server, z types.Size) int64 {
	v := z.MemoryReservationBytes
	if v < s.cfg.AdmissionExpectedBox {
		v = s.cfg.AdmissionExpectedBox
	}
	return v
}
func pendingBytes(ctx context.Context, s *Server, nodeID string, now time.Time) int64 {
	boxes, _ := s.store.ListBoxes(ctx, nodeID)
	var n int64
	for _, b := range boxes {
		if b.State == types.StateProvisioning || b.State == types.StateResuming || (b.State == types.StateReady && now.Sub(b.CreatedAt) < 60*time.Second) {
			z, e := types.SizeFor(b.Type)
			if e == nil {
				n += expectedBox(s, z)
			}
		}
	}
	return n
}

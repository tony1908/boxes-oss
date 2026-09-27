package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
)

type idleCPUSample struct {
	At      time.Time
	Percent float64
}

// RunReaper periodically stops boxes whose TTL has elapsed.
func (s *Server) RunReaper(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.reapExpired(ctx, now.UTC())
		}
	}
}

func (s *Server) reapExpired(ctx context.Context, now time.Time) {
	boxes, err := s.store.ListBoxes(ctx, "")
	if err != nil {
		return
	}
	for _, box := range boxes {
		if box.Box.State == types.StateStopped || box.Box.State == types.StateStopping || box.Box.State == types.StateDeleting {
			continue
		}
		ttlDue := box.Box.TTLSeconds > 0 && box.Box.ExpiresAt != nil && !box.Box.ExpiresAt.After(now)
		idleDue := false
		if !ttlDue && box.Box.IdleStopSeconds > 0 {
			idleDue = s.boxIdleDue(ctx, box, now)
		}
		if !ttlDue && !idleDue {
			continue
		}
		client, ok := newAgentClient(s.hub, box.Box.NodeID)
		if !ok {
			continue
		}
		if !s.beginSnapshot(box.Box.ID) {
			continue
		}
		var response struct {
			Snapshot struct {
				SnapshotID string    `json:"snapshotId"`
				SizeBytes  int64     `json:"sizeBytes"`
				CreatedAt  time.Time `json:"createdAt"`
			} `json:"snapshot"`
		}
		if err := client.json(ctx, http.MethodPost, agentPath("v1", "boxes", box.Box.ID, "stop"), map[string]bool{"snapshot": true}, &response); err != nil {
			actor := "auto-stop timer"
			if idleDue {
				actor = "idle auto-stop"
			}
			s.recordSystemActivity(ctx, box.Box.ID, actor, "lifecycle", "error", map[string]any{"reason": err.Error()}, "error")
			s.endSnapshot(box.Box.ID)
			continue
		}
		if response.Snapshot.SnapshotID == "" {
			actor := "auto-stop timer"
			if idleDue {
				actor = "idle auto-stop"
			}
			s.recordSystemActivity(ctx, box.Box.ID, actor, "lifecycle", "error", map[string]any{"reason": "agent returned no stop snapshot"}, "error")
			s.endSnapshot(box.Box.ID)
			continue
		}
		createdAt := response.Snapshot.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		if _, err := s.recordSnapshot(ctx, box.Box.ID, response.Snapshot.SnapshotID, response.Snapshot.SizeBytes, createdAt, s.storageConfig()); err != nil {
			actor := "auto-stop timer"
			if idleDue {
				actor = "idle auto-stop"
			}
			s.recordSystemActivity(ctx, box.Box.ID, actor, "lifecycle", "error", map[string]any{"reason": err.Error()}, "error")
			s.endSnapshot(box.Box.ID)
			continue
		}
		actor := "auto-stop timer"
		if idleDue {
			actor = "idle auto-stop"
		}
		s.recordSystemActivity(ctx, box.Box.ID, actor, "snapshot", "stop capture", map[string]any{"sizeBytes": response.Snapshot.SizeBytes}, "ok")
		box.Box.State = types.StateStopped
		box.Box.StoppedAt = &now
		closeRunningInterval(&box, now)
		if err := s.store.UpdateBox(ctx, box); err == nil {
			s.clearIdleCPUSamples(box.Box.ID)
			_ = s.store.CloseRunningInterval(ctx, box.Box.ID, now)
			if idleDue {
				s.recordSystemActivity(ctx, box.Box.ID, actor, "lifecycle", fmt.Sprintf("Stopped after %d min idle", box.Box.IdleStopSeconds/60), nil, "ok")
			} else {
				s.recordSystemActivity(ctx, box.Box.ID, actor, "lifecycle", "auto-stopped by timer", nil, "ok")
			}
			s.stopEventPump(box.Box.ID)
			s.emitWebhookEvent(context.Background(), "sandbox.stopped", map[string]any{"sandbox": box.Box})
			s.queueRetentionDelete(ctx, box)
		}
		s.endSnapshot(box.Box.ID)
	}
}

func (s *Server) boxIdleDue(ctx context.Context, box store.BoxRecord, now time.Time) bool {
	last, err := s.store.BoxIdleSince(ctx, box.Box.ID)
	if err != nil {
		return false
	}
	if last.IsZero() {
		last = box.Box.CreatedAt
	}
	if box.RunningSince != nil && box.RunningSince.After(last) {
		last = *box.RunningSince
	}
	if !idleStopEligible(now, last, box.RunningSince, time.Duration(box.Box.IdleStopSeconds)*time.Second, false, false, 0) {
		return false
	}
	running, err := s.store.BoxHasRunningAgentRun(ctx, box.Box.ID)
	if err != nil || running {
		return false
	}
	proxyOpen, err := s.store.BoxHasOpenProxySession(ctx, box.Box.ID)
	if err != nil || proxyOpen {
		return false
	}
	client, ok := newAgentClient(s.hub, box.Box.NodeID)
	if !ok {
		return false
	}
	var stats struct {
		CPUPercent         float64 `json:"cpuPercent"`
		DesktopConnections int     `json:"desktopConnections"`
	}
	if err := client.json(ctx, http.MethodGet, agentPath("v1", "boxes", box.Box.ID, "stats"), nil, &stats); err != nil {
		return false
	}
	if stats.DesktopConnections > 0 {
		s.clearIdleCPUSamples(box.Box.ID)
		return false
	}
	average, ready := s.addIdleCPUSample(box.Box.ID, now, stats.CPUPercent)
	return ready && idleStopEligible(now, last, box.RunningSince, time.Duration(box.Box.IdleStopSeconds)*time.Second, running, proxyOpen, average)
}

func (s *Server) addIdleCPUSample(boxID string, at time.Time, percent float64) (float64, bool) {
	s.idleCPUMu.Lock()
	defer s.idleCPUMu.Unlock()
	samples := append(s.idleCPUSamples[boxID], idleCPUSample{At: at, Percent: percent})
	cutoff := at.Add(-time.Minute)
	kept := samples[:0]
	var total float64
	for _, sample := range samples {
		if !sample.At.Before(cutoff) {
			kept = append(kept, sample)
			total += sample.Percent
		}
	}
	s.idleCPUSamples[boxID] = kept
	if len(kept) < 4 || at.Sub(kept[0].At) < 45*time.Second {
		return 0, false
	}
	return total / float64(len(kept)), true
}

func (s *Server) clearIdleCPUSamples(boxID string) {
	s.idleCPUMu.Lock()
	delete(s.idleCPUSamples, boxID)
	s.idleCPUMu.Unlock()
}

func idleStopEligible(now, last time.Time, runningSince *time.Time, timeout time.Duration, activeRun, openProxy bool, cpuPercent float64) bool {
	if timeout <= 0 {
		return false
	}
	if runningSince != nil && runningSince.After(last) {
		last = *runningSince
	}
	return !last.IsZero() && now.Sub(last) >= timeout && !activeRun && !openProxy && cpuPercent <= 10
}

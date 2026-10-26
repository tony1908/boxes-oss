package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tony1908/paddock/internal/types"
)

const snapshotsPerBox = 5 // default retention when a box has no keepSnapshots
const maxKeepSnapshots = 50
const snapshotWaitTimeout = 10 * time.Minute
const snapshotPruneBackoff = time.Hour
const snapshotPruneInterval = 6 * time.Hour

// RunSnapshotLoop periodically captures every ready or idle box on connected nodes.
func (s *Server) RunSnapshotLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.SnapshotInterval)
	defer ticker.Stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.captureReadySnapshots(ctx, &workers)
		}
	}
}

func (s *Server) captureReadySnapshots(ctx context.Context, workers *sync.WaitGroup) {
	boxes, err := s.store.ListBoxes(ctx, "")
	if err != nil {
		log.Printf("list boxes for periodic snapshots: %v", err)
		return
	}
	for _, box := range boxes {
		if box.State != types.StateReady && box.State != types.StateIdle {
			continue
		}
		s.snapshotMu.Lock()
		nextAt := s.snapshotNextAt[box.ID]
		s.snapshotMu.Unlock()
		if !nextAt.IsZero() && time.Now().Before(nextAt) {
			continue
		}
		client, ok := newAgentClient(s.hub, box.NodeID)
		if !ok || !s.beginSnapshot(box.ID) {
			continue
		}
		workers.Add(1)
		go func(boxID string, client *agentClient) {
			defer workers.Done()
			defer s.endSnapshot(boxID)
			skipped, err := s.recordPeriodicSnapshotResult(ctx, boxID, client)
			s.snapshotMu.Lock()
			if skipped {
				delete(s.snapshotChanged, boxID)
				delete(s.snapshotNextAt, boxID)
			} else if err == nil {
				s.snapshotChanged[boxID]++
				if s.snapshotChanged[boxID] >= 3 {
					s.snapshotNextAt[boxID] = time.Now().Add(s.cfg.SnapshotBusyInterval)
				}
			}
			s.snapshotMu.Unlock()
			if err != nil && ctx.Err() == nil {
				log.Printf("periodic snapshot for %s: %v", boxID, err)
			}
		}(box.ID, client)
	}
}

func (s *Server) recordPeriodicSnapshot(ctx context.Context, boxID string, client *agentClient) error {
	_, err := s.recordPeriodicSnapshotResult(ctx, boxID, client)
	return err
}

func (s *Server) recordPeriodicSnapshotResult(ctx context.Context, boxID string, client *agentClient) (bool, error) {
	if box, err := s.store.GetBox(ctx, boxID); err != nil || (box.State != types.StateReady && box.State != types.StateIdle) {
		return false, nil
	}
	var captured struct {
		SnapshotID string    `json:"snapshotId"`
		SizeBytes  int64     `json:"sizeBytes"`
		CreatedAt  time.Time `json:"createdAt"`
		Skipped    bool      `json:"skipped"`
	}
	body := map[string]bool{"ifChanged": true}
	if err := client.json(ctx, http.MethodPost, agentPath("v1", "boxes", boxID, "snapshot"), body, &captured); err != nil {
		return false, err
	}
	if captured.Skipped {
		return true, nil
	}
	if strings.TrimSpace(captured.SnapshotID) == "" {
		return false, errors.New("agent returned no snapshot identifier")
	}
	if captured.CreatedAt.IsZero() {
		captured.CreatedAt = time.Now().UTC()
	}
	if _, err := s.recordSnapshot(ctx, boxID, captured.SnapshotID, captured.SizeBytes, captured.CreatedAt, s.storageConfig()); err != nil {
		return false, err
	}
	if err := s.store.RecordPeriodicSnapshot(ctx, boxID, captured.CreatedAt); err != nil {
		return false, err
	}
	if err := s.pruneSnapshots(ctx, boxID, client); err != nil {
		log.Printf("prune snapshots for %s (capture recorded; will retry next tick): %v", boxID, err)
	}
	return false, nil
}

// keepSnapshotsFor returns how many snapshots a box retains; 0 means the default.
func keepSnapshotsFor(box types.Box) int {
	if box.KeepSnapshots > 0 {
		return box.KeepSnapshots
	}
	return snapshotsPerBox
}

// validKeepSnapshots accepts 0 (default) or 1 through maxKeepSnapshots.
func validKeepSnapshots(n int) bool {
	return n >= 0 && n <= maxKeepSnapshots
}

func (s *Server) pruneSnapshots(ctx context.Context, boxID string, client *agentClient) error {
	box, err := s.store.GetBox(ctx, boxID)
	if err != nil {
		return err
	}
	keep := keepSnapshotsFor(box.Box)
	snapshots, err := s.store.ListSnapshots(ctx, boxID)
	if err != nil {
		return err
	}
	if len(snapshots) <= keep {
		return nil
	}
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()

	// Serialize pruning with named-snapshot creation so a pinned restic snapshot
	// cannot be forgotten after its reference is checked.
	s.snapshotRefs.Lock()
	defer s.snapshotRefs.Unlock()
	named, err := s.store.ListNamedSnapshots(ctx)
	if err != nil {
		return err
	}
	pinned := make(map[string]struct{}, len(named))
	for _, snapshot := range named {
		pinned[snapshot.SnapshotID] = struct{}{}
	}
	type expiredGroup struct {
		snapshots []types.Snapshot
		headers   http.Header
	}
	groups := make(map[string]*expiredGroup)
	cutoff := len(snapshots) - keep
	for _, snapshot := range snapshots[:cutoff] {
		if snapshot.ID == snapshots[len(snapshots)-1].ID {
			continue
		}
		if _, isPinned := pinned[snapshot.ID]; isPinned {
			continue
		}
		repository := snapshot.Repository
		if repository == "" {
			repository = s.storageConfig().Repository
		}
		if retryAt := s.pruneRetryAt[repository]; time.Now().Before(retryAt) {
			continue
		}
		storageHeaders, err := s.snapshotStorageHeader(snapshot)
		if err != nil {
			s.backoffSnapshotPrune(repository, err)
			continue
		}
		group := groups[repository]
		if group == nil {
			group = &expiredGroup{headers: storageHeaders}
			groups[repository] = group
		}
		group.snapshots = append(group.snapshots, snapshot)
	}
	for repository, group := range groups {
		ids := make([]string, 0, len(group.snapshots))
		for _, snapshot := range group.snapshots {
			ids = append(ids, snapshot.ResticID)
		}
		forgetSucceeded := true
		if err := client.jsonWithHeaders(ctx, http.MethodPost, agentPath("v1", "snapshots", "forget"), map[string][]string{"ids": ids}, nil, group.headers); err != nil {
			forgetSucceeded = false
			var agentErr *agentStatusError
			if errors.As(err, &agentErr) && (strings.Contains(strings.ToLower(agentErr.code), "repository_not_found") || strings.Contains(strings.ToLower(agentErr.code), "repository_unreachable") || strings.Contains(strings.ToLower(agentErr.message), "repository not found")) {
				log.Printf("snapshot repository for %d expired snapshots is missing; removing database rows: %v", len(group.snapshots), err)
			} else {
				s.backoffSnapshotPrune(repository, err)
				continue
			}
		}
		deleteFailed := false
		for _, snapshot := range group.snapshots {
			if deleteErr := s.store.DeleteSnapshot(ctx, snapshot.ID); deleteErr != nil {
				s.backoffSnapshotPrune(repository, deleteErr)
				deleteFailed = true
				break
			}
		}
		if deleteFailed {
			continue
		}
		delete(s.pruneRetryAt, repository)
		if forgetSucceeded && time.Since(s.pruneLastAt[repository]) >= snapshotPruneInterval {
			// The throttle is intentionally in memory; a server restart resets it.
			s.pruneLastAt[repository] = time.Now()
			if err := client.jsonWithHeaders(ctx, http.MethodPost, agentPath("v1", "snapshots", "prune"), nil, nil, group.headers); err != nil {
				log.Printf("restic prune for repository failed after forgetting %d snapshots: %v", len(group.snapshots), err)
			}
		}
	}
	return nil
}

func (s *Server) backoffSnapshotPrune(repository string, err error) {
	retryAt := time.Now().Add(snapshotPruneBackoff)
	s.pruneRetryAt[repository] = retryAt
	fingerprint := sha256.Sum256([]byte(repository))
	log.Printf("prune snapshots for repository %s failed; retry after %s: %v",
		hex.EncodeToString(fingerprint[:6]), retryAt.UTC().Format(time.RFC3339), err)
}

func (s *Server) beginSnapshot(boxID string) bool {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	if _, exists := s.snapshotBusy[boxID]; exists {
		return false
	}
	s.snapshotBusy[boxID] = make(chan struct{})
	return true
}

func (s *Server) waitSnapshot(ctx context.Context, boxID string) error {
	waitCtx, cancel := context.WithTimeout(ctx, snapshotWaitTimeout)
	defer cancel()
	for {
		s.snapshotMu.Lock()
		finished, busy := s.snapshotBusy[boxID]
		if !busy {
			s.snapshotBusy[boxID] = make(chan struct{})
			s.snapshotMu.Unlock()
			return nil
		}
		s.snapshotMu.Unlock()
		select {
		case <-finished:
		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	}
}

func (s *Server) endSnapshot(boxID string) {
	s.snapshotMu.Lock()
	if finished, exists := s.snapshotBusy[boxID]; exists {
		delete(s.snapshotBusy, boxID)
		close(finished)
	}
	s.snapshotMu.Unlock()
}

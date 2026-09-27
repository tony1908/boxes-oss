package server

import (
	"context"
	"log"
	"time"
)

func (s *Server) flushAPIKeyUsageLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flushAPIKeyUsage(context.Background())
			return
		case at := <-ticker.C:
			s.flushAPIKeyUsage(ctx, at)
		}
	}
}

func (s *Server) flushAPIKeyUsage(ctx context.Context, at ...time.Time) {
	s.apiUsageMu.Lock()
	counts := s.apiUsagePending
	s.apiUsagePending = make(map[string]int64)
	s.apiUsageMu.Unlock()
	if len(counts) == 0 {
		return
	}
	now := time.Now().UTC()
	if len(at) > 0 {
		now = at[0].UTC()
	}
	if err := s.store.AddAPIKeyRequestCounts(ctx, counts, now); err != nil {
		s.apiUsageMu.Lock()
		for id, count := range counts {
			s.apiUsagePending[id] += count
		}
		s.apiUsageMu.Unlock()
		log.Printf("flush API key usage: %v", err)
	}
}

func (s *Server) apiKeyUsageFor(ctx context.Context, keyID string) (map[string]int64, error) {
	usage, err := s.store.APIKeyUsage(ctx, keyID, time.Now().UTC().Add(-29*24*time.Hour))
	if err != nil {
		return nil, err
	}
	s.apiUsageMu.Lock()
	usage["requests"] += s.apiUsagePending[keyID]
	s.apiUsageMu.Unlock()
	return usage, nil
}

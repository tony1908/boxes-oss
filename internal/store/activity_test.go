package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRunningUsageAcrossIntervalsAndMonthBoundary(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Date(2026, time.February, 1, 1, 30, 0, 0, time.UTC)
	if err := st.OpenRunningInterval(ctx, "bx_usage", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseRunningInterval(ctx, "bx_usage", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.OpenRunningInterval(ctx, "bx_usage", now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	total, month, days, err := st.RunningUsage(ctx, "bx_usage", now)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5400 || month != 3600 {
		t.Fatalf("usage total=%d month=%d, want 5400 and 3600", total, month)
	}
	if days["2026-01-31"] != 1800 || days["2026-02-01"] != 3600 {
		t.Fatalf("per-day usage = %#v, want Jan 31=1800 and Feb 1=3600", days)
	}
}

func TestActivityPaginationKeepsEqualTimeRowsAndPrunesOldRows(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"act_a", "act_b", "act_c"} {
		if err := st.RecordActivity(ctx, Activity{ID: id, BoxID: "bx_history", Time: at, ActorType: "system", ActorLabel: "system", Category: "lifecycle", Action: "created", Detail: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.ListActivityPage(ctx, "bx_history", "", nil, "", 2)
	if err != nil || len(first) != 2 {
		t.Fatalf("first page = %d rows, err %v", len(first), err)
	}
	second, err := st.ListActivityPage(ctx, "bx_history", "", &first[1].Time, first[1].ID, 2)
	if err != nil || len(second) != 1 || second[0].ID == first[0].ID || second[0].ID == first[1].ID {
		t.Fatalf("second page = %#v, err %v; want the remaining equal-time row", second, err)
	}
	if err := st.PruneActivity(ctx, at.Add(-90*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	remaining, err := st.ListActivityPage(ctx, "bx_history", "", nil, "", 10)
	if err != nil || len(remaining) != 3 {
		t.Fatalf("prune removed rows at exactly 90 days: remaining=%d err=%v", len(remaining), err)
	}
	if err := st.PruneActivity(ctx, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	remaining, err = st.ListActivityPage(ctx, "bx_history", "", nil, "", 10)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("prune retained old rows: remaining=%d err=%v", len(remaining), err)
	}
}

func TestPeriodicSnapshotActivityIsOneDailySummary(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	day := time.Date(2026, time.September, 25, 8, 0, 0, 0, time.UTC)
	for index := 0; index < 3; index++ {
		if err := st.RecordPeriodicSnapshot(ctx, "bx_periodic", day.Add(time.Duration(index)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.ListActivityPage(ctx, "bx_periodic", "snapshot", nil, "", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("periodic summary rows=%d err=%v, want one daily row", len(rows), err)
	}
	var detail map[string]int
	if err := json.Unmarshal(rows[0].Detail, &detail); err != nil || detail["count"] != 3 {
		t.Fatalf("periodic summary detail=%s err=%v, want count 3", rows[0].Detail, err)
	}
}

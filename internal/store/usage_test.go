package store

import (
	"context"
	"sync"
	"testing"
)

func TestUsageAggregatesAndWindow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ws, err := s.CreateWorkspace(ctx, "Usage")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateWorkspace(ctx, "Other")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.RecordContextUsage(ctx, ws.ID, 101, 21); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err = s.RecordContextUsage(ctx, other.ID, 4000, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `INSERT INTO workspace_usage_daily(workspace_id,day,retrievals,baseline_tokens,returned_tokens,saved_tokens) VALUES($1,(now() AT TIME ZONE 'UTC')::date-30,99,999,0,999)`, ws.ID); err != nil {
		t.Fatal(err)
	}
	report, err := s.Usage(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Daily) != 30 || report.Totals.Retrievals != 10 || report.Totals.BaselineTokens != 260 || report.Totals.ReturnedTokens != 60 || report.Totals.SavedTokens != 200 {
		t.Fatalf("bad report %+v", report)
	}
	if report.Daily[0].Retrievals != 0 || report.Daily[29].Retrievals != 10 {
		t.Fatal("missing zero-filled UTC timeline", report)
	}
	if err = s.RecordContextUsage(ctx, ws.ID, 1, 2); err == nil {
		t.Fatal("invalid counts accepted")
	}
}

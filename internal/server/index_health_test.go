package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tenapato/kasane-mcp/internal/core"
	"github.com/tenapato/kasane-mcp/internal/search"
)

type fakeVectorIndex struct {
	ensureErr error
	created   bool
	points    int
	countErr  error
}

func (f *fakeVectorIndex) Ensure(context.Context) (bool, error) {
	created := f.created
	f.created = false
	return created, f.ensureErr
}
func (f *fakeVectorIndex) Count(context.Context) (int, error) { return f.points, f.countErr }

type fakeIndexSource struct {
	live, indexed int
	countErr      error
	reindexErr    error
	reindexed     int
}

func (f *fakeIndexSource) IndexCounts(context.Context) (int, int, error) {
	return f.live, f.indexed, f.countErr
}
func (f *fakeIndexSource) Reindex(_ context.Context, ws string) error {
	if ws != "" {
		return fmt.Errorf("unexpected workspace %q", ws)
	}
	if f.reindexErr != nil {
		return f.reindexErr
	}
	f.reindexed++
	f.indexed = 0
	return nil
}

var logMu sync.Mutex

// captureLogs redirects slog for the duration of a test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	logMu.Lock()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev); logMu.Unlock() })
	return buf
}

func TestReconcileRebuildsMissingCollection(t *testing.T) {
	logs := captureLogs(t)
	ctx := context.Background()
	h := newIndexHealth()
	idx := &fakeVectorIndex{created: true}
	src := &fakeIndexSource{live: 3, indexed: 3}
	if !reconcileIndex(ctx, idx, src, h) {
		t.Fatal("worker should keep processing jobs while rebuilding")
	}
	if src.reindexed != 1 {
		t.Fatalf("expected one full reindex, got %d", src.reindexed)
	}
	if st := h.snapshot(); st.State != "degraded" || st.Reason != reasonRebuilding || !st.Qdrant || h.searchable() {
		t.Fatalf("unexpected state while rebuilding: %+v", st)
	}
	for _, want := range []string{"collection was missing", "queuing a full rebuild", "qdrant_points=0", "indexed_memories=3"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
	// Partially rebuilt: still degraded and no duplicate reindex.
	idx.points, src.indexed = 2, 2
	reconcileIndex(ctx, idx, src, h)
	if src.reindexed != 1 || h.searchable() {
		t.Fatalf("partial rebuild: reindexed=%d searchable=%v", src.reindexed, h.searchable())
	}
	idx.points, src.indexed = 3, 3
	reconcileIndex(ctx, idx, src, h)
	if st := h.snapshot(); st.State != "ready" || !h.searchable() || src.reindexed != 1 {
		t.Fatalf("rebuild should complete: %+v reindexed=%d", st, src.reindexed)
	}
	if !strings.Contains(logs.String(), "rebuild complete") {
		t.Errorf("missing completion log:\n%s", logs)
	}
}

func TestReconcileDetectsEmptyExistingCollection(t *testing.T) {
	captureLogs(t)
	h := newIndexHealth()
	src := &fakeIndexSource{live: 5, indexed: 4}
	reconcileIndex(context.Background(), &fakeVectorIndex{points: 0}, src, h)
	if src.reindexed != 1 || h.searchable() {
		t.Fatalf("empty collection must trigger rebuild: reindexed=%d", src.reindexed)
	}
}

func TestReconcileHealthyIndexDoesNothing(t *testing.T) {
	captureLogs(t)
	h := newIndexHealth()
	// Extra points (pending deletes) and unindexed new writes are normal.
	src := &fakeIndexSource{live: 10, indexed: 10}
	for i := 0; i < 3; i++ {
		if !reconcileIndex(context.Background(), &fakeVectorIndex{points: 12}, src, h) {
			t.Fatal("healthy index should be ready")
		}
	}
	if src.reindexed != 0 || !h.searchable() {
		t.Fatalf("healthy index must not be rebuilt: reindexed=%d", src.reindexed)
	}
	// An empty install has nothing to rebuild.
	empty := &fakeIndexSource{}
	reconcileIndex(context.Background(), &fakeVectorIndex{created: true}, empty, newIndexHealth())
	if empty.reindexed != 0 {
		t.Fatal("empty database must not queue a rebuild")
	}
}

func TestReconcileFinishesInterruptedRebuildAfterRestart(t *testing.T) {
	captureLogs(t)
	h := newIndexHealth()
	src := &fakeIndexSource{live: 10, indexed: 4}
	reconcileIndex(context.Background(), &fakeVectorIndex{points: 6}, src, h)
	if src.reindexed != 0 || h.searchable() || h.snapshot().Reason != reasonRebuilding {
		t.Fatalf("startup with unindexed memories should wait for rebuild: %+v", h.snapshot())
	}
}

func TestReconcileReportsQdrantFailures(t *testing.T) {
	captureLogs(t)
	cases := []struct {
		idx    *fakeVectorIndex
		reason string
		qdrant bool
	}{
		{&fakeVectorIndex{ensureErr: fmt.Errorf("%w: dial tcp: connection refused", search.ErrUnreachable)}, reasonUnreachable, false},
		{&fakeVectorIndex{ensureErr: search.ErrIncompatible}, reasonIncompatible, false},
		{&fakeVectorIndex{ensureErr: &search.HTTPError{Status: 500}}, reasonQdrantError, false},
		{&fakeVectorIndex{countErr: &search.HTTPError{Status: 503}}, reasonQdrantError, false},
	}
	for _, tc := range cases {
		h := newIndexHealth()
		src := &fakeIndexSource{live: 1, indexed: 1}
		if reconcileIndex(context.Background(), tc.idx, src, h) {
			t.Errorf("%s: worker must pause", tc.reason)
		}
		if st := h.snapshot(); st.Reason != tc.reason || st.Qdrant != tc.qdrant || st.Message == "" || h.searchable() {
			t.Errorf("unexpected state %+v, want %s", st, tc.reason)
		}
		if src.reindexed != 0 {
			t.Errorf("%s: must not reindex", tc.reason)
		}
	}
	h := newIndexHealth()
	if reconcileIndex(context.Background(), &fakeVectorIndex{}, &fakeIndexSource{countErr: errors.New("db down")}, h) || h.snapshot().Reason != reasonDatabase {
		t.Errorf("database failure should degrade: %+v", h.snapshot())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h = newIndexHealth()
	reconcileIndex(ctx, &fakeVectorIndex{ensureErr: context.Canceled}, &fakeIndexSource{}, h)
	if h.snapshot().Reason != reasonNotChecked {
		t.Error("shutdown must not record a degraded state")
	}
}

func TestDegradedWarningsAreRateLimited(t *testing.T) {
	logs := captureLogs(t)
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	h := newIndexHealth()
	h.now = func() time.Time { return now }
	warnings := func() int { return strings.Count(logs.String(), "search index degraded") }
	h.degrade(reasonUnreachable, false)
	for i := 0; i < 50; i++ { // 50 checks 5s apart ≈ 4 minutes
		now = now.Add(5 * time.Second)
		h.degrade(reasonUnreachable, false)
	}
	if warnings() != 1 {
		t.Fatalf("expected one warning within the window, got %d", warnings())
	}
	now = now.Add(degradedWarnEvery)
	h.degrade(reasonUnreachable, false)
	if warnings() != 2 || !strings.Contains(logs.String(), "degraded_for=9m10s") {
		t.Fatalf("expected periodic warning with duration:\n%s", logs)
	}
	h.degrade(reasonIncompatible, false)
	if warnings() != 3 {
		t.Fatal("reason change must warn immediately")
	}
	h.recover()
	if !strings.Contains(logs.String(), "search index healthy again") || h.snapshot().State != "ready" {
		t.Fatalf("recovery must be logged:\n%s", logs)
	}
}

type countingIndex struct{ calls int }

func (c *countingIndex) Search(context.Context, string, core.SearchInput) ([]core.Hit, error) {
	c.calls++
	return []core.Hit{}, nil
}

func TestGatedIndexFallsBackWhileDegraded(t *testing.T) {
	captureLogs(t)
	inner := &countingIndex{}
	h := newIndexHealth()
	g := gatedIndex{index: inner, health: h}
	if _, err := g.Search(context.Background(), "ws", core.SearchInput{Query: "x"}); !errors.Is(err, errIndexDegraded) || inner.calls != 0 {
		t.Fatal("unverified index must not be searched")
	}
	h.recover()
	if _, err := g.Search(context.Background(), "ws", core.SearchInput{Query: "x"}); err != nil || inner.calls != 1 {
		t.Fatal("healthy index must be searched")
	}
	if _, err := (gatedIndex{health: h}).Search(context.Background(), "ws", core.SearchInput{}); err == nil {
		t.Fatal("missing index must fall back")
	}
}

func TestHealthzReportsDegradedSearchButStaysLive(t *testing.T) {
	captureLogs(t)
	a, err := New(nil, nil, Config{PublicURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	get := func() map[string]string {
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "http://other.example/healthz", nil))
		if rec.Code != 200 {
			t.Fatalf("healthz must stay 200, got %d", rec.Code)
		}
		var out map[string]string
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	a.indexHealth.degrade(reasonUnreachable, false)
	if out := get(); out["status"] != "ok" || out["search_index"] != "degraded" || out["reason"] != reasonUnreachable {
		t.Fatalf("unexpected degraded healthz: %v", out)
	}
	a.indexHealth.recover()
	if out := get(); out["search_index"] != "ready" || out["reason"] != "" {
		t.Fatalf("unexpected healthy healthz: %v", out)
	}
}

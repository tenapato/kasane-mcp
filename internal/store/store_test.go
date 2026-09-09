package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tenapato/kasane-mcp/internal/core"
	"os"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	u := os.Getenv("KASANE_TEST_DATABASE_URL")
	if u == "" {
		t.Skip("KASANE_TEST_DATABASE_URL not set")
	}
	c := context.Background()
	a, e := pgxpool.New(c, u)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	n := fmt.Sprintf("s_%d", time.Now().UnixNano())
	if _, e = a.Exec(c, `CREATE SCHEMA `+n); e != nil {
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(u)
	if e != nil {
		t.Fatal(e)
	}
	cfg.AfterConnect = func(c context.Context, x *pgx.Conn) error { _, e := x.Exec(c, `SET search_path TO `+n); return e }
	p, e := pgxpool.NewWithConfig(c, cfg)
	if e != nil {
		t.Fatal(e)
	}
	s := &Store{DB: p}
	if e = s.Migrate(c); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { p.Close(); a.Exec(c, `DROP SCHEMA `+n+` CASCADE`) })
	return s
}
func TestConcurrentIdempotentCreates(t *testing.T) {
	s := testStore(t)
	c := context.Background()
	w, e := s.CreateWorkspace(c, "w")
	if e != nil {
		t.Fatal(e)
	}
	in := core.RememberInput{Title: "x", Content: "y", IdempotencyKey: "same"}
	var g sync.WaitGroup
	ms := make(chan core.Memory, 2)
	es := make(chan error, 2)
	for i := 0; i < 2; i++ {
		g.Add(1)
		go func() { defer g.Done(); m, e := s.Remember(c, w.ID, in); ms <- m; es <- e }()
	}
	g.Wait()
	close(ms)
	close(es)
	for e := range es {
		if e != nil {
			t.Fatal(e)
		}
	}
	var id string
	for m := range ms {
		if id == "" {
			id = m.ID
		} else if id != m.ID {
			t.Fatalf("different IDs")
		}
	}
}
func TestRevisionIsolationAndForget(t *testing.T) {
	s := testStore(t)
	c := context.Background()
	w1, _ := s.CreateWorkspace(c, "a")
	w2, _ := s.CreateWorkspace(c, "b")
	m, e := s.Remember(c, w1.ID, core.RememberInput{Title: "x", Content: "secret"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Get(c, w2.ID, m.ID); !errors.Is(e, core.ErrNotFound) {
		t.Fatal(e)
	}
	if _, e = s.Remember(c, w1.ID, core.RememberInput{ID: m.ID, ExpectedRevision: 99, Title: "z", Content: "z"}); !errors.Is(e, core.ErrConflict) {
		t.Fatal(e)
	}
	if e = s.Forget(c, w1.ID, m.ID); e != nil {
		t.Fatal(e)
	}
	r, e := s.Rehydrate(c, w1.ID, []core.Hit{{ID: m.ID, Revision: m.Revision}})
	if e != nil || len(r) != 0 {
		t.Fatalf("stale hit")
	}
}
func TestRetryAndReindex(t *testing.T) {
	s := testStore(t)
	c := context.Background()
	w, _ := s.CreateWorkspace(c, "w")
	m, _ := s.Remember(c, w.ID, core.RememberInput{Title: "x", Content: "y"})
	ok, e := s.ProcessOne(c, func(context.Context, core.Memory) error { return errors.New("failure") })
	if e != nil || !ok {
		t.Fatal(e)
	}
	var n int
	s.DB.QueryRow(c, `SELECT attempts FROM outbox WHERE memory_id=$1`, m.ID).Scan(&n)
	if n != 1 {
		t.Fatal(n)
	}
	if e = s.Reindex(c, w.ID); e != nil {
		t.Fatal(e)
	}
}

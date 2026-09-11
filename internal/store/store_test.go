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

func TestMovePreservesMemoryAndQueuesNewRevision(t *testing.T) {
	s := testStore(t)
	c := context.Background()
	w1, _ := s.CreateWorkspace(c, "source")
	w2, _ := s.CreateWorkspace(c, "target")
	m, e := s.Remember(c, w1.ID, core.RememberInput{Title: "title", Content: "content", Tags: []string{"tag"}, Source: "source", IdempotencyKey: "create-key"})
	if e != nil {
		t.Fatal(e)
	}
	got, e := s.Move(c, w1.ID, w2.ID, m.ID, 1)
	if e != nil {
		t.Fatal(e)
	}
	if got.ID != m.ID || got.WorkspaceID != w2.ID || got.Title != m.Title || got.Content != m.Content || got.Revision != 2 || got.IndexedRevision != 0 {
		t.Fatalf("move changed memory unexpectedly: %+v", got)
	}
	var idem *string
	if e = s.DB.QueryRow(c, `SELECT idempotency_key FROM memories WHERE id=$1`, m.ID).Scan(&idem); e != nil {
		t.Fatal(e)
	}
	if idem != nil {
		t.Fatalf("idempotency key was not cleared: %q", *idem)
	}
	var rev int64
	if e = s.DB.QueryRow(c, `SELECT revision FROM outbox WHERE memory_id=$1`, m.ID).Scan(&rev); e != nil || rev != 2 {
		t.Fatalf("outbox revision: %d %v", rev, e)
	}
	if _, e = s.Remember(c, w2.ID, core.RememberInput{Title: "new", Content: "memory", IdempotencyKey: "create-key"}); e != nil {
		t.Fatalf("cleared idempotency key still collides: %v", e)
	}
	if _, e = s.Move(c, w1.ID, w2.ID, m.ID, 1); !errors.Is(e, core.ErrConflict) {
		t.Fatalf("stale move error: %v", e)
	}
}

func TestCreateWorkspaceForKeyGrantsAccessWithoutChangingHome(t *testing.T) {
	s := testStore(t)
	c := context.Background()
	home, _ := s.CreateWorkspace(c, "home")
	var keyID string
	if e := s.DB.QueryRow(c, `INSERT INTO agent_keys(id,workspace_id,name,prefix,token_hash,scope) VALUES(gen_random_uuid(),$1,'key','kas_','hash-create','write') RETURNING id::text`, home.ID).Scan(&keyID); e != nil {
		t.Fatal(e)
	}
	w, e := s.CreateWorkspaceForKey(c, keyID, "created")
	if e != nil {
		t.Fatal(e)
	}
	var homeAfter, granted string
	if e = s.DB.QueryRow(c, `SELECT workspace_id::text FROM agent_keys WHERE id=$1`, keyID).Scan(&homeAfter); e != nil {
		t.Fatal(e)
	}
	if e = s.DB.QueryRow(c, `SELECT workspace_id::text FROM agent_key_workspaces WHERE key_id=$1 AND workspace_id=$2`, keyID, w.ID).Scan(&granted); e != nil {
		t.Fatal(e)
	}
	if homeAfter != home.ID || granted != w.ID {
		t.Fatalf("home/grant mismatch: %s %s", homeAfter, granted)
	}
	if _, e = s.CreateWorkspaceForKey(c, "00000000-0000-0000-0000-000000000000", "missing"); !errors.Is(e, core.ErrNotFound) {
		t.Fatalf("missing key error: %v", e)
	}
}

func TestMoveEnforcesTargetProfileLimit(t *testing.T) {
	s := testStore(t)
	c := context.Background()
	source, _ := s.CreateWorkspace(c, "source")
	target, _ := s.CreateWorkspace(c, "target")
	for i := 0; i < 100; i++ {
		if _, e := s.Remember(c, target.ID, core.RememberInput{Title: "choice", Content: "Use Go", Kind: "stack"}); e != nil {
			t.Fatal(e)
		}
	}
	m, e := s.Remember(c, source.ID, core.RememberInput{Title: "choice", Content: "Use Rust", Kind: "practice"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Move(c, source.ID, target.ID, m.ID, 1); !errors.Is(e, core.ErrInvalid) {
		t.Fatalf("profile overflow move accepted: %v", e)
	}
	if _, e = s.Get(c, source.ID, m.ID); e != nil {
		t.Fatalf("failed move changed source memory: %v", e)
	}
}

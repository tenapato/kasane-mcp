package store

import (
	"context"
	"errors"
	"github.com/tenapato/kasane-mcp/internal/core"
	"testing"
)

func TestProfileLimitAppliesToKindTransitions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, e := s.CreateWorkspace(ctx, "Profile")
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 100; i++ {
		if _, e = s.Remember(ctx, w.ID, core.RememberInput{Title: "Stack choice", Content: "Use Go", Kind: "stack"}); e != nil {
			t.Fatal(e)
		}
	}
	m, e := s.Remember(ctx, w.ID, core.RememberInput{Title: "Ordinary memory", Content: "A useful fact"})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.Remember(ctx, w.ID, core.RememberInput{ID: m.ID, ExpectedRevision: 1, Title: m.Title, Content: m.Content, Kind: "practice"})
	if !errors.Is(e, core.ErrInvalid) {
		t.Fatalf("profile overflow transition accepted: %v", e)
	}
	profile, e := s.Profile(ctx, w.ID)
	if e != nil || len(profile) != 100 {
		t.Fatalf("profile: %d %v", len(profile), e)
	}
}

func TestMigrationRerunPreservesIndexedMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, e := s.CreateWorkspace(ctx, "Migration")
	if e != nil {
		t.Fatal(e)
	}
	m, e := s.Remember(ctx, w.ID, core.RememberInput{Title: "Deploy", Content: "Use Docker"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ProcessOne(ctx, func(context.Context, core.Memory) error { return nil }); e != nil {
		t.Fatal(e)
	}
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	got, e := s.Get(ctx, w.ID, m.ID)
	if e != nil || got.Content != "Use Docker" || got.IndexedRevision != 1 {
		t.Fatalf("migration lost data: %+v %v", got, e)
	}
}

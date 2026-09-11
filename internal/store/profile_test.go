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

func TestMigrationBackfillsLegacyKeyAccessOnRepeatedRuns(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	home, e := s.CreateWorkspace(ctx, "Legacy home")
	if e != nil {
		t.Fatal(e)
	}
	var keyID string
	if e = s.DB.QueryRow(ctx, `INSERT INTO agent_keys(id,workspace_id,name,prefix,token_hash,scope) VALUES(gen_random_uuid(),$1,'legacy','kas_','legacy-token','write') RETURNING id::text`, home.ID).Scan(&keyID); e != nil {
		t.Fatal(e)
	}
	// Simulate a database created by the v1 schema, including its version
	// marker, while retaining the legacy key row.
	if _, e = s.DB.Exec(ctx, `DROP TABLE agent_key_workspaces`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `ALTER TABLE agent_keys DROP COLUMN access_mode, DROP COLUMN can_create_workspaces`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `DELETE FROM schema_version WHERE version>=2`); e != nil {
		t.Fatal(e)
	}
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	var accessMode string
	var canCreate bool
	if e = s.DB.QueryRow(ctx, `SELECT access_mode,can_create_workspaces FROM agent_keys WHERE id=$1`, keyID).Scan(&accessMode, &canCreate); e != nil {
		t.Fatal(e)
	}
	if accessMode != "single" || canCreate {
		t.Fatalf("legacy defaults: access_mode=%q can_create=%v", accessMode, canCreate)
	}
	var grants int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM agent_key_workspaces WHERE key_id=$1 AND workspace_id=$2`, keyID, home.ID).Scan(&grants); e != nil {
		t.Fatal(e)
	}
	if grants != 1 {
		t.Fatalf("home grants=%d, want one", grants)
	}
	var version int
	if e = s.DB.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); e != nil {
		t.Fatal(e)
	}
	if version != 4 {
		t.Fatalf("schema version=%d, want 4", version)
	}
}

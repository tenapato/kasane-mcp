package search

import (
	"context"
	"github.com/tenapato/kasane-mcp/internal/core"
	"os"
	"testing"
)

func TestQdrantIntegration(t *testing.T) {
	url := os.Getenv("KASANE_TEST_QDRANT_URL")
	if url == "" {
		t.Skip("set KASANE_TEST_QDRANT_URL for real Qdrant test")
	}
	q := NewQdrant(url, "")
	ctx := context.Background()
	if err := q.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	m := core.Memory{ID: core.UUID(), WorkspaceID: core.UUID(), Title: "Docker deployment", Content: "Deploy applications using Docker Compose and PostgreSQL.", Tags: []string{"docker"}, Kind: "stack", Revision: 1}
	t.Cleanup(func() { m.Deleted = true; _ = q.Apply(context.Background(), m) })
	if err := q.Apply(ctx, m); err != nil {
		t.Fatal(err)
	}
	hits, err := q.Search(ctx, m.WorkspaceID, core.SearchInput{Query: "docker", Limit: 10, Kind: "stack"})
	if err != nil || len(hits) != 1 || hits[0].ID != m.ID {
		t.Fatalf("index/search failed: %+v %v", hits, err)
	}
	other, err := q.Search(ctx, core.UUID(), core.SearchInput{Query: "docker", Limit: 10})
	if err != nil || len(other) != 0 {
		t.Fatalf("workspace leak: %+v %v", other, err)
	}
	m.Deleted = true
	if err := q.Apply(ctx, m); err != nil {
		t.Fatal(err)
	}
	hits, err = q.Search(ctx, m.WorkspaceID, core.SearchInput{Query: "docker", Limit: 10})
	if err != nil || len(hits) != 0 {
		t.Fatal("deleted vector remains")
	}
}

package search

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tenapato/kasane-mcp/internal/core"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQdrantSearchScopesWorkspaceAndRevision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		must := body["filter"].(map[string]any)["must"].([]any)
		if must[0].(map[string]any)["match"].(map[string]any)["value"] != "workspace-a" {
			t.Error("workspace filter missing")
		}
		if body["query"].(map[string]any)["model"] != "qdrant/bm25" {
			t.Error("must use non-neural BM25")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":{"points":[{"id":"id-a","score":2.5,"payload":{"revision":3}}]}}`))
	}))
	defer srv.Close()
	hits, err := NewQdrant(srv.URL, "").Search(context.Background(), "workspace-a", core.SearchInput{Query: "docker", Limit: 10})
	if err != nil || len(hits) != 1 || hits[0].Revision != 3 || hits[0].Score != 2.5 {
		t.Fatalf("bad hits: %+v %v", hits, err)
	}
}

type repoFake struct{ fallback bool }

func (r *repoFake) List(context.Context, string, core.SearchInput) (core.SearchResult, error) {
	r.fallback = true
	return core.SearchResult{Memories: []core.Memory{{ID: "lexical"}}}, nil
}
func (r *repoFake) Rehydrate(context.Context, string, []core.Hit) ([]core.Memory, error) {
	return []core.Memory{}, nil
}

type brokenIndex struct{}

func (brokenIndex) Search(context.Context, string, core.SearchInput) ([]core.Hit, error) {
	return nil, errors.New("offline")
}

func TestIndexOutageReturnsExplicitLexicalFallback(t *testing.T) {
	repo := &repoFake{}
	svc := Service{Repo: repo, Index: brokenIndex{}}
	got, err := svc.Search(context.Background(), "workspace", core.SearchInput{Query: "docker"})
	if err != nil || !got.Degraded || len(got.Memories) != 1 || got.Memories[0].ID != "lexical" {
		t.Fatalf("fallback lost: %+v %v", got, err)
	}
}

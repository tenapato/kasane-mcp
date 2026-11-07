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

func TestQdrantEnsureCreatesMissingCollectionAndCounts(t *testing.T) {
	exists := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/collections/"+Collection:
			if !exists {
				w.WriteHeader(404)
				return
			}
			w.Write([]byte(`{"result":{"config":{"params":{"sparse_vectors":{"keywords":{"modifier":"idf"}}}}}}`))
		case r.Method == "PUT" && r.URL.Path == "/collections/"+Collection:
			if exists {
				w.WriteHeader(409)
				return
			}
			exists = true
			w.Write([]byte(`{"result":true}`))
		case r.Method == "PUT" && r.URL.Path == "/collections/"+Collection+"/index":
			w.Write([]byte(`{"result":{}}`))
		case r.Method == "POST" && r.URL.Path == "/collections/"+Collection+"/points/count":
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["exact"] != true {
				t.Error("count must be exact")
			}
			w.Write([]byte(`{"result":{"count":7}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	q := NewQdrant(srv.URL, "")
	ctx := context.Background()
	if created, err := q.Ensure(ctx); err != nil || !created {
		t.Fatalf("first Ensure should create: %v %v", created, err)
	}
	if created, err := q.Ensure(ctx); err != nil || created {
		t.Fatalf("second Ensure must be idempotent: %v %v", created, err)
	}
	if n, err := q.Count(ctx); err != nil || n != 7 {
		t.Fatalf("count: %d %v", n, err)
	}
}

func TestQdrantEnsureNeverReplacesIncompatibleCollection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("incompatible collection must not be modified: %s %s", r.Method, r.URL.Path)
		}
		w.Write([]byte(`{"result":{"config":{"params":{"sparse_vectors":{"keywords":{}}}}}}`))
	}))
	defer srv.Close()
	if _, err := NewQdrant(srv.URL, "").Ensure(context.Background()); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("expected ErrIncompatible, got %v", err)
	}
}

func TestQdrantUnreachableIsClassified(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := NewQdrant(url, "").Ensure(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected ErrUnreachable, got %v", err)
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		t.Fatal("network failure is not an HTTP error")
	}
}

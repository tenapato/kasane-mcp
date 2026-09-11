package server

import (
	"context"
	"encoding/json"
	"github.com/tenapato/kasane-mcp/internal/core"
	"github.com/tenapato/kasane-mcp/internal/search"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestKnowledgeMapOnlyCurrentWorkspaceVectors(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ws, _ := s.CreateWorkspace(ctx, "Map")
	memories := []core.Memory{}
	for _, title := range []string{"Docker A", "Docker B", "Stale", "Wrong workspace"} {
		m, err := s.Remember(ctx, ws.ID, core.RememberInput{Title: title, Content: "Docker postgres"})
		if err != nil {
			t.Fatal(err)
		}
		memories = append(memories, m)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE memories SET indexed_revision=revision WHERE workspace_id=$1`, ws.ID); err != nil {
		t.Fatal(err)
	}
	var failIndex atomic.Bool
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failIndex.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		if r.URL.Path != "/collections/kasane_memories_v1/points" || r.Method != "POST" {
			t.Errorf("unexpected vector request %s %s", r.Method, r.URL.Path)
		}
		var input struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if len(input.IDs) != 4 {
			t.Errorf("unexpected ids: %v", input.IDs)
		}
		points := []map[string]any{}
		for i, m := range memories {
			workspace := ws.ID
			revision := 1
			if i == 2 {
				revision = 0
			}
			if i == 3 {
				workspace = core.UUID()
			}
			points = append(points, map[string]any{"id": m.ID, "vector": map[string]any{"keywords": search.SparseVector{Indices: []uint32{1, 2}, Values: []float64{1, 2}}}, "payload": map[string]any{"workspace_id": workspace, "revision": revision}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": points})
	}))
	defer index.Close()
	a, err := New(s, search.NewQdrant(index.URL, ""), Config{PublicURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.SetPathValue("workspace", strings.ToUpper(ws.ID))
	w := httptest.NewRecorder()
	a.knowledgeMap(w, req)
	var out mapResult
	if err = json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || out.Degraded || len(out.Nodes) != 4 || len(out.Edges) != 1 {
		t.Fatalf("bad map %d %+v", w.Code, out)
	}
	for _, n := range out.Nodes {
		if n.ID == memories[2].ID || n.ID == memories[3].ID {
			if n.Dimensions != 0 || len(n.Weights) != 0 || n.IndexingStatus != "pending" {
				t.Fatalf("stale or cross-workspace vector exposed %+v", n)
			}
		}
	}
	failIndex.Store(true)
	w = httptest.NewRecorder()
	a.knowledgeMap(w, req)
	if err = json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || !out.Degraded || len(out.Edges) != 0 || len(out.Nodes) != 4 {
		t.Fatalf("bad unavailable state %+v", out)
	}
}

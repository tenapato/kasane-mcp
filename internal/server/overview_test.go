package server

import (
	"context"
	"encoding/json"
	"github.com/tenapato/kasane-mcp/internal/core"
	"github.com/tenapato/kasane-mcp/internal/search"
	"github.com/tenapato/kasane-mcp/internal/store"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOverviewCombinesOnlyOwnWorkspaces(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, user := range []string{"alice", "bob", "empty"} {
		_, err := s.DB.Exec(ctx, `INSERT INTO owners(username,password_hash,role) VALUES($1,'unused','admin')`, user)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.DB.Exec(ctx, `INSERT INTO sessions(token_hash,username,csrf_token,expires_at) VALUES($1,$2,'csrf',now()+interval '1 day')`, hashToken(user), user)
		if err != nil {
			t.Fatal(err)
		}
	}
	memories := map[string]core.Memory{}
	for i, user := range []string{"alice", "alice", "bob"} {
		ws, err := s.CreateWorkspaceOwned(ctx, "Project", user)
		if err != nil {
			t.Fatal(err)
		}
		m, err := s.Remember(ctx, ws.ID, core.RememberInput{Title: "Docker setup", Content: "Docker postgres", Kind: []string{"memory", "stack", "practice"}[i]})
		if err != nil {
			t.Fatal(err)
		}
		memories[m.ID] = m
		if err = s.RecordContextUsage(ctx, ws.ID, int64(400*(i+1)), 40); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.Exec(ctx, `UPDATE memories SET indexed_revision=revision`); err != nil {
		t.Fatal(err)
	}
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		points := []map[string]any{}
		for _, id := range input.IDs {
			m := memories[id]
			points = append(points, map[string]any{"id": id, "payload": map[string]any{"workspace_id": m.WorkspaceID, "revision": m.Revision}, "vector": map[string]any{"keywords": search.SparseVector{Indices: []uint32{1}, Values: []float64{1}}}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": points})
	}))
	defer index.Close()
	a, err := New(s, search.NewQdrant(index.URL, ""), Config{PublicURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	h := a.Handler()
	get := func(user, path string, want int, target any) {
		t.Helper()
		r := httptest.NewRequest("GET", "http://localhost/api/v1/overview"+path, nil)
		if user != "" {
			r.AddCookie(&http.Cookie{Name: "kasane_session", Value: user})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", user, path, w.Code, w.Body.String())
		}
		if target != nil {
			if err := json.Unmarshal(w.Body.Bytes(), target); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, path := range []string{"", "/usage", "/memories", "/map"} {
		get("", path, 401, nil)
	}
	for _, tc := range []struct {
		user  string
		count int
		saved int64
	}{{"alice", 2, 280}, {"bob", 1, 290}, {"empty", 0, 0}} {
		var summary struct {
			Workspaces []core.Workspace `json:"workspaces"`
			Count      int              `json:"memory_count"`
		}
		get(tc.user, "", 200, &summary)
		if len(summary.Workspaces) != tc.count || summary.Count != tc.count {
			t.Fatalf("summary leaked: %+v", summary)
		}
		own := map[string]bool{}
		for _, ws := range summary.Workspaces {
			own[ws.ID] = true
		}
		var usage store.UsageReport
		get(tc.user, "/usage", 200, &usage)
		if usage.Totals.SavedTokens != tc.saved || usage.Totals.Retrievals != int64(tc.count) || len(usage.Daily) != 30 {
			t.Fatalf("bad usage %+v", usage)
		}
		var list core.SearchResult
		get(tc.user, "/memories", 200, &list)
		if list.Total != tc.count || len(list.Memories) != tc.count {
			t.Fatalf("bad list %+v", list)
		}
		for _, m := range list.Memories {
			if !own[m.WorkspaceID] {
				t.Fatal("foreign memory")
			}
		}
		var graph mapResult
		get(tc.user, "/map", 200, &graph)
		if graph.Total != tc.count || len(graph.Nodes) != tc.count || graph.Degraded {
			t.Fatalf("bad map %+v", graph)
		}
		for _, n := range graph.Nodes {
			if !own[n.WorkspaceID] {
				t.Fatal("foreign vector")
			}
		}
		if tc.user == "alice" && len(graph.Edges) != 1 {
			t.Fatal("missing cross-workspace connection")
		}
	}
	var list core.SearchResult
	get("alice", "/memories?kind=stack&q=Docker&limit=1", 200, &list)
	if list.Total != 1 || len(list.Memories) != 1 || list.Memories[0].Kind != "stack" {
		t.Fatalf("bad filter %+v", list)
	}
	get("alice", "/memories?limit=1&offset=1", 200, &list)
	if list.Total != 2 || len(list.Memories) != 1 {
		t.Fatalf("bad pagination %+v", list)
	}
	get("alice", "/memories?offset=-1", 400, nil)
	get("alice", "/memories?limit=oops", 400, nil)
}

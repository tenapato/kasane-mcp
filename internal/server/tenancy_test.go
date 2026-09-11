package server

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tenapato/kasane-mcp/internal/core"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUserWorkspaceIsolation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, user := range []string{"alice", "bob"} {
		role := "user"
		if user == "alice" {
			role = "admin"
		}
		if _, err := s.DB.Exec(ctx, `INSERT INTO owners(username,password_hash,role) VALUES($1,'unused',$2)`, user, role); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.Exec(ctx, `INSERT INTO sessions(token_hash,username,csrf_token,expires_at) VALUES($1,$2,'csrf',now()+interval '1 day')`, hashToken(user), user); err != nil {
			t.Fatal(err)
		}
	}
	alice, err := s.CreateWorkspaceOwned(ctx, "Same project name", "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateWorkspaceOwned(ctx, "Same project name", "bob")
	if err != nil {
		t.Fatal(err)
	}
	memory, err := s.Remember(ctx, bob.ID, core.RememberInput{Title: "Private", Content: "Bob's secret project"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(s, nil, Config{PublicURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	handler := a.Handler()
	request := func(user, method, path string, body any, want int) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "http://localhost"+path, bytes.NewReader(raw))
		r.AddCookie(&http.Cookie{Name: "kasane_session", Value: user})
		r.Header.Set("Origin", "http://localhost")
		r.Header.Set("X-CSRF-Token", "csrf")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s as %s got %d want %d: %s", method, path, user, w.Code, want, w.Body.String())
		}
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	for _, user := range []string{"alice", "bob"} {
		list := request(user, "GET", "/api/v1/workspaces", nil, 200)
		if len(list["workspaces"].([]any)) != 1 {
			t.Fatal(list)
		}
	}
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/map", nil}, {"GET", "/usage", nil}, {"GET", "/memories", nil}, {"GET", "/memories/" + memory.ID, nil}, {"DELETE", "/memories/" + memory.ID, nil},
		{"PUT", "/memories/" + memory.ID, map[string]any{"title": "hacked", "content": "hacked", "expected_revision": 1}},
		{"POST", "/memories", map[string]any{"title": "hacked", "content": "hacked"}},
		{"GET", "/keys", nil}, {"POST", "/keys", map[string]any{"name": "hacked", "scope": "write"}},
		{"DELETE", "/keys/" + core.UUID(), nil}, {"POST", "/reindex", nil},
	} {
		request("alice", tc.method, "/api/v1/workspaces/"+bob.ID+tc.path, tc.body, 404)
	}
	// Even the admin may not read another user's project data or include it in keys.
	request("alice", "POST", "/api/v1/workspaces/"+alice.ID+"/keys", map[string]any{"name": "cross-user", "scope": "write", "access_mode": "selected", "workspace_ids": []string{bob.ID}}, 400)
	status := request("alice", "GET", "/api/v1/status", nil, 200)
	if status["memory_count"] != float64(0) {
		t.Fatal(status)
	}
	request("bob", "GET", "/api/v1/admin/waitlist", nil, 403)
	request("bob", "GET", "/api/v1/admin/invitations", nil, 403)
	request("bob", "POST", "/api/v1/admin/invitations", map[string]any{"email": "x@example.com"}, 403)
	request("bob", "DELETE", "/api/v1/admin/invitations/"+core.UUID(), nil, 403)
	created := request("bob", "POST", "/api/v1/workspaces", map[string]any{"name": "New"}, 201)
	owned, err := s.WorkspaceOwned(ctx, created["id"].(string), "bob")
	if err != nil || !owned {
		t.Fatal("workspace ownership missing", err)
	}
	// MCP all access is restricted to the key owner's workspaces, including future ones.
	key := request("alice", "POST", "/api/v1/workspaces/"+alice.ID+"/keys", map[string]any{"name": "All mine", "scope": "write", "access_mode": "all", "can_create_workspaces": true}, 201)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "tenant-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token: key["token"].(string)}}, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(name string, args map[string]any, wantError bool) map[string]any {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError != wantError {
			t.Fatalf("%s: %+v %v", name, result, err)
		}
		raw, _ := json.Marshal(result.StructuredContent)
		out := map[string]any{}
		_ = json.Unmarshal(raw, &out)
		return out
	}
	listed := call("kasane_workspaces", map[string]any{}, false)
	if len(listed["workspaces"].([]any)) != 1 {
		t.Fatal(listed)
	}
	for _, name := range []string{"kasane_profile", "kasane_get", "kasane_search", "kasane_context", "kasane_forget", "kasane_remember"} {
		call(name, map[string]any{"workspace_id": bob.ID, "id": memory.ID, "title": "hack", "content": "hack", "query": "Private"}, true)
	}
	call("kasane_move", map[string]any{"workspace_id": bob.ID, "target_workspace_id": alice.ID, "id": memory.ID, "expected_revision": 1}, true)
	newWS := call("kasane_workspace_create", map[string]any{"name": "Agent project"}, false)
	owned, err = s.WorkspaceOwned(ctx, newWS["id"].(string), "alice")
	if err != nil || !owned {
		t.Fatal("agent creation owner missing", err)
	}
	call("kasane_profile", map[string]any{"workspace_id": newWS["id"]}, false)
}

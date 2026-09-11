package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tenapato/kasane-mcp/internal/core"
)

func TestMultiWorkspacePermissions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	home, _ := s.CreateWorkspace(ctx, "Home")
	other, _ := s.CreateWorkspace(ctx, "Selected")
	excluded, _ := s.CreateWorkspace(ctx, "Excluded")
	a, err := New(s, nil, Config{PublicURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	createKey := func(scope, mode string, create bool, grants []string) (*mcp.ClientSession, string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"name": "test", "scope": scope, "access_mode": mode, "can_create_workspaces": create, "workspace_ids": grants})
		req := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
		req.SetPathValue("workspace", strings.ToUpper(home.ID))
		w := httptest.NewRecorder()
		a.keys(w, req)
		if w.Code != 201 {
			t.Fatalf("create key: %d %s", w.Code, w.Body.String())
		}
		var result struct {
			Token string   `json:"token"`
			Key   agentKey `json:"key"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token: result.Token}}, DisableStandaloneSSE: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { session.Close() })
		return session, result.Key.ID
	}
	call := func(client *mcp.ClientSession, name string, args map[string]any, wantError bool) map[string]any {
		t.Helper()
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s transport: %v", name, err)
		}
		if result.IsError != wantError {
			t.Fatalf("%s expected error=%v: %+v", name, wantError, result)
		}
		raw, _ := json.Marshal(result.StructuredContent)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	selected, keyID := createKey("write", "selected", true, []string{other.ID, home.ID})
	listReq := httptest.NewRequest("GET", "/", nil)
	listReq.SetPathValue("workspace", home.ID)
	listRecorder := httptest.NewRecorder()
	a.keys(listRecorder, listReq)
	var keyList struct {
		Keys []agentKey `json:"keys"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &keyList); err != nil || listRecorder.Code != 200 || len(keyList.Keys) != 1 {
		t.Fatalf("key list: %s %v", listRecorder.Body.String(), err)
	}
	if keyList.Keys[0].AccessMode != "selected" || !keyList.Keys[0].CanCreateWorkspaces || len(keyList.Keys[0].WorkspaceIDs) != 2 {
		t.Fatalf("key metadata: %+v", keyList.Keys[0])
	}
	listed := call(selected, "kasane_workspaces", map[string]any{}, false)
	if len(listed["workspaces"].([]any)) != 2 {
		t.Fatal(listed)
	}
	call(selected, "kasane_profile", map[string]any{}, true)
	call(selected, "kasane_profile", map[string]any{"workspace_id": excluded.ID}, true)
	memory := call(selected, "kasane_remember", map[string]any{"workspace_id": home.ID, "title": "Stack", "content": "Use Go", "kind": "stack"}, false)
	id := memory["id"].(string)
	call(selected, "kasane_move", map[string]any{"workspace_id": home.ID, "target_workspace_id": excluded.ID, "id": id, "expected_revision": 1}, true)
	moved := call(selected, "kasane_move", map[string]any{"workspace_id": home.ID, "target_workspace_id": other.ID, "id": id, "expected_revision": 1}, false)
	if moved["workspace_id"] != other.ID || moved["id"] != id || moved["revision"] != float64(2) {
		t.Fatal(moved)
	}
	call(selected, "kasane_get", map[string]any{"workspace_id": home.ID, "id": id}, true)
	call(selected, "kasane_get", map[string]any{"workspace_id": other.ID, "id": id}, false)
	call(selected, "kasane_move", map[string]any{"workspace_id": other.ID, "target_workspace_id": home.ID, "id": id, "expected_revision": 1}, true)
	created := call(selected, "kasane_workspace_create", map[string]any{"name": "Created by agent"}, false)
	call(selected, "kasane_profile", map[string]any{"workspace_id": created["id"]}, false)
	read, _ := createKey("read", "all", false, nil)
	call(read, "kasane_workspace_create", map[string]any{"name": "Forbidden"}, true)
	call(read, "kasane_move", map[string]any{"workspace_id": other.ID, "target_workspace_id": home.ID, "id": id, "expected_revision": 2}, true)
	call(read, "kasane_forget", map[string]any{"workspace_id": other.ID, "id": id}, true)
	call(read, "kasane_remember", map[string]any{"workspace_id": home.ID, "title": "Denied", "content": "Denied"}, true)
	future, _ := s.CreateWorkspace(ctx, "Future")
	call(read, "kasane_profile", map[string]any{"workspace_id": future.ID}, false)
	call(selected, "kasane_profile", map[string]any{"workspace_id": future.ID}, true)
	single, _ := createKey("write", "single", false, nil)
	call(single, "kasane_profile", map[string]any{}, false)
	call(single, "kasane_profile", map[string]any{"workspace_id": other.ID}, true)
	call(single, "kasane_workspace_create", map[string]any{"name": "Denied"}, true)
	all, _ := createKey("write", "all", false, nil)
	call(all, "kasane_profile", map[string]any{"workspace_id": future.ID}, false)
	call(all, "kasane_workspace_create", map[string]any{"name": "Denied"}, true)
	if _, err := s.DB.Exec(ctx, "UPDATE agent_keys SET revoked_at=now() WHERE id=$1", keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := selected.CallTool(ctx, &mcp.CallToolParams{Name: "kasane_help", Arguments: map[string]any{}}); err == nil {
		t.Fatal("revoked key accepted")
	}
	// Invalid scope/creation combinations cannot be minted even by the owner API.
	for _, input := range []map[string]any{
		{"scope": "read", "access_mode": "all", "can_create_workspaces": true},
		{"scope": "write", "access_mode": "single", "can_create_workspaces": true},
		{"scope": "write", "access_mode": "unknown"},
		{"scope": "write", "access_mode": "selected", "workspace_ids": []string{core.UUID()}},
	} {
		input["name"] = "invalid"
		raw, _ := json.Marshal(input)
		req := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
		req.SetPathValue("workspace", home.ID)
		w := httptest.NewRecorder()
		a.keys(w, req)
		if w.Code != 400 {
			t.Fatalf("accepted invalid key: %d %s", w.Code, w.Body.String())
		}
	}
}

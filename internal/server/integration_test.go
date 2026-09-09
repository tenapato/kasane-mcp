package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tenapato/kasane-mcp/internal/core"
	"github.com/tenapato/kasane-mcp/internal/search"
	"github.com/tenapato/kasane-mcp/internal/store"
)

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.Host = "localhost"
	copy.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(copy)
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	raw := os.Getenv("KASANE_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("set KASANE_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	db, e := pgx.Connect(ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	schema := "server_" + strings.ReplaceAll(core.UUID(), "-", "")
	if _, e = db.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, e := store.Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		s.Close()
		_, _ = db.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		db.Close(context.Background())
	})
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	return s
}

func TestOwnerAndMCPFlows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if e := Bootstrap(ctx, s, "owner", "test-password-long"); e != nil {
		t.Fatal(e)
	}
	if e := Bootstrap(ctx, s, "second", "test-password-long"); e == nil {
		t.Fatal("second owner permitted")
	}
	index := search.NewQdrant("http://127.0.0.1:1", "")
	a, e := New(s, index, Config{PublicURL: "http://localhost"})
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	var cookie *http.Cookie
	csrf := ""
	request := func(method, path string, body any, token string, withCSRF bool) (int, map[string]any) {
		t.Helper()
		data, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(data))
		r.Host = "localhost"
		r.Header.Set("Origin", "http://localhost")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if withCSRF {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		res, e := srv.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		if cs := res.Cookies(); len(cs) > 0 {
			cookie = cs[0]
		}
		raw, _ := io.ReadAll(res.Body)
		var out map[string]any
		if e = json.Unmarshal(raw, &out); e != nil {
			t.Fatalf("%s %s (%d) returned %s", method, path, res.StatusCode, raw)
		}
		return res.StatusCode, out
	}
	status, _ := request("GET", "/api/v1/workspaces", nil, "", false)
	if status != 401 {
		t.Fatal("anonymous owner access")
	}
	status, session := request("POST", "/api/v1/login", map[string]string{"username": "owner", "password": "test-password-long"}, "", false)
	if status != 200 {
		t.Fatalf("login failed %+v", session)
	}
	csrf = session["csrf_token"].(string)
	status, _ = request("POST", "/api/v1/workspaces", map[string]string{"name": "App stack"}, "", false)
	if status != 403 {
		t.Fatal("CSRF omitted but write accepted")
	}
	status, ws := request("POST", "/api/v1/workspaces", map[string]string{"name": "App stack"}, "", true)
	if status != 201 {
		t.Fatalf("workspace failed %+v", ws)
	}
	wid := ws["id"].(string)
	status, k := request("POST", "/api/v1/workspaces/"+wid+"/keys", map[string]string{"name": "Codex", "scope": "write"}, "", true)
	if status != 201 {
		t.Fatal(k)
	}
	token := k["token"].(string)
	client := mcp.NewClient(&mcp.Implementation{Name: "kasane-integration-test", Version: "1"}, nil)
	mcpSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token: token}}, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("SDK initialization failed: %v", err)
	}
	defer mcpSession.Close()
	tools, err := mcpSession.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 6 {
		t.Fatalf("SDK tool discovery failed: %+v %v", tools, err)
	}
	profileResult, err := mcpSession.CallTool(ctx, &mcp.CallToolParams{Name: "kasane_profile", Arguments: map[string]any{}})
	if err != nil || profileResult.IsError {
		t.Fatalf("SDK profile call failed: %+v %v", profileResult, err)
	}
	keyID := k["key"].(map[string]any)["id"].(string)
	call := func(name string, args any, token string) map[string]any {
		t.Helper()
		status, out := request("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}, token, false)
		if status != 200 {
			t.Fatalf("MCP HTTP %d: %+v", status, out)
		}
		if out["error"] != nil {
			t.Fatalf("MCP protocol error: %+v", out)
		}
		return out["result"].(map[string]any)
	}
	remembered := call("kasane_remember", map[string]any{"title": "Go services", "content": "We build APIs in Go and deploy with Docker.", "kind": "stack"}, token)
	if remembered["isError"] == true {
		t.Fatalf("remember failed %+v", remembered)
	}
	m := remembered["structuredContent"].(map[string]any)
	mid := m["id"].(string)
	profile := call("kasane_profile", map[string]any{}, token)
	if !strings.Contains(fmt.Sprint(profile), "We build APIs in Go") {
		t.Fatalf("profile missing stack: %+v", profile)
	}
	result := call("kasane_search", map[string]any{"query": "Docker"}, token)
	if !strings.Contains(fmt.Sprint(result), "degraded:true") {
		t.Fatalf("outage not marked: %+v", result)
	}
	status, other := request("POST", "/api/v1/workspaces", map[string]string{"name": "Other"}, "", true)
	if status != 201 {
		t.Fatal(other)
	}
	_, otherKey := request("POST", "/api/v1/workspaces/"+other["id"].(string)+"/keys", map[string]string{"name": "Other key", "scope": "write"}, "", true)
	leaked := call("kasane_get", map[string]any{"id": mid}, otherKey["token"].(string))
	if leaked["isError"] != true {
		t.Fatalf("workspace leak: %+v", leaked)
	}
	_, readKey := request("POST", "/api/v1/workspaces/"+wid+"/keys", map[string]string{"name": "Read", "scope": "read"}, "", true)
	denied := call("kasane_forget", map[string]any{"id": mid}, readKey["token"].(string))
	if denied["isError"] != true {
		t.Fatal("read-only key deleted memory")
	}
	status, _ = request("PUT", "/api/v1/workspaces/"+wid+"/memories/"+mid, map[string]any{"title": "Go services", "content": "Updated convention", "kind": "stack", "expected_revision": 1}, "", true)
	if status != 200 {
		t.Fatal("UI edit failed")
	}
	status, _ = request("PUT", "/api/v1/workspaces/"+wid+"/memories/"+mid, map[string]any{"title": "Go services", "content": "Overwrite", "kind": "stack", "expected_revision": 1}, "", true)
	if status != 409 {
		t.Fatal("stale UI write accepted")
	}
	forgotten := call("kasane_forget", map[string]any{"id": mid}, token)
	if forgotten["isError"] == true {
		t.Fatal(forgotten)
	}
	gone := call("kasane_get", map[string]any{"id": mid}, token)
	if gone["isError"] != true {
		t.Fatal("forgotten memory returned")
	}
	status, _ = request("DELETE", "/api/v1/workspaces/"+wid+"/keys/"+keyID, nil, "", true)
	if status != 200 {
		t.Fatal("revoke failed")
	}
	status, _ = request("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, token, false)
	if status != 401 {
		t.Fatal("revoked key accepted")
	}
	status, _ = request("POST", "/api/v1/logout", map[string]any{}, "", true)
	if status != 200 {
		t.Fatal("logout failed")
	}
	status, _ = request("GET", "/api/v1/session", nil, "", false)
	if status != 401 {
		t.Fatal("session survives logout")
	}
}

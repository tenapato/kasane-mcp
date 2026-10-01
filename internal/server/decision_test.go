package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tenapato/kasane-mcp/internal/core"
	"github.com/tenapato/kasane-mcp/internal/decision"
)

func TestDecisionWorkspaceAndMCPIntegration(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	home, _ := s.CreateWorkspace(ctx, "Decision home")
	other, _ := s.CreateWorkspace(ctx, "Decision other")
	practice, e := s.Remember(ctx, home.ID, core.RememberInput{Title: "Secrets", Content: "Never log secrets", Kind: "practice"})
	if e != nil {
		t.Fatal(e)
	}
	foreign, e := s.Remember(ctx, other.ID, core.RememberInput{Title: "Private", Content: "Private other project", Kind: "practice"})
	if e != nil {
		t.Fatal(e)
	}
	if e := Bootstrap(ctx, s, "owner", "integration-password"); e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var in decision.Request
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("s", 32) {
			t.Error("token missing")
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || len(in.Rules) != 1 || in.Rules[0].Text != practice.Content {
			t.Error("wrong authorized source")
		}
		_ = json.NewEncoder(w).Encode(decision.Response{DecisionID: "decision-1", Mode: "baseline", Coverage: "complete", Decisions: []decision.Decision{{RuleID: practice.ID, Include: true, Reason: "required"}}})
	}))
	defer upstream.Close()
	a, e := New(s, nil, Config{PublicURL: "http://localhost", DecisionAPIURL: upstream.URL, DecisionAPIToken: strings.Repeat("s", 32)})
	if e != nil {
		t.Fatal(e)
	}
	// Exercise MCP authentication + workspace grants over the real streamable transport.
	raw, _ := json.Marshal(map[string]any{"name": "reader", "scope": "read"})
	req := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
	req.SetPathValue("workspace", home.ID)
	rec := httptest.NewRecorder()
	a.keys(rec, req)
	var key struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &key) != nil || rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token: key.Token}}, DisableStandaloneSSE: true}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "kasane_relevance", Arguments: map[string]any{"task": "Update logging", "rules": []any{map[string]any{"id": practice.ID, "required": true}}}})
	if e != nil || result.IsError {
		t.Fatalf("call: %+v %v", result, e)
	}
	b, _ := json.Marshal(result.StructuredContent)
	var out relevanceResult
	if json.Unmarshal(b, &out) != nil || out.DecisionID != "decision-1" || len(out.Rules) != 1 || out.Rules[0].Revision != practice.Revision {
		t.Fatalf("result: %s", b)
	}
	before := calls.Load()
	for _, args := range []map[string]any{{"task": "x", "rules": []any{map[string]any{"id": foreign.ID}}}, {"workspace_id": other.ID, "task": "x", "rules": []any{map[string]any{"id": foreign.ID}}}} {
		res, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "kasane_relevance", Arguments: args})
		if e == nil && !res.IsError {
			t.Fatal("unauthorized call succeeded")
		}
	}
	if calls.Load() != before {
		t.Fatal("unauthorized source sent upstream")
	}
	// Web route rejects unauthenticated requests before contacting the decision service.
	r := httptest.NewRequest("POST", "http://localhost/api/v1/workspaces/"+home.ID+"/decision/relevance", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 401 || calls.Load() != before {
		t.Fatalf("unauthenticated route: %d", w.Code)
	}
	// Real session cookie and CSRF protections on the browser endpoint.
	login := httptest.NewRequest("POST", "http://localhost/api/v1/login", strings.NewReader(`{"username":"owner","password":"integration-password"}`))
	login.Header.Set("Origin", "http://localhost")
	lr := httptest.NewRecorder()
	a.Handler().ServeHTTP(lr, login)
	if lr.Code != 200 {
		t.Fatalf("login failed: %d", lr.Code)
	}
	var identity map[string]string
	_ = json.Unmarshal(lr.Body.Bytes(), &identity)
	payload, _ := json.Marshal(relevanceInput{Task: "logging", Rules: []ruleReference{{ID: practice.ID, Required: true}}})
	for _, csrf := range []bool{false, true} {
		req := httptest.NewRequest("POST", "http://localhost/api/v1/workspaces/"+home.ID+"/decision/relevance", bytes.NewReader(payload))
		req.AddCookie(lr.Result().Cookies()[0])
		req.Header.Set("Origin", "http://localhost")
		if csrf {
			req.Header.Set("X-CSRF-Token", identity["csrf_token"])
		}
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		want := 403
		if csrf {
			want = 200
		}
		if rec.Code != want {
			t.Fatalf("csrf=%v: %d %s", csrf, rec.Code, rec.Body.String())
		}
	}

}

// Run explicitly against the local Rust baseline container; no fixture server.
func TestDecisionLiveService(t *testing.T) {
	origin := os.Getenv("KASANE_TEST_DECISION_URL")
	if origin == "" {
		t.Skip("set KASANE_TEST_DECISION_URL")
	}
	s := testStore(t)
	ctx := context.Background()
	workspace, e := s.CreateWorkspace(ctx, "Live decision test")
	if e != nil {
		t.Fatal(e)
	}
	a, e := New(s, nil, Config{PublicURL: "http://localhost", DecisionAPIURL: origin, DecisionAPIToken: os.Getenv("KASANE_TEST_DECISION_TOKEN")})
	if e != nil {
		t.Fatal(e)
	}
	in := relevanceInput{Task: "Update logging", Paths: []string{"src/http.go"}}
	for i, text := range []string{"Always preserve required conventions", "Use prepared statements for database queries", "Logging must redact credentials", "Document invoice currencies"} {
		m, e := s.Remember(ctx, workspace.ID, core.RememberInput{Title: text, Content: text, Kind: "practice"})
		if e != nil {
			t.Fatal(e)
		}
		ref := ruleReference{ID: m.ID, Required: i == 0}
		if i == 1 {
			ref.Paths = []string{"src/**"}
		}
		in.Rules = append(in.Rules, ref)
	}
	out, e := a.relevance(ctx, workspace.ID, in)
	if e != nil {
		t.Fatal(e)
	}
	if out.Mode != "baseline" || out.Coverage != "complete" || len(out.Decisions) != 4 {
		t.Fatalf("unexpected result: %+v", out.Response)
	}
	for i, reason := range []string{"required", "scope_match", "keyword", "abstained"} {
		if out.Decisions[i].Reason != reason || out.Decisions[i].Include != (i < 3) {
			t.Fatalf("rule %d: %+v", i, out.Decisions[i])
		}
	}
}

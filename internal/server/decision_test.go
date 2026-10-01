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

func TestDecisionDiagnosticSessionRoutes(t *testing.T) {
	store := testStore(t)
	if err := Bootstrap(context.Background(), store, "diagnostic-owner", "integration-password"); err != nil {
		t.Fatal(err)
	}
	mode := "ready"
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.URL.Path == "/readyz" {
			w.WriteHeader(200)
			return
		}
		if r.URL.Path != "/v1/relevance" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if mode == "unauthorized" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte("secret service message"))
			return
		}
		var request decision.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Rules) != 1 ||
			!request.Rules[0].Required || request.Context != nil || len(request.Paths) != 0 {
			t.Errorf("diagnostic sent non-synthetic request: %+v, %v", request, err)
		}
		if mode == "malformed" {
			_, _ = w.Write([]byte(`{"unexpected":"private"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(decision.Response{DecisionID: "diagnostic-1", Mode: map[string]string{"ready": "baseline", "model": "model"}[mode],
			Coverage: "complete", Decisions: []decision.Decision{{RuleID: request.Rules[0].ID, Include: true, Reason: "required"}}})
	}))
	defer upstream.Close()
	app, err := New(store, nil, Config{PublicURL: "http://localhost", DecisionAPIURL: upstream.URL,
		DecisionAPIToken: strings.Repeat("s", 32)})
	if err != nil {
		t.Fatal(err)
	}
	handler := app.Handler()
	for _, endpoint := range []string{"/api/v1/decision/status", "/api/v1/decision/check"} {
		method := http.MethodGet
		if strings.HasSuffix(endpoint, "/check") {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, "http://localhost"+endpoint, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", endpoint, rec.Code)
		}
	}
	if upstreamCalls.Load() != 0 {
		t.Fatal("unauthenticated diagnostics contacted service")
	}
	login := httptest.NewRequest(http.MethodPost, "http://localhost/api/v1/login",
		strings.NewReader(`{"username":"diagnostic-owner","password":"integration-password"}`))
	login.Header.Set("Origin", "http://localhost")
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, login)
	if loginResponse.Code != 200 {
		t.Fatalf("login: %d %s", loginResponse.Code, loginResponse.Body.String())
	}
	var identity map[string]string
	if err := json.Unmarshal(loginResponse.Body.Bytes(), &identity); err != nil {
		t.Fatal(err)
	}
	cookie := loginResponse.Result().Cookies()[0]
	call := func(method, endpoint, origin, csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://localhost"+endpoint, nil)
		req.AddCookie(cookie)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	status := call(http.MethodGet, "/api/v1/decision/status", "", "")
	if status.Code != 200 {
		t.Fatalf("readiness: %d", status.Code)
	}
	var ready decisionDiagnostic
	if err := json.Unmarshal(status.Body.Bytes(), &ready); err != nil || ready.Status != "ready" || ready.Check != "readiness" || ready.Mode != "" || ready.DecisionID != "" || ready.CheckedAt == "" {
		t.Fatalf("bad readiness: %s, %v", status.Body.String(), err)
	}
	before := upstreamCalls.Load()
	for _, pair := range [][2]string{{"", ""}, {"http://wrong.example", identity["csrf_token"]}, {"http://localhost", "wrong"}} {
		result := call(http.MethodPost, "/api/v1/decision/check", pair[0], pair[1])
		if result.Code != 403 {
			t.Fatalf("CSRF/origin not enforced: %d", result.Code)
		}
	}
	if upstreamCalls.Load() != before {
		t.Fatal("invalid CSRF contacted service")
	}
	for _, tc := range []struct{ upstream, want string }{{"ready", "ready"}, {"model", "ready"},
		{"unauthorized", "unauthorized"}, {"malformed", "invalid_response"}} {
		mode = tc.upstream
		result := call(http.MethodPost, "/api/v1/decision/check", "http://localhost", identity["csrf_token"])
		if result.Code != 200 {
			t.Fatalf("%s: browser HTTP status %d", tc.upstream, result.Code)
		}
		var diagnostic decisionDiagnostic
		if err := json.Unmarshal(result.Body.Bytes(), &diagnostic); err != nil || diagnostic.Status != tc.want ||
			diagnostic.Check != "authenticated" || diagnostic.CheckedAt == "" || diagnostic.LatencyMS < 0 {
			t.Fatalf("%s: bad diagnostic %s, %v", tc.upstream, result.Body.String(), err)
		}
		if tc.want == "ready" && (diagnostic.Mode == "" || diagnostic.DecisionID != "diagnostic-1") {
			t.Fatalf("%s: missing success metadata", tc.upstream)
		}
		if tc.want != "ready" && (diagnostic.Mode != "" || diagnostic.DecisionID != "" || strings.Contains(result.Body.String(), "secret")) {
			t.Fatalf("%s: leaked upstream detail: %s", tc.upstream, result.Body.String())
		}
	}
	disabled, err := New(store, nil, Config{PublicURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/api/v1/decision/check", nil)
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://localhost")
	req.Header.Set("X-CSRF-Token", identity["csrf_token"])
	rec := httptest.NewRecorder()
	disabled.Handler().ServeHTTP(rec, req)
	var diagnostic decisionDiagnostic
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &diagnostic) != nil || diagnostic.Status != "disabled" {
		t.Fatalf("disabled diagnostic: %d %s", rec.Code, rec.Body.String())
	}
}

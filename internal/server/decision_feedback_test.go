package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tenapato/kasane-mcp/internal/core"
	"github.com/tenapato/kasane-mcp/internal/decision"
)

func TestEvaluationJudgementValidation(t *testing.T) {
	id := core.UUID()
	rules := []ruleReference{{ID: id}}
	for _, label := range []string{"relevant", "not_relevant", "insufficient_context"} {
		if e := validateJudgements(rules, []relevanceJudgement{{id, label, "Based on this task"}}); e != nil {
			t.Fatal(e)
		}
	}
	for _, labels := range [][]relevanceJudgement{nil, {{id, "approved", "why"}}, {{id, "relevant", " "}}, {{core.UUID(), "relevant", "why"}}, {{id, "relevant", strings.Repeat("x", 2001)}}} {
		if validateJudgements(rules, labels) == nil {
			t.Fatal("invalid labels accepted")
		}
	}
	if validateJudgements(append(rules, rules...), []relevanceJudgement{{id, "relevant", "why"}, {id, "relevant", "why"}}) == nil {
		t.Fatal("duplicates accepted")
	}
}
func TestMCPTrainingEvaluationLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	home, _ := s.CreateWorkspace(ctx, "Training project")
	other, _ := s.CreateWorkspace(ctx, "Other project")
	practice, e := s.Remember(ctx, home.ID, core.RememberInput{Title: "HTTP tests", Content: "Write tests for HTTP handlers", Kind: "practice"})
	if e != nil {
		t.Fatal(e)
	}
	foreign, _ := s.Remember(ctx, other.ID, core.RememberInput{Title: "Other practice", Content: "Do not export this other workspace", Kind: "practice"})
	if e = Bootstrap(ctx, s, "owner", "integration-password"); e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var in decision.Request
		if e := json.NewDecoder(r.Body).Decode(&in); e != nil {
			t.Error(e)
		}
		if len(in.Rules) != 1 || in.Rules[0].Text != practice.Content {
			t.Error("untrusted rule content used")
		}
		_ = json.NewEncoder(w).Encode(decision.Response{DecisionID: fmt.Sprintf("decision-%d", n), Mode: "baseline", Coverage: "complete", Decisions: []decision.Decision{{RuleID: practice.ID, Include: true, Reason: "keyword"}}})
	}))
	defer upstream.Close()
	a, e := New(s, nil, Config{PublicURL: "http://localhost", DecisionAPIURL: upstream.URL, DecisionAPIToken: strings.Repeat("s", 32), DecisionFeedbackRetentionDays: 30})
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	makeClient := func(scope string) *mcp.ClientSession {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"name": "evaluation " + scope, "scope": scope})
		r := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
		r.SetPathValue("workspace", home.ID)
		w := httptest.NewRecorder()
		a.keys(w, r)
		var key struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(w.Body.Bytes(), &key) != nil || w.Code != 201 {
			t.Fatalf("key: %s", w.Body.String())
		}
		c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		session, e := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token: key.Token}}, DisableStandaloneSSE: true}, nil)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { session.Close() })
		return session
	}
	writer, reader := makeClient("write"), makeClient("read")
	in := evaluationInput{EvaluationID: core.UUID(), SourceGroup: "http-auth-change", SourceOrigin: "authored", AgentModel: "test-agent", relevanceInput: relevanceInput{Task: "Add tests for HTTP authentication", Paths: []string{"src/http.go"}, Rules: []ruleReference{{ID: practice.ID, ExpectedRevision: practice.Revision}}}, Judgements: []relevanceJudgement{{practice.ID, "not_relevant", "Initial agent proposal for human correction"}}}
	call := func(c *mcp.ClientSession, input evaluationInput, success bool) evaluationReceipt {
		t.Helper()
		b, _ := json.Marshal(input)
		var args map[string]any
		_ = json.Unmarshal(b, &args)
		result, e := c.CallTool(ctx, &mcp.CallToolParams{Name: "kasane_relevance_evaluate", Arguments: args})
		if !success {
			if e == nil && !result.IsError {
				t.Fatal("unexpected successful tool call")
			}
			return evaluationReceipt{}
		}
		if e != nil || result.IsError {
			t.Fatalf("evaluation call: %+v %v", result, e)
		}
		b, _ = json.Marshal(result.StructuredContent)
		var out evaluationReceipt
		if e = json.Unmarshal(b, &out); e != nil {
			t.Fatal(e)
		}
		return out
	}
	call(reader, in, false)
	if calls.Load() != 0 {
		t.Fatal("read key contacted upstream")
	}
	dis := a.cfg.DecisionFeedbackRetentionDays
	a.cfg.DecisionFeedbackRetentionDays = 0
	call(writer, in, false)
	a.cfg.DecisionFeedbackRetentionDays = dis
	bad := in
	bad.WorkspaceID = other.ID
	call(writer, bad, false)
	bad = in
	bad.Rules = []ruleReference{{ID: foreign.ID, ExpectedRevision: foreign.Revision}}
	bad.Judgements = []relevanceJudgement{{foreign.ID, "relevant", "foreign"}}
	call(writer, bad, false)
	if calls.Load() != 0 {
		t.Fatal("unauthorized source contacted upstream")
	}
	bad = in
	bad.EvaluationID = core.UUID()
	bad.Rules = []ruleReference{{ID: practice.ID, ExpectedRevision: 99}}
	call(writer, bad, false)
	if calls.Load() != 0 {
		t.Fatal("stale evaluation sent upstream")
	}
	out := call(writer, in, true)
	if out.ReviewStatus != "unreviewed" || out.ReadyForExport {
		t.Fatal("agent self-approved")
	}
	row, e := s.DecisionEvaluation(ctx, home.ID, in.EvaluationID)
	if e != nil {
		t.Fatal(e)
	}
	payload, e := decodeEvaluation(row)
	if e != nil || payload.Request.Rules[0].Text != practice.Content || payload.Result.Rules[0].Revision != 1 || payload.LabelAuthor != "agent" {
		t.Fatal("missing immutable snapshot")
	}
	if _, e = evaluationArchive(row); e == nil {
		t.Fatal("unreviewed export succeeded")
	}
	// The retry returns the original snapshot, even after the saved practice changes.
	_, e = s.Remember(ctx, home.ID, core.RememberInput{ID: practice.ID, ExpectedRevision: practice.Revision, Title: practice.Title, Content: "Changed practice after evaluation", Kind: "practice"})
	if e != nil {
		t.Fatal(e)
	}
	if retry := call(writer, in, true); retry.DecisionID != out.DecisionID || calls.Load() != 1 {
		t.Fatal("retry re-evaluated")
	}
	bad = in
	bad.Task = "Different task"
	call(writer, bad, false)
	if calls.Load() != 1 {
		t.Fatal("conflicting retry evaluated")
	}
	// Review routes require a real session and CSRF, not agent authorization.
	login := httptest.NewRequest("POST", "http://localhost/api/v1/login", strings.NewReader(`{"username":"owner","password":"integration-password"}`))
	login.Header.Set("Origin", "http://localhost")
	lr := httptest.NewRecorder()
	a.Handler().ServeHTTP(lr, login)
	if lr.Code != 200 {
		t.Fatal("login failed")
	}
	var identity map[string]string
	_ = json.Unmarshal(lr.Body.Bytes(), &identity)
	route := func(method, path string, body any, auth, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "http://localhost"+path, bytes.NewReader(b))
		r.Header.Set("Origin", "http://localhost")
		if auth {
			r.AddCookie(lr.Result().Cookies()[0])
		}
		if csrf {
			r.Header.Set("X-CSRF-Token", identity["csrf_token"])
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	base := "/api/v1/workspaces/" + home.ID + "/decision/evaluations/" + in.EvaluationID
	review := evaluationReview{true, true, "authored", []relevanceJudgement{{practice.ID, "relevant", "The task explicitly adds tests for these handlers"}}}
	if w := route("POST", base+"/review", review, false, false); w.Code != 401 {
		t.Fatalf("no auth: %d", w.Code)
	}
	if w := route("POST", base+"/review", review, true, false); w.Code != 403 {
		t.Fatalf("no CSRF: %d", w.Code)
	}
	refused := review
	refused.SharedTrainingAllowed = false
	if w := route("POST", base+"/review", refused, true, true); w.Code != 400 {
		t.Fatalf("missing consent: %d", w.Code)
	}
	if w := route("POST", base+"/review", review, true, true); w.Code != 200 {
		t.Fatalf("review: %d %s", w.Code, w.Body.String())
	}
	if w := route("POST", base+"/review", review, true, true); w.Code != 409 {
		t.Fatal("review overwritten")
	}
	export := route("GET", base+"/export", nil, true, false)
	if export.Code != 200 {
		t.Fatalf("export: %d %s", export.Code, export.Body.String())
	}
	z, e := zip.NewReader(bytes.NewReader(export.Body.Bytes()), int64(export.Body.Len()))
	if e != nil {
		t.Fatal(e)
	}
	files := map[string][]byte{}
	for _, f := range z.File {
		r, _ := f.Open()
		files[f.Name], e = io.ReadAll(r)
		r.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
	var event map[string]any
	if e = json.Unmarshal(files["events.jsonl"], &event); e != nil {
		t.Fatal(e)
	}
	if event["feedback_label"] != "relevant" || event["rule_revision"] != "1" || event["permission_record_id"] != in.EvaluationID {
		t.Fatal("export did not use reviewed label and original revision")
	}
	var snap map[string]any
	_ = json.Unmarshal(files["snapshots/"+practice.ID+".json"], &snap)
	raw, _ := json.Marshal(snap)
	if !bytes.Contains(raw, []byte(practice.Content)) || bytes.Contains(raw, []byte("Changed practice")) {
		t.Fatal("export changed historical input")
	}
	// Optional test artifact consumed by the real Rust importer, not a reimplemented validator.
	if dir := os.Getenv("KANAME_FEEDBACK_TEST_EXPORT"); dir != "" {
		for name, data := range files {
			path := filepath.Join(dir, name)
			if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(path, data, 0600); e != nil {
				t.Fatal(e)
			}
		}
	}
	if w := route("POST", base+"/withdraw", nil, true, true); w.Code != 200 {
		t.Fatal("withdraw failed")
	}
	if w := route("GET", base+"/export", nil, true, false); w.Code != 404 {
		t.Fatal("withdrawn content exported")
	}
	permissions := route("GET", "/api/v1/workspaces/"+home.ID+"/decision/permissions", nil, true, false)
	var grants []feedbackPermission
	_ = json.Unmarshal(permissions.Body.Bytes(), &grants)
	if len(grants) != 1 || !grants[0].Withdrawn {
		t.Fatal("registry lost withdrawal")
	}
	var purged bool
	if e = s.DB.QueryRow(ctx, `SELECT payload IS NULL AND review IS NULL FROM decision_evaluations WHERE workspace_id=$1 AND id=$2`, home.ID, in.EvaluationID).Scan(&purged); e != nil || !purged {
		t.Fatal("withdrawal retained raw content")
	}
	// Expiry purges raw snapshots and prevents a saved permission from being renewed by retry.
	_, e = s.DB.Exec(ctx, `UPDATE decision_evaluations SET payload='{}',review='{}',review_status='reviewed',expires_at=now()-interval '1 second' WHERE workspace_id=$1 AND id=$2`, home.ID, in.EvaluationID)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.PurgeDecisionEvaluations(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DecisionEvaluation(ctx, home.ID, in.EvaluationID); e == nil {
		t.Fatal("expired evaluation still visible")
	}
	// One owner opt-in permits automatic candidate exports without per-row review.
	policyPath := "/api/v1/workspaces/" + home.ID + "/decision/training-policy"
	policy := map[string]bool{"agent_candidates_enabled": true}
	if w := route("POST", policyPath, map[string]bool{}, true, true); w.Code != 400 {
		t.Fatal("missing policy flag accepted")
	}
	if w := route("POST", policyPath, policy, true, false); w.Code != 403 {
		t.Fatal("policy bypassed CSRF")
	}
	if w := route("POST", policyPath, policy, false, false); w.Code != 401 {
		t.Fatal("policy bypassed session")
	}
	if w := route("POST", policyPath, policy, true, true); w.Code != 200 {
		t.Fatalf("policy: %s", w.Body.String())
	}
	_, e = s.Remember(ctx, home.ID, core.RememberInput{ID: practice.ID, ExpectedRevision: 2, Title: practice.Title, Content: practice.Content, Kind: "practice"})
	if e != nil {
		t.Fatal(e)
	}
	in.EvaluationID = core.UUID()
	in.Rules[0].ExpectedRevision = 3
	auto := call(writer, in, true)
	if auto.ReviewStatus != "agent_generated" || !auto.ReadyForExport {
		t.Fatal("candidate required human review")
	}
	autoBase := "/api/v1/workspaces/" + home.ID + "/decision/evaluations/" + in.EvaluationID
	export = route("GET", autoBase+"/export", nil, true, false)
	if export.Code != 200 {
		t.Fatalf("automatic export: %d %s", export.Code, export.Body.String())
	}
	if w := route("GET", "/api/v1/workspaces/"+other.ID+"/decision/evaluations/"+in.EvaluationID+"/export", nil, true, false); w.Code != 404 {
		t.Fatal("cross-workspace export")
	}
	z, e = zip.NewReader(bytes.NewReader(export.Body.Bytes()), int64(export.Body.Len()))
	if e != nil {
		t.Fatal(e)
	}
	files = map[string][]byte{}
	for _, f := range z.File {
		r, _ := f.Open()
		files[f.Name], e = io.ReadAll(r)
		r.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
	if e = json.Unmarshal(files["events.jsonl"], &event); e != nil {
		t.Fatal(e)
	}
	if event["review_status"] != "agent_generated" || event["feedback_label"] != "not_relevant" {
		t.Fatal("agent label was replaced with model prediction or human-reviewed claim")
	}
	var evidence map[string]any
	_ = json.Unmarshal(files["review.json"], &evidence)
	if evidence["human_review"] != nil || evidence["reviewed_by"] != nil {
		t.Fatal("fabricated human review")
	}
	if dir := os.Getenv("KANAME_FEEDBACK_TEST_EXPORT"); dir != "" {
		for name, data := range files {
			path := filepath.Join(dir, "agent", name)
			if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(path, data, 0600); e != nil {
				t.Fatal(e)
			}
		}
	}
	// Revocation and capture serialize on a workspace lock: disabling prevents
	// future automatic grants and withdraws existing ones atomically.
	if w := route("POST", policyPath, map[string]bool{"agent_candidates_enabled": false}, true, true); w.Code != 200 {
		t.Fatal("policy revocation failed")
	}
	if w := route("GET", autoBase+"/export", nil, true, false); w.Code != 404 {
		t.Fatal("disabled contribution still exported")
	}
	permissions = route("GET", "/api/v1/workspaces/"+home.ID+"/decision/permissions", nil, true, false)
	_ = json.Unmarshal(permissions.Body.Bytes(), &grants)
	if len(grants) != 2 {
		t.Fatal("missing permission tombstones")
	}
	for _, grant := range grants {
		if !grant.Withdrawn {
			t.Fatal("revoked permission is active")
		}
	}

}

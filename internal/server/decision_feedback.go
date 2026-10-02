package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tenapato/kasane-mcp/internal/core"
	"github.com/tenapato/kasane-mcp/internal/decision"
	"github.com/tenapato/kasane-mcp/internal/store"
)

type relevanceJudgement struct {
	RuleID    string `json:"rule_id"`
	Label     string `json:"label" jsonschema:"relevant, not_relevant, or insufficient_context; judge task relevance independently of Kaname"`
	Rationale string `json:"rationale" jsonschema:"Brief evidence for the label, without secrets or complete conversations"`
}
type evaluationInput struct {
	WorkspaceID  string `json:"workspace_id,omitempty"`
	EvaluationID string `json:"evaluation_id" jsonschema:"Caller-generated UUID; reuse unchanged for retries"`
	SourceGroup  string `json:"source_group" jsonschema:"Stable related-task/source group for dataset split deduplication"`
	SourceOrigin string `json:"source_origin" jsonschema:"real, authored, or synthetic; never describe generated examples as real"`
	AgentModel   string `json:"agent_model" jsonschema:"Model identifier used to produce these judgements"`
	relevanceInput
	Judgements []relevanceJudgement `json:"judgements" jsonschema:"One independent proposed label for every selected rule; saved with agent-generated provenance; never human-reviewed by this tool"`
}
type evaluationPayload struct {
	SourceGroup  string               `json:"source_group"`
	SourceOrigin string               `json:"source_origin"`
	AgentModel   string               `json:"agent_model"`
	LabelAuthor  string               `json:"label_author"`
	Request      decision.Request     `json:"request"`
	Result       relevanceResult      `json:"result"`
	Judgements   []relevanceJudgement `json:"judgements"`
}
type evaluationReceipt struct {
	EvaluationID   string          `json:"evaluation_id"`
	DecisionID     string          `json:"decision_id"`
	ReviewStatus   string          `json:"review_status"`
	ReadyForExport bool            `json:"ready_for_export"`
	ExpiresAt      time.Time       `json:"expires_at"`
	Result         relevanceResult `json:"result"`
}
type evaluationReview struct {
	SharedTrainingAllowed    bool                 `json:"shared_training_allowed"`
	SensitiveContentReviewed bool                 `json:"sensitive_content_reviewed"`
	SourceOrigin             string               `json:"source_origin"`
	Judgements               []relevanceJudgement `json:"judgements"`
}

func validOriginLabel(v string) bool { return v == "real" || v == "authored" || v == "synthetic" }
func validateJudgements(rules []ruleReference, labels []relevanceJudgement) error {
	if len(rules) < 1 || len(rules) > 64 || len(labels) != len(rules) {
		return fmt.Errorf("%w: provide one judgement per selected practice (1–64)", core.ErrInvalid)
	}
	ids := map[string]bool{}
	for _, r := range rules {
		if !core.ValidID(r.ID) || ids[r.ID] {
			return core.ErrInvalid
		}
		ids[r.ID] = true
	}
	for _, j := range labels {
		if !ids[j.RuleID] || (j.Label != "relevant" && j.Label != "not_relevant" && j.Label != "insufficient_context") || strings.TrimSpace(j.Rationale) == "" || len(j.Rationale) > 2000 {
			return fmt.Errorf("%w: invalid or duplicate judgement; provide a label and 1–2000 byte rationale", core.ErrInvalid)
		}
		delete(ids, j.RuleID)
	}
	return nil
}
func decodeEvaluation(v store.DecisionEvaluation) (evaluationPayload, error) {
	var p evaluationPayload
	e := json.Unmarshal(v.Payload, &p)
	return p, e
}
func evaluationResult(v store.DecisionEvaluation) (evaluationReceipt, error) {
	p, e := decodeEvaluation(v)
	if e != nil {
		return evaluationReceipt{}, e
	}
	return evaluationReceipt{v.ID, p.Result.DecisionID, v.ReviewStatus, (v.ReviewStatus == "reviewed" || v.ReviewStatus == "agent_generated") && v.ExpiresAt.After(time.Now()), v.ExpiresAt, p.Result}, nil
}
func (a *App) evaluateForReview(ctx context.Context, id identity, in evaluationInput) (evaluationReceipt, error) {
	if a.cfg.DecisionFeedbackRetentionDays == 0 {
		return evaluationReceipt{}, fmt.Errorf("%w: evaluation capture is disabled; configure DECISION_FEEDBACK_RETENTION_DAYS on the backend", core.ErrInvalid)
	}
	if !core.ValidID(in.EvaluationID) || !validOriginLabel(in.SourceOrigin) || strings.TrimSpace(in.SourceGroup) == "" || len(in.SourceGroup) > 256 || strings.TrimSpace(in.AgentModel) == "" || len(in.AgentModel) > 128 {
		return evaluationReceipt{}, core.ErrInvalid
	}
	if e := validateJudgements(in.Rules, in.Judgements); e != nil {
		return evaluationReceipt{}, e
	}
	// Bound input before JSON hashing, including calls through in-process MCP.
	if len(in.Task) > 16384 || len(in.Paths) > 64 || (in.Context != nil && len(*in.Context) > 65536) {
		return evaluationReceipt{}, core.ErrInvalid
	}
	for _, p := range in.Paths {
		if len(p) > 1024 {
			return evaluationReceipt{}, core.ErrInvalid
		}
	}
	for _, r := range in.Rules {
		if r.ExpectedRevision < 1 || len(r.Paths) > 64 {
			return evaluationReceipt{}, core.ErrInvalid
		}
		for _, p := range r.Paths {
			if len(p) > 1024 {
				return evaluationReceipt{}, core.ErrInvalid
			}
		}
	}
	in.WorkspaceID = id.Workspace
	raw, e := json.Marshal(in)
	if e != nil || len(raw) > 1<<20 {
		return evaluationReceipt{}, core.ErrInvalid
	}
	hash := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(hash[:])
	old, e := a.store.DecisionEvaluation(ctx, id.Workspace, in.EvaluationID)
	if e == nil {
		if old.RequestHash != fingerprint || old.SubmittedBy != id.KeyID {
			return evaluationReceipt{}, core.ErrConflict
		}
		return evaluationResult(old)
	}
	if !errors.Is(e, core.ErrNotFound) {
		return evaluationReceipt{}, e
	}
	result, e := a.relevance(ctx, id.Workspace, in.relevanceInput)
	if e != nil {
		return evaluationReceipt{}, e
	}
	// Bind server-resolved exact text/revisions and the actual upstream prediction.
	req := decision.Request{Task: in.Task, Paths: in.Paths, Context: in.Context, Rules: []decision.Rule{}}
	if req.Paths == nil {
		req.Paths = []string{}
	}
	for i, m := range result.Rules {
		paths := in.Rules[i].Paths
		if paths == nil {
			paths = []string{}
		}
		req.Rules = append(req.Rules, decision.Rule{ID: m.ID, Text: m.Content, Required: in.Rules[i].Required, Paths: paths})
	}
	p := evaluationPayload{in.SourceGroup, in.SourceOrigin, in.AgentModel, "agent", req, result, in.Judgements}
	payload, e := json.Marshal(p)
	if e != nil {
		return evaluationReceipt{}, e
	}
	row, e := a.store.SaveDecisionEvaluation(ctx, id.Workspace, in.EvaluationID, fingerprint, id.KeyID, payload, a.cfg.DecisionFeedbackRetentionDays)
	if e != nil {
		return evaluationReceipt{}, e
	}
	return evaluationResult(row)
}
func (a *App) addDecisionEvaluationTool(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "kasane_relevance_evaluate", Description: "Submit independent relevance labels and reasons for selected practices, run Kaname, and retain the exact inputs/revisions/predictions in this workspace's review queue. Requires a write key and enabled feedback retention. Reuse evaluation_id for retries. When the workspace owner enables automatic contributions, labels become agent-generated training candidates; otherwise they remain unreviewed. Agent labels never count as human-reviewed evaluation evidence. Does not train or change serving weights. Ordinary kasane_relevance remains stateless."}, func(ctx context.Context, r *mcp.CallToolRequest, in evaluationInput) (*mcp.CallToolResult, evaluationReceipt, error) {
		id, e := a.workspacePrincipal(ctx, in.WorkspaceID, true)
		if e != nil {
			return nil, evaluationReceipt{}, e
		}
		out, e := a.evaluateForReview(ctx, id, in)
		return nil, out, toolError(e)
	})
}
func (a *App) listDecisionEvaluations(w http.ResponseWriter, r *http.Request) {
	offset := 0
	var e error
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, e = strconv.Atoi(v)
		if e != nil {
			a.failure(w, core.ErrInvalid)
			return
		}
	}
	rows, e := a.store.DecisionEvaluations(r.Context(), r.PathValue("workspace"), offset)
	if e != nil {
		a.failure(w, e)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]any{"evaluations": rows, "limit": 10, "offset": offset, "retention_days": a.cfg.DecisionFeedbackRetentionDays})
}
func (a *App) reviewDecisionEvaluation(w http.ResponseWriter, r *http.Request) {
	var in evaluationReview
	if !decode(w, r, &in) {
		return
	}
	if !in.SharedTrainingAllowed || !in.SensitiveContentReviewed || !validOriginLabel(in.SourceOrigin) {
		fail(w, 400, "explicit shared-training permission, sensitive-content review and source origin are required")
		return
	}
	row, e := a.store.DecisionEvaluation(r.Context(), r.PathValue("workspace"), r.PathValue("evaluation"))
	if e != nil {
		a.failure(w, e)
		return
	}
	p, e := decodeEvaluation(row)
	if e != nil {
		a.failure(w, e)
		return
	}
	rules := []ruleReference{}
	for _, rule := range p.Request.Rules {
		rules = append(rules, ruleReference{ID: rule.ID})
	}
	if e = validateJudgements(rules, in.Judgements); e != nil {
		a.failure(w, e)
		return
	}
	b, e := json.Marshal(in)
	if e != nil {
		a.failure(w, e)
		return
	}
	out, e := a.store.ReviewDecisionEvaluation(r.Context(), row.WorkspaceID, row.ID, r.Context().Value(identityKey{}).(identity).Username, b)
	if e != nil {
		a.failure(w, e)
		return
	}
	respond(w, 200, map[string]any{"id": out.ID, "review_status": out.ReviewStatus, "ready_for_export": true})
}
func (a *App) withdrawDecisionEvaluation(w http.ResponseWriter, r *http.Request) {
	if e := a.store.WithdrawDecisionEvaluation(r.Context(), r.PathValue("workspace"), r.PathValue("evaluation")); e != nil {
		a.failure(w, e)
		return
	}
	respond(w, 200, okResult{true})
}

type feedbackPermission struct {
	ID             string   `json:"id"`
	WorkspaceGroup string   `json:"workspace_group"`
	ProjectGroup   string   `json:"project_group"`
	Purpose        string   `json:"purpose"`
	AllowedContent []string `json:"allowed_content"`
	GrantedAt      int64    `json:"granted_at"`
	ExpiresAt      int64    `json:"expires_at"`
	Withdrawn      bool     `json:"withdrawn"`
}

func permissionFor(row store.DecisionEvaluation) feedbackPermission {
	granted := int64(0)
	if row.PermissionGrantedAt != nil {
		granted = row.PermissionGrantedAt.Unix()
	}
	return feedbackPermission{row.ID, row.WorkspaceID, row.WorkspaceID, "shared_model_training", []string{"task", "paths", "rule_text", "code"}, granted, row.ExpiresAt.Unix(), (row.ReviewStatus != "reviewed" && row.ReviewStatus != "agent_generated") || !row.ExpiresAt.After(time.Now())}
}
func (a *App) decisionPermissions(w http.ResponseWriter, r *http.Request) {
	rows, e := a.store.DecisionEvaluationPermissions(r.Context(), r.PathValue("workspace"))
	if e != nil {
		a.failure(w, e)
		return
	}
	permissions := []feedbackPermission{}
	for _, row := range rows {
		permissions = append(permissions, permissionFor(row))
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, permissions)
}
func evaluationArchive(row store.DecisionEvaluation) ([]byte, error) {
	if (row.ReviewStatus != "reviewed" && row.ReviewStatus != "agent_generated") || row.PermissionGrantedAt == nil || !row.ExpiresAt.After(time.Now()) {
		return nil, core.ErrConflict
	}
	p, e := decodeEvaluation(row)
	if e != nil {
		return nil, e
	}
	agent := row.ReviewStatus == "agent_generated"
	review := evaluationReview{SharedTrainingAllowed: true, SourceOrigin: p.SourceOrigin, Judgements: p.Judgements}
	if !agent {
		if e = json.Unmarshal(row.Review, &review); e != nil {
			return nil, e
		}
		if !review.SharedTrainingAllowed || !review.SensitiveContentReviewed {
			return nil, core.ErrConflict
		}
	}
	labels := map[string]relevanceJudgement{}
	for _, j := range review.Judgements {
		labels[j.RuleID] = j
	}
	var buf bytes.Buffer
	archive := zip.NewWriter(&buf)
	write := func(name string, v any) error {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0600)
		f, e := archive.CreateHeader(h)
		if e != nil {
			return e
		}
		return json.NewEncoder(f).Encode(v)
	}
	var events bytes.Buffer
	version := p.Result.Mode
	if p.Result.ModelVersion != nil {
		version = *p.Result.ModelVersion
	}
	for i, d := range p.Result.Decisions {
		rule := p.Result.Rules[i]
		prediction := "not_relevant"
		if d.Include {
			prediction = "relevant"
		} else if d.Reason == "abstained" {
			prediction = "insufficient_context"
		}
		file := "snapshots/" + rule.ID + ".json"
		snapshot := map[string]any{"schema_version": 1, "decision_id": p.Result.DecisionID, "workspace_group": row.WorkspaceID, "project_group": row.WorkspaceID, "source_group": p.SourceGroup, "model_version": version, "rule_id": rule.ID, "rule_revision": strconv.FormatInt(rule.Revision, 10), "task_kind": "relevance", "prediction": prediction, "request": p.Request, "sensitive_content_reviewed": !agent, "source_origin": review.SourceOrigin, "agent_model": p.AgentModel}
		if e = write(file, snapshot); e != nil {
			return nil, e
		}
		event := map[string]any{"schema_version": 1, "event_id": row.ID + "-" + rule.ID, "decision_id": p.Result.DecisionID, "workspace_group": row.WorkspaceID, "project_group": row.WorkspaceID, "model_version": version, "rule_id": rule.ID, "rule_revision": strconv.FormatInt(rule.Revision, 10), "task_kind": "relevance", "prediction": prediction, "feedback_label": labels[rule.ID].Label, "input_snapshot_ref": file, "review_status": row.ReviewStatus, "permission_record_id": row.ID, "created_at": row.PermissionGrantedAt.Unix()}
		if e = json.NewEncoder(&events).Encode(event); e != nil {
			return nil, e
		}
	}
	h := &zip.FileHeader{Name: "events.jsonl", Method: zip.Deflate}
	h.SetMode(0600)
	f, e := archive.CreateHeader(h)
	if e != nil {
		return nil, e
	}
	if _, e = f.Write(events.Bytes()); e != nil {
		return nil, e
	}
	if e = write("permissions.json", []feedbackPermission{permissionFor(row)}); e != nil {
		return nil, e
	}
	// Keep agent proposals and human corrections as review evidence, outside the
	// strict training schema. The importer reads only events and snapshots.
	var humanReview any
	if !agent {
		humanReview = review
	}
	if e = write("review.json", map[string]any{"evaluation_id": row.ID, "label_author": "agent", "agent_model": p.AgentModel, "agent_judgements": p.Judgements, "human_review": humanReview, "reviewed_by": row.ReviewedBy, "reviewed_at": row.ReviewedAt}); e != nil {
		return nil, e
	}
	if e = archive.Close(); e != nil {
		return nil, e
	}
	return buf.Bytes(), nil
}
func (a *App) exportDecisionEvaluation(w http.ResponseWriter, r *http.Request) {
	row, e := a.store.DecisionEvaluation(r.Context(), r.PathValue("workspace"), r.PathValue("evaluation"))
	if e != nil {
		a.failure(w, e)
		return
	}
	data, e := evaluationArchive(row)
	if e != nil {
		a.failure(w, e)
		return
	}
	// Recheck state after archive construction before publishing a response.
	latest, e := a.store.DecisionEvaluation(r.Context(), row.WorkspaceID, row.ID)
	if e != nil {
		a.failure(w, e)
		return
	}
	if latest.ReviewStatus != "reviewed" && latest.ReviewStatus != "agent_generated" {
		a.failure(w, core.ErrConflict)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="kaname-evaluation.zip"`)
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func (a *App) decisionTrainingPolicy(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("workspace")
	if r.Method == http.MethodPost {
		var in struct {
			AgentCandidatesEnabled *bool `json:"agent_candidates_enabled"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.AgentCandidatesEnabled == nil {
			fail(w, 400, "agent_candidates_enabled is required")
			return
		}
		if *in.AgentCandidatesEnabled && a.cfg.DecisionFeedbackRetentionDays == 0 {
			fail(w, 400, "configure DECISION_FEEDBACK_RETENTION_DAYS before enabling contributions")
			return
		}
		if e := a.store.SetDecisionTrainingPolicy(r.Context(), ws, r.Context().Value(identityKey{}).(identity).Username, *in.AgentCandidatesEnabled); e != nil {
			a.failure(w, e)
			return
		}
	}
	enabled, e := a.store.DecisionTrainingPolicy(r.Context(), ws)
	if e != nil {
		a.failure(w, e)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]any{"agent_candidates_enabled": enabled, "retention_days": a.cfg.DecisionFeedbackRetentionDays})
}

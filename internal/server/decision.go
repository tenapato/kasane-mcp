package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tenapato/kasane-mcp/internal/core"
	"github.com/tenapato/kasane-mcp/internal/decision"
)

type ruleReference struct {
	ExpectedRevision int64    `json:"expected_revision,omitempty" jsonschema:"Required for training evaluation submissions; exact revision read by the agent"`
	ID               string   `json:"id" jsonschema:"Saved practice ID from this workspace"`
	Required         bool     `json:"required,omitempty" jsonschema:"Always include this convention for this request"`
	Paths            []string `json:"paths,omitempty" jsonschema:"Repository-relative glob scopes for this request"`
}
type relevanceInput struct {
	Task    string          `json:"task"`
	Paths   []string        `json:"paths,omitempty"`
	Context *string         `json:"context,omitempty"`
	Rules   []ruleReference `json:"rules" jsonschema:"Explicit saved practice candidates, maximum 64; this is not a search of all conventions"`
}
type relevanceResult struct {
	decision.Response
	Rules []core.Memory `json:"rules"`
}

func (a *App) relevance(ctx context.Context, workspace string, in relevanceInput) (relevanceResult, error) {
	var out relevanceResult
	if len(in.Rules) == 0 || len(in.Rules) > 64 {
		return out, fmt.Errorf("%w: select 1–64 practices", core.ErrInvalid)
	}
	request := decision.Request{Task: in.Task, Paths: in.Paths, Context: in.Context, Rules: []decision.Rule{}}
	out.Rules = []core.Memory{}
	seen := map[string]bool{}
	for _, ref := range in.Rules {
		if !core.ValidID(ref.ID) || seen[ref.ID] {
			return relevanceResult{}, fmt.Errorf("%w: invalid or duplicate practice ID", core.ErrInvalid)
		}
		seen[ref.ID] = true
		memory, e := a.store.Get(ctx, workspace, ref.ID)
		if e != nil {
			return relevanceResult{}, e
		}
		if ref.ExpectedRevision > 0 && ref.ExpectedRevision != memory.Revision {
			return relevanceResult{}, core.ErrConflict
		}
		if memory.Kind != "practice" {
			return relevanceResult{}, fmt.Errorf("%w: candidates must be saved practices", core.ErrInvalid)
		}
		request.Rules = append(request.Rules, decision.Rule{ID: memory.ID, Text: memory.Content, Required: ref.Required, Paths: ref.Paths})
		out.Rules = append(out.Rules, memory)
	}
	result, e := a.decision.Relevance(ctx, request)
	if errors.Is(e, decision.ErrInvalid) {
		return relevanceResult{}, fmt.Errorf("%w: %s", core.ErrInvalid, e)
	}
	if e != nil {
		return relevanceResult{}, e
	}
	out.Response = result
	return out, nil
}

type decisionDiagnostic struct {
	Status     string  `json:"status"`
	Check      string  `json:"check"`
	CheckedAt  string  `json:"checked_at"`
	LatencyMS  float64 `json:"latency_ms"`
	Mode       string  `json:"mode,omitempty"`
	DecisionID string  `json:"decision_id,omitempty"`
}

func (a *App) decisionGlobalStatus(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	status := a.decision.Status(r.Context())
	respond(w, 200, decisionDiagnostic{Status: status, Check: "readiness",
		CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), LatencyMS: float64(time.Since(started).Microseconds()) / 1000})
}

func (a *App) decisionCheck(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	status, mode, id := a.decision.Check(r.Context())
	respond(w, 200, decisionDiagnostic{Status: status, Check: "authenticated",
		CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), LatencyMS: float64(time.Since(started).Microseconds()) / 1000,
		Mode: mode, DecisionID: id})
}

func (a *App) decisionStatus(w http.ResponseWriter, r *http.Request) {
	respond(w, 200, map[string]string{"status": a.decision.Status(r.Context())})
}
func (a *App) decisionRelevance(w http.ResponseWriter, r *http.Request) {
	var in relevanceInput
	if !decode(w, r, &in) {
		return
	}
	out, e := a.relevance(r.Context(), r.PathValue("workspace"), in)
	if errors.Is(e, decision.ErrUnavailable) {
		fail(w, 503, decision.ErrUnavailable.Error())
		return
	}
	if e != nil {
		a.failure(w, e)
		return
	}
	respond(w, 200, out)
}
func (a *App) addDecisionTool(s *mcp.Server) {
	type input struct {
		WorkspaceID string `json:"workspace_id,omitempty"`
		relevanceInput
	}
	mcp.AddTool(s, &mcp.Tool{Name: "kasane_relevance", Description: "Evaluate explicitly selected saved practices for a task and repository-relative paths. Requires configured decision service. Returns decision mode, coverage, reasons and exact practice revisions. Baseline uses keywords, not a trained model. Errors mean no decision; retain conventions and never interpret this as a compliance or violation check. required and rule paths apply only to this request."}, func(ctx context.Context, r *mcp.CallToolRequest, in input) (*mcp.CallToolResult, relevanceResult, error) {
		id, e := a.workspacePrincipal(ctx, in.WorkspaceID, false)
		if e != nil {
			return nil, relevanceResult{}, e
		}
		out, e := a.relevance(ctx, id.Workspace, in.relevanceInput)
		if errors.Is(e, decision.ErrUnavailable) {
			return nil, relevanceResult{}, e
		}
		return nil, out, toolError(e)
	})
}

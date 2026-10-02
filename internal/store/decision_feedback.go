package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tenapato/kasane-mcp/internal/core"
)

// Expired rows retain only identifiers and withdrawal metadata for current
// permission registries. Raw task/rule snapshots and judgements are purged.
type DecisionEvaluation struct {
	ID                  string          `json:"id"`
	WorkspaceID         string          `json:"workspace_id"`
	RequestHash         string          `json:"-"`
	SubmittedBy         string          `json:"submitted_by"`
	Payload             json.RawMessage `json:"payload"`
	ReviewStatus        string          `json:"review_status"`
	Review              json.RawMessage `json:"review"`
	ReviewedBy          *string         `json:"reviewed_by"`
	ReviewedAt          *time.Time      `json:"reviewed_at"`
	PermissionGrantedAt *time.Time      `json:"permission_granted_at"`
	CreatedAt           time.Time       `json:"created_at"`
	ExpiresAt           time.Time       `json:"expires_at"`
}

const evaluationCols = `id::text,workspace_id::text,request_hash,submitted_by,payload,review_status,review,reviewed_by,reviewed_at,permission_granted_at,created_at,expires_at`

func scanEvaluation(row pgx.Row) (DecisionEvaluation, error) {
	var out DecisionEvaluation
	e := row.Scan(&out.ID, &out.WorkspaceID, &out.RequestHash, &out.SubmittedBy, &out.Payload, &out.ReviewStatus, &out.Review, &out.ReviewedBy, &out.ReviewedAt, &out.PermissionGrantedAt, &out.CreatedAt, &out.ExpiresAt)
	if errors.Is(e, pgx.ErrNoRows) {
		e = core.ErrNotFound
	}
	return out, e
}
func (s *Store) DecisionEvaluation(ctx context.Context, ws, id string) (DecisionEvaluation, error) {
	if !validUUID(ws) || !validUUID(id) {
		return DecisionEvaluation{}, core.ErrInvalid
	}
	return scanEvaluation(s.DB.QueryRow(ctx, `SELECT `+evaluationCols+` FROM decision_evaluations WHERE workspace_id=$1 AND id=$2 AND payload IS NOT NULL AND expires_at>now()`, ws, id))
}
func (s *Store) SaveDecisionEvaluation(ctx context.Context, ws, id, hash, actor string, payload []byte, days int) (DecisionEvaluation, error) {
	if !validUUID(ws) || !validUUID(id) || len(hash) != 64 || len(payload) > 2<<20 || !json.Valid(payload) || days < 1 || days > 365 {
		return DecisionEvaluation{}, core.ErrInvalid
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return DecisionEvaluation{}, e
	}
	defer tx.Rollback(ctx)
	// Serialize quota accounting and same-ID retries for a workspace.
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,57))`, ws); e != nil {
		return DecisionEvaluation{}, e
	}
	old, e := scanEvaluation(tx.QueryRow(ctx, `SELECT `+evaluationCols+` FROM decision_evaluations WHERE workspace_id=$1 AND id=$2`, ws, id))
	if e == nil {
		if old.RequestHash != hash || old.SubmittedBy != actor {
			return DecisionEvaluation{}, core.ErrConflict
		}
		if old.Payload == nil || !old.ExpiresAt.After(time.Now()) {
			return DecisionEvaluation{}, core.ErrNotFound
		}
		return old, nil
	}
	if !errors.Is(e, core.ErrNotFound) {
		return DecisionEvaluation{}, e
	}
	var count int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM decision_evaluations WHERE workspace_id=$1 AND expires_at>now() AND payload IS NOT NULL`, ws).Scan(&count); e != nil {
		return DecisionEvaluation{}, e
	}
	if count >= 500 {
		return DecisionEvaluation{}, core.ErrConflict
	}
	var auto bool
	if e = tx.QueryRow(ctx, `SELECT COALESCE((SELECT agent_candidates_enabled FROM decision_feedback_policies WHERE workspace_id=$1),false)`, ws).Scan(&auto); e != nil {
		return DecisionEvaluation{}, e
	}
	status := "unreviewed"
	if auto {
		status = "agent_generated"
	}
	out, e := scanEvaluation(tx.QueryRow(ctx, `INSERT INTO decision_evaluations(workspace_id,id,request_hash,submitted_by,payload,expires_at,review_status,permission_granted_at) VALUES($1,$2,$3,$4,$5,now()+make_interval(days=>$6),$7,CASE WHEN $8 THEN now() ELSE NULL END) RETURNING `+evaluationCols, ws, id, hash, actor, payload, days, status, auto))
	if e != nil {
		return out, e
	}
	return out, tx.Commit(ctx)
}
func (s *Store) DecisionEvaluations(ctx context.Context, ws string, offset int) ([]DecisionEvaluation, error) {
	if !validUUID(ws) || offset < 0 || offset > 100000 {
		return nil, core.ErrInvalid
	}
	rows, e := s.DB.Query(ctx, `SELECT `+evaluationCols+` FROM decision_evaluations WHERE workspace_id=$1 AND payload IS NOT NULL AND expires_at>now() ORDER BY created_at DESC,id LIMIT 10 OFFSET $2`, ws, offset)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []DecisionEvaluation{}
	for rows.Next() {
		v, e := scanEvaluation(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) ReviewDecisionEvaluation(ctx context.Context, ws, id, actor string, review []byte) (DecisionEvaluation, error) {
	if !validUUID(ws) || !validUUID(id) || !json.Valid(review) {
		return DecisionEvaluation{}, core.ErrInvalid
	}
	out, e := scanEvaluation(s.DB.QueryRow(ctx, `UPDATE decision_evaluations SET review_status='reviewed',review=$4,reviewed_by=$3,reviewed_at=now(),permission_granted_at=now() WHERE workspace_id=$1 AND id=$2 AND review_status='unreviewed' AND payload IS NOT NULL AND expires_at>now() RETURNING `+evaluationCols, ws, id, actor, review))
	if errors.Is(e, core.ErrNotFound) {
		e = core.ErrConflict
	}
	return out, e
}
func (s *Store) WithdrawDecisionEvaluation(ctx context.Context, ws, id string) error {
	if !validUUID(ws) || !validUUID(id) {
		return core.ErrInvalid
	}
	result, e := s.DB.Exec(ctx, `UPDATE decision_evaluations SET review_status='withdrawn',payload=NULL,review=NULL WHERE workspace_id=$1 AND id=$2`, ws, id)
	if e == nil && result.RowsAffected() == 0 {
		return core.ErrNotFound
	}
	return e
}
func (s *Store) PurgeDecisionEvaluations(ctx context.Context) error {
	_, e := s.DB.Exec(ctx, `UPDATE decision_evaluations SET payload=NULL,review=NULL,review_status='withdrawn' WHERE expires_at<=now() AND payload IS NOT NULL`)
	return e
}

// Registry entries remain available after withdrawal/expiry, without raw content.
func (s *Store) DecisionEvaluationPermissions(ctx context.Context, ws string) ([]DecisionEvaluation, error) {
	if !validUUID(ws) {
		return nil, core.ErrInvalid
	}
	rows, e := s.DB.Query(ctx, `SELECT id::text,review_status,permission_granted_at,expires_at FROM decision_evaluations WHERE workspace_id=$1 AND permission_granted_at IS NOT NULL ORDER BY id`, ws)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []DecisionEvaluation{}
	for rows.Next() {
		v := DecisionEvaluation{WorkspaceID: ws}
		if e := rows.Scan(&v.ID, &v.ReviewStatus, &v.PermissionGrantedAt, &v.ExpiresAt); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Only an owner session can change this policy. Disabling also withdraws existing
// automatic candidates in the same transaction; MCP keys cannot grant consent.
func (s *Store) SetDecisionTrainingPolicy(ctx context.Context, ws, owner string, enabled bool) error {
	if !validUUID(ws) {
		return core.ErrInvalid
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,57))`, ws); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO decision_feedback_policies(workspace_id,agent_candidates_enabled,updated_by) VALUES($1,$2,$3) ON CONFLICT(workspace_id) DO UPDATE SET agent_candidates_enabled=EXCLUDED.agent_candidates_enabled,updated_by=EXCLUDED.updated_by,updated_at=now()`, ws, enabled, owner); e != nil {
		return e
	}
	if !enabled {
		if _, e = tx.Exec(ctx, `UPDATE decision_evaluations SET review_status='withdrawn',payload=NULL,review=NULL WHERE workspace_id=$1 AND review_status='agent_generated'`, ws); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func (s *Store) DecisionTrainingPolicy(ctx context.Context, ws string) (bool, error) {
	if !validUUID(ws) {
		return false, core.ErrInvalid
	}
	var enabled bool
	e := s.DB.QueryRow(ctx, `SELECT COALESCE((SELECT agent_candidates_enabled FROM decision_feedback_policies WHERE workspace_id=$1),false)`, ws).Scan(&enabled)
	return enabled, e
}

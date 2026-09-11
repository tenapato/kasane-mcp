package store

import (
	"context"
	"github.com/tenapato/kasane-mcp/internal/core"
	"strings"
)

func (s *Store) WorkspaceOwned(ctx context.Context, ws, username string) (bool, error) {
	if !validUUID(ws) {
		return false, core.ErrInvalid
	}
	var ok bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1::uuid AND owner_username=$2)`, ws, username).Scan(&ok)
	return ok, err
}
func (s *Store) WorkspacesFor(ctx context.Context, username string) ([]core.Workspace, error) {
	rows, err := s.DB.Query(ctx, `SELECT id::text,name,created_at FROM workspaces WHERE owner_username=$1 ORDER BY created_at,id`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Workspace{}
	for rows.Next() {
		var ws core.Workspace
		if err = rows.Scan(&ws.ID, &ws.Name, &ws.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}
func (s *Store) CreateWorkspaceOwned(ctx context.Context, name, username string) (core.Workspace, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 || username == "" {
		return core.Workspace{}, core.ErrInvalid
	}
	var ws core.Workspace
	err := s.DB.QueryRow(ctx, `INSERT INTO workspaces(id,name,owner_username) VALUES($1,$2,$3) RETURNING id::text,name,created_at`, core.UUID(), name, username).Scan(&ws.ID, &ws.Name, &ws.CreatedAt)
	return ws, err
}
func (s *Store) StatsFor(ctx context.Context, username string) (Stats, error) {
	var out Stats
	err := s.DB.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM outbox o JOIN memories m ON m.id=o.memory_id JOIN workspaces w ON w.id=m.workspace_id WHERE w.owner_username=$1),
 (SELECT count(*) FROM outbox o JOIN memories m ON m.id=o.memory_id JOIN workspaces w ON w.id=m.workspace_id WHERE w.owner_username=$1 AND o.attempts>0),
 (SELECT count(*) FROM memories m JOIN workspaces w ON w.id=m.workspace_id WHERE w.owner_username=$1 AND NOT m.deleted)`, username).Scan(&out.PendingJobs, &out.FailedJobs, &out.MemoryCount)
	return out, err
}

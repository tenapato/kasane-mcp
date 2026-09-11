package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tenapato/kasane-mcp/internal/core"
)

type rememberInput struct {
	core.RememberInput
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"Project workspace ID; required for multi-workspace keys"`
}
type searchInput struct {
	core.SearchInput
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"Project workspace ID; required for multi-workspace keys"`
}

func (a *App) workspacePrincipal(ctx context.Context, ws string, write bool) (identity, error) {
	id, err := principal(ctx, write)
	if err != nil {
		return id, err
	}
	if ws == "" {
		if id.AccessMode == "all" || id.AccessMode == "selected" {
			return id, fmt.Errorf("%w: workspace_id required; call kasane_workspaces to choose a project", core.ErrInvalid)
		}
		ws = id.Workspace
	}
	if !core.ValidID(ws) {
		return id, fmt.Errorf("%w: invalid workspace_id", core.ErrInvalid)
	}
	ws = strings.ToLower(ws)
	allowed := false
	switch id.AccessMode {
	case "all":
		allowed, err = a.store.WorkspaceExists(ctx, ws)
	case "selected":
		err = a.store.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM agent_key_workspaces WHERE key_id=$1 AND workspace_id=$2::uuid)", id.KeyID, ws).Scan(&allowed)
	default:
		allowed = ws == id.Workspace
	}
	if err != nil {
		return id, toolError(err)
	}
	if !allowed {
		return id, fmt.Errorf("%w: workspace unavailable to this key", errForbidden)
	}
	id.Workspace = ws
	return id, nil
}

type workspacesResult struct {
	Workspaces          []core.Workspace `json:"workspaces"`
	AccessMode          string           `json:"access_mode"`
	Scope               string           `json:"scope"`
	CanCreateWorkspaces bool             `json:"can_create_workspaces"`
}
type workspaceCreateInput struct {
	Name string `json:"name"`
}
type moveInput struct {
	WorkspaceID       string `json:"workspace_id" jsonschema:"Source project workspace ID"`
	TargetWorkspaceID string `json:"target_workspace_id" jsonschema:"Destination project workspace ID"`
	ID                string `json:"id"`
	ExpectedRevision  int64  `json:"expected_revision"`
}

func (a *App) addWorkspaceTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "kasane_workspaces", Description: "List project workspaces accessible to this key and its permissions. Multi-workspace keys must choose a workspace_id for each memory/profile call."}, func(ctx context.Context, r *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, workspacesResult, error) {
		id, err := principal(ctx, false)
		if err != nil {
			return nil, workspacesResult{}, err
		}
		rows, err := a.store.DB.Query(ctx, `SELECT id::text,name,created_at FROM workspaces WHERE $1='all' OR ($1='single' AND id=$2::uuid) OR ($1='selected' AND EXISTS(SELECT 1 FROM agent_key_workspaces WHERE key_id=$3::uuid AND workspace_id=workspaces.id)) ORDER BY created_at,id`, id.AccessMode, id.Workspace, id.KeyID)
		if err != nil {
			return nil, workspacesResult{}, toolError(err)
		}
		defer rows.Close()
		out := workspacesResult{Workspaces: []core.Workspace{}, AccessMode: id.AccessMode, Scope: id.Scope, CanCreateWorkspaces: id.CanCreateWorkspaces}
		for rows.Next() {
			var w core.Workspace
			if err = rows.Scan(&w.ID, &w.Name, &w.CreatedAt); err != nil {
				return nil, workspacesResult{}, toolError(err)
			}
			out.Workspaces = append(out.Workspaces, w)
		}
		return nil, out, toolError(rows.Err())
	})
	mcp.AddTool(s, &mcp.Tool{Name: "kasane_workspace_create", Description: "Create a project workspace. Requires a write key with workspace creation permission. The creating key gains access automatically."}, func(ctx context.Context, r *mcp.CallToolRequest, in workspaceCreateInput) (*mcp.CallToolResult, core.Workspace, error) {
		id, err := principal(ctx, true)
		if err != nil {
			return nil, core.Workspace{}, err
		}
		if !id.CanCreateWorkspaces || (id.AccessMode != "all" && id.AccessMode != "selected") {
			return nil, core.Workspace{}, errForbidden
		}
		name := strings.TrimSpace(in.Name)
		if name == "" || len(name) > 120 {
			return nil, core.Workspace{}, fmt.Errorf("%w: name must be 1–120 bytes", core.ErrInvalid)
		}
		ws, err := a.store.CreateWorkspaceForKey(ctx, id.KeyID, name)
		return nil, ws, toolError(err)
	})
	mcp.AddTool(s, &mcp.Tool{Name: "kasane_move", Description: "Move a memory between projects, preserving its ID and content and incrementing its revision. Requires write access to both workspaces and expected_revision from get. Clears the original create idempotency key and queues reindexing."}, func(ctx context.Context, r *mcp.CallToolRequest, in moveInput) (*mcp.CallToolResult, core.Memory, error) {
		source, err := a.workspacePrincipal(ctx, in.WorkspaceID, true)
		if err != nil {
			return nil, core.Memory{}, err
		}
		if in.TargetWorkspaceID == "" {
			return nil, core.Memory{}, core.ErrInvalid
		}
		target, err := a.workspacePrincipal(ctx, in.TargetWorkspaceID, true)
		if err != nil {
			return nil, core.Memory{}, err
		}
		memory, err := a.store.Move(ctx, source.Workspace, target.Workspace, in.ID, in.ExpectedRevision)
		return nil, memory, toolError(err)
	})
}

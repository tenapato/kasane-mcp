package search

import (
	"context"
	"github.com/tenapato/kasane-mcp/internal/core"
)

type Repository interface {
	List(context.Context, string, core.SearchInput) (core.SearchResult, error)
	Rehydrate(context.Context, string, []core.Hit) ([]core.Memory, error)
}
type Index interface {
	Search(context.Context, string, core.SearchInput) ([]core.Hit, error)
}
type Service struct {
	Repo  Repository
	Index Index
}

func (s *Service) Search(ctx context.Context, workspace string, in core.SearchInput) (core.SearchResult, error) {
	if err := in.Validate(); err != nil {
		return core.SearchResult{}, err
	}
	if in.Query == "" {
		return s.Repo.List(ctx, workspace, in)
	}
	hits, err := s.Index.Search(ctx, workspace, in)
	if err != nil {
		out, e := s.Repo.List(ctx, workspace, in)
		out.Degraded = true
		return out, e
	}
	memories, err := s.Repo.Rehydrate(ctx, workspace, hits)
	if err != nil {
		return core.SearchResult{}, err
	}
	return core.SearchResult{Memories: memories, Total: len(memories)}, nil
}

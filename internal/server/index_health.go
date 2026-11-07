package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/tenapato/kasane-mcp/internal/core"
	"github.com/tenapato/kasane-mcp/internal/search"
)

// Qdrant is a derived index; PostgreSQL stays the source of truth. These
// helpers keep the service usable while the index is unavailable, make the
// degraded state visible, and rebuild the index once Qdrant returns.

const (
	reasonNotChecked   = "not_checked"
	reasonUnreachable  = "qdrant_unreachable"
	reasonIncompatible = "collection_incompatible"
	reasonQdrantError  = "qdrant_error"
	reasonDatabase     = "database_unavailable"
	reasonRebuilding   = "index_rebuilding"

	degradedWarnEvery = 5 * time.Minute
)

var reasonMessages = map[string]string{
	reasonNotChecked:   "search index not verified yet",
	reasonUnreachable:  "Qdrant is unreachable; search uses PostgreSQL full-text fallback",
	reasonIncompatible: "Qdrant collection has an incompatible configuration; manual recovery required (see docs/operations.md)",
	reasonQdrantError:  "Qdrant returned an error; search uses PostgreSQL full-text fallback",
	reasonDatabase:     "cannot compare the search index with PostgreSQL",
	reasonRebuilding:   "search index is being rebuilt from PostgreSQL; search uses PostgreSQL full-text fallback",
}

type indexStatus struct {
	State   string    `json:"state"`
	Reason  string    `json:"reason,omitempty"`
	Message string    `json:"message,omitempty"`
	Since   time.Time `json:"since"`
	// Qdrant reports whether the Qdrant collection itself answered.
	Qdrant bool `json:"-"`
}

// indexHealth tracks search index state and rate-limits degraded warnings.
type indexHealth struct {
	mu         sync.Mutex
	reason     string
	qdrant     bool
	since      time.Time
	lastWarn   time.Time
	rebuilding bool
	checked    bool
	now        func() time.Time
}

func newIndexHealth() *indexHealth {
	h := &indexHealth{now: time.Now, reason: reasonNotChecked}
	h.since = h.now()
	return h
}

func (h *indexHealth) snapshot() indexStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := indexStatus{State: "ready", Since: h.since, Qdrant: h.qdrant}
	if h.reason != "" {
		st.State, st.Reason, st.Message = "degraded", h.reason, reasonMessages[h.reason]
	}
	return st
}

// searchable reports whether ranked Qdrant search can be trusted to be complete.
func (h *indexHealth) searchable() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reason == ""
}

// degrade records a degraded state. It logs on every reason change and then at
// most every degradedWarnEvery, so a long outage stays visible without
// flooding logs.
func (h *indexHealth) degrade(reason string, qdrantUp bool, attrs ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.qdrant = qdrantUp
	h.checked = true
	changed := h.reason != reason
	if changed {
		if h.reason == "" || h.reason == reasonNotChecked {
			h.since = now
		}
		h.reason = reason
	} else if now.Sub(h.lastWarn) < degradedWarnEvery {
		return
	}
	h.lastWarn = now
	attrs = append([]any{"reason", reason, "detail", reasonMessages[reason], "degraded_for", now.Sub(h.since).Round(time.Second).String()}, attrs...)
	slog.Warn("search index degraded", attrs...)
}

func (h *indexHealth) recover() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.qdrant = true
	if h.reason != "" {
		if h.checked {
			slog.Info("search index healthy again", "previous_reason", h.reason, "degraded_for", now.Sub(h.since).Round(time.Second).String())
		}
		h.reason, h.since = "", now
	}
	h.checked = true
}

func classifyIndexError(err error) string {
	switch {
	case errors.Is(err, search.ErrIncompatible):
		return reasonIncompatible
	case errors.Is(err, search.ErrUnreachable), errors.Is(err, context.DeadlineExceeded):
		return reasonUnreachable
	default:
		return reasonQdrantError
	}
}

type vectorIndex interface {
	Ensure(context.Context) (bool, error)
	Count(context.Context) (int, error)
}
type indexSource interface {
	IndexCounts(context.Context) (live, indexed int, err error)
	Reindex(context.Context, string) error
}

// reconcileIndex verifies Qdrant against PostgreSQL. A missing collection is
// recreated; when Qdrant holds fewer points than PostgreSQL has indexed
// memories, every live memory is queued again. It never deletes Qdrant data.
// It returns whether the indexing worker may apply queued jobs.
func reconcileIndex(ctx context.Context, idx vectorIndex, src indexSource, h *indexHealth) bool {
	created, err := idx.Ensure(ctx)
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		h.degrade(classifyIndexError(err), false, "error", err.Error())
		return false
	}
	if created {
		slog.Warn("search index collection was missing and has been created", "collection", search.Collection)
	}
	live, indexed, err := src.IndexCounts(ctx)
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		h.degrade(reasonDatabase, true)
		return false
	}
	points, err := idx.Count(ctx)
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		h.degrade(classifyIndexError(err), false, "error", err.Error())
		return false
	}
	h.mu.Lock()
	first := !h.checked
	h.mu.Unlock()
	if points < indexed {
		slog.Error("search index is missing points; queuing a full rebuild from PostgreSQL", "collection", search.Collection, "qdrant_points", points, "indexed_memories", indexed, "live_memories", live)
		if err := src.Reindex(ctx, ""); err != nil {
			h.degrade(reasonDatabase, true)
			return true
		}
		h.setRebuilding(true)
		h.degrade(reasonRebuilding, true, "indexed_memories", 0, "live_memories", live)
		return true
	} else if first && indexed < live {
		// A restart may interrupt a rebuild; finish it before trusting Qdrant.
		h.setRebuilding(true)
	}
	if h.isRebuilding() {
		if indexed < live {
			h.degrade(reasonRebuilding, true, "indexed_memories", indexed, "live_memories", live)
			return true
		}
		slog.Info("search index rebuild complete", "memories", live)
		h.setRebuilding(false)
	}
	h.recover()
	return true
}

func (h *indexHealth) setRebuilding(v bool) {
	h.mu.Lock()
	h.rebuilding = v
	h.mu.Unlock()
}
func (h *indexHealth) isRebuilding() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rebuilding
}

var errIndexDegraded = errors.New("search index degraded")

// gatedIndex sends searches to PostgreSQL fallback while the index is degraded
// or rebuilding, so partially rebuilt Qdrant results never look complete.
type gatedIndex struct {
	index  search.Index
	health *indexHealth
}

func (g gatedIndex) Search(ctx context.Context, workspace string, in core.SearchInput) ([]core.Hit, error) {
	if g.index == nil || !g.health.searchable() {
		return nil, errIndexDegraded
	}
	return g.index.Search(ctx, workspace, in)
}

package server

import (
	"github.com/tenapato/kasane-mcp/internal/core"
	"net/http"
	"strconv"
)

// Overview always follows the signed-in account, including for administrators.
func (a *App) overview(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(identityKey{}).(identity).Username
	workspaces, err := a.store.WorkspacesFor(r.Context(), user)
	if err != nil {
		a.failure(w, err)
		return
	}
	stats, err := a.store.StatsFor(r.Context(), user)
	if err != nil {
		a.failure(w, err)
		return
	}
	respond(w, 200, map[string]any{"workspaces": workspaces, "memory_count": stats.MemoryCount})
}
func (a *App) overviewUsage(w http.ResponseWriter, r *http.Request) {
	report, err := a.store.UsageFor(r.Context(), r.Context().Value(identityKey{}).(identity).Username)
	if err != nil {
		a.failure(w, err)
		return
	}
	respond(w, 200, report)
}
func (a *App) overviewMemories(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	in := core.SearchInput{Query: q.Get("q"), Tag: q.Get("tag"), Kind: q.Get("kind"), Limit: 50}
	var err error
	if q.Has("limit") {
		in.Limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil {
			fail(w, 400, "invalid limit")
			return
		}
	}
	if q.Has("offset") {
		in.Offset, err = strconv.Atoi(q.Get("offset"))
		if err != nil {
			fail(w, 400, "invalid offset")
			return
		}
	}
	result, err := a.store.ListFor(r.Context(), r.Context().Value(identityKey{}).(identity).Username, in)
	if err != nil {
		a.failure(w, err)
		return
	}
	respond(w, 200, result)
}
func (a *App) overviewMap(w http.ResponseWriter, r *http.Request) {
	records, err := a.store.ListFor(r.Context(), r.Context().Value(identityKey{}).(identity).Username, core.SearchInput{Limit: 50})
	if err != nil {
		a.failure(w, err)
		return
	}
	a.renderKnowledgeMap(w, r, records)
}

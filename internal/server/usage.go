package server

import (
	"context"
	"fmt"
	"github.com/tenapato/kasane-mcp/internal/core"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"
)

func fullContextCharacters(memories []core.Memory) int64 {
	var total int64
	for _, m := range memories {
		total += int64(utf8.RuneCountInString(fmt.Sprintf("[%s] %s\nSource: %s\n", m.ID, m.Title, m.Source)) + utf8.RuneCountInString(m.Content) + 2)
	}
	return total
}
func (a *App) recordContextUsage(ctx context.Context, ws string, memories []core.Memory, result core.ContextResult) {
	accountingCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := a.store.RecordContextUsage(accountingCtx, ws, fullContextCharacters(memories), int64(result.Characters)); err != nil {
		slog.Warn("context usage accounting unavailable")
	}
}
func (a *App) usage(w http.ResponseWriter, r *http.Request) {
	report, err := a.store.Usage(r.Context(), r.PathValue("workspace"))
	if err != nil {
		a.failure(w, err)
		return
	}
	respond(w, 200, report)
}

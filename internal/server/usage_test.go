package server

import (
	"github.com/tenapato/kasane-mcp/internal/core"
	"testing"
)

func TestFullContextBaselineMatchesUntrimmedText(t *testing.T) {
	memories := []core.Memory{{ID: core.UUID(), Title: "日本語", Source: "文書", Content: "Hello 世界 👋"}, {ID: core.UUID(), Title: "Second", Content: "More knowledge"}}
	full := core.PackContext(memories, 40000)
	if got := fullContextCharacters(memories); got != int64(full.Characters) {
		t.Fatalf("baseline %d differs from text %d", got, full.Characters)
	}
	short := core.PackContext(memories, 70)
	if !short.Truncated || short.Characters >= full.Characters {
		t.Fatal("fixture was not truncated")
	}
	if fullContextCharacters(nil) != 0 {
		t.Fatal("empty baseline nonzero")
	}
}

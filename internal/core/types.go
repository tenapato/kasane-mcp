package core

import (
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("memory changed; reload before saving")
	ErrInvalid  = errors.New("invalid input")
)

type Workspace struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Memory struct {
	Kind            string    `json:"kind"`
	ID              string    `json:"id"`
	WorkspaceID     string    `json:"workspace_id"`
	Title           string    `json:"title"`
	Content         string    `json:"content"`
	Tags            []string  `json:"tags"`
	Source          string    `json:"source"`
	Revision        int64     `json:"revision"`
	IndexedRevision int64     `json:"indexed_revision"`
	IndexingStatus  string    `json:"indexing_status"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Deleted         bool      `json:"-"`
	Score           float64   `json:"score,omitempty"`
}

type RememberInput struct {
	Kind             string   `json:"kind,omitempty"`
	ID               string   `json:"id,omitempty"`
	Title            string   `json:"title"`
	Content          string   `json:"content"`
	Tags             []string `json:"tags,omitempty"`
	Source           string   `json:"source,omitempty"`
	ExpectedRevision int64    `json:"expected_revision,omitempty"`
	IdempotencyKey   string   `json:"idempotency_key,omitempty"`
}

type SearchInput struct {
	Kind   string `json:"kind,omitempty"`
	Query  string `json:"query"`
	Tag    string `json:"tag,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Offset int    `json:"offset,omitempty"`
}

type SearchResult struct {
	Memories []Memory `json:"memories"`
	Degraded bool     `json:"degraded"`
	Total    int      `json:"total"`
}

type Hit struct {
	ID       string
	Revision int64
	Score    float64
}

type ContextResult struct {
	Text       string   `json:"text"`
	MemoryIDs  []string `json:"memory_ids"`
	Characters int      `json:"characters"`
	Truncated  bool     `json:"truncated"`
	Degraded   bool     `json:"degraded"`
}

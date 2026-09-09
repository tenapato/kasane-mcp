package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tenapato/kasane-mcp/internal/core"
)

const collection = "kasane_memories_v1"

type Qdrant struct {
	URL, Key string
	Client   *http.Client
}

func NewQdrant(url, key string) *Qdrant {
	return &Qdrant{strings.TrimRight(url, "/"), key, &http.Client{Timeout: 5 * time.Second}}
}

func (q *Qdrant) request(ctx context.Context, method, path string, body any, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, q.URL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if q.Key != "" {
		req.Header.Set("api-key", q.Key)
	}
	res, err := q.Client.Do(req)
	if err != nil {
		return fmt.Errorf("vector service unavailable: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		return fmt.Errorf("vector service HTTP %d", res.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	return nil
}

func (q *Qdrant) Health(ctx context.Context) error {
	var out struct {
		Result struct {
			Config struct {
				Params struct {
					Sparse map[string]struct {
						Modifier string `json:"modifier"`
					} `json:"sparse_vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	if err := q.request(ctx, "GET", "/collections/"+collection, nil, &out); err != nil {
		return err
	}
	if out.Result.Config.Params.Sparse["keywords"].Modifier != "idf" {
		return fmt.Errorf("incompatible vector collection: keywords IDF index required")
	}
	return nil
}
func (q *Qdrant) Ensure(ctx context.Context) error {
	if err := q.Health(ctx); err != nil {
		err = q.request(ctx, "PUT", "/collections/"+collection, map[string]any{"sparse_vectors": map[string]any{"keywords": map[string]any{"modifier": "idf"}}}, nil)
		if err != nil {
			if q.Health(ctx) != nil {
				return err
			}
		}
	}
	for _, field := range []string{"workspace_id", "kind", "tags"} {
		if err := q.request(ctx, "PUT", "/collections/"+collection+"/index?wait=true", map[string]any{"field_name": field, "field_schema": "keyword"}, nil); err != nil {
			return err
		}
	}
	return nil
}

// BM25 is a statistical keyword algorithm, not a neural model or provider call.
func document(text string) map[string]any {
	return map[string]any{"text": text, "model": "qdrant/bm25", "options": map[string]any{"language": "none"}}
}

func (q *Qdrant) Apply(ctx context.Context, m core.Memory) error {
	if m.Deleted {
		return q.request(ctx, "POST", "/collections/"+collection+"/points/delete?wait=true", map[string]any{"points": []string{m.ID}}, nil)
	}
	point := map[string]any{"id": m.ID, "vector": map[string]any{"keywords": document(m.Title + "\n" + m.Content + "\n" + strings.Join(m.Tags, " "))}, "payload": map[string]any{"workspace_id": m.WorkspaceID, "revision": m.Revision, "kind": m.Kind, "tags": m.Tags}}
	return q.request(ctx, "PUT", "/collections/"+collection+"/points?wait=true", map[string]any{"points": []any{point}}, nil)
}

func (q *Qdrant) Search(ctx context.Context, workspace string, in core.SearchInput) ([]core.Hit, error) {
	must := []any{map[string]any{"key": "workspace_id", "match": map[string]any{"value": workspace}}}
	if in.Tag != "" {
		must = append(must, map[string]any{"key": "tags", "match": map[string]any{"value": in.Tag}})
	}
	if in.Kind != "" {
		must = append(must, map[string]any{"key": "kind", "match": map[string]any{"value": in.Kind}})
	}
	body := map[string]any{"query": document(in.Query), "using": "keywords", "filter": map[string]any{"must": must}, "limit": in.Limit, "offset": in.Offset, "with_payload": true, "score_threshold": 0.000001}
	var result struct {
		Result struct {
			Points []struct {
				ID      string  `json:"id"`
				Score   float64 `json:"score"`
				Payload struct {
					Revision int64 `json:"revision"`
				} `json:"payload"`
			} `json:"points"`
		} `json:"result"`
	}
	if err := q.request(ctx, "POST", "/collections/"+collection+"/points/query", body, &result); err != nil {
		return nil, err
	}
	hits := []core.Hit{}
	for _, p := range result.Result.Points {
		hits = append(hits, core.Hit{ID: p.ID, Revision: p.Payload.Revision, Score: p.Score})
	}
	return hits, nil
}

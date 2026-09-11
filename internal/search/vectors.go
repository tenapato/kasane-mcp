package search

import (
	"context"
	"math"
	"sort"
	"sync"
)

type SparseVector struct {
	Indices []uint32  `json:"indices"`
	Values  []float64 `json:"values"`
}
type VectorPoint struct {
	ID      string                  `json:"id"`
	Vector  map[string]SparseVector `json:"vector"`
	Payload struct {
		WorkspaceID string `json:"workspace_id"`
		Revision    int64  `json:"revision"`
	} `json:"payload"`
}

// Vectors only retrieves caller-selected IDs; the caller must validate payload
// workspace and revision against canonical records before displaying any data.
func (q *Qdrant) Vectors(ctx context.Context, ids []string) (map[string]VectorPoint, error) {
	out := map[string]VectorPoint{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	// The map endpoint caps ids at50. Small batches bound response size.
	for start := 0; start < len(ids); start += 10 {
		end := start + 10
		if end > len(ids) {
			end = len(ids)
		}
		batch := append([]string(nil), ids[start:end]...)
		wg.Add(1)
		go func() {
			defer wg.Done()
			var response struct {
				Result []VectorPoint `json:"result"`
			}
			err := q.request(ctx, "POST", "/collections/"+collection+"/points", map[string]any{"ids": batch, "with_vector": []string{"keywords"}, "with_payload": []string{"workspace_id", "revision"}}, &response)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for _, p := range response.Result {
				out[p.ID] = p
			}
		}()
	}
	wg.Wait()
	return out, firstErr
}

func (v SparseVector) Normalized() map[uint32]float64 {
	if len(v.Indices) != len(v.Values) {
		return nil
	}
	out := map[uint32]float64{}
	sum := 0.0
	for i, index := range v.Indices {
		value := v.Values[i]
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil
		}
		if value > 0 {
			out[index] += value
		}
	}
	for _, v := range out {
		sum += v * v
	}
	if sum <= 0 || math.IsInf(sum, 0) {
		return nil
	}
	norm := math.Sqrt(sum)
	for k, v := range out {
		out[k] = v / norm
	}
	return out
}
func Cosine(a, b map[uint32]float64) float64 {
	if len(a) > len(b) {
		a, b = b, a
	}
	score := 0.0
	for dim, weight := range a {
		score += weight * b[dim]
	}
	return math.Min(1, math.Max(0, score))
}

type VectorWeight struct {
	Dimension uint32  `json:"dimension"`
	Value     float64 `json:"value"`
}

func (v SparseVector) TopWeights() []VectorWeight {
	out := []VectorWeight{}
	if len(v.Indices) != len(v.Values) {
		return out
	}
	for i, dim := range v.Indices {
		out = append(out, VectorWeight{dim, v.Values[i]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value == out[j].Value {
			return out[i].Dimension < out[j].Dimension
		}
		return out[i].Value > out[j].Value
	})
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

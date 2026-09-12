package server

import (
	"github.com/tenapato/kasane-mcp/internal/core"
	"github.com/tenapato/kasane-mcp/internal/search"
	"net/http"
	"sort"
)

type mapNode struct {
	WorkspaceID    string                `json:"workspace_id"`
	ID             string                `json:"id"`
	Title          string                `json:"title"`
	Kind           string                `json:"kind"`
	Tags           []string              `json:"tags"`
	IndexingStatus string                `json:"indexing_status"`
	Dimensions     int                   `json:"dimensions"`
	Weights        []search.VectorWeight `json:"weights"`
}
type mapEdge struct {
	Source     string  `json:"source"`
	Target     string  `json:"target"`
	Similarity float64 `json:"similarity"`
}
type mapResult struct {
	Nodes    []mapNode `json:"nodes"`
	Edges    []mapEdge `json:"edges"`
	Total    int       `json:"total"`
	Limit    int       `json:"limit"`
	Degraded bool      `json:"degraded"`
}

func (a *App) knowledgeMap(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("workspace")
	records, err := a.store.List(r.Context(), ws, core.SearchInput{Limit: 50})
	if err != nil {
		a.failure(w, err)
		return
	}
	a.renderKnowledgeMap(w, r, records)
}
func (a *App) renderKnowledgeMap(w http.ResponseWriter, r *http.Request, records core.SearchResult) {
	var err error
	out := mapResult{Nodes: []mapNode{}, Edges: []mapEdge{}, Total: records.Total, Limit: 50}
	ids := []string{}
	for _, m := range records.Memories {
		ids = append(ids, m.ID)
	}
	points := map[string]search.VectorPoint{}
	if len(ids) > 0 {
		if a.index == nil {
			out.Degraded = true
		} else {
			points, err = a.index.Vectors(r.Context(), ids)
			out.Degraded = err != nil
		}
	}
	vectors := map[string]map[uint32]float64{}
	for _, m := range records.Memories {
		node := mapNode{ID: m.ID, WorkspaceID: m.WorkspaceID, Title: m.Title, Kind: m.Kind, Tags: m.Tags, IndexingStatus: "pending", Weights: []search.VectorWeight{}}
		if out.Degraded {
			node.IndexingStatus = "unavailable"
		}
		if p, ok := points[m.ID]; ok && p.Payload.WorkspaceID == m.WorkspaceID && p.Payload.Revision == m.Revision && m.IndexedRevision == m.Revision {
			vector := p.Vector["keywords"]
			normalized := vector.Normalized()
			// Empty vectors can be valid for punctuation-only memories; they have no edges.
			if normalized != nil || (len(vector.Values) == 0 && len(vector.Indices) == 0) {
				if _, present := p.Vector["keywords"]; present {
					node.IndexingStatus = "indexed"
					node.Dimensions = len(normalized)
					node.Weights = vector.TopWeights()
					vectors[m.ID] = normalized
				}
			}
		}
		out.Nodes = append(out.Nodes, node)
	}
	// Keep at most three strongest neighbors per node (union of selections).
	seen := map[[2]string]bool{}
	for _, node := range out.Nodes {
		neighbors := []mapEdge{}
		for _, other := range out.Nodes {
			if node.ID == other.ID {
				continue
			}
			score := search.Cosine(vectors[node.ID], vectors[other.ID])
			if score >= 0.15 {
				neighbors = append(neighbors, mapEdge{node.ID, other.ID, score})
			}
		}
		sort.Slice(neighbors, func(i, j int) bool {
			if neighbors[i].Similarity == neighbors[j].Similarity {
				return neighbors[i].Target < neighbors[j].Target
			}
			return neighbors[i].Similarity > neighbors[j].Similarity
		})
		if len(neighbors) > 3 {
			neighbors = neighbors[:3]
		}
		for _, edge := range neighbors {
			if edge.Source > edge.Target {
				edge.Source, edge.Target = edge.Target, edge.Source
			}
			key := [2]string{edge.Source, edge.Target}
			if !seen[key] {
				seen[key] = true
				out.Edges = append(out.Edges, edge)
			}
		}
	}
	respond(w, 200, out)
}

package search

import (
	"math"
	"testing"
)

func TestSparseCosine(t *testing.T) {
	a := SparseVector{Indices: []uint32{9, 1}, Values: []float64{4, 3}}.Normalized()
	same := SparseVector{Indices: []uint32{1, 9}, Values: []float64{6, 8}}.Normalized()
	other := SparseVector{Indices: []uint32{8}, Values: []float64{1}}.Normalized()
	if math.Abs(Cosine(a, same)-1) > 1e-9 || Cosine(a, other) != 0 || Cosine(a, nil) != 0 {
		t.Fatal("cosine incorrect")
	}
	for _, v := range []SparseVector{{Indices: []uint32{1}, Values: nil}, {Indices: []uint32{1}, Values: []float64{math.NaN()}}, {Indices: []uint32{1}, Values: []float64{-1}}} {
		if v.Normalized() != nil {
			t.Fatal("accepted invalid vector")
		}
	}
}

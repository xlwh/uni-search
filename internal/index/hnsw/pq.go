// Package hnsw pq.go implements Product Quantization (PQ) as a third
// quantization tier (inspired by vsag's product_quantization/). PQ
// splits vectors into M subspaces, each independently K-means clustered
// into 256 centroids. Every vector is encoded as M bytes (one 8-bit
// code per subspace). Search uses a precomputed LUT (look-up table)
// for coarse ranking + float32 rerank.
//
// K-means++ initialization is used for better centroid selection.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.3
package hnsw

import (
	"fmt"
	"math"
	"math/rand/v2"
)

// PQState holds Product Quantization state: M subspaces, each with
// 256 K-means centroids. Every vector is encoded as M bytes.
type PQState struct {
	m         int       // number of subspaces
	dsub      int       // dims per subspace = dims / m
	centroids []float32 // m × 256 × dsub, flattened as m*256*dsub
	codes     []byte    // n*m bytes, FixedLayout: code[i] at codes[i*m:(i+1)*m]
	n         int
	dims      int
}

// trainPQ trains PQ with K-means++ initialization.
// M is auto-selected when <= 0: max(dims/32, 8), capped at dims.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.3.1
func trainPQ(g *Graph, m int) (*PQState, error) {
	dims := g.dims
	n := int(g.n)
	if m <= 0 {
		m = dims / 32
		if m < 8 {
			m = 8
		}
		if m > dims {
			m = dims
		}
	}
	dsub := dims / m
	if dsub*m != dims {
		return nil, fmt.Errorf("pq: dims %d not divisible by m %d", dims, m)
	}
	pq := &PQState{
		m:         m,
		dsub:      dsub,
		n:         n,
		dims:      dims,
		codes:     make([]byte, n*m),
		centroids: make([]float32, m*256*dsub),
	}
	if n == 0 {
		return pq, nil
	}
	// Train each subspace independently with K-means++.
	for j := 0; j < m; j++ {
		subvecs := extractSubspace(g.vecs, n, dims, j, dsub)
		centroids := kmeansPP(subvecs, n, dsub, 256, 25, uint64(j))
		copy(pq.centroids[j*256*dsub:(j+1)*256*dsub], centroids)
	}
	// Encode all vectors.
	for i := 0; i < n; i++ {
		v := g.Vector(int32(i))
		for j := 0; j < m; j++ {
			sub := v[j*dsub : (j+1)*dsub]
			pq.codes[i*m+j] = uint8(nearestCentroidIdx(sub, pq.centroids[j*256*dsub:(j+1)*256*dsub], dsub))
		}
	}
	return pq, nil
}

// extractSubspace returns a contiguous copy of subspace j from all n vectors.
func extractSubspace(vecs []float32, n, dims, j, dsub int) []float32 {
	out := make([]float32, n*dsub)
	for i := 0; i < n; i++ {
		copy(out[i*dsub:(i+1)*dsub], vecs[i*dims+j*dsub:i*dims+(j+1)*dsub])
	}
	return out
}

// kmeansPP runs K-means with K-means++ initialization.
// Returns k*dsub centroids as a flat []float32.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.3.2
func kmeansPP(data []float32, n, d, k, maxIters int, seed uint64) []float32 {
	rng := rand.New(rand.NewPCG(seed, 0))
	centroids := make([]float32, k*d)
	// K-means++ initialization.
	centroids[0*d] = data[rng.IntN(n)*d]
	for i := 1; i < d; i++ {
		centroids[0*d+i] = data[rng.IntN(n)*d+i]
	}
	minDistSq := make([]float64, n)
	for i := range minDistSq {
		minDistSq[i] = math.MaxFloat64
	}
	for c := 1; c < k; c++ {
		// Update min distances to nearest center.
		for i := 0; i < n; i++ {
			dSq := l2SquaredFloat32(data[i*d:(i+1)*d], centroids[(c-1)*d:c*d], d)
			if dSq < minDistSq[i] {
				minDistSq[i] = dSq
			}
		}
		// Weighted random selection.
		total := 0.0
		for _, d := range minDistSq {
			total += d
		}
		if total == 0 {
			// All points coincide; pick random.
			idx := rng.IntN(n)
			copy(centroids[c*d:(c+1)*d], data[idx*d:(idx+1)*d])
			continue
		}
		r := rng.Float64() * total
		cum := 0.0
		selected := 0
		for i := 0; i < n; i++ {
			cum += minDistSq[i]
			if cum >= r {
				selected = i
				break
			}
		}
		copy(centroids[c*d:(c+1)*d], data[selected*d:(selected+1)*d])
	}
	// Lloyd iterations.
	assignments := make([]int, n)
	for iter := 0; iter < maxIters; iter++ {
		changed := 0
		for i := 0; i < n; i++ {
			nn := nearestCentroidIdx(data[i*d:(i+1)*d], centroids, d)
			if assignments[i] != nn {
				assignments[i] = nn
				changed++
			}
		}
		if iter > 0 && changed == 0 {
			break
		}
		// Update centroids.
		counts := make([]int, k)
		sums := make([]float64, k*d)
		for i := 0; i < n; i++ {
			c := assignments[i]
			counts[c]++
			for j := 0; j < d; j++ {
				sums[c*d+j] += float64(data[i*d+j])
			}
		}
		for c := 0; c < k; c++ {
			if counts[c] > 0 {
				for j := 0; j < d; j++ {
					centroids[c*d+j] = float32(sums[c*d+j] / float64(counts[c]))
				}
			}
		}
	}
	return centroids
}

// nearestCentroidIdx returns the index of the nearest centroid to vec.
func nearestCentroidIdx(vec []float32, centroids []float32, d int) int {
	bestIdx := 0
	bestDist := l2SquaredFloat32(vec, centroids[0:d], d)
	for c := 1; c < 256; c++ {
		dist := l2SquaredFloat32(vec, centroids[c*d:(c+1)*d], d)
		if dist < bestDist {
			bestDist = dist
			bestIdx = c
		}
	}
	return bestIdx
}

// l2SquaredFloat32 computes squared L2 distance between two float32 vectors.
func l2SquaredFloat32(a, b []float32, d int) float64 {
	var sum float64
	for i := 0; i < d; i++ {
		diff := float64(a[i] - b[i])
		sum += diff * diff
	}
	return sum
}

// buildLUT precomputes the LUT for a query vector.
// LUT[j][c] = squared L2 distance from query subvector j to centroid c.
//
// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.3.3
func (pq *PQState) buildLUT(q []float32) [][]float32 {
	lut := make([][]float32, pq.m)
	for j := 0; j < pq.m; j++ {
		lut[j] = make([]float32, 256)
		sub := q[j*pq.dsub : (j+1)*pq.dsub]
		centBase := j * 256 * pq.dsub
		for c := 0; c < 256; c++ {
			lut[j][c] = float32(l2SquaredFloat32(sub, pq.centroids[centBase+c*pq.dsub:centBase+(c+1)*pq.dsub], pq.dsub))
		}
	}
	return lut
}

// pqDistance computes PQ approximate distance via LUT lookup.
func pqDistance(lut [][]float32, code []byte, m int) float32 {
	var dist float32
	for j := 0; j < m; j++ {
		dist += lut[j][code[j]]
	}
	return dist
}

// MemBytes reports PQ memory: codes + centroids.
func (pq *PQState) MemBytes() int64 {
	return int64(len(pq.codes)) + int64(len(pq.centroids))*4
}

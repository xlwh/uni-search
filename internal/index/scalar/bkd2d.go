package scalar

import (
	"context"
	"fmt"
	"slices"

	"github.com/RoaringBitmap/roaring"
	"github.com/unisearch-db/unisearch/internal/index"
)

// BKD 2D over geo_point (§6.1): recursive median split alternating the
// dimension (lat / lon), leaves of at most LeafSize points. Inner nodes
// carry min/max bounding boxes in encoded fixed-point space; a geo
// distance query descends the tree, prunes every node whose bounding
// box cannot intersect the query circle's MBR, and applies the exact
// haversine check to surviving leaf points.

// geoPoint is one stored point: encoded lat/lon plus its row id.
type geoPoint struct {
	lat, lon int64
	row      int32
}

// geoNode is one node of the 2D tree. Node i covers points
// pts[node.lo : node.hi] of the point array; leaves have left < 0.
type geoNode struct {
	lo, hi         int32
	minLat, maxLat int64
	minLon, maxLon int64
	left, right    int32 // children ids; left < 0 marks a leaf
}

// geoTree is the immutable, concurrency-safe search structure.
type geoTree struct {
	pts   []geoPoint
	nodes []geoNode
}

// buildGeoTree recursively median-splits pts (a copy the builder owns)
// alternating lat/lon until runs of at most leafSize points remain.
// Node ids follow pre-order allocation.
func buildGeoTree(pts []geoPoint, leafSize int) (*geoTree, error) {
	if leafSize <= 0 {
		return nil, fmt.Errorf("scalar: leaf size %d", leafSize)
	}
	t := &geoTree{pts: pts}
	if len(pts) == 0 {
		return t, nil
	}
	t.nodes = make([]geoNode, 0, 2*len(pts)/leafSize+1)
	t.buildRange(0, int32(len(pts)), 0, leafSize)
	return t, nil
}

// buildRange builds the subtree over pts[lo:hi] splitting on depth%2
// (0 = lat, 1 = lon) and returns its node id.
func (t *geoTree) buildRange(lo, hi, depth int32, leafSize int) int32 {
	id := int32(len(t.nodes))
	// left = -1 marks a leaf; inner nodes overwrite it after recursing.
	// Without the marker a leaf's zero-valued children would point back
	// at the root and loop the search forever.
	t.nodes = append(t.nodes, geoNode{lo: lo, hi: hi, left: -1})
	if hi-lo > int32(leafSize) {
		if depth%2 == 0 {
			slices.SortFunc(t.pts[lo:hi], func(a, b geoPoint) int {
				if a.lat != b.lat {
					return cmpInt64(a.lat, b.lat)
				}
				if a.lon != b.lon {
					return cmpInt64(a.lon, b.lon)
				}
				return cmpInt64(int64(a.row), int64(b.row))
			})
		} else {
			slices.SortFunc(t.pts[lo:hi], func(a, b geoPoint) int {
				if a.lon != b.lon {
					return cmpInt64(a.lon, b.lon)
				}
				if a.lat != b.lat {
					return cmpInt64(a.lat, b.lat)
				}
				return cmpInt64(int64(a.row), int64(b.row))
			})
		}
		mid := lo + (hi-lo)/2
		left := t.buildRange(lo, mid, depth+1, leafSize)
		right := t.buildRange(mid, hi, depth+1, leafSize)
		// t.nodes may have been reallocated by the recursive calls; do
		// not hold a pointer across them.
		t.nodes[id].left = left
		t.nodes[id].right = right
	}
	// Bounding box over the range (O(size) per level, O(n log n) in
	// total). Computed after the children exist; re-index freshly.
	n := &t.nodes[id]
	n.minLat, n.maxLat = t.rangeLat(lo, hi)
	n.minLon, n.maxLon = t.rangeLon(lo, hi)
	return id
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func (t *geoTree) rangeLat(lo, hi int32) (int64, int64) {
	mn, mx := t.pts[lo].lat, t.pts[lo].lat
	for i := lo + 1; i < hi; i++ {
		if v := t.pts[i].lat; v < mn {
			mn = v
		} else if v > mx {
			mx = v
		}
	}
	return mn, mx
}

func (t *geoTree) rangeLon(lo, hi int32) (int64, int64) {
	mn, mx := t.pts[lo].lon, t.pts[lo].lon
	for i := lo + 1; i < hi; i++ {
		if v := t.pts[i].lon; v < mn {
			mn = v
		} else if v > mx {
			mx = v
		}
	}
	return mn, mx
}

// searchDistance returns the rows within q.RadiusM meters of
// (q.Lat, q.Lon). The MBR prunes whole subtrees; each surviving point
// gets the exact haversine check on its decoded coordinates. stats
// records rows touched (MBR + distance checks), distance computations
// and nodes visited so tests can verify pruning against a linear scan.
func (t *geoTree) searchDistance(ctx context.Context, q GeoDistanceQuery, budget index.SearchBudget, stats *Stats) (*roaring.Bitmap, error) {
	if isNaN(q.Lat) || isNaN(q.Lon) || isNaN(q.RadiusM) {
		return nil, fmt.Errorf("scalar: invalid geo query %+v", q)
	}
	if q.Lat < -90 || q.Lat > 90 {
		return nil, fmt.Errorf("scalar: query latitude %v out of range", q.Lat)
	}
	if q.Lon < -180 || q.Lon > 180 {
		return nil, fmt.Errorf("scalar: query longitude %v out of range", q.Lon)
	}
	if q.RadiusM < 0 {
		return nil, fmt.Errorf("scalar: negative radius %v", q.RadiusM)
	}
	out := roaring.New()
	if len(t.nodes) == 0 {
		return out, nil
	}
	// All geometry happens in quantized space (Lucene semantics): the
	// query center is encoded and decoded once, so a stored point equal
	// to the center matches a radius-0 query exactly.
	qcLat := decodeLat(encodeLat(q.Lat))
	qcLon := decodeLon(encodeLon(q.Lon))
	mbr := newCircleMBR(qcLat, qcLon, q.RadiusM)
	// Explicit-stack DFS keeps the descent iteration flat.
	stack := []int32{0}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := &t.nodes[id]
		stats.NodesVisited++
		// Bounding-box pruning: the subtree cannot contribute a match.
		if n.maxLat < mbr.latMin || n.minLat > mbr.latMax {
			continue
		}
		if !mbr.intersectsLon(n.minLon, n.maxLon) {
			continue
		}
		if n.left < 0 {
			// Leaf: MBR pre-check then exact haversine per point.
			for i := n.lo; i < n.hi; i++ {
				p := &t.pts[i]
				if !mbr.containsPoint(p.lat, p.lon) {
					continue
				}
				stats.RowsTouched++
				if err := touchBudget(budget, stats); err != nil {
					return nil, err
				}
				// Exact decision on the decoded fixed-point coordinates
				// (§6.1: distance filtering in the leaf).
				d := haversineMeters(qcLat, qcLon, decodeLat(p.lat), decodeLon(p.lon))
				stats.DistComps++
				if d <= q.RadiusM {
					out.Add(uint32(p.row))
				}
			}
			continue
		}
		stack = append(stack, n.left, n.right)
	}
	return out, nil
}

// touchBudget enforces the SearchBudget caps against the stats
// counters. Scalar indexes are exact filters, so exhaustion is an
// error, never a silent partial result.
func touchBudget(budget index.SearchBudget, stats *Stats) error {
	if budget.MaxVisited > 0 && stats.RowsTouched > budget.MaxVisited {
		return fmt.Errorf("%w: touched %d rows (cap %d)",
			ErrBudgetExhausted, stats.RowsTouched, budget.MaxVisited)
	}
	if budget.MaxDistComps > 0 && stats.DistComps > budget.MaxDistComps {
		return fmt.Errorf("%w: %d distance comps (cap %d)",
			ErrBudgetExhausted, stats.DistComps, budget.MaxDistComps)
	}
	return nil
}

func isNaN(f float64) bool { return f != f }

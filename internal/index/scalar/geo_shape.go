// Package scalar geo_shape.go implements the M3 geo_shape support
// (design 02-index-engine.md §6, 概要设计 §8.3). Extends BKD from
// point queries to polygon/linestring encoding and spatial predicates
// (intersects/within/disjoint).
// @spec: ./archive/2026-10-20-m3-ecosystem-advanced/02_spec.md#2.2
package scalar

import (
	"math"
	"sort"
)

// GeoShapeType enumerates supported shape types.
type GeoShapeType string

const (
	GeoShapePoint      GeoShapeType = "point"
	GeoShapeLineString GeoShapeType = "linestring"
	GeoShapePolygon    GeoShapeType = "polygon"
)

// GeoShape represents a geographic shape for indexing/querying.
type GeoShape struct {
	Type        GeoShapeType
	Coordinates [][][]float64 // [ring][point][lon,lat]
}

// BoundingBox is the min/max extent of a shape.
type BoundingBox struct {
	MinLon, MaxLon float64
	MinLat, MaxLat float64
}

// ComputeBBox computes the bounding box of a geo shape.
func ComputeBBox(shape GeoShape) BoundingBox {
	bb := BoundingBox{
		MinLon: math.Inf(1), MaxLon: math.Inf(-1),
		MinLat: math.Inf(1), MaxLat: math.Inf(-1),
	}
	for _, ring := range shape.Coordinates {
		for _, pt := range ring {
			if len(pt) < 2 {
				continue
			}
			lon, lat := pt[0], pt[1]
			if lon < bb.MinLon {
				bb.MinLon = lon
			}
			if lon > bb.MaxLon {
				bb.MaxLon = lon
			}
			if lat < bb.MinLat {
				bb.MinLat = lat
			}
			if lat > bb.MaxLat {
				bb.MaxLat = lat
			}
		}
	}
	return bb
}

// BBoxIntersects checks if two bounding boxes intersect.
func BBoxIntersects(a, b BoundingBox) bool {
	return a.MinLon <= b.MaxLon && a.MaxLon >= b.MinLon &&
		a.MinLat <= b.MaxLat && a.MaxLat >= b.MinLat
}

// BBoxContains checks if bounding box a contains point (lon, lat).
func BBoxContains(bb BoundingBox, lon, lat float64) bool {
	return lon >= bb.MinLon && lon <= bb.MaxLon &&
		lat >= bb.MinLat && lat <= bb.MaxLat
}

// PointInPolygon checks if point (lon, lat) is inside a polygon ring.
func PointInPolygon(polygon [][]float64, lon, lat float64) bool {
	n := len(polygon)
	if n < 3 {
		return false
	}
	inside := false
	j := n - 1
	for i := 0; i < n; i++ {
		xi, yi := polygon[i][0], polygon[i][1]
		xj, yj := polygon[j][0], polygon[j][1]
		if ((yi > lat) != (yj > lat)) &&
			(lon < (xj-xi)*(lat-yi)/(yj-yi)+xi) {
			inside = !inside
		}
		j = i
	}
	return inside
}

// ShapesIntersect checks if two geo shapes spatially intersect.
// Uses bounding box pre-filter + precise geometry for point-in-polygon,
// line-polygon, and polygon-polygon intersection.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#geo_shape
func ShapesIntersect(a, b GeoShape) bool {
	bbA := ComputeBBox(a)
	bbB := ComputeBBox(b)
	if !BBoxIntersects(bbA, bbB) {
		return false
	}
	// Precise geometric intersection based on shape types.
	return preciseIntersect(a, b)
}

// preciseIntersect performs exact geometric intersection after
// the bounding box pre-filter has passed.
func preciseIntersect(a, b GeoShape) bool {
	// Point vs any: check if point is inside the other shape.
	if a.Type == GeoShapePoint && len(a.Coordinates) > 0 && len(a.Coordinates[0]) > 0 {
		pt := a.Coordinates[0][0]
		if len(pt) < 2 {
			return false
		}
		return pointInShape(pt[0], pt[1], b)
	}
	if b.Type == GeoShapePoint && len(b.Coordinates) > 0 && len(b.Coordinates[0]) > 0 {
		pt := b.Coordinates[0][0]
		if len(pt) < 2 {
			return false
		}
		return pointInShape(pt[0], pt[1], a)
	}
	// Polygon vs Polygon: check point-in-polygon + edge intersection.
	if a.Type == GeoShapePolygon && b.Type == GeoShapePolygon {
		return polygonsIntersect(a, b)
	}
	// LineString vs Polygon: check if any line segment crosses the polygon.
	if a.Type == GeoShapeLineString && b.Type == GeoShapePolygon {
		return linePolygonIntersect(a, b)
	}
	if b.Type == GeoShapeLineString && a.Type == GeoShapePolygon {
		return linePolygonIntersect(b, a)
	}
	// LineString vs LineString: check segment intersection.
	if a.Type == GeoShapeLineString && b.Type == GeoShapeLineString {
		return linesIntersect(a, b)
	}
	// Fallback: bounding box intersection (already confirmed).
	return true
}

// pointInShape checks if point (lon, lat) is inside the shape.
func pointInShape(lon, lat float64, shape GeoShape) bool {
	switch shape.Type {
	case GeoShapePoint:
		if len(shape.Coordinates) > 0 && len(shape.Coordinates[0]) > 0 {
			pt := shape.Coordinates[0][0]
			return len(pt) >= 2 && pt[0] == lon && pt[1] == lat
		}
	case GeoShapeLineString:
		// Check if point lies on any line segment.
		if len(shape.Coordinates) > 0 {
			pts := shape.Coordinates[0]
			for i := 0; i < len(pts)-1; i++ {
				if pointOnSegment(lon, lat, pts[i][0], pts[i][1], pts[i+1][0], pts[i+1][1]) {
					return true
				}
			}
		}
	case GeoShapePolygon:
		// Check if point is inside any ring of the polygon.
		for _, ring := range shape.Coordinates {
			if PointInPolygon(ring, lon, lat) {
				return true
			}
		}
	}
	return false
}

// polygonsIntersect checks if two polygons intersect by testing
// point-in-polygon (both directions) and edge intersection.
func polygonsIntersect(a, b GeoShape) bool {
	// Check if any vertex of A is inside B.
	for _, ring := range a.Coordinates {
		for _, pt := range ring {
			if len(pt) >= 2 && pointInShape(pt[0], pt[1], b) {
				return true
			}
		}
	}
	// Check if any vertex of B is inside A.
	for _, ring := range b.Coordinates {
		for _, pt := range ring {
			if len(pt) >= 2 && pointInShape(pt[0], pt[1], a) {
				return true
			}
		}
	}
	// Check edge intersection between all ring pairs.
	for _, ringA := range a.Coordinates {
		for _, ringB := range b.Coordinates {
			if ringsEdgeIntersect(ringA, ringB) {
				return true
			}
		}
	}
	return false
}

// linePolygonIntersect checks if a linestring crosses a polygon.
func linePolygonIntersect(line, poly GeoShape) bool {
	// Check if any line endpoint is inside the polygon.
	if len(line.Coordinates) > 0 {
		pts := line.Coordinates[0]
		for _, pt := range pts {
			if len(pt) >= 2 && pointInShape(pt[0], pt[1], poly) {
				return true
			}
		}
		// Check if any line segment crosses any polygon edge.
		for _, ring := range poly.Coordinates {
			if ringsEdgeIntersect(pts, ring) {
				return true
			}
		}
	}
	return false
}

// linesIntersect checks if two linestrings have crossing segments.
func linesIntersect(a, b GeoShape) bool {
	if len(a.Coordinates) == 0 || len(b.Coordinates) == 0 {
		return false
	}
	return ringsEdgeIntersect(a.Coordinates[0], b.Coordinates[0])
}

// ringsEdgeIntersect checks if any edge of ringA crosses any edge of ringB.
func ringsEdgeIntersect(ringA, ringB [][]float64) bool {
	if len(ringA) < 2 || len(ringB) < 2 {
		return false
	}
	for i := 0; i < len(ringA)-1; i++ {
		x1, y1 := ringA[i][0], ringA[i][1]
		x2, y2 := ringA[i+1][0], ringA[i+1][1]
		for j := 0; j < len(ringB)-1; j++ {
			x3, y3 := ringB[j][0], ringB[j][1]
			x4, y4 := ringB[j+1][0], ringB[j+1][1]
			if segmentsCross(x1, y1, x2, y2, x3, y3, x4, y4) {
				return true
			}
		}
	}
	return false
}

// segmentsCross checks if segment (x1,y1)-(x2,y2) crosses (x3,y3)-(x4,y4).
func segmentsCross(x1, y1, x2, y2, x3, y3, x4, y4 float64) bool {
	d1 := cross(x3, y3, x4, y4, x1, y1)
	d2 := cross(x3, y3, x4, y4, x2, y2)
	d3 := cross(x1, y1, x2, y2, x3, y3)
	d4 := cross(x1, y1, x2, y2, x4, y4)
	if ((d1 > 0 && d2 < 0) || (d1 < 0 && d2 > 0)) &&
		((d3 > 0 && d4 < 0) || (d3 < 0 && d4 > 0)) {
		return true
	}
	// Collinear/degenerate cases.
	if d1 == 0 && onSegment(x3, y3, x4, y4, x1, y1) {
		return true
	}
	if d2 == 0 && onSegment(x3, y3, x4, y4, x2, y2) {
		return true
	}
	if d3 == 0 && onSegment(x1, y1, x2, y2, x3, y3) {
		return true
	}
	if d4 == 0 && onSegment(x1, y1, x2, y2, x4, y4) {
		return true
	}
	return false
}

// cross computes the cross product of vectors (x3-x1, y3-y1) and (x4-x2, y4-y2).
func cross(x1, y1, x2, y2, x3, y3 float64) float64 {
	return (x3-x1)*(y2-y1) - (x2-x1)*(y3-y1)
}

// onSegment checks if point (px, py) lies on segment (x1,y1)-(x2,y2).
func onSegment(x1, y1, x2, y2, px, py float64) bool {
	return min(x1, x2) <= px && px <= max(x1, x2) &&
		min(y1, y2) <= py && py <= max(y1, y2)
}

// pointOnSegment checks if point (px, py) lies on segment (x1,y1)-(x2,y2).
func pointOnSegment(px, py, x1, y1, x2, y2 float64) bool {
	if cross(x1, y1, x2, y2, px, py) != 0 {
		return false
	}
	return onSegment(x1, y1, x2, y2, px, py)
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// EncodeGeoShape serializes a shape for BKD indexing.
func EncodeGeoShape(shape GeoShape) []byte {
	bb := ComputeBBox(shape)
	// Encode as 4 float64 values (min/max lon/lat).
	out := make([]byte, 32)
	bits := math.Float64bits(bb.MinLon)
	for i := 0; i < 8; i++ {
		out[i] = byte(bits >> (56 - i*8))
	}
	bits = math.Float64bits(bb.MaxLon)
	for i := 0; i < 8; i++ {
		out[8+i] = byte(bits >> (56 - i*8))
	}
	bits = math.Float64bits(bb.MinLat)
	for i := 0; i < 8; i++ {
		out[16+i] = byte(bits >> (56 - i*8))
	}
	bits = math.Float64bits(bb.MaxLat)
	for i := 0; i < 8; i++ {
		out[24+i] = byte(bits >> (56 - i*8))
	}
	return out
}

// GeoShapeQueryType enumerates spatial predicates.
type GeoShapeQueryType string

const (
	GeoQueryIntersects GeoShapeQueryType = "intersects"
	GeoQueryWithin     GeoShapeQueryType = "within"
	GeoQueryDisjoint   GeoShapeQueryType = "disjoint"
)

// QueryGeoShape filters a set of indexed shapes by spatial predicate.
func QueryGeoShape(predicate GeoShapeQueryType, queryShape GeoShape, indexedShapes []GeoShape) []int {
	var results []int
	for i, shape := range indexedShapes {
		intersects := ShapesIntersect(queryShape, shape)
		switch predicate {
		case GeoQueryIntersects:
			if intersects {
				results = append(results, i)
			}
		case GeoQueryWithin:
			bbQ := ComputeBBox(queryShape)
			bbS := ComputeBBox(shape)
			if bbS.MinLon >= bbQ.MinLon && bbS.MaxLon <= bbQ.MaxLon &&
				bbS.MinLat >= bbQ.MinLat && bbS.MaxLat <= bbQ.MaxLat {
				results = append(results, i)
			}
		case GeoQueryDisjoint:
			if !intersects {
				results = append(results, i)
			}
		}
	}
	sort.Ints(results)
	return results
}

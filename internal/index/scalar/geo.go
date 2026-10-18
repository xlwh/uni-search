package scalar

import "math"

// Geo fixed-point encoding (§6.1 "geo_point interleave 定点"): lat and
// lon degrees map monotonically onto int64 [0, 2^31), ~0.009 m per step
// at the equator. Encoding is monotone so encoded-space interval checks
// (MBR pruning) are exactly the decoded-space checks.
const (
	geoCoordMax = float64(1<<31 - 1)
	geoLatScale = geoCoordMax / 180.0 // over [-90, 90]
	geoLonScale = geoCoordMax / 360.0 // over [-180, 180]
)

// encodeLat maps a latitude in degrees to its fixed-point int64.
// Out-of-range inputs clamp to the representable ends; NaN clamps to 0
// (the build rejects NaN rows outright).
func encodeLat(lat float64) int64 {
	v := int64(math.Round((lat + 90) * geoLatScale))
	return clamp64(v, 0, int64(geoCoordMax))
}

// encodeLon maps a longitude in degrees to its fixed-point int64.
func encodeLon(lon float64) int64 {
	v := int64(math.Round((lon + 180) * geoLonScale))
	return clamp64(v, 0, int64(geoCoordMax))
}

// decodeLat is the inverse of encodeLat (up to one encoding step,
// ~5 mm).
func decodeLat(v int64) float64 { return float64(v)/geoLatScale - 90 }

// decodeLon is the inverse of encodeLon.
func decodeLon(v int64) float64 { return float64(v)/geoLonScale - 180 }

func clamp64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// earthRadiusM is the mean Earth radius used by the haversine kernel
// (WGS-84 mean radius; ES/Lucene-compatible semantics).
const earthRadiusM = 6371008.8

// haversineMeters computes the great-circle distance between two
// (lat, lon) pairs in degrees, in meters.
func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	rad := math.Pi / 180
	sinDLat := math.Sin((lat2 - lat1) * rad * 0.5)
	sinDLon := math.Sin((lon2 - lon1) * rad * 0.5)
	c1 := math.Cos(lat1 * rad)
	c2 := math.Cos(lat2 * rad)
	h := sinDLat*sinDLat + c1*c2*sinDLon*sinDLon
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(h)))
}

// circleMBR is the axis-aligned bounding box (in encoded int64 space)
// of a geo distance query circle, precomputed once per query. It is a
// pure pre-filter: every row inside the circle is inside the MBR, so
// pruning with it never loses a match; the exact decision is the
// haversine check on candidates.
type circleMBR struct {
	latMin, latMax int64
	// allLon marks lon-unconstrained circles (near-pole or huge
	// radius); otherwise lon matches iff lonEnc >= lonLowEnc (in
	// [lonLow, 180]) or lonEnc <= lonHighEnc (in [-180, lonHigh]) —
	// the second interval only when wraps, crossing the antimeridian.
	allLon     bool
	wraps      bool
	lonLowEnc  int64 // encoded lower bound of the primary interval
	lonHighEnc int64 // encoded upper bound of the wrapped interval
}

// newCircleMBR computes the MBR of the circle (lat, lon, radiusM).
// Spherical-safe bounds: any point within angular distance r of the
// center differs in latitude by at most r, and — while the circle does
// not swallow a pole — in longitude by at most
// asin(sin r / cos centerLat) (the small-circle tangency bound, same
// construction as Lucene's LatLonPoint). A circle that reaches a pole
// (or spans the whole sphere) covers every longitude; a circle wider
// than a meridian quadrant always does.
func newCircleMBR(lat, lon, radiusM float64) circleMBR {
	r := radiusM / earthRadiusM // angular radius (radians)
	if r >= math.Pi {
		return circleMBR{latMin: 0, latMax: int64(geoCoordMax), allLon: true}
	}
	dLatDeg := r * 180 / math.Pi
	m := circleMBR{
		latMin: encodeLat(lat - dLatDeg),
		latMax: encodeLat(lat + dLatDeg),
	}
	if lat-dLatDeg <= -90 || lat+dLatDeg >= 90 {
		// The circle swallows a pole: every longitude can match.
		m.allLon = true
		return m
	}
	sinR := math.Sin(r)
	cosLat := math.Cos(lat * math.Pi / 180)
	if cosLat <= 0 || sinR >= cosLat {
		// Safety net: asin would leave its domain — treat as full
		// longitude coverage.
		m.allLon = true
		return m
	}
	dLon := math.Asin(sinR/cosLat) * 180 / math.Pi
	lonMin, lonMax := lon-dLon, lon+dLon
	switch {
	case lonMax-lonMin >= 360:
		m.allLon = true
	case lonMin < -180:
		// [lonMin+360, 180] ∪ [-180, lonMax]
		m.lonLowEnc = encodeLon(lonMin + 360)
		m.lonHighEnc = encodeLon(lonMax)
		m.wraps = true
	case lonMax > 180:
		// [lonMin, 180] ∪ [-180, lonMax-360]
		m.lonLowEnc = encodeLon(lonMin)
		m.lonHighEnc = encodeLon(lonMax - 360)
		m.wraps = true
	default:
		m.lonLowEnc = encodeLon(lonMin)
		m.lonHighEnc = encodeLon(lonMax)
	}
	return m
}

// containsPoint reports whether the encoded point can be inside the
// circle (necessary condition only). Longitude coverage is
// [lonLowEnc, lonHighEnc] when the circle does not wrap the
// antimeridian, and [lonLowEnc, max] ∪ [0, lonHighEnc] when it wraps.
func (m circleMBR) containsPoint(latEnc, lonEnc int64) bool {
	if latEnc < m.latMin || latEnc > m.latMax {
		return false
	}
	if m.allLon {
		return true
	}
	if m.wraps {
		return lonEnc >= m.lonLowEnc || lonEnc <= m.lonHighEnc
	}
	return lonEnc >= m.lonLowEnc && lonEnc <= m.lonHighEnc
}

// intersectsLon reports whether the encoded longitude range
// [lonMinEnc, lonMaxEnc] intersects the MBR's longitude coverage.
func (m circleMBR) intersectsLon(lonMinEnc, lonMaxEnc int64) bool {
	if m.allLon {
		return true
	}
	if m.wraps {
		return lonMaxEnc >= m.lonLowEnc || lonMinEnc <= m.lonHighEnc
	}
	return lonMaxEnc >= m.lonLowEnc && lonMinEnc <= m.lonHighEnc
}

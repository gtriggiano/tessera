// Package area holds the areas of an area set: the polygons that Read takes
// from a GeoJSON document, and the lookup of a point in them.
//
// A copy is never changed once built. Its polygons are kept in flat arrays:
// the vertices of every ring, the boundaries of the rings and of the
// polygons, and the bounding box of each ring. A static R-tree, packed along
// a Hilbert curve, indexes the boxes of the polygons, and each ring of at
// least bandVertices vertices has horizontal bands that list the edges
// crossing each of them, so that a lookup tests only the edges near the
// point. A lookup allocates nothing.
package area

import (
	"math"
	"slices"
)

// point is a vertex: x is its longitude, y its latitude, in degrees.
type point struct{ x, y float64 }

// box is a bounding box, its edges included.
type box struct{ minX, minY, maxX, maxY float64 }

// emptyBox holds no point; extend makes it the box of its first point.
var emptyBox = box{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}

func (b box) contains(x, y float64) bool {
	return x >= b.minX && x <= b.maxX && y >= b.minY && y <= b.maxY
}

func (b box) extend(x, y float64) box {
	return box{min(b.minX, x), min(b.minY, y), max(b.maxX, x), max(b.maxY, y)}
}

func (b box) union(o box) box {
	return box{min(b.minX, o.minX), min(b.minY, o.minY), max(b.maxX, o.maxX), max(b.maxY, o.maxY)}
}

// Polygons is an immutable copy of the polygons of an area set. It is safe
// for concurrent use.
type Polygons struct {
	// points are the vertices of every ring, without the closing one: ring r
	// is points[rings[r]:rings[r+1]].
	points []point
	rings  []uint32
	// boxes holds the bounding box of each ring.
	boxes []box
	// Polygon p has the rings polygons[p] to polygons[p+1]-1: the first is
	// its exterior, the others its holes.
	polygons []uint32
	// bandOf is the index in bands of the bands of each ring, or -1 for a
	// ring without them.
	bandOf []int32
	bands  []bands
	tree   rtree
}

// Len returns the number of polygons: a Polygon counts one, a MultiPolygon
// one for each of its polygons. A nil Polygons has none.
func (a *Polygons) Len() int {
	if a == nil || len(a.polygons) == 0 {
		return 0
	}
	return len(a.polygons) - 1
}

// Contains reports whether the point at latitude lat and longitude lon is in
// a polygon: inside its exterior ring and outside each of its holes, with
// the even-odd rule. The boundary is half-open: a point exactly on an edge is
// inside or outside, always the same for the same point, and a point on an
// edge that two polygons share is in exactly one of them. A nil Polygons
// contains nothing.
func (a *Polygons) Contains(lat, lon float64) bool {
	if a == nil {
		return false
	}
	x, y := lon, lat
	t := &a.tree
	n := len(t.boxes)
	if n == 0 || !t.boxes[n-1].contains(x, y) {
		return false
	}
	// A depth-first search of the nodes whose box holds the point. Each
	// level below the root adds at most nodeSize-1 entries to the stack,
	// and a tree of the largest set has fewer than ten levels.
	var stack [256]int32
	stack[0] = int32(n - 1)
	sp := 1
	for sp > 0 {
		sp--
		pos := int(stack[sp])
		level := t.levelOf(pos)
		if level == 0 {
			if a.polygonContains(t.items[pos], x, y) {
				return true
			}
			continue
		}
		first := int(t.items[pos])
		end := min(first+nodeSize, t.levels[level-1])
		for c := first; c < end; c++ {
			if !t.boxes[c].contains(x, y) {
				continue
			}
			if level == 1 {
				if a.polygonContains(t.items[c], x, y) {
					return true
				}
				continue
			}
			stack[sp] = int32(c)
			sp++
		}
	}
	return false
}

// polygonContains reports whether (x, y), in the box of the exterior ring of
// polygon p, is in the polygon.
func (a *Polygons) polygonContains(p uint32, x, y float64) bool {
	first, end := a.polygons[p], a.polygons[p+1]
	if !a.ringContains(first, x, y) {
		return false
	}
	for r := first + 1; r < end; r++ {
		if a.boxes[r].contains(x, y) && a.ringContains(r, x, y) {
			return false
		}
	}
	return true
}

// ringContains reports whether (x, y), in the box of ring r, is inside the
// ring, with the even-odd rule.
func (a *Polygons) ringContains(r uint32, x, y float64) bool {
	v := a.points[a.rings[r]:a.rings[r+1]]
	if b := a.bandOf[r]; b >= 0 {
		return a.bands[b].contains(v, x, y)
	}
	in := false
	j := len(v) - 1
	for i := range v {
		if crosses(v[i], v[j], x, y) {
			in = !in
		}
		j = i
	}
	return in
}

// crosses reports whether the ray from (x, y) towards growing longitudes
// crosses the edge from a to b, with the half-open rule: the edge counts when
// one end is above y and the other at or below it, and the point is strictly
// on its left, seen from its lower end. The edge is always taken from its
// lower end, so that the same edge in two rings gives the same answer, and
// the conversions keep each product rounded on its own, as the Go
// specification requires of an explicit conversion, so that no fused
// multiply-add changes the answer from one call site to another.
func crosses(a, b point, x, y float64) bool {
	if (a.y > y) == (b.y > y) {
		return false
	}
	if a.y > b.y {
		a, b = b, a
	}
	return float64((b.x-a.x)*(y-a.y)) > float64((x-a.x)*(b.y-a.y))
}

// index builds the R-tree and the bands of a, whose rings and polygons are
// complete.
func (a *Polygons) index() {
	n := a.Len()
	boxes := make([]box, n)
	for p := range n {
		boxes[p] = a.boxes[a.polygons[p]]
	}
	a.tree = buildTree(boxes)
	a.bandOf = make([]int32, len(a.rings)-1)
	for r := range a.bandOf {
		a.bandOf[r] = -1
		v := a.points[a.rings[r]:a.rings[r+1]]
		if len(v) >= bandVertices {
			a.bandOf[r] = int32(len(a.bands))
			a.bands = append(a.bands, buildBands(v, a.boxes[r]))
		}
	}
}

// nodeSize is the number of children of a node of the R-tree.
const nodeSize = 16

// rtree is a static R-tree, packed along a Hilbert curve: the boxes of every
// level in one array, the leaves first and the root last.
type rtree struct {
	boxes []box
	// items holds, for a leaf, its polygon, and for a node, the position of
	// its first child.
	items []uint32
	// levels holds the end of each level in boxes, the leaves first.
	levels []int
}

// levelOf returns the level of the entry at pos: 0 for a leaf.
func (t *rtree) levelOf(pos int) int {
	for level, end := range t.levels {
		if pos < end {
			return level
		}
	}
	return len(t.levels) - 1
}

// buildTree builds the tree of the polygons whose boxes are items.
func buildTree(items []box) rtree {
	n := len(items)
	if n == 0 {
		return rtree{}
	}
	extent := items[0]
	for _, b := range items[1:] {
		extent = extent.union(b)
	}
	width, height := extent.maxX-extent.minX, extent.maxY-extent.minY
	type keyed struct {
		key  uint64
		item uint32
	}
	order := make([]keyed, n)
	for i, b := range items {
		var hx, hy uint32
		if width > 0 {
			hx = uint32(math.Floor(hilbertMax * ((b.minX+b.maxX)/2 - extent.minX) / width))
		}
		if height > 0 {
			hy = uint32(math.Floor(hilbertMax * ((b.minY+b.maxY)/2 - extent.minY) / height))
		}
		order[i] = keyed{hilbert(hx, hy), uint32(i)}
	}
	slices.SortFunc(order, func(a, b keyed) int {
		switch {
		case a.key < b.key:
			return -1
		case a.key > b.key:
			return 1
		}
		return int(a.item) - int(b.item)
	})
	total := n
	for m := n; m > 1; {
		m = (m + nodeSize - 1) / nodeSize
		total += m
	}
	t := rtree{boxes: make([]box, 0, total), items: make([]uint32, 0, total)}
	for _, o := range order {
		t.boxes = append(t.boxes, items[o.item])
		t.items = append(t.items, o.item)
	}
	t.levels = append(t.levels, n)
	for first, end := 0, n; end-first > 1; first, end = end, len(t.boxes) {
		for i := first; i < end; i += nodeSize {
			u := t.boxes[i]
			for c := i + 1; c < min(i+nodeSize, end); c++ {
				u = u.union(t.boxes[c])
			}
			t.boxes = append(t.boxes, u)
			t.items = append(t.items, uint32(i))
		}
		t.levels = append(t.levels, len(t.boxes))
	}
	return t
}

// hilbertOrder is the order of the Hilbert curve that sorts the leaves: a
// grid of 2^16 by 2^16 cells.
const hilbertOrder = 16

// hilbertMax is the largest coordinate of a cell of the grid.
const hilbertMax = 1<<hilbertOrder - 1

// hilbert returns the distance of the cell (x, y) along the Hilbert curve
// that covers the grid.
func hilbert(x, y uint32) uint64 {
	var d uint64
	for s := uint32(1 << (hilbertOrder - 1)); s > 0; s /= 2 {
		var rx, ry uint32
		if x&s != 0 {
			rx = 1
		}
		if y&s != 0 {
			ry = 1
		}
		d += uint64(s) * uint64(s) * uint64((3*rx)^ry)
		// Rotate the quadrant, so that the curve stays continuous: the
		// next steps read the lower bits only.
		if ry == 0 {
			if rx == 1 {
				x, y = hilbertMax-x, hilbertMax-y
			}
			x, y = y, x
		}
	}
	return d
}

// bandVertices is the number of vertices from which a ring has bands.
const bandVertices = 256

// bands divide the box of one ring into horizontal bands of equal height,
// and list, for each band, the edges whose latitudes meet it. Edge e goes
// from vertex e to vertex e+1, the last one back to the first.
type bands struct {
	minY, scale float64 // band(y) is (y - minY) * scale
	count       int
	// The edges of band b are edges[start[b]:start[b+1]].
	start []uint32
	edges []uint32
}

// band returns the band of latitude y, in the box of the ring.
func (b *bands) band(y float64) int {
	i := int((y - b.minY) * b.scale)
	return max(0, min(i, b.count-1))
}

// buildBands builds the bands of the ring of vertices v, in the box bx: one
// band for four vertices, or fewer when long edges would be listed in more
// than four bands each on average.
func buildBands(v []point, bx box) bands {
	n := len(v)
	for count := max(1, n/4); ; count /= 2 {
		b := bands{minY: bx.minY, count: count}
		if height := bx.maxY - bx.minY; height > 0 && count > 1 {
			b.scale = float64(count) / height
		} else {
			b.count = 1
		}
		span := func(e int) (int, int) {
			p, q := v[e], v[(e+1)%n]
			return b.band(min(p.y, q.y)), b.band(max(p.y, q.y))
		}
		total := 0
		for e := range n {
			lo, hi := span(e)
			total += hi - lo + 1
		}
		if total > 4*n && b.count > 1 {
			continue
		}
		b.start = make([]uint32, b.count+1)
		for e := range n {
			lo, hi := span(e)
			for i := lo; i <= hi; i++ {
				b.start[i+1]++
			}
		}
		for i := 1; i <= b.count; i++ {
			b.start[i] += b.start[i-1]
		}
		b.edges = make([]uint32, total)
		fill := slices.Clone(b.start[:b.count])
		for e := range n {
			lo, hi := span(e)
			for i := lo; i <= hi; i++ {
				b.edges[fill[i]] = uint32(e)
				fill[i]++
			}
		}
		return b
	}
}

// contains reports whether (x, y), in the box of the ring of vertices v, is
// inside it: as ringContains, with the edges of the band of y only. An edge
// that the ray can cross has y between the latitudes of its ends, so it is
// listed in the band of y.
func (b *bands) contains(v []point, x, y float64) bool {
	in := false
	n := uint32(len(v))
	i := b.band(y)
	for _, e := range b.edges[b.start[i]:b.start[i+1]] {
		next := e + 1
		if next == n {
			next = 0
		}
		if crosses(v[e], v[next], x, y) {
			in = !in
		}
	}
	return in
}

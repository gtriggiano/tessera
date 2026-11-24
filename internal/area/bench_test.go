package area

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strconv"
	"testing"
	"time"
)

// benchSet is a GeoJSON document of the benchmarks, with the number of its
// polygons and of their vertices, closing positions excluded.
type benchSet struct {
	name               string
	doc                []byte
	polygons, vertices int
}

// appendRing appends ring to dst as the coordinates of a GeoJSON ring, with
// six decimals, as exports of GIS tools write them, closed with its first
// position.
func appendRing(dst []byte, ring []point) []byte {
	dst = append(dst, '[')
	for i := 0; i <= len(ring); i++ {
		p := ring[i%len(ring)]
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '[')
		dst = strconv.AppendFloat(dst, p.x, 'f', 6, 64)
		dst = append(dst, ',')
		dst = strconv.AppendFloat(dst, p.y, 'f', 6, 64)
		dst = append(dst, ']')
	}
	return append(dst, ']')
}

// circleRing returns a ring of n vertices around the latitude lat and the
// longitude lon, of radius km kilometers, counterclockwise, or clockwise for
// a hole; jitter, when set, scales the radius at each angle.
func circleRing(lat, lon, km float64, n int, hole bool, jitter func(angle float64) float64) []point {
	dLat := km / 111.32
	dLon := km / (111.32 * math.Cos(lat*math.Pi/180))
	ring := make([]point, n)
	for i := range ring {
		angle := 2 * math.Pi * float64(i) / float64(n)
		if hole {
			angle = -angle
		}
		scale := 1.0
		if jitter != nil {
			scale = jitter(angle)
		}
		ring[i] = point{lon + scale*dLon*math.Cos(angle), lat + scale*dLat*math.Sin(angle)}
	}
	return ring
}

// geofences returns a FeatureCollection of n geofences around places in
// Europe, the same ones at every run: circles of 0.5 to 5 km of radius, with
// 32 to 64 vertices, of which one in fifty has a hole of 32 vertices, each
// feature with a name in its properties, as a store locator would export
// them.
func geofences(n int) benchSet {
	r := rand.New(rand.NewPCG(2026, 31))
	s := benchSet{name: fmt.Sprintf("geofences=%d", n), polygons: n}
	doc := []byte(`{"type":"FeatureCollection","features":[` + "\n")
	for i := range n {
		lat, lon := 36+r.Float64()*24, -10+r.Float64()*40
		km := 0.5 + r.Float64()*4.5
		if i > 0 {
			doc = append(doc, ",\n"...)
		}
		doc = fmt.Appendf(doc, `{"type":"Feature","properties":{"name":"store-%d"},"geometry":{"type":"Polygon","coordinates":[`, i+1)
		exterior := circleRing(lat, lon, km, 32+r.IntN(33), false, nil)
		doc = appendRing(doc, exterior)
		s.vertices += len(exterior)
		if r.IntN(50) == 0 {
			hole := circleRing(lat, lon, km*0.3, 32, true, nil)
			doc = appendRing(append(doc, ','), hole)
			s.vertices += len(hole)
		}
		doc = append(doc, "]}}"...)
	}
	s.doc = append(doc, "\n]}\n"...)
	return s
}

// regions returns a FeatureCollection of n large polygons around places of
// the world, the same ones at every run, as regions or countries drawn in
// detail: 8,192 to 32,768 vertices each, at 1.5 to 6 degrees of latitude
// from their center, with a radius that changes along the border and from
// one vertex to the next, as a coast does.
func regions(n int) benchSet {
	r := rand.New(rand.NewPCG(2026, 32))
	s := benchSet{name: fmt.Sprintf("regions=%d", n), polygons: n}
	doc := []byte(`{"type":"FeatureCollection","features":[` + "\n")
	for i := range n {
		lat, lon := -50+r.Float64()*110, -170+r.Float64()*340
		km := (1.5 + r.Float64()*4.5) * 111.32
		phase, lobes := r.Float64()*2*math.Pi, float64(3+r.IntN(5))
		jitter := func(angle float64) float64 {
			return 0.8 + 0.15*math.Sin(lobes*angle+phase) + 0.05*r.Float64()
		}
		if i > 0 {
			doc = append(doc, ",\n"...)
		}
		doc = fmt.Appendf(doc, `{"type":"Feature","properties":{"name":"region-%d"},"geometry":{"type":"Polygon","coordinates":[`, i+1)
		ring := circleRing(lat, lon, km, 8192+r.IntN(24577), false, jitter)
		doc = appendRing(doc, ring)
		s.vertices += len(ring)
		doc = append(doc, "]}}"...)
	}
	s.doc = append(doc, "\n]}\n"...)
	return s
}

// benchSets make the documents of the benchmarks, one at a time: 10,000 and
// 100,000 geofences, and 16 regions.
var benchSets = []func() benchSet{
	func() benchSet { return geofences(10_000) },
	func() benchSet { return geofences(100_000) },
	func() benchSet { return regions(16) },
}

// readBench reads the document of s from r, and checks its polygons and
// vertices.
func readBench(b *testing.B, s benchSet, r io.Reader) *Polygons {
	b.Helper()
	a, err := Read(r, 1_000_000)
	if err != nil {
		b.Fatalf("%s: %v", s.name, err)
	}
	if a.Len() != s.polygons || len(a.points) != s.vertices {
		b.Fatalf("%s: %d polygons and %d vertices, want %d and %d", s.name, a.Len(), len(a.points), s.polygons, s.vertices)
	}
	return a
}

// probes returns n points, the same ones at every run, for which keep
// returns true: in the box of a polygon of a drawn at random when inBox, or
// anywhere in the box of all the polygons.
func probes(a *Polygons, n int, seed uint64, inBox bool, keep func(lat, lon float64) bool) [][2]float64 {
	r := rand.New(rand.NewPCG(2026, seed))
	extent := a.tree.boxes[len(a.tree.boxes)-1]
	points := make([][2]float64, 0, n)
	for len(points) < n {
		bx := extent
		if inBox {
			bx = a.boxes[a.polygons[r.IntN(a.Len())]]
		}
		lat := bx.minY + r.Float64()*(bx.maxY-bx.minY)
		lon := bx.minX + r.Float64()*(bx.maxX-bx.minX)
		if keep(lat, lon) {
			points = append(points, [2]float64{lat, lon})
		}
	}
	return points
}

// BenchmarkContains looks up points in the areas of each document: points
// in a polygon (inside); points in the bounding box of a polygon but in no
// polygon (near), the most work for one polygon, since every edge near the
// point is tested; and points anywhere in the bounding box of all the
// polygons, in none (outside). Each case cycles through 65,536 points.
func BenchmarkContains(b *testing.B) {
	for _, set := range benchSets {
		s := set()
		a := readBench(b, s, bytes.NewReader(s.doc))
		const n = 1 << 16
		in := func(lat, lon float64) bool { return a.Contains(lat, lon) }
		out := func(lat, lon float64) bool { return !a.Contains(lat, lon) }
		for _, c := range []struct {
			name   string
			inside bool
			points [][2]float64
		}{
			{"inside", true, probes(a, n, 1, true, in)},
			{"near", false, probes(a, n, 2, true, out)},
			{"outside", false, probes(a, n, 3, false, out)},
		} {
			b.Run(s.name+"/"+c.name, func(b *testing.B) {
				b.ReportAllocs()
				found, i := 0, 0
				for b.Loop() {
					p := c.points[i&(n-1)]
					if a.Contains(p[0], p[1]) {
						found++
					}
					i++
				}
				if c.inside && found != i || !c.inside && found != 0 {
					b.Fatalf("%d of %d points found", found, i)
				}
			})
		}
	}
}

// liveHeap returns the bytes of the heap held by reachable objects, measured
// after a garbage collection, as the benchmarks of internal/sets do.
func liveHeap() uint64 {
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// heapObjects returns the bytes of the heap held by objects, after a garbage
// collection, as peakHeap reads them.
func heapObjects() uint64 {
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// peakHeap runs f, and returns the largest heap held by objects while it
// ran, unreachable ones not yet freed included, read every 100 µs.
func peakHeap(f func()) uint64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	stop, peak := make(chan struct{}), make(chan uint64)
	go func() {
		ticker := time.NewTicker(100 * time.Microsecond)
		defer ticker.Stop()
		var largest uint64
		for {
			metrics.Read(sample)
			largest = max(largest, sample[0].Value.Uint64())
			select {
			case <-stop:
				peak <- largest
				return
			case <-ticker.C:
			}
		}
	}()
	f()
	close(stop)
	return <-peak
}

// BenchmarkRead reads each document from a file, as a refresh of a changed
// file does, without its SHA-256: the reading of the JSON from the
// descriptor, the checks, and the building of the polygons and of their
// index. The document is not in the heap. Besides the time and the
// allocations of one read, it reports:
//
//   - file-MB: the size of the document;
//   - vertices: its vertices, closing positions excluded;
//   - heap-B/vertex and heap-MB: the heap that one copy holds, measured
//     after a garbage collection;
//   - peak-MB: the largest heap during the reads, above the heap before the
//     first copy; each read keeps the previous copy until the new one is
//     built, as a refresh does, so two copies coexist.
func BenchmarkRead(b *testing.B) {
	for _, set := range benchSets {
		s := set()
		path := filepath.Join(b.TempDir(), "areas.geojson")
		if err := os.WriteFile(path, s.doc, 0o644); err != nil {
			b.Fatal(err)
		}
		size := len(s.doc)
		s.doc = nil
		read := func() *Polygons {
			f, err := os.Open(path)
			if err != nil {
				b.Fatal(err)
			}
			defer f.Close()
			return readBench(b, s, f)
		}
		b.Run(s.name, func(b *testing.B) {
			base, before := liveHeap(), heapObjects()
			kept := read()
			held := liveHeap() - base
			b.ReportAllocs()
			b.SetBytes(int64(size))
			peak := peakHeap(func() {
				for b.Loop() {
					kept = read()
				}
			})
			runtime.KeepAlive(kept)
			b.ReportMetric(float64(size)/1e6, "file-MB")
			b.ReportMetric(float64(s.vertices), "vertices")
			b.ReportMetric(float64(held)/float64(s.vertices), "heap-B/vertex")
			b.ReportMetric(float64(held)/1e6, "heap-MB")
			b.ReportMetric(float64(peak-before)/1e6, "peak-MB")
		})
	}
}

package area

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func mustRead(t *testing.T, doc string) *Polygons {
	t.Helper()
	a, err := Read(strings.NewReader(doc), 1_000_000)
	if err != nil {
		t.Fatalf("%v\n%.200s", err, doc)
	}
	return a
}

// square is the ring of the square from (lon, lat) to (lon+side, lat+side),
// counterclockwise, as GeoJSON writes it.
func square(lon, lat, side float64) string {
	return fmt.Sprintf("[[%g,%g],[%g,%g],[%g,%g],[%g,%g],[%g,%g]]", lon, lat, lon+side, lat, lon+side, lat+side, lon, lat+side, lon, lat)
}

func TestDocuments(t *testing.T) {
	rome := `[[12.35,41.80],[12.62,41.80],[12.62,42.00],[12.35,42.00],[12.35,41.80]]`
	for _, tc := range []struct {
		name     string
		doc      string
		polygons int
	}{
		{"polygon", `{"type":"Polygon","coordinates":[` + rome + `]}`, 1},
		{"multipolygon", `{"type":"MultiPolygon","coordinates":[[` + square(0, 0, 1) + `],[` + square(5, 5, 1) + `]]}`, 2},
		{"feature", `{"type":"Feature","properties":{"name":"Rome"},"geometry":{"type":"Polygon","coordinates":[` + rome + `]}}`, 1},
		{"feature collection", `{"type":"FeatureCollection","features":[
			{"type":"Feature","geometry":{"type":"Polygon","coordinates":[` + square(0, 0, 1) + `]},"properties":null},
			{"type":"Feature","id":7,"bbox":[5,5,7,7],"geometry":{"type":"MultiPolygon","coordinates":[[` + square(5, 5, 1) + `],[` + square(6, 6, 1) + `]]},"properties":{}}
		]}`, 3},
		{"empty feature collection", `{"type":"FeatureCollection","features":[]}`, 0},
		// Members in any order: the type last, as sorted keys put it.
		{"type last", `{"features":[{"geometry":{"coordinates":[` + square(0, 0, 1) + `],"type":"Polygon"},"properties":{},"type":"Feature"}],"type":"FeatureCollection"}`, 1},
		{"coordinates first", `{"coordinates":[[` + square(0, 0, 1) + `]],"type":"MultiPolygon"}`, 1},
		{"geometry first", `{"geometry":{"type":"Polygon","coordinates":[` + square(0, 0, 1) + `]},"type":"Feature"}`, 1},
		// Altitude, foreign members, bbox, crs, id, a member named Type and
		// escapes in names and types.
		{"altitude", `{"type":"Polygon","coordinates":[[[0,0,10],[1,0,10.5],[1,1,-3],[0,1,0],[0,0,10]]]}`, 1},
		{"foreign members", `{"type":"FeatureCollection","name":"x","crs":{"type":"name","properties":{"name":"urn:ogc:def:crs:OGC:1.3:CRS84"}},"bbox":[0,0,1,1],"Type":"Polygon","features":[
			{"type":"Feature","centerline":{"type":"LineString","coordinates":[[0,0],[1,1]]},"geometry":{"type":"Polygon","coordinates":[` + square(0, 0, 1) + `],"bbox":[0,0,1,1],"geometries":[]},"properties":{"a":[1,{"b":null}],"Type":true}}
		]}`, 1},
		{"escapes", `{"\u0074ype":"Poly\u0067on","coordinates":[` + square(0, 0, 1) + `],"p\"q":"\ud83d\ude00\u00e9\n\/\\"}`, 1},
		{"byte order mark", "\uFEFF" + `{"type":"Polygon","coordinates":[` + square(0, 0, 1) + `]}`, 1},
		{"spaces", " \r\n\t{ \"type\" : \"Polygon\" ,\r\n \"coordinates\" : [ [ [ 0 , 0 ] , [ 1 , 0 ] , [ 1 , 1 ] , [ 0 , 0 ] ] ] }\n\n", 1},
		{"hole", `{"type":"Polygon","coordinates":[` + square(0, 0, 10) + `,` + square(4, 4, 2) + `]}`, 1},
		// Touching the antimeridian, and both poles, is allowed.
		{"antimeridian edge", `{"type":"MultiPolygon","coordinates":[[[[170,-10],[180,-10],[180,10],[170,10],[170,-10]]],[[[-180,-10],[-170,-10],[-170,10],[-180,10],[-180,-10]]]]}`, 2},
		{"poles", `{"type":"Polygon","coordinates":[[[-180,-90],[0,-90],[180,-90],[180,90],[0,90],[-180,90],[-180,-90]]]}`, 1},
		// Not checked: orientation, self-intersection, holes outside,
		// repeated positions.
		{"clockwise", `{"type":"Polygon","coordinates":[[[0,0],[0,1],[1,1],[1,0],[0,0]]]}`, 1},
		{"bow tie", `{"type":"Polygon","coordinates":[[[0,0],[1,1],[1,0],[0,1],[0,0]]]}`, 1},
		{"hole outside", `{"type":"Polygon","coordinates":[` + square(0, 0, 1) + `,` + square(5, 5, 1) + `]}`, 1},
		{"repeated positions", `{"type":"Polygon","coordinates":[[[0,0],[0,0],[1,0],[1,1],[1,1],[0,0]]]}`, 1},
		{"numbers", `{"type":"Polygon","coordinates":[[[-0,0.0],[1e0,0E+0],[1.0e-0,1],[0,1e-400],[-0.0,0]]]}`, 1},
		// A number of a position has at most 64 characters; one that Read
		// ignores has no limit.
		{"numbers of 64 characters", `{"type":"Polygon","id":0.` + strings.Repeat("0", 100) + `,"coordinates":[[[0.` + strings.Repeat("0", 62) + `,-1.` + strings.Repeat("0", 61) + `,1.` + strings.Repeat("5", 62) + `],[1,0],[1,1],[0,-1]]]}`, 1},
		// Repeated names are checked among the members that Read reads only:
		// what it ignores can repeat them.
		{"repeats in ignored members", `{"type":"FeatureCollection","name":"a","name":"b","features":[{"type":"Feature","properties":{"a":1,"a":2,"b":{"c":1,"c":2}},"properties":null,"centerline":{"type":"LineString","type":"Point"},"geometry":{"type":"Polygon","bbox":[0],"bbox":[1],"coordinates":[` + square(0, 0, 1) + `]}}]}`, 1},
		{"repeats among many properties", `{"type":"Polygon","properties":{` + manyMembers(40) + `,"m7":0},"coordinates":[` + square(0, 0, 1) + `]}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := mustRead(t, tc.doc)
			if a.Len() != tc.polygons {
				t.Errorf("%d polygons, want %d", a.Len(), tc.polygons)
			}
		})
	}
}

func TestContains(t *testing.T) {
	a := mustRead(t, `{"type":"FeatureCollection","features":[
		{"type":"Feature","properties":{"name":"Rome"},"geometry":{"type":"Polygon","coordinates":[[[12.35,41.80],[12.62,41.80],[12.62,42.00],[12.35,42.00],[12.35,41.80]]]}},
		{"type":"Feature","properties":{},"geometry":{"type":"MultiPolygon","coordinates":[[`+square(0, 0, 10)+`,`+square(4, 4, 2)+`],[`+square(20, 20, 1)+`]]}}
	]}`)
	for _, tc := range []struct {
		name     string
		lat, lon float64
		want     bool
	}{
		{"Rome", 41.8919, 12.5113, true},
		{"Berlin", 52.52, 13.405, false},
		{"San Francisco", 37.7749, -122.4194, false},
		{"in the square", 1, 1, true},
		{"in the hole", 5, 5, false},
		{"around the hole", 3, 5, true},
		{"second polygon", 20.5, 20.5, true},
		{"between the polygons", 15, 15, false},
		{"latitude and longitude swapped", 12.5113, 41.8919, false},
		{"NaN", math.NaN(), 1, false},
	} {
		if got := a.Contains(tc.lat, tc.lon); got != tc.want {
			t.Errorf("%s: Contains(%v, %v) = %v, want %v", tc.name, tc.lat, tc.lon, got, tc.want)
		}
	}
	var none *Polygons
	if none.Contains(1, 1) || none.Len() != 0 {
		t.Error("a nil Polygons contains a point")
	}
	if empty := mustRead(t, `{"type":"FeatureCollection","features":[]}`); empty.Contains(0, 0) {
		t.Error("an empty set contains a point")
	}
}

// The boundary is half-open: a point on an edge that two polygons share is
// in exactly one of them, and a point on a hole edge is in the polygon or in
// the polygon that fills the hole, never both.
func TestBoundary(t *testing.T) {
	left := mustRead(t, `{"type":"Polygon","coordinates":[`+square(10, 40, 10)+`]}`)
	right := mustRead(t, `{"type":"Polygon","coordinates":[`+square(20, 40, 10)+`]}`)
	// A triangle and its neighbor across a slanted edge.
	below := mustRead(t, `{"type":"Polygon","coordinates":[[[0,0],[3,0],[0,3],[0,0]]]}`)
	above := mustRead(t, `{"type":"Polygon","coordinates":[[[3,0],[3,3],[0,3],[3,0]]]}`)
	ring := mustRead(t, `{"type":"Polygon","coordinates":[`+square(0, 0, 10)+`,`+square(4, 4, 2)+`]}`)
	island := mustRead(t, `{"type":"Polygon","coordinates":[`+square(4, 4, 2)+`]}`)
	for _, tc := range []struct {
		name     string
		a, b     *Polygons
		lat, lon float64
	}{
		{"shared edge", left, right, 45, 20},
		{"shared vertex, bottom", left, right, 40, 20},
		{"slanted edge, midpoint", below, above, 1.5, 1.5},
		{"slanted edge, a third", below, above, 1, 2},
		{"slanted edge, near a vertex", below, above, 2.9999999999, 0.0000000001},
		{"hole edge, left", ring, island, 5, 4},
		{"hole edge, right", ring, island, 5, 6},
		{"hole edge, bottom", ring, island, 4, 5},
		{"hole edge, top", ring, island, 6, 5},
		{"hole vertex", ring, island, 4, 4},
	} {
		a, b := tc.a.Contains(tc.lat, tc.lon), tc.b.Contains(tc.lat, tc.lon)
		if a == b {
			t.Errorf("%s: in both or in neither (%v, %v)", tc.name, a, b)
		}
		// The same point, the same answer.
		if tc.a.Contains(tc.lat, tc.lon) != a {
			t.Errorf("%s: two answers", tc.name)
		}
	}
	// The outer edges of a union are inside on one side, outside on the
	// other: left and bottom in, right and top out.
	for _, tc := range []struct {
		lat, lon float64
		want     bool
	}{{45, 10, true}, {40, 15, true}, {45, 30, false}, {50, 15, false}, {50, 20, false}, {40, 20, true}} {
		if got := left.Contains(tc.lat, tc.lon) || right.Contains(tc.lat, tc.lon); got != tc.want {
			t.Errorf("edge point (%v, %v): %v, want %v", tc.lat, tc.lon, got, tc.want)
		}
	}
}

// circle is a ring of n vertices around (lon, lat), with a radius that
// alternates between r and r/2 to make a star.
func circle(lon, lat, r float64, n int) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i <= n; i++ {
		radius := r
		if i%n%2 == 1 {
			radius = r / 2
		}
		angle := 2 * math.Pi * float64(i%n) / float64(n)
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "[%v,%v]", lon+radius*math.Cos(angle), lat+radius*math.Sin(angle))
	}
	b.WriteString("]")
	return b.String()
}

// plainContains is Contains without the index and without the bands: the
// even-odd rule over every edge of every polygon whose box holds the point. A
// point outside the box of a ring crosses an even number of its edges.
func plainContains(a *Polygons, lat, lon float64) bool {
	ring := func(r int) bool {
		if !a.boxes[r].contains(lon, lat) {
			return false
		}
		v := a.points[a.rings[r]:a.rings[r+1]]
		in := false
		for i := range v {
			if crosses(v[i], v[(i+len(v)-1)%len(v)], lon, lat) {
				in = !in
			}
		}
		return in
	}
	for p := range a.Len() {
		first, end := int(a.polygons[p]), int(a.polygons[p+1])
		if !ring(first) {
			continue
		}
		hole := false
		for r := first + 1; r < end; r++ {
			hole = hole || ring(r)
		}
		if !hole {
			return true
		}
	}
	return false
}

// The index and the bands answer as the even-odd rule over every edge does.
func TestIndexAgreesWithTheRule(t *testing.T) {
	var polygons []string
	for i := range 40 {
		for j := range 40 {
			polygons = append(polygons, "["+square(float64(i)*0.5, float64(j)*0.5, 0.4)+"]")
		}
	}
	// Rings with bands, one with a hole with bands too.
	polygons = append(polygons, "["+circle(50, 10, 5, 1000)+","+circle(50, 10, 1, 300)+"]", "["+circle(-60, -20, 8, 4001)+"]")
	a := mustRead(t, `{"type":"MultiPolygon","coordinates":[`+strings.Join(polygons, ",")+`]}`)
	if a.Len() != 1602 || len(a.bands) != 3 {
		t.Fatalf("%d polygons, %d rings with bands", a.Len(), len(a.bands))
	}
	state := uint64(1)
	random := func(lo, hi float64) float64 {
		state = state*6364136223846793005 + 1442695040888963407
		return lo + (hi-lo)*float64(state>>11)/float64(1<<53)
	}
	inside := 0
	for range 20000 {
		var lat, lon float64
		switch random(0, 3) < 1 {
		case true:
			lat, lon = random(-1, 21), random(-1, 21)
		default:
			lat, lon = random(-30, 20), random(-70, 60)
		}
		got := a.Contains(lat, lon)
		if want := plainContains(a, lat, lon); got != want {
			t.Fatalf("Contains(%v, %v) = %v, the rule says %v", lat, lon, got, want)
		}
		if got {
			inside++
		}
	}
	if inside < 2000 {
		t.Errorf("only %d points inside: the test checks too few", inside)
	}
	// The vertices of the rings with bands, on their edges.
	for _, b := range []int{len(a.rings) - 2, len(a.rings) - 3} {
		for _, v := range a.points[a.rings[b]:a.rings[b+1]] {
			if got, want := a.Contains(v.y, v.x), plainContains(a, v.y, v.x); got != want {
				t.Fatalf("vertex (%v, %v): %v, the rule says %v", v.y, v.x, got, want)
			}
		}
	}
}

func TestContainsAllocatesNothing(t *testing.T) {
	a := mustRead(t, `{"type":"MultiPolygon","coordinates":[[`+circle(12, 42, 1, 1000)+`],[`+square(0, 0, 1)+`]]}`)
	if n := testing.AllocsPerRun(100, func() { a.Contains(42, 12); a.Contains(0.5, 0.5); a.Contains(-5, -5) }); n != 0 {
		t.Errorf("%v allocations per lookup", n)
	}
}

func TestInvalidDocuments(t *testing.T) {
	sq := square(0, 0, 1)
	polygon := `{"type":"Polygon","coordinates":[` + sq + `]}`
	feature := `{"type":"Feature","geometry":` + polygon + `}`
	for _, tc := range []struct{ name, doc, want string }{
		// JSON.
		{"empty", "", "line 1, column 1: the content is empty"},
		{"spaces only", " \n  ", "line 2, column 3: the content is empty"},
		{"not JSON", "x", "line 1, column 1: invalid JSON: expected a value"},
		{"not an object", `[` + polygon + `]`, "line 1, column 1: expected a GeoJSON object"},
		{"end in an object", `{"type":"Polygon"`, "line 1, column 18: invalid JSON: unexpected end of the content"},
		{"end in a string", `{"type":"Poly`, "line 1, column 14: invalid JSON: unexpected end of the content"},
		{"missing comma", "{\"type\":\"FeatureCollection\",\n\"features\":[" + feature + " " + feature + "]}", "line 2, column 110: invalid JSON: expected , or ]"},
		{"missing comma in an object", `{"type":"Polygon" "coordinates":[]}`, "line 1, column 19: invalid JSON: expected , or }"},
		{"missing colon", `{"type" "Polygon"}`, "line 1, column 9: invalid JSON: expected :"},
		{"unquoted name", `{type:"Polygon"}`, "line 1, column 2: invalid JSON: expected a member name in double quotes"},
		{"trailing comma in an object", `{"type":"Polygon",}`, "line 1, column 19: invalid JSON: expected a member name in double quotes"},
		{"trailing comma in an array", `{"type":"Polygon","coordinates":[` + sq + `,]}`, "line 1, column 66: invalid JSON: expected a value"},
		{"trailing comma in coordinates", `{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,0],]]}`, "line 1, column 59: invalid JSON: expected a value"},
		{"trailing comma in properties", `{"type":"Polygon","properties":[1,],"coordinates":[` + sq + `]}`, "line 1, column 35: invalid JSON: expected a value"},
		{"leading zero", `{"type":"Polygon","id":01,"coordinates":[` + sq + `]}`, "line 1, column 24: invalid JSON: invalid number"},
		{"leading zero in a coordinate", `{"type":"Polygon","coordinates":[[[00,0]]]}`, "line 1, column 36: invalid JSON: invalid number"},
		{"plus sign", `{"type":"Polygon","coordinates":[[[+1,0]]]}`, "line 1, column 36: invalid JSON: expected a value"},
		{"bare dot", `{"type":"Polygon","coordinates":[[[1.,0]]]}`, "line 1, column 36: invalid JSON: invalid number"},
		{"longitude of 65 characters", `{"type":"Polygon","coordinates":[[[0.` + strings.Repeat("0", 63) + `,0]]]}`, "line 1, column 36: invalid JSON: invalid number"},
		{"latitude of 65 characters", `{"type":"Polygon","coordinates":[[[0,-1.` + strings.Repeat("0", 62) + `]]]}`, "line 1, column 38: invalid JSON: invalid number"},
		{"altitude of 65 characters", `{"type":"Polygon","coordinates":[[[0,0,1e` + strings.Repeat("0", 63) + `]]]}`, "line 1, column 40: invalid JSON: invalid number"},
		{"exponent without digits", `{"type":"Polygon","id":1e,"coordinates":[]}`, "line 1, column 24: invalid JSON: invalid number"},
		{"minus alone", `{"type":"Polygon","id":-,"coordinates":[]}`, "line 1, column 24: invalid JSON: invalid number"},
		{"NaN", `{"type":"Polygon","coordinates":[[[NaN,0]]]}`, "line 1, column 36: invalid JSON: expected a value"},
		{"bad literal", `{"type":"Polygon","id":nul,"coordinates":[]}`, "line 1, column 24: invalid JSON: expected a value"},
		{"single quotes", `{"type":'Polygon'}`, "line 1, column 9: invalid JSON: expected a value"},
		{"bad escape", `{"type":"Polygon","p":"a\x"}`, "line 1, column 25: invalid JSON: invalid escape in a string"},
		{"short unicode escape", `{"type":"Polygon","p":"\u12x4"}`, "line 1, column 24: invalid JSON: invalid escape in a string"},
		{"lone low surrogate", `{"type":"Polygon","p":"\udc00"}`, "line 1, column 24: invalid JSON: invalid escape in a string"},
		{"lone high surrogate", `{"type":"Polygon","p":"\ud800x"}`, "line 1, column 24: invalid JSON: invalid escape in a string"},
		{"high surrogate and no low one", `{"type":"Polygon","p":"\ud800\u0041"}`, "line 1, column 24: invalid JSON: invalid escape in a string"},
		{"control character", "{\"type\":\"Polygon\",\"p\":\"a\tb\"}", "line 1, column 25: invalid JSON: control character in a string"},
		{"newline in a string", "{\"type\":\"Polygon\",\"p\":\"a\nb\"}", "line 1, column 25: invalid JSON: control character in a string"},
		{"invalid UTF-8", "{\"type\":\"Polygon\",\"p\":\"é\xff\"}", "line 1, column 25: the content is not valid UTF-8"},
		{"UTF-8 cut", "{\"type\":\"Polygon\",\"p\":\"\xe2\x82", "line 1, column 24: the content is not valid UTF-8"},
		{"surrogate in UTF-8", "{\"type\":\"Polygon\",\"p\":\"\xed\xa0\x80\"}", "line 1, column 24: the content is not valid UTF-8"},
		{"overlong UTF-8", "{\"type\":\"Polygon\",\"p\":\"\xc0\xaf\"}", "line 1, column 24: the content is not valid UTF-8"},
		{"byte outside a string", "{\"type\":\"Polygon\",\"p\":\xc3\xa9}", "line 1, column 23: invalid JSON: expected a value"},
		{"content after the document", polygon + "\n x", "line 2, column 2: invalid JSON: content after the document"},
		{"two documents", polygon + polygon, "line 1, column 67: invalid JSON: content after the document"},
		{"two byte order marks", "\uFEFF\uFEFF" + polygon, "line 1, column 1: invalid JSON: expected a value"},
		{"repeated member", `{"type":"Polygon","coordinates":[` + sq + `],"coordinates":[]}`, "line 1, column 67: a member name is repeated in an object"},
		{"repeated type, escaped", `{"type":"Polygon","t\u0079pe":"Polygon"}`, "line 1, column 19: a member name is repeated in an object"},
		{"repeated features", `{"type":"FeatureCollection","features":[],"features":[]}`, "line 1, column 43: a member name is repeated in an object"},
		{"repeated geometry in a feature", `{"type":"FeatureCollection","features":[{"type":"Feature","geometry":` + polygon + `,"geometry":null}]}`, "line 1, column 137: feature 1: a member name is repeated in an object"},
		{"repeated type in a geometry", `{"type":"Feature","geometry":{"type":"Polygon","coordinates":[` + sq + `],"type":"Polygon"}}`, "line 1, column 96: a member name is repeated in an object"},
		{"nested too deeply", `{"type":"Polygon","properties":` + strings.Repeat("[", 64) + strings.Repeat("]", 64) + `}`, "line 1, column 95: the JSON is nested more than 64 levels deep"},

		// The structure.
		{"no type", `{"coordinates":[` + sq + `]}`, "line 1, column 1: the type member is missing"},
		{"type in another case", `{"Type":"Polygon","coordinates":[` + sq + `]}`, "line 1, column 1: the type member is missing"},
		{"type not a string", `{"type":7}`, "line 1, column 9: the type must be a string"},
		{"unknown type", `{"type":"Polygn","coordinates":[]}`, "line 1, column 9: unknown GeoJSON type"},
		{"type in lowercase", `{"type":"polygon","coordinates":[]}`, "line 1, column 9: unknown GeoJSON type"},
		{"long type", `{"type":"` + strings.Repeat("Polygon", 10) + `"}`, "line 1, column 9: unknown GeoJSON type"},
		{"point document", `{"type":"Point","coordinates":[1,2]}`, "line 1, column 9: a Point is not allowed: the document must be a FeatureCollection, a Feature, a Polygon or a MultiPolygon"},
		{"geometry collection document", `{"type":"GeometryCollection","geometries":[` + polygon + `]}`, "line 1, column 9: a GeometryCollection is not allowed: the document must be a FeatureCollection, a Feature, a Polygon or a MultiPolygon"},
		{"feature collection without features", `{"type":"FeatureCollection"}`, "line 1, column 1: the features member is missing"},
		{"features not an array", `{"type":"FeatureCollection","features":{}}`, "line 1, column 40: the features member must be an array"},
		{"features null", `{"type":"FeatureCollection","features":null}`, "line 1, column 40: the features member must be an array"},
		{"feature not an object", `{"type":"FeatureCollection","features":[` + feature + `,7]}`, "line 1, column 138: feature 2: expected a GeoJSON object"},
		{"feature without type", `{"type":"FeatureCollection","features":[{"geometry":` + polygon + `}]}`, "line 1, column 41: feature 1: the type member is missing"},
		{"geometry as a feature", `{"type":"FeatureCollection","features":[` + polygon + `]}`, "line 1, column 49: feature 1: a Polygon is not allowed: an item of features must be a Feature"},
		{"feature without geometry", `{"type":"FeatureCollection","features":[{"type":"Feature","properties":{}}]}`, "line 1, column 41: feature 1: the geometry member is missing"},
		{"null geometry", `{"type":"FeatureCollection","features":[` + feature + `,{"type":"Feature","geometry":null}]}`, "line 1, column 167: feature 2: the geometry is null"},
		{"null geometry first", `{"geometry":null,"type":"Feature"}`, "line 1, column 13: the geometry is null"},
		{"geometry not an object", `{"type":"Feature","geometry":[]}`, "line 1, column 30: expected a GeoJSON object"},
		{"point geometry", `{"type":"FeatureCollection","features":[{"type":"Feature","geometry":{"type":"Point","coordinates":[1,2]}}]}`, "line 1, column 78: feature 1: a Point is not allowed: a geometry must be a Polygon or a MultiPolygon"},
		{"line geometry", `{"type":"Feature","geometry":{"type":"LineString","coordinates":[[1,2],[3,4]]}}`, "line 1, column 38: a LineString is not allowed: a geometry must be a Polygon or a MultiPolygon"},
		{"multipoint geometry", `{"type":"Feature","geometry":{"type":"MultiPoint","coordinates":[[1,2]]}}`, "line 1, column 38: a MultiPoint is not allowed: a geometry must be a Polygon or a MultiPolygon"},
		{"multiline geometry", `{"type":"Feature","geometry":{"type":"MultiLineString","coordinates":[]}}`, "line 1, column 38: a MultiLineString is not allowed: a geometry must be a Polygon or a MultiPolygon"},
		{"collection geometry", `{"type":"Feature","geometry":{"type":"GeometryCollection","geometries":[` + polygon + `]}}`, "line 1, column 38: a GeometryCollection is not allowed: a geometry must be a Polygon or a MultiPolygon"},
		{"feature geometry", `{"type":"Feature","geometry":` + feature + `}`, "line 1, column 38: a Feature is not allowed: a geometry must be a Polygon or a MultiPolygon"},
		{"point geometry, type last", `{"type":"Feature","geometry":{"coordinates":[1,2],"type":"Point"}}`, "line 1, column 46: the coordinates do not have the shape of a Polygon or a MultiPolygon"},
		{"geometry without coordinates", `{"type":"Feature","geometry":{"type":"Polygon"}}`, "line 1, column 30: the coordinates member is missing"},
		{"coordinates not an array", `{"type":"Polygon","coordinates":{}}`, "line 1, column 33: the coordinates do not have the shape of a Polygon"},
		{"coordinates null", `{"coordinates":null,"type":"MultiPolygon"}`, "line 1, column 16: the coordinates do not have the shape of a Polygon or a MultiPolygon"},

		// Members of another kind of object.
		{"coordinates in a feature collection", `{"type":"FeatureCollection","features":[],"coordinates":[]}`, "line 1, column 43: the coordinates member is not allowed in a FeatureCollection"},
		{"geometry in a feature collection, first", `{"geometry":null,"features":[],"type":"FeatureCollection"}`, "line 1, column 18: the features member is not allowed in a Feature"},
		{"features in a feature, type last", `{"features":[],"type":"Feature"}`, "line 1, column 2: the features member is not allowed in a Feature"},
		{"coordinates in a feature", `{"type":"FeatureCollection","features":[{"type":"Feature","coordinates":[],"geometry":` + polygon + `}]}`, "line 1, column 59: feature 1: the coordinates member is not allowed in a Feature"},
		{"features in a geometry", `{"type":"Feature","geometry":{"features":[],"type":"Polygon"}}`, "line 1, column 31: the features member is not allowed in a geometry"},
		{"geometry in a polygon", `{"type":"Polygon","coordinates":[` + sq + `],"geometry":null}`, "line 1, column 67: the geometry member is not allowed in a Polygon"},

		// Coordinates.
		{"polygon without rings", `{"type":"Polygon","coordinates":[]}`, "line 1, column 33: the polygon has no rings"},
		{"polygon without rings, type last", `{"coordinates":[],"type":"Polygon"}`, "line 1, column 16: the polygon has no rings"},
		{"multipolygon without polygons", `{"type":"MultiPolygon","coordinates":[]}`, "line 1, column 38: the MultiPolygon has no polygons"},
		{"multipolygon without polygons, type last", `{"coordinates":[],"type":"MultiPolygon"}`, "line 1, column 16: the MultiPolygon has no polygons"},
		{"empty polygon in a multipolygon", `{"type":"MultiPolygon","coordinates":[[` + sq + `],[]]}`, "line 1, column 73: polygon 2: the polygon has no rings"},
		{"empty ring", `{"type":"Polygon","coordinates":[[]]}`, "line 1, column 34: ring 1: the ring has fewer than 4 positions"},
		{"empty ring, type last", `{"coordinates":[[]],"type":"Polygon"}`, "line 1, column 17: the coordinates do not have the shape of a Polygon or a MultiPolygon"},
		{"ring of 3 positions", `{"type":"FeatureCollection","features":[` + feature + `,{"type":"Feature","geometry":{"type":"MultiPolygon","coordinates":[[` + sq + `,[[0,0],[1,0],[0,0]]]]}}]}`, "line 1, column 238: feature 2, polygon 1, ring 2: the ring has fewer than 4 positions"},
		{"ring not closed", `{"type":"FeatureCollection","features":[` + feature + `,` + feature + `,{"type":"Feature","geometry":{"type":"MultiPolygon","coordinates":[[` + sq + `,[[0,0],[1,0],[1,1],[0,1]]]]}}]}`, "line 1, column 335: feature 3, polygon 1, ring 2: the ring is not closed: its last position differs from its first"},
		{"position of one number", `{"type":"Polygon","coordinates":[[[0,0],[1],[1,1],[0,0]]]}`, "line 1, column 41: ring 1, position 2: the position has fewer than 2 numbers"},
		{"empty position", `{"type":"Polygon","coordinates":[[[0,0],[],[1,1],[0,0]]]}`, "line 1, column 41: ring 1, position 2: the position has fewer than 2 numbers"},
		{"position of four numbers", `{"type":"Polygon","coordinates":[[[0,0],[1,0,0,0],[1,1],[0,0]]]}`, "line 1, column 41: ring 1, position 2: the position has more than 3 numbers"},
		{"string in a position", `{"type":"Polygon","coordinates":[[[0,0],[1,"0"],[1,1],[0,0]]]}`, "line 1, column 44: ring 1, position 2: the position holds a value that is not a number"},
		{"null in a position", `{"type":"Polygon","coordinates":[[[0,0],[1,null],[1,1],[0,0]]]}`, "line 1, column 44: ring 1, position 2: the position holds a value that is not a number"},
		{"array in a position", `{"type":"Polygon","coordinates":[[[0,0],[1,[0]],[1,1],[0,0]]]}`, "line 1, column 44: ring 1, position 2: the coordinates do not have the shape of a Polygon"},
		{"number for a ring", `{"type":"Polygon","coordinates":[1]}`, "line 1, column 34: the coordinates do not have the shape of a Polygon"},
		{"number for a position", `{"type":"Polygon","coordinates":[[1,2,3,4]]}`, "line 1, column 35: ring 1: the coordinates do not have the shape of a Polygon"},
		{"string for a ring", `{"type":"Polygon","coordinates":["x"]}`, "line 1, column 34: the coordinates do not have the shape of a Polygon"},
		{"polygon depth in a multipolygon", `{"type":"MultiPolygon","coordinates":[` + sq + `]}`, "line 1, column 41: polygon 1, ring 1: the coordinates do not have the shape of a MultiPolygon"},
		{"multipolygon depth in a polygon", `{"type":"Polygon","coordinates":[[` + sq + `]]}`, "line 1, column 36: ring 1, position 1: the coordinates do not have the shape of a Polygon"},
		{"polygon depth, type last", `{"coordinates":[` + sq + `],"type":"MultiPolygon"}`, "line 1, column 16: the coordinates do not have the shape of a MultiPolygon"},
		{"multipolygon depth, type last", `{"coordinates":[[` + sq + `]],"type":"Polygon"}`, "line 1, column 16: the coordinates do not have the shape of a Polygon"},
		{"mixed depths", `{"type":"Polygon","coordinates":[` + sq + `,[[[0,0]]]]}`, "line 1, column 68: ring 2, position 1: the coordinates do not have the shape of a Polygon"},
		{"nested too deeply in coordinates", `{"coordinates":[[[[[0,0]]]]],"type":"Polygon"}`, "line 1, column 20: the coordinates do not have the shape of a Polygon or a MultiPolygon"},
		{"number beyond a double", `{"type":"Polygon","coordinates":[[[0,0],[1e999,0],[1,1],[0,0]]]}`, "line 1, column 42: ring 1, position 2: the number is beyond the range of a double"},
		{"altitude beyond a double", `{"type":"Polygon","coordinates":[[[0,0],[1,0,-1e999],[1,1],[0,0]]]}`, "line 1, column 46: ring 1, position 2: the number is beyond the range of a double"},
		{"longitude 200", `{"type":"Polygon","coordinates":[[[0,0],[200,0],[1,1],[0,0]]]}`, "line 1, column 42: ring 1, position 2: the longitude is outside -180 to 180"},
		{"longitude -180.5", `{"type":"Polygon","coordinates":[[[0,0],[-180.5,0],[1,1],[0,0]]]}`, "line 1, column 42: ring 1, position 2: the longitude is outside -180 to 180"},
		{"latitude 95.5", "{\"type\":\"Polygon\",\n\"coordinates\":[[[0,0],[1,95.5],[1,1],[0,0]]]}", "line 2, column 26: ring 1, position 2: the latitude is outside -90 to 90"},
		{"latitude -90.000001", `{"type":"Polygon","coordinates":[[[0,0],[1,-90.000001],[1,1],[0,0]]]}`, "line 1, column 44: ring 1, position 2: the latitude is outside -90 to 90"},
		{"antimeridian", `{"type":"Polygon","coordinates":[[[170,0],[-170,0],[-170,10],[170,10],[170,0]]]}`, "line 1, column 43: ring 1, position 2: two consecutive positions are more than 180 degrees of longitude apart: split the polygon at the antimeridian"},
		{"antimeridian, later position", `{"type":"Polygon","coordinates":[[[-179,0],[-179,10],[179,10],[179,0],[-179,0]]]}`, "line 1, column 54: ring 1, position 3: two consecutive positions are more than 180 degrees of longitude apart: split the polygon at the antimeridian"},
		{"antimeridian, type last", `{"coordinates":[[[[170,0],[-170,0],[-170,10],[170,10],[170,0]]]],"type":"MultiPolygon"}`, "line 1, column 27: polygon 1, ring 1, position 2: two consecutive positions are more than 180 degrees of longitude apart: split the polygon at the antimeridian"},

		// Columns count characters.
		{"column after characters", `{"name":"Città è ✓ 😀","type":"Polygon","coordinates":[1]}`, "line 1, column 55: the coordinates do not have the shape of a Polygon"},
		{"column after an escape", `{"name":"\u00e9","type":"Polygon","coordinates":[1]}`, "line 1, column 50: the coordinates do not have the shape of a Polygon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Read(strings.NewReader(tc.doc), 1000)
			var invalid *Error
			if !errors.As(err, &invalid) || err.Error() != tc.want {
				t.Errorf("got  %v\nwant %s", err, tc.want)
			}
		})
	}
}

// manyMembers writes n members, m0 to m(n-1), of an object.
func manyMembers(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`"m%d":0`, i)
	}
	return strings.Join(parts, ",")
}

func TestCeilings(t *testing.T) {
	polygons := strings.Repeat(`[`+square(0, 0, 1)+`],`, 3)
	multi := `{"type":"MultiPolygon","coordinates":[` + strings.TrimSuffix(polygons, ",") + `]}`
	if a, err := Read(strings.NewReader(multi), 3); err != nil || a.Len() != 3 {
		t.Errorf("3 polygons with max_entries 3: %v", err)
	}
	if _, err := Read(strings.NewReader(multi), 2); err == nil || err.Error() != "line 1, column 107: polygon 3: more polygons than max_entries" {
		t.Errorf("3 polygons with max_entries 2: %v", err)
	}
	// The positions are counted with the closing ones; 15 of them.
	if _, err := read(strings.NewReader(multi), 10, 15); err != nil {
		t.Errorf("15 positions with a ceiling of 15: %v", err)
	}
	if _, err := read(strings.NewReader(multi), 10, 14); err == nil || err.Error() != "line 1, column 133: polygon 3, ring 1, position 5: more than 10000000 positions" {
		t.Errorf("15 positions with a ceiling of 14: %v", err)
	}
	// The ceiling stops the read: the rest of the content is not read.
	r := &countingReader{r: io.MultiReader(strings.NewReader(`{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1]`), endless{})}
	if _, err := read(r, 10, 2); err == nil || !strings.Contains(err.Error(), "more than 10000000 positions") || r.n > 1<<20 {
		t.Errorf("a ceiling on endless content: %v, after %d bytes", err, r.n)
	}
	// 64 levels are allowed, 65 are not.
	nested := func(n int) string {
		return `{"type":"Polygon","coordinates":[` + square(0, 0, 1) + `],"p":` + strings.Repeat("[", n-1) + strings.Repeat("]", n-1) + `}`
	}
	if _, err := Read(strings.NewReader(nested(MaxDepth)), 1); err != nil {
		t.Errorf("%d levels: %v", MaxDepth, err)
	}
	if _, err := Read(strings.NewReader(nested(MaxDepth+1)), 1); err == nil || !strings.Contains(err.Error(), "nested more than 64 levels deep") {
		t.Errorf("%d levels: %v", MaxDepth+1, err)
	}
	if MaxPositions != 10_000_000 {
		t.Errorf("MaxPositions is %d", MaxPositions)
	}
}

// endless is content made of commas, without end.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ','
	}
	return len(p), nil
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// oneByte reads one byte at a time, so that every value spans reads.
type oneByte struct{ r io.Reader }

func (o oneByte) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

// The reads of the content can split it anywhere: one byte at a time, the
// result is the same.
func TestReadsOfOneByte(t *testing.T) {
	doc := "\uFEFF" + `{"type":"Feature","properties":{"name":"Città \u00e9 \ud83d\ude00 😀"},"geometry":{"type":"Polygon","coordinates":[` + circle(12, 42, 1, 600) + `]}}`
	whole := mustRead(t, doc)
	split, err := Read(oneByte{strings.NewReader(doc)}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(whole.points, split.points) || whole.Contains(42, 12) != split.Contains(42, 12) {
		t.Error("the result depends on the reads")
	}
	bad := `{"type":"Polygon","p":"` + strings.Repeat("é", 10) + "\xff\"}"
	_, err = Read(oneByte{strings.NewReader(bad)}, 10)
	if err == nil || err.Error() != "line 1, column 34: the content is not valid UTF-8" {
		t.Errorf("invalid UTF-8 across reads: %v", err)
	}
}

type failingReader struct{}

var errBroken = errors.New("broken")

func (failingReader) Read([]byte) (int, error) { return 0, errBroken }

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, nil }

func TestReadError(t *testing.T) {
	r := io.MultiReader(strings.NewReader(`{"type":"Polygon","coordinates":[`), failingReader{})
	if _, err := Read(r, 10); !errors.Is(err, errBroken) {
		t.Errorf("a failing reader: %v", err)
	}
	if _, err := Read(emptyReader{}, 10); !errors.Is(err, io.ErrNoProgress) {
		t.Errorf("a reader without progress: %v", err)
	}
}

// problems are the fixed problems of a document.
var problems = []string{
	problemEmpty, problemValue, problemEnd, problemArraySep, problemObjectSep, problemColon, problemName, problemNumber,
	problemEscape, problemControl, problemAfter, problemUTF8, problemDuplicate, problemObject, problemType, problemTypeString,
	problemTypeName, problemFeatures, problemNoFeatures, problemNoGeometry, problemNull, problemNoCoords, problemNoRings,
	problemNoPolygons, problemShortRing, problemOpenRing, problemShortPos, problemLongPos, problemNotNumber, problemRange,
	problemLongitude, problemLatitude, problemAntimer, problemPolygons, problemDepth, problemPositions,
	shapeOf(0), shapeOf(3), shapeOf(4),
}

func init() {
	for k, name := range kindNames {
		if name == "" {
			continue
		}
		for _, c := range []context{inDocument, inFeatures, inGeometry} {
			if !allowedKind(c, kind(k)) {
				problems = append(problems, kindNotAllowed(c, kind(k)))
			}
		}
	}
	for _, member := range []string{memberFeatures, memberGeometry, memberCoordinates} {
		for _, name := range []string{"FeatureCollection", "Feature", "Polygon", "MultiPolygon", "geometry"} {
			problems = append(problems, memberNotAllowed(member, name))
		}
	}
}

var pathRE = regexp.MustCompile(`^(feature [1-9][0-9]*)?((^|, )polygon [1-9][0-9]*)?((^|, )ring [1-9][0-9]*)?((^|, )position [1-9][0-9]*)?$`)

func FuzzReadGeoJSON(f *testing.F) {
	for _, seed := range []string{
		`{"type":"Polygon","coordinates":[` + square(0, 0, 1) + `,` + square(0.2, 0.2, 0.1) + `]}`,
		`{"type":"MultiPolygon","coordinates":[[` + square(0, 0, 1) + `],[` + square(5, 5, 1) + `]]}`,
		`{"type":"FeatureCollection","features":[{"type":"Feature","properties":{"a":[1,"b",null,true]},"geometry":{"type":"Polygon","coordinates":[[[0,0,1],[1,0],[1,1],[0,0]]]}}]}`,
		`{"coordinates":[[[[170,0],[-170,0],[-170,10],[170,10],[170,0]]]],"type":"MultiPolygon"}`,
		`{"geometry":null,"type":"Feature"}`,
		"\uFEFF" + `{"t\u0079pe":"Polygon","p":"\ud83d\ude00","coordinates":[[[0,0],[1,0],[1,1],[0,0]]]}`,
		`{"type":"Polygon","coordinates":[[[1e999,0]]]}`,
		`{"type":"Polygon","coordinates":[[[0,0],[1,1]]],"type":"Polygon"}`,
		"{\"type\":\"Polygon\",\"p\":\"\xff\"}",
		"[[[[[[[[[[[[[[[[[",
		"",
	} {
		f.Add(seed, uint8(3))
	}
	f.Fuzz(func(t *testing.T, doc string, maxPolygons uint8) {
		a, err := Read(strings.NewReader(doc), int(maxPolygons))
		again, err2 := Read(oneByte{strings.NewReader(doc)}, int(maxPolygons))
		if (err == nil) != (err2 == nil) || err != nil && err.Error() != err2.Error() {
			t.Fatalf("two reads, two results: %v, %v", err, err2)
		}
		if err != nil {
			var invalid *Error
			switch {
			case !errors.As(err, &invalid):
				t.Fatalf("not an *Error: %v", err)
			case !slices.Contains(problems, invalid.Problem):
				t.Fatalf("unexpected problem: %q", invalid.Problem)
			case invalid.Line < 1 || invalid.Column < 1 || invalid.Line > strings.Count(doc, "\n")+1 || invalid.Column > utf8.RuneCountInString(doc)+1:
				t.Fatalf("position out of the content: %v", err)
			case !pathRE.MatchString(invalid.Path):
				t.Fatalf("unexpected path: %q", invalid.Path)
			}
			return
		}
		if !json.Valid([]byte(strings.TrimPrefix(doc, "\uFEFF"))) {
			t.Fatal("Read accepted content that is not JSON")
		}
		if a.Len() > int(maxPolygons) || !slices.Equal(a.points, again.points) {
			t.Fatalf("%d polygons, max %d", a.Len(), maxPolygons)
		}
		for _, v := range a.points {
			if !(v.x >= -180 && v.x <= 180 && v.y >= -90 && v.y <= 90) {
				t.Fatalf("a position out of range: %v", v)
			}
		}
		for _, v := range a.points {
			a.Contains(v.y, v.x)
		}
	})
}

// Read keeps no name of what it ignores: the members of properties, however
// many, allocate nothing in proportion to them.
func TestIgnoredNamesAreNotKept(t *testing.T) {
	names := manyMembers(100_000) // about 1.4 MB of names
	doc := `{"type":"Polygon","properties":{` + names + `,"m7":{` + names + `}},"coordinates":[` + square(0, 0, 1) + `]}`
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := Read(strings.NewReader(doc), 1); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 256<<10 {
		t.Errorf("%d bytes allocated to read %d bytes of names", allocated, 2*len(names))
	}
}

// Read writes the vertices it reads at most twice: what it allocates is a
// small multiple of the copy it returns, not one for each time the arrays
// grow.
func TestReadAllocatesAFewCopies(t *testing.T) {
	polygons := make([]string, 1000)
	for i := range polygons {
		polygons[i] = "[" + circle(float64(i%40)*4-80, float64(i/40)*3-60, 1, 200) + "]"
	}
	doc := `{"type":"MultiPolygon","coordinates":[` + strings.Join(polygons, ",") + `]}`
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	a, err := Read(strings.NewReader(doc), 1000)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	// 200,000 vertices, 3.2 MB: about 7.7 MB are allocated, the chunks, with
	// at most maxChunk vertices to spare, and the copy; growing the array
	// with append alone would allocate about 18 MB.
	vertices := uint64(len(a.points)) * 16
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 3*vertices {
		t.Errorf("%d bytes allocated for %d bytes of vertices", allocated, vertices)
	}
}

// The chunks of an array keep its values in order, across the boundaries of
// chunks of growing sizes, and pop removes the last one.
func TestChunks(t *testing.T) {
	var c chunks[int]
	var want []int
	for i := range 3*maxChunk + 7 {
		c.add(i)
		want = append(want, i)
		if i%(minChunk+1) == 0 {
			c.pop()
			want = want[:len(want)-1]
		}
		if c.len() != len(want) {
			t.Fatalf("len %d, want %d", c.len(), len(want))
		}
	}
	if got := c.collect(); !slices.Equal(got, want) || cap(got) != len(want) {
		t.Fatalf("collect: %d values, capacity %d; want %d", len(got), cap(got), len(want))
	}
	if c.len() != 0 || len(c.collect()) != 0 {
		t.Error("collect does not empty the chunks")
	}
}

// Read keeps the texts it reads within fixed bounds, whatever their length
// in the document: a number of a position longer than MaxNumber characters is
// rejected, a type or a member name of a GeoJSON object is kept to its first
// bytes only. Megabytes of such a text allocate nothing in proportion to them.
func TestKeptTextsAreBounded(t *testing.T) {
	huge := strings.Repeat("0", 8<<20)
	ring := square(0, 0, 1)
	for _, tc := range []struct{ name, doc, want string }{
		{"number", `{"type":"Polygon","coordinates":[[[0.` + huge + `,0]]]}`, "line 1, column 36: invalid JSON: invalid number"},
		{"type", `{"type":"Polygon` + huge + `","coordinates":[` + ring + `]}`, "line 1, column 9: unknown GeoJSON type"},
		{"member name", `{"type":"Polygon","coordinates` + huge + `":0,"coordinates":[` + ring + `]}`, ""},
		{"ignored number", `{"type":"Polygon","id":0.` + huge + `,"coordinates":[` + ring + `]}`, ""},
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := Read(strings.NewReader(tc.doc), 1)
		runtime.ReadMemStats(&after)
		if got := fmt.Sprint(err); tc.want == "" && err != nil || tc.want != "" && got != tc.want {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 256<<10 {
			t.Errorf("%s: %d bytes allocated to read %d bytes of it", tc.name, allocated, len(huge))
		}
	}
}

package sets

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// rome is a document of one area around Rome, and one around Berlin with a
// hole.
const rome = `{"type":"FeatureCollection","features":[
{"type":"Feature","properties":{"name":"Rome"},"geometry":{"type":"Polygon","coordinates":[[[12.35,41.80],[12.62,41.80],[12.62,42.00],[12.35,42.00],[12.35,41.80]]]}},
{"type":"Feature","properties":{"name":"Berlin"},"geometry":{"type":"MultiPolygon","coordinates":[[[[13.0,52.3],[13.8,52.3],[13.8,52.7],[13.0,52.7],[13.0,52.3]],[[13.3,52.4],[13.5,52.4],[13.5,52.6],[13.3,52.6],[13.3,52.4]]]]}}
]}`

func TestAreaSet(t *testing.T) {
	s, err := Read(Area, strings.NewReader(rome), 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.Type() != Area || s.Entries() != 2 {
		t.Errorf("type %s, %d entries", s.Type(), s.Entries())
	}
	for _, tc := range []struct {
		name     string
		lat, lon float64
		want     bool
	}{
		{"Rome", 41.8919, 12.5113, true},
		{"Berlin, around the hole", 52.35, 13.2, true},
		{"Berlin, in the hole", 52.52, 13.405, false},
		{"San Francisco", 37.7749, -122.4194, false},
	} {
		if got := s.ContainsPoint(tc.lat, tc.lon); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	// The other lookups find nothing in an area set, and an area lookup
	// nothing in another set.
	if s.ContainsString("Rome") || s.ContainsASN(64496) {
		t.Error("an area set holds a value")
	}
	hosts, _ := FromValues(String, []string{"api.example.com"}, 10)
	if hosts.ContainsPoint(41.8919, 12.5113) {
		t.Error("a string set holds a point")
	}

	// A polygon is an entry: a Polygon counts one, a MultiPolygon one for
	// each of its polygons.
	if _, err := Read(Area, strings.NewReader(rome), 1); err == nil || err.Error() != "line 3, column 99: feature 2, polygon 1: more polygons than max_entries" {
		t.Errorf("more polygons than max_entries: %v", err)
	}
	// An empty collection is an empty set; so is an area set of no values.
	empty, err := Read(Area, strings.NewReader(`{"type":"FeatureCollection","features":[]}`), 10)
	if err != nil || empty.Entries() != 0 || empty.ContainsPoint(41.8919, 12.5113) {
		t.Errorf("an empty collection: %v", err)
	}
	none, err := FromValues(Area, nil, 10)
	if err != nil || none.Type() != Area || none.Entries() != 0 || none.ContainsPoint(0, 0) {
		t.Errorf("an area set of no values: %v", err)
	}
	if _, err := FromValues(Area, []string{"x"}, 10); err == nil || err.Error() != "value 1: an area set has no values: it reads a GeoJSON document" {
		t.Errorf("an area set of a value: %v", err)
	}
	if err := ParseValue(Area, "41.9,12.5"); err == nil {
		t.Error("ParseValue accepted a value of an area set")
	}
}

func TestInvalidArea(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"not JSON", "office: 192.0.2.0/24\n", "line 1, column 1: invalid JSON: expected a value"},
		{"a list of values", "192.0.2.0/24\n", "line 1, column 1: expected a GeoJSON object"},
		{"empty", "", "line 1, column 1: the content is empty"},
		{"ring not closed", "{\"type\":\"Polygon\",\n\"coordinates\":[[[0,0],[1,0],[1,1],[0,1]]]}", "line 2, column 16: ring 1: the ring is not closed: its last position differs from its first"},
	} {
		_, err := Read(Area, strings.NewReader(tc.doc), 10)
		var invalid *Error
		if !errors.As(err, &invalid) || err.Error() != tc.want {
			t.Errorf("%s: got %v, want %s", tc.name, err, tc.want)
		}
	}
	_, err := Read(Area, strings.NewReader("{\"type\":\"Point\"}"), 10)
	var invalid *Error
	if !errors.As(err, &invalid) || invalid.Line != 1 || invalid.Column != 9 || invalid.Path != "" || !strings.HasPrefix(invalid.Problem, "a Point is not allowed") {
		t.Errorf("the fields of the error: %+v", invalid)
	}

	// The ceiling of the content: a document of exactly maxBytes is read,
	// one byte more is too large, even when it is valid.
	doc := `{"type":"FeatureCollection","features":[]}`
	if _, err := read(Area, strings.NewReader(doc), 10, int64(len(doc))); err != nil {
		t.Errorf("a document of maxBytes: %v", err)
	}
	if _, err := read(Area, strings.NewReader(doc+" "), 10, int64(len(doc))); err == nil || err.Error() != "the content is larger than 256 MiB" {
		t.Errorf("a document larger than maxBytes: %v", err)
	}
	// A failure of the reader is a *ReadError, not invalid content.
	_, err = Read(Area, io.MultiReader(strings.NewReader(`{"type":`), failingReader{}), 10)
	var readErr *ReadError
	if !errors.As(err, &readErr) || errors.As(err, &invalid) {
		t.Errorf("a failing reader: %v", err)
	}
}

func TestAreaStore(t *testing.T) {
	store, err := NewStore([]Declaration{{Name: "stores", Type: Area, Source: SourceFile, OnUnavailable: Fail}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Read(Area, strings.NewReader(rome), 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Replace("stores", s); err != nil {
		t.Fatal(err)
	}
	cidr, _ := FromValues(CIDR, nil, 10)
	if err := store.Replace("stores", cidr); err == nil {
		t.Error("a cidr copy replaced an area set")
	}
	if e, ok := store.Get("stores"); !ok || !e.Available || !e.Set.ContainsPoint(41.8919, 12.5113) {
		t.Errorf("the area set in the store: %+v", e)
	}
}

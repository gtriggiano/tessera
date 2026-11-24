//go:build e2e

package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gtriggiano/tessera/internal/mmdbtest"
)

// The areas of TestEnvoyAreas after the first document, the example of the
// documentation with Rome and Milan: San Francisco and Oakland, in one
// MultiPolygon, and a ring around Rome with a hole over its center, where the
// example geo database puts 192.0.2.0/24. Three polygons.
const (
	bayAreas = `{"type":"FeatureCollection","features":[
{"type":"Feature","properties":{"name":"Bay"},"geometry":{"type":"MultiPolygon","coordinates":[
  [[[-122.52,37.70],[-122.35,37.70],[-122.35,37.83],[-122.52,37.83],[-122.52,37.70]]],
  [[[-122.30,37.75],[-122.20,37.75],[-122.20,37.85],[-122.30,37.85],[-122.30,37.75]]]]}},
{"type":"Feature","properties":{"name":"Around Rome"},"geometry":{"type":"Polygon","coordinates":[
  [[12.20,41.70],[12.80,41.70],[12.80,42.10],[12.20,42.10],[12.20,41.70]],
  [[12.40,41.80],[12.40,42.00],[12.60,42.00],[12.60,41.80],[12.40,41.80]]]}}
]}
`
	// An invalid document: its last ring is not closed. Applied in part, it
	// would leave San Francisco out.
	notClosed = `{"type":"Polygon","coordinates":[[[-122.52,37.70],[-122.35,37.70],[-122.35,37.83],[-122.52,37.83],[-122.50,37.70]]]}
`
)

// TestEnvoyAreas decides requests through Envoy with a deny rule on an area
// set read from a GeoJSON file, on the location that the example geo
// database gives to the client address: a location inside an area is denied,
// one outside is allowed, and so is an address without a location, without
// an error. A document renamed over the file in use, in a mounted directory,
// changes the next decisions without a restart; an invalid one changes
// nothing, and shows in one set_load_failed event that does not repeat the
// document.
func TestEnvoyAreas(t *testing.T) {
	e := setUp(t)
	config, err := filepath.Abs(filepath.Join("testdata", "area.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The directory of the database and of the areas, mounted as a volume,
	// as for TestEnvoySets: readable by the user of the image, 65532.
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// rename makes the file next readable by all, and renames it over name.
	rename := func(next, name string) {
		t.Helper()
		if err := os.Chmod(next, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	// replace writes content to a new file and renames it over
	// closed.geojson.
	replace := func(content string) {
		t.Helper()
		next := filepath.Join(dir, ".closed.geojson.new")
		if err := os.WriteFile(next, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		rename(next, "closed.geojson")
	}
	// The example geo database: 192.0.2.0/24 in Rome (41.8919, 12.5113);
	// 198.51.100.0/24 in San Francisco (37.7749, -122.4194); 203.0.113.0/26
	// in Japan, without a location; 203.0.113.128/25 in no record;
	// 2001:db8::/48 in Berlin (52.52, 13.405).
	database := filepath.Join(dir, ".geo.mmdb.new")
	if err := mmdbtest.CityDatabase(6).WriteFile(database); err != nil {
		t.Fatal(err)
	}
	rename(database, "geo.mmdb")
	cities, err := os.ReadFile(filepath.Join(repoRoot, "docs", "examples", "cities.geojson"))
	if err != nil {
		t.Fatal(err)
	}
	replace(string(cities))
	tessera, front := e.start(t, "areas", config, dir+":/etc/tessera/data:ro")
	waitEvent(t, tessera, "set_loaded", func(event map[string]any) bool {
		return event["set"] == "closed" && event["set_type"] == "area" && event["set_source"] == "file" && event["entries"] == float64(2)
	})

	check := func(path, client string, status int, rule string) {
		t.Helper()
		if got, _, _ := reflected(t, front, path, client); got != status {
			t.Errorf("from %s: status %d, want %d", client, got, status)
		}
		event := decisionEvents(t, tessera, []string{"/reflect/" + path})["/reflect/"+path]
		if event["rule"] != rule || event["client_ip"] != client || event["error"] != nil {
			t.Errorf("from %s: decision event %v, want rule %s, without an error", client, event, rule)
		}
	}
	// Rome is in the first document, the other locations are not; an
	// address without a location is in no area, and the deny rule does not
	// deny it.
	check("area/1", "192.0.2.10", http.StatusUnavailableForLegalReasons, "closed-areas")
	check("area/2", "198.51.100.7", http.StatusOK, "default")
	check("area/3", "2001:db8::10", http.StatusOK, "default")
	check("area/4", "203.0.113.10", http.StatusOK, "default")
	check("area/5", "203.0.113.200", http.StatusOK, "default")

	// A new document, renamed over the file in use: San Francisco is in
	// it, Rome in the hole of its ring.
	replace(bayAreas)
	waitStatus(t, front, "198.51.100.7", http.StatusUnavailableForLegalReasons)
	waitEvent(t, tessera, "set_loaded", func(event map[string]any) bool {
		return event["set"] == "closed" && event["set_type"] == "area" && event["entries"] == float64(3)
	})
	check("area/6", "198.51.100.7", http.StatusUnavailableForLegalReasons, "closed-areas")
	check("area/7", "192.0.2.10", http.StatusOK, "default")
	check("area/8", "203.0.113.10", http.StatusOK, "default")

	// An invalid document: it is rejected, and the copy in use stays.
	replace(notClosed)
	failed := waitEvent(t, tessera, "set_load_failed", func(event map[string]any) bool {
		return event["set"] == "closed" && event["set_type"] == "area" && event["set_source"] == "file"
	})
	const problem = ": ring 1: the ring is not closed: its last position differs from its first"
	if got, _ := failed["error"].(string); !strings.HasPrefix(got, "line 1, column ") || !strings.HasSuffix(got, " of the file"+problem) || strings.Contains(got, "122") {
		t.Errorf("set_load_failed error %q, want the line, the column and the problem only", got)
	}
	check("area/9", "198.51.100.7", http.StatusUnavailableForLegalReasons, "closed-areas")
	check("area/10", "192.0.2.10", http.StatusOK, "default")

	// All of it without a restart, and no lookup in the database or in the
	// areas failed: the requests without X-Forwarded-For with which start
	// waits for Envoy have no client address.
	if state := docker(t, "inspect", "--format", "{{.State.Running}} {{.RestartCount}}", tessera); state != "true 0" {
		t.Errorf("tessera: running and restarts %q, want \"true 0\"", state)
	}
	if n := len(events(t, tessera, "starting")); n != 1 {
		t.Errorf("%d starting events in the logs of tessera, want 1", n)
	}
	for _, event := range events(t, tessera, "evaluation_error") {
		if event["cause"] != "client_ip_missing" {
			t.Errorf("evaluation_error event %v", event)
		}
	}
}

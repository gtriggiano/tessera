package app

import (
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/gtriggiano/tessera/internal/mmdbtest"
)

// areaServeConfig reads the area set stores from storesPath and the example
// geo database from geoPath: a request from a store is allowed, any other
// denied.
func areaServeConfig(storesPath, geoPath string) string {
	return `server:
  listen: ` + grpcPlaceholder + `
  drain_delay: 0s
admin:
  listen: ` + adminPlaceholder + `
client_ip:
  from: x_forwarded_for
  trusted_hops: 0
log:
  decisions: none
enrichment:
  geo:
    database: ` + geoPath + `
sets:
  stores:
    type: area
    file: ` + storesPath + `
    refresh: 1s
policy:
  default_action: deny
  rules:
    - name: stores
      action: allow
      when: sets.stores.contains(geo.location)
`
}

// The areas around Rome and San Francisco, where the example geo database
// puts 192.0.2.0/24 and 198.51.100.0/24.
const (
	romeArea          = `{"type":"Polygon","coordinates":[[[12.35,41.80],[12.62,41.80],[12.62,42.00],[12.35,42.00],[12.35,41.80]]]}`
	sanFranciscoAreas = `{"type":"FeatureCollection","features":[{"type":"Feature","properties":{},"geometry":{"type":"MultiPolygon","coordinates":[[[[-122.52,37.70],[-122.35,37.70],[-122.35,37.83],[-122.52,37.83],[-122.52,37.70]]],[[[-122.30,37.75],[-122.20,37.75],[-122.20,37.85],[-122.30,37.85],[-122.30,37.75]]]]}}]}`
	fromRome          = "192.0.2.7"
	fromSanFrancisco  = "198.51.100.7"
	withoutLocation   = "203.0.113.7"
	storesLabels      = `set="stores",source="file",type="area"`
)

// The exit criterion of the areas in serve: a point inside and one outside;
// a new document changes the decisions without a restart; an invalid one
// changes nothing, shows in the metrics and in one set_load_failed event.
func TestAreaSetRefresh(t *testing.T) {
	stores := writeSetFile(t, romeArea)
	geo := filepath.Join(t.TempDir(), "geo.mmdb")
	writeDatabase(t, geo, mmdbtest.CityDatabase(6))
	h := launch(t, areaServeConfig(stores, geo), Options{setTime: fastSets})
	grpcAddress, adminAddress := h.addresses()
	conn := dialGRPC(t, grpcAddress)
	wantDecisions(t, conn, map[string]codes.Code{fromRome: codes.OK, fromSanFrancisco: codes.PermissionDenied, withoutLocation: codes.PermissionDenied})
	m := scrape(t, adminAddress)
	for series, want := range map[string]float64{
		"tessera_set_entries{" + storesLabels + "}":                          1,
		"tessera_set_available{" + storesLabels + "}":                        1,
		`tessera_set_refreshes_total{result="updated",` + storesLabels + `}`: 1,
	} {
		if m[series] != want {
			t.Errorf("%s = %v, want %v", series, m[series], want)
		}
	}

	// A new document changes the decisions: its two polygons are two
	// entries.
	replaceSetFile(t, stores, sanFranciscoAreas)
	waitUntil(t, "the new document", func() bool { return decide(t, conn, fromSanFrancisco) == codes.OK })
	wantDecisions(t, conn, map[string]codes.Code{fromRome: codes.PermissionDenied})
	if got := scrape(t, adminAddress)["tessera_set_entries{"+storesLabels+"}"]; got != 2 {
		t.Errorf("entries %v, want 2", got)
	}

	// An invalid document changes nothing.
	rejected := `tessera_set_refreshes_total{result="rejected",` + storesLabels + `}`
	replaceSetFile(t, stores, strings.Replace(romeArea, ",[12.35,41.80]]]}", "]]}", 1))
	waitUntil(t, "three rejected checks", func() bool { return scrape(t, adminAddress)[rejected] >= 3 })
	wantDecisions(t, conn, map[string]codes.Code{fromSanFrancisco: codes.OK, fromRome: codes.PermissionDenied})
	if got := scrape(t, adminAddress)["tessera_set_entries{"+storesLabels+"}"]; got != 2 {
		t.Errorf("entries %v after a rejected document, want 2", got)
	}

	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	wantEvents(t, setEvents(h), []string{
		`level=info msg=set_loaded set=stores set_type=area set_source=file entries=1`,
		`level=info msg=set_loaded set=stores set_type=area set_source=file entries=2`,
		`level=warn msg=set_load_failed set=stores set_type=area set_source=file error="line 1, column 34 of the file: ring 1: the ring is not closed: its last position differs from its first"`,
	})
}

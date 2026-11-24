package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gtriggiano/tessera/internal/mmdbtest"
)

// areaConfig reads the area set stores from stores.geojson and the geo
// database from geo.mmdb, next to the configuration.
const areaConfig = `enrichment:
  geo:
    database: geo.mmdb
sets:
  stores:
    type: area
    file: stores.geojson
policy:
  default_action: deny
  rules:
    - name: stores
      action: allow
      when: sets.stores.contains(geo.location)
`

// romeStores is an area around Rome, where the example geo database puts
// 192.0.2.0/24.
const romeStores = `{"type":"FeatureCollection","features":[
  {"type":"Feature","properties":{"name":"Rome"},"geometry":{"type":"Polygon","coordinates":[[[12.35,41.80],[12.62,41.80],[12.62,42.00],[12.35,42.00],[12.35,41.80]]]}}
]}
`

// writeArea writes areaConfig, the example geo database and stores in a
// directory of their own, and returns the path of the configuration.
func writeArea(t *testing.T, stores string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"config.yaml": areaConfig, "stores.geojson": stores} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := mmdbtest.CityDatabase(6).WriteFile(filepath.Join(dir, "geo.mmdb")); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "config.yaml")
}

// An invalid document is a configuration error, in config validate, eval and
// serve, with its line, its column and its place in the structure, never its
// content.
func TestConfigValidateArea(t *testing.T) {
	code, out, errOut := execute([]string{"config", "validate", "--config", writeArea(t, romeStores)})
	if code != 0 || out != "configuration valid\n" || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, tc := range []struct{ name, doc, want string }{
		{"ring not closed", strings.Replace(romeStores, ",[12.35,41.80]]]}}", "]]}}", 1), "tessera: sets.stores.file (line 7, column 11): line 2, column 94 of the file: feature 1, ring 1: the ring is not closed: its last position differs from its first\n"},
		{"point", `{"type":"Feature","properties":{"name":"s3cr3t"},"geometry":{"type":"Point","coordinates":[12.5,41.9]}}`, "tessera: sets.stores.file (line 7, column 11): line 1, column 69 of the file: a Point is not allowed: a geometry must be a Polygon or a MultiPolygon\n"},
		{"not JSON", "s3cr3t\n", "tessera: sets.stores.file (line 7, column 11): line 1, column 1 of the file: invalid JSON: expected a value\n"},
		{"empty", "", "tessera: sets.stores.file (line 7, column 11): line 1, column 1 of the file: the content is empty\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeArea(t, tc.doc)
			code, out, errOut := execute([]string{"config", "validate", "--config", path})
			if code != 2 || out != "" || errOut != tc.want {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
			if code, out, errOut := execute([]string{"eval", "--config", path, "--ip", "192.0.2.7"}); code != 2 || out != "" || errOut != tc.want {
				t.Fatalf("eval: code=%d stdout=%q stderr=%q", code, out, errOut)
			}
			var stdout, stderr bytes.Buffer
			code = Run(context.Background(), []string{"serve", "--config", path}, Options{Out: &stdout, Err: &stderr, Serve: noServe(t)})
			if code != 2 || stdout.Len() != 0 || stderr.String() != tc.want {
				t.Fatalf("serve: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
	// An area set has no values.
	values := writeConfig(t, "sets:\n  stores:\n    type: area\n    values: [s3cr3t]\npolicy: {default_action: deny}\n")
	if code, out, errOut := execute([]string{"config", "validate", "--config", values}); code != 2 || out != "" || errOut != "tessera: sets.stores.values (line 4, column 13): not allowed with type area: an area set reads a GeoJSON file\n" {
		t.Fatalf("values: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestEvalArea(t *testing.T) {
	path := writeArea(t, romeStores)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"inside", []string{"--ip", "192.0.2.7"}, "geo.location  {lat: 41.8919, lon: 12.5113}\n\nstores  true -> allow\n\ndecision: allow (rule stores, reason rule)\n"},
		{"outside", []string{"--ip", "198.51.100.7"}, "geo.location  {lat: 37.7749, lon: -122.4194}\n\nstores  false\n\ndecision: deny (rule default, reason default)\nresponse: status 403\n"},
		{"without a location", []string{"--ip", "203.0.113.7"}, "geo.location  missing\n\nstores  false\n\ndecision: deny (rule default, reason default)\nresponse: status 403\n"},
		{"without a client address", nil, "geo.location  missing\n\nstores  indeterminate: client_ip_missing\n\ndecision: deny (rule default, reason default)\nresponse: status 403\n"},
		{"empty area", []string{"--ip", "192.0.2.7", "--set", "stores="}, "geo.location  {lat: 41.8919, lon: 12.5113}\n\nstores  false\n\ndecision: deny (rule default, reason default)\nresponse: status 403\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := execute(append([]string{"eval", "--config", path}, tc.args...))
			if code != 0 || out != tc.want || errOut != "" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}
	// An empty area does not read the file, which can be missing; a value
	// is a usage error, which does not repeat it.
	if err := os.Remove(filepath.Join(filepath.Dir(path), "stores.geojson")); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := execute([]string{"eval", "--config", path, "--ip", "192.0.2.7", "--set", "stores="}); code != 0 || !strings.Contains(out, "stores  false\n") || errOut != "" {
		t.Fatalf("empty area without its file: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut := execute([]string{"eval", "--config", path, "--ip", "192.0.2.7", "--set", "stores=s3cr3t"})
	if code != 2 || out != "" || errOut != "tessera: invalid --set for the set stores: an area set takes no values; --set stores= makes it empty\n" {
		t.Fatalf("a value: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

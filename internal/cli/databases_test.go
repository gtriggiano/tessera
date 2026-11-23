package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gtriggiano/tessera/internal/app"
	"github.com/gtriggiano/tessera/internal/config"
	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/mmdbtest"
)

// databasesConfig reads geo.mmdb and asn.mmdb next to the configuration;
// no rule reads asn.
const databasesConfig = `enrichment:
  geo:
    database: geo.mmdb
  asn:
    database: asn.mmdb
policy:
  default_action: deny
  rules:
    - name: europe
      action: allow
      when: geo.country.in_eu
    - name: location
      action: allow
      when: has(geo.location) && geo.location.lon < 0.0
`

// writeDatabases writes the configuration yaml, the geo database geo and the
// asn database asn in a directory of their own, and returns the path of the
// configuration.
func writeDatabases(t *testing.T, yaml string, geo, asn *mmdbtest.Writer) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, w := range map[string]*mmdbtest.Writer{"geo.mmdb": geo, "asn.mmdb": asn} {
		if err := w.WriteFile(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "config.yaml")
}

func TestConfigValidateDatabases(t *testing.T) {
	path := writeDatabases(t, databasesConfig, mmdbtest.CityDatabase(6), mmdbtest.ASNDatabase())
	// A database that no rule reads is a warning: the configuration stays
	// valid.
	code, out, errOut := execute([]string{"config", "validate", "--config", path})
	if code != 0 || out != "configuration valid\n" || errOut != "tessera: warning: enrichment.asn (line 4, column 3): the database is not used by any rule, upstream header or decision field\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}

	// Every database is read: missing or invalid, it is a configuration
	// error at its key, without its path.
	dir := filepath.Dir(path)
	if err := os.WriteFile(filepath.Join(dir, "asn.mmdb"), []byte("AS64496\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = execute([]string{"config", "validate", "--config", path})
	if code != 2 || out != "" || errOut != "tessera: enrichment.asn.database (line 5, column 15): the file is not a valid MMDB database\n" {
		t.Fatalf("invalid database: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if err := os.Remove(filepath.Join(dir, "geo.mmdb")); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = execute([]string{"config", "validate", "--config", path})
	if code != 2 || out != "" || errOut != "tessera: enrichment.geo.database (line 3, column 15): the file does not exist\n" {
		t.Fatalf("missing database: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	// config print reads no database, and prints the blocks with their
	// defaults.
	code, out, errOut = execute([]string{"config", "print", "--config", path})
	if code != 0 || errOut != "" || !strings.Contains(out, "enrichment:\n  geo:\n    database: "+filepath.Join(dir, "geo.mmdb")+"\n    refresh: 1m0s\n  asn:\n") {
		t.Fatalf("config print: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	// A rule on geo without enrichment.geo stops the configuration at its
	// position.
	code, out, errOut = execute([]string{"config", "validate", "--config", writeConfig(t, "policy:\n  default_action: deny\n  rules:\n    - name: europe\n      action: allow\n      when: request.host == \"www.example.com\" && geo.country.in_eu\n")})
	if code != 2 || out != "" || errOut != "tessera: policy.rules[0].when (line 6, column 50): geo is not available: the configuration has no enrichment.geo\n" {
		t.Fatalf("geo without enrichment.geo: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

// serve loads every database before it runs: an invalid one is a
// configuration error, and the loaded databases reach the server.
func TestServeDatabases(t *testing.T) {
	path := writeDatabases(t, databasesConfig, mmdbtest.CityDatabase(6), mmdbtest.New("GeoLite2-ASN", 1))
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "geo.mmdb"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"serve", "--config", path}, Options{Out: &stdout, Err: &stderr, Serve: noServe(t)})
	if code != 2 || stdout.Len() != 0 || stderr.String() != "tessera: enrichment.geo.database (line 3, column 15): the file is not a valid MMDB database\n" {
		t.Fatalf("invalid database: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	path = writeDatabases(t, databasesConfig, mmdbtest.CityDatabase(6), mmdbtest.ASNDatabase())
	var got app.Options
	stderr.Reset()
	code = Run(context.Background(), []string{"serve", "--config", path}, Options{Out: &stdout, Err: &stderr, Serve: func(_ *config.Config, opts app.Options) error {
		got = opts
		return nil
	}})
	if code != 0 || got.Databases == nil {
		t.Fatalf("code=%d stderr=%q databases=%v", code, stderr.String(), got.Databases)
	}
	if db := got.Databases.Store.Snapshot().Database(enrichment.Geo); db == nil || db.Type() != "GeoIP2-City" || len(got.Databases.Files) != 2 {
		t.Errorf("databases %+v", got.Databases)
	}
}

func TestEvalDatabases(t *testing.T) {
	path := writeDatabases(t, databasesConfig, mmdbtest.CityDatabase(6), mmdbtest.ASNDatabase())
	for _, tc := range []struct {
		name, ip, want string
	}{
		{"in the European Union", "192.0.2.7", `geo.country.in_eu  true

europe  true -> allow

decision: allow (rule europe, reason rule)
`},
		{"with a location", "198.51.100.7", `geo.country.in_eu  false
geo.location       {lat: 37.7749, lon: -122.4194}
geo.location.lon   -122.4194

europe    false
location  true -> allow

decision: allow (rule location, reason rule)
`},
		{"without a location", "203.0.113.7", `geo.country.in_eu  false
geo.location       missing
geo.location.lon   missing

europe    false
location  false

decision: deny (rule default, reason default)
response: status 403
`},
		{"without a client address", "", `geo.country.in_eu  missing
geo.location       missing
geo.location.lon   missing

europe    indeterminate: client_ip_missing
location  indeterminate: client_ip_missing

decision: deny (rule default, reason default)
response: status 403
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"eval", "--config", path}
			if tc.ip != "" {
				args = append(args, "--ip", tc.ip)
			}
			code, out, errOut := execute(args)
			if code != 0 || errOut != "" || out != tc.want {
				t.Fatalf("code=%d stderr=%q stdout:\n%s\nwant:\n%s", code, errOut, out, tc.want)
			}
		})
	}

	// Every attribute of geo and asn: the strings quoted in the text, the
	// objects and the list read whole as CEL literals, and, in JSON, every
	// value as a string.
	every := writeDatabases(t, `enrichment:
  geo: {database: geo.mmdb}
  asn: {database: asn.mmdb}
policy:
  default_action: deny
  rules:
    - name: all
      action: allow
      when: |
        geo.found && geo.country.code != "" && geo.country.name != "" && geo.country.in_eu
          && geo.continent.code != "" && geo.continent.name != "" && geo.subdivisions.exists(s, s.code == "RM")
          && geo.city != "" && geo.postal_code != "" && geo.timezone != ""
          && geo.country == geo.country && geo.continent == geo.continent && geo.location == geo.location
          && asn.found && asn.number > 0 && asn.organization != ""
`, mmdbtest.CityDatabase(6), mmdbtest.ASNDatabase())
	code, out, errOut := execute([]string{"eval", "--config", every, "--ip", "192.0.2.7"})
	want := `asn.found           true
asn.number          64496
asn.organization    "Example Transit"
geo.city            "Rome"
geo.continent       {code: "EU", name: "Europe"}
geo.continent.code  "EU"
geo.continent.name  "Europe"
geo.country         {code: "IT", name: "Italy", in_eu: true}
geo.country.code    "IT"
geo.country.in_eu   true
geo.country.name    "Italy"
geo.found           true
geo.location        {lat: 41.8919, lon: 12.5113}
geo.postal_code     "00118"
geo.subdivisions    [{code: "62", name: "Lazio"}, {code: "RM", name: "Rome"}]
geo.timezone        "Europe/Rome"

all  true -> allow

decision: allow (rule all, reason rule)
`
	if code != 0 || errOut != "" || out != want {
		t.Fatalf("code=%d stderr=%q stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
	code, out, _ = execute([]string{"eval", "--config", every, "--ip", "192.0.2.7", "--output", "json"})
	if code != 0 || !strings.HasPrefix(out, `{"attributes":[{"name":"asn.found","value":"true"},{"name":"asn.number","value":"64496"},{"name":"asn.organization","value":"Example Transit"},{"name":"geo.city","value":"Rome"},{"name":"geo.continent","value":"{code: \"EU\", name: \"Europe\"}"},`) ||
		!strings.Contains(out, `{"name":"geo.location","value":"{lat: 41.8919, lon: 12.5113}"},{"name":"geo.postal_code","value":"00118"},{"name":"geo.subdivisions","value":"[{code: \"62\", name: \"Lazio\"}, {code: \"RM\", name: \"Rome\"}]"},`) {
		t.Fatalf("code=%d stdout=%s", code, out)
	}
	// An address that the database does not have: empty strings, an empty
	// list, no location.
	code, out, _ = execute([]string{"eval", "--config", every, "--ip", "203.0.113.200"})
	want = `asn.found           false
asn.number          0
asn.organization    ""
geo.city            ""
geo.continent       {code: "", name: ""}
geo.continent.code  ""
geo.continent.name  ""
geo.country         {code: "", name: "", in_eu: false}
geo.country.code    ""
geo.country.in_eu   false
geo.country.name    ""
geo.found           false
geo.location        missing
geo.postal_code     ""
geo.subdivisions    []
geo.timezone        ""

all  false

decision: deny (rule default, reason default)
response: status 403
`
	if code != 0 || out != want {
		t.Fatalf("not found: code=%d stdout:\n%s\nwant:\n%s", code, out, want)
	}
	// Without a client address, and when the lookup fails, every attribute
	// is missing, the objects and the list included.
	brokenEvery := filepath.Join(t.TempDir(), "config.yaml")
	data, err := os.ReadFile(every)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brokenEvery, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, w := range map[string]*mmdbtest.Writer{"geo.mmdb": mmdbtest.BrokenDatabase("geo"), "asn.mmdb": mmdbtest.BrokenDatabase("asn")} {
		if err := w.WriteFile(filepath.Join(filepath.Dir(brokenEvery), name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ config, ip, cause string }{{every, "", "client_ip_missing"}, {brokenEvery, "198.51.100.7", "database_error"}} {
		args := []string{"eval", "--config", tc.config}
		if tc.ip != "" {
			args = append(args, "--ip", tc.ip)
		}
		code, out, _ = execute(args)
		var b strings.Builder
		for _, name := range []string{"asn.found", "asn.number", "asn.organization", "geo.city", "geo.continent", "geo.continent.code", "geo.continent.name", "geo.country", "geo.country.code", "geo.country.in_eu", "geo.country.name", "geo.found", "geo.location", "geo.postal_code", "geo.subdivisions", "geo.timezone"} {
			fmt.Fprintf(&b, "%-18s  missing\n", name)
		}
		want = b.String() + "\nall  indeterminate: " + tc.cause + "\n\ndecision: deny (rule default, reason default)\nresponse: status 403\n"
		if code != 0 || out != want {
			t.Fatalf("%s: code=%d stdout:\n%s\nwant:\n%s", tc.cause, code, out, want)
		}
	}
	// Any read of a subdivision shows the list; the strings of a literal
	// are quoted as in the text.
	w := mmdbtest.New("GeoIP2-City", mmdbtest.BuildEpoch)
	if err := w.Insert(netip.MustParsePrefix("192.0.2.0/24"), map[string]any{
		"country":      map[string]any{"iso_code": "FR", "names": map[string]any{"en": "France"}},
		"subdivisions": []any{map[string]any{"iso_code": "PAC", "names": map[string]any{"en": "Provence-Alpes-Côte d'Azur \"\t"}}},
	}); err != nil {
		t.Fatal(err)
	}
	indexed := writeDatabases(t, `enrichment:
  geo: {database: geo.mmdb}
  asn: {database: asn.mmdb}
policy:
  default_action: deny
  rules:
    - name: last
      action: allow
      when: geo.subdivisions[size(geo.subdivisions) - 1].code == "PAC"
`, w, mmdbtest.ASNDatabase())
	code, out, _ = execute([]string{"eval", "--config", indexed, "--ip", "192.0.2.7"})
	if want := "geo.subdivisions  [{code: \"PAC\", name: \"Provence-Alpes-Côte d'Azur \\\"\\t\"}]\n\nlast  true -> allow\n"; code != 0 || !strings.HasPrefix(out, want) {
		t.Fatalf("indexed: code=%d stdout:\n%s\nwant:\n%s", code, out, want)
	}

	// A double always has a decimal point, as a leaf and inside a literal,
	// in the text and in JSON: a whole-number coordinate prints 52.0.
	whole := mmdbtest.New("GeoIP2-City", mmdbtest.BuildEpoch)
	if err := whole.Insert(netip.MustParsePrefix("192.0.2.0/24"), map[string]any{
		"country":  map[string]any{"iso_code": "DE"},
		"location": map[string]any{"latitude": 52.0, "longitude": -13.0},
	}); err != nil {
		t.Fatal(err)
	}
	coordinates := writeDatabases(t, `enrichment:
  geo: {database: geo.mmdb}
  asn: {database: asn.mmdb}
policy:
  default_action: deny
  rules:
    - name: north-west
      action: allow
      when: geo.location == geo.location && geo.location.lat > 50.0 && geo.location.lon < 0.0
`, whole, mmdbtest.ASNDatabase())
	code, out, _ = execute([]string{"eval", "--config", coordinates, "--ip", "192.0.2.7"})
	want = `geo.location      {lat: 52.0, lon: -13.0}
geo.location.lat  52.0
geo.location.lon  -13.0

north-west  true -> allow

decision: allow (rule north-west, reason rule)
`
	if code != 0 || out != want {
		t.Fatalf("whole coordinates: code=%d stdout:\n%s\nwant:\n%s", code, out, want)
	}
	code, out, _ = execute([]string{"eval", "--config", coordinates, "--ip", "192.0.2.7", "--output", "json"})
	if code != 0 || !strings.HasPrefix(out, `{"attributes":[{"name":"geo.location","value":"{lat: 52.0, lon: -13.0}"},{"name":"geo.location.lat","value":"52.0"},{"name":"geo.location.lon","value":"-13.0"}],`) {
		t.Fatalf("whole coordinates, JSON: code=%d stdout=%s", code, out)
	}

	// A lookup that fails shows the attribute as missing, and the cause
	// database_error.
	broken := writeDatabases(t, databasesConfig, mmdbtest.BrokenDatabase("geo"), mmdbtest.ASNDatabase())
	code, out, _ = execute([]string{"eval", "--config", broken, "--ip", "198.51.100.7"})
	want = `geo.country.in_eu  missing
geo.location       missing
geo.location.lon   missing

europe    indeterminate: database_error
location  indeterminate: database_error

decision: deny (rule default, reason default)
response: status 403
`
	if code != 0 || out != want {
		t.Fatalf("code=%d stdout:\n%s\nwant:\n%s", code, out, want)
	}

	// A missing database is a configuration error.
	if err := os.Remove(filepath.Join(filepath.Dir(path), "asn.mmdb")); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = execute([]string{"eval", "--config", path, "--ip", "192.0.2.7"})
	if code != 2 || out != "" || errOut != "tessera: enrichment.asn.database (line 5, column 15): the file does not exist\n" {
		t.Fatalf("missing database: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

// A database of the other kind stops config validate and serve, as an
// invalid file does: an ASN file as geo, a City file as asn.
func TestSwappedDatabases(t *testing.T) {
	for _, tc := range []struct {
		name     string
		geo, asn *mmdbtest.Writer
		err      string
	}{
		{"ASN as geo", mmdbtest.ASNDatabase(), mmdbtest.ASNDatabase(), "tessera: enrichment.geo.database (line 3, column 15): a record of the database does not have the schema of a GeoIP2 City or Country database\n"},
		{"City as asn", mmdbtest.CityDatabase(6), mmdbtest.CityDatabase(6), "tessera: enrichment.asn.database (line 5, column 15): a record of the database does not have the schema of a GeoLite2 ASN database\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeDatabases(t, databasesConfig, tc.geo, tc.asn)
			code, out, errOut := execute([]string{"config", "validate", "--config", path})
			if code != 2 || out != "" || errOut != tc.err {
				t.Fatalf("config validate: code=%d stdout=%q stderr=%q", code, out, errOut)
			}
			var stdout, stderr bytes.Buffer
			code = Run(context.Background(), []string{"serve", "--config", path}, Options{Out: &stdout, Err: &stderr, Serve: noServe(t)})
			if code != 2 || stdout.Len() != 0 || stderr.String() != tc.err {
				t.Fatalf("serve: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

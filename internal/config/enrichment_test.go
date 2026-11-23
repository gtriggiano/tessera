package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/enrichment"
)

const enrichmentConfig = `
enrichment:
  geo:
    database: geo.mmdb
    refresh: 10m
  asn:
    database: /var/lib/geoip/asn.mmdb
policy:
  default_action: deny
  rules:
    - name: italy
      action: allow
      when: geo.country.code == "IT"
`

func TestEnrichment(t *testing.T) {
	c := mustParse(t, enrichmentConfig, nil)
	want := Enrichment{
		Geo: &Database{Database: "geo.mmdb", Refresh: 10 * time.Minute},
		ASN: &Database{Database: "/var/lib/geoip/asn.mmdb", Refresh: time.Minute},
	}
	if !reflect.DeepEqual(c.Enrichment, want) {
		t.Fatalf("got %+v %+v", c.Enrichment.Geo, c.Enrichment.ASN)
	}
	if got := c.DatabaseKinds(); !slices.Equal(got, []enrichment.Kind{enrichment.Geo, enrichment.ASN}) {
		t.Errorf("kinds %v", got)
	}
	if c.Enrichment.Database(enrichment.ASN) != c.Enrichment.ASN || c.Enrichment.Database("ua") != nil {
		t.Error("Database")
	}
	// The database that no rule reads is a warning, at the key of its
	// block.
	wantWarnings := []Warning{{Path: "enrichment.asn", Line: 6, Column: 3, Problem: "the database is not used by any rule, upstream header or decision field"}}
	if got := c.Warnings(); !reflect.DeepEqual(got, wantWarnings) {
		t.Errorf("warnings %+v", got)
	}

	// Each block is optional: without it, its attribute does not exist.
	for _, tc := range []struct {
		yaml  string
		kinds []enrichment.Kind
	}{
		{"policy: {default_action: deny}\n", nil},
		{"enrichment: {}\npolicy: {default_action: deny}\n", nil},
		{"enrichment: {asn: {database: asn.mmdb}}\npolicy: {default_action: deny}\n", []enrichment.Kind{enrichment.ASN}},
		// The order of the reference, whatever the order of the file.
		{"enrichment: {asn: {database: asn.mmdb}, geo: {database: geo.mmdb}}\npolicy: {default_action: deny}\n", []enrichment.Kind{enrichment.Geo, enrichment.ASN}},
	} {
		c := mustParse(t, tc.yaml, nil)
		if got := c.DatabaseKinds(); !slices.Equal(got, tc.kinds) {
			t.Errorf("%s: kinds %v, want %v", tc.yaml, got, tc.kinds)
		}
	}

	// The warnings of the databases and of the sets follow the order of the
	// file; a database read by a rule in audit mode is used.
	c = mustParse(t, `sets:
  unused: {type: asn, values: [64496]}
enrichment:
  geo: {database: geo.mmdb}
  asn: {database: asn.mmdb}
policy:
  default_action: deny
  rules:
    - name: hosting
      action: deny
      mode: audit
      when: asn.number == 64500
`, nil)
	var got []string
	for _, w := range c.Warnings() {
		got = append(got, w.String())
	}
	if want := []string{
		"sets.unused (line 2, column 3): the set is not used by any rule, upstream header or decision field",
		"enrichment.geo (line 4, column 3): the database is not used by any rule, upstream header or decision field",
	}; !slices.Equal(got, want) {
		t.Errorf("warnings %q", got)
	}
}

func TestInvalidEnrichment(t *testing.T) {
	const tail = "policy: {default_action: deny}\n"
	for _, tc := range []struct{ name, yaml, want string }{
		{"not a mapping", "enrichment: []\n", "enrichment (line 1, column 13): expected a mapping"},
		{"block not a mapping", "enrichment:\n  geo: geo.mmdb\n", "enrichment.geo (line 2, column 8): expected a mapping"},
		{"null block", "enrichment:\n  geo:\n", "enrichment.geo (line 2, column 7): null is not allowed; omit the key to use its default"},
		{"unknown database", "enrichment:\n  ua: {database: ua.mmdb}\n", "enrichment.ua (line 2, column 3): unknown key"},
		{"unknown key", "enrichment:\n  geo: {database: geo.mmdb, max_staleness: 1h}\n", "enrichment.geo.max_staleness (line 2, column 29): unknown key"},
		{"no database", "enrichment:\n  geo:\n    refresh: 1m\n", "enrichment.geo.database (line 2, column 3): required"},
		{"empty block", "enrichment:\n  asn: {}\n", "enrichment.asn.database (line 2, column 3): required"},
		{"empty database", "enrichment:\n  geo: {database: \"\"}\n", "enrichment.geo.database (line 2, column 19): must not be empty"},
		{"control character", "enrichment:\n  geo: {database: \"a\\tb\"}\n", "enrichment.geo.database (line 2, column 19): expected a path"},
		{"refresh too short", "enrichment:\n  asn: {database: asn.mmdb, refresh: 999ms}\n", "enrichment.asn.refresh (line 2, column 38): must be at least 1s"},
		{"refresh without unit", "enrichment:\n  asn: {database: asn.mmdb, refresh: 60}\n", "enrichment.asn.refresh (line 2, column 38): expected a duration with units, such as 5s"},
		{"unset variable", "enrichment:\n  geo: {database: \"${GEO_DATABASE}\"}\n", "enrichment.geo.database (line 2, column 19): environment variable GEO_DATABASE is not set"},
		{"geo without enrichment.geo", "policy:\n  default_action: deny\n  rules:\n    - name: r\n      action: deny\n      when: request.host == \"x\" && geo.country.code == \"IT\"\n", "policy.rules[0].when (line 6, column 36): geo is not available: the configuration has no enrichment.geo"},
		{"asn with only enrichment.geo", "enrichment: {geo: {database: geo.mmdb}}\npolicy:\n  default_action: deny\n  rules:\n    - name: r\n      action: deny\n      when: asn.number == 64496\n", "policy.rules[0].when (line 7, column 13): asn is not available: the configuration has no enrichment.asn"},
		{"a field geo does not have", "enrichment: {geo: {database: geo.mmdb}}\npolicy:\n  default_action: deny\n  rules:\n    - name: r\n      action: deny\n      when: geo.region == \"62\"\n", "policy.rules[0].when (line 7, column 16): undefined field 'region'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := tc.yaml
			if !strings.Contains(yaml, "policy:") {
				yaml += tail
			}
			_, err := parse(t, yaml, nil)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRedactedEnrichment(t *testing.T) {
	c := mustParse(t, enrichmentConfig, nil)
	got, err := c.Redacted()
	if err != nil {
		t.Fatal(err)
	}
	want := `enrichment:
  geo:
    database: geo.mmdb
    refresh: 10m0s
  asn:
    database: /var/lib/geoip/asn.mmdb
    refresh: 1m0s
sets: {}
`
	if !strings.Contains(string(got), want) {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if again := mustParse(t, string(got), nil); !reflect.DeepEqual(again.Enrichment, c.Enrichment) {
		t.Fatalf("reads back as %+v", again.Enrichment)
	}
	// A block that the file does not write is not printed, and a path
	// from the environment is redacted.
	c = mustParse(t, "enrichment:\n  asn: {database: \"${ASN_DATABASE}\"}\npolicy: {default_action: deny}\n", map[string]string{"ASN_DATABASE": "/run/" + secret + ".mmdb"})
	if got, err = c.Redacted(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), secret) || strings.Contains(string(got), "geo") || !strings.Contains(string(got), "enrichment:\n  asn:\n    database: '[REDACTED]'\n    refresh: 1m0s\n") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestLoadDatabasePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(enrichmentConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A relative path is relative to the configuration file; Load reads no
	// database.
	if got := c.Enrichment.Geo.Database; got != filepath.Join(dir, "geo.mmdb") {
		t.Errorf("relative path %q", got)
	}
	if got := c.Enrichment.ASN.Database; got != "/var/lib/geoip/asn.mmdb" {
		t.Errorf("absolute path %q", got)
	}
	if err := c.ErrorAt("enrichment.geo.database", "the file does not exist"); err.Error() != "enrichment.geo.database (line 4, column 15): the file does not exist" {
		t.Errorf("ErrorAt: %v", err)
	}
}

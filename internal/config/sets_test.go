package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/sets"
)

const setsConfig = `
sets:
  office:
    type: cidr
    values:
      - 192.0.2.0/24
      - "2001:db8::/32"
  networks:
    type: asn
    values: [64496, AS64497, "64498"]
    max_entries: 3
  partners:
    type: string
    file: partners.txt
    refresh: 30s
    max_staleness: 1h
    on_unavailable: no_match
    max_entries: 5000
  blocked:
    type: cidr
    file: /etc/tessera/blocked.txt
policy:
  default_action: deny
  rules:
    - name: office
      action: allow
      when: sets.office.contains(client.ip) || sets.networks.contains(64496)
    - name: partners
      action: allow
      mode: audit
      when: sets.partners.contains(request.host)
`

func TestSets(t *testing.T) {
	c := mustParse(t, setsConfig, nil)
	want := map[string]Set{
		"office":   {Type: "cidr", Values: []SetValue{"192.0.2.0/24", "2001:db8::/32"}, Refresh: 10 * time.Second, OnUnavailable: "fail", MaxEntries: 1_000_000},
		"networks": {Type: "asn", Values: []SetValue{"64496", "AS64497", "64498"}, Refresh: 10 * time.Second, OnUnavailable: "fail", MaxEntries: 3},
		"partners": {Type: "string", File: "partners.txt", Refresh: 30 * time.Second, MaxStaleness: time.Hour, OnUnavailable: "no_match", MaxEntries: 5000},
		"blocked":  {Type: "cidr", File: "/etc/tessera/blocked.txt", Refresh: 10 * time.Second, OnUnavailable: "fail", MaxEntries: 1_000_000},
	}
	if !reflect.DeepEqual(c.Sets, want) {
		t.Fatalf("got  %+v\nwant %+v", c.Sets, want)
	}
	wantDecls := []sets.Declaration{
		{Name: "office", Type: sets.CIDR, Source: sets.SourceValues, OnUnavailable: sets.Fail},
		{Name: "networks", Type: sets.ASN, Source: sets.SourceValues, OnUnavailable: sets.Fail},
		{Name: "partners", Type: sets.String, Source: sets.SourceFile, OnUnavailable: sets.NoMatch},
		{Name: "blocked", Type: sets.CIDR, Source: sets.SourceFile, OnUnavailable: sets.Fail},
	}
	if got := c.SetDeclarations(); !reflect.DeepEqual(got, wantDecls) {
		t.Errorf("declarations %+v", got)
	}
	// blocked is the one set that no rule reads; a rule in audit mode
	// reads partners.
	wantWarnings := []Warning{{Path: "sets.blocked", Line: 19, Column: 3, Problem: "the set is not used by any rule, upstream header or decision field"}}
	if got := c.Warnings(); !reflect.DeepEqual(got, wantWarnings) {
		t.Errorf("warnings %+v", got)
	}
	if got := wantWarnings[0].String(); got != "sets.blocked (line 19, column 3): the set is not used by any rule, upstream header or decision field" {
		t.Errorf("warning %q", got)
	}
	if err := c.ErrorAt("sets.partners.file", "line 7 of the file: the value is empty"); err == nil || err.Error() != "sets.partners.file (line 14, column 11): line 7 of the file: the value is empty" {
		t.Errorf("ErrorAt: %v", err)
	}

	// The warnings follow the order of the file.
	c = mustParse(t, "sets:\n  b: {type: string, values: []}\n  a: {type: asn, values: [1]}\npolicy: {default_action: deny}\n", nil)
	if got := c.Warnings(); len(got) != 2 || got[0].Path != "sets.b" || got[1].Path != "sets.a" || got[1].Line != 3 {
		t.Errorf("warnings %+v", got)
	}
	if got := mustParse(t, "policy: {default_action: deny}\n", nil).Warnings(); len(got) != 0 {
		t.Errorf("warnings without sets: %+v", got)
	}
}

func TestSetNames(t *testing.T) {
	for _, name := range []string{"a", "office", "partner_hosts_2", "x" + strings.Repeat("y", 62)} {
		c := mustParse(t, "sets:\n  "+name+": {type: string, values: [a]}\npolicy: {default_action: deny}\n", nil)
		if _, ok := c.Sets[name]; !ok {
			t.Errorf("%s: not loaded", name)
		}
	}
	for _, name := range []string{"Office", "2nd", "_office", "office-hosts", "x" + strings.Repeat("y", 63), "in", `"true"`, `"null"`, "namespace", "while", `"a b"`, `"s3cr3t value"`} {
		_, err := parse(t, "sets:\n  "+name+": {type: string, values: [a]}\npolicy: {default_action: deny}\n", nil)
		want := "sets (line 2, column 3): expected a name of lowercase letters, digits and underscores that starts with a letter, at most 63 characters and not a reserved word of the expression language"
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInvalidSets(t *testing.T) {
	const tail = "policy: {default_action: deny}\n"
	for _, tc := range []struct{ name, yaml, want string }{
		{"not a mapping", "sets: []\n", "sets (line 1, column 7): expected a mapping"},
		{"set not a mapping", "sets:\n  office: cidr\n", "sets.office (line 2, column 11): expected a mapping"},
		{"duplicate name", "sets:\n  office: {type: cidr, values: []}\n  office: {type: cidr, values: []}\n", "sets.office (line 3, column 3): duplicate key"},
		{"merge key", "sets:\n  <<: {office: {type: cidr}}\n", "sets (line 2, column 3): keys must be plain strings; merge keys are not allowed"},
		{"null set", "sets:\n  office:\n", "sets.office (line 2, column 10): null is not allowed; omit the key to use its default"},
		{"unknown key", "sets:\n  office: {type: cidr, values: [], url: x}\n", "sets.office.url (line 2, column 36): unknown key"},
		{"no type", "sets:\n  office:\n    values: []\n", "sets.office.type (line 2, column 3): required"},
		{"unknown type", "sets:\n  office: {type: polygon, file: a.geojson}\n", "sets.office.type (line 2, column 18): expected cidr, asn, string or area"},
		{"area of values", "sets:\n  stores:\n    type: area\n    values: []\n", "sets.stores.values (line 4, column 13): not allowed with type area: an area set reads a GeoJSON file"},
		{"area of values and a file", "sets:\n  stores: {type: area, file: s.geojson, values: []}\n", "sets.stores.file (line 2, column 30): values and file are both set: a set has one source"},
		{"no source", "sets:\n  office: {type: cidr}\n", "sets.office (line 2, column 3): needs a source: values or file"},
		{"two sources", "sets:\n  office:\n    type: cidr\n    values: []\n    file: office.txt\n", "sets.office.file (line 5, column 11): values and file are both set: a set has one source"},
		{"two sources, file first", "sets:\n  office:\n    type: cidr\n    file: office.txt\n    values: []\n", "sets.office.values (line 5, column 13): values and file are both set: a set has one source"},
		{"values not a list", "sets:\n  office: {type: cidr, values: 192.0.2.0/24}\n", "sets.office.values (line 2, column 32): expected a list"},
		{"refresh with values", "sets:\n  office: {type: cidr, values: [], refresh: 1m}\n", "sets.office.refresh (line 2, column 45): allowed only with file: a set of values is always available"},
		{"staleness with values", "sets:\n  office: {type: cidr, values: [], max_staleness: 1m}\n", "sets.office.max_staleness (line 2, column 51): allowed only with file: a set of values is always available"},
		{"answer with values", "sets:\n  office: {type: cidr, values: [], on_unavailable: match}\n", "sets.office.on_unavailable (line 2, column 52): allowed only with file: a set of values is always available"},
		{"invalid network", "sets:\n  office:\n    type: cidr\n    values:\n      - 192.0.2.0/24\n      - 10.0.0.1/8\n", "sets.office.values[1] (line 6, column 9): the network has bits set after the prefix length: write its first address, such as 192.0.2.0/24"},
		{"value not repeated", "sets:\n  hosts:\n    type: asn\n    values: [" + secret + "]\n", "sets.hosts.values[0] (line 4, column 14): expected an AS number from 1 to 4294967295, such as 64496 or AS64496, without a sign or leading zeros"},
		{"AS0", "sets:\n  asns: {type: asn, values: [0]}\n", "sets.asns.values[0] (line 2, column 30): AS0 is not allowed: write a number from 1 to 4294967295"},
		{"YAML integer of another form", "sets:\n  asns: {type: asn, values: [0x10]}\n", "sets.asns.values[0] (line 2, column 30): expected an AS number from 1 to 4294967295, such as 64496 or AS64496, without a sign or leading zeros"},
		{"integer in a cidr set", "sets:\n  office: {type: cidr, values: [1]}\n", "sets.office.values[0] (line 2, column 33): expected a string"},
		{"integer in a string set", "sets:\n  hosts: {type: string, values: [42]}\n", "sets.hosts.values[0] (line 2, column 34): expected a string"},
		{"boolean value", "sets:\n  hosts: {type: string, values: [true]}\n", "sets.hosts.values[0] (line 2, column 34): expected a string, or, in an asn set, a decimal integer without leading zeros, such as 64496"},
		{"leading zero in an asn set", "sets:\n  asns: {type: asn, values: [064496]}\n", "sets.asns.values[0] (line 2, column 30): expected a string, or, in an asn set, a decimal integer without leading zeros, such as 64496"},
		{"exponent in an asn set", "sets:\n  asns: {type: asn, values: [6.4e4]}\n", "sets.asns.values[0] (line 2, column 30): expected a string, or, in an asn set, a decimal integer without leading zeros, such as 64496"},
		{"null value", "sets:\n  hosts: {type: string, values: [~]}\n", "sets.hosts.values[0] (line 2, column 34): null is not allowed; omit the key to use its default"},
		{"empty string", "sets:\n  hosts: {type: string, values: [\"  \"]}\n", "sets.hosts.values[0] (line 2, column 34): the value is empty"},
		{"value too long", "sets:\n  hosts: {type: string, values: [" + strings.Repeat("x", sets.MaxValueBytes+1) + "]}\n", "sets.hosts.values[0] (line 2, column 34): the value is longer than 4 KiB"},
		{"too many values", "sets:\n  hosts: {type: string, values: [a, b, c], max_entries: 2}\n", "sets.hosts.values (line 2, column 33): more values than max_entries"},
		{"max entries zero", "sets:\n  hosts: {type: string, values: [], max_entries: 0}\n", "sets.hosts.max_entries (line 2, column 50): must be positive"},
		{"empty file", "sets:\n  hosts: {type: string, file: \"\"}\n", "sets.hosts.file (line 2, column 31): must not be empty"},
		{"control character in file", "sets:\n  hosts: {type: string, file: \"a\\nb\"}\n", "sets.hosts.file (line 2, column 31): expected a path"},
		{"refresh too short", "sets:\n  hosts: {type: string, file: h.txt, refresh: 999ms}\n", "sets.hosts.refresh (line 2, column 47): must be at least 1s"},
		{"negative staleness", "sets:\n  hosts: {type: string, file: h.txt, max_staleness: -1s}\n", "sets.hosts.max_staleness (line 2, column 53): must not be negative"},
		{"staleness equal to refresh", "sets:\n  hosts: {type: string, file: h.txt, refresh: 30s, max_staleness: 30s}\n", "sets.hosts.max_staleness (line 2, column 67): must be longer than refresh, or 0s for no limit"},
		{"staleness shorter than the default refresh", "sets:\n  hosts: {type: string, file: h.txt, max_staleness: 5s}\n", "sets.hosts.max_staleness (line 2, column 53): must be longer than refresh, or 0s for no limit"},
		{"unknown answer", "sets:\n  hosts: {type: string, file: h.txt, on_unavailable: allow}\n", "sets.hosts.on_unavailable (line 2, column 54): expected fail, match or no_match"},
		{"unset variable", "sets:\n  hosts: {type: string, file: \"${HOSTS_FILE}\"}\n", "sets.hosts.file (line 2, column 31): environment variable HOSTS_FILE is not set"},
		{"undeclared set in a rule", "sets:\n  hosts: {type: string, values: []}\npolicy:\n  default_action: deny\n  rules:\n    - name: r\n      action: deny\n      when: sets.host.contains(request.host)\n", "policy.rules[0].when (line 8, column 17): undefined field 'host'"},
		{"string to a cidr set", "sets:\n  office: {type: cidr, values: []}\npolicy:\n  default_action: deny\n  rules:\n    - name: r\n      action: allow\n      when: sets.office.contains(\"192.0.2.1\")\n", "policy.rules[0].when (line 8, column 33): found no matching overload for 'contains' applied to 'tessera.CIDRSet.(string)'"},
		{"set as a value", "sets:\n  office: {type: cidr, values: []}\npolicy:\n  default_action: deny\n  rules:\n    - name: r\n      action: allow\n      when: has(sets.office)\n", "policy.rules[0].when (line 8, column 17): a set can be used only as the target of contains, as in sets.<name>.contains(value)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := tc.yaml
			if !strings.Contains(yaml, "policy:") {
				yaml += tail
			}
			_, err := parse(t, yaml, nil)
			if err == nil {
				t.Fatal("Parse succeeded")
			}
			if err.Error() != tc.want {
				t.Fatalf("error %q, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the error repeats a value: %v", err)
			}
		})
	}
}

// max_staleness, when it is not 0s, is longer than refresh, so that the set
// is not unavailable before each check.
func TestSetStaleness(t *testing.T) {
	for _, keys := range []string{"refresh: 1s, max_staleness: 1001ms", "max_staleness: 11s", "refresh: 1h, max_staleness: 0s"} {
		if _, err := parse(t, "sets:\n  hosts: {type: string, file: h.txt, "+keys+"}\npolicy: {default_action: deny}\n", nil); err != nil {
			t.Errorf("%s: %v", keys, err)
		}
	}
}

func TestRedactedSets(t *testing.T) {
	c := mustParse(t, setsConfig+`
  # Environment.
`, nil)
	c.Sets["partners"] = Set{Type: "string", File: "/etc/tessera/partners.txt", Refresh: 30 * time.Second, MaxStaleness: time.Hour, OnUnavailable: "no_match", MaxEntries: 5000}
	got, err := c.Redacted()
	if err != nil {
		t.Fatal(err)
	}
	// The keys of the source only, so that the output reads back; the
	// names in order.
	want := `sets:
  blocked:
    type: cidr
    file: /etc/tessera/blocked.txt
    refresh: 10s
    max_staleness: 0s
    on_unavailable: fail
    max_entries: 1000000
  networks:
    type: asn
    values:
      - "64496"
      - AS64497
      - "64498"
    max_entries: 3
  office:
    type: cidr
    values:
      - 192.0.2.0/24
      - 2001:db8::/32
    max_entries: 1000000
  partners:
    type: string
    file: /etc/tessera/partners.txt
    refresh: 30s
    max_staleness: 1h0m0s
    on_unavailable: no_match
    max_entries: 5000
policy:`
	if !strings.Contains(string(got), want) {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	again := mustParse(t, string(got), nil)
	if !reflect.DeepEqual(again.Sets, c.Sets) {
		t.Fatalf("reads back as %+v\nwant %+v", again.Sets, c.Sets)
	}

	// A value or a path from the environment is redacted.
	c = mustParse(t, `
sets:
  hosts:
    type: string
    values: [api.example.com, "${PARTNER}"]
  blocked:
    type: cidr
    file: ${BLOCKED_FILE}
policy:
  default_action: deny
`, map[string]string{"PARTNER": secret, "BLOCKED_FILE": "/run/" + secret + ".txt"})
	if c.Sets["hosts"].Values[1] != secret || c.Sets["blocked"].File != "/run/"+secret+".txt" {
		t.Fatalf("substitution: %+v", c.Sets)
	}
	got, err = c.Redacted()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), secret) || !strings.Contains(string(got), "      - api.example.com\n      - '[REDACTED]'\n") || !strings.Contains(string(got), "    file: '[REDACTED]'\n") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestLoadSetPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(setsConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A relative path is relative to the configuration file; Load reads
	// no set file.
	if got := c.Sets["partners"].File; got != filepath.Join(dir, "partners.txt") {
		t.Errorf("relative file %q", got)
	}
	if got := c.Sets["blocked"].File; got != "/etc/tessera/blocked.txt" {
		t.Errorf("absolute file %q", got)
	}
	if got := c.Sets["office"].File; got != "" {
		t.Errorf("a set of values has the file %q", got)
	}
}

// An area set reads a file, relative to the configuration; contains takes
// the location of geo.
func TestAreaSetConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "enrichment:\n  geo: {database: geo.mmdb}\nsets:\n  stores:\n    type: area\n    file: stores.geojson\n    max_entries: 50\npolicy:\n  default_action: deny\n  rules:\n    - name: stores\n      action: allow\n      when: sets.stores.contains(geo.location)\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Set{Type: "area", File: filepath.Join(dir, "stores.geojson"), Refresh: 10 * time.Second, OnUnavailable: "fail", MaxEntries: 50}
	if got := c.Sets["stores"]; !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if got := c.SetDeclarations(); len(got) != 1 || got[0] != (sets.Declaration{Name: "stores", Type: sets.Area, Source: sets.SourceFile, OnUnavailable: sets.Fail}) {
		t.Errorf("declarations %+v", got)
	}
	if got := c.Warnings(); len(got) != 0 {
		t.Errorf("warnings %+v", got)
	}
	// A point of another type, or without enrichment.geo, is an error at
	// its position in the condition.
	for when, want := range map[string]string{
		"sets.stores.contains(client.ip)": "policy.rules[0].when (line 13, column 33): found no matching overload for 'contains' applied to 'tessera.AreaSet.(tessera.IP)'",
	} {
		_, err := parse(t, strings.Replace(yaml, "sets.stores.contains(geo.location)", when, 1), nil)
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %s", when, err, want)
		}
	}
	_, err = parse(t, strings.Replace(yaml, "enrichment:\n  geo: {database: geo.mmdb}\n", "", 1), nil)
	if err == nil || err.Error() != "policy.rules[0].when (line 11, column 34): geo is not available: the configuration has no enrichment.geo" {
		t.Errorf("without enrichment.geo: %v", err)
	}
}

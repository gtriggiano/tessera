package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/config"
	"github.com/gtriggiano/tessera/internal/sets"
)

func writeSetFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "set.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadSets(t *testing.T) {
	partners := writeSetFile(t, "# partners\napi.example.com\nwww.example.com\n")
	cfg, err := config.Parse([]byte(`
sets:
  office:
    type: cidr
    values: [192.0.2.0/24]
  partners:
    type: string
    file: `+partners+`
  asns:
    type: asn
    file: /nonexistent/asns.txt
policy:
  default_action: deny
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A replaced set takes its copy, and its file is not read.
	replacement, _ := sets.FromValues(sets.ASN, []string{"64496"}, 10)
	loaded, err := LoadSets(cfg, map[string]*sets.Set{"asns": replacement})
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Store.Declarations(); len(got) != 3 || got[0].Name != "office" || got[1].Name != "partners" || got[2].Name != "asns" {
		t.Errorf("declarations %+v", got)
	}
	for name, check := range map[string]func(*sets.Set) bool{
		"office":   func(s *sets.Set) bool { return s.Entries() == 1 },
		"partners": func(s *sets.Set) bool { return s.Entries() == 2 && s.ContainsString("www.example.com") },
		"asns":     func(s *sets.Set) bool { return s == replacement },
	} {
		if e, ok := loaded.Store.Get(name); !ok || !e.Available || !check(e.Set) {
			t.Errorf("%s: %+v", name, e)
		}
	}
	info, err := os.Stat(partners)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := loaded.Files["partners"]; !ok || id.Size != info.Size() || !id.ModTime.Equal(info.ModTime()) || len(loaded.Files) != 1 {
		t.Errorf("identities %+v", loaded.Files)
	}

	// Without the replacement, the missing file is a configuration error at
	// its key.
	_, err = LoadSets(cfg, nil)
	var invalid *config.Error
	if !errors.As(err, &invalid) || err.Error() != "sets.asns.file (line 11, column 11): the file does not exist" {
		t.Errorf("missing file: %v", err)
	}
	// Invalid content gives the line of the file and the problem, not the
	// content.
	if err := os.WriteFile(partners, []byte("api.example.com\ns3cr3t\x00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadSets(cfg, map[string]*sets.Set{"asns": replacement})
	if err == nil || err.Error() != "sets.partners.file (line 8, column 11): line 2 of the file: the value has a control character" {
		t.Errorf("invalid content: %v", err)
	}
}

// setsServeConfig reads a file of partners and has a set that no rule reads.
func setsServeConfig(partners string) string {
	return `server:
  listen: ` + grpcPlaceholder + `
  drain_delay: 0s
admin:
  listen: ` + adminPlaceholder + `
client_ip:
  from: x_forwarded_for
  trusted_hops: 0
log:
  decisions: all
sets:
  blocked:
    type: cidr
    values: [198.51.100.0/24]
  partners:
    type: string
    file: ` + partners + `
  unused:
    type: asn
    values: [64496]
policy:
  default_action: deny
  rules:
    - name: blocked
      action: deny
      when: sets.blocked.contains(client.ip)
    - name: partners
      action: allow
      when: sets.partners.contains(request.host)
`
}

func TestServeWithSets(t *testing.T) {
	yaml := setsServeConfig(writeSetFile(t, "api.example.com\n"))
	cfg, err := config.Parse([]byte(yaml), nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSets(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := launch(t, yaml, Options{Sets: loaded})
	grpcAddress, adminAddress := h.addresses()
	conn := dialGRPC(t, grpcAddress)

	// Every rule has a series for the cause set_unavailable, from the start.
	series := strings.Join(scrapeSeries(t, adminAddress), "\n")
	for _, s := range []string{`tessera_rule_evaluation_errors_total{cause="set_unavailable",rule="blocked"}`, `tessera_rule_evaluation_errors_total{cause="set_unavailable",rule="partners"}`} {
		if !strings.Contains(series, s) {
			t.Errorf("no series %s", s)
		}
	}
	// Every set has its series of entries and availability, labelled with
	// its source; only the set read from a file has the series of the
	// refreshes, its load at startup counted as one.
	m := scrape(t, adminAddress)
	for _, s := range []struct{ name, typ, source string }{{"blocked", "cidr", "values"}, {"partners", "string", "file"}, {"unused", "asn", "values"}} {
		labels := `set="` + s.name + `",source="` + s.source + `",type="` + s.typ + `"`
		for series, want := range map[string]float64{
			`tessera_set_entries{` + labels + `}`:   1,
			`tessera_set_available{` + labels + `}`: 1,
		} {
			if got, ok := m[series]; !ok || got != want {
				t.Errorf("%s = %v, want %v", series, got, want)
			}
		}
		refreshes := map[string]float64{
			`tessera_set_refreshes_total{result="updated",` + labels + `}`: 1,
			`tessera_set_refreshes_total{result="error",` + labels + `}`:   0,
			`tessera_set_refresh_duration_seconds_count{` + labels + `}`:   1,
		}
		for series, want := range refreshes {
			got, ok := m[series]
			if s.source == "values" && ok {
				t.Errorf("a set of values has the series %s", series)
			}
			if s.source == "file" && (!ok || got != want) {
				t.Errorf("%s = %v, want %v", series, got, want)
			}
		}
		last, ok := m[`tessera_set_last_refresh_success_timestamp_seconds{`+labels+`}`]
		if ok != (s.source == "file") || ok && last < float64(time.Now().Add(-time.Minute).Unix()) {
			t.Errorf("%s: last success %v, %t", s.name, last, ok)
		}
	}

	checkMany(t, conn, 1, func(int) (string, string, string) { return "api.example.com", "/", "192.0.2.10" })
	// blocked becomes unavailable: the deny rule that reads it denies.
	if err := loaded.Store.SetAvailable("blocked", false); err != nil {
		t.Fatal(err)
	}
	checkMany(t, conn, 1, func(int) (string, string, string) { return "api.example.com", "/", "192.0.2.10" })
	h.waitFor("evaluation_error")
	if _, body := get(t, "http://"+adminAddress+"/metrics"); !strings.Contains(body, "\n"+`tessera_rule_evaluation_errors_total{cause="set_unavailable",rule="blocked"} 1`+"\n") {
		t.Error("tessera_rule_evaluation_errors_total does not count the condition")
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	events := h.events()
	// config_warning follows starting, then set_loaded for each set, in the
	// order of the file, before listening.
	for i := range events {
		events[i] = durationMS.ReplaceAllString(events[i], "")
	}
	if len(events) < 6 || !strings.HasPrefix(events[0], "level=info msg=starting ") || !strings.HasPrefix(events[5], "level=info msg=listening ") {
		t.Fatalf("events:\n%s", strings.Join(events, "\n"))
	}
	wantEvents(t, events[1:5], []string{
		`level=warn msg=config_warning key=sets.unused warning="the set is not used by any rule"`,
		`level=info msg=set_loaded set=blocked set_type=cidr set_source=values entries=1`,
		`level=info msg=set_loaded set=partners set_type=string set_source=file entries=1`,
		`level=info msg=set_loaded set=unused set_type=asn set_source=values entries=1`,
	})
	logs := h.logs.String()
	for _, want := range []string{
		` level=info msg=decision request_id="" host=api.example.com method=GET path=/ client_ip=192.0.2.10 action=allow enforced=true rule=partners reason=rule duration_ms=`,
		` level=warn msg=decision request_id="" host=api.example.com method=GET path=/ client_ip=192.0.2.10 action=deny enforced=true rule=blocked reason=error error=set_unavailable duration_ms=`,
		` level=warn msg=evaluation_error rule=blocked cause=set_unavailable count=1` + "\n",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("no event %q in:\n%s", want, logs)
		}
	}
}

// Run loads the sets itself when it is not given them.
func TestRunLoadsTheSets(t *testing.T) {
	h := launch(t, setsServeConfig(writeSetFile(t, "api.example.com\n")), Options{})
	grpcAddress, _ := h.addresses()
	checkMany(t, dialGRPC(t, grpcAddress), 1, func(int) (string, string, string) { return "api.example.com", "/", "192.0.2.10" })
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if logs := h.logs.String(); !strings.Contains(logs, " action=allow enforced=true rule=partners reason=rule ") {
		t.Errorf("no decision by partners in:\n%s", logs)
	}

	missing := filepath.Join(t.TempDir(), "missing.txt")
	cfg, err := config.Parse([]byte(setsServeConfig(missing)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(cfg, Options{}); err == nil || !strings.Contains(err.Error(), "sets.partners.file") {
		t.Errorf("Run with a missing file: %v", err)
	}
}

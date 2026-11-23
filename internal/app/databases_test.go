package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/gtriggiano/tessera/internal/config"
	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/mmdbtest"
)

// writeDatabase writes the database of w to a new file and renames it over
// path, as a safe update does.
func writeDatabase(t *testing.T, path string, w *mmdbtest.Writer) {
	t.Helper()
	data, err := w.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

// usDatabase is a geo database in which 192.0.2.0/24 is in the United
// States, built a day before the example databases.
func usDatabase() *mmdbtest.Writer {
	w := mmdbtest.New("GeoIP2-Country", mmdbtest.BuildEpoch-86400)
	if err := w.Insert(mmdbtest.ExampleCities[0].Network, mmdbtest.SanFrancisco.CountryRecord()); err != nil {
		panic(err)
	}
	return w
}

// databasesConfig reads the databases geo and asn from the files geoPath and
// asnPath: requests from AS 64500 are denied, from the European Union
// allowed, and the others denied.
func databasesConfig(geoPath, asnPath, server string) string {
	return `server:
  listen: ` + grpcPlaceholder + `
` + server + `admin:
  listen: ` + adminPlaceholder + `
client_ip:
  from: x_forwarded_for
  trusted_hops: 0
log:
  decisions: none
enrichment:
  geo:
    database: ` + geoPath + `
  asn:
    database: ` + asnPath + `
policy:
  default_action: deny
  rules:
    - name: hosting
      action: deny
      when: asn.number == 64500
    - name: europe
      action: allow
      when: geo.country.in_eu
`
}

func TestLoadDatabases(t *testing.T) {
	dir := t.TempDir()
	geo, asn := filepath.Join(dir, "geo.mmdb"), filepath.Join(dir, "asn.mmdb")
	writeDatabase(t, geo, mmdbtest.CityDatabase(6))
	writeDatabase(t, asn, mmdbtest.ASNDatabase())
	cfg, err := config.Parse([]byte(databasesConfig(geo, asn, "")), nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadDatabases(cfg)
	if err != nil {
		t.Fatal(err)
	}
	snap := loaded.Store.Snapshot()
	if snap.Database(enrichment.Geo).Type() != "GeoIP2-City" || snap.Database(enrichment.ASN).Type() != "GeoLite2-ASN" {
		t.Errorf("databases %+v", snap)
	}
	info, _ := os.Stat(geo)
	if id := loaded.Files[enrichment.Geo]; id.Size != info.Size() || len(loaded.Files) != 2 || loaded.Loads[enrichment.ASN].At.IsZero() {
		t.Errorf("identities %+v, loads %+v", loaded.Files, loaded.Loads)
	}

	// A database that is missing or invalid is a configuration error at its
	// key, without its path.
	if err := os.Remove(asn); err != nil {
		t.Fatal(err)
	}
	_, err = LoadDatabases(cfg)
	var invalid *config.Error
	if !errors.As(err, &invalid) || err.Error() != "enrichment.asn.database (line 14, column 15): the file does not exist" {
		t.Errorf("missing file: %v", err)
	}
	if err := os.WriteFile(asn, []byte("AS64496\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadDatabases(cfg); err == nil || err.Error() != "enrichment.asn.database (line 14, column 15): the file is not a valid MMDB database" {
		t.Errorf("invalid file: %v", err)
	}
	// No database configured: an empty store.
	none, err := LoadDatabases(mustConfig(t, baseConfig("deny", "")))
	if err != nil || len(none.Store.Kinds()) != 0 {
		t.Errorf("no database: %v", err)
	}
}

func mustConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml), nil)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// checkFrom decides a request from addr, and returns the gRPC status of the
// decision, or an error.
func checkFrom(conn *grpc.ClientConn, addr string) (codes.Code, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := authv3.NewAuthorizationClient(conn).Check(ctx, &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
			Method: "GET", Host: "api.example.com", Path: "/", Headers: map[string]string{"x-forwarded-for": addr},
		}},
	}})
	if err != nil {
		return 0, err
	}
	return codes.Code(resp.GetStatus().GetCode()), nil
}

// The exit criterion of the databases: a database replaced by a new file is
// used by the next decisions, without a restart, while the requests in
// progress see no error; an invalid or missing file changes nothing, and
// shows in the metrics and in one database_load_failed event.
func TestDatabaseRefresh(t *testing.T) {
	dir := t.TempDir()
	geo, asn := filepath.Join(dir, "geo.mmdb"), filepath.Join(dir, "asn.mmdb")
	writeDatabase(t, geo, usDatabase())
	writeDatabase(t, asn, mmdbtest.ASNDatabase())
	started := time.Now()
	yaml := strings.Replace(databasesConfig(geo, asn, "  drain_delay: 0s\n"), "    database: "+geo+"\n", "    database: "+geo+"\n    refresh: 1s\n", 1)
	h := launch(t, yaml, Options{setTime: fastSets})
	grpcAddress, adminAddress := h.addresses()
	conn := dialGRPC(t, grpcAddress)
	const italy, hosting = "192.0.2.7", "198.51.100.7"
	wantDecisions(t, conn, map[string]codes.Code{italy: codes.PermissionDenied, hosting: codes.PermissionDenied})

	// The load at startup is a refresh, and the checks of the unchanged
	// file are observed. A check is counted before it is observed: wait for
	// both.
	geoSeries := func(name string) string { return name + `{database="geo"}` }
	refreshes := func(database, result string) string {
		return `tessera_enrichment_database_refreshes_total{database="` + database + `",result="` + result + `"}`
	}
	waitUntil(t, "an unchanged check, observed", func() bool {
		m := scrape(t, adminAddress)
		return m[refreshes("geo", "unchanged")] > 0 && m[geoSeries("tessera_enrichment_database_refresh_duration_seconds_count")] >= 2
	})
	m := scrape(t, adminAddress)
	for series, want := range map[string]float64{
		geoSeries("tessera_enrichment_database_build_timestamp_seconds"):                         mmdbtest.BuildEpoch - 86400,
		`tessera_enrichment_database_build_timestamp_seconds{database="asn"}`:                    mmdbtest.BuildEpoch,
		refreshes("geo", "updated"):                                                              1,
		refreshes("geo", "rejected"):                                                             0,
		refreshes("geo", "error"):                                                                0,
		refreshes("asn", "updated"):                                                              1,
		`tessera_rule_evaluation_errors_total{cause="database_error",rule="europe"}`:             0,
		`tessera_decisions_total{action="deny",enforced="true",reason="default",rule="default"}`: 1,
	} {
		if got, ok := m[series]; !ok || got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	// The asn database is checked too, as often as the test lasts: its load
	// at startup is observed at least.
	if got := m[`tessera_enrichment_database_refresh_duration_seconds_count{database="asn"}`]; got < 1 {
		t.Errorf("asn refresh duration count %v", got)
	}
	if last := m[geoSeries("tessera_enrichment_database_last_refresh_success_timestamp_seconds")]; last < float64(started.Unix()) || last > float64(time.Now().Unix()+1) {
		t.Errorf("last success at %v, started at %v", last, started.Unix())
	}

	// Requests run while the file is replaced: each is decided with one of
	// the two databases, and none fails.
	var stop atomic.Bool
	var failures atomic.Int64
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for !stop.Load() {
				code, err := checkFrom(conn, italy)
				if err != nil || code != codes.OK && code != codes.PermissionDenied {
					failures.Add(1)
				}
			}
		})
	}
	writeDatabase(t, geo, mmdbtest.CityDatabase(6))
	waitUntil(t, "the new database", func() bool {
		code, err := checkFrom(conn, italy)
		return err == nil && code == codes.OK
	})
	h.waitFor("database_loaded database=geo database_type=GeoIP2-City")
	stop.Store(true)
	workers.Wait()
	if n := failures.Load(); n > 0 {
		t.Fatalf("%d requests failed during the replacement", n)
	}
	wantDecisions(t, conn, map[string]codes.Code{italy: codes.OK, hosting: codes.PermissionDenied})
	m = scrape(t, adminAddress)
	for series, want := range map[string]float64{
		geoSeries("tessera_enrichment_database_build_timestamp_seconds"):                       mmdbtest.BuildEpoch,
		refreshes("geo", "updated"):                                                            2,
		`tessera_rule_evaluation_errors_total{cause="database_error",rule="europe"}`:           0,
		`tessera_rule_evaluation_errors_total{cause="database_error",rule="hosting"}`:          0,
		`tessera_decisions_total{action="deny",enforced="true",reason="error",rule="hosting"}`: 0,
	} {
		if got := m[series]; got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}

	// An invalid file, a database of the other kind and a missing file
	// change nothing: each check fails, and one event reports each
	// failure.
	for _, tc := range []struct {
		change func()
		result string
		event  string
	}{
		{func() { replaceSetFile(t, geo, "not a database\n") }, "rejected", `error="the file is not a valid MMDB database"`},
		{func() { writeDatabase(t, geo, mmdbtest.ASNDatabase()) }, "rejected", `error="a record of the database does not have the schema of a GeoIP2 City or Country database"`},
		{func() {
			if err := os.Remove(geo); err != nil {
				t.Fatal(err)
			}
		}, "error", `error="the file does not exist"`},
	} {
		before := scrape(t, adminAddress)[refreshes("geo", tc.result)]
		tc.change()
		h.waitFor("database_load_failed database=geo " + tc.event)
		waitUntil(t, "two failed checks", func() bool { return scrape(t, adminAddress)[refreshes("geo", tc.result)] >= before+2 })
		wantDecisions(t, conn, map[string]codes.Code{italy: codes.OK, hosting: codes.PermissionDenied})
	}
	if got := scrape(t, adminAddress)[refreshes("geo", "updated")]; got != 2 {
		t.Errorf("updated %v after the failures", got)
	}

	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if n := refresherGoroutines(); n != 0 {
		t.Errorf("%d goroutines of the refreshers left", n)
	}
	events := h.events()
	for i, e := range events {
		events[i] = durationMS.ReplaceAllString(e, "")
	}
	wantEvents(t, events, []string{
		`level=info msg=starting version=1.2.3 revision=abc123 config_path=` + h.path,
		`level=info msg=database_loaded database=geo database_type=GeoIP2-Country build_time=2026-11-16T10:00:00Z`,
		`level=info msg=database_loaded database=asn database_type=GeoLite2-ASN build_time=2026-11-17T10:00:00Z`,
		`level=info msg=listening listener=grpc address=` + grpcAddress,
		`level=info msg=listening listener=admin address=` + adminAddress,
		`level=info msg=ready`,
		`level=info msg=database_loaded database=geo database_type=GeoIP2-City build_time=2026-11-17T10:00:00Z`,
		`level=warn msg=database_load_failed database=geo error="the file is not a valid MMDB database"`,
		`level=warn msg=database_load_failed database=geo error="a record of the database does not have the schema of a GeoIP2 City or Country database"`,
		`level=warn msg=database_load_failed database=geo error="the file does not exist"`,
		`level=info msg=shutdown_started reason=signal`,
		`level=info msg=not_ready`,
		`level=info msg=shutdown_complete outcome=drained`,
	})
}

// A build time beyond the year 9999 is taken as its last second, in
// database_loaded and in the metric, and the database is used.
func TestDatabaseBuildTimeClamp(t *testing.T) {
	dir := t.TempDir()
	geo, asn := filepath.Join(dir, "geo.mmdb"), filepath.Join(dir, "asn.mmdb")
	future := mmdbtest.ASNDatabase()
	future.BuildEpoch = 1 << 63
	writeDatabase(t, geo, mmdbtest.CityDatabase(6))
	writeDatabase(t, asn, future)
	h := launch(t, databasesConfig(geo, asn, "  drain_delay: 0s\n"), Options{})
	grpcAddress, adminAddress := h.addresses()
	wantDecisions(t, dialGRPC(t, grpcAddress), map[string]codes.Code{"198.51.100.7": codes.PermissionDenied, "192.0.2.7": codes.OK})
	if got := scrape(t, adminAddress)[`tessera_enrichment_database_build_timestamp_seconds{database="asn"}`]; got != 253402300799 {
		t.Errorf("build timestamp %v", got)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logs.String(), " msg=database_loaded database=asn database_type=GeoLite2-ASN build_time=9999-12-31T23:59:59Z duration_ms=") {
		t.Errorf("no database_loaded with the last time in:\n%s", h.logs.String())
	}
}

// Every rule has a series for the cause database_error from the start, and a
// failed lookup counts there and in evaluation_error.
func TestServeDatabaseError(t *testing.T) {
	dir := t.TempDir()
	geo, asn := filepath.Join(dir, "geo.mmdb"), filepath.Join(dir, "asn.mmdb")
	writeDatabase(t, geo, mmdbtest.BrokenDatabase("geo"))
	writeDatabase(t, asn, mmdbtest.ASNDatabase())
	h := launch(t, databasesConfig(geo, asn, "  drain_delay: 0s\n"), Options{})
	grpcAddress, adminAddress := h.addresses()
	conn := dialGRPC(t, grpcAddress)
	// 203.0.113.7 has no record in the asn database, and a broken one in the
	// geo database: the allow rule does not match.
	wantDecisions(t, conn, map[string]codes.Code{"203.0.113.7": codes.PermissionDenied, "192.0.2.7": codes.OK})
	m := scrape(t, adminAddress)
	if got := m[`tessera_rule_evaluation_errors_total{cause="database_error",rule="europe"}`]; got != 1 {
		t.Errorf("database_error counted %v times", got)
	}
	if got, ok := m[`tessera_rule_evaluation_errors_total{cause="database_error",rule="hosting"}`]; !ok || got != 0 {
		t.Errorf("hosting: %v, %t", got, ok)
	}
	h.waitFor("evaluation_error")
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logs.String(), " level=warn msg=evaluation_error rule=europe cause=database_error count=1\n") {
		t.Errorf("no evaluation_error in:\n%s", h.logs.String())
	}
}

// The events of the databases name the database first, in JSON too.
func TestDatabaseEventsInJSON(t *testing.T) {
	dir := t.TempDir()
	geo, asn := filepath.Join(dir, "geo.mmdb"), filepath.Join(dir, "asn.mmdb")
	writeDatabase(t, geo, mmdbtest.CityDatabase(6))
	writeDatabase(t, asn, mmdbtest.ASNDatabase())
	yaml := strings.Replace(databasesConfig(geo, asn, "  drain_delay: 0s\n"), "log:\n  decisions: none\n", "log:\n  decisions: none\n  format: json\n", 1)
	h := launch(t, yaml, Options{setTime: fastSets})
	logged := func(msg string) func() bool {
		return func() bool { return strings.Contains(h.logs.String(), `"msg":"`+msg+`"`) }
	}
	waitUntil(t, "ready", logged("ready"))
	if err := os.Remove(asn); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "database_load_failed", logged("database_load_failed"))
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	logs := h.logs.String()
	for _, want := range []string{
		`"level":"info","msg":"database_loaded","database":"geo","database_type":"GeoIP2-City","build_time":"2026-11-17T10:00:00Z","duration_ms":`,
		`"level":"info","msg":"database_loaded","database":"asn","database_type":"GeoLite2-ASN","build_time":"2026-11-17T10:00:00Z","duration_ms":`,
		`"level":"warn","msg":"database_load_failed","database":"asn","error":"the file does not exist"}` + "\n",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("no %s in:\n%s", want, logs)
		}
	}
}

// Run loads the databases itself when it is not given them, and fails on
// one that cannot be read.
func TestRunLoadsTheDatabases(t *testing.T) {
	dir := t.TempDir()
	geo, asn := filepath.Join(dir, "geo.mmdb"), filepath.Join(dir, "asn.mmdb")
	writeDatabase(t, geo, mmdbtest.CityDatabase(6))
	writeDatabase(t, asn, mmdbtest.ASNDatabase())
	h := launch(t, databasesConfig(geo, asn, "  drain_delay: 0s\n"), Options{})
	grpcAddress, _ := h.addresses()
	wantDecisions(t, dialGRPC(t, grpcAddress), map[string]codes.Code{"2001:db8::7": codes.OK, "198.51.100.7": codes.PermissionDenied})
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}

	cfg := mustConfig(t, databasesConfig(geo, filepath.Join(dir, "missing.mmdb"), ""))
	if err := Run(cfg, Options{}); err == nil || !strings.Contains(err.Error(), "enrichment.asn.database") {
		t.Errorf("Run with a missing database: %v", err)
	}
}

//go:build e2e

package e2e

import (
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/mmdbtest"
)

// reflected sends a GET request through Envoy to path, under /reflect/, from
// the client address client, with the values of the client for the
// upstream headers of the policy, and returns the status of the response and
// the values of these headers that reached the upstream: "" for a header
// that did not.
func reflected(t *testing.T, front, path, client string) (status int, asn, country string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+front+"/reflect/"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "www.example.com"
	req.Header.Set("X-Forwarded-For", client)
	// Values that a client would forge: tessera's replace them, or remove
	// them when it has none.
	req.Header.Set("X-Client-Asn", "1")
	req.Header.Set("X-Client-Country", "ZZ")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("X-Upstream-Client-Asn"), resp.Header.Get("X-Upstream-Client-Country")
}

// TestEnvoyEnrichment decides requests through Envoy with rules on the
// autonomous system and the country of the client, read from databases
// generated with example data: the allowed requests reach the upstream with
// tessera's values of the upstream headers, never the client's, and without
// a header whose value is empty. A database renamed over the file in use,
// in a mounted directory, changes the next decisions without a restart.
func TestEnvoyEnrichment(t *testing.T) {
	e := setUp(t)
	config, err := filepath.Abs(filepath.Join("testdata", "enrichment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The directory of the databases, mounted as a volume, as for the sets
	// of TestEnvoySets: readable by the user of the image, 65532.
	dir := filepath.Join(t.TempDir(), "databases")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// replace writes the database of w to a new file and renames it over
	// name.
	replace := func(name string, w *mmdbtest.Writer) {
		t.Helper()
		next := filepath.Join(dir, "."+name+".new")
		if err := w.WriteFile(next); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(next, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	// The example databases: 192.0.2.0/24 in Rome, Italy, AS 64496;
	// 198.51.100.0/24 in San Francisco, United States, AS 64500;
	// 203.0.113.0/26 in Tokyo, Japan, in no autonomous system;
	// 203.0.113.128/25 in neither database; 2001:db8::/48 in Berlin,
	// Germany, AS 64511.
	replace("geo.mmdb", mmdbtest.CityDatabase(6))
	replace("asn.mmdb", mmdbtest.ASNDatabase())
	tessera, front := e.start(t, "enrichment", config, dir+":/etc/tessera/databases:ro")

	for _, database := range []struct{ name, typ string }{{"geo", "GeoIP2-City"}, {"asn", "GeoLite2-ASN"}} {
		waitEvent(t, tessera, "database_loaded", func(event map[string]any) bool {
			return event["database"] == database.name && event["database_type"] == database.typ && event["build_time"] == "2026-11-17T10:00:00Z"
		})
	}

	type want struct {
		status       int
		asn, country string // the values that reached the upstream
		rule         string
		fields       [3]string // the decision fields asn, country and region
	}
	check := func(path, client string, w want) {
		t.Helper()
		status, asn, country := reflected(t, front, path, client)
		if status != w.status || asn != w.asn || country != w.country {
			t.Errorf("from %s: status %d, upstream x-client-asn %q and x-client-country %q; want %d, %q and %q", client, status, asn, country, w.status, w.asn, w.country)
		}
		event := decisionEvents(t, tessera, []string{"/reflect/" + path})["/reflect/"+path]
		if event["rule"] != w.rule || event["client_ip"] != client || event["asn"] != w.fields[0] || event["country"] != w.fields[1] || event["region"] != w.fields[2] {
			t.Errorf("from %s: decision event %v, want rule %s, asn %q, country %q and region %q", client, event, w.rule, w.fields[0], w.fields[1], w.fields[2])
		}
	}
	// Rules on the country and the autonomous system, for clients of
	// different networks. The upstream gets tessera's values: the client's
	// never reach it, and a header whose value is empty is removed.
	check("1", "192.0.2.10", want{http.StatusOK, "64496", "IT", "allowed-countries", [3]string{"64496", "IT", "Lazio"}})
	check("2", "198.51.100.7", want{http.StatusUnavailableForLegalReasons, "", "", "hosting-networks", [3]string{"64500", "US", "California"}})
	check("3", "203.0.113.10", want{http.StatusOK, "", "JP", "allowed-countries", [3]string{"0", "JP", ""}})
	check("4", "203.0.113.200", want{http.StatusForbidden, "", "", "default", [3]string{"0", "", ""}})
	check("5", "2001:db8::10", want{http.StatusForbidden, "", "", "default", [3]string{"64511", "DE", "State of Berlin"}})

	// A new geo database, renamed over the file in use, a day younger: it
	// moves 192.0.2.0/24 to San Francisco, and adds 203.0.113.128/25 in
	// Rome. The next decisions read it, without a restart.
	next := mmdbtest.New("GeoIP2-City", mmdbtest.BuildEpoch+86400)
	for _, c := range []struct {
		network string
		city    mmdbtest.City
	}{
		{"192.0.2.0/24", mmdbtest.SanFrancisco},
		{"198.51.100.0/24", mmdbtest.SanFrancisco},
		{"203.0.113.0/26", mmdbtest.Tokyo},
		{"203.0.113.128/25", mmdbtest.Rome},
		{"2001:db8::/48", mmdbtest.Berlin},
	} {
		if err := next.Insert(netip.MustParsePrefix(c.network), c.city.Record()); err != nil {
			t.Fatal(err)
		}
	}
	replace("geo.mmdb", next)
	waitStatus(t, front, "192.0.2.10", http.StatusForbidden)
	waitEvent(t, tessera, "database_loaded", func(event map[string]any) bool {
		return event["database"] == "geo" && event["build_time"] == "2026-11-18T10:00:00Z"
	})
	check("6", "192.0.2.10", want{http.StatusForbidden, "", "", "default", [3]string{"64496", "US", "California"}})
	check("7", "203.0.113.200", want{http.StatusOK, "", "IT", "allowed-countries", [3]string{"0", "IT", "Lazio"}})
	check("8", "203.0.113.10", want{http.StatusOK, "", "JP", "allowed-countries", [3]string{"0", "JP", ""}})

	// All of it without a restart, without a failed load, and without a
	// failed lookup.
	if state := docker(t, "inspect", "--format", "{{.State.Running}} {{.RestartCount}}", tessera); state != "true 0" {
		t.Errorf("tessera: running and restarts %q, want \"true 0\"", state)
	}
	if n := len(events(t, tessera, "starting")); n != 1 {
		t.Errorf("%d starting events in the logs of tessera, want 1", n)
	}
	if failed := events(t, tessera, "database_load_failed"); len(failed) != 0 {
		t.Errorf("database_load_failed events: %v", failed)
	}
	// The requests without X-Forwarded-For with which start waits for
	// Envoy have no client address; no lookup failed.
	for _, event := range events(t, tessera, "evaluation_error") {
		if event["cause"] != "client_ip_missing" {
			t.Errorf("evaluation_error event %v", event)
		}
	}
}

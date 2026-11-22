//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// within is how long the test waits for tessera to see a new file: with
// refresh: 1s, a check every second, and the time for Docker to show the
// file of the host in the container.
const within = 30 * time.Second

// statusFrom sends a GET request through Envoy, from the client address
// client, and returns the status of the response.
func statusFrom(t *testing.T, front, client string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+front+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "www.example.com"
	req.Header.Set("X-Forwarded-For", client)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// waitStatus sends requests from client until one gets the status want.
func waitStatus(t *testing.T, front, client string, want int) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := statusFrom(t, front, client)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("from %s: status %d after %v, want %d", client, got, within, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// refreshes returns the value of tessera_set_refreshes_total for the set
// blocked and the result result, on the admin endpoint admin.
func refreshes(t *testing.T, admin, result string) float64 {
	t.Helper()
	resp, err := http.Get("http://" + admin + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		series, value, ok := strings.Cut(sc.Text(), " ")
		if ok && strings.HasPrefix(series, "tessera_set_refreshes_total{") &&
			strings.Contains(series, `result="`+result+`"`) && strings.Contains(series, `set="blocked"`) && strings.Contains(series, `type="cidr"`) {
			n, err := strconv.ParseFloat(value, 64)
			if err != nil {
				t.Fatalf("%s: %v", sc.Text(), err)
			}
			return n
		}
	}
	t.Fatalf("no series of tessera_set_refreshes_total for result=%s in /metrics", result)
	return 0
}

// events returns the events of the JSON logs of container named msg.
func events(t *testing.T, container, msg string) []map[string]any {
	t.Helper()
	logs, err := exec.Command("docker", "logs", container).Output()
	if err != nil {
		t.Fatal(err)
	}
	var found []map[string]any
	for line := range strings.Lines(string(logs)) {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil && event["msg"] == msg {
			found = append(found, event)
		}
	}
	return found
}

// waitEvent reads the logs of container until it has an event named msg for
// which match returns true, and returns it.
func waitEvent(t *testing.T, container, msg string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		for _, event := range events(t, container, msg) {
			if match(event) {
				return event
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching %s event in the logs of tessera after %v: %v", msg, within, events(t, container, msg))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestEnvoySets changes the file of a set while tessera runs, through Envoy:
// a file renamed over it changes the decisions within a few seconds, without
// a restart; an invalid file changes nothing, and shows in the metrics and in
// the logs.
func TestEnvoySets(t *testing.T) {
	e := setUp(t)
	config, err := filepath.Abs(filepath.Join("testdata", "sets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The directory of the file, mounted as a volume: a file renamed over
	// blocked.txt on the host is seen in the container, unlike a file mounted
	// alone. The user of the image, 65532, reads them: the directory and its
	// files are readable by all, whatever the umask.
	dir := filepath.Join(t.TempDir(), "sets")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// replace writes content to a new file and renames it over blocked.txt.
	replace := func(content string) {
		t.Helper()
		next := filepath.Join(dir, ".blocked.txt.new")
		if err := os.WriteFile(next, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(next, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, filepath.Join(dir, "blocked.txt")); err != nil {
			t.Fatal(err)
		}
	}

	replace("# Blocked networks\n198.51.100.0/24\n")
	tessera, front := e.start(t, "sets", config, dir+":/etc/tessera/sets:ro")
	admin := hostAddress(t, tessera, "9090/tcp")
	const client = "192.0.2.10"
	if got := statusFrom(t, front, client); got != http.StatusOK {
		t.Fatalf("from %s, not in the set: status %d, want 200", client, got)
	}
	if got := statusFrom(t, front, "198.51.100.7"); got != http.StatusForbidden {
		t.Fatalf("from 198.51.100.7, in the set: status %d, want 403", got)
	}

	// A new file, renamed over the old one: the same request is denied.
	replace("# Blocked networks\n198.51.100.0/24\n192.0.2.0/24\n")
	waitStatus(t, front, client, http.StatusForbidden)
	waitEvent(t, tessera, "set_loaded", func(event map[string]any) bool {
		return event["set"] == "blocked" && event["set_type"] == "cidr" && event["set_source"] == "file" && event["entries"] == float64(2)
	})

	// An invalid file, which would allow the request again if it were
	// applied in part: it is rejected, and the copy in use stays.
	rejected := refreshes(t, admin, "rejected")
	replace("# Blocked networks\n198.51.100.0/24\nnot-a-network\n")
	deadline := time.Now().Add(within)
	for refreshes(t, admin, "rejected") <= rejected {
		if time.Now().After(deadline) {
			t.Fatalf("tessera_set_refreshes_total{result=\"rejected\"} still %v after %v", rejected, within)
		}
		time.Sleep(200 * time.Millisecond)
	}
	failed := waitEvent(t, tessera, "set_load_failed", func(event map[string]any) bool {
		return event["set"] == "blocked" && event["set_type"] == "cidr" && event["set_source"] == "file"
	})
	if problem, _ := failed["error"].(string); !strings.HasPrefix(problem, "line 3 of the file: ") || strings.Contains(problem, "not-a-network") {
		t.Errorf("set_load_failed error %q, want the line number and the problem only", problem)
	}
	for range 3 {
		if got := statusFrom(t, front, client); got != http.StatusForbidden {
			t.Fatalf("from %s after the invalid file: status %d, want 403", client, got)
		}
	}

	// All of it without a restart.
	if state := docker(t, "inspect", "--format", "{{.State.Running}} {{.RestartCount}}", tessera); state != "true 0" {
		t.Errorf("tessera: running and restarts %q, want \"true 0\"", state)
	}
	if n := len(events(t, tessera, "starting")); n != 1 {
		t.Errorf("%d starting events in the logs of tessera, want 1", n)
	}
}

//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var repoRoot = filepath.Join("..", "..")

// docker runs the docker command and returns its standard output, trimmed.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := dockerOutput(args...)
	if err != nil {
		t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func dockerOutput(args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// environment is what every case shares: the images, the network and the
// test upstream.
type environment struct {
	suffix  string
	network string
	tessera string // image
	envoy   string // image
}

// setUp builds the images and starts the network and the upstream, all
// removed when the test ends.
func setUp(t *testing.T) *environment {
	t.Helper()
	arch := docker(t, "version", "--format", "{{.Server.Arch}}")
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()%1e6)
	e := &environment{
		suffix:  suffix,
		network: "tessera-e2e-" + suffix,
		tessera: "tessera-e2e:" + suffix,
		envoy:   "tessera-e2e-envoy:" + suffix,
	}

	// The image of a release, from the Dockerfile of the repository, with a
	// binary of the working tree for the platform of the Docker server and
	// the files that GoReleaser adds to the build context.
	context := t.TempDir()
	for _, name := range []string{"LICENSE", "NOTICE"} {
		data, err := os.ReadFile(filepath.Join(repoRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(context, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "build", "-trimpath", "-o", filepath.Join(context, "linux", arch, "tessera"), "./cmd/tessera")
	build.Dir = repoRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	docker(t, "build", "--quiet", "--platform", "linux/"+arch, "--file", filepath.Join(repoRoot, "Dockerfile"), "--tag", e.tessera, context)
	t.Cleanup(func() { _, _ = dockerOutput("image", "rm", "--force", e.tessera) })
	checkImageFiles(t, e.tessera, suffix)
	docker(t, "build", "--quiet", "--tag", e.envoy, "testdata")
	t.Cleanup(func() { _, _ = dockerOutput("image", "rm", "--force", e.envoy) })

	docker(t, "network", "create", e.network)
	t.Cleanup(func() { _, _ = dockerOutput("network", "rm", e.network) })
	e.run(t, "upstream", "--network-alias", "upstream", e.envoy, "-c", "/etc/envoy/upstream.yaml")
	return e
}

// checkImageFiles checks that image carries the LICENSE and NOTICE of the
// repository in /usr/share/doc/tessera/.
func checkImageFiles(t *testing.T, image, suffix string) {
	t.Helper()
	container := "tessera-files-" + suffix
	docker(t, "create", "--name", container, image)
	defer func() { _, _ = dockerOutput("rm", "--force", container) }()
	dir := t.TempDir()
	for _, name := range []string{"LICENSE", "NOTICE"} {
		copied := filepath.Join(dir, name)
		docker(t, "cp", container+":/usr/share/doc/tessera/"+name, copied)
		got, err := os.ReadFile(copied)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(repoRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("/usr/share/doc/tessera/%s in the image differs from the repository", name)
		}
	}
}

// run starts a detached container, removed when the test ends, and returns
// its name.
func (e *environment) run(t *testing.T, name string, args ...string) string {
	t.Helper()
	container := name + "-" + e.suffix
	docker(t, append([]string{"run", "--detach", "--name", container, "--network", e.network}, args...)...)
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
			t.Logf("logs of %s:\n%s", name, logs)
		}
		_, _ = dockerOutput("rm", "--force", container)
	})
	return container
}

// hostAddress returns the loopback address where the port of container is
// published.
func hostAddress(t *testing.T, container, port string) string {
	t.Helper()
	for line := range strings.Lines(docker(t, "port", container, port)) {
		if address := strings.TrimSpace(line); strings.HasPrefix(address, "127.0.0.1:") {
			return address
		}
	}
	t.Fatalf("%s: port %s is not published on 127.0.0.1", container, port)
	return ""
}

// poll sends GET requests to url until accept returns true for a response,
// and returns its status and body.
func poll(t *testing.T, url string, accept func(status int) bool) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(60 * time.Second)
	last := "no response"
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "www.example.com"
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr == nil && accept(resp.StatusCode) {
				return resp.StatusCode, string(body)
			}
			last = fmt.Sprintf("status %d", resp.StatusCode)
		} else {
			last = err.Error()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("GET %s: %s", url, last)
	return 0, ""
}

// TestEnvoy sends a request through Envoy for each policy.default_action, and
// stops tessera with a signal.
func TestEnvoy(t *testing.T) {
	e := setUp(t)
	for _, tc := range []struct {
		defaultAction string
		status        int
		body          string
	}{
		// tessera's denial: 403 with an empty body.
		{"deny", http.StatusForbidden, ""},
		// The request reaches the upstream, which answers "upstream".
		{"allow", http.StatusOK, "upstream\n"},
	} {
		t.Run(tc.defaultAction, func(t *testing.T) {
			config, err := filepath.Abs(filepath.Join("testdata", tc.defaultAction+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			tessera := e.run(t, "tessera-"+tc.defaultAction, "--network-alias", "tessera", "--read-only",
				"--volume", config+":/etc/tessera/config.yaml:ro", "--publish", "127.0.0.1::9090", e.tessera)
			admin := hostAddress(t, tessera, "9090/tcp")
			poll(t, "http://"+admin+"/readyz", func(status int) bool { return status == http.StatusOK })

			envoy := e.run(t, "envoy-"+tc.defaultAction, "--publish", "127.0.0.1::8080", e.envoy)
			front := hostAddress(t, envoy, "8080/tcp")
			// Until Envoy reaches tessera and the upstream, it answers 503.
			status, body := poll(t, "http://"+front+"/", func(status int) bool { return status != http.StatusServiceUnavailable })
			if status != tc.status || body != tc.body {
				t.Fatalf("through Envoy: %d %q, want %d %q", status, body, tc.status, tc.body)
			}

			docker(t, "stop", "--timeout", "30", tessera)
			if code := docker(t, "inspect", "--format", "{{.State.ExitCode}}", tessera); code != "0" {
				t.Fatalf("tessera exited with status %s", code)
			}
			logs, err := exec.Command("docker", "logs", tessera).CombinedOutput()
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range []string{" msg=ready\n", " msg=shutdown_started reason=signal\n", " msg=shutdown_complete outcome=drained\n"} {
				if !bytes.Contains(logs, []byte(event)) {
					t.Fatalf("no%s event in the logs of tessera:\n%s", strings.TrimSuffix(event, "\n"), logs)
				}
			}
		})
	}
}

// start runs tessera with the configuration file config, then Envoy, and
// waits until a request through Envoy gets an answer other than 503. It
// returns the tessera container and the address of Envoy.
func (e *environment) start(t *testing.T, name, config string) (tessera, front string) {
	t.Helper()
	tessera = e.run(t, "tessera-"+name, "--network-alias", "tessera", "--read-only",
		"--volume", config+":/etc/tessera/config.yaml:ro", "--publish", "127.0.0.1::9090", e.tessera)
	admin := hostAddress(t, tessera, "9090/tcp")
	poll(t, "http://"+admin+"/readyz", func(status int) bool { return status == http.StatusOK })
	envoy := e.run(t, "envoy-"+name, "--publish", "127.0.0.1::8080", e.envoy)
	front = hostAddress(t, envoy, "8080/tcp")
	poll(t, "http://"+front+"/", func(status int) bool { return status != http.StatusServiceUnavailable })
	return tessera, front
}

// decisionEvents reads the decision events of tessera, in JSON, by path, until
// it has one for each path of paths.
func decisionEvents(t *testing.T, container string, paths []string) map[string]map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		logs, err := exec.Command("docker", "logs", container).Output()
		if err != nil {
			t.Fatal(err)
		}
		events := map[string]map[string]any{}
		for line := range strings.Lines(string(logs)) {
			var event map[string]any
			if json.Unmarshal([]byte(line), &event) == nil && event["msg"] == "decision" {
				events[event["path"].(string)] = event
			}
		}
		missing := slices.ContainsFunc(paths, func(p string) bool { _, ok := events[p]; return !ok })
		if !missing {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("no decision event for one of %v in:\n%s", paths, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// envoyCase is a request through Envoy, what the client gets, and the
// decision event that tessera writes for it.
type envoyCase struct {
	name, host, path, xff, userAgent string
	// The response: status line, body, and headers that must have a value
	// (the empty value: no header).
	status  string
	body    string
	headers map[string]string
	// The decision event, by its path.
	eventPath string
	event     map[string]any
}

// send sends the request, checks the response, and returns the x-request-id
// that Envoy sets in it, which is the one it used for the request.
func (c envoyCase) send(t *testing.T, front string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+front+c.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = c.host
	if c.xff != "" {
		req.Header.Set("X-Forwarded-For", c.xff)
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", c.name, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != c.status || string(body) != c.body {
		t.Errorf("%s: %q %q, want %q %q", c.name, resp.Status, body, c.status, c.body)
	}
	for name, want := range c.headers {
		if got := resp.Header.Get(name); got != want && !(want == "*" && got != "") {
			t.Errorf("%s: header %s %q, want %q", c.name, name, got, want)
		}
	}
	if c.body != "" && resp.ContentLength != int64(len(c.body)) {
		t.Errorf("%s: content-length %d, want %d", c.name, resp.ContentLength, len(c.body))
	}
	return resp.Header.Get("X-Request-Id")
}

// TestEnvoyRules sends requests through Envoy to a policy with rules on
// hosts, paths and networks: the allowed ones reach the upstream, the denied
// ones get the response of their rule, a rule in audit mode lets its requests
// through, and every decision event names its rule. Then the same policy in
// audit mode lets every request through.
func TestEnvoyRules(t *testing.T) {
	e := setUp(t)
	source, err := os.ReadFile(filepath.Join("testdata", "rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const upstream = "upstream\n"
	envoyHeaders := map[string]string{"Date": "*", "Server": "envoy"}
	with := func(h map[string]string) map[string]string {
		out := maps.Clone(envoyHeaders)
		maps.Copy(out, h)
		return out
	}
	event := func(action string, enforced bool, rule, reason, clientIP string, extra ...string) map[string]any {
		m := map[string]any{"action": action, "enforced": enforced, "rule": rule, "reason": reason, "client_ip": clientIP, "host": "www.example.com", "method": "GET"}
		for i := 0; i < len(extra); i += 2 {
			m[extra[i]] = extra[i+1]
		}
		return m
	}
	enforce := []envoyCase{
		{name: "public site", host: "www.example.com", path: "/index/1", xff: "192.0.2.10",
			status: "200 OK", body: upstream, eventPath: "/index/1", event: event("allow", true, "public-site", "rule", "192.0.2.10")},
		// A status that Envoy's list does not name, with the body and the
		// content-type of the rule.
		{name: "blocked network", host: "www.example.com", path: "/index/2", xff: "198.51.100.7",
			status: "451 Unknown", body: `{"error": "unavailable"}`, headers: with(map[string]string{"Content-Type": "application/json", "Cache-Control": ""}),
			eventPath: "/index/2", event: event("deny", true, "blocked-networks", "rule", "198.51.100.7")},
		// policy.deny_response: Envoy adds content-type: text/plain to its body.
		{name: "default action", host: "api.example.com", path: "/v2/3", xff: "192.0.2.10",
			status: "403 Forbidden", body: "Forbidden.\n", headers: with(map[string]string{"Content-Type": "text/plain", "Cache-Control": "no-store"}),
			eventPath: "/v2/3", event: map[string]any{"action": "deny", "enforced": true, "rule": "default", "reason": "default", "client_ip": "192.0.2.10", "host": "api.example.com"}},
		{name: "admin from the office", host: "www.example.com", path: "/admin/4", xff: "203.0.113.7",
			status: "200 OK", body: upstream, eventPath: "/admin/4", event: event("allow", true, "admin-from-office", "rule", "203.0.113.7")},
		// The client writes an office address on the left of the header: the
		// address read from the right stays its own. The rule's response has
		// an empty body and inherits the headers.
		{name: "spoofed X-Forwarded-For", host: "www.example.com", path: "/admin/5", xff: "203.0.113.7, 192.0.2.10",
			status: "404 Not Found", body: "", headers: with(map[string]string{"Cache-Control": "no-store", "Content-Type": ""}),
			eventPath: "/admin/5", event: event("deny", true, "admin", "rule", "192.0.2.10")},
		{name: "rule in audit mode", host: "www.example.com", path: "/index/6", xff: "192.0.2.10", userAgent: "scanner/1.0",
			status: "200 OK", body: upstream, eventPath: "/index/6", event: event("allow", true, "public-site", "rule", "192.0.2.10", "audit_rules", "watch-scanners")},
		// Without X-Forwarded-For, the only entry is Envoy's: there is no
		// client address, and the deny rule that reads it denies.
		{name: "no client address", host: "www.example.com", path: "/index/7",
			status: "451 Unknown", body: `{"error": "unavailable"}`, eventPath: "/index/7", event: event("deny", true, "blocked-networks", "error", "", "error", "client_ip_missing")},
		// Envoy normalizes the path before tessera sees it.
		{name: "normalized path", host: "www.example.com", path: "/index/../admin//8", xff: "192.0.2.10",
			status: "404 Not Found", body: "", eventPath: "/admin/8", event: event("deny", true, "admin", "rule", "192.0.2.10")},
	}
	audit := []envoyCase{
		{name: "blocked network, audited", host: "www.example.com", path: "/index/9", xff: "198.51.100.7",
			status: "200 OK", body: upstream, eventPath: "/index/9", event: event("deny", false, "blocked-networks", "rule", "198.51.100.7")},
		{name: "default action, audited", host: "api.example.com", path: "/v2/10", xff: "192.0.2.10",
			status: "200 OK", body: upstream, eventPath: "/v2/10", event: map[string]any{"action": "deny", "enforced": false, "rule": "default", "reason": "default", "host": "api.example.com"}},
	}
	for _, run := range []struct {
		mode  string
		cases []envoyCase
	}{{"enforce", enforce}, {"audit", audit}} {
		t.Run(run.mode, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(config, bytes.Replace(source, []byte("mode: enforce"), []byte("mode: "+run.mode), 1), 0o644); err != nil {
				t.Fatal(err)
			}
			tessera, front := e.start(t, "rules-"+run.mode, config)
			var paths []string
			requestIDs := map[string]string{}
			for _, c := range run.cases {
				requestIDs[c.eventPath] = c.send(t, front)
				paths = append(paths, c.eventPath)
			}
			events := decisionEvents(t, tessera, paths)
			for _, c := range run.cases {
				got := events[c.eventPath]
				for key, want := range c.event {
					if got[key] != want {
						t.Errorf("%s: decision event %s=%v, want %v: %v", c.name, key, got[key], want, got)
					}
				}
				if _, ok := c.event["audit_rules"]; !ok && got["audit_rules"] != nil {
					t.Errorf("%s: decision event with audit_rules: %v", c.name, got)
				}
				level := map[string]string{"allow": "info", "deny": "warn"}[got["action"].(string)]
				if got["level"] != level || got["duration_ms"] == nil {
					t.Errorf("%s: decision event %v", c.name, got)
				}
				// request_id is the x-request-id of the request, which Envoy
				// generated and returns in the response.
				if id := requestIDs[c.eventPath]; id == "" || got["request_id"] != id {
					t.Errorf("%s: decision event request_id %v, x-request-id of the response %q", c.name, got["request_id"], id)
				}
			}
		})
	}
}

//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

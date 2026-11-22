package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	cliReference = filepath.Join("..", "..", "docs", "reference", "cli.md")
	examplesDir  = filepath.Join("..", "..", "docs", "examples")
)

// documentedRuns returns each command of the CLI reference in an sh block
// that starts with "tessera eval" and is followed by a text or json block,
// with that block.
func documentedRuns(t *testing.T) (runs []struct{ command, output string }) {
	t.Helper()
	data, err := os.ReadFile(cliReference)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []struct{ info, body string }
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		info, ok := strings.CutPrefix(lines[i], "```")
		if !ok || info == "" {
			continue
		}
		var body []string
		for i++; i < len(lines) && lines[i] != "```"; i++ {
			body = append(body, lines[i])
		}
		blocks = append(blocks, struct{ info, body string }{info, strings.Join(body, "\n") + "\n"})
	}
	for i := 0; i+1 < len(blocks); i++ {
		if blocks[i].info == "sh" && strings.HasPrefix(blocks[i].body, "tessera eval ") && (blocks[i+1].info == "text" || blocks[i+1].info == "json") {
			runs = append(runs, struct{ command, output string }{blocks[i].body, blocks[i+1].body})
		}
	}
	return runs
}

// shellWords splits a command as a POSIX shell would: lines joined at a
// trailing backslash, words split at blanks, single and double quotes
// removed.
func shellWords(t *testing.T, command string) []string {
	t.Helper()
	command = strings.ReplaceAll(command, "\\\n", " ")
	var words []string
	var word strings.Builder
	inWord := false
	var quote rune
	for _, r := range strings.TrimSpace(command) {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			word.WriteRune(r)
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		t.Fatalf("unclosed quote in %q", command)
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}

// The examples of tessera eval in the CLI reference print what the page
// shows, with the configuration file of docs/examples that they name.
func TestDocumentedEval(t *testing.T) {
	runs := documentedRuns(t)
	if len(runs) == 0 {
		t.Fatalf("%s: no tessera eval example", cliReference)
	}
	for _, run := range runs {
		args := shellWords(t, run.command)[1:]
		for i := range args {
			if i > 0 && args[i-1] == "--config" {
				args[i] = filepath.Join(examplesDir, args[i])
			}
		}
		code, out, errOut := executeWithEnv(args, nil)
		if code != 0 || errOut != "" {
			t.Fatalf("%s: code %d, stderr %q", run.command, code, errOut)
		}
		if out != run.output {
			t.Errorf("%s\nprints:\n%s\nthe page shows:\n%s", run.command, out, run.output)
		}
	}
}

const evalConfig = `
policy:
  default_action: deny
  deny_response:
    status: 403
    body: Forbidden.
  rules:
    - name: blocked
      description: |
        Networks blocked
        during an incident.
      action: deny
      when: client.ip.inCidr("198.51.100.0/24")
      response:
        status: 451
        headers:
          content-type: text/plain
    - name: watch
      action: deny
      mode: audit
      when: request.headers.exists(h, h.startsWith("x-debug"))
    - name: count
      action: allow
      when: int(request.headers["x-count"]) > 5
    - name: api
      action: allow
      when: request.host == "api.example.com" && request.method in ["GET", "POST"] && request.query == ""
`

func TestEval(t *testing.T) {
	path := writeConfig(t, evalConfig)
	code, out, errOut := executeWithEnv([]string{"eval", "--config", path, "--ip", "::ffff:198.51.100.7"}, nil)
	want := `client.ip  198.51.100.7

blocked  true -> deny  Networks blocked during an incident.

decision: deny (rule blocked, reason rule)
response: status 451
`
	if code != 0 || errOut != "" || out != want {
		t.Fatalf("code=%d stderr=%q stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}

	// Every attribute the evaluated rules read; the header joined as Envoy
	// joins it; the cookie never printed; the host and method normalized.
	code, out, errOut = executeWithEnv([]string{"eval", "--config", path, "--ip", "192.0.2.10", "--host", "API.example.com:443", "--method", "get",
		"--header", "X-Debug-Trace: 1", "--header", "Cookie: s=s3cr3t", "--header", "x-count: one", "--header", "x-count: two"}, nil)
	want = `client.ip                         192.0.2.10
request.headers["cookie"]         [REDACTED]
request.headers["x-count"]        "one,two"
request.headers["x-debug-trace"]  "1"
request.host                      "api.example.com"
request.method                    "GET"
request.query                     ""

blocked  false                      Networks blocked during an incident.
watch    true (audit)
count    indeterminate: expression
api      true -> allow

decision: allow (rule api, reason rule)
`
	if code != 0 || errOut != "" || out != want {
		t.Fatalf("code=%d stderr=%q stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
	if strings.Contains(out, "s3cr3t") {
		t.Fatal("eval prints a cookie")
	}

	// The same in JSON.
	code, out, errOut = executeWithEnv([]string{"eval", "--config", path, "--ip", "192.0.2.10", "--host", "API.example.com:443", "--method", "get",
		"--header", "X-Debug-Trace: 1", "--header", "Cookie: s=s3cr3t", "--header", "x-count: one", "--header", "x-count: two", "--output", "json"}, nil)
	wantJSON := `{"attributes":[{"name":"client.ip","value":"192.0.2.10"},{"name":"request.headers[\"cookie\"]","value":"[REDACTED]"},{"name":"request.headers[\"x-count\"]","value":"one,two"},{"name":"request.headers[\"x-debug-trace\"]","value":"1"},{"name":"request.host","value":"api.example.com"},{"name":"request.method","value":"GET"},{"name":"request.query","value":""}],` +
		`"rules":[{"name":"blocked","description":"Networks blocked during an incident.","action":"deny","mode":"enforce","result":"false"},{"name":"watch","action":"deny","mode":"audit","result":"true"},{"name":"count","action":"allow","mode":"enforce","result":"indeterminate","cause":"expression"},{"name":"api","action":"allow","mode":"enforce","result":"true"}],` +
		`"decision":{"action":"allow","enforced":true,"rule":"api","reason":"rule","audit_rules":["watch"],"indeterminate_rules":["count"]}}` + "\n"
	if code != 0 || errOut != "" || out != wantJSON {
		t.Fatalf("code=%d stderr=%q stdout:\n%s\nwant:\n%s", code, errOut, out, wantJSON)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}

	// Without --ip, client.ip is missing; the deny rule that reads it denies.
	code, out, _ = executeWithEnv([]string{"eval", "--config", path, "--output", "json"}, nil)
	if code != 0 || !strings.Contains(out, `{"name":"client.ip","missing":true}`) || !strings.Contains(out, `"decision":{"action":"deny","enforced":true,"rule":"blocked","reason":"error","error":"client_ip_missing","response":{"status":451,"body":"Forbidden.","headers":{"content-type":"text/plain"}}}`) {
		t.Fatalf("code=%d stdout=%s", code, out)
	}

	// --ip takes a port, and an IPv6 address in brackets; --query the query;
	// an x-request-id header the request ID.
	cfg := writeConfig(t, "policy:\n  default_action: deny\n  rules:\n    - name: r\n      action: allow\n      when: request.query == \"page=2\" && request.id == \"d2c1e2a0\" && client.ip.inCidr(\"2001:db8::/32\")\n")
	code, out, _ = executeWithEnv([]string{"eval", "--config", cfg, "--ip", "[2001:db8::7]:443", "--query", "page=2", "--header", "X-Request-Id: d2c1e2a0"}, nil)
	if code != 0 || !strings.Contains(out, "decision: allow (rule r, reason rule)") || !strings.Contains(out, "client.ip      2001:db8::7\nrequest.id     \"d2c1e2a0\"\n") {
		t.Fatalf("code=%d stdout:\n%s", code, out)
	}

	// Under policy.mode: audit, a denial is not enforced.
	audit := writeConfig(t, strings.Replace(evalConfig, "  default_action: deny\n", "  mode: audit\n  default_action: deny\n", 1))
	code, out, _ = executeWithEnv([]string{"eval", "--config", audit, "--ip", "198.51.100.7"}, nil)
	if code != 0 || !strings.Contains(out, "\ndecision: deny (rule blocked, reason rule), not enforced: policy.mode is audit\nresponse: status 451\n") {
		t.Fatalf("code=%d stdout:\n%s", code, out)
	}
}

func TestEvalErrors(t *testing.T) {
	path := writeConfig(t, evalConfig)
	for _, tc := range []struct {
		name string
		args []string
		code int
		err  string
	}{
		{"no configuration", []string{"eval"}, 2, "tessera: no configuration file; use --config or TESSERA_CONFIG\n"},
		{"invalid configuration", []string{"eval", "--config", writeConfig(t, "policy: {default_action: maybe}\n")}, 2, "tessera: policy.default_action (line 1, column 26): expected allow or deny\n"},
		{"invalid address", []string{"eval", "--config", path, "--ip", "s3cr3t"}, 2, "tessera: invalid --ip; want an IP address\n"},
		{"address with a zone", []string{"eval", "--config", path, "--ip", "fe80::1%eth0"}, 2, "tessera: invalid --ip; want an IP address\n"},
		{"header without a colon", []string{"eval", "--config", path, "--header", "s3cr3t"}, 2, "tessera: invalid --header; want 'name: value'\n"},
		{"header name", []string{"eval", "--config", path, "--header", "x s3cr3t: v"}, 2, "tessera: invalid --header; want 'name: value'\n"},
		{"output", []string{"eval", "--config", path, "--output", "s3cr3t"}, 2, "tessera: invalid --output; want text or json\n"},
		{"query in the path", []string{"eval", "--config", path, "--path", "/a?token=s3cr3t"}, 2, "tessera: invalid --path; write the query with --query\n"},
		{"argument", []string{"eval", "--config", path, "s3cr3t"}, 2, "tessera: unexpected arguments; see --help\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := executeWithEnv(tc.args, nil)
			if code != tc.code || out != "" || errOut != tc.err {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}
}

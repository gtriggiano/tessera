package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gtriggiano/tessera/internal/app"
	"github.com/gtriggiano/tessera/internal/config"
)

// setsConfig declares a set from a file, partners.txt next to the
// configuration, a set of values and a set that no rule reads.
const setsConfig = `sets:
  partners:
    type: string
    file: partners.txt
  office:
    type: cidr
    values: [192.0.2.0/24, "2001:db8::/32"]
  unused:
    type: asn
    values: [64496]
policy:
  default_action: deny
  rules:
    - name: office
      action: allow
      when: sets.office.contains(client.ip)
    - name: partners
      action: allow
      when: sets.partners.contains(request.host)
`

// writeSets writes setsConfig and partners.txt in a directory of their own,
// and returns the path of the configuration.
func writeSets(t *testing.T, partners string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"config.yaml": setsConfig, "partners.txt": partners} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "config.yaml")
}

func TestConfigValidateSets(t *testing.T) {
	path := writeSets(t, "# partners\napi.example.com\n")
	// A set that no rule reads is a warning: the configuration stays valid.
	code, out, errOut := execute([]string{"config", "validate", "--config", path})
	if code != 0 || out != "configuration valid\n" || errOut != "tessera: warning: sets.unused (line 8, column 3): the set is not used by any rule, upstream header or decision field\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}

	// The file of every set is read: missing or invalid, it is a
	// configuration error at the key, with the line of the file, without
	// its content or its path.
	code, out, errOut = execute([]string{"config", "validate", "--config", writeSets(t, "api.example.com\n\nbad\x01s3cr3t\n")})
	if code != 2 || out != "" || errOut != "tessera: sets.partners.file (line 4, column 11): line 3 of the file: the value has a control character\n" {
		t.Fatalf("invalid line: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	missing := writeSets(t, "")
	if err := os.Remove(filepath.Join(filepath.Dir(missing), "partners.txt")); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = execute([]string{"config", "validate", "--config", missing})
	if code != 2 || out != "" || errOut != "tessera: sets.partners.file (line 4, column 11): the file does not exist\n" {
		t.Fatalf("missing file: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	// config print reads no set file.
	if code, out, errOut := execute([]string{"config", "print", "--config", missing}); code != 0 || !strings.Contains(out, "    file: "+filepath.Join(filepath.Dir(missing), "partners.txt")+"\n") || errOut != "" {
		t.Fatalf("config print: code=%d stdout=%q stderr=%q", code, out, errOut)
	}

	// max_staleness not longer than refresh is a configuration error.
	stale := writeConfig(t, "sets:\n  partners: {type: string, file: partners.txt, refresh: 1m, max_staleness: 1m}\npolicy: {default_action: deny}\n")
	code, out, errOut = execute([]string{"config", "validate", "--config", stale})
	if code != 2 || out != "" || errOut != "tessera: sets.partners.max_staleness (line 2, column 76): must be longer than refresh, or 0s for no limit\n" {
		t.Fatalf("max_staleness not longer than refresh: code=%d stdout=%q stderr=%q", code, out, errOut)
	}

	// A string passed to a set of networks stops the configuration at its
	// position.
	bad := writeConfig(t, "sets:\n  office: {type: cidr, values: []}\npolicy:\n  default_action: deny\n  rules:\n    - name: office\n      action: allow\n      when: sets.office.contains(request.host)\n")
	code, out, errOut = execute([]string{"config", "validate", "--config", bad})
	if code != 2 || out != "" || errOut != "tessera: policy.rules[0].when (line 8, column 33): found no matching overload for 'contains' applied to 'tessera.CIDRSet.(string)'\n" {
		t.Fatalf("string to a cidr set: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestEvalSets(t *testing.T) {
	path := writeSets(t, "api.example.com\n")
	// The sets are loaded as serve loads them, the file included.
	code, out, errOut := execute([]string{"eval", "--config", path, "--ip", "198.51.100.7", "--host", "api.example.com"})
	want := `client.ip     198.51.100.7
request.host  "api.example.com"

office    false
partners  true -> allow

decision: allow (rule partners, reason rule)
`
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}

	for _, tc := range []struct {
		name string
		sets []string
		want string
	}{
		{"replaced", []string{"office=198.51.100.0/24"}, "office  true -> allow"},
		{"replaced, values of the type", []string{"office=203.0.113.0/24, ::ffff:198.51.100.7"}, "office  true -> allow"},
		{"empty set", []string{"partners="}, "partners  false"},
		{"two sets", []string{"partners=", "office=192.0.2.0/24"}, "decision: deny (rule default, reason default)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"eval", "--config", path, "--ip", "198.51.100.7", "--host", "api.example.com"}
			for _, s := range tc.sets {
				args = append(args, "--set", s)
			}
			code, out, errOut := execute(args)
			if code != 0 || !strings.Contains(out, tc.want+"\n") || errOut != "" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}

	// The file of a replaced set is not read: it can be missing.
	if err := os.Remove(filepath.Join(filepath.Dir(path), "partners.txt")); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = execute([]string{"eval", "--config", path, "--set", "partners=www.example.com", "--host", "www.example.com"})
	if code != 0 || !strings.Contains(out, "partners  true -> allow\n") || errOut != "" {
		t.Fatalf("replaced set without its file: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = execute([]string{"eval", "--config", path, "--host", "www.example.com"})
	if code != 2 || out != "" || errOut != "tessera: sets.partners.file (line 4, column 11): the file does not exist\n" {
		t.Fatalf("missing file: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestEvalSetErrors(t *testing.T) {
	path := writeSets(t, "api.example.com\n")
	for _, tc := range []struct {
		name string
		sets []string
		err  string
	}{
		{"no equals sign", []string{"s3cr3t"}, "tessera: invalid --set; want name=value,value\n"},
		{"unknown set", []string{"s3cr3t=a"}, "tessera: invalid --set: the configuration declares no set of this name\n"},
		{"invalid value", []string{"office=192.0.2.0/24,s3cr3t"}, "tessera: invalid --set for the set office: value 2: expected an IP address or a network, such as 192.0.2.0/24 or 2001:db8::/32\n"},
		{"empty value", []string{"partners=a.example.com,,b.example.com"}, "tessera: invalid --set for the set partners: value 2: the value is empty\n"},
		{"value too long", []string{"partners=" + strings.Repeat("s3cr3t", 1000)}, "tessera: invalid --set for the set partners: value 1: the value is longer than 4 KiB\n"},
		{"AS0", []string{"unused=AS0"}, "tessera: invalid --set for the set unused: value 1: AS0 is not allowed: write a number from 1 to 4294967295\n"},
		{"given twice", []string{"office=192.0.2.0/24", "office="}, "tessera: invalid --set: the set office is given twice\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"eval", "--config", path}
			for _, s := range tc.sets {
				args = append(args, "--set", s)
			}
			code, out, errOut := execute(args)
			if code != 2 || out != "" || errOut != tc.err {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}
}

// serve loads every set before it runs: an invalid one is a configuration
// error, and the loaded sets reach the server.
func TestServeSets(t *testing.T) {
	path := writeSets(t, "api.example.com\nnot\x01valid\n")
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"serve", "--config", path}, Options{Out: &stdout, Err: &stderr, Serve: noServe(t)})
	if code != 2 || stdout.Len() != 0 || stderr.String() != "tessera: sets.partners.file (line 4, column 11): line 2 of the file: the value has a control character\n" {
		t.Fatalf("invalid set: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	path = writeSets(t, "")
	if err := os.Remove(filepath.Join(filepath.Dir(path), "partners.txt")); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	code = Run(context.Background(), []string{"serve", "--config", path}, Options{Out: &stdout, Err: &stderr, Serve: noServe(t)})
	if code != 2 || stdout.Len() != 0 || stderr.String() != "tessera: sets.partners.file (line 4, column 11): the file does not exist\n" {
		t.Fatalf("missing set file: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	path = writeSets(t, "api.example.com\n")
	var got app.Options
	code = Run(context.Background(), []string{"serve", "--config", path}, Options{Out: &stdout, Err: &stderr, Serve: func(_ *config.Config, opts app.Options) error {
		got = opts
		return nil
	}})
	if code != 0 || got.Sets == nil {
		t.Fatalf("code=%d stderr=%q sets=%v", code, stderr.String(), got.Sets)
	}
	if e, ok := got.Sets.Store.Get("partners"); !ok || !e.Available || !e.Set.ContainsString("api.example.com") {
		t.Errorf("partners: %+v", e)
	}
	if _, ok := got.Sets.Files["partners"]; !ok || len(got.Sets.Files) != 1 {
		t.Errorf("file identities: %+v", got.Sets.Files)
	}
}

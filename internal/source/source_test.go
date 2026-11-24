package source

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gtriggiano/tessera/internal/sets"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "partners.txt")
	writeFile(t, path, "# partners\n203.0.113.0/24\n2001:db8::/32\n")
	set, id, err := ReadFile(path, sets.CIDR, 10)
	if err != nil {
		t.Fatal(err)
	}
	if set.Entries() != 2 || !set.ContainsAddr(netip.MustParseAddr("203.0.113.9")) {
		t.Errorf("entries %d", set.Entries())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if id.Size != info.Size() || !id.ModTime.Equal(info.ModTime()) || id.Inode == 0 {
		t.Errorf("identity %+v, file size %d, modified %v", id, info.Size(), info.ModTime())
	}

	// The same file read again has the same identity; a file renamed over
	// it has another one, even with the same content and time.
	_, again, err := ReadFile(path, sets.CIDR, 10)
	if err != nil || again != id {
		t.Errorf("same file: %+v, %v; want %+v", again, err, id)
	}
	next := filepath.Join(dir, "partners.txt.new")
	writeFile(t, next, "# partners\n203.0.113.0/24\n2001:db8::/32\n")
	if err := os.Chtimes(next, id.ModTime, id.ModTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	_, renamed, err := ReadFile(path, sets.CIDR, 10)
	if err != nil || renamed == id {
		t.Errorf("file renamed over: %+v, %v; want another identity than %+v", renamed, err, id)
	}

	// A symbolic link is followed, as in the layout of a Kubernetes
	// ConfigMap: the identity is that of the file it leads to.
	data := filepath.Join(dir, "..data")
	if err := os.Mkdir(data, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(data, "hosts.txt"), "api.example.com\n")
	link := filepath.Join(dir, "hosts.txt")
	if err := os.Symlink(filepath.Join("..data", "hosts.txt"), link); err != nil {
		t.Fatal(err)
	}
	hosts, linked, err := ReadFile(link, sets.String, 10)
	if err != nil || !hosts.ContainsString("api.example.com") {
		t.Fatalf("through a symbolic link: %v", err)
	}
	target, _ := os.Stat(filepath.Join(data, "hosts.txt"))
	if linked.Size != target.Size() || !linked.ModTime.Equal(target.ModTime()) {
		t.Errorf("identity %+v, want that of the target", linked)
	}
}

func TestReadFileErrors(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "s3cr3t-name.txt")
	for name, tc := range map[string]struct {
		setup func() string
		want  error
	}{
		"missing":   {func() string { return secret }, ErrNotFound},
		"directory": {func() string { return dir }, ErrNotRegular},
		"broken link": {func() string {
			link := filepath.Join(dir, "broken")
			if err := os.Symlink(filepath.Join(dir, "nowhere"), link); err != nil {
				t.Fatal(err)
			}
			return link
		}, ErrNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := ReadFile(tc.setup(), sets.String, 10)
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("the error repeats the path: %v", err)
			}
		})
	}
	if os.Geteuid() != 0 {
		unreadable := filepath.Join(dir, "unreadable.txt")
		writeFile(t, unreadable, "a\n")
		if err := os.Chmod(unreadable, 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ReadFile(unreadable, sets.String, 10); !errors.Is(err, ErrPermission) {
			t.Errorf("unreadable file: %v", err)
		}
	}

	// Invalid content is a *sets.Error, with the line number.
	invalid := filepath.Join(dir, "invalid.txt")
	writeFile(t, invalid, "64496\n\nAS64497 # ok\ns3cr3t\n")
	_, _, err := ReadFile(invalid, sets.ASN, 10)
	var e *sets.Error
	if !errors.As(err, &e) || e.Line != 4 || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("invalid content: %v", err)
	}
	_, _, err = ReadFile(invalid, sets.ASN, 1)
	if !errors.As(err, &e) || e.Line != 3 {
		t.Errorf("more than max_entries: %v", err)
	}
}

// The file of an area set is a GeoJSON document; Problem gives the line, the
// column and the place in the structure of an invalid one, never its content.
func TestReadAreaFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stores.geojson")
	writeFile(t, path, `{"type":"Polygon","coordinates":[[[12.35,41.80],[12.62,41.80],[12.62,42.00],[12.35,42.00],[12.35,41.80]]]}`)
	set, id, err := ReadFile(path, sets.Area, 10)
	if err != nil {
		t.Fatal(err)
	}
	if set.Type() != sets.Area || set.Entries() != 1 || !set.ContainsPoint(41.8919, 12.5113) || id.Sum == [32]byte{} {
		t.Errorf("entries %d, identity %+v", set.Entries(), id)
	}
	for _, tc := range []struct{ content, want string }{
		{"{\"type\":\"FeatureCollection\",\"features\":[\n{\"type\":\"Feature\",\"properties\":{\"name\":\"s3cr3t\"},\"geometry\":{\"type\":\"Polygon\",\"coordinates\":[[[0,0],[1,0],[1,1],[0,1]]]}}]}", "line 2, column 94 of the file: feature 1, ring 1: the ring is not closed: its last position differs from its first"},
		{`{"type":"Polygon","coordinates":"s3cr3t"}`, "line 1, column 33 of the file: the coordinates do not have the shape of a Polygon"},
		{"s3cr3t", "line 1, column 1 of the file: invalid JSON: expected a value"},
	} {
		writeFile(t, path, tc.content)
		_, _, err := ReadFile(path, sets.Area, 10)
		if got := Problem(err); got != tc.want {
			t.Errorf("Problem = %q, want %q", got, tc.want)
		}
	}
	// The problems of text content keep their form.
	writeFile(t, path, "192.0.2.0/24\ns3cr3t\n")
	if _, _, err := ReadFile(path, sets.CIDR, 10); Problem(err) != "line 2 of the file: expected an IP address or a network, such as 192.0.2.0/24 or 2001:db8::/32" {
		t.Errorf("Problem of a text file: %q", Problem(err))
	}
}

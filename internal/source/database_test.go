package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/mmdbtest"
)

func writeDatabase(t *testing.T, path string, w *mmdbtest.Writer) {
	t.Helper()
	data, err := w.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestReadDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s3cr3t-geo.mmdb")
	writeDatabase(t, path, mmdbtest.CityDatabase(6))
	db, id, err := ReadDatabase(path, enrichment.Geo)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if id.Size != info.Size() || id.Sum == ([32]byte{}) || db.Type() != "GeoIP2-City" {
		t.Errorf("identity %+v, type %q", id, db.Type())
	}
	if r, err := db.Geo(netip.MustParseAddr("192.0.2.7")); err != nil || r.Country.Code != "IT" {
		t.Errorf("lookup: %+v, %v", r, err)
	}
	// Errors are fixed texts, without the path.
	writeFile(t, path, "192.0.2.0/24\n")
	for name, tc := range map[string]struct {
		path string
		want string
	}{
		"invalid":   {path, "the file is not a valid MMDB database"},
		"missing":   {path + ".missing", "the file does not exist"},
		"directory": {dir, "not a regular file"},
	} {
		_, _, err := ReadDatabase(tc.path, enrichment.ASN)
		if err == nil || err.Error() != tc.want || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Invalid content keeps the identity of the file read, without its sum.
	_, id, err = Read(path, DatabaseParse(enrichment.Geo))
	var invalid *Invalid
	if !errors.As(err, &invalid) || id.Size == 0 || id.Sum != ([32]byte{}) {
		t.Errorf("invalid content: %+v, %v", id, err)
	}
}

// databases is the Store of the refreshers of the databases: they never
// mark a database unavailable.
type databases struct{ *enrichment.Store }

func (databases) SetAvailable(string, bool) error {
	return errors.New("a database is always available")
}

// databaseObserver records what the refreshers of databases report.
type databaseObserver struct {
	mu     sync.Mutex
	checks []Result
	events []string
}

func (o *databaseObserver) Checked(_ string, r Result, _ time.Duration, _ time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.checks = append(o.checks, r)
}

func (o *databaseObserver) Loaded(name string, db *enrichment.Database, _ time.Duration) {
	o.add(fmt.Sprintf("loaded %s %s", name, db.Type()))
}
func (o *databaseObserver) Failed(name string, err error) {
	o.add("failed " + name + ": " + Problem(err))
}
func (o *databaseObserver) Unavailable(name string) { o.add("unavailable " + name) }
func (o *databaseObserver) Available(name string)   { o.add("available " + name) }

func (o *databaseObserver) add(e string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, e)
}

func (o *databaseObserver) state() ([]Result, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.checks), slices.Clone(o.events)
}

// The refresher of the sets keeps a database up to date too: a new valid
// file replaces the copy, and an invalid or missing one leaves it in use,
// with one Failed for each new failure.
func TestRefreshDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geo.mmdb")
	writeDatabase(t, path, mmdbtest.CountryDatabase())
	store, err := enrichment.NewStore([]enrichment.Kind{enrichment.Geo})
	if err != nil {
		t.Fatal(err)
	}
	read := time.Now()
	db, id, err := ReadDatabase(path, enrichment.Geo)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Replace("geo", db); err != nil {
		t.Fatal(err)
	}
	o := &databaseObserver{}
	file := File[*enrichment.Database]{Name: "geo", Path: path, Parse: DatabaseParse(enrichment.Geo), Refresh: 5 * time.Millisecond, Identity: id, Read: read}
	rs := Start([]File[*enrichment.Database]{file}, databases{store}, o)
	t.Cleanup(func() {
		if err := rs.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	city := func() string {
		r, _ := store.Snapshot().Database(enrichment.Geo).Geo(netip.MustParseAddr("192.0.2.7"))
		return r.City
	}
	var want []string
	waitEvent := func(e string) {
		t.Helper()
		want = append(want, e)
		eventually(t, e, func() bool {
			_, events := o.state()
			return len(events) >= len(want)
		})
		if _, events := o.state(); !slices.Equal(events, want) {
			t.Fatalf("events %q, want %q", events, want)
		}
	}

	writeDatabase(t, path, mmdbtest.CityDatabase(6))
	waitEvent("loaded geo GeoIP2-City")
	if city() != "Rome" {
		t.Fatal("not the copy of the new file")
	}
	in := store.Snapshot().Database(enrichment.Geo)
	replaceFile(t, path, "not a database\n", 0o644)
	waitEvent("failed geo: the file is not a valid MMDB database")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitEvent("failed geo: the file does not exist")
	if store.Snapshot().Database(enrichment.Geo) != in {
		t.Fatal("a failed check replaced the copy")
	}
	checks, _ := o.state()
	for _, r := range []Result{Updated, Rejected, Failed} {
		if !slices.Contains(checks, r) {
			t.Errorf("no %s check in %v", r, checks)
		}
	}
}

// A read asks the file for at most maxChunk bytes at a time, and the end of
// its context stops it between two chunks.
func TestReadInChunks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.mmdb")
	if err := os.WriteFile(path, make([]byte, 3*maxChunk), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var first, after int
	var afterErr error
	parse := func(r io.Reader, size int64) (int, error) {
		buf := make([]byte, size)
		first, _ = r.Read(buf)
		cancel()
		after, afterErr = r.Read(buf)
		return first, nil
	}
	if _, _, err := read(ctx, path, parse); err != ErrRead {
		t.Errorf("a read stopped by its context: %v, want %v", err, ErrRead)
	}
	if first == 0 || first > maxChunk || after != 0 || afterErr == nil {
		t.Errorf("first read of %d bytes, then %d bytes and %v", first, after, afterErr)
	}
	// Uncancelled, the whole file is read, a chunk at a time.
	db, _, err := Read(path, func(r io.Reader, size int64) (int64, error) {
		var total int64
		buf := make([]byte, size)
		for {
			n, err := r.Read(buf)
			if n > maxChunk {
				return 0, errors.New("a chunk too large")
			}
			total += int64(n)
			if err == io.EOF {
				return total, nil
			}
			if err != nil {
				return 0, err
			}
		}
	})
	if err != nil || db != 3*maxChunk {
		t.Errorf("read %d bytes, %v", db, err)
	}
}

// The ceiling of a database rejects a larger file, through ReadDatabase and
// through its refresher: the check is rejected, with the fixed text, and the
// copy in use stays. The test lowers the ceiling of 512 MiB.
func TestDatabaseCeiling(t *testing.T) {
	country, city := mustBytes(t, mmdbtest.CountryDatabase()), mustBytes(t, mmdbtest.CityDatabase(6))
	if len(city) <= len(country) {
		t.Fatalf("the City database (%d bytes) is not larger than the Country one (%d bytes)", len(city), len(country))
	}
	defer func(saved int64) { databaseMaxBytes = saved }(databaseMaxBytes)
	databaseMaxBytes = int64(len(country))

	path := filepath.Join(t.TempDir(), "geo.mmdb")
	writeDatabase(t, path, mmdbtest.CityDatabase(6))
	if _, _, err := ReadDatabase(path, enrichment.Geo); err == nil || err.Error() != "the file is larger than 512 MiB" {
		t.Fatalf("ReadDatabase: %v", err)
	}

	writeDatabase(t, path, mmdbtest.CountryDatabase())
	store, _ := enrichment.NewStore([]enrichment.Kind{enrichment.Geo})
	read := time.Now()
	db, id, err := ReadDatabase(path, enrichment.Geo)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Replace("geo", db)
	o := &databaseObserver{}
	rs := Start([]File[*enrichment.Database]{{Name: "geo", Path: path, Parse: DatabaseParse(enrichment.Geo), Refresh: 5 * time.Millisecond, Identity: id, Read: read}}, databases{store}, o)
	defer func() { _ = rs.Stop(context.Background()) }()
	writeDatabase(t, path, mmdbtest.CityDatabase(6))
	eventually(t, "the rejection", func() bool {
		checks, events := o.state()
		return len(events) > 0 && slices.Contains(checks, Rejected)
	})
	if _, events := o.state(); !slices.Equal(events, []string{"failed geo: the file is larger than 512 MiB"}) {
		t.Errorf("events %q", events)
	}
	if store.Snapshot().Database(enrichment.Geo) != db {
		t.Error("the copy in use changed")
	}
}

func mustBytes(t *testing.T, w *mmdbtest.Writer) []byte {
	t.Helper()
	data, err := w.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A database of the other kind is rejected on a reload too, and the copy in
// use stays.
func TestRefreshSwappedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asn.mmdb")
	writeDatabase(t, path, mmdbtest.ASNDatabase())
	store, _ := enrichment.NewStore([]enrichment.Kind{enrichment.ASN})
	read := time.Now()
	db, id, err := ReadDatabase(path, enrichment.ASN)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Replace("asn", db)
	o := &databaseObserver{}
	rs := Start([]File[*enrichment.Database]{{Name: "asn", Path: path, Parse: DatabaseParse(enrichment.ASN), Refresh: 5 * time.Millisecond, Identity: id, Read: read}}, databases{store}, o)
	defer func() { _ = rs.Stop(context.Background()) }()
	writeDatabase(t, path, mmdbtest.CityDatabase(6))
	eventually(t, "the rejection", func() bool {
		checks, events := o.state()
		return len(events) > 0 && slices.Contains(checks, Rejected)
	})
	if _, events := o.state(); !slices.Equal(events, []string{"failed asn: a record of the database does not have the schema of a GeoLite2 ASN database"}) {
		t.Errorf("events %q", events)
	}
	if store.Snapshot().Database(enrichment.ASN) != db {
		t.Error("the copy in use changed")
	}
}

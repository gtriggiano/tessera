package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/sets"
)

// observer records what the refreshers report: the results of the checks,
// and the other calls as events.
type observer struct {
	mu            sync.Mutex
	checks        []Result
	events        []string
	lastSuccess   time.Time // the end of the last successful check
	unavailableAt time.Time // when Unavailable was last called
}

func (o *observer) Checked(set string, r Result, took time.Duration, at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if took < 0 || at.IsZero() {
		panic("invalid check")
	}
	o.checks = append(o.checks, r)
	if r == Updated || r == Unchanged {
		o.lastSuccess = at
	}
}

func (o *observer) Loaded(set string, c *sets.Set, took time.Duration) {
	o.add(fmt.Sprintf("loaded %s %d", set, c.Entries()))
}

func (o *observer) Failed(set string, err error) { o.add("failed " + set + ": " + Problem(err)) }
func (o *observer) Unavailable(set string) {
	o.mu.Lock()
	o.unavailableAt = time.Now()
	o.mu.Unlock()
	o.add("unavailable " + set)
}

func (o *observer) Available(set string) { o.add("available " + set) }

func (o *observer) add(e string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, e)
}

// times returns the end of the last successful check, and when the set
// last became unavailable.
func (o *observer) times() (lastSuccess, unavailableAt time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastSuccess, o.unavailableAt
}

// state returns a copy of the results and of the events so far.
func (o *observer) state() ([]Result, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.checks), slices.Clone(o.events)
}

func (o *observer) count(r Result) int {
	checks, _ := o.state()
	n := 0
	for _, c := range checks {
		if c == r {
			n++
		}
	}
	return n
}

// eventually waits until cond holds, and fails after 10 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// afterNewCheck waits until a check that started after the call has
// recorded everything it records. A check records its result first, then
// changes the store and records its events, all before the next check
// starts: three more results mean that the second of them started after the
// call, since the first may have started before it, and that the second has
// recorded its events, since the third has begun.
func (o *observer) afterNewCheck(t *testing.T) {
	t.Helper()
	checks, _ := o.state()
	eventually(t, "a new check", func() bool {
		now, _ := o.state()
		return len(now) >= len(checks)+3
	})
}

// wantEvents checks that the events so far are want.
func (o *observer) wantEvents(t *testing.T, want ...string) {
	t.Helper()
	if _, events := o.state(); !slices.Equal(events, want) {
		t.Fatalf("events %q, want %q", events, want)
	}
}

// waitEvents waits until there are as many events as in want, then checks
// that they are want. The store changes before the events that report the
// change are recorded: a test that waits for a state of the store, then
// reads the events, must wait for the events.
func (o *observer) waitEvents(t *testing.T, want ...string) {
	t.Helper()
	eventually(t, fmt.Sprintf("%d events", len(want)), func() bool {
		_, events := o.state()
		return len(events) >= len(want)
	})
	o.wantEvents(t, want...)
}

// replaceFile writes content to a new file and renames it over path, as a
// safe update does.
func replaceFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	tmp := path + ".tmp"
	writeFile(t, tmp, content)
	if err := os.Chmod(tmp, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// refresherGoroutines counts the goroutines of the refreshers.
func refresherGoroutines() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		if strings.Contains(g, "internal/source.(*refresher)") {
			n++
		}
	}
	return n
}

// fileSet is the set "blocked", of type cidr, read from path, with its store.
type fileSet struct {
	store *sets.Store
	file  File[*sets.Set]
}

func newFileSet(t *testing.T, path string, refresh, maxStaleness time.Duration) fileSet {
	t.Helper()
	store, err := sets.NewStore([]sets.Declaration{{Name: "blocked", Type: sets.CIDR, OnUnavailable: sets.Fail}})
	if err != nil {
		t.Fatal(err)
	}
	read := time.Now()
	set, id, err := ReadFile(path, sets.CIDR, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Replace("blocked", set); err != nil {
		t.Fatal(err)
	}
	return fileSet{store: store, file: File[*sets.Set]{Name: "blocked", Path: path, Parse: SetParse(sets.CIDR, 2), Refresh: refresh, MaxStaleness: maxStaleness, Identity: id, Read: read}}
}

// start starts the refresher of s, and stops it at the end of the test,
// which fails if a goroutine of a refresher is left.
func (s fileSet) start(t *testing.T, o *observer) *Refreshers {
	t.Helper()
	rs := Start([]File[*sets.Set]{s.file}, s.store, o)
	t.Cleanup(func() {
		if err := rs.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
		if n := refresherGoroutines(); n != 0 {
			t.Errorf("%d goroutines of the refreshers left after Stop", n)
		}
	})
	return rs
}

func (s fileSet) copy() sets.Entry {
	e, _ := s.store.Get("blocked")
	return e
}

func (s fileSet) contains(addr string) bool {
	return s.copy().Set.ContainsAddr(netip.MustParseAddr(addr))
}

// lastCheck returns the result of the last check, or "" before the first.
func (o *observer) lastCheck() Result {
	checks, _ := o.state()
	if len(checks) == 0 {
		return ""
	}
	return checks[len(checks)-1]
}

// writeInPlace writes content over the start of the file at path, in one
// write, without truncating it first: content is longer than what it
// replaces.
func writeInPlace(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(content), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// A changed file replaces the copy, whether it is written in place or
// renamed over the path; an unchanged one does not.
func TestRefreshUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	writeFile(t, path, "198.51.100.0/24\n")
	set := newFileSet(t, path, 5*time.Millisecond, 0)
	o := &observer{}
	set.start(t, o)

	// Unchanged: the copy stays.
	first := set.copy().Set
	o.afterNewCheck(t)
	if set.copy().Set != first || o.count(Unchanged) < 2 {
		t.Fatalf("an unchanged file: %d unchanged checks", o.count(Unchanged))
	}

	// Written in place.
	writeInPlace(t, path, "203.0.113.0/24\n2001:db8::/32\n")
	o.waitEvents(t, "loaded blocked 2")
	if !set.contains("203.0.113.7") {
		t.Fatal("not the copy of the file written in place")
	}
	// Renamed over the path.
	replaceFile(t, path, "192.0.2.0/24\n", 0o644)
	o.waitEvents(t, "loaded blocked 2", "loaded blocked 1")
	if e := set.copy(); !e.Available || !set.contains("192.0.2.7") || set.contains("203.0.113.7") || e.Set.Entries() != 1 {
		t.Fatalf("copy %+v", e)
	}
	if o.count(Updated) != 2 {
		t.Errorf("%d updated checks", o.count(Updated))
	}

	// A new file with the same content counts as unchanged, without a new
	// copy.
	current := set.copy().Set
	replaceFile(t, path, "192.0.2.0/24\n", 0o644)
	o.afterNewCheck(t)
	if set.copy().Set != current || o.lastCheck() != Unchanged {
		t.Fatal("the same content made a new copy")
	}
	o.wantEvents(t, "loaded blocked 2", "loaded blocked 1")
}

// writeConfigMap lays out dir as the kubelet does for a ConfigMap volume:
// each file is a link to ..data/<name>, and ..data a link to the directory
// of the version, which is swapped atomically, the old one then removed.
func writeConfigMap(t *testing.T, dir string, version int, files map[string]string) {
	t.Helper()
	versionDir := fmt.Sprintf("..2026_11_22_10_00_%02d.%d", version, version)
	if err := os.Mkdir(filepath.Join(dir, versionDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		writeFile(t, filepath.Join(dir, versionDir, name), content)
	}
	data := filepath.Join(dir, "..data")
	old, _ := os.Readlink(data)
	if err := os.Symlink(versionDir, data+"_tmp"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(data+"_tmp", data); err != nil {
		t.Fatal(err)
	}
	for name := range files {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if old != "" {
		if err := os.RemoveAll(filepath.Join(dir, old)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRefreshConfigMap(t *testing.T) {
	dir := t.TempDir()
	writeConfigMap(t, dir, 1, map[string]string{"blocked.txt": "198.51.100.0/24\n", "other.txt": "a\n"})
	set := newFileSet(t, filepath.Join(dir, "blocked.txt"), 5*time.Millisecond, 0)
	o := &observer{}
	set.start(t, o)

	writeConfigMap(t, dir, 2, map[string]string{"blocked.txt": "203.0.113.0/24\n", "other.txt": "a\n"})
	o.waitEvents(t, "loaded blocked 1")
	if !set.contains("203.0.113.7") {
		t.Fatal("not the copy of the new version of the ConfigMap")
	}
	// Another file of the ConfigMap changes: this one is unchanged.
	current := set.copy().Set
	writeConfigMap(t, dir, 3, map[string]string{"blocked.txt": "203.0.113.0/24\n", "other.txt": "b\n"})
	o.afterNewCheck(t)
	if set.copy().Set != current {
		t.Fatal("a version of the ConfigMap with the same file made a new copy")
	}
	o.wantEvents(t, "loaded blocked 1")
	if checks, _ := o.state(); slices.ContainsFunc(checks, func(r Result) bool { return r == Failed || r == Rejected }) {
		t.Errorf("checks %v", checks)
	}
}

// A missing, unreadable or invalid file fails the checks, and the copy in
// use stays: one Failed for each new failure, not for each check.
func TestRefreshFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	content := "198.51.100.0/24\n" // of the copy in use
	writeFile(t, path, content)
	set := newFileSet(t, path, 5*time.Millisecond, 0)
	inUse := set.copy().Set
	o := &observer{}
	set.start(t, o)
	var want []string
	failsWith := func(problem string, result Result) {
		t.Helper()
		want = append(want, "failed blocked: "+problem)
		eventually(t, problem, func() bool {
			_, events := o.state()
			return len(events) >= len(want)
		})
		o.afterNewCheck(t)
		o.wantEvents(t, want...)
		if last := o.lastCheck(); last != result {
			t.Fatalf("last check %s, want %s", last, result)
		}
		if e := set.copy(); e.Set != inUse || !e.Available {
			t.Fatalf("the copy changed: %+v", e)
		}
	}

	if mkfifo(path+".fifo") == nil {
		// A FIFO is refused without being opened: a read would block.
		if err := os.Rename(path+".fifo", path); err != nil {
			t.Fatal(err)
		}
		failsWith("not a regular file", Failed)
	}
	replaceFile(t, path, "198.51.100.0/24\nexample.com\n", 0o644)
	failsWith("line 2 of the file: expected an IP address or a network, such as 192.0.2.0/24 or 2001:db8::/32", Rejected)
	// Another version with the same problem is a new failure, and so is
	// another problem.
	replaceFile(t, path, "198.51.100.0/24\nexample.org\n", 0o644)
	failsWith("line 2 of the file: expected an IP address or a network, such as 192.0.2.0/24 or 2001:db8::/32", Rejected)
	// Modified long ago, this version has an identity that can be trusted.
	writeFile(t, path+".tmp", "198.51.100.0/24\n203.0.113.0/24\n192.0.2.0/24\n")
	if err := os.Chtimes(path+".tmp", time.Time{}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
	failsWith("line 3 of the file: more entries than max_entries", Rejected)

	if os.Geteuid() != 0 {
		// A rejected version that can be trusted is not read again: made
		// unreadable, which does not change its identity, it is still
		// rejected.
		checks, _ := o.state()
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		o.afterNewCheck(t)
		now, _ := o.state()
		for _, r := range now[len(checks):] {
			if r != Rejected {
				t.Fatalf("the rejected version was read again: %v", now[len(checks):])
			}
		}
		o.wantEvents(t, want...)

		// A new version that cannot be opened fails; made readable, it is
		// read at the next check.
		replaceFile(t, path, "203.0.113.0/24\n", 0)
		failsWith("the file cannot be opened: permission denied", Failed)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		want = append(want, "loaded blocked 1")
		o.waitEvents(t, want...)
		if !set.contains("203.0.113.7") {
			t.Fatal("not the copy of the file made readable")
		}
		content, inUse = "203.0.113.0/24\n", set.copy().Set
	}

	// After a successful check, the same failure is a new one.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	failsWith("the file does not exist", Failed)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	failsWith("not a regular file", Failed)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	failsWith("the file does not exist", Failed)

	// A file restored with the content of the copy in use is unchanged,
	// without a new copy.
	replaceFile(t, path, content, 0o644)
	eventually(t, "the restored file", func() bool { return o.lastCheck() == Unchanged })
	o.wantEvents(t, want...)
	if set.copy().Set != inUse {
		t.Fatal("the restored file made a new copy")
	}
}

// A set with max_staleness becomes unavailable when its last successful
// check gets older, at that moment, not at the next check. Before the first
// check, the age counts from Start, not from the read of the copy: a copy
// read long before Start stays available for max_staleness after it.
func TestStaleAtTheMoment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	writeFile(t, path, "198.51.100.0/24\n")
	const maxStaleness = 100 * time.Millisecond
	set := newFileSet(t, path, time.Hour, maxStaleness)
	first := set.copy().Set
	time.Sleep(2 * maxStaleness)
	o := &observer{}
	started := time.Now()
	set.start(t, o)
	o.waitEvents(t, "unavailable blocked")
	if _, at := o.times(); at.Sub(started) < maxStaleness {
		t.Fatalf("unavailable %v after Start, before max_staleness", at.Sub(started))
	}
	if checks, _ := o.state(); len(checks) != 0 || set.copy().Available || set.copy().Set != first {
		t.Fatalf("checks %v, copy %+v", checks, set.copy())
	}
}

// Failed checks make the set stale, max_staleness after the last successful
// one; the next successful check makes it available again, with a new copy
// or with the same.
func TestStaleAndAvailableAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	writeFile(t, path, "198.51.100.0/24\n")
	// max_staleness is two hundred checks: a goroutine held off for less
	// than a second makes no premature staleness.
	const maxStaleness = time.Second
	set := newFileSet(t, path, 5*time.Millisecond, maxStaleness)
	o := &observer{}
	set.start(t, o)
	o.afterNewCheck(t)
	if !set.copy().Available {
		t.Fatal("unavailable while the checks succeed")
	}

	for _, events := range [][]string{
		{"failed blocked: the file does not exist", "unavailable blocked", "loaded blocked 1", "available blocked"},
		// The same content: the set is available again without a new copy.
		{"failed blocked: the file does not exist", "unavailable blocked", "available blocked"},
	} {
		_, before := o.state()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		o.waitEvents(t, append(before, events[:2]...)...)
		if last, at := o.times(); at.Sub(last) < maxStaleness {
			t.Fatalf("unavailable %v after the last successful check, before max_staleness", at.Sub(last))
		}
		if e := set.copy(); e.Available || e.Set == nil {
			t.Fatalf("stale copy %+v", e)
		}
		replaceFile(t, path, "203.0.113.0/24\n", 0o644)
		o.waitEvents(t, append(before, events...)...)
		if !set.copy().Available || !set.contains("203.0.113.7") {
			t.Fatalf("not available with the new copy: %+v", set.copy())
		}
	}
}

// pinnedStat is a stat whose files keep the modification time modTime, as a
// file rewritten in place within the precision of the modification times of
// its file system does.
func pinnedStat(modTime time.Time) func(string) (fs.FileInfo, error) {
	return func(name string) (fs.FileInfo, error) {
		info, err := os.Stat(name)
		if err != nil {
			return nil, err
		}
		return pinnedInfo{info, modTime}, nil
	}
}

type pinnedInfo struct {
	fs.FileInfo
	modTime time.Time
}

func (p pinnedInfo) ModTime() time.Time { return p.modTime }

// startPinned loads the set from path, whose modification time is set to
// modTime, and starts its refresher with a stat that keeps that time.
func startPinned(t *testing.T, path string, modTime time.Time) (fileSet, *observer) {
	t.Helper()
	writeFile(t, path, "198.51.100.0/24\n")
	if err := os.Chtimes(path, time.Time{}, modTime); err != nil {
		t.Fatal(err)
	}
	set := newFileSet(t, path, 5*time.Millisecond, 0)
	o := &observer{}
	rs := start([]File[*sets.Set]{set.file}, set.store, o, pinnedStat(modTime))
	t.Cleanup(func() {
		if err := rs.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return set, o
}

// A version modified less than racyMargin before its read is read again at
// each check: a rewrite in place of the same size that keeps its identity is
// seen, rejected or not.
func TestRacyIdentityIsReadAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	set, o := startPinned(t, path, time.Now())
	writeInPlace(t, path, "198.51.100.0/2x\n")
	o.waitEvents(t, "failed blocked: line 1 of the file: expected an IP address or a network, such as 192.0.2.0/24 or 2001:db8::/32")
	writeInPlace(t, path, "198.51.101.0/24\n")
	o.waitEvents(t, "failed blocked: line 1 of the file: expected an IP address or a network, such as 192.0.2.0/24 or 2001:db8::/32", "loaded blocked 1")
	if !set.contains("198.51.101.7") || set.contains("198.51.100.7") {
		t.Fatalf("copy %+v", set.copy())
	}
}

// A version modified long before its read is trusted: it is not read again
// while its identity stays the same, and a rewrite that keeps the identity
// is not seen.
func TestTrustedIdentityIsNotReadAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	set, o := startPinned(t, path, time.Now().Add(-time.Hour))
	first := set.copy().Set
	writeInPlace(t, path, "198.51.101.0/24\n")
	o.afterNewCheck(t)
	o.wantEvents(t)
	if set.copy().Set != first {
		t.Fatal("the trusted version was read again")
	}
}

// A racy version is read again until its modification time is racyMargin
// older than the start of a read; from then on, it is trusted.
func TestRacyIdentityBecomesTrusted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	modTime := time.Now().Add(-racyMargin + 300*time.Millisecond)
	set, o := startPinned(t, path, modTime)
	first := set.copy().Set
	eventually(t, "racyMargin to pass", func() bool { return time.Since(modTime) > racyMargin })
	o.afterNewCheck(t)
	writeInPlace(t, path, "198.51.101.0/24\n")
	o.afterNewCheck(t)
	o.wantEvents(t)
	if set.copy().Set != first || o.count(Updated) != 0 {
		t.Fatal("the version was read again after racyMargin")
	}
}

// The next check starts Refresh after the previous one ends: checks never
// overlap or queue up.
func TestChecksDoNotQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	writeFile(t, path, "198.51.100.0/24\n")
	set := newFileSet(t, path, 5*time.Millisecond, 0)
	var mu sync.Mutex
	var begins, ends []time.Time
	inFlight, overlap := 0, false
	slow := func(name string) (fs.FileInfo, error) {
		mu.Lock()
		inFlight++
		overlap = overlap || inFlight > 1
		begins = append(begins, time.Now())
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		inFlight--
		ends = append(ends, time.Now())
		return os.Stat(name)
	}
	o := &observer{}
	rs := start([]File[*sets.Set]{set.file}, set.store, o, slow)
	eventually(t, "five checks", func() bool { return o.count(Unchanged) >= 5 })
	if err := rs.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if overlap {
		t.Fatal("two checks at once")
	}
	for i := 1; i < len(begins); i++ {
		if gap := begins[i].Sub(ends[i-1]); gap < set.file.Refresh {
			t.Fatalf("check %d started %v after the end of the previous one, before refresh", i, gap)
		}
	}
}

// Stop cuts a check short, records nothing more, and waits for the
// goroutines within its context.
func TestStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	writeFile(t, path, "198.51.100.0/24\n")
	set := newFileSet(t, path, time.Millisecond, time.Hour)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	stuck := func(name string) (fs.FileInfo, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return os.Stat(name)
	}
	o := &observer{}
	rs := start([]File[*sets.Set]{set.file}, set.store, o, stuck)
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := rs.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop with a stuck check: %v", err)
	}
	// The file changes while the check is stuck; once it ends, nothing is
	// recorded.
	writeFile(t, path, "203.0.113.0/24\n")
	close(release)
	if err := rs.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := refresherGoroutines(); n != 0 {
		t.Fatalf("%d goroutines left", n)
	}
	if checks, events := o.state(); len(checks) != 0 || len(events) != 0 || set.contains("203.0.113.7") {
		t.Fatalf("recorded after Stop: %v %v", checks, events)
	}
	// A nil Refreshers has nothing to stop.
	if err := (*Refreshers)(nil).Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Cancel stops the refreshers without waiting for a check in progress, which
// records nothing once it returns; Stop then waits for the goroutines.
func TestCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	writeFile(t, path, "198.51.100.0/24\n")
	set := newFileSet(t, path, time.Millisecond, 0)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	stuck := func(name string) (fs.FileInfo, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return os.Stat(name)
	}
	o := &observer{}
	rs := start([]File[*sets.Set]{set.file}, set.store, o, stuck)
	<-entered
	rs.Cancel()
	writeFile(t, path, "203.0.113.0/24\n")
	close(release)
	if err := rs.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if checks, events := o.state(); len(checks) != 0 || len(events) != 0 || set.contains("203.0.113.7") {
		t.Fatalf("recorded after Cancel: %v %v", checks, events)
	}
	(*Refreshers)(nil).Cancel()
}

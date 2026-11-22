package source

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gtriggiano/tessera/internal/sets"
)

// Result is the outcome of a check of a file.
type Result = sets.RefreshResult

// The results of a check of a file.
const (
	// Updated: a new copy replaced the one in use.
	Updated = sets.RefreshUpdated
	// Unchanged: the file has not changed, or holds the content of the copy
	// in use.
	Unchanged = sets.RefreshUnchanged
	// Rejected: the content is invalid, or exceeds a ceiling; the copy in
	// use stays.
	Rejected = sets.RefreshRejected
	// Failed: the file does not exist or cannot be read; the copy in use
	// stays.
	Failed = sets.RefreshError
)

// racyMargin is how much older than the start of its read the modification
// time of a version of a file must be for its identity to be trusted. A file
// system records modification times with its own precision: a nanosecond on
// ext4, XFS or APFS, a second on ext3 or HFS+, two seconds on FAT. A file
// rewritten in place, with the same size, within that precision of the
// version read keeps the identity of that version. Like git with the files
// it calls racily clean, a refresher reads again, at each check, a version
// whose modification time is within racyMargin of the start of its read,
// until it is older.
const racyMargin = 2 * time.Second

// racy reports whether the identity of a version modified at modTime, whose
// read started at read, cannot be trusted.
func racy(modTime, read time.Time) bool { return !modTime.Before(read.Add(-racyMargin)) }

// File is a set whose source is a file, as its refresher needs it.
type File struct {
	Name       string
	Path       string
	Type       sets.Type
	MaxEntries int
	// Refresh is the time from the end of a check to the start of the next.
	Refresh time.Duration
	// MaxStaleness is how long the copy in use stays available after the
	// last successful check, or after Start before the first one; 0 sets no
	// limit.
	MaxStaleness time.Duration
	// Identity is that of the version of the file that the copy in the
	// store was read from, and Read is when that read started.
	Identity Identity
	Read     time.Time
}

// Observer receives what the refreshers do, from their goroutines: the
// calls about one set come one at a time, and none comes after Stop has
// returned. The calls about different sets can be concurrent.
type Observer interface {
	// Checked reports a check of the file of set: its result, how long it
	// took, and when it ended.
	Checked(set string, r Result, took time.Duration, at time.Time)
	// Loaded reports that a new copy of set, of entries entries, read in
	// took, is in use.
	Loaded(set string, entries int, took time.Duration)
	// Failed reports a new failure of the checks of set, rejected or not: a
	// failure on another version of the file, or with another error, than
	// the last one. A check that fails again in the same way reports only
	// Checked.
	Failed(set string, err error)
	// Unavailable reports that set became unavailable, because its last
	// successful check is older than its MaxStaleness.
	Unavailable(set string)
	// Available reports that set is available again, after a successful
	// check.
	Available(set string)
}

// Refreshers keep the copies of the sets read from files up to date in a
// sets.Store, each in the background with its own goroutine:
//
//   - Every Refresh, counted from the end of the previous check, a check
//     reads the state of the file, following symbolic links. A file with the
//     device, inode, size and modification time of the copy in use is
//     unchanged, unless that version is racy (see racyMargin). Any other
//     file is read; content that is invalid, or that exceeds a ceiling, is
//     rejected and the copy in use stays, and content equal to that of the
//     copy in use is unchanged. Otherwise the new copy replaces the one in
//     use, atomically. A file that does not exist, or cannot be read, fails
//     the check, and the copy in use stays. A version of the file that was
//     rejected, and is not racy, is not read again: each check rejects it at
//     once, until the file changes.
//   - A set with a MaxStaleness has a second goroutine, which marks the set
//     unavailable as soon as its last successful check, unchanged or
//     updated, is older than MaxStaleness, even while a check is in
//     progress. Before the first check, the age counts from Start, not from
//     the read of the copy in the store. The next successful check makes the
//     set available again.
//
// The decisions read the copies and their availability from the store, and
// never wait for a refresher.
type Refreshers struct {
	cancel  context.CancelFunc
	running atomic.Int64  // goroutines that have not returned
	done    chan struct{} // closed once every goroutine has returned
	all     []*refresher
}

// Start starts a refresher for each of files, whose copies are in store,
// and reports what they do to obs.
func Start(files []File, store *sets.Store, obs Observer) *Refreshers {
	return start(files, store, obs, os.Stat)
}

// start is Start, with the function that reads the state of a file.
func start(files []File, store *sets.Store, obs Observer, stat func(string) (fs.FileInfo, error)) *Refreshers {
	ctx, cancel := context.WithCancel(context.Background())
	rs := &Refreshers{cancel: cancel, done: make(chan struct{})}
	// One more than the goroutines, until they have all started.
	rs.running.Add(1)
	for _, f := range files {
		// The first window of staleness starts now: the first check comes
		// Refresh later.
		r := &refresher{File: f, store: store, obs: obs, stat: stat, current: f.Identity, racy: racy(f.Identity.ModTime, f.Read), lastSuccess: time.Now()}
		e, _ := store.Get(f.Name)
		r.available = e.Available
		rs.all = append(rs.all, r)
		if f.MaxStaleness > 0 {
			r.stale = time.NewTimer(f.MaxStaleness)
			rs.goroutine(func() { r.watch(ctx) })
		}
		rs.goroutine(func() { r.run(ctx) })
	}
	rs.returned()
	return rs
}

// goroutine runs f in a new goroutine, which Stop waits for.
func (rs *Refreshers) goroutine(f func()) {
	rs.running.Add(1)
	go func() {
		defer rs.returned()
		f()
	}()
}

// returned counts a goroutine that has returned, and closes done after the
// last one.
func (rs *Refreshers) returned() {
	if rs.running.Add(-1) == 0 {
		close(rs.done)
	}
}

// Stop stops every refresher: a check in progress is cut short, at the
// latest when the read of the file returns, and recorded nowhere. No call
// to the Observer follows the return of Stop. Stop waits until every
// goroutine has returned, or until ctx ends, and then returns ctx.Err(). A
// nil Refreshers has nothing to stop.
func (rs *Refreshers) Stop(ctx context.Context) error {
	if rs == nil {
		return nil
	}
	rs.cancel()
	for _, r := range rs.all {
		r.mu.Lock()
		r.stopped = true
		if r.stale != nil {
			r.stale.Stop()
		}
		r.mu.Unlock()
	}
	select {
	case <-rs.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// refresher keeps the copy of one set up to date.
type refresher struct {
	File
	store *sets.Store
	obs   Observer
	stat  func(string) (fs.FileInfo, error)

	// Owned by the goroutine of the checks, which changes them with mu held.
	current Identity // of the copy in use
	racy    bool     // current cannot be trusted: see racyMargin
	failure *failure // the last failure, nil after a successful check

	// mu serializes the changes of the store and the calls to obs.
	mu          sync.Mutex
	stopped     bool
	available   bool
	lastSuccess time.Time   // or the start of the checks, before the first success
	stale       *time.Timer // nil without MaxStaleness
}

// failure is a check that failed: the version of the file, whether its
// identity cannot be trusted, the result and the error.
type failure struct {
	id     Identity
	racy   bool
	result Result
	err    error
}

// outcome is what a check found.
type outcome struct {
	result Result
	set    *sets.Set // the new copy, when Updated
	id     Identity  // of the version of the file
	racy   bool      // id cannot be trusted: see racyMargin
	err    error     // of a failure
}

// run checks the file every Refresh, from the end of the previous check, so
// that checks never queue up, until ctx ends.
func (r *refresher) run(ctx context.Context) {
	timer := time.NewTimer(r.Refresh)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		r.check(ctx)
		timer.Reset(r.Refresh)
	}
}

// watch marks the set unavailable when its timer of staleness fires, until
// ctx ends.
func (r *refresher) watch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stale.C:
			r.expire()
		}
	}
}

func (r *refresher) expire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || !r.available {
		return
	}
	if age := time.Since(r.lastSuccess); age < r.MaxStaleness {
		// A check succeeded as the timer fired.
		r.stale.Reset(r.MaxStaleness - age)
		return
	}
	r.available = false
	_ = r.store.SetAvailable(r.Name, false)
	r.obs.Unavailable(r.Name)
}

// check checks the file once, and records the result.
func (r *refresher) check(ctx context.Context) {
	begin := time.Now()
	o := r.inspect(ctx, begin)
	if ctx.Err() != nil {
		return
	}
	end := time.Now()
	took := end.Sub(begin)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	r.obs.Checked(r.Name, o.result, took, end)
	switch o.result {
	case Updated:
		// The copy has the type of the set.
		_ = r.store.Replace(r.Name, o.set)
		r.current, r.racy, r.failure = o.id, o.racy, nil
		r.obs.Loaded(r.Name, o.set.Entries(), took)
		r.succeeded(end, true)
	case Unchanged:
		r.current, r.racy, r.failure = o.id, o.racy, nil
		r.succeeded(end, false)
	default:
		last := r.failure
		r.failure = &failure{id: o.id, racy: o.racy, result: o.result, err: o.err}
		if last == nil || !sameFile(last.id, o.id) || Problem(last.err) != Problem(o.err) {
			r.obs.Failed(r.Name, o.err)
		}
	}
}

// succeeded records a successful check, at, which replaced the copy in use
// or not. r.mu is held.
func (r *refresher) succeeded(at time.Time, replaced bool) {
	r.lastSuccess = at
	if r.stale != nil {
		r.stale.Reset(r.MaxStaleness)
	}
	if !r.available {
		r.available = true
		if !replaced {
			// Replace marks a new copy available.
			_ = r.store.SetAvailable(r.Name, true)
		}
		r.obs.Available(r.Name)
	}
}

// inspect reads the state of the file and, when it has changed or its
// identity cannot be trusted, its content. begin is the start of the check.
func (r *refresher) inspect(ctx context.Context, begin time.Time) outcome {
	info, err := r.stat(r.Path)
	if err != nil {
		return outcome{result: Failed, err: openError(err)}
	}
	id := identity(info)
	switch {
	case !info.Mode().IsRegular():
		return outcome{result: Failed, id: id, err: ErrNotRegular}
	case sameFile(id, r.current) && !r.racy:
		return outcome{result: Unchanged, id: r.current}
	case r.failure != nil && r.failure.result == Rejected && !r.failure.racy && sameFile(id, r.failure.id):
		return outcome{result: Rejected, id: r.failure.id, err: r.failure.err}
	}
	set, read, err := readFile(ctx, r.Path, r.Type, r.MaxEntries)
	unsure := racy(read.ModTime, begin)
	var invalid *sets.Error
	switch {
	case errors.As(err, &invalid):
		return outcome{result: Rejected, id: read, racy: unsure, err: err}
	case err != nil:
		return outcome{result: Failed, id: id, err: err}
	case read.Sum == r.current.Sum:
		// The content of the copy in use, in another file, as when a
		// Kubernetes ConfigMap changes another of its files, or in the same
		// racy version.
		return outcome{result: Unchanged, id: read, racy: unsure}
	}
	return outcome{result: Updated, set: set, id: read, racy: unsure}
}

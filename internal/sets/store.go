package sets

import (
	"errors"
	"sync"
	"sync/atomic"
)

// OnUnavailable is what contains answers when a set is unavailable.
type OnUnavailable string

// The answers of contains on an unavailable set.
const (
	// Fail makes the condition indeterminate, with the cause
	// set_unavailable.
	Fail OnUnavailable = "fail"
	// Match answers true.
	Match OnUnavailable = "match"
	// NoMatch answers false.
	NoMatch OnUnavailable = "no_match"
)

// Source is the kind of source of a set.
type Source string

// The kinds of source of a set.
const (
	// SourceValues is a list written in the configuration.
	SourceValues Source = "values"
	// SourceFile is a file, checked again every refresh.
	SourceFile Source = "file"
)

// Refreshes reports whether a set of this source is checked again while
// tessera runs: every source but values.
func (s Source) Refreshes() bool { return s != SourceValues }

// RefreshResult is the outcome of a check of the source of a set.
type RefreshResult string

// The results of a check.
const (
	// RefreshUpdated: a new copy replaced the one in use.
	RefreshUpdated RefreshResult = "updated"
	// RefreshUnchanged: the source has not changed, or holds the content of
	// the copy in use.
	RefreshUnchanged RefreshResult = "unchanged"
	// RefreshRejected: the content is invalid, or exceeds a ceiling; the
	// copy in use stays.
	RefreshRejected RefreshResult = "rejected"
	// RefreshError: the source is missing or cannot be read; the copy in
	// use stays.
	RefreshError RefreshResult = "error"
)

// RefreshResults are the results of a check, in the order of the metrics
// reference.
var RefreshResults = []RefreshResult{RefreshUpdated, RefreshUnchanged, RefreshRejected, RefreshError}

// Declaration is a set as the configuration declares it: what does not
// change while tessera runs.
type Declaration struct {
	Name          string
	Type          Type
	Source        Source
	OnUnavailable OnUnavailable
}

// Entry is the state of one set in a Snapshot.
type Entry struct {
	// Set is the copy in use, or nil when the set was never loaded.
	Set *Set
	// Available is false when the set was never loaded, or when its copy is
	// stale.
	Available bool
}

// Snapshot is the state of every set of a Store at one moment, in the order
// of its declarations. It never changes: a decision takes one, and reads
// every set from it, so that a replacement during the decision changes
// nothing for it.
type Snapshot struct {
	entries []Entry
}

// Entry returns the state of the set of index i, in the order of the
// declarations. A nil Snapshot, or an index out of range, gives a set never
// loaded.
func (sn *Snapshot) Entry(i int) Entry {
	if sn == nil || i < 0 || i >= len(sn.entries) {
		return Entry{}
	}
	return sn.entries[i]
}

// Store holds the state of every declared set. A decision reads it with one
// atomic load, Snapshot; Replace and SetAvailable publish a new Snapshot,
// one at a time. It is safe for concurrent use.
type Store struct {
	decls   []Declaration
	index   map[string]int
	mu      sync.Mutex // serializes the writers
	current atomic.Pointer[Snapshot]
}

// The errors of the Store.
var (
	errDuplicate     = errors.New("sets: a set is declared twice")
	errUnknownSet    = errors.New("sets: no set has this name")
	errTypeMismatch  = errors.New("sets: the copy has another type than the set")
	errNeverLoaded   = errors.New("sets: the set was never loaded")
	errUnknownAnswer = errors.New("sets: unknown answer for an unavailable set")
)

// NewStore returns a Store of the sets of decls, every one never loaded, so
// unavailable until Replace gives it a copy.
func NewStore(decls []Declaration) (*Store, error) {
	s := &Store{decls: append([]Declaration(nil), decls...), index: make(map[string]int, len(decls))}
	for i, d := range decls {
		if _, ok := s.index[d.Name]; ok {
			return nil, errDuplicate
		}
		if _, err := newBuilder(d.Type, 0); err != nil {
			return nil, err
		}
		switch d.OnUnavailable {
		case Fail, Match, NoMatch:
		default:
			return nil, errUnknownAnswer
		}
		s.index[d.Name] = i
	}
	s.current.Store(&Snapshot{entries: make([]Entry, len(decls))})
	return s, nil
}

// Declarations returns the declarations of the sets, in the order of the
// entries of a Snapshot. A nil Store has none.
func (s *Store) Declarations() []Declaration {
	if s == nil {
		return nil
	}
	return append([]Declaration(nil), s.decls...)
}

// Snapshot returns the current state of every set. A nil Store gives a nil
// Snapshot, in which every set is never loaded.
func (s *Store) Snapshot() *Snapshot {
	if s == nil {
		return nil
	}
	return s.current.Load()
}

// Get returns the current state of the set name, and whether it is declared.
func (s *Store) Get(name string) (Entry, bool) {
	i, ok := s.index[name]
	if !ok {
		return Entry{}, false
	}
	return s.Snapshot().Entry(i), true
}

// Replace makes set the copy in use of the set name, and marks it
// available. The decisions in progress keep the copy they had.
func (s *Store) Replace(name string, set *Set) error {
	i, ok := s.index[name]
	switch {
	case !ok:
		return errUnknownSet
	case set == nil || set.typ != s.decls[i].Type:
		return errTypeMismatch
	}
	s.update(i, Entry{Set: set, Available: true})
	return nil
}

// SetAvailable marks the set name available or not, with the copy it has. A
// set never loaded cannot be marked available.
func (s *Store) SetAvailable(name string, available bool) error {
	i, ok := s.index[name]
	if !ok {
		return errUnknownSet
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.current.Load().entries[i]
	if available && e.Set == nil {
		return errNeverLoaded
	}
	e.Available = available
	s.publish(i, e)
	return nil
}

func (s *Store) update(i int, e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publish(i, e)
}

// publish stores a new Snapshot, with the entry i replaced. s.mu is held.
func (s *Store) publish(i int, e Entry) {
	entries := append([]Entry(nil), s.current.Load().entries...)
	entries[i] = e
	s.current.Store(&Snapshot{entries: entries})
}

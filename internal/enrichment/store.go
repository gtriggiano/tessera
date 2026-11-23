package enrichment

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
)

// Snapshot is the copy in use of each configured database at one moment. It
// never changes: a decision takes one when it starts, and looks every
// address up in it, so that a replacement during the decision changes
// nothing for it.
type Snapshot struct {
	geo, asn *Database
}

// Database returns the copy of the database of kind k, or nil when the
// database is not configured or not loaded. A nil Snapshot has no database.
func (sn *Snapshot) Database(k Kind) *Database {
	if sn == nil {
		return nil
	}
	switch k {
	case Geo:
		return sn.geo
	case ASN:
		return sn.asn
	}
	return nil
}

// Store holds the copy in use of each configured database. A decision reads
// it with one atomic load, Snapshot; Replace publishes a new Snapshot. It is
// safe for concurrent use.
type Store struct {
	kinds   []Kind
	mu      sync.Mutex // serializes the writers
	current atomic.Pointer[Snapshot]
}

// The errors of the Store.
var (
	errDuplicate     = errors.New("enrichment: a database is configured twice")
	errNotConfigured = errors.New("enrichment: no database of this kind is configured")
	errKindMismatch  = errors.New("enrichment: the copy has another kind than the database")
)

// NewStore returns a Store of the databases of kinds, none loaded yet.
func NewStore(kinds []Kind) (*Store, error) {
	s := &Store{}
	for _, k := range kinds {
		switch {
		case k != Geo && k != ASN:
			return nil, errUnknownKind
		case slices.Contains(s.kinds, k):
			return nil, errDuplicate
		}
		s.kinds = append(s.kinds, k)
	}
	s.current.Store(&Snapshot{})
	return s, nil
}

// Kinds returns the kinds of the configured databases, in the order of
// NewStore. A nil Store has none.
func (s *Store) Kinds() []Kind {
	if s == nil {
		return nil
	}
	return slices.Clone(s.kinds)
}

// Snapshot returns the current copy of every database. A nil Store gives a
// nil Snapshot, which has no database.
func (s *Store) Snapshot() *Snapshot {
	if s == nil {
		return nil
	}
	return s.current.Load()
}

// Replace makes db the copy in use of the database name, a kind of a
// configured database. The decisions in progress keep the copy they had.
func (s *Store) Replace(name string, db *Database) error {
	k := Kind(name)
	switch {
	case !slices.Contains(s.kinds, k):
		return errNotConfigured
	case db == nil || db.kind != k:
		return errKindMismatch
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := *s.current.Load()
	if k == Geo {
		next.geo = db
	} else {
		next.asn = db
	}
	s.current.Store(&next)
	return nil
}

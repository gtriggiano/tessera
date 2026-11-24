package useragent

import (
	"hash/maphash"
	"sync"
	"sync/atomic"
)

// The shards of the cache.
const (
	// maxShards is the number of shards of a cache with at least
	// maxShards*minShardEntries entries: a power of two.
	maxShards = 16
	// minShardEntries is the fewest entries of a shard: a smaller cache has
	// fewer shards, down to one, so that the entries of each shard stay
	// enough to even out the headers that it gets.
	minShardEntries = 64
	// maxHandSteps bounds the entries that the hand passes in one eviction,
	// so that an insertion holds the lock of its shard for a bounded time
	// whatever the size of the shard.
	maxHandSteps = 128
)

// cache maps the first bytes of a header to its record, with at most capacity
// entries in all. It is split into shards, chosen by a hash of the key with a
// seed of the process, each with its own lock and its own share of the
// entries: the shares add up to capacity, so that the entries never exceed it.
//
// Each shard evicts with SIEVE: a lookup that finds an entry marks it
// visited, under the read lock of the shard; an insertion into a full shard
// moves a hand from the oldest entry towards the newest, clears the mark of
// the entries it passes, and evicts the first entry without one, or the one
// where it stops after maxHandSteps. A lookup allocates nothing.
//
// No decision waits to insert: an insertion takes the write lock of its
// shard only when it is free at once, and otherwise keeps nothing, since the
// decision already has its record. With every header new and many decisions
// at once, waiting for the lock made decisions queue behind each other's
// insertions. A lookup still takes the read lock, and can wait for an
// insertion in progress, whose work maxHandSteps bounds: making it a miss
// instead would cost an analysis, far more than that wait.
type cache struct {
	seed     maphash.Seed
	mask     uint64
	capacity int
	shards   []*shard
}

// shard is one shard of a cache. Its list holds its entries from the newest,
// head, to the oldest, tail.
type shard struct {
	mu               sync.RWMutex
	capacity         int
	entries          map[string]*entry
	head, tail, hand *entry
	hits, misses     atomic.Uint64
	// The shards are separate allocations; the padding keeps the locks of
	// two shards off the same cache line.
	_ [64]byte
}

// entry is an entry of a shard. visited changes under the read lock of the
// shard; everything else under its write lock.
type entry struct {
	key        string
	record     Record
	visited    atomic.Bool
	prev, next *entry // prev is newer, next older
}

// newCache returns a cache of at most capacity entries, at least 1.
func newCache(capacity int) *cache {
	n := maxShards
	for n > 1 && capacity/n < minShardEntries {
		n /= 2
	}
	c := &cache{seed: maphash.MakeSeed(), mask: uint64(n - 1), capacity: capacity, shards: make([]*shard, n)}
	for i := range c.shards {
		share := capacity / n
		if i < capacity%n {
			share++
		}
		c.shards[i] = &shard{capacity: share, entries: map[string]*entry{}}
	}
	return c
}

func (c *cache) shard(key string) *shard {
	return c.shards[maphash.String(c.seed, key)&c.mask]
}

// get returns the record of key, and counts the lookup.
func (c *cache) get(key string) (Record, bool) {
	s := c.shard(key)
	s.mu.RLock()
	e, ok := s.entries[key]
	if !ok {
		s.mu.RUnlock()
		s.misses.Add(1)
		return Record{}, false
	}
	// Only the first lookup since the hand passed writes the mark.
	if !e.visited.Load() {
		e.visited.Store(true)
	}
	r := e.record
	s.mu.RUnlock()
	s.hits.Add(1)
	return r, true
}

// add inserts the record of key, evicting an entry of its shard when the
// shard is full. A key already there, which another lookup added meanwhile,
// keeps its entry: its record is the same. When another goroutine holds the
// lock of the shard, a lookup or an insertion, add inserts nothing and
// returns at once.
func (c *cache) add(key string, r Record) {
	s := c.shard(key)
	if !s.mu.TryLock() {
		return
	}
	defer s.mu.Unlock()
	if _, ok := s.entries[key]; ok {
		return
	}
	if len(s.entries) >= s.capacity {
		s.evict()
	}
	e := &entry{key: key, record: r, next: s.head}
	if s.head != nil {
		s.head.prev = e
	} else {
		s.tail = e
	}
	s.head = e
	s.entries[key] = e
}

// evict removes one entry of s, which is not empty, with the SIEVE hand: the
// first entry without a mark, or the one where the hand stops after
// maxHandSteps entries with a mark.
func (s *shard) evict() {
	e := s.hand
	if e == nil {
		e = s.tail
	}
	for steps := 0; steps < maxHandSteps && e.visited.Load(); steps++ {
		e.visited.Store(false)
		if e = e.prev; e == nil {
			e = s.tail
		}
	}
	s.hand = e.prev
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		s.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		s.tail = e.prev
	}
	delete(s.entries, e.key)
}

// len returns the number of entries.
func (c *cache) len() int {
	n := 0
	for _, s := range c.shards {
		s.mu.RLock()
		n += len(s.entries)
		s.mu.RUnlock()
	}
	return n
}

// lookups returns the number of lookups that found their key, and of those
// that did not.
func (c *cache) lookups() (hits, misses uint64) {
	for _, s := range c.shards {
		hits += s.hits.Load()
		misses += s.misses.Load()
	}
	return hits, misses
}

package useragent

import (
	"hash/maphash"
	"strconv"
	"sync"
	"testing"
	"time"
)

// distinct is the user-agent header i, different for each i.
func distinct(i int) string { return "agent/" + strconv.Itoa(i) }

// The criterion of the cache: one million different headers never make the
// cache hold more than its entries. Eight goroutines analyze them at once,
// and check the entries as they go; an insertion that finds its shard busy
// keeps nothing, but the cache fills all the same.
func TestCacheMillionHeaders(t *testing.T) {
	const entries, headers, workers = 10_000, 1_000_000, 8
	a := New(entries)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := w; i < headers; i += workers {
				if got := a.Analyze(distinct(i)); got.Device != DeviceUnknown {
					t.Errorf("%s: %+v", distinct(i), got)
					return
				}
				if i%64 == w {
					if n := a.Entries(); n > entries {
						t.Errorf("%d entries after %d headers, more than %d", n, i, entries)
						return
					}
				}
			}
		})
	}
	wg.Wait()
	hits, misses := a.Lookups()
	if n := a.Entries(); n != entries || hits != 0 || misses != headers {
		t.Errorf("%d entries, %d hits, %d misses; want %d, 0, %d", n, hits, misses, entries, headers)
	}
}

// After each new header the entries grow by one or stay, and never exceed the
// limit, for any limit, whatever the number of shards it makes. A cache of
// one shard fills up to the limit.
func TestCacheEntriesNeverExceeded(t *testing.T) {
	for _, entries := range []int{1, 2, 63, 64, 127, 128, 129, 1000, 1023, 1025} {
		a := New(entries)
		if a.CacheEntries() != entries {
			t.Fatalf("CacheEntries %d, want %d", a.CacheEntries(), entries)
		}
		previous := 0
		for i := range 4 * entries {
			a.Analyze(distinct(i))
			n := a.Entries()
			if n > entries || n < previous || n > previous+1 {
				t.Fatalf("limit %d: %d entries after %d headers, %d before", entries, n, i+1, previous)
			}
			if entries < 2*minShardEntries && n != min(i+1, entries) {
				t.Fatalf("limit %d, one shard: %d entries after %d headers", entries, n, i+1)
			}
			previous = n
		}
	}
}

// A header already in the cache is found there, without an analysis and
// without an allocation.
func TestCacheHit(t *testing.T) {
	a := New(100)
	first := a.Analyze(chromeWindows)
	if got := a.Analyze(chromeWindows); got != first {
		t.Fatalf("from the cache: %+v, want %+v", got, first)
	}
	if hits, misses := a.Lookups(); hits != 1 || misses != 1 {
		t.Fatalf("lookups %d, %d", hits, misses)
	}
	if allocs := testing.AllocsPerRun(100, func() { a.Analyze(chromeWindows) }); allocs != 0 {
		t.Errorf("%v allocations for a header in the cache", allocs)
	}
}

// An entry found since the hand last passed it stays, and the hand evicts the
// oldest entry not found meanwhile.
func TestCacheEviction(t *testing.T) {
	const entries = minShardEntries // one shard
	a := New(entries)
	for i := range entries {
		a.Analyze(distinct(i))
	}
	a.Analyze(distinct(0)) // the oldest, found
	a.Analyze(distinct(entries))
	if _, misses := a.Lookups(); misses != entries+1 {
		t.Fatalf("misses %d", misses)
	}
	a.Analyze(distinct(0))
	if hits, _ := a.Lookups(); hits != 2 {
		t.Errorf("the entry that was found was evicted: hits %d", hits)
	}
	a.Analyze(distinct(1))
	if hits, misses := a.Lookups(); hits != 2 || misses != entries+2 {
		t.Errorf("the oldest entry not found stayed: hits %d, misses %d", hits, misses)
	}
}

// A decision never waits to insert: while another holds the lock of the
// shard, the header is analyzed and its record is not kept, and the lookup
// counts as a miss.
func TestCacheBusyShard(t *testing.T) {
	a := New(minShardEntries) // one shard
	s := a.cache.shards[0]
	// A lookup of another decision in progress.
	s.mu.RLock()
	done := make(chan Record, 1)
	go func() { done <- a.Analyze(googlebot) }()
	select {
	case r := <-done:
		if !r.IsBot || r.Bot.Name != "Googlebot" {
			t.Errorf("analysis while the shard is busy: %+v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the analysis waited for the lock of the shard")
	}
	s.mu.RUnlock()
	if hits, misses := a.Lookups(); hits != 0 || misses != 1 || a.Entries() != 0 {
		t.Fatalf("after a busy shard: %d hits, %d misses, %d entries", hits, misses, a.Entries())
	}
	// Free again, the next analysis is kept, and found after.
	a.Analyze(googlebot)
	a.Analyze(googlebot)
	if hits, misses := a.Lookups(); hits != 1 || misses != 2 || a.Entries() != 1 {
		t.Fatalf("after a free shard: %d hits, %d misses, %d entries", hits, misses, a.Entries())
	}

	// An insertion of another decision in progress.
	s.mu.Lock()
	added := make(chan struct{})
	go func() {
		a.cache.add("agent/busy", Record{Device: DeviceUnknown})
		close(added)
	}()
	select {
	case <-added:
	case <-time.After(30 * time.Second):
		t.Fatal("the insertion waited for the lock of the shard")
	}
	s.mu.Unlock()
	if a.Entries() != 1 {
		t.Errorf("%d entries; want the insertion skipped", a.Entries())
	}
}

// An eviction passes at most maxHandSteps entries, whatever the size of the
// shard: when every entry has a mark, the hand clears that many and evicts
// the next.
func TestCacheEvictionBounded(t *testing.T) {
	const entries = 4 * maxHandSteps
	c := &cache{seed: maphash.MakeSeed(), capacity: entries, shards: []*shard{{capacity: entries, entries: map[string]*entry{}}}}
	for i := range entries {
		c.add(distinct(i), Record{})
	}
	for i := range entries {
		if _, ok := c.get(distinct(i)); !ok {
			t.Fatalf("%s is not in the cache", distinct(i))
		}
	}
	c.add(distinct(entries), Record{})
	unmarked := 0
	for _, e := range c.shards[0].entries {
		if !e.visited.Load() {
			unmarked++
		}
	}
	// The marks cleared, and the new entry.
	if n := c.len(); n != entries || unmarked != maxHandSteps+1 {
		t.Errorf("%d entries, %d without a mark; want %d and %d", n, unmarked, entries, maxHandSteps+1)
	}
}

// Without a cache, nothing is looked up or kept.
func TestNoCache(t *testing.T) {
	for _, a := range []*Analyzer{New(0), New(-1), nil} {
		if got := a.Analyze(googlebot); !got.IsBot {
			t.Errorf("%+v", got)
		}
		hits, misses := a.Lookups()
		if a.CacheEntries() != 0 || a.Entries() != 0 || hits != 0 || misses != 0 {
			t.Errorf("an Analyzer without a cache: %d, %d, %d, %d", a.CacheEntries(), a.Entries(), hits, misses)
		}
	}
}

// Concurrent analyses of the same headers and of new ones agree with an
// analysis without a cache, and the entries stay within the limit. Run with
// -race.
func TestCacheConcurrent(t *testing.T) {
	const entries = 200
	a := New(entries)
	headers := []string{chromeWindows, edgeWindows, ie11, safariIPhone, firefoxLinux, googlebot, curl, sqlmap, tizenTV, playStation}
	want := make([]Record, len(headers))
	for i, h := range headers {
		want[i] = New(0).Analyze(h)
	}
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 2000 {
				k := (i + w) % len(headers)
				if got := a.Analyze(headers[k]); got != want[k] {
					t.Errorf("%s: %+v, want %+v", headers[k], got, want[k])
					return
				}
				a.Analyze(distinct(w*2000 + i))
				if n := a.Entries(); n > entries {
					t.Errorf("%d entries", n)
					return
				}
			}
		})
	}
	wg.Wait()
	if hits, misses := a.Lookups(); hits+misses != 8*2000*2 {
		t.Errorf("lookups %d + %d", hits, misses)
	}
}

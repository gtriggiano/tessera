package useragent

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// benchAgent is a kind of user-agent header: a real header of a browser, a
// bot or a program, with three %d for the numbers of its version that the
// benchmarks draw, the first one from minMajor to maxMajor.
type benchAgent struct {
	format             string
	minMajor, maxMajor int
	bot                bool
}

var (
	benchBrowsers = []benchAgent{
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.%d Safari/537.36", 100, 140, false},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.%d Safari/537.36", 100, 140, false},
		{"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.%d Mobile Safari/537.36", 100, 140, false},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/%d.0.%d.%d", 100, 140, false},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:%[1]d.0) Gecko/20100101 Firefox/%[1]d.%[2]d.%[3]d", 100, 140, false},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/%d.%d.%d Mobile/15E148 Safari/604.1", 13, 18, false},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/%d.%d.%d Safari/605.1.15", 13, 18, false},
		{"Mozilla/5.0 (Linux; Android 14; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/%d.%d Chrome/115.0.%d.0 Mobile Safari/537.36", 20, 27, false},
	}
	benchBots = []benchAgent{
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Googlebot/2.1; +http://www.google.com/bot.html) Chrome/%d.0.%d.%d Safari/537.36", 100, 140, true},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm) Chrome/%d.0.%d.%d Safari/537.36", 100, 140, true},
		{"curl/%d.%d.%d", 7, 8, true},
		{"python-requests/%d.%d.%d", 2, 2, true},
	}
)

// benchHeaders returns n distinct user-agent headers, the same ones for the
// same seed: four in five of a browser, one in five of a bot or a program.
// It fails b if a header of a browser is analyzed as a bot, or one of a bot
// is not.
func benchHeaders(b *testing.B, n int, seed uint64) []string {
	b.Helper()
	r := rand.New(rand.NewPCG(2026, seed))
	seen := make(map[string]struct{}, n)
	headers := make([]string, 0, n)
	checked := map[string]bool{}
	for len(headers) < n {
		kinds := benchBrowsers
		if r.IntN(5) == 0 {
			kinds = benchBots
		}
		k := kinds[r.IntN(len(kinds))]
		h := fmt.Sprintf(k.format, k.minMajor+r.IntN(k.maxMajor-k.minMajor+1), r.IntN(10000), r.IntN(1000))
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		headers = append(headers, h)
		if !checked[k.format] {
			checked[k.format] = true
			if got := analyze(h); got.IsBot != k.bot {
				b.Fatalf("%s: %+v", h, got)
			}
		}
	}
	return headers
}

// fill returns a header of size bytes: prefix, then unit repeated, then
// suffix, cut at size.
func fill(prefix, unit, suffix string, size int) string {
	var s strings.Builder
	s.WriteString(prefix)
	for s.Len()+len(suffix) < size {
		s.WriteString(unit)
	}
	s.WriteString(suffix)
	return s.String()[:size]
}

const benchChrome = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// worstHeaders are the headers that a client can write to make the analysis
// slow, as long as the header that Envoy accepts: a browser followed by many
// products, a long comment, the same browser repeated, nested parentheses,
// versions, words that the analysis looks for, letters.
var worstHeaders = []struct {
	name string
	make func(size int) string
}{
	{"chrome+tokens", func(n int) string { return fill(benchChrome, " Foo/1.2.3", "", n) }},
	{"comment", func(n int) string {
		return fill("Mozilla/5.0 (Windows NT 10.0; Win64; x64; ", "A", ") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36", n)
	}},
	{"repeated", func(n int) string { return fill("", benchChrome+" ", "", n) }},
	{"parentheses", func(n int) string { return fill("Mozilla/5.0 ", "(", "", n) }},
	{"versions", func(n int) string { return fill("Chrome/", "1.2.", "", n) }},
	{"keywords", func(n int) string {
		return fill("", "Android iPhone iPad Mobile bot crawler spider Linux Windows Macintosh TV Tablet ", "", n)
	}},
	{"letters", func(n int) string { return fill("", "abcdefghijklmnopqrstuvwxyz", "", n) }},
}

// reportHits reports the share of the lookups since hits and misses that
// found their header in the cache of a, as hit-%.
func reportHits(b *testing.B, a *Analyzer, hits, misses uint64) {
	b.Helper()
	h, m := a.Lookups()
	if total := h - hits + m - misses; total > 0 {
		b.ReportMetric(100*float64(h-hits)/float64(total), "hit-%")
	}
}

// BenchmarkAnalyze times the analysis of one header, from one goroutine:
//
//   - nocache: without a cache, 131,072 different headers of browsers, bots
//     and programs in turn;
//   - cold: the same headers with a cache of 10,000 entries, the default, so
//     that every lookup misses, and each analysis replaces an entry;
//   - warm: 1,024 different headers already in a cache of 10,000 entries, so
//     that every lookup finds its header;
//   - worst: without a cache, the headers that a client can write to make
//     the analysis slow, of MaxBytes bytes, and of 60 KiB, the largest
//     headers that Envoy accepts by default, of which tessera analyzes the
//     first MaxBytes bytes.
func BenchmarkAnalyze(b *testing.B) {
	many, few := benchHeaders(b, 1<<17, 1), benchHeaders(b, 1<<10, 2)
	for _, c := range []struct {
		name    string
		entries int
		headers []string
		warm    bool
	}{
		{"nocache", 0, many, false},
		{"cold", 10_000, many, false},
		{"warm", 10_000, few, true},
	} {
		b.Run(c.name, func(b *testing.B) {
			a := New(c.entries)
			if c.warm {
				for _, h := range c.headers {
					a.Analyze(h)
				}
			}
			hits, misses := a.Lookups()
			mask := len(c.headers) - 1
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				a.Analyze(c.headers[i&mask])
				i++
			}
			reportHits(b, a, hits, misses)
		})
	}
	for _, w := range worstHeaders {
		for _, size := range []int{MaxBytes, 60 << 10} {
			b.Run(fmt.Sprintf("worst/%s/bytes=%d", w.name, size), func(b *testing.B) {
				a, h := New(0), w.make(size)
				b.ReportAllocs()
				for b.Loop() {
					a.Analyze(h)
				}
			})
		}
	}
}

// BenchmarkAnalyzeParallel times the analysis of one header from many
// goroutines at once, as in a decision of each call from Envoy, with a cache
// of 10,000 entries that they share: cold, with 131,072 different headers
// that the goroutines take in turns of 256, in order, so that a header comes
// back only after all the others and every lookup misses; warm, with 1,024
// headers already in the cache. The goroutines are GOMAXPROCS, then 16 times
// as many. ns/op is the time of all the goroutines divided by the number of
// analyses.
func BenchmarkAnalyzeParallel(b *testing.B) {
	many, few := benchHeaders(b, 1<<17, 1), benchHeaders(b, 1<<10, 2)
	for _, c := range []struct {
		name    string
		headers []string
		warm    bool
	}{
		{"cold", many, false},
		{"warm", few, true},
	} {
		for _, p := range []int{1, 16} {
			b.Run(fmt.Sprintf("%s/goroutines=%d", c.name, p*runtime.GOMAXPROCS(0)), func(b *testing.B) {
				a := New(10_000)
				if c.warm {
					for _, h := range c.headers {
						a.Analyze(h)
					}
				}
				hits, misses := a.Lookups()
				mask := uint64(len(c.headers) - 1)
				var next atomic.Uint64
				b.ReportAllocs()
				b.SetParallelism(p)
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					for {
						first := next.Add(256) - 256
						for i := first; i < first+256; i++ {
							if !pb.Next() {
								return
							}
							a.Analyze(c.headers[i&mask])
						}
					}
				})
				b.StopTimer()
				reportHits(b, a, hits, misses)
			})
		}
	}
}

// liveHeap returns the bytes of the heap held by reachable objects, measured
// after a garbage collection, as the benchmarks of internal/sets do.
func liveHeap() uint64 {
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// BenchmarkCacheMemory fills a cache with as many different headers as its
// entries, from one goroutine: 10,000 entries, the default, and 1,000,000,
// the largest number that the configuration accepts, with headers of
// browsers, bots and programs of 130 bytes on average; and 10,000 entries
// with headers of MaxBytes bytes, the longest key. ns/op is the time to fill
// the cache, and ns/header the time of each header. Besides, it reports:
//
//   - entries: the entries of the cache once filled, at most its limit, and
//     fewer when the headers do not spread evenly over its shards;
//   - heap-B/entry and heap-MB: the heap that the cache holds, measured
//     after a garbage collection, without the headers of the requests.
func BenchmarkCacheMemory(b *testing.B) {
	for _, c := range []struct {
		entries int
		long    bool
	}{
		{10_000, false},
		{1_000_000, false},
		{10_000, true},
	} {
		name := fmt.Sprintf("entries=%d", c.entries)
		if c.long {
			name += fmt.Sprintf("/bytes=%d", MaxBytes)
		}
		b.Run(name, func(b *testing.B) {
			headers := benchHeaders(b, c.entries, 3)
			if c.long {
				for i, h := range headers {
					headers[i] = fill(h, " Foo/1.2.3", "", MaxBytes)
				}
			}
			base := liveHeap()
			var a *Analyzer
			for b.Loop() {
				a = New(c.entries)
				for _, h := range headers {
					a.Analyze(h)
				}
			}
			held := liveHeap() - base
			// The headers were in base: they stay until held is measured.
			runtime.KeepAlive(headers)
			n := a.Entries()
			if n > c.entries || n < c.entries*9/10 {
				b.Fatalf("%d entries in a cache of %d", n, c.entries)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*c.entries), "ns/header")
			b.ReportMetric(float64(n), "entries")
			b.ReportMetric(float64(held)/float64(n), "heap-B/entry")
			b.ReportMetric(float64(held)/1e6, "heap-MB")
		})
	}
}

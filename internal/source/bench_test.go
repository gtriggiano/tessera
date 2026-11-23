package source

import (
	"encoding/binary"
	"math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strings"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/mmdbtest"
	"github.com/gtriggiano/tessera/internal/sets"
)

// BenchmarkReadFile reads a file of one million distinct networks, as a
// refresh of a changed file does: open, read through the descriptor, SHA-256
// of the content, parse and build. The networks are the mixed blocklist of
// the benchmarks of internal/sets, generated, always the same ones, in ranges
// that are not real address space: 60% IPv4 addresses, 24.9% IPv4 /24
// networks, 0.1% IPv4 networks from /16 to /23, all in 240.0.0.0/4 (reserved
// for future use), 12% IPv6 /64 networks and 3% IPv6 /48 networks, in
// 2001:db8::/32.
func BenchmarkReadFile(b *testing.B) {
	const n = 1_000_000
	r := rand.New(rand.NewPCG(2026, 11))
	v4 := func(bits int) netip.Prefix {
		var a [4]byte
		binary.BigEndian.PutUint32(a[:], 0xf0000000|r.Uint32N(1<<28))
		return netip.PrefixFrom(netip.AddrFrom4(a), bits).Masked()
	}
	v6 := func(bits int) netip.Prefix {
		a := [16]byte{0x20, 0x01, 0x0d, 0xb8}
		binary.BigEndian.PutUint32(a[4:], r.Uint32())
		binary.BigEndian.PutUint64(a[8:], r.Uint64())
		return netip.PrefixFrom(netip.AddrFrom16(a), bits).Masked()
	}
	seen := make(map[netip.Prefix]struct{}, n)
	var content strings.Builder
	for len(seen) < n {
		var p netip.Prefix
		switch x := r.IntN(1000); {
		case x < 600:
			p = v4(32)
		case x < 849:
			p = v4(24)
		case x < 850:
			p = v4(16 + r.IntN(8))
		case x < 970:
			p = v6(64)
		default:
			p = v6(48)
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		if p.IsSingleIP() {
			content.WriteString(p.Addr().String())
		} else {
			content.WriteString(p.String())
		}
		content.WriteByte('\n')
	}
	path := filepath.Join(b.TempDir(), "blocked.txt")
	if err := os.WriteFile(path, []byte(content.String()), 0o644); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(content.Len()))
	for b.Loop() {
		if s, _, err := ReadFile(path, sets.CIDR, n); err != nil || s.Entries() != n {
			b.Fatalf("ReadFile: %v", err)
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

// heapObjects returns the bytes of the heap held by objects, after a garbage
// collection, as peakHeap reads them.
func heapObjects() uint64 {
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// peakHeap runs f, and returns the largest heap held by objects while it
// ran, unreachable ones not yet freed included, read every 100 µs.
func peakHeap(f func()) uint64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	stop, peak := make(chan struct{}), make(chan uint64)
	go func() {
		ticker := time.NewTicker(100 * time.Microsecond)
		defer ticker.Stop()
		var largest uint64
		for {
			metrics.Read(sample)
			largest = max(largest, sample[0].Value.Uint64())
			select {
			case <-stop:
				peak <- largest
				return
			case <-ticker.C:
			}
		}
	}()
	f()
	close(stop)
	return <-peak
}

// BenchmarkReadDatabase reads a geo database as large as GeoLite2 City and
// an asn database as large as GeoLite2 ASN, generated in 240.0.0.0/4 and
// 2001:db8::/32, from a file, as a refresh of a changed file does: open,
// read through the descriptor into one buffer, SHA-256 of the content, the
// checks of the metadata and the decoding of the first records. Besides the
// time and the allocations of one read, it reports:
//
//   - file-MB: the size of the file;
//   - heap-MB: the heap that one copy holds, measured after a garbage
//     collection;
//   - peak-MB: the largest heap during the reads, above the heap before the
//     first copy; each read keeps the previous copy until the new one is
//     ready, as a refresh does, so two copies coexist.
func BenchmarkReadDatabase(b *testing.B) {
	for _, c := range []struct {
		kind  enrichment.Kind
		large func() ([]byte, []netip.Prefix, error)
	}{
		{enrichment.Geo, mmdbtest.LargeCityDatabase},
		{enrichment.ASN, mmdbtest.LargeASNDatabase},
	} {
		b.Run(string(c.kind), func(b *testing.B) {
			content, _, err := c.large()
			if err != nil {
				b.Fatal(err)
			}
			size := len(content)
			path := filepath.Join(b.TempDir(), string(c.kind)+".mmdb")
			if err := os.WriteFile(path, content, 0o644); err != nil {
				b.Fatal(err)
			}
			content = nil
			read := func() *enrichment.Database {
				db, _, err := ReadDatabase(path, c.kind)
				if err != nil {
					b.Fatalf("ReadDatabase: %v", err)
				}
				return db
			}
			base, before := liveHeap(), heapObjects()
			kept := read()
			held := liveHeap() - base
			b.ReportAllocs()
			b.SetBytes(int64(size))
			peak := peakHeap(func() {
				for b.Loop() {
					kept = read()
				}
			})
			runtime.KeepAlive(kept)
			b.ReportMetric(float64(size)/1e6, "file-MB")
			b.ReportMetric(float64(held)/1e6, "heap-MB")
			b.ReportMetric(float64(peak-before)/1e6, "peak-MB")
		})
	}
}

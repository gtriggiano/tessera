package sets

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The benchmarks generate their values, always the same ones, in ranges that
// are not real address space: IPv4 networks in 240.0.0.0/4, reserved for
// future use (2^20 /24 networks, 2^28 addresses), and IPv6 networks in
// 2001:db8::/32, the documentation range.

// shape is a kind of list: how its values are drawn.
type shape struct {
	name string
	typ  Type
	draw func(r *rand.Rand) string
}

var (
	ipv4Addrs = shape{"ipv4-32", CIDR, func(r *rand.Rand) string { return cidrValue(randomV4(r, 32)) }}
	ipv4Nets  = shape{"ipv4-24", CIDR, func(r *rand.Rand) string { return cidrValue(randomV4(r, 24)) }}
	ipv6Nets  = shape{"ipv6-64", CIDR, func(r *rand.Rand) string { return cidrValue(randomV6(r, 64)) }}
	mixed     = shape{"mix", CIDR, drawMixed}
	numbers   = shape{"asn", ASN, func(r *rand.Rand) string { return strconv.FormatUint(uint64(r.Uint32N(MaxASN))+1, 10) }}
	hosts     = shape{"string", String, drawHost}
)

// drawMixed draws a value of a blocklist of both families: 60% IPv4
// addresses, 24.9% IPv4 /24 networks, 0.1% IPv4 networks from /16 to /23,
// 12% IPv6 /64 networks and 3% IPv6 /48 networks.
func drawMixed(r *rand.Rand) string {
	switch x := r.IntN(1000); {
	case x < 600:
		return cidrValue(randomV4(r, 32))
	case x < 849:
		return cidrValue(randomV4(r, 24))
	case x < 850:
		return cidrValue(randomV4(r, 16+r.IntN(8)))
	case x < 970:
		return cidrValue(randomV6(r, 64))
	default:
		return cidrValue(randomV6(r, 48))
	}
}

// drawHost draws a host name of 18 to 32 characters under example.com.
func drawHost(r *rand.Rand) string {
	label := make([]byte, 6+r.IntN(15))
	for i := range label {
		label[i] = 'a' + byte(r.IntN(26))
	}
	return string(label) + ".example.com"
}

// randomV4 returns a random network of bits in 240.0.0.0/4.
func randomV4(r *rand.Rand, bits int) netip.Prefix {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], 0xf0000000|r.Uint32N(1<<28))
	return netip.PrefixFrom(netip.AddrFrom4(b), bits).Masked()
}

// randomV6 returns a random network of bits in 2001:db8::/32.
func randomV6(r *rand.Rand, bits int) netip.Prefix {
	b := [16]byte{0x20, 0x01, 0x0d, 0xb8}
	binary.BigEndian.PutUint32(b[4:], r.Uint32())
	binary.BigEndian.PutUint64(b[8:], r.Uint64())
	return netip.PrefixFrom(netip.AddrFrom16(b), bits).Masked()
}

// randomIn returns a random address of p.
func randomIn(r *rand.Rand, p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		if r.IntN(2) == 1 {
			b[i/8] |= 0x80 >> (i % 8)
		}
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// cidrValue writes p as a list writes it: an address without its length.
func cidrValue(p netip.Prefix) string {
	if p.IsSingleIP() {
		return p.Addr().String()
	}
	return p.String()
}

// generate returns n distinct values of s, the same ones at every run.
func generate(s shape, n int) []string {
	r := rand.New(rand.NewPCG(2026, 11))
	seen := make(map[string]struct{}, n)
	values := make([]string, 0, n)
	for len(values) < n {
		if v := s.draw(r); !has(seen, v) {
			seen[v] = struct{}{}
			values = append(values, v)
		}
	}
	return values
}

func has(m map[string]struct{}, v string) bool { _, ok := m[v]; return ok }

// liveHeap returns the bytes of the heap held by reachable objects, measured
// after a garbage collection.
func liveHeap() uint64 {
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
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

// heapObjects returns the bytes of the heap held by objects, after a garbage
// collection, as peakHeap reads them.
func heapObjects() uint64 {
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// BenchmarkContainsAddr looks up addresses in a cidr set of the mixed shape,
// of 100,000 and of 1,000,000 networks: addresses in a network of the set
// (match), and addresses of the same ranges in none (miss), IPv4 and IPv6.
// Each case cycles through 65,536 different addresses.
func BenchmarkContainsAddr(b *testing.B) {
	for _, n := range []int{100_000, 1_000_000} {
		values := generate(mixed, n)
		s, err := FromValues(CIDR, values, n)
		if err != nil {
			b.Fatal(err)
		}
		r := rand.New(rand.NewPCG(2026, 22))
		var nets4, nets6 []netip.Prefix
		for _, v := range values {
			p, err := parseCIDR(v)
			if err != nil {
				b.Fatal(err)
			}
			if p.Addr().Is4() {
				nets4 = append(nets4, p)
			} else {
				nets6 = append(nets6, p)
			}
		}
		const probes = 1 << 16
		for _, c := range []struct {
			name  string
			match bool
			draw  func() netip.Addr
		}{
			{"ipv4-match", true, func() netip.Addr { return randomIn(r, nets4[r.IntN(len(nets4))]) }},
			{"ipv4-miss", false, func() netip.Addr { return randomV4(r, 32).Addr() }},
			{"ipv6-match", true, func() netip.Addr { return randomIn(r, nets6[r.IntN(len(nets6))]) }},
			{"ipv6-miss", false, func() netip.Addr { return randomV6(r, 128).Addr() }},
		} {
			addrs := make([]netip.Addr, 0, probes)
			for len(addrs) < probes {
				if a := c.draw(); s.ContainsAddr(a) == c.match {
					addrs = append(addrs, a)
				}
			}
			b.Run(fmt.Sprintf("networks=%d/%s", n, c.name), func(b *testing.B) {
				found, i := 0, 0
				for b.Loop() {
					if s.ContainsAddr(addrs[i&(probes-1)]) {
						found++
					}
					i++
				}
				if c.match && found != i || !c.match && found != 0 {
					b.Fatalf("%d of %d addresses found", found, i)
				}
			})
		}
	}
}

// BenchmarkRead reads text content of each shape, parse and build, from
// memory. Besides the time and the allocations of one read, it reports:
//
//   - heap-B/entry and heap-MB: the heap that one copy holds, measured
//     after a garbage collection;
//   - peak-MB: the largest heap during the reads, above the heap before the
//     first copy; each read keeps the previous copy until the new one is
//     built, as a refresh does, so two copies coexist.
func BenchmarkRead(b *testing.B) {
	for _, c := range []struct {
		shape shape
		n     int
	}{
		{ipv4Addrs, 1_000_000},
		{ipv4Nets, 1_000_000},
		{ipv6Nets, 1_000_000},
		{mixed, 100_000},
		{mixed, 1_000_000},
		{numbers, 1_000_000},
		{hosts, 1_000_000},
	} {
		b.Run(fmt.Sprintf("%s/entries=%d", c.shape.name, c.n), func(b *testing.B) {
			content := []byte(strings.Join(generate(c.shape, c.n), "\n") + "\n")
			read := func() *Set {
				s, err := Read(c.shape.typ, bytes.NewReader(content), c.n)
				if err != nil || s.Entries() != c.n {
					b.Fatalf("Read: %v", err)
				}
				return s
			}
			base, before := liveHeap(), heapObjects()
			kept := read()
			held := liveHeap() - base
			b.ReportAllocs()
			b.SetBytes(int64(len(content)))
			peak := peakHeap(func() {
				for b.Loop() {
					kept = read()
				}
			})
			runtime.KeepAlive(kept)
			b.ReportMetric(float64(held)/float64(c.n), "heap-B/entry")
			b.ReportMetric(float64(held)/1e6, "heap-MB")
			b.ReportMetric(float64(peak-before)/1e6, "peak-MB")
		})
	}
}

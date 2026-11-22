package source

import (
	"encoding/binary"
	"math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

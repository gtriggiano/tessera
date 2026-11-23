package enrichment

import (
	"encoding/binary"
	"math/rand/v2"
	"net/netip"
	"testing"

	"github.com/gtriggiano/tessera/internal/mmdbtest"
)

// BenchmarkLookup looks addresses up in a geo database as large as
// GeoLite2 City and in an asn database as large as GeoLite2 ASN, generated
// in 240.0.0.0/4 and 2001:db8::/32, and decodes their records as geo and
// asn do: IPv4 and IPv6 addresses that the database has (found), and IPv4
// addresses that it does not have (miss). Each case cycles through 65,536
// different addresses, spread over the whole database.
func BenchmarkLookup(b *testing.B) {
	for _, c := range []struct {
		kind  Kind
		large func() ([]byte, []netip.Prefix, error)
	}{
		{Geo, mmdbtest.LargeCityDatabase},
		{ASN, mmdbtest.LargeASNDatabase},
	} {
		content, sample, err := c.large()
		if err != nil {
			b.Fatal(err)
		}
		db, err := Parse(c.kind, content)
		if err != nil {
			b.Fatal(err)
		}
		lookup := func(a netip.Addr) bool {
			if c.kind == Geo {
				r, err := db.Geo(a)
				if err != nil {
					b.Fatal(err)
				}
				return r.Found
			}
			r, err := db.ASN(a)
			if err != nil {
				b.Fatal(err)
			}
			return r.Found
		}
		var sample4, sample6 []netip.Prefix
		for _, p := range sample {
			if p.Addr().Is4() {
				sample4 = append(sample4, p)
			} else {
				sample6 = append(sample6, p)
			}
		}
		const probes = 1 << 16
		r := rand.New(rand.NewPCG(2026, 22))
		var misses []netip.Addr
		for len(misses) < probes {
			var a [4]byte
			binary.BigEndian.PutUint32(a[:], 0xf0000000|r.Uint32N(1<<28))
			if addr := netip.AddrFrom4(a); !lookup(addr) {
				misses = append(misses, addr)
			}
		}
		for _, k := range []struct {
			name  string
			found bool
			addrs []netip.Addr
		}{
			{"ipv4-found", true, mmdbtest.Addresses(sample4, probes, 1)},
			{"ipv6-found", true, mmdbtest.Addresses(sample6, probes, 2)},
			{"ipv4-miss", false, misses},
		} {
			b.Run(string(c.kind)+"/"+k.name, func(b *testing.B) {
				b.ReportAllocs()
				found, i := 0, 0
				for b.Loop() {
					if lookup(k.addrs[i&(probes-1)]) {
						found++
					}
					i++
				}
				if k.found && found != i || !k.found && found != 0 {
					b.Fatalf("%d of %d addresses found", found, i)
				}
			})
		}
	}
}

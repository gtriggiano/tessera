package mmdbtest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
)

// The large databases of the benchmarks are as large as GeoLite2 City and
// GeoLite2 ASN, 63.7 MB and 11.4 MB, with 2.2 million and 765,000 networks:
// always the same ones, generated in ranges that are not real address space,
// IPv4 networks in 240.0.0.0/4, reserved for future use, and IPv6 networks
// in 2001:db8::/32, the documentation range. Besides them, they hold the
// networks of the example databases.

// largeCountry is a country of the records of the large City database.
type largeCountry struct {
	continent, code, timeZone string
	inEU                      bool
}

var largeCountries = []largeCountry{
	{"EU", "IT", "Europe/Rome", true}, {"EU", "DE", "Europe/Berlin", true}, {"EU", "FR", "Europe/Paris", true},
	{"EU", "ES", "Europe/Madrid", true}, {"EU", "NL", "Europe/Amsterdam", true}, {"EU", "PL", "Europe/Warsaw", true},
	{"EU", "SE", "Europe/Stockholm", true}, {"EU", "GB", "Europe/London", false}, {"EU", "CH", "Europe/Zurich", false},
	{"EU", "NO", "Europe/Oslo", false}, {"NA", "US", "America/New_York", false}, {"NA", "US", "America/Chicago", false},
	{"NA", "US", "America/Los_Angeles", false}, {"NA", "CA", "America/Toronto", false}, {"NA", "MX", "America/Mexico_City", false},
	{"SA", "BR", "America/Sao_Paulo", false}, {"SA", "AR", "America/Argentina/Buenos_Aires", false}, {"SA", "CL", "America/Santiago", false},
	{"AS", "JP", "Asia/Tokyo", false}, {"AS", "KR", "Asia/Seoul", false}, {"AS", "IN", "Asia/Kolkata", false},
	{"AS", "SG", "Asia/Singapore", false}, {"AS", "ID", "Asia/Jakarta", false}, {"AS", "VN", "Asia/Ho_Chi_Minh", false},
	{"AF", "ZA", "Africa/Johannesburg", false}, {"AF", "NG", "Africa/Lagos", false}, {"AF", "KE", "Africa/Nairobi", false},
	{"OC", "AU", "Australia/Sydney", false}, {"OC", "NZ", "Pacific/Auckland", false},
}

// largeLanguages are the languages of the names of a large City database,
// those of GeoIP2.
var largeLanguages = []string{"de", "en", "es", "fr", "ja", "pt-BR", "ru", "zh-CN"}

// largeNames returns the names of a place in every language of
// largeLanguages: the same name in the Latin alphabet, as for most places,
// and other names in three of them.
func largeNames(name string) map[string]any {
	names := map[string]any{}
	for _, l := range largeLanguages {
		names[l] = name
	}
	names["ja"], names["ru"], names["zh-CN"] = name+" (ja)", name+" (ru)", name+" (zh)"
	return names
}

// largeCity returns the record i of a large City database, with the
// fields of GeoIP2 City.
func largeCity(r *rand.Rand, i int) map[string]any {
	c := largeCountries[r.IntN(len(largeCountries))]
	country := map[string]any{"geoname_id": uint32(1000 + len(c.code)), "iso_code": c.code, "names": largeNames("Country " + c.code)}
	if c.inEU {
		country["is_in_european_union"] = true
	}
	subdivision := fmt.Sprintf("%02d", r.IntN(100))
	return map[string]any{
		"city":               map[string]any{"geoname_id": uint32(2_000_000 + i), "names": largeNames(fmt.Sprintf("City %d", i))},
		"continent":          map[string]any{"code": c.continent, "geoname_id": uint32(6_255_000 + len(c.continent)), "names": largeNames("Continent " + c.continent)},
		"country":            country,
		"registered_country": country,
		"location": map[string]any{
			"accuracy_radius": uint16(5 + r.IntN(500)),
			"latitude":        float64(r.IntN(18000)-9000) / 100,
			"longitude":       float64(r.IntN(36000)-18000) / 100,
			"time_zone":       c.timeZone,
		},
		"postal":       map[string]any{"code": fmt.Sprintf("%05d", r.IntN(100000))},
		"subdivisions": []any{map[string]any{"geoname_id": uint32(3_000_000 + r.IntN(5000)), "iso_code": subdivision, "names": largeNames("Subdivision " + c.code + "-" + subdivision)}},
	}
}

// largeShape is what a large database holds: its type; its records, drawn
// by record; the number of networks of each family, and the length of each,
// drawn by ipv4Bits and ipv6Bits; the percentage of networks followed by
// addresses that the database does not have; and the networks of the
// examples. One network in sampleEvery is in the sample.
type largeShape struct {
	ipv4, ipv6         int
	ipv4Bits, ipv6Bits func(r *rand.Rand) int
	gapPercent         int
	sampleEvery        int
	records            int
	record             func(r *rand.Rand, i int) map[string]any
	dbType             string
	examples           func(w *Writer) error
}

// drawBits returns a function that draws a length from weights, the weight
// of each length from first.
func drawBits(first int, weights ...int) func(r *rand.Rand) int {
	total := 0
	for _, w := range weights {
		total += w
	}
	return func(r *rand.Rand) int {
		x := r.IntN(total)
		for i, w := range weights {
			if x < w {
				return first + i
			}
			x -= w
		}
		return first + len(weights) - 1
	}
}

// LargeCityDatabase returns the content of a database with the schema of
// GeoIP2 City as large as GeoLite2 City, and a sample of its generated
// networks, of both families, in the order of the addresses.
func LargeCityDatabase() ([]byte, []netip.Prefix, error) {
	return large(largeShape{
		dbType: "GeoIP2-City",
		// 1.75 million IPv4 networks from /24 to /32, and 450,000 IPv6
		// networks from /48 to /64, with 80,000 records: a tree of 4.5
		// million nodes of 28 bits, and 32.5 MB of data.
		ipv4:     1_750_000,
		ipv4Bits: drawBits(24, 10, 5, 10, 15, 20, 15, 10, 5, 10),
		ipv6:     450_000,
		ipv6Bits: drawBits(48, 1, 0, 0, 0, 0, 0, 0, 0, 9, 0, 0, 0, 20, 0, 0, 0, 70),
		records:  80_000,
		record:   largeCity,
		examples: func(w *Writer) error {
			for _, c := range ExampleCities {
				if err := w.Insert(c.Network, c.City.Record()); err != nil {
					return err
				}
			}
			return nil
		},
		gapPercent:  5,
		sampleEvery: 64,
	})
}

// LargeASNDatabase returns the content of a database with the schema of
// GeoLite2 ASN as large as GeoLite2 ASN, and a sample of its generated
// networks, of both families, in the order of the addresses. The generated
// networks have the AS numbers 64496 to 64509.
func LargeASNDatabase() ([]byte, []netip.Prefix, error) {
	return large(largeShape{
		dbType: "GeoLite2-ASN",
		// 630,000 IPv4 networks from /23 to /29, and 135,000 IPv6 networks
		// from /48 to /64, with 60,000 records: a tree of 1.6 million
		// nodes of 24 bits, and 1.8 MB of data.
		ipv4:     630_000,
		ipv4Bits: drawBits(23, 5, 15, 15, 20, 20, 15, 10),
		ipv6:     135_000,
		ipv6Bits: drawBits(48, 10, 0, 0, 0, 20, 0, 0, 0, 40, 0, 0, 0, 0, 0, 0, 0, 30),
		records:  60_000,
		record: func(r *rand.Rand, i int) map[string]any {
			return ASN{uint32(64496 + i%14), fmt.Sprintf("Example Network %d", i)}.Record()
		},
		examples: func(w *Writer) error {
			for _, a := range ExampleASNs {
				if err := w.Insert(a.Network, a.ASN.Record()); err != nil {
					return err
				}
			}
			return nil
		},
		gapPercent:  5,
		sampleEvery: 16,
	})
}

// large writes the database of shape s.
func large(s largeShape) ([]byte, []netip.Prefix, error) {
	r := rand.New(rand.NewPCG(2026, 11))
	w := New(s.dbType, BuildEpoch)
	w.Languages = largeLanguages
	refs := make([]Ref, s.records)
	for i := range refs {
		ref, err := w.Add(s.record(r, i))
		if err != nil {
			return nil, nil, err
		}
		refs[i] = ref
	}
	var sample []netip.Prefix
	insert := func(p netip.Prefix, n int) error {
		if n%s.sampleEvery == 0 {
			sample = append(sample, p)
		}
		return w.Insert(p, refs[r.IntN(len(refs))])
	}
	// The networks of each family, one after the other from the start of
	// its range, each aligned on its size.
	families := []struct {
		count  int
		bits   func(*rand.Rand) int
		start  uint64 // the first address of the range, as a number
		end    uint64 // after the last
		width  int    // the bits of the numbers
		prefix func(n uint64, bits int) netip.Prefix
	}{
		{s.ipv4, s.ipv4Bits, 0xf0000000, 1 << 32, 32, func(n uint64, bits int) netip.Prefix {
			var a [4]byte
			binary.BigEndian.PutUint32(a[:], uint32(n))
			return netip.PrefixFrom(netip.AddrFrom4(a), bits)
		}},
		// The IPv6 numbers count /64 networks in 2001:db8::/32, from
		// 2001:db8:1::, after the network of the examples.
		{s.ipv6, s.ipv6Bits, 1 << 16, 1 << 32, 64, func(n uint64, bits int) netip.Prefix {
			var a [16]byte
			binary.BigEndian.PutUint64(a[:8], 0x20010db8<<32|n)
			return netip.PrefixFrom(netip.AddrFrom16(a), bits)
		}},
	}
	for _, f := range families {
		next := f.start
		for n := range f.count {
			bits := f.bits(r)
			size := uint64(1) << (f.width - bits)
			next = (next + size - 1) &^ (size - 1)
			if next+size > f.end {
				return nil, nil, errors.New("mmdbtest: the networks of a large database do not fit in their range")
			}
			if err := insert(f.prefix(next, bits), n); err != nil {
				return nil, nil, err
			}
			next += size
			if r.IntN(100) < s.gapPercent {
				next += uint64(1) << (f.width - f.bits(r))
			}
		}
	}
	if err := s.examples(w); err != nil {
		return nil, nil, err
	}
	content, err := w.Bytes()
	return content, sample, err
}

// Addresses returns n random addresses of the networks of sample, the same
// ones for the same seed.
func Addresses(sample []netip.Prefix, n int, seed uint64) []netip.Addr {
	r := rand.New(rand.NewPCG(seed, 1))
	out := make([]netip.Addr, n)
	for i := range out {
		p := sample[r.IntN(len(sample))]
		b := p.Addr().AsSlice()
		for bit := p.Bits(); bit < len(b)*8; bit++ {
			if r.IntN(2) == 1 {
				b[bit/8] |= 0x80 >> (bit % 8)
			}
		}
		out[i], _ = netip.AddrFromSlice(b)
	}
	return out
}

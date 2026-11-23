package mmdbtest

import (
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/oschwald/maxminddb-golang/v2"
)

// open reads the database of w with the reader that tessera uses, and
// checks it whole with Verify.
func open(t *testing.T, w *Writer) *maxminddb.Reader {
	t.Helper()
	data, err := w.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	db, err := maxminddb.OpenBytes(data)
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	if err := db.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return db
}

// place is a continent, a country or a subdivision of a record.
type place struct {
	Code    string            `maxminddb:"code"`
	ISOCode string            `maxminddb:"iso_code"`
	Names   map[string]string `maxminddb:"names"`
}

type cityRecord struct {
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
	Continent place `maxminddb:"continent"`
	Country   struct {
		place
		InEU bool `maxminddb:"is_in_european_union"`
	} `maxminddb:"country"`
	Registered struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
	Location struct {
		Latitude  *float64 `maxminddb:"latitude"`
		Longitude *float64 `maxminddb:"longitude"`
		TimeZone  string   `maxminddb:"time_zone"`
	} `maxminddb:"location"`
	Postal struct {
		Code string `maxminddb:"code"`
	} `maxminddb:"postal"`
	Subdivisions []place `maxminddb:"subdivisions"`
}

// city returns what the record holds of a City.
func (r cityRecord) city() City {
	c := City{
		Continent:  Place{r.Continent.Code, r.Continent.Names["en"]},
		Country:    Place{r.Country.ISOCode, r.Country.Names["en"]},
		InEU:       r.Country.InEU,
		Registered: r.Registered.ISOCode,
		City:       r.City.Names["en"],
		PostalCode: r.Postal.Code,
		TimeZone:   r.Location.TimeZone,
	}
	for _, s := range r.Subdivisions {
		c.Subdivisions = append(c.Subdivisions, Place{s.ISOCode, s.Names["en"]})
	}
	if r.Location.Latitude != nil && r.Location.Longitude != nil {
		c.Location = &Point{*r.Location.Latitude, *r.Location.Longitude}
	}
	return c
}

// The example databases read back, through every record size, with and
// without pointers, and in IPv4 and IPv6 trees.
func TestRoundTrip(t *testing.T) {
	for _, ipVersion := range []int{4, 6} {
		for _, size := range []int{24, 28, 32} {
			for _, pointers := range []bool{false, true} {
				t.Run(fmt.Sprintf("IPv%d, %d bits, pointers %t", ipVersion, size, pointers), func(t *testing.T) {
					w := CityDatabase(ipVersion)
					w.RecordSize, w.Pointers = size, pointers
					db := open(t, w)
					m := db.Metadata
					if m.DatabaseType != "GeoIP2-City" || m.BuildEpoch != BuildEpoch || m.IPVersion != uint(ipVersion) || m.RecordSize != uint(size) || m.BinaryFormatMajorVersion != 2 || !reflect.DeepEqual(m.Languages, []string{"en"}) {
						t.Fatalf("metadata %+v", m)
					}
					for _, c := range ExampleCities {
						if ipVersion == 4 && c.Network.Addr().Is6() {
							continue
						}
						for _, addr := range []netip.Addr{c.Network.Addr(), lastAddr(c.Network)} {
							var r cityRecord
							result := db.Lookup(addr)
							if err := result.Decode(&r); err != nil || !result.Found() {
								t.Fatalf("%s: found %t, %v", addr, result.Found(), err)
							}
							if got := r.city(); !reflect.DeepEqual(got, c.City) {
								t.Errorf("%s: %+v, want %+v", addr, got, c.City)
							}
							if result.Prefix() != c.Network {
								t.Errorf("%s: network %s, want %s", addr, result.Prefix(), c.Network)
							}
						}
					}
					for _, addr := range []string{"203.0.113.200", "192.0.3.1", "2001:db8:1::1"} {
						a := netip.MustParseAddr(addr)
						if ipVersion == 4 && a.Is6() {
							continue
						}
						if db.Lookup(a).Found() {
							t.Errorf("%s is found", addr)
						}
					}
				})
			}
		}
	}
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

func TestCountryAndASN(t *testing.T) {
	db := open(t, CountryDatabase())
	var r cityRecord
	if err := db.Lookup(netip.MustParseAddr("192.0.2.7")).Decode(&r); err != nil {
		t.Fatal(err)
	}
	if got := r.city(); !reflect.DeepEqual(got, City{Continent: Place{"EU", "Europe"}, Country: Place{"IT", "Italy"}, InEU: true, Registered: "IT"}) {
		t.Errorf("country record %+v", got)
	}
	db = open(t, ASNDatabase())
	for _, a := range ExampleASNs {
		var got struct {
			Number       uint32 `maxminddb:"autonomous_system_number"`
			Organization string `maxminddb:"autonomous_system_organization"`
		}
		if err := db.Lookup(a.Network.Addr().Next()).Decode(&got); err != nil || got.Number != a.ASN.Number || got.Organization != a.ASN.Organization {
			t.Errorf("%s: %+v, %v", a.Network, got, err)
		}
	}
	if db.Lookup(netip.MustParseAddr("203.0.113.7")).Found() {
		t.Error("203.0.113.7 is in the ASN database")
	}
}

// Every type of value and every size, pointers of one, two and three bytes,
// and networks that overlap.
func TestValues(t *testing.T) {
	w := New("tessera-values", 7)
	values := map[string]any{
		"string":     "a",
		"empty":      "",
		"string 29":  strings.Repeat("b", 29),
		"string 285": strings.Repeat("c", 285),
		"string 64k": strings.Repeat("d", 65821),
		"double":     -122.4194,
		"float":      float32(1.5),
		"uint16":     uint16(65535),
		"uint32":     uint32(4294967295),
		"uint64":     uint64(1 << 63),
		"zero":       uint32(0),
		"int32":      int32(-5),
		"true":       true,
		"false":      false,
		"bytes":      []byte{0, 1, 2},
		"array":      []any{"x", uint16(1), []any{}},
		"map":        map[string]any{"nested": map[string]any{}},
	}
	must(w.Insert(netip.MustParsePrefix("192.0.2.0/24"), values))
	// Strings far into the data section, which later records point to.
	var far []string
	for i := range 600 {
		s := fmt.Sprintf("%04d%s", i, strings.Repeat("e", 996))
		far = append(far, s)
		must(w.Insert(netip.PrefixFrom(netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 1, byte(i >> 8), byte(i)}), 64), map[string]any{"s": s}))
	}
	shared := map[string]any{"near": far[0], "middle": far[5], "far": far[599]}
	must(w.Insert(netip.MustParsePrefix("198.51.100.0/24"), shared))
	// A network inside 198.51.100.0/24, inserted after it: the rest of the
	// larger network keeps its data.
	must(w.Insert(netip.MustParsePrefix("198.51.100.128/26"), "inside"))
	data, err := w.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 600_000 || !strings.Contains(string(data), far[599]) {
		t.Fatalf("the data section has %d bytes", len(data))
	}
	db := open(t, w)
	var got map[string]any
	if err := db.Lookup(netip.MustParseAddr("192.0.2.1")).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"string": "a", "empty": "", "string 29": values["string 29"], "string 285": values["string 285"], "string 64k": values["string 64k"],
		"double": -122.4194, "float": float32(1.5), "uint16": uint64(65535), "uint32": uint64(4294967295), "uint64": uint64(1 << 63), "zero": uint64(0),
		"int32": int32(-5), "true": true, "false": false, "bytes": []byte{0, 1, 2},
		"array": []any{"x", uint64(1), []any{}}, "map": map[string]any{"nested": map[string]any{}},
	}
	for k, v := range want {
		if !reflect.DeepEqual(got[k], v) {
			t.Errorf("%s: %#v (%T), want %#v (%T)", k, got[k], got[k], v, v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d values, want %d", len(got), len(want))
	}
	var s map[string]string
	if err := db.Lookup(netip.MustParseAddr("198.51.100.1")).Decode(&s); err != nil || s["near"] != far[0] || s["middle"] != far[5] || s["far"] != far[599] {
		t.Errorf("pointed strings: %v", err)
	}
	var inside string
	if err := db.Lookup(netip.MustParseAddr("198.51.100.130")).Decode(&inside); err != nil || inside != "inside" {
		t.Errorf("inner network: %q, %v", inside, err)
	}
	for _, addr := range []string{"198.51.100.127", "198.51.100.192"} {
		if err := db.Lookup(netip.MustParseAddr(addr)).Decode(&s); err != nil || s["far"] != far[599] {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

// A record encoded once reads back as the record.
func TestEncode(t *testing.T) {
	raw, err := Encode(Rome.Record())
	if err != nil {
		t.Fatal(err)
	}
	w := New("GeoIP2-City", 1)
	for i := range 4 {
		must(w.Insert(netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(64 * i)}), 26), raw))
	}
	db := open(t, w)
	var r cityRecord
	if err := db.Lookup(netip.MustParseAddr("192.0.2.200")).Decode(&r); err != nil || !reflect.DeepEqual(r.city(), Rome) {
		t.Fatalf("%+v, %v", r.city(), err)
	}
	if _, err := Encode(7); err == nil {
		t.Error("an int is not a type of the data section")
	}
}

// A record added once, then inserted by its Ref for several networks, reads
// back as the record, with its pointers.
func TestAdd(t *testing.T) {
	w := New("GeoIP2-City", 1)
	rome, err := w.Add(Rome.Record())
	if err != nil {
		t.Fatal(err)
	}
	tokyo, err := w.Add(Tokyo.Record())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		must(w.Insert(netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(64 * i)}), 26), rome))
	}
	must(w.Insert(netip.MustParsePrefix("2001:db8::/48"), tokyo))
	db := open(t, w)
	for addr, want := range map[string]City{"192.0.2.200": Rome, "192.0.2.7": Rome, "2001:db8::7": Tokyo} {
		var r cityRecord
		if err := db.Lookup(netip.MustParseAddr(addr)).Decode(&r); err != nil || !reflect.DeepEqual(r.city(), want) {
			t.Errorf("%s: %+v, %v", addr, r.city(), err)
		}
	}
	if _, err := w.Add(7); err == nil {
		t.Error("an int is not a type of the data section")
	}
}

func TestInsertErrors(t *testing.T) {
	w := New("x", 0)
	w.IPVersion = 4
	for _, p := range []netip.Prefix{{}, netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("2001:db8::/32")} {
		if err := w.Insert(p, "a"); err == nil {
			t.Errorf("%s: inserted", p)
		}
	}
	if err := w.Insert(netip.MustParsePrefix("192.0.2.0/24"), 7); err == nil {
		t.Error("an int is not a type of the data section")
	}
	w.RecordSize = 20
	if _, err := w.Bytes(); err == nil {
		t.Error("a record size of 20 bits")
	}
}

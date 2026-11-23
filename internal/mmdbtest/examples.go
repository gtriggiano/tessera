package mmdbtest

import (
	"net/netip"
	"path/filepath"
	"testing"
)

// BuildEpoch is the build time of the example databases: 2026-11-17 at
// 10:00 UTC, in seconds since the Unix epoch.
const BuildEpoch = 1794909600

// Point is a latitude and a longitude.
type Point struct{ Latitude, Longitude float64 }

// Place is the code and the English name of a continent, a country or a
// subdivision.
type Place struct{ Code, Name string }

// City is the content of a record of a GeoIP2 City database that geo reads.
// An empty field is left out of the record.
type City struct {
	Continent Place // continent.code and continent.names.en
	Country   Place // country.iso_code and country.names.en
	InEU      bool  // country.is_in_european_union
	// Registered is registered_country.iso_code, which geo does not read.
	Registered string
	// Subdivisions are the subdivisions, from the largest to the smallest:
	// iso_code, without the country, and names.en.
	Subdivisions []Place
	City         string // city.names.en
	PostalCode   string // postal.code
	TimeZone     string // location.time_zone
	Location     *Point // location.latitude and location.longitude
}

// Record returns the record of c in a GeoIP2 City database, with the
// fields that geo does not read too, such as the names in other languages
// and the GeoNames identifiers.
func (c City) Record() map[string]any {
	r := c.CountryRecord()
	if c.City != "" {
		r["city"] = map[string]any{"geoname_id": uint32(3169070), "names": names(c.City)}
	}
	if c.PostalCode != "" {
		r["postal"] = map[string]any{"code": c.PostalCode}
	}
	location := map[string]any{}
	if c.TimeZone != "" {
		location["time_zone"] = c.TimeZone
	}
	if c.Location != nil {
		location["latitude"] = c.Location.Latitude
		location["longitude"] = c.Location.Longitude
		location["accuracy_radius"] = uint16(20)
	}
	if len(location) > 0 {
		r["location"] = location
	}
	if len(c.Subdivisions) > 0 {
		subdivisions := make([]any, len(c.Subdivisions))
		for i, s := range c.Subdivisions {
			subdivisions[i] = map[string]any{"geoname_id": uint32(3174976 + i), "iso_code": s.Code, "names": names(s.Name)}
		}
		r["subdivisions"] = subdivisions
	}
	return r
}

// CountryRecord returns the record of c in a GeoIP2 Country database: the
// continent and the countries only.
func (c City) CountryRecord() map[string]any {
	r := map[string]any{}
	if c.Continent != (Place{}) {
		r["continent"] = map[string]any{"code": c.Continent.Code, "geoname_id": uint32(6255148), "names": names(c.Continent.Name)}
	}
	if c.Country != (Place{}) {
		country := map[string]any{"geoname_id": uint32(3175395), "iso_code": c.Country.Code, "names": names(c.Country.Name)}
		if c.InEU {
			country["is_in_european_union"] = true
		}
		r["country"] = country
	}
	if c.Registered != "" {
		r["registered_country"] = map[string]any{"geoname_id": uint32(2921044), "iso_code": c.Registered, "names": names(c.Registered)}
	}
	return r
}

// ASN is the content of a record of a GeoLite2 ASN database.
type ASN struct {
	Number       uint32
	Organization string
}

// Record returns the record of a in a GeoLite2 ASN database.
func (a ASN) Record() map[string]any {
	return map[string]any{"autonomous_system_number": a.Number, "autonomous_system_organization": a.Organization}
}

// The example data, in the documentation ranges. 203.0.113.128/25 and the
// rest of 2001:db8::/32 are in no example database, and 203.0.113.0/24 in no
// example ASN database.
var (
	Rome = City{
		Continent: Place{"EU", "Europe"}, Country: Place{"IT", "Italy"}, InEU: true, Registered: "IT",
		Subdivisions: []Place{{"62", "Lazio"}, {"RM", "Rome"}},
		City:         "Rome", PostalCode: "00118", TimeZone: "Europe/Rome", Location: &Point{41.8919, 12.5113},
	}
	SanFrancisco = City{
		Continent: Place{"NA", "North America"}, Country: Place{"US", "United States"}, Registered: "US",
		Subdivisions: []Place{{"CA", "California"}},
		City:         "San Francisco", PostalCode: "94118", TimeZone: "America/Los_Angeles", Location: &Point{37.7749, -122.4194},
	}
	Tokyo      = City{Continent: Place{"AS", "Asia"}, Country: Place{"JP", "Japan"}, Registered: "JP", TimeZone: "Asia/Tokyo"}
	Registered = City{Registered: "DE"}
	Berlin     = City{
		Continent: Place{"EU", "Europe"}, Country: Place{"DE", "Germany"}, InEU: true, Registered: "DE",
		Subdivisions: []Place{{"BE", "State of Berlin"}},
		City:         "Berlin", PostalCode: "10178", TimeZone: "Europe/Berlin", Location: &Point{52.52, 13.405},
	}

	ExampleCities = []struct {
		Network netip.Prefix
		City    City
	}{
		{netip.MustParsePrefix("192.0.2.0/24"), Rome},
		{netip.MustParsePrefix("198.51.100.0/24"), SanFrancisco},
		{netip.MustParsePrefix("203.0.113.0/26"), Tokyo},
		{netip.MustParsePrefix("203.0.113.64/26"), Registered},
		{netip.MustParsePrefix("2001:db8::/48"), Berlin},
	}

	ExampleASNs = []struct {
		Network netip.Prefix
		ASN     ASN
	}{
		{netip.MustParsePrefix("192.0.2.0/24"), ASN{64496, "Example Transit"}},
		{netip.MustParsePrefix("198.51.100.0/24"), ASN{64500, "Example Hosting"}},
		{netip.MustParsePrefix("2001:db8::/48"), ASN{64511, "Example IPv6 Networks"}},
	}
)

// CityDatabase returns a GeoIP2-City-like database of the example cities,
// of IP version 6 or 4; an IPv4 database leaves out the IPv6 networks.
func CityDatabase(ipVersion int) *Writer {
	w := New("GeoIP2-City", BuildEpoch)
	w.IPVersion = ipVersion
	for _, c := range ExampleCities {
		if ipVersion == 4 && c.Network.Addr().Is6() {
			continue
		}
		must(w.Insert(c.Network, c.City.Record()))
	}
	return w
}

// CountryDatabase returns a GeoIP2-Country-like database of the example
// cities.
func CountryDatabase() *Writer {
	w := New("GeoIP2-Country", BuildEpoch)
	for _, c := range ExampleCities {
		must(w.Insert(c.Network, c.City.CountryRecord()))
	}
	return w
}

// ASNDatabase returns a GeoLite2-ASN-like database of the example networks.
func ASNDatabase() *Writer {
	w := New("GeoLite2-ASN", BuildEpoch)
	for _, a := range ExampleASNs {
		must(w.Insert(a.Network, a.ASN.Record()))
	}
	return w
}

// BrokenDatabase returns a database of kind, geo or asn, that tessera
// loads, but in which every address of 198.51.100.0/24, 203.0.113.0/24 and
// 2001:db8::/32 has a record that a lookup cannot decode, because its
// fields have other types than those of the schema. The records that the
// load checks, in 192.0.2.0/24, are valid.
func BrokenDatabase(kind string) *Writer {
	w := New("tessera-broken-"+kind, BuildEpoch)
	good, bad := Rome.Record(), map[string]any{"country": "IT", "location": "here", "city": uint32(1)}
	if kind == "asn" {
		good, bad = ExampleASNs[0].ASN.Record(), map[string]any{"autonomous_system_number": "AS64496", "autonomous_system_organization": uint32(1)}
	}
	for i := range 16 {
		must(w.Insert(netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(16 * i)}), 28), good))
	}
	for _, n := range []string{"198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32"} {
		must(w.Insert(netip.MustParsePrefix(n), bad))
	}
	return w
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// WriteFile writes w to a file named name in a new temporary directory of
// t, and returns its path.
func WriteFile(t testing.TB, w *Writer, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := w.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	return path
}

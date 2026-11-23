package enrichment

import (
	"bytes"
	"errors"
	"io"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/mmdbtest"
)

func database(t testing.TB, k Kind, w *mmdbtest.Writer) *Database {
	t.Helper()
	data, err := w.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	db, err := Read(k, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return db
}

func TestRead(t *testing.T) {
	db := database(t, Geo, mmdbtest.CityDatabase(6))
	if db.Kind() != Geo || db.Type() != "GeoIP2-City" || !db.Built().Equal(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)) || db.Built().Location() != time.UTC {
		t.Errorf("kind %s, type %q, built %v", db.Kind(), db.Type(), db.Built())
	}
	// The size is a hint: a wrong one reads the same database.
	data, _ := mmdbtest.ASNDatabase().Bytes()
	for _, size := range []int64{0, -1, 10, int64(len(data)), 1 << 40} {
		db, err := Read(ASN, bytes.NewReader(data), size)
		if err != nil || db.Type() != "GeoLite2-ASN" {
			t.Errorf("size %d: %v", size, err)
		}
	}
	// A long type is cut at a character boundary.
	w := mmdbtest.ASNDatabase()
	w.DatabaseType = strings.Repeat("é", 100)
	if got := database(t, ASN, w).Type(); got != strings.Repeat("é", 64) {
		t.Errorf("type of %d bytes", len(got))
	}
}

// The records of the example cities, as geo reads them.
var (
	rome = GeoRecord{
		Found: true, Country: Country{"IT", "Italy", true}, Continent: Continent{"EU", "Europe"},
		Subdivisions: []Subdivision{{"62", "Lazio"}, {"RM", "Rome"}},
		City:         "Rome", PostalCode: "00118", TimeZone: "Europe/Rome",
		HasLocation: true, Latitude: 41.8919, Longitude: 12.5113,
	}
	sanFrancisco = GeoRecord{
		Found: true, Country: Country{"US", "United States", false}, Continent: Continent{"NA", "North America"},
		Subdivisions: []Subdivision{{"CA", "California"}},
		City:         "San Francisco", PostalCode: "94118", TimeZone: "America/Los_Angeles",
		HasLocation: true, Latitude: 37.7749, Longitude: -122.4194,
	}
	berlin = GeoRecord{
		Found: true, Country: Country{"DE", "Germany", true}, Continent: Continent{"EU", "Europe"},
		Subdivisions: []Subdivision{{"BE", "State of Berlin"}},
		City:         "Berlin", PostalCode: "10178", TimeZone: "Europe/Berlin",
		HasLocation: true, Latitude: 52.52, Longitude: 13.405,
	}
)

func TestGeo(t *testing.T) {
	for _, ipVersion := range []int{6, 4} {
		db := database(t, Geo, mmdbtest.CityDatabase(ipVersion))
		for addr, want := range map[string]GeoRecord{
			"192.0.2.7":        rome,
			"::ffff:192.0.2.7": rome,
			"198.51.100.255":   sanFrancisco,
			// A record without subdivisions, city, postal code and
			// location.
			"203.0.113.7":       {Found: true, Country: Country{"JP", "Japan", false}, Continent: Continent{"AS", "Asia"}, TimeZone: "Asia/Tokyo"},
			"203.0.113.70":      {Found: true}, // registered_country only
			"203.0.113.200":     {},
			"2001:db8::7":       berlin,
			"2001:db8:1::7":     {},
			"fe80::1%eth0":      {},
			"2001:db8::7%en0":   berlin,
			"::":                {},
			"255.255.255.255":   {},
			"2001:db8:ffff::ff": {},
		} {
			if ipVersion == 4 && strings.Contains(addr, "2001:db8::7") {
				// An IPv6 address is not in an IPv4 database: not an
				// error.
				want = GeoRecord{}
			}
			got, err := db.Geo(netip.MustParseAddr(addr))
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("IPv%d database, %s: %+v, %v\nwant %+v", ipVersion, addr, got, err, want)
			}
		}
	}
	// A Country database has no subdivisions, city, postal code, time zone
	// or location.
	db := database(t, Geo, mmdbtest.CountryDatabase())
	if got, err := db.Geo(netip.MustParseAddr("192.0.2.7")); err != nil || !reflect.DeepEqual(got, GeoRecord{Found: true, Country: Country{"IT", "Italy", true}, Continent: Continent{"EU", "Europe"}}) {
		t.Errorf("Country database: %+v, %v", got, err)
	}
	// A location needs both coordinates. A subdivision without a code or
	// a name in English has an empty one, and keeps its place in the
	// order; a name in another language is not read.
	w := mmdbtest.New("GeoIP2-City", 1)
	if err := w.Insert(netip.MustParsePrefix("192.0.2.0/24"), map[string]any{
		"location": map[string]any{"latitude": 41.9},
		"subdivisions": []any{
			map[string]any{"iso_code": "62"},
			map[string]any{"geoname_id": uint32(1), "names": map[string]any{"en": "Rome", "it": "Roma"}},
			map[string]any{"iso_code": "RM", "names": map[string]any{"it": "Roma"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Insert(netip.MustParsePrefix("198.51.100.0/24"), map[string]any{
		"country":      map[string]any{"iso_code": "IT", "names": map[string]any{"de": "Italien"}},
		"continent":    map[string]any{"names": map[string]any{"en": "Europe"}},
		"subdivisions": []any{},
		"postal":       map[string]any{"code": "EC4N"},
		"location":     map[string]any{"longitude": 12.5},
	}); err != nil {
		t.Fatal(err)
	}
	db = database(t, Geo, w)
	for addr, want := range map[string]GeoRecord{
		"192.0.2.7":    {Found: true, Subdivisions: []Subdivision{{"62", ""}, {"", "Rome"}, {"RM", ""}}},
		"198.51.100.7": {Found: true, Country: Country{Code: "IT"}, Continent: Continent{Name: "Europe"}, PostalCode: "EC4N"},
	} {
		if got, err := db.Geo(netip.MustParseAddr(addr)); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %+v, %v\nwant %+v", addr, got, err, want)
		}
	}
	if _, err := db.ASN(netip.MustParseAddr("192.0.2.7")); err == nil {
		t.Error("an asn lookup in a geo database")
	}
}

func TestASN(t *testing.T) {
	db := database(t, ASN, mmdbtest.ASNDatabase())
	for addr, want := range map[string]ASNRecord{
		"192.0.2.7":     {Found: true, Number: 64496, Organization: "Example Transit"},
		"198.51.100.1":  {Found: true, Number: 64500, Organization: "Example Hosting"},
		"2001:db8::1":   {Found: true, Number: 64511, Organization: "Example IPv6 Networks"},
		"203.0.113.7":   {},
		"2001:db8:2::1": {},
	} {
		got, err := db.ASN(netip.MustParseAddr(addr))
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v; want %+v", addr, got, err, want)
		}
	}
	if _, err := db.Geo(netip.MustParseAddr("192.0.2.7")); err == nil {
		t.Error("a geo lookup in an asn database")
	}
}

// A record that cannot be decoded fails the lookup: it is never an address
// that the database does not have.
func TestLookupErrors(t *testing.T) {
	for _, tc := range []struct {
		kind   Kind
		lookup func(*Database, netip.Addr) (bool, error)
	}{
		{Geo, func(db *Database, a netip.Addr) (bool, error) { r, err := db.Geo(a); return r.Found, err }},
		{ASN, func(db *Database, a netip.Addr) (bool, error) { r, err := db.ASN(a); return r.Found, err }},
	} {
		db := database(t, tc.kind, mmdbtest.BrokenDatabase(string(tc.kind)))
		if found, err := tc.lookup(db, netip.MustParseAddr("192.0.2.7")); !found || err != nil {
			t.Errorf("%s: a valid record: %t, %v", tc.kind, found, err)
		}
		for _, addr := range []string{"198.51.100.7", "203.0.113.7", "2001:db8::7"} {
			if found, err := tc.lookup(db, netip.MustParseAddr(addr)); found || !errors.Is(err, ErrLookup) {
				t.Errorf("%s, %s: %t, %v; want ErrLookup", tc.kind, addr, found, err)
			}
		}
	}
}

func TestInvalidContent(t *testing.T) {
	city, _ := mmdbtest.CityDatabase(6).Bytes()
	marker := bytes.LastIndex(city, []byte("\xAB\xCD\xEFMaxMind.com"))
	withMetadata := func(key string, value any) []byte {
		w := mmdbtest.CityDatabase(6)
		w.Metadata = map[string]any{key: value}
		data, err := w.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	// A record that does not have the schema of its kind.
	wrongGeo := mmdbtest.New("GeoIP2-City", 1)
	if err := wrongGeo.Insert(netip.MustParsePrefix("192.0.2.0/24"), map[string]any{"country": map[string]any{"iso_code": uint32(380)}}); err != nil {
		t.Fatal(err)
	}
	wrongGeoData, _ := wrongGeo.Bytes()
	wrongASN := mmdbtest.New("GeoLite2-ASN", 1)
	if err := wrongASN.Insert(netip.MustParsePrefix("192.0.2.0/24"), map[string]any{"autonomous_system_number": "AS64496"}); err != nil {
		t.Fatal(err)
	}
	wrongASNData, _ := wrongASN.Bytes()
	// A data pointer of the tree beyond the data section.
	corrupt := mmdbtest.New("GeoIP2-City", 1)
	if err := corrupt.Insert(netip.MustParsePrefix("192.0.2.0/24"), mmdbtest.Raw{0x20, 0xff}); err != nil {
		t.Fatal(err)
	}
	corruptData, _ := corrupt.Bytes()
	for _, tc := range []struct {
		name    string
		kind    Kind
		content []byte
		want    string
	}{
		{"empty", Geo, nil, "the file is not a valid MMDB database"},
		{"text", Geo, []byte("192.0.2.0/24\n"), "the file is not a valid MMDB database"},
		{"no metadata", Geo, city[:marker], "the file is not a valid MMDB database"},
		{"truncated tree", Geo, append(slices.Clone(city[:40]), city[marker:]...), "the file is not a valid MMDB database"},
		{"format version", Geo, withMetadata("binary_format_major_version", uint16(3)), ErrUnsupported.Error()},
		{"IP version", Geo, withMetadata("ip_version", uint16(5)), ErrUnsupported.Error()},
		{"record size", Geo, withMetadata("record_size", uint16(20)), ""},
		{"metadata of another type", Geo, withMetadata("node_count", "many"), "the file is not a valid MMDB database"},
		{"geo schema", Geo, wrongGeoData, "a record of the database does not have the schema of a GeoIP2 City or Country database"},
		{"asn schema", ASN, wrongASNData, "a record of the database does not have the schema of a GeoLite2 ASN database"},
		{"corrupt record", Geo, corruptData, "the file is not a valid MMDB database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Parse(tc.kind, tc.content)
			if db != nil || err == nil {
				t.Fatalf("accepted: %v", db)
			}
			if tc.want == "" {
				if err != ErrInvalid && err != ErrUnsupported {
					t.Errorf("error %v", err)
				}
			} else if err.Error() != tc.want {
				t.Errorf("error %q, want %q", err, tc.want)
			}
		})
	}
	if _, err := Parse("ua", city); err == nil {
		t.Error("an unknown kind")
	}
}

// A database of the other kind decodes without error, but none of its
// records has a key of the schema of the kind: it is rejected, so that a
// swapped file is not taken for a database that has no address.
func TestSwappedDatabases(t *testing.T) {
	asn, _ := mmdbtest.ASNDatabase().Bytes()
	city, _ := mmdbtest.CityDatabase(6).Bytes()
	if _, err := Parse(Geo, asn); err == nil || err.Error() != "a record of the database does not have the schema of a GeoIP2 City or Country database" {
		t.Errorf("an ASN database as geo: %v", err)
	}
	if _, err := Parse(ASN, city); err == nil || err.Error() != "a record of the database does not have the schema of a GeoLite2 ASN database" {
		t.Errorf("a City database as asn: %v", err)
	}
	// A key of the schema is enough, whatever its value, and so is one
	// record among those probed; a database without records has nothing to
	// tell its kind.
	only := func(kind string, records ...map[string]any) []byte {
		w := mmdbtest.New(kind, 1)
		for i, r := range records {
			if err := w.Insert(netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(16 * i)}), 28), r); err != nil {
				t.Fatal(err)
			}
		}
		data, err := w.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for name, tc := range map[string]struct {
		kind    Kind
		content []byte
	}{
		"Country":                  {Geo, mustBytes(t, mmdbtest.CountryDatabase())},
		"postal only":              {Geo, only("x", map[string]any{"postal": map[string]any{"code": "00100"}})},
		"represented, any value":   {Geo, only("x", map[string]any{"represented_country": "IT"})},
		"one record of the schema": {Geo, only("x", map[string]any{"other": "a"}, map[string]any{"other": "b"}, map[string]any{"registered_country": map[string]any{"iso_code": "DE"}})},
		"organization only":        {ASN, only("x", map[string]any{"autonomous_system_organization": "Example Transit"})},
		"no records":               {ASN, only("x")},
	} {
		if _, err := Parse(tc.kind, tc.content); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		kind    Kind
		content []byte
	}{
		"empty records": {Geo, only("x", map[string]any{}, map[string]any{})},
		"other keys":    {ASN, only("x", map[string]any{"country": map[string]any{"iso_code": "IT"}, "asn": uint32(64496)})},
	} {
		if _, err := Parse(tc.kind, tc.content); err == nil || (err != errGeoSchema && err != errASNSchema) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func mustBytes(t *testing.T, w *mmdbtest.Writer) []byte {
	t.Helper()
	data, err := w.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A build time that RFC 3339 cannot write is taken as the last second of the
// year 9999; the database stays usable.
func TestBuildTimeClamp(t *testing.T) {
	last := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	for epoch, want := range map[uint64]time.Time{
		uint64(last.Unix()):     last,
		uint64(last.Unix()) + 1: last,
		1 << 63:                 last,
		0:                       time.Unix(0, 0).UTC(),
	} {
		w := mmdbtest.ASNDatabase()
		w.BuildEpoch = epoch
		db := database(t, ASN, w)
		if !db.Built().Equal(want) || db.Built().Format(time.RFC3339) != want.Format(time.RFC3339) {
			t.Errorf("build_epoch %d: %v, want %v", epoch, db.Built(), want)
		}
		if r, err := db.ASN(netip.MustParseAddr("192.0.2.7")); err != nil || r.Number != 64496 {
			t.Errorf("build_epoch %d: lookup %+v, %v", epoch, r, err)
		}
	}
	if MaxBuildTime.Format(time.RFC3339) != "9999-12-31T23:59:59Z" {
		t.Errorf("MaxBuildTime %v", MaxBuildTime)
	}
}

// The ceiling counts the bytes read, not the size that the caller gives.
func TestCeiling(t *testing.T) {
	data, _ := mmdbtest.ASNDatabase().Bytes()
	if _, err := ReadAtMost(ASN, bytes.NewReader(data), 0, int64(len(data))); err != nil {
		t.Errorf("content of the ceiling: %v", err)
	}
	if _, err := ReadAtMost(ASN, bytes.NewReader(append(data, 0)), int64(len(data)), int64(len(data))); err != ErrTooLarge || err.Error() != "the file is larger than 512 MiB" {
		t.Errorf("content over the ceiling: %v", err)
	}
	// A failure of the reader is returned as it is.
	failure := errors.New("s3cr3t path")
	if _, err := Read(ASN, io.MultiReader(bytes.NewReader(data[:10]), errReader{failure}), int64(len(data))); err != failure {
		t.Errorf("read failure: %v", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestStore(t *testing.T) {
	store, err := NewStore([]Kind{ASN, Geo})
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Kinds(); !slices.Equal(got, []Kind{ASN, Geo}) {
		t.Errorf("kinds %v", got)
	}
	first := store.Snapshot()
	if first.Database(Geo) != nil || first.Database(ASN) != nil {
		t.Fatal("a database before Replace")
	}
	geo, asn := database(t, Geo, mmdbtest.CityDatabase(6)), database(t, ASN, mmdbtest.ASNDatabase())
	if err := store.Replace("geo", geo); err != nil {
		t.Fatal(err)
	}
	if err := store.Replace("asn", asn); err != nil {
		t.Fatal(err)
	}
	snap := store.Snapshot()
	if snap.Database(Geo) != geo || snap.Database(ASN) != asn || first.Database(Geo) != nil {
		t.Error("snapshots")
	}
	for _, tc := range []struct {
		name string
		db   *Database
	}{{"geo", asn}, {"geo", nil}, {"ua", geo}} {
		if err := store.Replace(tc.name, tc.db); err == nil {
			t.Errorf("Replace(%s) accepted", tc.name)
		}
	}
	only, _ := NewStore([]Kind{Geo})
	if err := only.Replace("asn", asn); err == nil {
		t.Error("a database that is not configured")
	}
	for _, kinds := range [][]Kind{{Geo, Geo}, {"ua"}} {
		if _, err := NewStore(kinds); err == nil {
			t.Errorf("NewStore(%v)", kinds)
		}
	}
	var none *Store
	if none.Snapshot() != nil || none.Kinds() != nil || none.Snapshot().Database(Geo) != nil {
		t.Error("a nil Store")
	}
}

// Lookups run while the copies are replaced: each sees a whole copy. Run
// with -race.
func TestConcurrentReplacement(t *testing.T) {
	store, _ := NewStore([]Kind{Geo})
	a := database(t, Geo, mmdbtest.CityDatabase(6))
	b := database(t, Geo, mmdbtest.CountryDatabase())
	_ = store.Replace("geo", a)
	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = store.Replace("geo", []*Database{a, b}[i%2])
		}
	})
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 2000 {
				r, err := store.Snapshot().Database(Geo).Geo(netip.MustParseAddr("192.0.2.7"))
				if err != nil || r.Country.Code != "IT" || r.City != "" && r.City != "Rome" {
					t.Errorf("%+v, %v", r, err)
					return
				}
			}
		})
	}
	workers.Wait()
	close(stop)
	writer.Wait()
}

// FuzzParse reads arbitrary content as a database of each kind: it never
// panics, and either accepts the content, whose lookups then fail only with
// ErrLookup, or rejects it with one of the fixed errors.
func FuzzParse(f *testing.F) {
	for _, w := range []*mmdbtest.Writer{mmdbtest.CityDatabase(6), mmdbtest.CityDatabase(4), mmdbtest.CountryDatabase(), mmdbtest.ASNDatabase(), mmdbtest.BrokenDatabase("geo")} {
		data, err := w.Bytes()
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
		f.Add(data[len(data)/2:])
	}
	f.Add([]byte{})
	f.Add([]byte("\xAB\xCD\xEFMaxMind.com"))
	fixed := []error{ErrInvalid, ErrUnsupported, errGeoSchema, errASNSchema}
	addrs := []netip.Addr{netip.MustParseAddr("192.0.2.7"), netip.MustParseAddr("0.0.0.0"), netip.MustParseAddr("255.255.255.255"), netip.MustParseAddr("2001:db8::7"), netip.MustParseAddr("::"), netip.MustParseAddr("ffff::1")}
	f.Fuzz(func(t *testing.T, content []byte) {
		for _, k := range Kinds {
			// parse, not Parse: a panic of the reader must show here.
			db, err := parse(k, content)
			if err != nil {
				if db != nil || !slices.Contains(fixed, err) {
					t.Fatalf("%s: %v", k, err)
				}
				continue
			}
			for _, a := range addrs {
				var err error
				if k == Geo {
					_, err = db.Geo(a)
				} else {
					_, err = db.ASN(a)
				}
				if err != nil && err != ErrLookup {
					t.Fatalf("%s, %s: %v", k, a, err)
				}
			}
		}
	})
}

// Package enrichment holds the MaxMind DB databases that the attributes geo
// and asn read: the strict reading of a database from its content, in
// memory, the records that tessera decodes, the lookups of an address, and
// the store from which each decision takes one snapshot of the databases.
//
// A database is read whole into memory and never changed: a new copy
// replaces it, atomically, in the Store, and the garbage collector frees the
// previous one once no decision holds it. A lookup reads memory only.
package enrichment

import (
	"errors"
	"io"
	"net/netip"
	"time"
	"unicode/utf8"

	"github.com/oschwald/maxminddb-golang/v2"
	"github.com/oschwald/maxminddb-golang/v2/mmdbdata"
)

// Kind is the kind of a database: the attribute that reads it.
type Kind string

// The kinds of a database.
const (
	// Geo is a database with the schema of GeoIP2 or GeoLite2 City or
	// Country, which geo reads.
	Geo Kind = "geo"
	// ASN is a database with the schema of GeoLite2 ASN, which asn reads.
	ASN Kind = "asn"
)

// Kinds are the kinds of a database, in the order of the configuration.
var Kinds = []Kind{Geo, ASN}

// The fixed ceilings of a database.
const (
	// MaxFileBytes bounds the content of a database.
	MaxFileBytes = 512 << 20
	// MaxTypeBytes bounds the database type that Type returns.
	MaxTypeBytes = 128
)

// The light checks of the content: the records of the first networks of the
// tree, among the first networks visited, empty ones included, are decoded.
const (
	probeRecords  = 16
	probeNetworks = 1024
)

// MaxBuildTime is the latest build time of a database: a later build_epoch
// in the metadata, which RFC 3339 cannot write, is taken as this time.
var MaxBuildTime = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// The problems of the content of a database. They never repeat the content.
var (
	ErrTooLarge    = errors.New("the file is larger than 512 MiB")
	ErrInvalid     = errors.New("the file is not a valid MMDB database")
	ErrUnsupported = errors.New("the MMDB database has an unsupported format: tessera reads binary format 2, IP version 4 or 6, and records of 24, 28 or 32 bits")
	errGeoSchema   = errors.New("a record of the database does not have the schema of a GeoIP2 City or Country database")
	errASNSchema   = errors.New("a record of the database does not have the schema of a GeoLite2 ASN database")
)

// ErrLookup is the error of a lookup that failed: the search tree or the
// record of the address is invalid, or the record does not have the schema
// of its kind. It is never a database without the address.
var ErrLookup = errors.New("the database cannot be read for this address")

// errUnknownKind is the error of a kind that tessera does not know.
var errUnknownKind = errors.New("enrichment: unknown kind of database")

// Database is an immutable copy of a database. It is safe for concurrent
// use.
type Database struct {
	kind     Kind
	reader   *maxminddb.Reader
	ipv4Only bool
	typ      string
	built    time.Time
}

// Kind returns the kind of the database.
func (d *Database) Kind() Kind { return d.kind }

// Type returns the type of the database, as its metadata names it, such as
// GeoLite2-City, cut to MaxTypeBytes at a UTF-8 boundary.
func (d *Database) Type() string { return d.typ }

// Built returns the build time of the database, from its metadata, in UTC,
// at most MaxBuildTime.
func (d *Database) Built() time.Time { return d.built }

// Read reads a database of kind k from r: at most MaxFileBytes, whose
// size is size, as far as the caller knows. Content larger than
// MaxFileBytes is ErrTooLarge, and invalid content an error of Parse; a
// failure of r is returned as it is.
func Read(k Kind, r io.Reader, size int64) (*Database, error) {
	return ReadAtMost(k, r, size, MaxFileBytes)
}

// ReadAtMost is Read with the ceiling maxBytes, at most MaxFileBytes, which
// the tests lower: content larger than maxBytes is still ErrTooLarge, whose
// text names the ceiling of Read.
func ReadAtMost(k Kind, r io.Reader, size, maxBytes int64) (*Database, error) {
	maxBytes = min(maxBytes, MaxFileBytes)
	data := make([]byte, 0, min(max(size, 0), maxBytes)+1)
	limited := io.LimitReader(r, maxBytes+1)
	for {
		if len(data) == cap(data) {
			data = append(data, 0)[:len(data)]
		}
		n, err := limited.Read(data[len(data):cap(data)])
		data = data[:len(data)+n]
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrTooLarge
	}
	return Parse(k, data)
}

// Parse reads a database of kind k from content, which it keeps: content
// must not change afterwards. It checks the metadata, and decodes the
// records of the first networks of the tree as the lookups do, without
// reading the whole database: at least one of them must have a key of the
// schema of the kind, so that a database of the other kind is rejected.
// Invalid content is ErrInvalid, ErrUnsupported, or the error of records
// without the schema of the kind.
func Parse(k Kind, content []byte) (db *Database, err error) {
	defer func() {
		// The content is a file of the operator, but a failure of the
		// reader of MMDB must not stop tessera: the file is invalid.
		if r := recover(); r != nil {
			db, err = nil, ErrInvalid
		}
	}()
	return parse(k, content)
}

func parse(k Kind, content []byte) (*Database, error) {
	if k != Geo && k != ASN {
		return nil, errUnknownKind
	}
	reader, err := maxminddb.OpenBytes(content)
	if err != nil {
		return nil, ErrInvalid
	}
	m := reader.Metadata
	if m.BinaryFormatMajorVersion != 2 || m.IPVersion != 4 && m.IPVersion != 6 || m.RecordSize != 24 && m.RecordSize != 28 && m.RecordSize != 32 {
		return nil, ErrUnsupported
	}
	built := MaxBuildTime
	if m.BuildEpoch <= uint(MaxBuildTime.Unix()) {
		built = time.Unix(int64(m.BuildEpoch), 0).UTC()
	}
	db := &Database{
		kind:     k,
		reader:   reader,
		ipv4Only: m.IPVersion == 4,
		typ:      cut(m.DatabaseType, MaxTypeBytes),
		built:    built,
	}
	// The tree is visited with the empty networks and the aliases of the
	// IPv4 networks, so that every path ends in a network: the work of
	// the probe is bounded whatever the tree.
	visited, decoded, ofTheSchema := 0, 0, false
	for res := range reader.Networks(maxminddb.IncludeAliasedNetworks(), maxminddb.IncludeNetworksWithoutData()) {
		if res.Err() != nil {
			return nil, ErrInvalid
		}
		if res.Found() {
			keys, err := db.decode(res)
			if err != nil {
				return nil, err
			}
			ofTheSchema = ofTheSchema || keys
			decoded++
		}
		visited++
		if decoded == probeRecords || visited == probeNetworks {
			break
		}
	}
	// Records that decode, but without a key of the schema, are those of a
	// database of the other kind, such as an ASN file configured as geo,
	// which would answer that no address is found.
	if decoded > 0 && !ofTheSchema {
		return nil, db.schemaError()
	}
	return db, nil
}

// decode decodes the record of res as a lookup of the kind of d does, and
// reports whether it has a key of the schema of the kind. A record of
// another schema is the schema error of the kind; any other failure is
// ErrInvalid.
func (d *Database) decode(res maxminddb.Result) (bool, error) {
	var err error
	var keys interface{ any() bool }
	if d.kind == Geo {
		var r geoRecord
		var k geoKeys
		if err = res.Decode(&r); err == nil {
			err, keys = res.Decode(&k), k
		}
	} else {
		var r asnRecord
		var k asnKeys
		if err = res.Decode(&r); err == nil {
			err, keys = res.Decode(&k), k
		}
	}
	var schema maxminddb.UnmarshalTypeError
	switch {
	case err == nil:
		return keys.any(), nil
	case errors.As(err, &schema):
		return false, d.schemaError()
	}
	return false, ErrInvalid
}

// schemaError is the error of records without the schema of the kind of d.
func (d *Database) schemaError() error {
	if d.kind == Geo {
		return errGeoSchema
	}
	return errASNSchema
}

// present is set in a record of keys when the record has the key, whatever
// its value, which it skips.
type present struct{}

func (*present) UnmarshalMaxMindDBCursor(c mmdbdata.Cursor) (mmdbdata.Cursor, error) {
	return c.Skip()
}

// geoKeys are the top-level keys of the schema of GeoIP2 City and Country.
type geoKeys struct {
	Continent          *present `maxminddb:"continent"`
	Country            *present `maxminddb:"country"`
	RegisteredCountry  *present `maxminddb:"registered_country"`
	RepresentedCountry *present `maxminddb:"represented_country"`
	City               *present `maxminddb:"city"`
	Location           *present `maxminddb:"location"`
	Subdivisions       *present `maxminddb:"subdivisions"`
	Postal             *present `maxminddb:"postal"`
}

func (k geoKeys) any() bool {
	return k.Continent != nil || k.Country != nil || k.RegisteredCountry != nil || k.RepresentedCountry != nil ||
		k.City != nil || k.Location != nil || k.Subdivisions != nil || k.Postal != nil
}

// asnKeys are the keys of the schema of GeoLite2 ASN.
type asnKeys struct {
	Number       *present `maxminddb:"autonomous_system_number"`
	Organization *present `maxminddb:"autonomous_system_organization"`
}

func (k asnKeys) any() bool { return k.Number != nil || k.Organization != nil }

// cut cuts s to n bytes, at a UTF-8 boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// englishName is the English name of a place, from its names in every
// language: the decoder skips the others.
type englishName struct {
	English string `maxminddb:"en"`
}

// geoRecord is what tessera decodes of a record of a geo database.
type geoRecord struct {
	City struct {
		Names englishName `maxminddb:"names"`
	} `maxminddb:"city"`
	Continent struct {
		Code  string      `maxminddb:"code"`
		Names englishName `maxminddb:"names"`
	} `maxminddb:"continent"`
	Country struct {
		ISOCode string      `maxminddb:"iso_code"`
		Names   englishName `maxminddb:"names"`
		InEU    bool        `maxminddb:"is_in_european_union"`
	} `maxminddb:"country"`
	Location struct {
		Latitude  *float64 `maxminddb:"latitude"`
		Longitude *float64 `maxminddb:"longitude"`
		TimeZone  string   `maxminddb:"time_zone"`
	} `maxminddb:"location"`
	Postal struct {
		Code string `maxminddb:"code"`
	} `maxminddb:"postal"`
	Subdivisions []struct {
		ISOCode string      `maxminddb:"iso_code"`
		Names   englishName `maxminddb:"names"`
	} `maxminddb:"subdivisions"`
}

// asnRecord is what tessera decodes of a record of an asn database.
type asnRecord struct {
	Number       uint32 `maxminddb:"autonomous_system_number"`
	Organization string `maxminddb:"autonomous_system_organization"`
}

// GeoRecord is what geo reads of the record of an address. An address that
// the database does not have has the zero GeoRecord.
type GeoRecord struct {
	// Found: the database has the address.
	Found bool
	// Country is the country of the address, and Continent its continent.
	Country   Country
	Continent Continent
	// Subdivisions are the subdivisions of the country, in the order of the
	// record, from the largest to the smallest; none without them.
	Subdivisions []Subdivision
	// City is the name of the city in English, PostalCode the postal code
	// as the database has it, and TimeZone the IANA time zone.
	City, PostalCode, TimeZone string
	// HasLocation: the record has both a latitude and a longitude.
	HasLocation         bool
	Latitude, Longitude float64
}

// Country is the country of a record.
type Country struct {
	// Code is the ISO 3166-1 alpha-2 code, such as IT, and Name the name
	// in English.
	Code, Name string
	// InEU: the country is in the European Union.
	InEU bool
}

// Continent is the continent of a record: its code, such as EU, and its
// name in English.
type Continent struct{ Code, Name string }

// Subdivision is a subdivision of a country: its code as the database has
// it, without the code of the country, such as 62 or RM, and its name in
// English.
type Subdivision struct{ Code, Name string }

// ASNRecord is what asn reads of the record of an address. An address that
// the database does not have has the zero ASNRecord.
type ASNRecord struct {
	Found        bool
	Number       int64
	Organization string
}

// lookup returns the record of addr, and whether the database has it. An
// IPv6 address is never in a database of IPv4 addresses only.
func (d *Database) lookup(addr netip.Addr) (maxminddb.Result, bool, error) {
	addr = addr.Unmap()
	if !addr.IsValid() || d.ipv4Only && addr.Is6() {
		return maxminddb.Result{}, false, nil
	}
	res := d.reader.Lookup(addr.WithZone(""))
	if res.Err() != nil {
		return res, false, ErrLookup
	}
	return res, res.Found(), nil
}

// Geo looks addr up in d, a database of the kind Geo. An IPv4-mapped IPv6
// address is looked up as the IPv4 address it maps.
func (d *Database) Geo(addr netip.Addr) (GeoRecord, error) {
	if d.kind != Geo {
		return GeoRecord{}, errUnknownKind
	}
	res, found, err := d.lookup(addr)
	if err != nil || !found {
		return GeoRecord{}, err
	}
	var r geoRecord
	if err := res.Decode(&r); err != nil {
		return GeoRecord{}, ErrLookup
	}
	out := GeoRecord{
		Found:      true,
		Country:    Country{Code: r.Country.ISOCode, Name: r.Country.Names.English, InEU: r.Country.InEU},
		Continent:  Continent{Code: r.Continent.Code, Name: r.Continent.Names.English},
		City:       r.City.Names.English,
		PostalCode: r.Postal.Code,
		TimeZone:   r.Location.TimeZone,
	}
	if len(r.Subdivisions) > 0 {
		out.Subdivisions = make([]Subdivision, len(r.Subdivisions))
		for i, s := range r.Subdivisions {
			out.Subdivisions[i] = Subdivision{Code: s.ISOCode, Name: s.Names.English}
		}
	}
	if r.Location.Latitude != nil && r.Location.Longitude != nil {
		out.HasLocation, out.Latitude, out.Longitude = true, *r.Location.Latitude, *r.Location.Longitude
	}
	return out, nil
}

// ASN looks addr up in d, a database of the kind ASN. An IPv4-mapped IPv6
// address is looked up as the IPv4 address it maps.
func (d *Database) ASN(addr netip.Addr) (ASNRecord, error) {
	if d.kind != ASN {
		return ASNRecord{}, errUnknownKind
	}
	res, found, err := d.lookup(addr)
	if err != nil || !found {
		return ASNRecord{}, err
	}
	var r asnRecord
	if err := res.Decode(&r); err != nil {
		return ASNRecord{}, ErrLookup
	}
	return ASNRecord{Found: true, Number: int64(r.Number), Organization: r.Organization}, nil
}

package expr

import (
	"errors"
	"fmt"
	"reflect"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/interpreter"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/request"
	"github.com/gtriggiano/tessera/internal/sets"
	"github.com/gtriggiano/tessera/internal/useragent"
)

// Input is the activation of one request: it answers the variables request,
// client, sets, geo, asn and ua. It builds the CEL value of an attribute when
// a condition first reads it, and keeps it for the other conditions evaluated
// with it, so one Input serves every rule of one decision, and every
// expression evaluated after it: geo and asn look the client address up
// once, and ua analyzes the user-agent header once. It is not safe for
// concurrent use.
type Input struct {
	request requestVal
	client  clientVal
	sets    setsVal
	// dbs are the databases of the decision; geo and asn are nil until an
	// expression reads them, then their value or the error of their
	// lookup.
	dbs      *enrichment.Snapshot
	geo, asn ref.Val
	// userAgent analyzes the user-agent header; ua is nil until an
	// expression reads it, then its value or the error of its analysis.
	userAgent analyzer
	ua        ref.Val
	// lib is the library of the conditions evaluated with the Input, which
	// reports a panic recovered in a lookup.
	lib *library
	// rule is the rule being evaluated, which a recovered panic reports.
	rule string
}

// analyzer analyzes a user-agent header, as a *useragent.Analyzer does.
type analyzer interface {
	Analyze(header string) useragent.Record
}

// NewInput returns the activation of r, whose ClientIP is the client
// address, or the zero Addr when it is missing, of snap, the state of the
// sets for the whole decision, of dbs, the databases for the whole decision,
// and of ua, the analysis of the user-agent header. snap is a Snapshot of the
// Store whose declarations the Env was built with, or nil, in which every set
// is never loaded; dbs is a Snapshot of the store of the databases that the
// Env declares, or nil, which has none; ua is the Analyzer of the process, or
// nil, which analyzes without a cache. r must not change while the Input is
// in use.
func NewInput(r *request.Request, snap *sets.Snapshot, dbs *enrichment.Snapshot, ua *useragent.Analyzer) *Input {
	in := &Input{dbs: dbs, userAgent: ua}
	in.request = requestVal{object: object{requestType}, req: r, in: in}
	in.client = clientVal{object: object{clientType}, in: in}
	if r.ClientIP.IsValid() {
		in.client.ip = ip{addr: r.ClientIP.Unmap(), in: in}
		in.client.hasIP = true
	}
	in.sets = setsVal{object: object{setsType}, in: in, snap: snap}
	return in
}

// SetRule names the rule whose condition is evaluated next with in: a panic
// recovered meanwhile is reported with it.
func (in *Input) SetRule(name string) { in.rule = name }

// ResolveName implements interpreter.Activation.
func (in *Input) ResolveName(name string) (any, bool) {
	switch name {
	case "request":
		return &in.request, true
	case "client":
		return &in.client, true
	case "sets":
		return &in.sets, true
	case "geo":
		if in.geo == nil {
			in.geo = in.lookup(enrichment.Geo)
		}
		return in.geo, true
	case "asn":
		if in.asn == nil {
			in.asn = in.lookup(enrichment.ASN)
		}
		return in.asn, true
	case "ua":
		if in.ua == nil {
			in.ua = in.analyzeUserAgent()
		}
		return in.ua, true
	}
	return nil, false
}

// Parent implements interpreter.Activation.
func (in *Input) Parent() interpreter.Activation { return nil }

// ErrClientIPMissing is the error of a read of client.ip, geo or asn when
// the request has no client address.
var ErrClientIPMissing = errors.New("client.ip is not available")

// ErrDatabase is the error of a read of geo or asn whose lookup failed: the
// database of the decision cannot be read for the client address.
var ErrDatabase = errors.New("the database cannot be read for the client address")

// lookup looks the client address up in the database of kind k, and returns
// the value of geo or asn, or, when the request has no client address or
// the lookup fails, an error: so that every read of the variable fails,
// has() included.
func (in *Input) lookup(k enrichment.Kind) (out ref.Val) {
	if !in.client.hasIP {
		return types.WrapErr(ErrClientIPMissing)
	}
	db := in.dbs.Database(k)
	if db == nil {
		return types.WrapErr(ErrDatabase)
	}
	defer func() {
		if r := recover(); r != nil {
			out = types.WrapErr(in.recovered(r))
		}
	}()
	addr := in.client.ip.addr
	if k == enrichment.Geo {
		rec, err := db.Geo(addr)
		if err != nil {
			return types.WrapErr(ErrDatabase)
		}
		g := &geoVal{object: object{geoType}, in: in, rec: rec}
		g.country = countryVal{object: object{geoCountryType}, in: in, c: rec.Country}
		g.continent = continentVal{object: object{geoContinentType}, in: in, c: rec.Continent}
		g.location = pointVal{object: object{geoPointType}, in: in, lat: rec.Latitude, lon: rec.Longitude}
		return g
	}
	rec, err := db.ASN(addr)
	if err != nil {
		return types.WrapErr(ErrDatabase)
	}
	return &asnVal{object: object{asnType}, in: in, rec: rec}
}

// recovered reports r, a panic recovered while the value of a variable was
// built, with the library of the expressions evaluated with in, and returns it.
func (in *Input) recovered(r any) *Panic {
	if in.lib != nil {
		return in.lib.recovered(r, in)
	}
	return &Panic{Rule: in.rule, Value: r}
}

// analyzeUserAgent analyzes the user-agent header of the request, and returns
// the value of ua, or, when the analysis panics, an error, so that every read
// of the variable fails, has() included. The analysis itself never fails.
func (in *Input) analyzeUserAgent() (out ref.Val) {
	defer func() {
		if r := recover(); r != nil {
			out = types.WrapErr(in.recovered(r))
		}
	}()
	rec := in.userAgent.Analyze(in.request.req.Headers["user-agent"])
	u := &userAgentVal{object: object{userAgentType}, in: in, rec: rec}
	u.browser = browserVal{object: object{userAgentBrowserType}, in: in, b: rec.Browser}
	u.os = osVal{object: object{userAgentOSType}, in: in, os: rec.OS}
	u.bot = botVal{object: object{userAgentBotType}, in: in, b: rec.Bot}
	return u
}

// Geo returns what geo reads for the request: the record of the client
// address in the geo database of the decision, looked up once for the
// Input. The error is ErrClientIPMissing, ErrDatabase or a *Panic.
func (in *Input) Geo() (enrichment.GeoRecord, error) {
	v, _ := in.ResolveName("geo")
	if g, ok := v.(*geoVal); ok {
		return g.rec, nil
	}
	return enrichment.GeoRecord{}, valueError(v)
}

// ASN returns what asn reads for the request, as Geo does for geo.
func (in *Input) ASN() (enrichment.ASNRecord, error) {
	v, _ := in.ResolveName("asn")
	if a, ok := v.(*asnVal); ok {
		return a.rec, nil
	}
	return enrichment.ASNRecord{}, valueError(v)
}

// UserAgent returns what ua reads for the request: the record of its
// user-agent header, analyzed once for the Input. The error is a *Panic.
func (in *Input) UserAgent() (useragent.Record, error) {
	v, _ := in.ResolveName("ua")
	if u, ok := v.(*userAgentVal); ok {
		return u.rec, nil
	}
	return useragent.Record{}, valueError(v)
}

// valueError returns the error that an error value wraps.
func valueError(v any) error {
	if e, ok := v.(*types.Err); ok {
		if inner := e.Unwrap(); inner != nil {
			return inner
		}
		return e
	}
	return ErrDatabase
}

// geoVal is the value of the variable geo: the record of the client address
// in the geo database. Its objects are part of it, and the list of its
// subdivisions is built when an expression first reads it.
type geoVal struct {
	object
	in           *Input
	rec          enrichment.GeoRecord
	country      countryVal
	continent    continentVal
	location     pointVal
	subdivisions ref.Val
}

func (g *geoVal) Equal(other ref.Val) ref.Val { return types.Bool(g == other) }
func (g *geoVal) Value() any                  { return g }

// geoString declares a string field of geo.
func geoString(pick func(r *enrichment.GeoRecord) string) field {
	return field{
		typ:   types.StringType,
		isSet: always,
		get:   func(obj any) (any, error) { return types.String(pick(&obj.(*geoVal).rec)), nil },
	}
}

// errNoLocation is the error of a read of geo.location when the record has
// no location: an ordinary evaluation error, as a missing map key is.
var errNoLocation = errors.New("geo.location is not set")

var geoFields = map[string]field{
	"found": {
		typ:   types.BoolType,
		isSet: always,
		get:   func(obj any) (any, error) { return types.Bool(obj.(*geoVal).rec.Found), nil },
	},
	"country": {
		typ:   geoCountryType,
		isSet: always,
		get:   func(obj any) (any, error) { return &obj.(*geoVal).country, nil },
	},
	"continent": {
		typ:   geoContinentType,
		isSet: always,
		get:   func(obj any) (any, error) { return &obj.(*geoVal).continent, nil },
	},
	"subdivisions": {
		typ:   types.NewListType(geoSubdivisionType),
		isSet: always,
		get: func(obj any) (any, error) {
			g := obj.(*geoVal)
			if g.subdivisions == nil {
				vals := make([]subdivisionVal, len(g.rec.Subdivisions))
				list := make([]ref.Val, len(vals))
				for i, s := range g.rec.Subdivisions {
					vals[i] = subdivisionVal{object: object{geoSubdivisionType}, in: g.in, s: s}
					list[i] = &vals[i]
				}
				g.subdivisions = types.NewRefValList(types.DefaultTypeAdapter, list)
			}
			return g.subdivisions, nil
		},
	},
	"city":        geoString(func(r *enrichment.GeoRecord) string { return r.City }),
	"postal_code": geoString(func(r *enrichment.GeoRecord) string { return r.PostalCode }),
	"timezone":    geoString(func(r *enrichment.GeoRecord) string { return r.TimeZone }),
	"location": {
		typ:   geoPointType,
		isSet: func(obj any) bool { return obj.(*geoVal).rec.HasLocation },
		get: func(obj any) (any, error) {
			g := obj.(*geoVal)
			if !g.rec.HasLocation {
				return nil, errNoLocation
			}
			return &g.location, nil
		},
	},
}

// stringField declares a string field of an object whose Go value is a *T.
func stringField[T any](pick func(v *T) string) field {
	return field{
		typ:   types.StringType,
		isSet: always,
		get:   func(obj any) (any, error) { return types.String(pick(obj.(*T))), nil },
	}
}

// countryVal is the value of a tessera.GeoCountry. Two values with the same
// fields are equal.
type countryVal struct {
	object
	in *Input
	c  enrichment.Country
}

func (c *countryVal) Equal(other ref.Val) ref.Val {
	o, ok := other.(*countryVal)
	return types.Bool(ok && c.c == o.c)
}
func (c *countryVal) Value() any { return c }

var countryFields = map[string]field{
	"code": stringField(func(c *countryVal) string { return c.c.Code }),
	"name": stringField(func(c *countryVal) string { return c.c.Name }),
	"in_eu": {
		typ:   types.BoolType,
		isSet: always,
		get:   func(obj any) (any, error) { return types.Bool(obj.(*countryVal).c.InEU), nil },
	},
}

// continentVal is the value of a tessera.GeoContinent. Two values with the
// same fields are equal.
type continentVal struct {
	object
	in *Input
	c  enrichment.Continent
}

func (c *continentVal) Equal(other ref.Val) ref.Val {
	o, ok := other.(*continentVal)
	return types.Bool(ok && c.c == o.c)
}
func (c *continentVal) Value() any { return c }

var continentFields = map[string]field{
	"code": stringField(func(c *continentVal) string { return c.c.Code }),
	"name": stringField(func(c *continentVal) string { return c.c.Name }),
}

// subdivisionVal is the value of a tessera.GeoSubdivision. Two values with
// the same fields are equal.
type subdivisionVal struct {
	object
	in *Input
	s  enrichment.Subdivision
}

func (s *subdivisionVal) Equal(other ref.Val) ref.Val {
	o, ok := other.(*subdivisionVal)
	return types.Bool(ok && s.s == o.s)
}
func (s *subdivisionVal) Value() any { return s }

var subdivisionFields = map[string]field{
	"code": stringField(func(s *subdivisionVal) string { return s.s.Code }),
	"name": stringField(func(s *subdivisionVal) string { return s.s.Name }),
}

// pointVal is the value of a tessera.GeoPoint: a latitude and a longitude.
// Two values with the same coordinates are equal.
type pointVal struct {
	object
	in       *Input
	lat, lon float64
}

func (p *pointVal) Equal(other ref.Val) ref.Val {
	o, ok := other.(*pointVal)
	return types.Bool(ok && p.lat == o.lat && p.lon == o.lon)
}
func (p *pointVal) Value() any { return p }

var pointFields = map[string]field{
	"lat": {typ: types.DoubleType, isSet: always, get: func(obj any) (any, error) { return types.Double(obj.(*pointVal).lat), nil }},
	"lon": {typ: types.DoubleType, isSet: always, get: func(obj any) (any, error) { return types.Double(obj.(*pointVal).lon), nil }},
}

// asnVal is the value of the variable asn: the record of the client address
// in the asn database.
type asnVal struct {
	object
	in  *Input
	rec enrichment.ASNRecord
}

func (a *asnVal) Equal(other ref.Val) ref.Val { return types.Bool(a == other) }
func (a *asnVal) Value() any                  { return a }

var asnFields = map[string]field{
	"found": {
		typ:   types.BoolType,
		isSet: always,
		get:   func(obj any) (any, error) { return types.Bool(obj.(*asnVal).rec.Found), nil },
	},
	"number": {
		typ:   types.IntType,
		isSet: always,
		get:   func(obj any) (any, error) { return types.Int(obj.(*asnVal).rec.Number), nil },
	},
	"organization": {
		typ:   types.StringType,
		isSet: always,
		get:   func(obj any) (any, error) { return types.String(obj.(*asnVal).rec.Organization), nil },
	},
}

// userAgentVal is the value of the variable ua: the record of the
// user-agent header of the request. Its objects are part of it.
type userAgentVal struct {
	object
	in      *Input
	rec     useragent.Record
	browser browserVal
	os      osVal
	bot     botVal
}

func (u *userAgentVal) Equal(other ref.Val) ref.Val { return types.Bool(u == other) }
func (u *userAgentVal) Value() any                  { return u }

var userAgentFields = map[string]field{
	"browser": {
		typ:   userAgentBrowserType,
		isSet: always,
		get:   func(obj any) (any, error) { return &obj.(*userAgentVal).browser, nil },
	},
	"os": {
		typ:   userAgentOSType,
		isSet: always,
		get:   func(obj any) (any, error) { return &obj.(*userAgentVal).os, nil },
	},
	"device": stringField(func(u *userAgentVal) string { return u.rec.Device }),
	"is_bot": {
		typ:   types.BoolType,
		isSet: always,
		get:   func(obj any) (any, error) { return types.Bool(obj.(*userAgentVal).rec.IsBot), nil },
	},
	"bot": {
		typ:   userAgentBotType,
		isSet: always,
		get:   func(obj any) (any, error) { return &obj.(*userAgentVal).bot, nil },
	},
}

// intField declares an int field of an object whose Go value is a *T.
func intField[T any](pick func(v *T) int64) field {
	return field{
		typ:   types.IntType,
		isSet: always,
		get:   func(obj any) (any, error) { return types.Int(pick(obj.(*T))), nil },
	}
}

// browserVal is the value of a tessera.UserAgentBrowser. Two values with the
// same fields are equal.
type browserVal struct {
	object
	in *Input
	b  useragent.Browser
}

func (b *browserVal) Equal(other ref.Val) ref.Val {
	o, ok := other.(*browserVal)
	return types.Bool(ok && b.b == o.b)
}
func (b *browserVal) Value() any { return b }

var browserFields = map[string]field{
	"name":    stringField(func(b *browserVal) string { return b.b.Name }),
	"version": stringField(func(b *browserVal) string { return b.b.Version }),
	"major":   intField(func(b *browserVal) int64 { return b.b.Major }),
	"minor":   intField(func(b *browserVal) int64 { return b.b.Minor }),
}

// osVal is the value of a tessera.UserAgentOS. Two values with the same
// fields are equal.
type osVal struct {
	object
	in *Input
	os useragent.OS
}

func (o *osVal) Equal(other ref.Val) ref.Val {
	v, ok := other.(*osVal)
	return types.Bool(ok && o.os == v.os)
}
func (o *osVal) Value() any { return o }

var osFields = map[string]field{
	"name":    stringField(func(o *osVal) string { return o.os.Name }),
	"version": stringField(func(o *osVal) string { return o.os.Version }),
	"major":   intField(func(o *osVal) int64 { return o.os.Major }),
	"minor":   intField(func(o *osVal) int64 { return o.os.Minor }),
}

// botVal is the value of a tessera.UserAgentBot. Two values with the same
// fields are equal.
type botVal struct {
	object
	in *Input
	b  useragent.Bot
}

func (b *botVal) Equal(other ref.Val) ref.Val {
	o, ok := other.(*botVal)
	return types.Bool(ok && b.b == o.b)
}
func (b *botVal) Value() any { return b }

var botFields = map[string]field{
	"name":        stringField(func(b *botVal) string { return b.b.Name }),
	"description": stringField(func(b *botVal) string { return b.b.Description }),
}

// requestVal is the value of the variable request.
type requestVal struct {
	object
	in                                    *Input
	req                                   *request.Request
	host, method, path, query, scheme, id ref.Val
	headers                               ref.Val
}

func (r *requestVal) Equal(other ref.Val) ref.Val { return types.Bool(r == other) }
func (r *requestVal) Value() any                  { return r }

func always(any) bool { return true }

// requestString declares a string field of request: pick returns where its
// CEL value is kept and the Go string it is built from.
func requestString(pick func(r *requestVal) (*ref.Val, string)) field {
	return field{
		typ:   types.StringType,
		isSet: always,
		get: func(obj any) (any, error) {
			kept, s := pick(obj.(*requestVal))
			if *kept == nil {
				*kept = types.String(s)
			}
			return *kept, nil
		},
	}
}

var requestFields = map[string]field{
	"host":   requestString(func(r *requestVal) (*ref.Val, string) { return &r.host, r.req.Host }),
	"method": requestString(func(r *requestVal) (*ref.Val, string) { return &r.method, r.req.Method }),
	"path":   requestString(func(r *requestVal) (*ref.Val, string) { return &r.path, r.req.Path }),
	"query":  requestString(func(r *requestVal) (*ref.Val, string) { return &r.query, r.req.Query }),
	"scheme": requestString(func(r *requestVal) (*ref.Val, string) { return &r.scheme, r.req.Scheme }),
	"id":     requestString(func(r *requestVal) (*ref.Val, string) { return &r.id, r.req.ID }),
	"headers": {
		typ:   types.NewMapType(types.StringType, types.StringType),
		isSet: always,
		get: func(obj any) (any, error) {
			r := obj.(*requestVal)
			if r.headers == nil {
				r.headers = types.NewStringStringMap(types.DefaultTypeAdapter, r.req.Headers)
			}
			return r.headers, nil
		},
	},
}

// clientVal is the value of the variable client. client.ip is set when the
// request has a client address.
type clientVal struct {
	object
	in    *Input
	ip    ip
	hasIP bool
}

func (c *clientVal) Equal(other ref.Val) ref.Val { return types.Bool(c == other) }
func (c *clientVal) Value() any                  { return c }

var clientFields = map[string]field{
	"ip": {
		typ:   ipType,
		isSet: func(obj any) bool { return obj.(*clientVal).hasIP },
		get: func(obj any) (any, error) {
			c := obj.(*clientVal)
			if !c.hasIP {
				return nil, ErrClientIPMissing
			}
			return &c.ip, nil
		},
	},
}

// setsVal is the value of the variable sets: one field per declared set,
// read from the snapshot of the decision. It builds the value of a set when a
// condition first reads it, and keeps it for the rest of the decision.
type setsVal struct {
	object
	in   *Input
	snap *sets.Snapshot
	vals []setVal
}

func (s *setsVal) Equal(other ref.Val) ref.Val { return types.Bool(s == other) }
func (s *setsVal) Value() any                  { return s }

// setField declares sets.<name> for d, the declaration of index i among n.
func setField(i, n int, d sets.Declaration) (field, error) {
	typ, ok := setTypes[d.Type]
	if !ok {
		return field{}, fmt.Errorf("expr: set %d: unknown set type", i)
	}
	switch d.OnUnavailable {
	case sets.Fail, sets.Match, sets.NoMatch:
	default:
		return field{}, fmt.Errorf("expr: set %d: unknown answer for an unavailable set", i)
	}
	return field{
		typ:   typ,
		isSet: always,
		get: func(obj any) (any, error) {
			s := obj.(*setsVal)
			if s.vals == nil {
				s.vals = make([]setVal, n)
			}
			v := &s.vals[i]
			if v.typ == nil {
				*v = setVal{typ: typ, entry: s.snap.Entry(i), on: d.OnUnavailable, in: s.in}
			}
			return v, nil
		},
	}, nil
}

// setVal is the value of one set in one decision: its copy in the snapshot,
// its availability, and what contains answers when it is unavailable.
type setVal struct {
	typ   *types.Type
	entry sets.Entry
	on    sets.OnUnavailable
	in    *Input
}

func (v *setVal) ConvertToNative(typeDesc reflect.Type) (any, error) {
	return nil, fmt.Errorf("unsupported conversion of %s to %v", v.typ, typeDesc)
}

func (v *setVal) ConvertToType(typeValue ref.Type) ref.Val {
	if typeValue == types.TypeType {
		return v.typ
	}
	return types.NewErr("type conversion error from '%s' to '%s'", v.typ, typeValue)
}

func (v *setVal) Equal(other ref.Val) ref.Val { return types.Bool(v == other) }
func (v *setVal) Type() ref.Type              { return v.typ }
func (v *setVal) Value() any                  { return v }

// ErrSetUnavailable is the error of contains on an unavailable set whose
// on_unavailable is fail.
var ErrSetUnavailable = errors.New("the set is not available")

// inUse returns the copy of the set to look a value up in, or, when the set
// is unavailable, nil and what on_unavailable makes contains answer.
func (v *setVal) inUse() (*sets.Set, ref.Val) {
	if v.entry.Available && v.entry.Set != nil {
		return v.entry.Set, nil
	}
	switch v.on {
	case sets.Match:
		return nil, types.True
	case sets.NoMatch:
		return nil, types.False
	}
	return nil, types.WrapErr(ErrSetUnavailable)
}

package area

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// The fixed ceilings of a document.
const (
	// MaxPositions bounds the positions of a document, the closing position
	// of each ring included.
	MaxPositions = 10_000_000
	// MaxDepth bounds the nesting of the arrays and objects of a document.
	MaxDepth = 64
	// MaxNumber bounds the characters of a number of a position, which Read
	// keeps to parse it.
	MaxNumber = 64
)

// Error is a document that Read rejects: the line and the column, from 1,
// where the problem is, the column counted in characters; the place of the
// problem in the structure of the document, such as "feature 3, polygon 1,
// ring 2", or empty; and the problem. It never repeats the content of the
// document.
type Error struct {
	Line, Column int
	Path         string
	Problem      string
}

func (e *Error) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("line %d, column %d: %s: %s", e.Line, e.Column, e.Path, e.Problem)
	}
	return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.Problem)
}

// The problems of a document. A problem of JSON has no place in the
// structure. None repeats the content: the names they hold are those of the
// GeoJSON specification.
const (
	problemEmpty      = "the content is empty"
	problemValue      = "invalid JSON: expected a value"
	problemEnd        = "invalid JSON: unexpected end of the content"
	problemArraySep   = "invalid JSON: expected , or ]"
	problemObjectSep  = "invalid JSON: expected , or }"
	problemColon      = "invalid JSON: expected :"
	problemName       = "invalid JSON: expected a member name in double quotes"
	problemNumber     = "invalid JSON: invalid number"
	problemEscape     = "invalid JSON: invalid escape in a string"
	problemControl    = "invalid JSON: control character in a string"
	problemAfter      = "invalid JSON: content after the document"
	problemUTF8       = "the content is not valid UTF-8"
	problemDuplicate  = "a member name is repeated in an object"
	problemObject     = "expected a GeoJSON object"
	problemType       = "the type member is missing"
	problemTypeString = "the type must be a string"
	problemTypeName   = "unknown GeoJSON type"
	problemFeatures   = "the features member must be an array"
	problemNoFeatures = "the features member is missing"
	problemNoGeometry = "the geometry member is missing"
	problemNull       = "the geometry is null"
	problemNoCoords   = "the coordinates member is missing"
	problemNoRings    = "the polygon has no rings"
	problemNoPolygons = "the MultiPolygon has no polygons"
	problemShortRing  = "the ring has fewer than 4 positions"
	problemOpenRing   = "the ring is not closed: its last position differs from its first"
	problemShortPos   = "the position has fewer than 2 numbers"
	problemLongPos    = "the position has more than 3 numbers"
	problemNotNumber  = "the position holds a value that is not a number"
	problemRange      = "the number is beyond the range of a double"
	problemLongitude  = "the longitude is outside -180 to 180"
	problemLatitude   = "the latitude is outside -90 to 90"
	problemAntimer    = "two consecutive positions are more than 180 degrees of longitude apart: split the polygon at the antimeridian"
	problemPolygons   = "more polygons than max_entries"
)

// The problems of a fixed ceiling, which name it.
var (
	problemDepth     = fmt.Sprintf("the JSON is nested more than %d levels deep", MaxDepth)
	problemPositions = fmt.Sprintf("more than %d positions", MaxPositions)
)

// kind is the type of a GeoJSON object, as its type member names it.
type kind uint8

const (
	kindNone kind = iota // no type member read
	kindFeatureCollection
	kindFeature
	kindPolygon
	kindMultiPolygon
	kindPoint
	kindMultiPoint
	kindLineString
	kindMultiLineString
	kindGeometryCollection
	kindUnknown
)

// kindNames are the names of the kinds, as the type member writes them.
var kindNames = [...]string{
	kindFeatureCollection:  "FeatureCollection",
	kindFeature:            "Feature",
	kindPolygon:            "Polygon",
	kindMultiPolygon:       "MultiPolygon",
	kindPoint:              "Point",
	kindMultiPoint:         "MultiPoint",
	kindLineString:         "LineString",
	kindMultiLineString:    "MultiLineString",
	kindGeometryCollection: "GeometryCollection",
}

// maxKindName bounds the bytes of a type, or of a member name of a GeoJSON
// object, that Read keeps to recognize it: longer than any name of
// kindNames and than any member that Read reads.
const maxKindName = 32

func kindOf(name []byte) kind {
	for k, n := range kindNames {
		if n != "" && string(name) == n {
			return kind(k)
		}
	}
	return kindUnknown
}

// context is where a GeoJSON object is: the document, an item of the
// features of a FeatureCollection, or the geometry of a Feature.
type context uint8

const (
	inDocument context = iota
	inFeatures
	inGeometry
)

// allowedKind reports whether an object of kind k is allowed in c.
func allowedKind(c context, k kind) bool {
	switch c {
	case inFeatures:
		return k == kindFeature
	case inGeometry:
		return k == kindPolygon || k == kindMultiPolygon
	}
	return k <= kindMultiPolygon
}

// kindNotAllowed is the problem of an object of kind k in c.
func kindNotAllowed(c context, k kind) string {
	switch c {
	case inFeatures:
		return "a " + kindNames[k] + " is not allowed: an item of features must be a Feature"
	case inGeometry:
		return "a " + kindNames[k] + " is not allowed: a geometry must be a Polygon or a MultiPolygon"
	}
	return "a " + kindNames[k] + " is not allowed: the document must be a FeatureCollection, a Feature, a Polygon or a MultiPolygon"
}

// The members that Read reads: each belongs to one kind of object.
const (
	memberFeatures    = "features"
	memberGeometry    = "geometry"
	memberCoordinates = "coordinates"
)

// memberNotAllowed is the problem of the member in an object of kind name.
func memberNotAllowed(member, name string) string {
	return "the " + member + " member is not allowed in a " + name
}

// shapeOf is the problem of coordinates that do not have the shape of a
// Polygon, of depth 3, or of a MultiPolygon, of depth 4; or of either, when
// depth is 0.
func shapeOf(depth int) string {
	switch depth {
	case 3:
		return "the coordinates do not have the shape of a Polygon"
	case 4:
		return "the coordinates do not have the shape of a MultiPolygon"
	}
	return "the coordinates do not have the shape of a Polygon or a MultiPolygon"
}

// failure is the value that the parser panics with on an invalid document,
// an *Error, or on a failure of its reader, which Read recovers.
type failure struct{ err error }

// position is a place in the content: a line and a column, from 1.
type position struct{ line, col int }

// byteOrderMark is the UTF-8 byte order mark, which Read ignores at the start
// of the content.
const byteOrderMark = "\uFEFF"

// bufferSize is the size of the buffer of the content: what Read holds of the
// document at once, besides the copy it builds.
const bufferSize = 64 << 10

// Read builds the polygons of an area set from a GeoJSON document, read from
// r, as RFC 7946 describes it, more strictly:
//
//   - the document is a FeatureCollection, a Feature, a Polygon or a
//     MultiPolygon, and the geometry of a Feature is a Polygon or a
//     MultiPolygon, never null; a FeatureCollection with no features is an
//     empty set;
//   - every ring is closed and has at least 4 positions; a position has 2
//     numbers, longitude from -180 to 180 and latitude from -90 to 90, and
//     an optional third, which Read ignores; two consecutive positions are at
//     most 180 degrees of longitude apart;
//   - the content is JSON, in UTF-8, nested at most MaxDepth levels; member
//     names are case-sensitive. A GeoJSON object has the members of its type
//     only, among features, geometry and coordinates, and none of them, nor
//     type, twice; any other member, such as properties, bbox, crs or id, is
//     read as JSON and ignored, without checking its names for repeats, so
//     that Read keeps no name of what it ignores. A UTF-8 byte order mark at
//     the start is ignored.
//
// It reads the content once, from start to end, keeping only the copy that
// it builds, and texts of fixed bounds: a type or a member name of a GeoJSON
// object to recognize it, at most maxKindName bytes, which no name it knows
// reaches, and a number of a position, at most MaxNumber characters. More
// than maxPolygons polygons, or more than MaxPositions positions, stop it at
// once. The orientation of the rings, their
// intersections and the position of the holes are not checked.
//
// An invalid document is an *Error; a failure of r is the error of r.
func Read(r io.Reader, maxPolygons int) (*Polygons, error) {
	return read(r, maxPolygons, MaxPositions)
}

func read(r io.Reader, maxPolygons, maxPositions int) (a *Polygons, err error) {
	p := &parser{
		r: r, buf: make([]byte, bufferSize), line: 1, col: 1,
		maxPolygons: maxPolygons, maxPositions: maxPositions,
	}
	p.skipMember = func(position) { p.skipValue() }
	p.skipItem = func(int, position) { p.skipValue() }
	defer func() {
		if v := recover(); v != nil {
			f, ok := v.(failure)
			if !ok {
				panic(v)
			}
			a, err = nil, f.err
		}
	}()
	p.document()
	return p.finish(), nil
}

// parser reads a document, and builds its polygons.
type parser struct {
	r        io.Reader
	buf      []byte
	pos, end int  // the unread content is buf[pos:end]
	eof      bool // r has no more content
	empty    int  // consecutive reads of r that returned nothing
	// line and col are the position of buf[pos].
	line, col int

	depth int    // the open arrays and objects
	name  []byte // the name of the member being read, in a GeoJSON object
	text  []byte // the type being read
	num   []byte // the number being read

	skipMember func(position)
	skipItem   func(int, position)

	maxPolygons, maxPositions int
	positions                 int // read so far
	feature                   int // the feature being read, from 1, in a FeatureCollection

	// The ring being read: where its vertices start, its first position,
	// its box, and the position that the last number completed.
	ringStart int
	first     point
	box       box
	lon, lat  float64
	prevLon   float64

	// The arrays of the copy being built, in the order and the form of
	// those of Polygons, without their final entries.
	points   chunks[point]
	rings    chunks[uint32]
	boxes    chunks[box]
	polygons chunks[uint32]
}

func (p *parser) fail(at position, path, problem string) {
	panic(failure{&Error{Line: at.line, Column: at.col, Path: path, Problem: problem}})
}

func (p *parser) here() position { return position{p.line, p.col} }

// more reads more content after the unread bytes, and reports whether it got
// any.
func (p *parser) more() bool {
	if p.eof {
		return false
	}
	if p.pos > 0 {
		n := copy(p.buf, p.buf[p.pos:p.end])
		p.pos, p.end = 0, n
	}
	for {
		n, err := p.r.Read(p.buf[p.end:])
		p.end += n
		switch {
		case err == io.EOF:
			p.eof = true
			return n > 0
		case err != nil:
			panic(failure{err})
		case n > 0:
			p.empty = 0
			return true
		}
		if p.empty++; p.empty >= 100 {
			panic(failure{io.ErrNoProgress})
		}
	}
}

// ensure reports whether n bytes are unread, reading more if needed.
func (p *parser) ensure(n int) bool {
	for p.end-p.pos < n {
		if !p.more() {
			return false
		}
	}
	return true
}

// peek returns the next byte, and false at the end of the content.
func (p *parser) peek() (byte, bool) {
	if p.pos == p.end && !p.more() {
		return 0, false
	}
	return p.buf[p.pos], true
}

// next returns the next byte, and fails at the end of the content.
func (p *parser) next() byte {
	c, ok := p.peek()
	if !ok {
		p.fail(p.here(), "", problemEnd)
	}
	return c
}

// skip consumes n bytes of one line, which are n characters.
func (p *parser) skip(n int) {
	p.pos += n
	p.col += n
}

// skipSpace consumes the spaces of JSON.
func (p *parser) skipSpace() {
	for {
		for ; p.pos < p.end; p.pos++ {
			switch p.buf[p.pos] {
			case ' ', '\t', '\r':
				p.col++
			case '\n':
				p.line++
				p.col = 1
			default:
				return
			}
		}
		if !p.more() {
			return
		}
	}
}

// valueStart reports whether c starts a JSON value.
func valueStart(c byte) bool {
	return c == '{' || c == '[' || c == '"' || c == 't' || c == 'f' || c == 'n' || c == '-' || c >= '0' && c <= '9'
}

// unexpected fails at the next byte, which is not what the document has
// there: with problem, in the structure at path, when it starts a value;
// as invalid JSON otherwise.
func (p *parser) unexpected(path, problem string) {
	if valueStart(p.next()) {
		p.fail(p.here(), path, problem)
	}
	p.fail(p.here(), "", problemValue)
}

// where is the place in the structure of the object being read: the feature,
// in a FeatureCollection.
func (p *parser) where() string {
	if p.feature > 0 {
		return "feature " + strconv.Itoa(p.feature)
	}
	return ""
}

// open counts an array or an object that opens at the next byte.
func (p *parser) open() {
	if p.depth == MaxDepth {
		p.fail(p.here(), "", problemDepth)
	}
	p.depth++
	p.skip(1)
}

// document reads the document and what follows it.
func (p *parser) document() {
	if p.ensure(len(byteOrderMark)) && string(p.buf[p.pos:p.pos+len(byteOrderMark)]) == byteOrderMark {
		p.pos += len(byteOrderMark)
	}
	p.skipSpace()
	c, ok := p.peek()
	switch {
	case !ok:
		p.fail(p.here(), "", problemEmpty)
	case c != '{':
		p.unexpected("", problemObject)
	}
	p.geoObject(inDocument)
	p.skipSpace()
	if _, ok := p.peek(); ok {
		p.fail(p.here(), "", problemAfter)
	}
}

// object reads an object, whose { is the next byte, and calls member for
// each member at the position of its name: member reads the value, which is
// next. With names, member is given the name of the member, decoded, at most
// maxKindName bytes of it, in p.name, which it must use before it reads the
// value; without, the name is checked and not kept.
func (p *parser) object(member func(at position), names bool) {
	p.open()
	p.skipSpace()
	if p.next() == '}' {
		p.skip(1)
		p.depth--
		return
	}
	for {
		p.skipSpace()
		at := p.here()
		if p.next() != '"' {
			p.fail(at, "", problemName)
		}
		if names {
			p.name = p.name[:0]
			p.str(&p.name, maxKindName)
		} else {
			p.str(nil, 0)
		}
		p.skipSpace()
		if p.next() != ':' {
			p.fail(p.here(), "", problemColon)
		}
		p.skip(1)
		p.skipSpace()
		member(at)
		p.skipSpace()
		switch p.next() {
		case ',':
			p.skip(1)
		case '}':
			p.skip(1)
			p.depth--
			return
		default:
			p.fail(p.here(), "", problemObjectSep)
		}
	}
}

// array reads an array, whose [ is the next byte, and calls item with the
// index of each item, from 1, at its position: item reads the item, which is
// next.
func (p *parser) array(item func(index int, at position)) {
	p.open()
	p.skipSpace()
	if p.next() == ']' {
		p.skip(1)
		p.depth--
		return
	}
	for i := 1; ; i++ {
		p.skipSpace()
		item(i, p.here())
		p.skipSpace()
		switch p.next() {
		case ',':
			p.skip(1)
		case ']':
			p.skip(1)
			p.depth--
			return
		default:
			p.fail(p.here(), "", problemArraySep)
		}
	}
}

// skipValue reads a value, which is next, and checks it, without keeping it.
func (p *parser) skipValue() {
	switch c := p.next(); {
	case c == '{':
		p.object(p.skipMember, false)
	case c == '[':
		p.array(p.skipItem)
	case c == '"':
		p.str(nil, 0)
	case c == 't' || c == 'f' || c == 'n':
		p.literal()
	case c == '-' || c >= '0' && c <= '9':
		p.number(false)
	default:
		p.fail(p.here(), "", problemValue)
	}
}

// str reads a string, whose opening quote is the next byte, and, unless into
// is nil, appends its content, decoded, to *into: all of it when limit is 0,
// otherwise its first limit bytes.
func (p *parser) str(into *[]byte, limit int) {
	keep := func(b []byte) {
		if into == nil {
			return
		}
		if limit > 0 {
			b = b[:min(len(b), max(0, limit-len(*into)))]
		}
		*into = append(*into, b...)
	}
	p.skip(1)
	for {
		start := p.pos
		for p.pos < p.end {
			if c := p.buf[p.pos]; c < 0x20 || c == '"' || c == '\\' || c >= utf8.RuneSelf {
				break
			}
			p.pos++
		}
		keep(p.buf[start:p.pos])
		p.col += p.pos - start
		if p.pos == p.end {
			if !p.more() {
				p.fail(p.here(), "", problemEnd)
			}
			continue
		}
		switch c := p.buf[p.pos]; {
		case c == '"':
			p.skip(1)
			return
		case c == '\\':
			var r [utf8.UTFMax]byte
			keep(r[:utf8.EncodeRune(r[:], p.escape())])
		case c < 0x20:
			p.fail(p.here(), "", problemControl)
		default:
			p.ensure(utf8.UTFMax)
			r, size := utf8.DecodeRune(p.buf[p.pos:p.end])
			if r == utf8.RuneError && size <= 1 {
				p.fail(p.here(), "", problemUTF8)
			}
			keep(p.buf[p.pos : p.pos+size])
			p.pos += size
			p.col++
		}
	}
}

// escape reads an escape of a string, whose \ is the next byte, and returns
// the character it stands for. A surrogate pair of UTF-16 is one character;
// half a pair is an error.
func (p *parser) escape() rune {
	at := p.here()
	if !p.ensure(2) {
		p.fail(p.here(), "", problemEnd)
	}
	var r rune
	switch c := p.buf[p.pos+1]; c {
	case '"', '\\', '/':
		r = rune(c)
	case 'b':
		r = '\b'
	case 'f':
		r = '\f'
	case 'n':
		r = '\n'
	case 'r':
		r = '\r'
	case 't':
		r = '\t'
	case 'u':
		r = p.hex(at)
		switch {
		case r >= 0xDC00 && r <= 0xDFFF:
			p.fail(at, "", problemEscape)
		case r >= 0xD800 && r <= 0xDBFF:
			if !p.ensure(2) || p.buf[p.pos] != '\\' || p.buf[p.pos+1] != 'u' {
				p.fail(at, "", problemEscape)
			}
			low := p.hex(at)
			if low < 0xDC00 || low > 0xDFFF {
				p.fail(at, "", problemEscape)
			}
			r = utf16.DecodeRune(r, low)
		}
		return r
	default:
		p.fail(at, "", problemEscape)
	}
	p.skip(2)
	return r
}

// hex reads an escape \u followed by four hexadecimal digits, the next bytes,
// and returns their value; at is the escape that it belongs to.
func (p *parser) hex(at position) rune {
	if !p.ensure(6) {
		p.fail(p.here(), "", problemEnd)
	}
	var v rune
	for _, c := range p.buf[p.pos+2 : p.pos+6] {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | rune(c-'A'+10)
		default:
			p.fail(at, "", problemEscape)
		}
	}
	p.skip(6)
	return v
}

// literal reads true, false or null, whose first byte is next.
func (p *parser) literal() {
	word := "null"
	switch p.buf[p.pos] {
	case 't':
		word = "true"
	case 'f':
		word = "false"
	}
	if !p.ensure(len(word)) || string(p.buf[p.pos:p.pos+len(word)]) != word {
		p.fail(p.here(), "", problemValue)
	}
	p.skip(len(word))
}

// number reads a number, whose first byte is next, and, with keep, leaves its
// text in p.num: a number kept longer than MaxNumber characters is invalid,
// as soon as its next character is read. A number not kept has no limit.
func (p *parser) number(keep bool) {
	at := p.here()
	p.num = p.num[:0]
	take := func() {
		if keep {
			if len(p.num) == MaxNumber {
				p.fail(at, "", problemNumber)
			}
			p.num = append(p.num, p.buf[p.pos])
		}
		p.skip(1)
	}
	digit := func() bool {
		c, ok := p.peek()
		return ok && c >= '0' && c <= '9'
	}
	digits := func() {
		if !digit() {
			p.fail(at, "", problemNumber)
		}
		for digit() {
			take()
		}
	}
	is := func(set string) bool {
		c, ok := p.peek()
		return ok && strings.IndexByte(set, c) >= 0
	}
	if is("-") {
		take()
	}
	if is("0") {
		take()
		if digit() {
			p.fail(at, "", problemNumber)
		}
	} else {
		digits()
	}
	if is(".") {
		take()
		digits()
	}
	if is("eE") {
		take()
		if is("+-") {
			take()
		}
		digits()
	}
}

// geoObject is what Read knows of a GeoJSON object while it reads it.
type geoObject struct {
	in   context
	at   position
	kind kind
	// seen holds the members that Read reads, as bits: each at most once.
	seen uint8
	// implied is the first member of the document, among features,
	// geometry and coordinates, read before its type: it implies the kind of
	// the document.
	implied   string
	impliedAt position
	features  bool
	geometry  bool
	null      bool // the geometry is null
	nullAt    position
	// coordinates: the object has them, at coordsAt; early: they came
	// before the type, and depth is what they told, 3 or 4, or 0 for an
	// empty array.
	coordinates bool
	coordsAt    position
	early       bool
	depth       int
}

// kindName names the kind of o in a problem: its type, or, before it, what
// its place or its first member implies.
func (o *geoObject) kindName() string {
	switch {
	case o.kind != kindNone:
		return kindNames[o.kind]
	case o.in == inFeatures || o.implied == memberGeometry:
		return kindNames[kindFeature]
	case o.implied == memberFeatures:
		return kindNames[kindFeatureCollection]
	}
	return "geometry"
}

// allows reports whether member belongs to o, as far as Read knows o.
func (o *geoObject) allows(member string) bool {
	var k kind
	switch {
	case o.kind != kindNone:
		k = o.kind
	case o.in == inFeatures:
		k = kindFeature
	case o.in == inGeometry:
		k = kindPolygon
	case o.implied != "":
		return o.implied == member
	default:
		return true
	}
	switch member {
	case memberFeatures:
		return k == kindFeatureCollection
	case memberGeometry:
		return k == kindFeature
	}
	return k == kindPolygon || k == kindMultiPolygon
}

// geoObject reads a GeoJSON object, whose { is next, in c.
func (p *parser) geoObject(c context) {
	o := &geoObject{in: c, at: p.here()}
	p.object(func(at position) {
		var member string
		var bit uint8
		switch string(p.name) {
		case "type":
			member, bit = "type", 1
		case memberFeatures:
			member, bit = memberFeatures, 2
		case memberGeometry:
			member, bit = memberGeometry, 4
		case memberCoordinates:
			member, bit = memberCoordinates, 8
		default:
			p.skipValue()
			return
		}
		if o.seen&bit != 0 {
			p.fail(at, p.where(), problemDuplicate)
		}
		o.seen |= bit
		if member == "type" {
			p.readType(o)
			return
		}
		if !o.allows(member) {
			p.fail(at, p.where(), memberNotAllowed(member, o.kindName()))
		}
		if o.in == inDocument && o.kind == kindNone && o.implied == "" {
			o.implied, o.impliedAt = member, at
		}
		switch member {
		case memberFeatures:
			o.features = true
			p.readFeatures()
		case memberGeometry:
			o.geometry = true
			p.readGeometry(o)
		default:
			o.coordinates, o.coordsAt = true, p.here()
			depth := 0
			switch o.kind {
			case kindPolygon:
				depth = 3
			case kindMultiPolygon:
				depth = 4
			}
			o.early = depth == 0
			o.depth = p.coordinates(depth)
		}
	}, true)
	where := p.where()
	switch {
	case o.kind == kindNone:
		p.fail(o.at, where, problemType)
	case o.kind == kindFeatureCollection && !o.features:
		p.fail(o.at, where, problemNoFeatures)
	case o.kind == kindFeature && !o.geometry:
		p.fail(o.at, where, problemNoGeometry)
	case o.kind == kindFeature && o.null:
		p.fail(o.nullAt, where, problemNull)
	case (o.kind == kindPolygon || o.kind == kindMultiPolygon) && !o.coordinates:
		p.fail(o.at, where, problemNoCoords)
	}
}

// readType reads the value of the type member of o, which is next, and
// checks it against what o already has.
func (p *parser) readType(o *geoObject) {
	at := p.here()
	if p.next() != '"' {
		p.unexpected(p.where(), problemTypeString)
	}
	p.text = p.text[:0]
	p.str(&p.text, maxKindName)
	o.kind = kindOf(p.text)
	switch {
	case o.kind == kindUnknown:
		p.fail(at, p.where(), problemTypeName)
	case !allowedKind(o.in, o.kind):
		p.fail(at, p.where(), kindNotAllowed(o.in, o.kind))
	case o.implied != "" && !o.allows(o.implied):
		p.fail(o.impliedAt, p.where(), memberNotAllowed(o.implied, kindNames[o.kind]))
	}
	if !o.coordinates || !o.early {
		return
	}
	// The coordinates came first: their depth tells their shape.
	want := 3
	if o.kind == kindMultiPolygon {
		want = 4
	}
	switch {
	case o.depth == 0 && want == 3:
		p.fail(o.coordsAt, p.where(), problemNoRings)
	case o.depth == 0:
		p.fail(o.coordsAt, p.where(), problemNoPolygons)
	case o.depth != want:
		p.fail(o.coordsAt, p.where(), shapeOf(want))
	}
}

// readFeatures reads the value of the features member, which is next.
func (p *parser) readFeatures() {
	if p.next() != '[' {
		p.unexpected(p.where(), problemFeatures)
	}
	p.array(func(i int, at position) {
		p.feature = i
		if p.next() != '{' {
			p.unexpected(p.where(), problemObject)
		}
		p.geoObject(inFeatures)
	})
	p.feature = 0
}

// readGeometry reads the value of the geometry member of o, which is next.
func (p *parser) readGeometry(o *geoObject) {
	switch c := p.next(); {
	case c == 'n':
		o.null, o.nullAt = true, p.here()
		p.literal()
	case c == '{':
		p.geoObject(inGeometry)
	default:
		p.unexpected(p.where(), problemObject)
	}
}

// level is an open array of coordinates.
type level struct {
	at    position
	items int  // read so far, the current one included
	sep   bool // after an item: , or ] comes next
	comma bool // after a comma: an item comes next
}

// coordinates reads the value of a coordinates member, which is next: of
// depth 3 for a Polygon, 4 for a MultiPolygon, or 0 when the type is not read
// yet, and the first number tells the depth. It adds the polygons to the
// copy, and returns the depth, or 0 for an empty array of unknown depth.
//
// The array of the coordinates is at level 0, and the positions at level
// depth-1: in a Polygon, its rings at level 1; in a MultiPolygon, its
// polygons at level 1 and their rings at level 2.
func (p *parser) coordinates(depth int) int {
	var lv [4]level
	n := 0 // the open levels
	where := p.where()
	if p.next() != '[' {
		p.unexpected(where, shapeOf(depth))
	}
	for {
		c := p.next()
		top := n - 1
		switch {
		case n > 0 && lv[top].sep:
			switch c {
			case ',':
				p.skip(1)
				lv[top].sep, lv[top].comma = false, true
			case ']':
				n = p.closeLevel(&depth, lv[:n])
			default:
				p.fail(p.here(), "", problemArraySep)
			}
		case c == ']':
			if lv[top].comma {
				p.fail(p.here(), "", problemValue)
			}
			n = p.closeLevel(&depth, lv[:n])
		case c == '[':
			at := p.here()
			if n > 0 {
				lv[top].items++
				lv[top].comma = false
			}
			if depth == 0 && n == len(lv) || depth != 0 && n >= depth {
				p.fail(at, p.path(depth, lv[:n], top), shapeOf(depth))
			}
			p.open()
			lv[n] = level{at: at}
			n++
			if depth != 0 {
				p.startArray(depth, lv[:n], n-1)
			}
		case c == '-' || c >= '0' && c <= '9':
			lv[top].items++
			lv[top].comma = false
			p.coordinate(&depth, lv[:n])
			lv[top].sep = true
		case valueStart(c):
			if depth != 0 && top == depth-1 {
				p.fail(p.here(), p.path(depth, lv[:n], top), problemNotNumber)
			}
			p.fail(p.here(), p.path(depth, lv[:n], top), shapeOf(depth))
		default:
			p.fail(p.here(), "", problemValue)
		}
		if n == 0 {
			return depth
		}
		p.skipSpace()
	}
}

// closeLevel closes the innermost level of lv, whose ] is next, and returns
// the number of levels still open.
func (p *parser) closeLevel(depth *int, lv []level) int {
	top := len(lv) - 1
	p.skip(1)
	p.depth--
	switch {
	case *depth == 0 && top == 0:
		// [] before the type: its depth is unknown.
	case *depth == 0:
		p.fail(lv[top].at, p.where(), shapeOf(0))
	default:
		p.endArray(*depth, lv, top)
	}
	if top > 0 {
		lv[top-1].sep = true
	}
	return top
}

// role is what the array at level l of coordinates of depth d holds.
type role int

const (
	rolePosition role = iota
	roleRing
	rolePolygon
	roleMultiPolygon
)

func roleOf(depth, l int) role { return role(depth - 1 - l) }

// path is the place of the array at level l in the structure, after the
// feature.
func (p *parser) path(depth int, lv []level, l int) string {
	parts := []string{}
	if w := p.where(); w != "" {
		parts = append(parts, w)
	}
	if depth == 0 {
		return strings.Join(parts, ", ")
	}
	for i := 0; i < l; i++ {
		name := [...]string{rolePosition: "position", roleRing: "ring", rolePolygon: "polygon"}[roleOf(depth, i+1)]
		parts = append(parts, name+" "+strconv.Itoa(lv[i].items))
	}
	return strings.Join(parts, ", ")
}

// startArray starts the array at level l of lv.
func (p *parser) startArray(depth int, lv []level, l int) {
	switch roleOf(depth, l) {
	case rolePosition:
		if p.positions++; p.positions > p.maxPositions {
			p.fail(lv[l].at, p.path(depth, lv, l), problemPositions)
		}
	case roleRing:
		p.ringStart = p.points.len()
		p.box = emptyBox
	case rolePolygon:
		if p.polygons.len() >= p.maxPolygons {
			p.fail(lv[l].at, p.path(depth, lv, l), problemPolygons)
		}
		p.polygons.add(uint32(p.rings.len()))
	}
}

// endArray ends the array at level l of lv.
func (p *parser) endArray(depth int, lv []level, l int) {
	at, path := lv[l].at, func() string { return p.path(depth, lv, l) }
	switch roleOf(depth, l) {
	case rolePosition:
		if lv[l].items < 2 {
			p.fail(at, path(), problemShortPos)
		}
		if lv[l-1].items > 1 && math.Abs(p.lon-p.prevLon) > 180 {
			p.fail(at, path(), problemAntimer)
		}
		if lv[l-1].items == 1 {
			p.first = point{p.lon, p.lat}
		}
		p.prevLon = p.lon
		p.points.add(point{p.lon, p.lat})
		p.box = p.box.extend(p.lon, p.lat)
	case roleRing:
		if lv[l].items < 4 {
			p.fail(at, path(), problemShortRing)
		}
		// The last position is that of the last number read.
		if p.first != (point{p.lon, p.lat}) {
			p.fail(at, path(), problemOpenRing)
		}
		p.points.pop()
		p.rings.add(uint32(p.ringStart))
		p.boxes.add(p.box)
	case rolePolygon:
		if lv[l].items == 0 {
			p.fail(at, path(), problemNoRings)
		}
	case roleMultiPolygon:
		if lv[l].items == 0 {
			p.fail(at, path(), problemNoPolygons)
		}
	}
}

// coordinate reads a number of a position, which is next, in the innermost
// level of lv. The first number of coordinates whose depth is not known tells
// it, and starts the arrays that hold it.
func (p *parser) coordinate(depth *int, lv []level) {
	at, top := p.here(), len(lv)-1
	switch {
	case *depth == 0:
		if top != 2 && top != 3 {
			p.fail(at, p.where(), shapeOf(0))
		}
		*depth = top + 1
		for l := range lv {
			p.startArray(*depth, lv, l)
		}
	case top != *depth-1:
		p.fail(at, p.path(*depth, lv, top), shapeOf(*depth))
	}
	k := lv[top].items
	if k > 3 {
		p.fail(lv[top].at, p.path(*depth, lv, top), problemLongPos)
	}
	p.number(true)
	f, err := strconv.ParseFloat(string(p.num), 64)
	if err != nil {
		p.fail(at, p.path(*depth, lv, top), problemRange)
	}
	switch k {
	case 1:
		if f < -180 || f > 180 {
			p.fail(at, p.path(*depth, lv, top), problemLongitude)
		}
		p.lon = f
	case 2:
		if f < -90 || f > 90 {
			p.fail(at, p.path(*depth, lv, top), problemLatitude)
		}
		p.lat = f
	}
}

// finish completes the arrays with their final entries, copies each of
// them once into an array of its length, and indexes the copy.
func (p *parser) finish() *Polygons {
	rings := p.rings.len()
	p.rings.add(uint32(p.points.len()))
	p.polygons.add(uint32(rings))
	a := &Polygons{points: p.points.collect(), rings: p.rings.collect(), boxes: p.boxes.collect(), polygons: p.polygons.collect()}
	a.index()
	return a
}

// The sizes of the chunks of an array being built, in values: the first
// chunk has minChunk of them, and each one after it twice as many as the one
// before, up to maxChunk.
const (
	minChunk = 256
	maxChunk = 1 << 16
)

// chunks holds the values of an array being built, in chunks that are never
// copied while it grows, and copies them once, at the end, into an array of
// their exact length. Growing one array with append would copy it each time
// it grows, by about a quarter of its length once it is large, so that a
// large document would allocate several times the arrays it builds; with
// chunks, the values are written twice at most, and the array being built
// takes at most maxChunk values more than it holds.
type chunks[T any] struct {
	full [][]T // the full chunks, in order
	last []T   // the chunk being filled
	n    int   // the values of all the chunks
}

func (c *chunks[T]) len() int { return c.n }

// add appends v.
func (c *chunks[T]) add(v T) {
	if len(c.last) == cap(c.last) {
		size := minChunk
		if c.last != nil {
			c.full = append(c.full, c.last)
			size = min(2*cap(c.last), maxChunk)
		}
		c.last = make([]T, 0, size)
	}
	c.last = append(c.last, v)
	c.n++
}

// pop removes the last value, which add has just appended.
func (c *chunks[T]) pop() {
	c.last = c.last[:len(c.last)-1]
	c.n--
}

// collect returns the values in an array of their exact length, and empties
// c: each chunk can be freed once it is copied.
func (c *chunks[T]) collect() []T {
	out := make([]T, 0, c.n)
	for i, chunk := range c.full {
		out = append(out, chunk...)
		c.full[i] = nil
	}
	out = append(out, c.last...)
	*c = chunks[T]{}
	return out
}

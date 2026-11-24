// Package sets holds the named sets that conditions read: their types, the
// strict parsers of their values, the immutable copies, and the store from
// which a decision takes one snapshot of every set.
//
// A copy is never changed once built: a new copy replaces it, atomically, in
// the Store. An entry of a set is a value read from its source, a duplicate
// included, or, for an area set, a polygon: max_entries bounds the entries
// read, so it bounds both the memory of a copy and the work of building it,
// and Entries reports the same count.
package sets

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gaissmai/bart"

	"github.com/gtriggiano/tessera/internal/area"
)

// Type is the type of a set: what its values are.
type Type string

// The types of a set.
const (
	// CIDR holds IP networks and addresses.
	CIDR Type = "cidr"
	// ASN holds autonomous system numbers.
	ASN Type = "asn"
	// String holds strings, compared exactly.
	String Type = "string"
	// Area holds polygons, read from a GeoJSON document.
	Area Type = "area"
)

// The fixed ceilings of the content of a set.
const (
	// MaxValueBytes bounds a value of values or of --set.
	MaxValueBytes = 4 << 10
	// MaxLineBytes bounds a line of text content, its comment included and
	// its line break excluded.
	MaxLineBytes = 4 << 10
	// MaxFileBytes bounds text content, such as a file.
	MaxFileBytes = 256 << 20
)

// MaxASN is the largest autonomous system number.
const MaxASN = 1<<32 - 1

// byteOrderMark is the UTF-8 byte order mark, which Read ignores at the start
// of text content.
const byteOrderMark = "\uFEFF"

// Error is invalid content: an invalid value, or a ceiling exceeded. Line,
// for content read from a file, is the line of the problem, from 1; Column,
// for a GeoJSON document, its column, from 1, in characters, and Path its
// place in the structure of the document, such as "feature 3, polygon 1, ring
// 2", or empty; Value, for a list of values, is the position of the value,
// from 1; all are 0 for a problem of the whole content. Problem never repeats
// the content.
type Error struct {
	Line    int
	Column  int
	Path    string
	Value   int
	Problem string
}

func (e *Error) Error() string {
	switch {
	case e.Line > 0 && e.Column > 0:
		return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.Where())
	case e.Line > 0:
		return fmt.Sprintf("line %d: %s", e.Line, e.Problem)
	case e.Value > 0:
		return fmt.Sprintf("value %d: %s", e.Value, e.Problem)
	}
	return e.Problem
}

// Where returns the problem, after its place in the structure of a GeoJSON
// document, if any.
func (e *Error) Where() string {
	if e.Path != "" {
		return e.Path + ": " + e.Problem
	}
	return e.Problem
}

// The problems of a value. They never repeat the value.
var (
	errCIDRSyntax   = errors.New("expected an IP address or a network, such as 192.0.2.0/24 or 2001:db8::/32")
	errCIDRHostBits = errors.New("the network has bits set after the prefix length: write its first address, such as 192.0.2.0/24")
	errCIDRZone     = errors.New("an IPv6 zone is not allowed")
	errASNSyntax    = errors.New("expected an AS number from 1 to 4294967295, such as 64496 or AS64496, without a sign or leading zeros")
	errASNZero      = errors.New("AS0 is not allowed: write a number from 1 to 4294967295")
	errStringEmpty  = errors.New("the value is empty")
	errStringUTF8   = errors.New("the value is not valid UTF-8")
	errStringCtrl   = errors.New("the value has a control character")
	errValueLong    = errors.New("the value is longer than 4 KiB")
	errLineLong     = errors.New("the line is longer than 4 KiB")
	errFileLarge    = errors.New("the content is larger than 256 MiB")
	errTooMany      = errors.New("more entries than max_entries")
	errAreaValue    = errors.New("an area set has no values: it reads a GeoJSON document")
)

// errUnknownType is the error of a type that tessera does not know.
var errUnknownType = errors.New("unknown set type")

// Set is an immutable copy of the content of a set. It is safe for
// concurrent use.
type Set struct {
	typ      Type
	entries  int
	prefixes *bart.Lite
	numbers  map[uint32]struct{}
	strings  map[string]struct{}
	polygons *area.Polygons
}

// Type returns the type of the set.
func (s *Set) Type() Type { return s.typ }

// Entries returns the number of values that the set was built from,
// duplicates included, or, for an area set, its number of polygons.
func (s *Set) Entries() int { return s.entries }

// ContainsAddr reports whether a is in a network of a cidr set. An
// IPv4-mapped IPv6 address is looked up as the IPv4 address it maps; an IPv4
// address is never in an IPv6 network.
func (s *Set) ContainsAddr(a netip.Addr) bool {
	if s.prefixes == nil || !a.IsValid() {
		return false
	}
	return s.prefixes.Contains(a.Unmap())
}

// ContainsASN reports whether n is a number of an asn set. A number outside 1
// to 4294967295 is never in it.
func (s *Set) ContainsASN(n int64) bool {
	if n < 1 || n > MaxASN {
		return false
	}
	_, ok := s.numbers[uint32(n)]
	return ok
}

// ContainsString reports whether v is a value of a string set, compared
// exactly.
func (s *Set) ContainsString(v string) bool {
	_, ok := s.strings[v]
	return ok
}

// ContainsPoint reports whether the point at latitude lat and longitude lon
// is in a polygon of an area set, as area.Polygons.Contains says.
func (s *Set) ContainsPoint(lat, lon float64) bool {
	return s.polygons.Contains(lat, lon)
}

// builder builds a new Set, and counts the values added to it.
type builder struct {
	set *Set
	max int
}

func newBuilder(t Type, maxEntries int) (*builder, error) {
	s := &Set{typ: t}
	switch t {
	case CIDR:
		s.prefixes = new(bart.Lite)
	case ASN:
		s.numbers = map[uint32]struct{}{}
	case String:
		s.strings = map[string]struct{}{}
	case Area:
		// An empty area: its polygons come from a document only.
	default:
		return nil, errUnknownType
	}
	return &builder{set: s, max: maxEntries}, nil
}

// count counts one more entry, before it is parsed, so that max_entries
// bounds the work as well as the memory.
func (b *builder) count() error {
	if b.set.entries >= b.max {
		return errTooMany
	}
	b.set.entries++
	return nil
}

// add parses v, a value without the spaces around it, and adds it.
func (b *builder) add(v string) error {
	switch b.set.typ {
	case CIDR:
		p, err := parseCIDR(v)
		if err != nil {
			return err
		}
		b.set.prefixes.Insert(p)
	case ASN:
		n, err := parseASN(v)
		if err != nil {
			return err
		}
		b.set.numbers[n] = struct{}{}
	case String:
		if err := checkString(v); err != nil {
			return err
		}
		b.set.strings[v] = struct{}{}
	case Area:
		return errAreaValue
	}
	return nil
}

// trim removes the spaces around a value: spaces, tabs, and the carriage
// return of a line that ends with CR LF.
func trim(s string) string { return strings.Trim(s, " \t\r") }

// ParseValue checks value, one value of a set of type t as values and --set
// write it: at most MaxValueBytes, without the spaces around it. The error,
// if any, is the problem, and never repeats the value. An area set has no
// values.
func ParseValue(t Type, value string) error {
	b, err := newBuilder(t, 1)
	if err != nil {
		return err
	}
	return b.parseValue(value)
}

func (b *builder) parseValue(value string) error {
	if len(value) > MaxValueBytes {
		return errValueLong
	}
	return b.add(trim(value))
}

// FromValues builds a set of type t from values, each read as ParseValue
// reads it. An invalid value, or more than maxEntries values, is an *Error
// with the position of the value. An area set of no values is empty.
func FromValues(t Type, values []string, maxEntries int) (*Set, error) {
	b, err := newBuilder(t, maxEntries)
	if err != nil {
		return nil, err
	}
	for i, v := range values {
		if err := b.count(); err != nil {
			return nil, &Error{Value: i + 1, Problem: err.Error()}
		}
		if err := b.parseValue(v); err != nil {
			return nil, &Error{Value: i + 1, Problem: err.Error()}
		}
	}
	return b.set, nil
}

// ReadError is a failure to read text content, as opposed to invalid
// content. Its message never repeats the error it wraps, which can hold a
// path.
type ReadError struct{ err error }

func (e *ReadError) Error() string { return "the content cannot be read" }
func (e *ReadError) Unwrap() error { return e.err }

// Read builds a set of type t from text content: one value per line, blank
// lines ignored, # starting a comment to the end of the line, and spaces
// around a value ignored. Any other line must be a valid value. A UTF-8 byte
// order mark at the very start of the content is ignored. It reads at
// most MaxFileBytes and lines of at most MaxLineBytes, and counts at most
// maxEntries values. Invalid content, or a ceiling exceeded, is an *Error
// with the line number; a failure of r is a *ReadError. The content is
// never applied in part: on an error, there is no set.
//
// The content of an area set is a GeoJSON document instead, which
// area.Read reads, with at most maxEntries polygons: an invalid document is
// an *Error with its line, its column and its place in the structure.
func Read(t Type, r io.Reader, maxEntries int) (*Set, error) {
	return read(t, r, maxEntries, MaxFileBytes)
}

func read(t Type, r io.Reader, maxEntries int, maxBytes int64) (*Set, error) {
	if t == Area {
		return readArea(r, maxEntries, maxBytes)
	}
	b, err := newBuilder(t, maxEntries)
	if err != nil {
		return nil, err
	}
	limited := &io.LimitedReader{R: r, N: maxBytes + 1}
	buffered := bufio.NewReaderSize(limited, 64<<10)
	// Some editors start a UTF-8 file with a byte order mark. Content
	// shorter than the mark ends with io.EOF, and stays to be scanned.
	switch start, err := buffered.Peek(len(byteOrderMark)); {
	case string(start) == byteOrderMark:
		_, _ = buffered.Discard(len(byteOrderMark))
	case err != nil && !errors.Is(err, io.EOF):
		return nil, &ReadError{err: err}
	}
	sc := bufio.NewScanner(buffered)
	// A line of MaxLineBytes and its CR LF fit; a longer one is a token
	// that check rejects, or ErrTooLong.
	sc.Buffer(make([]byte, 0, MaxLineBytes+2), MaxLineBytes+2)
	line := 0
	for sc.Scan() {
		line++
		if limited.N <= 0 {
			return nil, &Error{Problem: errFileLarge.Error()}
		}
		text := sc.Bytes()
		if len(text) > MaxLineBytes {
			return nil, &Error{Line: line, Problem: errLineLong.Error()}
		}
		if i := bytes.IndexByte(text, '#'); i >= 0 {
			text = text[:i]
		}
		v := bytes.Trim(text, " \t\r")
		if len(v) == 0 {
			continue
		}
		if err := b.count(); err != nil {
			return nil, &Error{Line: line, Problem: err.Error()}
		}
		if err := b.add(string(v)); err != nil {
			return nil, &Error{Line: line, Problem: err.Error()}
		}
	}
	switch err := sc.Err(); {
	case errors.Is(err, bufio.ErrTooLong):
		return nil, &Error{Line: line + 1, Problem: errLineLong.Error()}
	case err != nil:
		return nil, &ReadError{err: err}
	}
	if limited.N <= 0 {
		return nil, &Error{Problem: errFileLarge.Error()}
	}
	return b.set, nil
}

// readArea builds an area set from a GeoJSON document, read from r, of at
// most maxBytes.
func readArea(r io.Reader, maxEntries int, maxBytes int64) (*Set, error) {
	limited := &io.LimitedReader{R: r, N: maxBytes + 1}
	polygons, err := area.Read(limited, maxEntries)
	var invalid *area.Error
	switch {
	case err != nil && !errors.As(err, &invalid):
		return nil, &ReadError{err: err}
	case limited.N <= 0:
		return nil, &Error{Problem: errFileLarge.Error()}
	case err != nil:
		return nil, &Error{Line: invalid.Line, Column: invalid.Column, Path: invalid.Path, Problem: invalid.Problem}
	}
	return &Set{typ: Area, entries: polygons.Len(), polygons: polygons}, nil
}

// parseCIDR parses a network or an address, which is a network of one
// address. An IPv4-mapped IPv6 address or network is normalized to IPv4.
func parseCIDR(s string) (netip.Prefix, error) {
	if strings.Contains(s, "%") {
		return netip.Prefix{}, errCIDRZone
	}
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, errCIDRSyntax
		}
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, errCIDRSyntax
	}
	if p != p.Masked() {
		return netip.Prefix{}, errCIDRHostBits
	}
	// A mapped network without host bits keeps the 96 bits of the mapping
	// prefix: it is an IPv4 network.
	if p.Addr().Is4In6() {
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	return p, nil
}

// parseASN parses an AS number: an optional AS prefix in any case, then a
// decimal number from 1 to MaxASN, without a sign or leading zeros.
func parseASN(s string) (uint32, error) {
	if len(s) >= 2 && (s[0] == 'A' || s[0] == 'a') && (s[1] == 'S' || s[1] == 's') {
		s = s[2:]
	}
	if s == "" || len(s) > 10 || strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' }) {
		return 0, errASNSyntax
	}
	if s[0] == '0' {
		if s == "0" {
			return 0, errASNZero
		}
		return 0, errASNSyntax
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, errASNSyntax
	}
	return uint32(n), nil
}

// checkString checks a value of a string set: not empty, valid UTF-8,
// without control characters.
func checkString(s string) error {
	switch {
	case s == "":
		return errStringEmpty
	case !utf8.ValidString(s):
		return errStringUTF8
	case strings.ContainsFunc(s, unicode.IsControl):
		return errStringCtrl
	}
	return nil
}

package sets

import (
	"errors"
	"io"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func mustRead(t *testing.T, typ Type, content string, maxEntries int) *Set {
	t.Helper()
	s, err := Read(typ, strings.NewReader(content), maxEntries)
	if err != nil {
		t.Fatalf("Read(%s): %v", typ, err)
	}
	return s
}

// contentError reads content and returns its *Error.
func contentError(t *testing.T, typ Type, content string, maxEntries int) *Error {
	t.Helper()
	_, err := Read(typ, strings.NewReader(content), maxEntries)
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("Read(%s): got %v, want an *Error", typ, err)
	}
	return e
}

func TestCIDRSet(t *testing.T) {
	s := mustRead(t, CIDR, `
# Partners, IPv4 and IPv6 mixed.
203.0.113.0/24
198.51.100.7          # an address is a /32
2001:db8::/32
  2001:db8:ffff::1    # an address is a /128
::ffff:192.0.2.0/120  # a mapped network is normalized
::ffff:198.51.100.200
203.0.113.128/25      # overlaps the /24
203.0.113.0/24        # a duplicate
`, 100)
	if s.Type() != CIDR {
		t.Errorf("type %s", s.Type())
	}
	if s.Entries() != 8 {
		t.Errorf("%d entries, want 8: a value read counts, a duplicate included", s.Entries())
	}
	for addr, want := range map[string]bool{
		"203.0.113.1":           true,
		"203.0.113.200":         true,
		"198.51.100.7":          true,
		"198.51.100.8":          false,
		"2001:db8::1":           true,
		"2001:db8:ffff::1":      true,
		"192.0.2.77":            true,
		"198.51.100.200":        true,
		"::ffff:203.0.113.1":    true, // a mapped address is looked up unmapped
		"::ffff:192.0.2.77":     true,
		"2001:db9::1":           false,
		"192.0.3.1":             false,
		"::ffff:c000:201":       true,
		"2001:db8:ffff::2":      true, // in 2001:db8::/32
		"64:ff9b::203.0.113.1":  false,
		"::203.0.113.1":         false, // IPv4-compatible, not mapped
		"2001:db8:0:0:0:0:0:ff": true,
	} {
		if got := s.ContainsAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("ContainsAddr(%s) = %v, want %v", addr, got, want)
		}
	}
	if s.ContainsAddr(netip.Addr{}) {
		t.Error("the zero address is contained")
	}
	if s.ContainsASN(64496) || s.ContainsString("203.0.113.0/24") {
		t.Error("a cidr set contains a number or a string")
	}

	// An IPv4 address is never in an IPv6 network, the whole space
	// included.
	v6 := mustRead(t, CIDR, "::/0\n", 10)
	if v6.ContainsAddr(netip.MustParseAddr("192.0.2.1")) || !v6.ContainsAddr(netip.MustParseAddr("2001:db8::1")) {
		t.Error("::/0 must hold every IPv6 address and no IPv4 one")
	}
	v4 := mustRead(t, CIDR, "0.0.0.0/0\n", 10)
	if !v4.ContainsAddr(netip.MustParseAddr("192.0.2.1")) || v4.ContainsAddr(netip.MustParseAddr("2001:db8::1")) {
		t.Error("0.0.0.0/0 must hold every IPv4 address and no IPv6 one")
	}
	mapped := mustRead(t, CIDR, "::ffff:0:0/96\n", 10)
	if !mapped.ContainsAddr(netip.MustParseAddr("198.51.100.1")) || mapped.ContainsAddr(netip.MustParseAddr("2001:db8::1")) {
		t.Error("::ffff:0:0/96 must be 0.0.0.0/0")
	}
}

func TestInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		typ     Type
		value   string
		problem string
	}{
		{CIDR, "203.0.113.1/24", "the network has bits set after the prefix length"},
		{CIDR, "2001:db8::1/32", "the network has bits set after the prefix length"},
		{CIDR, "203.0.113.0/33", "expected an IP address or a network"},
		{CIDR, "203.0.113", "expected an IP address or a network"},
		{CIDR, "203.0.113.01", "expected an IP address or a network"},
		{CIDR, "example.com", "expected an IP address or a network"},
		{CIDR, "203.0.113.0 /24", "expected an IP address or a network"},
		{CIDR, "fe80::1%eth0", "an IPv6 zone is not allowed"},
		{CIDR, "fe80::%eth0/64", "an IPv6 zone is not allowed"},
		{CIDR, "::ffff:0.0.0.0/95", "the network has bits set after the prefix length"},
		{CIDR, "", "expected an IP address or a network"},
		{ASN, "0", "AS0 is not allowed"},
		{ASN, "AS0", "AS0 is not allowed"},
		{ASN, "4294967296", "expected an AS number from 1 to 4294967295"},
		{ASN, "99999999999", "expected an AS number"},
		{ASN, "064496", "expected an AS number"},
		{ASN, "+64496", "expected an AS number"},
		{ASN, "-1", "expected an AS number"},
		{ASN, "AS 64496", "expected an AS number"},
		{ASN, "ASN64496", "expected an AS number"},
		{ASN, "AS", "expected an AS number"},
		{ASN, "0x10", "expected an AS number"},
		{ASN, "1_000", "expected an AS number"},
		{ASN, "64496.5", "expected an AS number"},
		{ASN, "", "expected an AS number"},
		{String, "", "the value is empty"},
		{String, "   ", "the value is empty"},
		{String, "bad\xffutf8", "the value is not valid UTF-8"},
		{String, "a\x00b", "the value has a control character"},
		{String, "a\tb", "the value has a control character"},
		{String, "a\u0085b", "the value has a control character"},
		{String, strings.Repeat("x", MaxValueBytes+1), "the value is longer than 4 KiB"},
	} {
		err := ParseValue(tc.typ, tc.value)
		if err == nil || !strings.HasPrefix(err.Error(), tc.problem) {
			t.Errorf("ParseValue(%s, %q) = %v, want %q…", tc.typ, tc.value, err, tc.problem)
			continue
		}
		if len(tc.value) >= 4 && strings.Contains(err.Error(), tc.value) {
			t.Errorf("ParseValue(%s, %q): the error repeats the value: %v", tc.typ, tc.value, err)
		}
	}
	if err := ParseValue("area", "x"); err == nil {
		t.Error("an unknown type is accepted")
	}
}

func TestValidValues(t *testing.T) {
	for _, tc := range []struct {
		typ   Type
		value string
	}{
		{CIDR, "192.0.2.0/24"},
		{CIDR, "  192.0.2.1  "},
		{CIDR, "2001:db8::/32"},
		{CIDR, "::ffff:192.0.2.1"},
		{CIDR, "0.0.0.0/0"},
		{CIDR, "::/0"},
		{ASN, "1"},
		{ASN, "64496"},
		{ASN, "AS64511"},
		{ASN, "as64511"},
		{ASN, "aS64511"},
		{ASN, "4294967295"},
		{ASN, " AS64500 "},
		{String, "api.example.com"},
		{String, "  café  "},
		{String, "#not-a-comment"}, // # is a comment in text content only
		{String, "a b"},
		{String, strings.Repeat("x", MaxValueBytes)},
	} {
		if err := ParseValue(tc.typ, tc.value); err != nil {
			t.Errorf("ParseValue(%s, %q) = %v", tc.typ, tc.value, err)
		}
	}
}

func TestASNSet(t *testing.T) {
	s, err := FromValues(ASN, []string{"64496", "AS64497", "as64498", "4294967295", "64496"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.Entries() != 5 {
		t.Errorf("%d entries, want 5", s.Entries())
	}
	for n, want := range map[int64]bool{64496: true, 64497: true, 64498: true, 4294967295: true, 64499: false, 0: false, -64496: false, 4294967296 + 64496: false, 1 << 62: false} {
		if got := s.ContainsASN(n); got != want {
			t.Errorf("ContainsASN(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestStringSet(t *testing.T) {
	s := mustRead(t, String, "api.example.com\n  www.example.com  # trimmed\n\r\nAPI.example.com\r\ncafé\n", 10)
	for v, want := range map[string]bool{
		"api.example.com":  true,
		"www.example.com":  true,
		"API.example.com":  true,
		"Api.example.com":  false, // exact, case-sensitive
		"café":             true,
		" api.example.com": false,
		"":                 false,
		"example.com":      false,
	} {
		if got := s.ContainsString(v); got != want {
			t.Errorf("ContainsString(%q) = %v, want %v", v, got, want)
		}
	}
	if s.Entries() != 4 {
		t.Errorf("%d entries, want 4", s.Entries())
	}
}

func TestReadContent(t *testing.T) {
	// Blank lines, comments, spaces, CR LF and a last line without a line
	// break.
	s := mustRead(t, CIDR, "\n   \n# comment\n\t192.0.2.0/24\t# a network\r\n#\n198.51.100.0/24", 10)
	if s.Entries() != 2 || !s.ContainsAddr(netip.MustParseAddr("198.51.100.9")) {
		t.Errorf("entries %d", s.Entries())
	}
	if s := mustRead(t, String, "", 10); s.Entries() != 0 {
		t.Error("empty content is not an empty set")
	}
	if s := mustRead(t, ASN, "# only comments\n\n", 10); s.Entries() != 0 {
		t.Error("only comments is not an empty set")
	}

	// An invalid line is an error with its line number, and no set.
	e := contentError(t, CIDR, "# partners\n192.0.2.0/24\n\n10.0.0.1/8 # wrong\n198.51.100.0/24\n", 10)
	if e.Line != 4 || !strings.HasPrefix(e.Problem, "the network has bits set") {
		t.Errorf("error %+v, want line 4", e)
	}
	if got := e.Error(); !strings.HasPrefix(got, "line 4: the network has bits set") || strings.Contains(got, "10.0.0.1") {
		t.Errorf("message %q", got)
	}
	if e := contentError(t, String, "a\nb\x01c\n", 10); e.Line != 2 || e.Problem != "the value has a control character" {
		t.Errorf("error %+v", e)
	}
	if e := contentError(t, ASN, "64496\nAS0\n", 10); e.Line != 2 {
		t.Errorf("error %+v", e)
	}
	// # always starts a comment in text content, for a string set too.
	if s := mustRead(t, String, "a#b\n", 10); !s.ContainsString("a") || s.ContainsString("a#b") {
		t.Error("# does not start a comment")
	}
}

// A UTF-8 byte order mark at the very start of text content is ignored, for
// every type. Elsewhere, and in a value of values or --set, it is a
// character like any other.
func TestByteOrderMark(t *testing.T) {
	const bom = "\uFEFF"
	if s := mustRead(t, CIDR, bom+"192.0.2.0/24\n", 10); s.Entries() != 1 || !s.ContainsAddr(netip.MustParseAddr("192.0.2.7")) {
		t.Error("cidr: the first value is lost")
	}
	if s := mustRead(t, ASN, bom+"AS64496\n", 10); !s.ContainsASN(64496) {
		t.Error("asn: the first value is lost")
	}
	if s := mustRead(t, String, bom+"api.example.com\n", 10); !s.ContainsString("api.example.com") || s.ContainsString(bom+"api.example.com") {
		t.Error("string: the mark is kept in the first value")
	}
	if s := mustRead(t, String, bom+"# partners\napi.example.com\n", 10); s.Entries() != 1 {
		t.Errorf("a comment after the mark: %d entries, want 1", s.Entries())
	}
	if s := mustRead(t, CIDR, bom, 10); s.Entries() != 0 {
		t.Errorf("the mark alone: %d entries, want 0", s.Entries())
	}
	// Only one mark, and only at the start.
	if e := contentError(t, CIDR, bom+bom+"192.0.2.0/24\n", 10); e.Line != 1 {
		t.Errorf("two marks: %+v", e)
	}
	if e := contentError(t, CIDR, "192.0.2.0/24\n"+bom+"198.51.100.0/24\n", 10); e.Line != 2 {
		t.Errorf("a mark on line 2: %+v", e)
	}
	if s := mustRead(t, String, "a\n"+bom+"b\n", 10); !s.ContainsString(bom + "b") {
		t.Error("string: a mark on line 2 is not part of the value")
	}
	// A value of values or --set keeps it: a cidr or asn value is invalid,
	// a string value holds it.
	if err := ParseValue(CIDR, bom+"192.0.2.0/24"); err == nil {
		t.Error("cidr value with a mark: accepted")
	}
	if err := ParseValue(ASN, bom+"64496"); err == nil {
		t.Error("asn value with a mark: accepted")
	}
	if s, err := FromValues(String, []string{bom + "api.example.com"}, 1); err != nil || !s.ContainsString(bom+"api.example.com") {
		t.Errorf("string value with a mark: %v", err)
	}
}

func TestCeilings(t *testing.T) {
	// A line of 4 KiB, comment included, is accepted, with its CR LF; one
	// byte more is rejected, comment or value.
	line := "192.0.2.0/24 #" + strings.Repeat("x", MaxLineBytes-14)
	mustRead(t, CIDR, line+"\r\n"+line+"\n", 10)
	if e := contentError(t, CIDR, "192.0.2.0/24\n"+line+"y\n", 10); e.Line != 2 || e.Problem != "the line is longer than 4 KiB" {
		t.Errorf("error %+v", e)
	}
	if e := contentError(t, String, "a\nb\n"+strings.Repeat("y", 100_000)+"\n", 10); e.Line != 3 || e.Problem != "the line is longer than 4 KiB" {
		t.Errorf("error %+v", e)
	}

	// max_entries counts the values read, duplicates included, and stops
	// at the first value beyond it.
	mustRead(t, ASN, "64496\n64496\n# x\n64497\n", 3)
	if e := contentError(t, ASN, "64496\n64496\n\n64497\n64498\n", 3); e.Line != 5 || e.Problem != "more entries than max_entries" {
		t.Errorf("error %+v", e)
	}
	// The value beyond the ceiling is not parsed.
	if e := contentError(t, CIDR, "192.0.2.0/24\nnot-a-network\n", 1); e.Problem != "more entries than max_entries" {
		t.Errorf("error %+v", e)
	}
	if _, err := FromValues(String, []string{"a", "b", "c"}, 2); err == nil || err.Error() != "value 3: more entries than max_entries" {
		t.Errorf("FromValues: %v", err)
	}

	// The content is at most 256 MiB.
	big := &repeatReader{line: []byte("# " + strings.Repeat("x", 4000) + "\n"), left: MaxFileBytes + 1}
	if _, err := Read(String, big, 10); err == nil || err.Error() != "the content is larger than 256 MiB" {
		t.Errorf("content over 256 MiB: %v", err)
	}
	exact := &repeatReader{line: []byte("# " + strings.Repeat("x", 4000) + "\n"), left: MaxFileBytes}
	if _, err := Read(String, exact, 10); err != nil {
		t.Errorf("content of 256 MiB: %v", err)
	}
}

// repeatReader returns left bytes of line, repeated.
type repeatReader struct {
	line []byte
	left int64
	off  int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && r.left > 0 {
		c := copy(p[n:], r.line[r.off:])
		if int64(c) > r.left {
			c = int(r.left)
		}
		n += c
		r.left -= int64(c)
		r.off = (r.off + c) % len(r.line)
	}
	return n, nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("read /secret/path: input/output error")
}

func TestReadError(t *testing.T) {
	_, err := Read(CIDR, failingReader{}, 10)
	var re *ReadError
	if !errors.As(err, &re) || strings.Contains(err.Error(), "secret") {
		t.Errorf("got %v, want a *ReadError that does not repeat its cause", err)
	}
}

func TestFromValues(t *testing.T) {
	s, err := FromValues(CIDR, []string{"192.0.2.0/24", " 2001:db8::/32 "}, 10)
	if err != nil || s.Entries() != 2 || !s.ContainsAddr(netip.MustParseAddr("2001:db8::1")) {
		t.Fatalf("FromValues: %v", err)
	}
	empty, err := FromValues(String, nil, 10)
	if err != nil || empty.Entries() != 0 || empty.ContainsString("") {
		t.Fatalf("an empty list: %v", err)
	}
	_, err = FromValues(CIDR, []string{"192.0.2.0/24", "192.0.2.1/24"}, 10)
	var e *Error
	if !errors.As(err, &e) || e.Value != 2 || e.Line != 0 || err.Error() != "value 2: the network has bits set after the prefix length: write its first address, such as 192.0.2.0/24" {
		t.Errorf("got %v", err)
	}
	// # is part of a value of values.
	hash, err := FromValues(String, []string{"#tag"}, 10)
	if err != nil || !hash.ContainsString("#tag") {
		t.Errorf("# in a value: %v", err)
	}
}

func TestStore(t *testing.T) {
	store, err := NewStore([]Declaration{
		{Name: "office", Type: CIDR, OnUnavailable: Fail},
		{Name: "partners", Type: String, OnUnavailable: NoMatch},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Declarations(); len(got) != 2 || got[1].Name != "partners" {
		t.Errorf("declarations %+v", got)
	}
	before := store.Snapshot()
	if e := before.Entry(0); e.Set != nil || e.Available {
		t.Errorf("a set never loaded: %+v", e)
	}
	office, _ := FromValues(CIDR, []string{"192.0.2.0/24"}, 10)
	if err := store.Replace("office", office); err != nil {
		t.Fatal(err)
	}
	after := store.Snapshot()
	if e := after.Entry(0); e.Set != office || !e.Available {
		t.Errorf("after Replace: %+v", e)
	}
	// A snapshot never changes.
	if e := before.Entry(0); e.Set != nil {
		t.Error("Replace changed an earlier snapshot")
	}
	if err := store.SetAvailable("office", false); err != nil {
		t.Fatal(err)
	}
	if e, ok := store.Get("office"); !ok || e.Available || e.Set != office {
		t.Errorf("after SetAvailable(false): %+v", e)
	}
	if e := after.Entry(0); !e.Available {
		t.Error("SetAvailable changed an earlier snapshot")
	}
	if err := store.SetAvailable("partners", true); err == nil {
		t.Error("a set never loaded is marked available")
	}
	if err := store.Replace("partners", office); err == nil {
		t.Error("a copy of another type is accepted")
	}
	if err := store.Replace("nope", office); err == nil {
		t.Error("an unknown set is accepted")
	}
	if _, ok := store.Get("nope"); ok {
		t.Error("Get finds an unknown set")
	}
	if e := after.Entry(7); e.Set != nil || e.Available {
		t.Error("an index out of range is a set")
	}
	var none *Store
	if none.Snapshot() != nil || none.Declarations() != nil || none.Snapshot().Entry(0).Available {
		t.Error("a nil Store")
	}
	for _, decls := range [][]Declaration{
		{{Name: "a", Type: CIDR, OnUnavailable: Fail}, {Name: "a", Type: ASN, OnUnavailable: Fail}},
		{{Name: "a", Type: "area", OnUnavailable: Fail}},
		{{Name: "a", Type: ASN, OnUnavailable: "maybe"}},
	} {
		if _, err := NewStore(decls); err == nil {
			t.Errorf("NewStore(%+v) succeeds", decls)
		}
	}
}

// Readers take snapshots while writers replace copies: run with -race.
func TestStoreConcurrency(t *testing.T) {
	store, err := NewStore([]Declaration{{Name: "hosts", Type: String, OnUnavailable: Fail}})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := FromValues(String, []string{"a.example.com"}, 10)
	if err := store.Replace("hosts", first); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s, _ := FromValues(String, []string{"a.example.com", "b" + strconv.Itoa(i) + ".example.com"}, 10)
			_ = store.Replace("hosts", s)
			_ = store.SetAvailable("hosts", i%2 == 0)
		}
	})
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 2000 {
				e := store.Snapshot().Entry(0)
				if !e.Set.ContainsString("a.example.com") {
					t.Error("a snapshot lost a value")
					return
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	wg.Wait()
}

// problemRE matches the message of every *Error: a line or a value
// position, and a fixed problem that never repeats the content.
var problemRE = regexp.MustCompile(`^((line|value) [1-9][0-9]*: )?(` + strings.Join([]string{
	regexp.QuoteMeta(errCIDRSyntax.Error()), regexp.QuoteMeta(errCIDRHostBits.Error()), regexp.QuoteMeta(errCIDRZone.Error()),
	regexp.QuoteMeta(errASNSyntax.Error()), regexp.QuoteMeta(errASNZero.Error()),
	regexp.QuoteMeta(errStringEmpty.Error()), regexp.QuoteMeta(errStringUTF8.Error()), regexp.QuoteMeta(errStringCtrl.Error()),
	regexp.QuoteMeta(errValueLong.Error()), regexp.QuoteMeta(errLineLong.Error()), regexp.QuoteMeta(errFileLarge.Error()),
	regexp.QuoteMeta(errTooMany.Error()),
}, "|") + `)$`)

// FuzzRead reads arbitrary content as each type of set. Read never panics;
// it accepts the content exactly when every line that holds a value, once
// a byte order mark at the start of the content, its comment and spaces are
// removed, is a value that ParseValue accepts, within the ceilings; its
// errors never repeat the content.
func FuzzRead(f *testing.F) {
	for _, seed := range []string{
		"192.0.2.0/24\n2001:db8::/32 # c\n",
		"::ffff:192.0.2.0/120\r\n198.51.100.7\n",
		"64496\nAS64497\n#\n",
		"api.example.com\n  b  \n",
		"10.0.0.1/8\n",
		"fe80::1%eth0\n",
		"a\x00b\n",
		"\xff\xfe\n",
		"\uFEFF192.0.2.0/24\n",
		"\uFEFF\uFEFFAS64496\n",
		"\xef\xbb",
		strings.Repeat("x", MaxLineBytes+1),
	} {
		f.Add(seed, uint8(0), uint8(10))
	}
	f.Fuzz(func(t *testing.T, content string, kind, maxEntries uint8) {
		typ := []Type{CIDR, ASN, String}[int(kind)%3]
		max := int(maxEntries)
		s, err := Read(typ, strings.NewReader(content), max)

		// One byte order mark at the start is not content.
		text := strings.TrimPrefix(content, byteOrderMark)
		valid, values := true, 0
		lines := strings.Split(text, "\n")
		if strings.HasSuffix(text, "\n") {
			lines = lines[:len(lines)-1]
		}
		for _, line := range lines {
			line = strings.TrimSuffix(line, "\r")
			if len(line) > MaxLineBytes {
				valid = false
				break
			}
			if i := strings.IndexByte(line, '#'); i >= 0 {
				line = line[:i]
			}
			v := trim(line)
			if v == "" {
				continue
			}
			values++
			if values > max || ParseValue(typ, v) != nil {
				valid = false
				break
			}
		}
		switch {
		case err == nil && !valid:
			t.Fatalf("Read accepted invalid content")
		case err != nil && valid:
			t.Fatalf("Read rejected valid content: %v", err)
		case err == nil && s.Entries() != values:
			t.Fatalf("%d entries, want %d", s.Entries(), values)
		case err != nil && !problemRE.MatchString(err.Error()):
			t.Fatalf("unexpected message: %q", err)
		}
	})
}

// FuzzParseValue checks one value of each type. ParseValue never panics,
// and a value it accepts builds a set that contains it.
func FuzzParseValue(f *testing.F) {
	for _, seed := range []string{"192.0.2.0/24", "::ffff:192.0.2.1", "2001:db8::/32", "AS64496", "64496", "0", "api.example.com", " x ", "a\tb"} {
		f.Add(seed, uint8(0))
		f.Add(seed, uint8(1))
		f.Add(seed, uint8(2))
	}
	f.Fuzz(func(t *testing.T, value string, kind uint8) {
		typ := []Type{CIDR, ASN, String}[int(kind)%3]
		err := ParseValue(typ, value)
		if err != nil {
			if !problemRE.MatchString(err.Error()) {
				t.Fatalf("unexpected message: %q", err)
			}
			return
		}
		s, err := FromValues(typ, []string{value}, 1)
		if err != nil {
			t.Fatalf("FromValues rejects a value that ParseValue accepts: %v", err)
		}
		v := trim(value)
		switch typ {
		case CIDR:
			p, err := parseCIDR(v)
			if err != nil || !s.ContainsAddr(p.Addr()) {
				t.Fatalf("the set does not contain the first address of its network")
			}
		case ASN:
			n, err := parseASN(v)
			if err != nil || !s.ContainsASN(int64(n)) {
				t.Fatalf("the set does not contain its number")
			}
		case String:
			if !s.ContainsString(v) {
				t.Fatalf("the set does not contain its value")
			}
		}
	})
}

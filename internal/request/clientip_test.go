package request

import (
	"net/netip"
	"strings"
	"testing"
)

// spoofed holds every header that commonly carries a client address, each
// with an address that the client chose.
var spoofed = map[string]string{
	"x-forwarded-for":          "192.0.2.66",
	"x-real-ip":                "192.0.2.66",
	"x-client-ip":              "192.0.2.66",
	"true-client-ip":           "192.0.2.66",
	"x-cluster-client-ip":      "192.0.2.66",
	"forwarded":                "for=192.0.2.66",
	"x-envoy-external-address": "192.0.2.66",
}

func resolve(s ClientIPSource, peer string, headers map[string]string) netip.Addr {
	return s.Resolve(Normalize(Raw{Peer: peer, Headers: headers}))
}

func wantAddress(t *testing.T, got netip.Addr, want string) {
	t.Helper()
	if want == "" {
		if got.IsValid() {
			t.Fatalf("got %v, want no address", got)
		}
		return
	}
	if got != netip.MustParseAddr(want) {
		t.Fatalf("got %v, want %s", got, want)
	}
}

func TestClientIPFromPeerIgnoresHeaders(t *testing.T) {
	for _, s := range []ClientIPSource{{From: FromPeer}, {From: FromPeer, Header: "x-real-ip", TrustedHops: 1}} {
		wantAddress(t, resolve(s, "203.0.113.10", spoofed), "203.0.113.10")
		wantAddress(t, resolve(s, "", spoofed), "")
		wantAddress(t, resolve(s, "not an address", spoofed), "")
	}
}

func TestClientIPFromHeader(t *testing.T) {
	s := ClientIPSource{From: FromHeader, Header: "X-Envoy-External-Address"}
	for _, tc := range []struct {
		name  string
		value *string
		want  string
	}{
		{"one address", ptr("198.51.100.7"), "198.51.100.7"},
		{"with spaces", ptr(" 198.51.100.7\t"), "198.51.100.7"},
		{"with a port", ptr("198.51.100.7:4711"), "198.51.100.7"},
		{"IPv6", ptr("2001:db8::7"), "2001:db8::7"},
		{"IPv4-mapped", ptr("::ffff:198.51.100.7"), "198.51.100.7"},
		{"missing", nil, ""},
		{"empty", ptr(""), ""},
		{"two addresses", ptr("198.51.100.7,192.0.2.66"), ""},
		{"two addresses with a space", ptr("198.51.100.7, 192.0.2.66"), ""},
		{"not an address", ptr("unknown"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{"x-real-ip": "192.0.2.66", "x-forwarded-for": "192.0.2.66"}
			if tc.value != nil {
				headers["x-envoy-external-address"] = *tc.value
			}
			// The peer is valid: a missing or invalid header must not fall
			// back on it.
			wantAddress(t, resolve(s, "203.0.113.10", headers), tc.want)
		})
	}
	// Undeclared headers are never read.
	for name := range spoofed {
		if name == "x-envoy-external-address" {
			continue
		}
		wantAddress(t, resolve(ClientIPSource{From: FromHeader, Header: "x-trusted-client"}, "203.0.113.10", map[string]string{name: "192.0.2.66"}), "")
	}
}

func TestClientIPFromXForwardedFor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hops  int
		value string
		want  string
	}{
		{"0 hops, rightmost entry", 0, "203.0.113.10, 198.51.100.1", "198.51.100.1"},
		{"1 hop", 1, "203.0.113.10, 198.51.100.1", "203.0.113.10"},
		{"1 hop without spaces", 1, "203.0.113.10,198.51.100.1", "203.0.113.10"},
		{"2 hops", 2, "203.0.113.10, 198.51.100.1, 192.0.2.1", "203.0.113.10"},
		{"one entry", 0, "203.0.113.10", "203.0.113.10"},
		{"spaces and tabs", 1, " 203.0.113.10 ,\t198.51.100.1 ", "203.0.113.10"},
		{"IPv4 with a port", 0, "203.0.113.10:4711", "203.0.113.10"},
		{"IPv6", 0, "2001:db8::1", "2001:db8::1"},
		{"IPv6 in brackets", 0, "[2001:db8::1]", "2001:db8::1"},
		{"IPv6 in brackets with a port", 0, "[2001:db8::1]:443", "2001:db8::1"},
		{"mixed families", 1, "2001:db8::1, 203.0.113.10", "2001:db8::1"},
		{"IPv4-mapped", 0, "::ffff:203.0.113.10", "203.0.113.10"},
		{"zone", 0, "fe80::1%eth0", ""},
		// The trusted entries are not checked: they are the proxies' own.
		{"invalid trusted entry", 1, "203.0.113.10, unknown", "203.0.113.10"},
		{"too few entries", 1, "203.0.113.10", ""},
		{"far too few entries", 3, "203.0.113.10, 198.51.100.1", ""},
		{"empty", 0, "", ""},
		{"empty entry", 0, "203.0.113.10,", ""},
		{"empty selected entry", 1, "203.0.113.10,,198.51.100.1", ""},
		{"invalid entry, no search further left", 0, "203.0.113.10, unknown", ""},
		{"obfuscated identifier", 0, "203.0.113.10, _hidden", ""},
		{"Forwarded syntax", 0, "for=203.0.113.10", ""},
		{"unspecified", 0, "0.0.0.0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ClientIPSource{From: FromXForwardedFor, TrustedHops: tc.hops}
			headers := map[string]string{"x-real-ip": "192.0.2.66"}
			if tc.value != "" {
				headers["x-forwarded-for"] = tc.value
			}
			// No fall back on the peer.
			wantAddress(t, resolve(s, "198.51.100.250", headers), tc.want)
		})
	}
	wantAddress(t, resolve(ClientIPSource{From: FromXForwardedFor}, "198.51.100.250", nil), "")
	wantAddress(t, resolve(ClientIPSource{From: FromXForwardedFor, TrustedHops: -1}, "198.51.100.250", map[string]string{"x-forwarded-for": "203.0.113.10"}), "")
}

// The client writes the left end of X-Forwarded-For: the entries it adds
// there, in its own header or in the one the proxies append to, do not move
// the address read from the right.
func TestClientIPXForwardedForPrependedEntries(t *testing.T) {
	s := ClientIPSource{From: FromXForwardedFor, TrustedHops: 1}
	for _, value := range []string{
		"203.0.113.10, 198.51.100.250",
		"192.0.2.66, 203.0.113.10, 198.51.100.250",
		"192.0.2.66,192.0.2.67, 203.0.113.10, 198.51.100.250",
		"unknown, 203.0.113.10, 198.51.100.250",
		", 203.0.113.10, 198.51.100.250",
	} {
		wantAddress(t, resolve(s, "198.51.100.250", map[string]string{"x-forwarded-for": value}), "203.0.113.10")
	}
}

func TestUnknownSourceGivesNoAddress(t *testing.T) {
	for _, from := range []Source{"", "Peer", "x-forwarded-for"} {
		wantAddress(t, resolve(ClientIPSource{From: from, Header: "x-forwarded-for"}, "203.0.113.10", map[string]string{"x-forwarded-for": "203.0.113.10"}), "")
	}
}

func TestParseAddress(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.10":               "203.0.113.10",
		" 203.0.113.10\t":            "203.0.113.10",
		"203.0.113.10:8080":          "203.0.113.10",
		"203.0.113.10:0":             "203.0.113.10",
		"2001:db8::1":                "2001:db8::1",
		"2001:DB8::1":                "2001:db8::1",
		"[2001:db8::1]":              "2001:db8::1",
		"[2001:db8::1]:8080":         "2001:db8::1",
		"::ffff:198.51.100.1":        "198.51.100.1",
		"[::ffff:198.51.100.1]":      "198.51.100.1",
		"fe80::1%en0":                "",
		"[fe80::1%en0]:80":           "",
		"fe80::1%a,198.51.100.1":     "",
		"::ffff:198.51.100.1%a":      "",
		"":                           "",
		" ":                          "",
		"0.0.0.0":                    "",
		"::":                         "",
		"::ffff:0.0.0.0":             "",
		"[::]:80":                    "",
		"[203.0.113.10]":             "",
		"[203.0.113.10]:80":          "",
		"not-an-ip":                  "",
		"unknown":                    "",
		"for=203.0.113.10":           "",
		"203.0.113.10, 198.51.100.1": "",
		"[2001:db8::1":               "",
		"2001:db8::1]":               "",
		"[]":                         "",
		"[":                          "",
		"203.0.113.10:":              "",
		"203.0.113.10:http":          "",
		"203.0.113.10:65536":         "",
		"203.0.113.010":              "",
		"203.0.113":                  "",
		"203.0.113.10%eth0":          "",
		"2001:db8::1:80":             "2001:db8::1:80",
		"\n203.0.113.10":             "",
		"203.0.113.10\r":             "",
		"[2001:db8::1]:8080:80":      "",
		"203.0.113.10:8080:8080":     "",
	} {
		got := ParseAddress(in)
		if want == "" && got.IsValid() || want != "" && got != netip.MustParseAddr(want) {
			t.Errorf("ParseAddress(%q) = %v, want %q", in, got, want)
		}
	}
}

// FuzzClientIP checks, for any request, that each source reads only what it
// declares, that the left end of X-Forwarded-For cannot move the address, and
// that every address returned is normalized.
func FuzzClientIP(f *testing.F) {
	f.Add("203.0.113.10", "198.51.100.7", "192.0.2.66, 203.0.113.10, 198.51.100.250", 1, "192.0.2.1")
	f.Add("2001:db8::1", "[2001:db8::7]:443", "[2001:db8::7]:443,198.51.100.250", 1, "")
	f.Add("::ffff:192.0.2.1", "fe80::1%eth0", ",,,", 0, "unknown")
	f.Add("", "", "", 5, ",")
	f.Fuzz(func(t *testing.T, peer, header, xff string, hops int, prefix string) {
		headers := map[string]string{"x-client": header, "x-forwarded-for": xff, "x-real-ip": prefix}
		normalized := func(a netip.Addr) {
			t.Helper()
			if a.IsValid() && (a.Is4In6() || a.Zone() != "" || a.IsUnspecified()) {
				t.Fatalf("%v is not normalized", a)
			}
		}

		fromPeer := resolve(ClientIPSource{From: FromPeer}, peer, headers)
		normalized(fromPeer)
		if fromPeer != ParseAddress(peer) || fromPeer != resolve(ClientIPSource{From: FromPeer}, peer, nil) {
			t.Fatalf("peer %q gave %v, depending on the headers", peer, fromPeer)
		}

		fromHeader := resolve(ClientIPSource{From: FromHeader, Header: "X-Client"}, peer, headers)
		normalized(fromHeader)
		if fromHeader != ParseAddress(header) || fromHeader != resolve(ClientIPSource{From: FromHeader, Header: "x-client"}, "", map[string]string{"x-client": header}) {
			t.Fatalf("header %q gave %v, depending on another source", header, fromHeader)
		}
		if strings.Contains(header, ",") && fromHeader.IsValid() {
			t.Fatalf("header %q with two entries gave %v", header, fromHeader)
		}

		if hops < 0 || hops > 64 {
			hops %= 65
			if hops < 0 {
				hops = -hops
			}
		}
		s := ClientIPSource{From: FromXForwardedFor, TrustedHops: hops}
		fromXFF := resolve(s, peer, headers)
		normalized(fromXFF)
		if fromXFF != resolve(s, "", map[string]string{"x-forwarded-for": xff}) {
			t.Fatalf("X-Forwarded-For %q gave %v, depending on another source", xff, fromXFF)
		}
		entries := strings.Split(xff, ",")
		if len(entries) <= hops {
			if fromXFF.IsValid() {
				t.Fatalf("X-Forwarded-For %q with %d trusted hops gave %v", xff, hops, fromXFF)
			}
		} else {
			if want := ParseAddress(entries[len(entries)-1-hops]); fromXFF != want {
				t.Fatalf("X-Forwarded-For %q with %d trusted hops gave %v, want %v", xff, hops, fromXFF, want)
			}
			if spoofed := resolve(s, peer, map[string]string{"x-forwarded-for": prefix + "," + xff}); spoofed != fromXFF {
				t.Fatalf("prepending %q to X-Forwarded-For %q moved the address from %v to %v", prefix, xff, fromXFF, spoofed)
			}
		}
	})
}

func ptr(s string) *string { return &s }

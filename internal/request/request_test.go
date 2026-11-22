package request

import (
	"maps"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalize(t *testing.T) {
	got := Normalize(Raw{
		Method:  "post",
		Host:    "API.Example.com:8443",
		Path:    "/v1/Webhooks?token=x&b=%2F?c",
		Scheme:  "HTTPS",
		Headers: map[string]string{"user-agent": "curl/8.0", "x-forwarded-for": "198.51.100.7,203.0.113.10", "X-Request-Id": "d2c1e2a0"},
		Peer:    "203.0.113.10",
	})
	want := &Request{
		Method:  "POST",
		Host:    "api.example.com",
		Path:    "/v1/Webhooks",
		Query:   "token=x&b=%2F?c",
		Scheme:  "https",
		Headers: map[string]string{"user-agent": "curl/8.0", "x-forwarded-for": "198.51.100.7,203.0.113.10", "x-request-id": "d2c1e2a0"},
		ID:      "d2c1e2a0",
		Peer:    netip.MustParseAddr("203.0.113.10"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if empty := Normalize(Raw{}); !reflect.DeepEqual(empty, &Request{Headers: map[string]string{}}) {
		t.Fatalf("empty request: got %+v", empty)
	}
}

func TestNormalizeHost(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"www.example.com", "www.example.com"},
		{"WWW.Example.COM", "www.example.com"},
		{"www.example.com:443", "www.example.com"},
		{"www.example.com:", "www.example.com"},
		{"www.example.com.", "www.example.com."},
		{"www.example.com.:8080", "www.example.com."},
		{"192.0.2.10:8080", "192.0.2.10"},
		{"[2001:DB8::1]:8443", "[2001:db8::1]"},
		{"[2001:db8::1]", "[2001:db8::1]"},
		{"[2001:db8::1]:", "[2001:db8::1]"},
		// Not a port: the host stays as it is, so that it matches no host
		// written in its usual form.
		{"www.example.com:https", "www.example.com:https"},
		{"www.example.com:80:80", "www.example.com:80:80"},
		{"2001:db8::1", "2001:db8::1"},
		{"[2001:db8::1]:80:80", "[2001:db8::1]:80:80"},
		// Only ASCII letters change case.
		{"Key.example.com", "Key.example.com"},
		{"", ""},
	} {
		if got := Normalize(Raw{Host: tc.raw}).Host; got != tc.want {
			t.Errorf("host %q: got %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestNormalizePathAndQuery(t *testing.T) {
	for _, tc := range []struct{ raw, path, query string }{
		{"/", "/", ""},
		{"/a/b", "/a/b", ""},
		{"/a?", "/a", ""},
		{"/a?b=1&c=2", "/a", "b=1&c=2"},
		{"/a?b=?", "/a", "b=?"},
		{"?b=1", "", "b=1"},
		// The path is not decoded or normalized.
		{"/a/../b//c/%2e%2E/%2F", "/a/../b//c/%2e%2E/%2F", ""},
		{"/A#f", "/A#f", ""},
		{"", "", ""},
	} {
		r := Normalize(Raw{Path: tc.raw})
		if r.Path != tc.path || r.Query != tc.query {
			t.Errorf("target %q: path %q and query %q, want %q and %q", tc.raw, r.Path, r.Query, tc.path, tc.query)
		}
	}
}

func TestNormalizeMethodAndScheme(t *testing.T) {
	r := Normalize(Raw{Method: "dElEte", Scheme: "Http"})
	if r.Method != "DELETE" || r.Scheme != "http" {
		t.Fatalf("method %q, scheme %q", r.Method, r.Scheme)
	}
	// Only ASCII letters change case: ß has an uppercase form, ı a lowercase
	// one, and neither is a letter Envoy would match.
	r = Normalize(Raw{Method: "gEtß", Scheme: "HTTPı"})
	if r.Method != "GETß" || r.Scheme != "httpı" {
		t.Fatalf("method %q, scheme %q", r.Method, r.Scheme)
	}
}

func TestNormalizeHeaders(t *testing.T) {
	got := Normalize(Raw{Headers: map[string]string{
		"x-api-key":   "k1",
		"X-Api-Key":   "k2",
		"X-API-KEY":   "k3",
		"Accept":      "text/html",
		"x-empty":     "",
		":authority":  "www.example.com",
		"x-multi":     "a,b, c",
		"x-not-utf-8": "a\xffb\xc3",
		"x-\xfe":      "v",
	}}).Headers
	want := map[string]string{
		// The name already in lowercase first, then the others in the order
		// of their original spelling.
		"x-api-key":   "k1,k3,k2",
		"accept":      "text/html",
		"x-empty":     "",
		":authority":  "www.example.com",
		"x-multi":     "a,b, c",
		"x-not-utf-8": "a!b!",
		"x-!":         "v",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestNormalizeReplacesInvalidUTF8(t *testing.T) {
	r := Normalize(Raw{Method: "G\xffT", Host: "a\xff.example.com", Path: "/\xc0\xaf?q=\xff", Scheme: "ht\xfftp", Headers: map[string]string{"x-request-id": "id\xff"}})
	want := &Request{Method: "G!T", Host: "a!.example.com", Path: "/!!", Query: "q=!", Scheme: "ht!tp", Headers: map[string]string{"x-request-id": "id!"}, ID: "id!"}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("got %+v\nwant %+v", r, want)
	}
}

func TestNormalizePeer(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"203.0.113.10", "203.0.113.10"},
		{"2001:db8::10", "2001:db8::10"},
		{"::ffff:203.0.113.10", "203.0.113.10"},
		{"", ""},
		{"/run/envoy.sock", ""},
		{"0.0.0.0", ""},
	} {
		got := Normalize(Raw{Peer: tc.raw}).Peer
		if tc.want == "" && got.IsValid() || tc.want != "" && got != netip.MustParseAddr(tc.want) {
			t.Errorf("peer %q: got %v, want %q", tc.raw, got, tc.want)
		}
	}
}

// FuzzNormalize checks the forms that Normalize promises, whatever the proxy
// sends, and that normalizing twice changes nothing.
func FuzzNormalize(f *testing.F) {
	f.Add("get", "WWW.Example.com:8443", "/a/b?c=d", "HTTPS", "X-Forwarded-For", "198.51.100.7, 203.0.113.10", "203.0.113.10")
	f.Add("DELETE", "[2001:db8::1]:443", "/admin?", "http", "x-a", "", "2001:db8::1")
	f.Add("\xff", "a:b:c", "??", "", "\xfe", "\xc3", "[::ffff:192.0.2.1]:80")
	f.Add("", "example.com.:", "", "h", "x", "y", "")
	f.Fuzz(func(t *testing.T, method, host, path, scheme, name, value, peer string) {
		raw := Raw{Method: method, Host: host, Path: path, Scheme: scheme, Headers: map[string]string{name: value, "x-other": "1"}, Peer: peer}
		r := Normalize(raw)
		for _, s := range []string{r.Method, r.Host, r.Path, r.Query, r.Scheme, r.ID} {
			if !utf8.ValidString(s) {
				t.Fatalf("invalid UTF-8 in %+v", r)
			}
		}
		if strings.ContainsFunc(r.Method, func(c rune) bool { return c >= 'a' && c <= 'z' }) {
			t.Fatalf("method %q has a lowercase letter", r.Method)
		}
		for _, s := range []string{r.Host, r.Scheme} {
			if strings.ContainsFunc(s, func(c rune) bool { return c >= 'A' && c <= 'Z' }) {
				t.Fatalf("%q has an uppercase letter", s)
			}
		}
		if strings.Contains(r.Path, "?") {
			t.Fatalf("path %q has a '?'", r.Path)
		}
		if target := validUTF8(path); strings.Contains(target, "?") && r.Path+"?"+r.Query != target || !strings.Contains(target, "?") && (r.Path != target || r.Query != "") {
			t.Fatalf("target %q split into %q and %q", target, r.Path, r.Query)
		}
		if i := strings.LastIndexByte(r.Host, ':'); i >= 0 && !strings.Contains(r.Host[:i], ":") {
			if port := r.Host[i+1:]; !strings.ContainsFunc(port, func(c rune) bool { return c < '0' || c > '9' }) {
				t.Fatalf("host %q keeps a port", r.Host)
			}
		}
		if len(r.Headers) == 0 || len(r.Headers) > 2 {
			t.Fatalf("%d headers from 2", len(r.Headers))
		}
		if r.ID != r.Headers["x-request-id"] {
			t.Fatalf("request ID %q, header %q", r.ID, r.Headers["x-request-id"])
		}
		for n, v := range r.Headers {
			if !utf8.ValidString(n) || !utf8.ValidString(v) || strings.ContainsFunc(n, func(c rune) bool { return c >= 'A' && c <= 'Z' }) {
				t.Fatalf("header %q: %q", n, v)
			}
		}
		if r.Peer.IsValid() && (r.Peer.Is4In6() || r.Peer.Zone() != "" || r.Peer.IsUnspecified()) {
			t.Fatalf("peer %v is not normalized", r.Peer)
		}
		var again Raw
		again.Method, again.Host, again.Path, again.Scheme, again.Headers = r.Method, r.Host, r.Path, r.Scheme, r.Headers
		if r.Peer.IsValid() {
			again.Peer = r.Peer.String()
		}
		twice := Normalize(again)
		twice.Query = r.Query
		if !reflect.DeepEqual(twice, r) {
			t.Fatalf("normalized twice: %+v\nonce: %+v", twice, r)
		}
	})
}

// Package request holds the HTTP request that tessera decides, in a form
// independent of the protocol that carried it, and reads the client address
// of a request from the one source that the configuration declares.
package request

import (
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Raw is a request as the proxy describes it, before normalization.
type Raw struct {
	Method string
	// Host is the Host header or the :authority pseudo-header, with any port.
	Host string
	// Path is the request target as the proxy forwards it, with its query.
	Path   string
	Scheme string
	// Headers maps each header name to its value. The proxy joins the
	// values of a repeated header with a comma.
	Headers map[string]string
	// Peer is the address of the connection to the proxy.
	Peer string
}

// Request is a normalized request. Its strings are valid UTF-8: Normalize
// replaces each byte that is not with '!', as Envoy does in the headers it
// describes.
type Request struct {
	// Method is in uppercase.
	Method string
	// Host is in lowercase, without the port. An IPv6 address keeps its
	// brackets.
	Host string
	// Path is the request target up to its first '?', as the proxy forwards
	// it: not decoded and not normalized.
	Path string
	// Query follows the first '?' of the target, without it; it is empty
	// when the target has none.
	Query string
	// Scheme is in lowercase.
	Scheme string
	// Headers has the names in lowercase. It is never nil.
	Headers map[string]string
	// ID is the value of the header x-request-id, empty without it.
	ID string
	// Peer is the address of the connection to the proxy; the zero Addr
	// when the proxy gave none, or none that ParseAddress accepts.
	Peer netip.Addr
	// ClientIP is the address of the client; the zero Addr when the
	// declared source gave none. Normalize leaves it zero: see
	// ClientIPSource.Resolve.
	ClientIP netip.Addr
	// Received is when the proxy's description of the request arrived, from
	// which the time of its decision counts; zero when unknown. Normalize
	// leaves it zero.
	Received time.Time
}

// Normalize returns the normalized form of raw.
func Normalize(raw Raw) *Request {
	path, query, _ := strings.Cut(validUTF8(raw.Path), "?")
	h := headers(raw.Headers)
	return &Request{
		Method:  upper(validUTF8(raw.Method)),
		Host:    withoutPort(lower(validUTF8(raw.Host))),
		Path:    path,
		Query:   query,
		Scheme:  lower(validUTF8(raw.Scheme)),
		Headers: h,
		ID:      h[requestIDHeader],
		Peer:    ParseAddress(raw.Peer),
	}
}

// requestIDHeader is the header of the request ID, which Envoy generates
// when a request has none.
const requestIDHeader = "x-request-id"

// headers returns raw with the names in lowercase, as Envoy sends them. The
// values of names that are equal once lowered are joined with a comma: the
// value of the name already in lowercase first, then the others in the order
// of their original names, so that the result does not depend on the order
// of the map.
func headers(raw map[string]string) map[string]string {
	out := make(map[string]string, len(raw))
	var others []string
	for name, value := range raw {
		if lower(name) == name && utf8.ValidString(name) {
			out[name] = validUTF8(value)
		} else {
			others = append(others, name)
		}
	}
	slices.Sort(others)
	joined := map[string][]string{}
	for _, name := range others {
		lowered := lower(validUTF8(name))
		if previous, ok := out[lowered]; ok && joined[lowered] == nil {
			joined[lowered] = []string{previous}
		}
		joined[lowered] = append(joined[lowered], validUTF8(raw[name]))
	}
	for name, values := range joined {
		out[name] = strings.Join(values, ",")
	}
	return out
}

// withoutPort removes from a host a port of decimal digits, which can be
// empty. A host without brackets that has another ':', such as an IPv6
// address, keeps its last group; any other text after the last ':' is not a
// port, and stays.
func withoutPort(host string) string {
	i := strings.LastIndexByte(host, ':')
	if i < 0 || strings.ContainsFunc(host[i+1:], func(r rune) bool { return r < '0' || r > '9' }) {
		return host
	}
	name := host[:i]
	if strings.IndexByte(name, ':') >= 0 && (name[0] != '[' || name[len(name)-1] != ']') {
		return host
	}
	return name
}

// validUTF8 replaces each byte of s that is not part of valid UTF-8 with '!'.
func validUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte('!')
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// lower maps the ASCII letters of s to lowercase, and nothing else: a
// character such as the Kelvin sign must not become a 'k' that Envoy never
// saw.
func lower(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r >= 'A' && r <= 'Z' }) {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// upper maps the ASCII letters of s to uppercase, and nothing else.
func upper(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r >= 'a' && r <= 'z' }) {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - ('a' - 'A')
		}
	}
	return string(b)
}

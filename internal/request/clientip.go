package request

import (
	"net/netip"
	"strings"
)

// Source names where the client address comes from: a value of
// client_ip.from.
type Source string

// The sources of the client address.
const (
	// FromPeer is the address of the connection to the proxy.
	FromPeer Source = "peer"
	// FromHeader is a header that holds exactly one address.
	FromHeader Source = "header"
	// FromXForwardedFor is the X-Forwarded-For header, read from the right.
	FromXForwardedFor Source = "x_forwarded_for"
)

// ClientIPSource is the one source of the client address: the client_ip
// block of the configuration.
type ClientIPSource struct {
	From Source
	// Header is the header of FromHeader, in any case.
	Header string
	// TrustedHops is the number of entries of FromXForwardedFor, at its right
	// end, that trusted proxies wrote after the client address.
	TrustedHops int
}

const xForwardedFor = "x-forwarded-for"

// Resolve returns the client address of r from s alone, or the zero Addr
// when s gives no address that ParseAddress accepts: it never falls back on
// another source. An unknown source gives no address.
func (s ClientIPSource) Resolve(r *Request) netip.Addr {
	switch s.From {
	case FromPeer:
		return r.Peer
	case FromHeader:
		return ParseAddress(r.Headers[lower(s.Header)])
	case FromXForwardedFor:
		return fromXForwardedFor(r.Headers[xForwardedFor], s.TrustedHops)
	}
	return netip.Addr{}
}

// fromXForwardedFor skips the trustedHops entries at the right end of the
// comma-separated list value, and parses the entry before them. The entries
// on its left are whatever the client sent, and are never read. Too few
// entries give no address.
func fromXForwardedFor(value string, trustedHops int) netip.Addr {
	if trustedHops < 0 {
		return netip.Addr{}
	}
	end := len(value)
	for range trustedHops {
		end = strings.LastIndexByte(value[:end], ',')
		if end < 0 {
			return netip.Addr{}
		}
	}
	return ParseAddress(value[strings.LastIndexByte(value[:end], ',')+1 : end])
}

// ParseAddress parses one address as proxies write it: an IPv4 or IPv6
// address, an IPv4 address with a port, or an IPv6 address in brackets, with
// or without a port, with spaces or tabs around. It drops the port, and turns
// an IPv4-mapped IPv6 address into the IPv4 address. It returns the zero Addr
// for anything else: an IPv6 address with a zone, whose text is free and
// whose meaning is local to a host, and an unspecified address (0.0.0.0,
// ::), which no client has.
func ParseAddress(s string) netip.Addr {
	s = strings.Trim(s, " \t")
	var addr netip.Addr
	switch {
	case s == "":
		return netip.Addr{}
	case s[0] == '[' && s[len(s)-1] == ']':
		a, err := netip.ParseAddr(s[1 : len(s)-1])
		if err != nil || !a.Is6() {
			return netip.Addr{}
		}
		addr = a
	case s[0] == '[':
		ap, err := netip.ParseAddrPort(s)
		if err != nil {
			return netip.Addr{}
		}
		addr = ap.Addr()
	default:
		a, err := netip.ParseAddr(s)
		if err != nil {
			ap, err := netip.ParseAddrPort(s)
			if err != nil {
				return netip.Addr{}
			}
			a = ap.Addr()
		}
		addr = a
	}
	if addr.Zone() != "" || addr.Unmap().IsUnspecified() {
		return netip.Addr{}
	}
	return addr.Unmap()
}

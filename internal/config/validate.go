package config

import (
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

func (c *Config) validate() error {
	s := c.Server
	if socket, ok := strings.CutPrefix(s.Listen, UnixPrefix); ok {
		if socket == "" || !printable(socket) {
			return c.err("server.listen", "expected host:port or unix: followed by a path")
		}
	} else if !validListener(s.Listen) {
		return c.err("server.listen", "expected host:port or unix: followed by a path")
	}
	if s.MaxConcurrentStreams < 1 || int64(s.MaxConcurrentStreams) > math.MaxUint32 {
		return c.err("server.max_concurrent_streams", "expected an integer from 1 to 4294967295")
	}
	if s.MaxConnectionAge < 0 {
		return c.err("server.max_connection_age", "must not be negative")
	}
	if s.DrainDelay < 0 {
		return c.err("server.drain_delay", "must not be negative")
	}
	if s.ShutdownTimeout <= 0 {
		return c.err("server.shutdown_timeout", "must be positive")
	}
	if !validListener(c.Admin.Listen) {
		return c.err("admin.listen", "expected host:port")
	}
	if listenersOverlap(s.Listen, c.Admin.Listen) {
		if !c.present("admin.listen") {
			return c.err("server.listen", "must not share its address and port with admin.listen")
		}
		return c.err("admin.listen", "must not share its address and port with server.listen")
	}
	switch c.Log.Level {
	case LevelDebug, LevelInfo, LevelWarn, LevelError:
	default:
		return c.err("log.level", "expected debug, info, warn or error")
	}
	switch c.Log.Format {
	case FormatLogfmt, FormatJSON:
	default:
		return c.err("log.format", "expected logfmt or json")
	}
	if !c.present("policy.default_action") {
		return c.err("policy.default_action", "required")
	}
	switch c.Policy.DefaultAction {
	case ActionAllow, ActionDeny:
	default:
		return c.err("policy.default_action", "expected allow or deny")
	}
	return nil
}

// printable reports whether s has no control characters.
func printable(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// validListener reports whether s is host:port with a port from 1 to 65535
// and a host that is empty, an IP address or a DNS name.
func validListener(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil || !canonicalPort(port) {
		return false
	}
	if host == "" {
		return true
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return dnsName(host)
}

// canonicalPort reports whether p is a decimal port from 1 to 65535, without
// a sign or leading zeros.
func canonicalPort(p string) bool {
	if p == "" || len(p) > 5 || p[0] == '0' || strings.ContainsFunc(p, func(r rune) bool { return r < '0' || r > '9' }) {
		return false
	}
	n, _ := strconv.Atoi(p)
	return n <= 65535
}

// dnsName reports whether s is a host name of ASCII letters, digits and
// hyphens, in labels of 1 to 63 characters that neither start nor end with a
// hyphen. The last label is not all digits, so that a name is never an
// address in a short form, such as 127.1.
func dnsName(s string) bool {
	last := s[strings.LastIndexByte(s, '.')+1:]
	if len(s) > 253 || !strings.ContainsFunc(last, func(r rune) bool { return r < '0' || r > '9' }) {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		if strings.ContainsFunc(label, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-')
		}) {
			return false
		}
	}
	return true
}

// listenersOverlap reports whether two listeners would compete for a socket:
// both on TCP with equal ports, and equal hosts, a wildcard host on either
// side, or loopback hosts on both sides.
func listenersOverlap(a, b string) bool {
	ah, ap, aerr := net.SplitHostPort(a)
	bh, bp, berr := net.SplitHostPort(b)
	if aerr != nil || berr != nil || strings.HasPrefix(a, UnixPrefix) || ap != bp {
		return false
	}
	if strings.EqualFold(ah, bh) || wildcardHost(ah) || wildcardHost(bh) {
		return true
	}
	ai, aerr := netip.ParseAddr(ah)
	bi, berr := netip.ParseAddr(bh)
	if aerr == nil && berr == nil && ai.Unmap() == bi.Unmap() {
		return true
	}
	return loopbackHost(ah) && loopbackHost(bh)
}

func wildcardHost(h string) bool {
	ip, err := netip.ParseAddr(h)
	return h == "" || err == nil && ip.Unmap().IsUnspecified()
}

func loopbackHost(h string) bool {
	ip, err := netip.ParseAddr(h)
	return strings.EqualFold(h, "localhost") || err == nil && ip.Unmap().IsLoopback()
}

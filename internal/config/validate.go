package config

import (
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/gtriggiano/tessera/internal/sets"
)

func (c *Config) validate() error {
	s := c.Server
	if socket, ok := strings.CutPrefix(s.Listen, UnixPrefix); ok {
		if socket == "" || !printable(socket) {
			return c.err("server.listen", "expected host:port or unix: followed by a path")
		}
		if strings.HasPrefix(socket, "@") {
			return c.err("server.listen", "abstract Unix sockets are not supported; write a path")
		}
	} else if !validListener(s.Listen) {
		return c.err("server.listen", "expected host:port or unix: followed by a path")
	}
	if s.MaxConnections < 1 || s.MaxConnections > MaxConnectionsLimit {
		return c.err("server.max_connections", "expected an integer from 1 to "+strconv.Itoa(MaxConnectionsLimit))
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
	switch c.Log.Decisions {
	case DecisionsNone, DecisionsDeny, DecisionsAll:
	default:
		return c.err("log.decisions", "expected none, deny or all")
	}
	if err := c.validateClientIP(); err != nil {
		return err
	}
	if err := c.validateSets(); err != nil {
		return err
	}
	return c.validatePolicy()
}

// validateSets checks each set, in the order of the file. The files of the
// sets are not read here: their loader reports their errors with ErrorAt.
func (c *Config) validateSets() error {
	for _, k := range c.keys["sets"] {
		if err := c.validateSet(k.name); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateSet(name string) error {
	s, p := c.Sets[name], childPath("sets", name)
	switch {
	case !c.present(p + ".type"):
		return c.err(p+".type", "required")
	case s.Type != SetTypeCIDR && s.Type != SetTypeASN && s.Type != SetTypeString:
		return c.err(p+".type", "expected cidr, asn or string")
	}
	values, file := c.present(p+".values"), c.present(p+".file")
	switch {
	case !values && !file:
		return c.err(p, "needs a source: values or file")
	case values && file:
		later := ".file"
		if c.nodes[p+".values"].line > c.nodes[p+".file"].line {
			later = ".values"
		}
		return c.err(p+later, "values and file are both set: a set has one source")
	}
	if s.MaxEntries < 1 {
		return c.err(p+".max_entries", "must be positive")
	}
	if values {
		for _, key := range []string{"refresh", "max_staleness", "on_unavailable"} {
			if c.present(p + "." + key) {
				return c.err(p+"."+key, "allowed only with file: a set of values is always available")
			}
		}
		if len(s.Values) > s.MaxEntries {
			return c.err(p+".values", "more values than max_entries")
		}
		for i, v := range s.Values {
			item := itemPath(p+".values", i)
			if c.integers[item] && s.Type != SetTypeASN {
				return c.err(item, "expected a string")
			}
			if err := sets.ParseValue(sets.Type(s.Type), string(v)); err != nil {
				return c.err(item, err.Error())
			}
		}
		return nil
	}
	switch {
	case s.File == "":
		return c.err(p+".file", "must not be empty")
	case !printable(s.File):
		return c.err(p+".file", "expected a path")
	case s.Refresh < MinSetRefresh:
		return c.err(p+".refresh", "must be at least 1s")
	case s.MaxStaleness < 0:
		return c.err(p+".max_staleness", "must not be negative")
	case s.MaxStaleness != 0 && s.MaxStaleness <= s.Refresh:
		// The set would be unavailable before each check.
		return c.err(p+".max_staleness", "must be longer than refresh, or 0s for no limit")
	}
	switch s.OnUnavailable {
	case OnUnavailableFail, OnUnavailableMatch, OnUnavailableNoMatch:
	default:
		return c.err(p+".on_unavailable", "expected fail, match or no_match")
	}
	return nil
}

// SetDeclarations returns the sets as the expressions declare them, in the
// order of the file.
func (c *Config) SetDeclarations() []sets.Declaration {
	out := make([]sets.Declaration, 0, len(c.Sets))
	for _, k := range c.keys["sets"] {
		s := c.Sets[k.name]
		source := sets.SourceValues
		if s.File != "" {
			source = sets.SourceFile
		}
		out = append(out, sets.Declaration{Name: k.name, Type: sets.Type(s.Type), Source: source, OnUnavailable: sets.OnUnavailable(s.OnUnavailable)})
	}
	return out
}

func (c *Config) validateClientIP() error {
	from := c.ClientIP.From
	switch from {
	case ClientIPPeer, ClientIPHeader, ClientIPXForwardedFor:
	default:
		return c.err("client_ip.from", "expected peer, header or x_forwarded_for")
	}
	switch {
	case from == ClientIPHeader && !c.present("client_ip.header"):
		return c.err("client_ip.header", "required when client_ip.from is header")
	case from == ClientIPHeader && !headerName(c.ClientIP.Header):
		return c.err("client_ip.header", "expected a header name")
	case from != ClientIPHeader && c.present("client_ip.header"):
		return c.err("client_ip.header", "allowed only when client_ip.from is header")
	}
	switch {
	case from == ClientIPXForwardedFor && !c.present("client_ip.trusted_hops"):
		return c.err("client_ip.trusted_hops", "required when client_ip.from is x_forwarded_for")
	case from == ClientIPXForwardedFor && c.ClientIP.TrustedHops < 0:
		return c.err("client_ip.trusted_hops", "must not be negative")
	case from != ClientIPXForwardedFor && c.present("client_ip.trusted_hops"):
		return c.err("client_ip.trusted_hops", "allowed only when client_ip.from is x_forwarded_for")
	}
	return nil
}

// headerName reports whether s is a field name of HTTP: a token of RFC 9110.
func headerName(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r))
	})
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

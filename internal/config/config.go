// Package config reads, checks and prints tessera's configuration file.
//
// The YAML field names, defaults and allowed values of these types are a
// public interface: the configuration reference documents each of them.
package config

import (
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/gtriggiano/tessera/internal/sets"
)

// LookupEnv returns the value of an environment variable and whether it is
// set, like os.LookupEnv.
type LookupEnv func(string) (string, bool)

// Config is a valid configuration with its defaults applied.
type Config struct {
	Server   Server         `yaml:"server"`
	Admin    Admin          `yaml:"admin"`
	Log      Log            `yaml:"log"`
	ClientIP ClientIP       `yaml:"client_ip"`
	Sets     map[string]Set `yaml:"sets"`
	Policy   Policy         `yaml:"policy"`

	// nodes maps the path of each key read from the file to its position.
	nodes map[string]location
	// tainted holds the paths of the values that contain a ${...}
	// substitution.
	tainted map[string]bool
	// keys holds the keys of each mapping of strings or of named blocks, in
	// the order of the file, by the path of the mapping.
	keys map[string][]mapKey
	// integers holds the paths of the values of a list of set values that
	// the file writes as YAML integers.
	integers map[string]bool
	// warnings are the problems that do not make the configuration invalid.
	warnings []Warning
	// styles holds the YAML style of each expression, by its path, and lines
	// the lines of the file: they place an error in an expression.
	styles map[string]yaml.Style
	lines  []string
}

// mapKey is a key of a mapping of strings, where the file writes it.
type mapKey struct {
	name string
	at   location
}

// Server is the gRPC listener for Envoy.
type Server struct {
	// Listen is host:port, or unix: followed by the path of a socket. Load
	// makes a relative socket path absolute.
	Listen               string        `yaml:"listen"`
	MaxConnections       int           `yaml:"max_connections"`
	MaxConcurrentStreams int           `yaml:"max_concurrent_streams"`
	MaxConnectionAge     time.Duration `yaml:"max_connection_age"`
	DrainDelay           time.Duration `yaml:"drain_delay"`
	ShutdownTimeout      time.Duration `yaml:"shutdown_timeout"`
}

// Admin is the listener of the health checks and the metrics.
type Admin struct {
	Listen string `yaml:"listen"`
}

// Log selects the level and the format of the log events, and the decisions
// that are logged.
type Log struct {
	Level     string `yaml:"level"`
	Format    string `yaml:"format"`
	Decisions string `yaml:"decisions"`
}

// ClientIP is the one source of the client address of a request. Header
// belongs to the header source and TrustedHops to the X-Forwarded-For one:
// each is required with its source, and an error with another.
type ClientIP struct {
	From        string `yaml:"from"`
	Header      string `yaml:"header"`
	TrustedHops int    `yaml:"trusted_hops"`
}

// MarshalYAML writes the keys of the selected source only, so that the
// printed configuration reads back.
func (c ClientIP) MarshalYAML() (any, error) {
	switch c.From {
	case ClientIPHeader:
		return struct {
			From   string `yaml:"from"`
			Header string `yaml:"header"`
		}{c.From, c.Header}, nil
	case ClientIPXForwardedFor:
		return struct {
			From        string `yaml:"from"`
			TrustedHops int    `yaml:"trusted_hops"`
		}{c.From, c.TrustedHops}, nil
	}
	return struct {
		From string `yaml:"from"`
	}{c.From}, nil
}

// Set is a named set: its type and its one source, values or file. Refresh,
// MaxStaleness and OnUnavailable belong to a file: they are errors with
// values, whose set is always available.
type Set struct {
	Type   string     `yaml:"type"`
	Values []SetValue `yaml:"values"`
	// File is the path of the file. Load makes a relative path absolute.
	File          string        `yaml:"file"`
	Refresh       time.Duration `yaml:"refresh"`
	MaxStaleness  time.Duration `yaml:"max_staleness"`
	OnUnavailable string        `yaml:"on_unavailable"`
	MaxEntries    int           `yaml:"max_entries"`
}

// SetValue is a value of sets.<name>.values, as the file writes it: a
// string or, for an asn set, a YAML integer too, kept as written.
type SetValue string

func (s *Set) setDefaults() {
	s.Refresh = DefaultSetRefresh
	s.OnUnavailable = OnUnavailableFail
	s.MaxEntries = DefaultMaxEntries
}

// MarshalYAML writes the keys of the source of the set only, so that the
// printed configuration reads back.
func (s Set) MarshalYAML() (any, error) {
	if s.File == "" {
		return struct {
			Type       string     `yaml:"type"`
			Values     []SetValue `yaml:"values"`
			MaxEntries int        `yaml:"max_entries"`
		}{s.Type, s.Values, s.MaxEntries}, nil
	}
	return struct {
		Type          string        `yaml:"type"`
		File          string        `yaml:"file"`
		Refresh       time.Duration `yaml:"refresh"`
		MaxStaleness  time.Duration `yaml:"max_staleness"`
		OnUnavailable string        `yaml:"on_unavailable"`
		MaxEntries    int           `yaml:"max_entries"`
	}{s.Type, s.File, s.Refresh, s.MaxStaleness, s.OnUnavailable, s.MaxEntries}, nil
}

// Policy holds the decision rules.
type Policy struct {
	Mode          string   `yaml:"mode"`
	DefaultAction string   `yaml:"default_action"`
	DenyResponse  Response `yaml:"deny_response"`
	Rules         []Rule   `yaml:"rules"`
}

// Response is what Envoy answers the client when a request is denied.
type Response struct {
	Status int    `yaml:"status"`
	Body   string `yaml:"body"`
	// Headers maps each header name, in any case, to its value.
	Headers map[string]string `yaml:"headers"`
}

// Expression is the text of a CEL expression.
type Expression string

// Rule is one rule of the policy. Once the configuration is valid, the
// Response of a deny rule is the response it sends: its own keys, and the
// others from policy.deny_response. An allow rule has none.
//
// The order of the fields is the order of the keys in every example of a
// rule, in config print and in the JSON Schema: TestRuleKeyOrder keeps the
// examples in it.
type Rule struct {
	Name        string     `yaml:"name"`
	Description string     `yaml:"description"`
	Action      string     `yaml:"action"`
	Mode        string     `yaml:"mode"`
	When        Expression `yaml:"when"`
	Response    Response   `yaml:"response"`

	// written records the keys of response in the file, which config print
	// writes alone.
	written struct{ response, status, body, headers bool }
}

func (r *Rule) setDefaults() {
	r.When = AlwaysTrue
	r.Mode = ModeEnforce
}

// MarshalYAML writes the response of the rule as the file wrote it, so that
// the printed configuration neither repeats policy.deny_response in every
// rule nor reads back differently.
func (r Rule) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	add := func(m *yaml.Node, key string, value any) error {
		var v yaml.Node
		if err := v.Encode(value); err != nil {
			return err
		}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &v)
		return nil
	}
	for _, kv := range []struct {
		key   string
		value any
	}{{"name", r.Name}, {"description", r.Description}, {"action", r.Action}, {"mode", r.Mode}, {"when", string(r.When)}} {
		if err := add(n, kv.key, kv.value); err != nil {
			return nil, err
		}
	}
	if !r.written.response {
		return n, nil
	}
	response := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, kv := range []struct {
		written bool
		key     string
		value   any
	}{{r.written.status, "status", r.Response.Status}, {r.written.body, "body", r.Response.Body}, {r.written.headers, "headers", r.Response.Headers}} {
		if kv.written {
			if err := add(response, kv.key, kv.value); err != nil {
				return nil, err
			}
		}
	}
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "response"}, response)
	return n, nil
}

// The values of log.level.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// The values of log.format.
const (
	FormatLogfmt = "logfmt"
	FormatJSON   = "json"
)

// The values of log.decisions.
const (
	DecisionsNone = "none"
	DecisionsDeny = "deny"
	DecisionsAll  = "all"
)

// The values of client_ip.from.
const (
	ClientIPPeer          = "peer"
	ClientIPHeader        = "header"
	ClientIPXForwardedFor = "x_forwarded_for"
)

// The values of sets.<name>.type.
const (
	SetTypeCIDR   = string(sets.CIDR)
	SetTypeASN    = string(sets.ASN)
	SetTypeString = string(sets.String)
)

// The values of sets.<name>.on_unavailable.
const (
	OnUnavailableFail    = string(sets.Fail)
	OnUnavailableMatch   = string(sets.Match)
	OnUnavailableNoMatch = string(sets.NoMatch)
)

// The defaults and limits of a set.
const (
	DefaultSetRefresh = 10 * time.Second
	MinSetRefresh     = time.Second
	DefaultMaxEntries = 1_000_000
	MaxSetNameLength  = 63
)

// The values of policy.default_action and policy.rules[].action.
const (
	ActionAllow = "allow"
	ActionDeny  = "deny"
)

// The values of policy.mode and policy.rules[].mode.
const (
	ModeEnforce = "enforce"
	ModeAudit   = "audit"
)

// AlwaysTrue is the condition of a rule without when.
const AlwaysTrue Expression = "true"

// The limits of the policy.
const (
	MaxBodyBytes        = 8 << 10
	MaxDescriptionBytes = 1024
	MaxRuleNameLength   = 63
	MaxRules            = 1000
)

// MaxConnectionsLimit is the largest value of server.max_connections.
const MaxConnectionsLimit = 65536

// UnixPrefix starts a server.listen value that names a Unix socket.
const UnixPrefix = "unix:"

func defaults() *Config {
	return &Config{
		Server: Server{
			Listen:               ":9001",
			MaxConnections:       1024,
			MaxConcurrentStreams: 1024,
			DrainDelay:           5 * time.Second,
			ShutdownTimeout:      20 * time.Second,
		},
		Admin:    Admin{Listen: ":9090"},
		Log:      Log{Level: LevelInfo, Format: FormatLogfmt, Decisions: DecisionsDeny},
		ClientIP: ClientIP{From: ClientIPPeer},
		Sets:     map[string]Set{},
		Policy: Policy{
			Mode:         ModeEnforce,
			DenyResponse: Response{Status: 403, Headers: map[string]string{}},
			Rules:        []Rule{},
		},
		nodes:    map[string]location{},
		tainted:  map[string]bool{},
		keys:     map[string][]mapKey{},
		integers: map[string]bool{},
		styles:   map[string]yaml.Style{},
	}
}

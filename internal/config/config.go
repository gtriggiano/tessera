// Package config reads, checks and prints tessera's configuration file.
//
// The YAML field names, defaults and allowed values of these types are a
// public interface: the configuration reference documents each of them.
package config

import "time"

// LookupEnv returns the value of an environment variable and whether it is
// set, like os.LookupEnv.
type LookupEnv func(string) (string, bool)

// Config is a valid configuration with its defaults applied.
type Config struct {
	Server Server `yaml:"server"`
	Admin  Admin  `yaml:"admin"`
	Log    Log    `yaml:"log"`
	Policy Policy `yaml:"policy"`

	// nodes maps the path of each key read from the file to its position.
	nodes map[string]location
	// tainted holds the paths of the values that contain a ${...}
	// substitution.
	tainted map[string]bool
}

// Server is the gRPC listener for Envoy.
type Server struct {
	// Listen is host:port, or unix: followed by the path of a socket. Load
	// makes a relative socket path absolute.
	Listen               string        `yaml:"listen"`
	MaxConcurrentStreams int           `yaml:"max_concurrent_streams"`
	MaxConnectionAge     time.Duration `yaml:"max_connection_age"`
	DrainDelay           time.Duration `yaml:"drain_delay"`
	ShutdownTimeout      time.Duration `yaml:"shutdown_timeout"`
}

// Admin is the listener of the health checks and the metrics.
type Admin struct {
	Listen string `yaml:"listen"`
}

// Log selects the level and the format of the log events.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Policy holds the decision rules.
type Policy struct {
	DefaultAction string `yaml:"default_action"`
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

// The values of policy.default_action.
const (
	ActionAllow = "allow"
	ActionDeny  = "deny"
)

// UnixPrefix starts a server.listen value that names a Unix socket.
const UnixPrefix = "unix:"

func defaults() *Config {
	return &Config{
		Server: Server{
			Listen:               ":9001",
			MaxConcurrentStreams: 1024,
			DrainDelay:           5 * time.Second,
			ShutdownTimeout:      20 * time.Second,
		},
		Admin:   Admin{Listen: ":9090"},
		Log:     Log{Level: LevelInfo, Format: FormatLogfmt},
		nodes:   map[string]location{},
		tainted: map[string]bool{},
	}
}

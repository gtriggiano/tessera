// Package telemetry writes tessera's log events.
//
// An event is one line: logfmt, as key=value pairs, or a JSON object. It
// starts with ts, the UTC time in RFC 3339 with milliseconds, level and msg,
// a stable name rather than a sentence, followed by the attributes of the
// event. The event names and their attributes are a public interface. The
// handlers of log/slog quote and escape every value, so a value cannot start
// a second line.
//
// NewLogger writes events as they happen. A Recorder writes the events about
// decisions without ever making a decision wait for the output: the decision
// events through a bounded queue, and the reports of dropped events,
// indeterminate conditions and recovered panics, rate-limited, from its
// writer goroutine.
package telemetry

import (
	"io"
	"log/slog"
	"strings"

	"github.com/gtriggiano/tessera/internal/config"
)

// timeLayout is RFC 3339 with milliseconds; a UTC time ends in Z.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// NewLogger returns a logger that writes to w the events of cfg.Level and
// above, in cfg.Format. cfg comes from a valid configuration.
func NewLogger(w io.Writer, cfg config.Log) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level(cfg.Level), ReplaceAttr: builtIn}
	if cfg.Format == config.FormatJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func level(name string) slog.Level {
	switch name {
	case config.LevelDebug:
		return slog.LevelDebug
	case config.LevelWarn:
		return slog.LevelWarn
	case config.LevelError:
		return slog.LevelError
	}
	return slog.LevelInfo
}

// builtIn writes the time as ts and the level in lowercase.
func builtIn(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		return slog.String("ts", a.Value.Time().UTC().Format(timeLayout))
	case slog.LevelKey:
		if l, ok := a.Value.Any().(slog.Level); ok {
			return slog.String(slog.LevelKey, strings.ToLower(l.String()))
		}
	}
	return a
}

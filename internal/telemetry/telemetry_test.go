package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/config"
)

// at is a local time with nanoseconds: an event writes it in UTC, to the
// millisecond.
var at = time.Date(2026, 11, 21, 11, 15, 2, 123456789, time.FixedZone("CET", 3600))

// event writes one event at the time at with logger.
func event(t *testing.T, logger *slog.Logger, level slog.Level, msg string, attrs ...slog.Attr) {
	t.Helper()
	if !logger.Enabled(context.Background(), level) {
		return
	}
	r := slog.NewRecord(at, level, msg, 0)
	r.AddAttrs(attrs...)
	if err := logger.Handler().Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func TestLogfmt(t *testing.T) {
	var out bytes.Buffer
	logger := NewLogger(&out, config.Log{Level: config.LevelInfo, Format: config.FormatLogfmt})
	event(t, logger, slog.LevelInfo, "listening", slog.String("listener", "grpc"), slog.String("address", ":9001"))
	event(t, logger, slog.LevelWarn, "set_refresh_failed", slog.String("set", "office"), slog.Int("entries", 3), slog.Bool("kept", true))
	want := "ts=2026-11-21T10:15:02.123Z level=info msg=listening listener=grpc address=:9001\n" +
		"ts=2026-11-21T10:15:02.123Z level=warn msg=set_refresh_failed set=office entries=3 kept=true\n"
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestJSON(t *testing.T) {
	var out bytes.Buffer
	logger := NewLogger(&out, config.Log{Level: config.LevelInfo, Format: config.FormatJSON})
	event(t, logger, slog.LevelError, "panic_recovered", slog.String("rule", "office"), slog.Int("entries", 3))
	want := `{"ts":"2026-11-21T10:15:02.123Z","level":"error","msg":"panic_recovered","rule":"office","entries":3}` + "\n"
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
	var fields map[string]any
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
}

func TestLevels(t *testing.T) {
	levels := []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}
	for _, tc := range []struct {
		level string
		want  []string
	}{
		{config.LevelDebug, []string{"debug", "info", "warn", "error"}},
		{config.LevelInfo, []string{"info", "warn", "error"}},
		{config.LevelWarn, []string{"warn", "error"}},
		{config.LevelError, []string{"error"}},
	} {
		for _, format := range []string{config.FormatLogfmt, config.FormatJSON} {
			var out bytes.Buffer
			logger := NewLogger(&out, config.Log{Level: tc.level, Format: format})
			for _, l := range levels {
				event(t, logger, l, "probe")
			}
			var got []string
			for line := range strings.Lines(out.String()) {
				switch format {
				case config.FormatJSON:
					var fields struct{ Level string }
					if err := json.Unmarshal([]byte(line), &fields); err != nil {
						t.Fatal(err)
					}
					got = append(got, fields.Level)
				default:
					_, rest, _ := strings.Cut(line, " level=")
					name, _, _ := strings.Cut(rest, " ")
					got = append(got, name)
				}
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("log.level %s, %s: levels written %v, want %v", tc.level, format, got, tc.want)
			}
		}
	}
}

// A value that a client could write never starts a second event.
func TestValuesStayOnOneLine(t *testing.T) {
	hostile := "a\nts=2026-01-01T00:00:00.000Z level=error msg=forged\r  \x85\"=}"
	for _, tc := range []struct{ format, escaped string }{
		{config.FormatLogfmt, `host="a\nts=`},
		{config.FormatJSON, `"host":"a\nts=`},
	} {
		var out bytes.Buffer
		logger := NewLogger(&out, config.Log{Level: config.LevelInfo, Format: tc.format})
		event(t, logger, slog.LevelInfo, "decision", slog.String("host", hostile))
		if lines := strings.Count(out.String(), "\n"); lines != 1 || !strings.HasSuffix(out.String(), "\n") {
			t.Fatalf("%s: %d lines: %q", tc.format, lines, out.String())
		}
		for _, raw := range []string{"\r", " ", " "} {
			if strings.Contains(out.String(), raw) {
				t.Fatalf("%s: raw %q in %q", tc.format, raw, out.String())
			}
		}
		if !strings.Contains(out.String(), tc.escaped) {
			t.Fatalf("%s: the value is not quoted and escaped: %q", tc.format, out.String())
		}
	}
}

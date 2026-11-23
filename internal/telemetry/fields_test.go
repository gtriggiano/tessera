package telemetry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gtriggiano/tessera/internal/config"
	"github.com/gtriggiano/tessera/internal/engine"
)

// fieldsEngine decides with a policy that denies the requests to
// /v1/webhooks and allows the others, and declares decision fields.
func fieldsEngine(t *testing.T) *engine.Engine {
	t.Helper()
	e, err := engine.New(engine.Policy{DefaultAction: engine.Allow, DenyResponse: engine.Response{Status: 403}, Rules: []engine.Rule{
		{Name: "webhooks", When: `request.path == "/v1/webhooks"`, Action: engine.Deny, Response: engine.Response{Status: 403}},
	}, DecisionFields: []engine.NamedExpression{
		{Name: "zone", Expression: `request.headers["x-zone"]`},
		{Name: "agent", Expression: `request.headers["user-agent"]`},
		{Name: "missing", Expression: `request.headers["x-missing"]`},
		{Name: "address", Expression: `string(client.ip)`},
	}}, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// The decision fields follow duration_ms, in the order of their names, as
// strings cut to MaxValueBytes; a field whose expression fails is empty.
func TestDecisionFieldsInTheEvent(t *testing.T) {
	e := fieldsEngine(t)
	r := sampleRequest()
	r.Headers["user-agent"] = "curl/8.7.1 (x)"
	r.Headers["x-zone"] = strings.Repeat("☃", MaxValueBytes/3+1)
	cut := strings.Repeat("☃", MaxValueBytes/3)
	d := e.Decide(r)
	d.Duration = 0
	for _, tc := range []struct {
		format, want string
	}{
		{config.FormatLogfmt, `level=warn msg=decision request_id=6f1c0f6e host=api.example.com method=POST path=/v1/webhooks client_ip=198.51.100.7 action=deny enforced=true rule=webhooks reason=rule duration_ms=0 address=198.51.100.7 agent="curl/8.7.1 (x)" missing="" zone=` + cut},
		{config.FormatJSON, `{"level":"warn","msg":"decision","request_id":"6f1c0f6e","host":"api.example.com","method":"POST","path":"/v1/webhooks","client_ip":"198.51.100.7","action":"deny","enforced":true,"rule":"webhooks","reason":"rule","duration_ms":0,"address":"198.51.100.7","agent":"curl/8.7.1 (x)","missing":"","zone":"` + cut + `"}`},
	} {
		t.Run(tc.format, func(t *testing.T) {
			dest := &buffer{}
			rec := newRecorder(t, dest, RecorderOptions{Log: logConfig(tc.format, config.DecisionsAll), Fields: e.DecisionFields})
			rec.Decision(d, r)
			if err := rec.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := dest.lines()
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), tc.want)
			}
			if tc.format == config.FormatJSON {
				var fields map[string]any
				if err := json.Unmarshal([]byte(got[0]), &fields); err != nil {
					t.Fatal(err)
				}
			}
			if strings.Contains(dest.String(), "s3cr3t") {
				t.Fatal("an event repeats the query, a cookie or an authorization header")
			}
		})
	}
}

// The decision fields are computed only for the events that log.decisions
// and log.level select, on the goroutine of the decision: when Decision
// returns, they are computed or never will be.
func TestDecisionFieldsOnlyForWrittenEvents(t *testing.T) {
	e := fieldsEngine(t)
	allowed := sampleRequest()
	allowed.Path = "/"
	allow, deny := e.Decide(allowed), e.Decide(sampleRequest())
	for _, tc := range []struct {
		decisions, level string
		computed         int
	}{
		{config.DecisionsNone, config.LevelInfo, 0},
		{config.DecisionsDeny, config.LevelInfo, 1},
		{config.DecisionsAll, config.LevelInfo, 2},
		{config.DecisionsAll, config.LevelWarn, 1},
		{config.DecisionsDeny, config.LevelError, 0},
	} {
		computed := 0
		dest := &buffer{}
		rec := newRecorder(t, dest, RecorderOptions{
			Log: config.Log{Level: tc.level, Format: config.FormatLogfmt, Decisions: tc.decisions},
			Fields: func(d engine.Decision) []engine.Field {
				computed++
				return e.DecisionFields(d)
			},
		})
		rec.Decision(allow, allowed)
		rec.Decision(deny, sampleRequest())
		if computed != tc.computed {
			t.Errorf("log.decisions %s, log.level %s: fields computed %d times, want %d", tc.decisions, tc.level, computed, tc.computed)
		}
		_ = rec.Close(context.Background())
		if got := len(dest.lines()); got != tc.computed {
			t.Errorf("log.decisions %s, log.level %s: %d events, want %d", tc.decisions, tc.level, got, tc.computed)
		}
		for _, line := range dest.lines() {
			if !strings.HasSuffix(line, ` address=198.51.100.7 agent="" missing="" zone=""`) {
				t.Errorf("event without its fields: %s", line)
			}
		}
	}
}

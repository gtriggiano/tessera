package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/config"
	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/expr"
	"github.com/gtriggiano/tessera/internal/request"
)

// buffer is a destination safe for concurrent use.
type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// lines returns the events written so far, without their ts.
func (b *buffer) lines() []string {
	var out []string
	for line := range strings.Lines(b.String()) {
		out = append(out, tsRE.ReplaceAllString(strings.TrimSuffix(line, "\n"), ""))
	}
	return out
}

var tsRE = regexp.MustCompile(`^ts=\S+ |"ts":"[^"]+",`)

func newRecorder(t *testing.T, dest *buffer, o RecorderOptions) *Recorder {
	t.Helper()
	r := NewRecorder(NewShared(dest), o)
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

func logConfig(format, decisions string) config.Log {
	return config.Log{Level: config.LevelInfo, Format: format, Decisions: decisions}
}

func sampleRequest() *request.Request {
	return &request.Request{
		ID:       "6f1c0f6e",
		Host:     "api.example.com",
		Method:   "POST",
		Path:     "/v1/webhooks",
		Query:    "token=s3cr3t",
		Headers:  map[string]string{"authorization": "Bearer s3cr3t", "cookie": "s=s3cr3t"},
		ClientIP: netip.MustParseAddr("198.51.100.7"),
	}
}

// TestDecisionEventFields fixes the fields of the decision event, their order
// and their values, in both formats.
func TestDecisionEventFields(t *testing.T) {
	deny := engine.Decision{
		Action: engine.Deny, Enforced: true, Rule: "incident-block", Reason: engine.ReasonError, Cause: expr.CauseClientIPMissing,
		AuditRules:         []engine.AuditMatch{{Rule: "watch-a", Action: engine.Deny}, {Rule: "watch-b", Action: engine.Allow}},
		IndeterminateRules: []string{"office", "partners"},
		Duration:           41*time.Microsecond + 499*time.Nanosecond,
	}
	allow := engine.Decision{Action: engine.Allow, Enforced: true, Rule: "default", Reason: engine.ReasonDefault, Duration: 2*time.Millisecond + 500*time.Nanosecond}
	audited := engine.Decision{Action: engine.Deny, Enforced: false, Rule: "office", Reason: engine.ReasonRule, Duration: 1234 * time.Millisecond}
	noAddress := sampleRequest()
	noAddress.ClientIP = netip.Addr{}
	long := sampleRequest()
	long.Host = strings.Repeat("h", MaxValueBytes+10)
	long.Path = "/" + strings.Repeat("☃", MaxValueBytes/3+1)
	for _, tc := range []struct {
		format string
		want   []string
	}{
		{config.FormatLogfmt, []string{
			`level=warn msg=decision request_id=6f1c0f6e host=api.example.com method=POST path=/v1/webhooks client_ip=198.51.100.7 action=deny enforced=true rule=incident-block reason=error audit_rules=watch-a,watch-b indeterminate_rules=office,partners error=client_ip_missing duration_ms=0.041`,
			`level=info msg=decision request_id=6f1c0f6e host=api.example.com method=POST path=/v1/webhooks client_ip="" action=allow enforced=true rule=default reason=default duration_ms=2.001`,
			`level=warn msg=decision request_id=6f1c0f6e host=` + strings.Repeat("h", MaxValueBytes) + ` method=POST path=/` + strings.Repeat("☃", MaxValueBytes/3) + ` client_ip=198.51.100.7 action=deny enforced=false rule=office reason=rule duration_ms=1234`,
		}},
		{config.FormatJSON, []string{
			`{"level":"warn","msg":"decision","request_id":"6f1c0f6e","host":"api.example.com","method":"POST","path":"/v1/webhooks","client_ip":"198.51.100.7","action":"deny","enforced":true,"rule":"incident-block","reason":"error","audit_rules":"watch-a,watch-b","indeterminate_rules":"office,partners","error":"client_ip_missing","duration_ms":0.041}`,
			`{"level":"info","msg":"decision","request_id":"6f1c0f6e","host":"api.example.com","method":"POST","path":"/v1/webhooks","client_ip":"","action":"allow","enforced":true,"rule":"default","reason":"default","duration_ms":2.001}`,
			`{"level":"warn","msg":"decision","request_id":"6f1c0f6e","host":"` + strings.Repeat("h", MaxValueBytes) + `","method":"POST","path":"/` + strings.Repeat("☃", MaxValueBytes/3) + `","client_ip":"198.51.100.7","action":"deny","enforced":false,"rule":"office","reason":"rule","duration_ms":1234}`,
		}},
	} {
		t.Run(tc.format, func(t *testing.T) {
			dest := &buffer{}
			r := newRecorder(t, dest, RecorderOptions{Log: logConfig(tc.format, config.DecisionsAll)})
			r.Decision(deny, sampleRequest())
			r.Decision(allow, noAddress)
			r.Decision(audited, long)
			if err := r.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := dest.lines()
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
			if strings.Contains(dest.String(), "s3cr3t") {
				t.Fatal("an event repeats the query, a cookie or an authorization header")
			}
			if tc.format == config.FormatJSON {
				for _, line := range got {
					var fields map[string]any
					if err := json.Unmarshal([]byte(line), &fields); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

// log.decisions selects the decisions, and log.level the levels.
func TestDecisionsSelection(t *testing.T) {
	allow := engine.Decision{Action: engine.Allow, Enforced: true, Rule: "office", Reason: engine.ReasonRule}
	auditAllow := engine.Decision{Action: engine.Allow, Enforced: true, Rule: "office", Reason: engine.ReasonRule, AuditRules: []engine.AuditMatch{{Rule: "watch", Action: engine.Allow}}}
	auditDeny := engine.Decision{Action: engine.Allow, Enforced: true, Rule: "office", Reason: engine.ReasonRule, AuditRules: []engine.AuditMatch{{Rule: "watch", Action: engine.Allow}, {Rule: "block", Action: engine.Deny}}}
	deny := engine.Decision{Action: engine.Deny, Enforced: true, Rule: "block", Reason: engine.ReasonRule}
	notEnforced := engine.Decision{Action: engine.Deny, Enforced: false, Rule: "block", Reason: engine.ReasonRule}
	all := []engine.Decision{allow, auditAllow, auditDeny, deny, notEnforced}
	for _, tc := range []struct {
		decisions, level string
		want             int
	}{
		{config.DecisionsNone, config.LevelInfo, 0},
		{config.DecisionsDeny, config.LevelInfo, 3},
		{config.DecisionsAll, config.LevelInfo, 5},
		{config.DecisionsAll, config.LevelWarn, 2},
		{config.DecisionsDeny, config.LevelError, 0},
	} {
		dest := &buffer{}
		r := newRecorder(t, dest, RecorderOptions{Log: config.Log{Level: tc.level, Format: config.FormatLogfmt, Decisions: tc.decisions}})
		for _, d := range all {
			r.Decision(d, sampleRequest())
		}
		_ = r.Close(context.Background())
		if got := len(dest.lines()); got != tc.want {
			t.Errorf("log.decisions %s, log.level %s: %d events, want %d:\n%s", tc.decisions, tc.level, got, tc.want, dest)
		}
	}
}

// blocking is a destination that blocks its writes until release is closed,
// and counts them.
type blocking struct {
	buffer
	release chan struct{}
	writes  atomic.Int32
}

func (b *blocking) Write(p []byte) (int, error) {
	b.writes.Add(1)
	<-b.release
	return b.buffer.Write(p)
}

// A full queue drops the events and counts them; the writer reports the
// drops once the destination writes again, at most once per interval.
func TestDropsWhenTheQueueIsFull(t *testing.T) {
	dest := &blocking{release: make(chan struct{})}
	var counted atomic.Int32
	r := NewRecorder(NewShared(dest), RecorderOptions{Log: logConfig(config.FormatLogfmt, config.DecisionsAll), Dropped: func() { counted.Add(1) }, maxEvents: 4, interval: time.Hour})
	d := engine.Decision{Action: engine.Allow, Enforced: true, Rule: "office", Reason: engine.ReasonRule}
	// The writer takes the first event and blocks on it; 4 more fill the
	// queue; the others are dropped. Decision never waits.
	start := time.Now()
	r.Decision(d, sampleRequest())
	for dest.writes.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	for range 10 {
		r.Decision(d, sampleRequest())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Decision waited %v for a blocked destination", elapsed)
	}
	if r.Dropped() != 6 || counted.Load() != 6 {
		t.Fatalf("dropped %d, counted %d; want 6", r.Dropped(), counted.Load())
	}
	close(dest.release)
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The report follows the first write that succeeds.
	lines := dest.lines()
	if len(lines) != 6 || lines[1] != "level=warn msg=decision_logs_dropped dropped=6 dropped_total=6" || strings.Count(dest.String(), "msg=decision ") != 5 {
		t.Fatalf("events:\n%s", strings.Join(lines, "\n"))
	}
	// After Close, a decision event is dropped and counted.
	r.Decision(d, sampleRequest())
	if r.Dropped() != 7 || counted.Load() != 7 {
		t.Fatalf("after close: dropped %d, counted %d; want 7", r.Dropped(), counted.Load())
	}
}

// A queue over its bytes drops too.
func TestDropsOverTheByteBound(t *testing.T) {
	dest := &blocking{release: make(chan struct{})}
	r := NewRecorder(NewShared(dest), RecorderOptions{Log: logConfig(config.FormatLogfmt, config.DecisionsAll), maxBytes: 600, interval: time.Hour})
	d := engine.Decision{Action: engine.Allow, Enforced: true, Rule: "office", Reason: engine.ReasonRule}
	for range 10 {
		r.Decision(d, sampleRequest())
	}
	if r.Dropped() < 5 {
		t.Fatalf("dropped %d of 10 events of about 200 bytes with a bound of 600 bytes", r.Dropped())
	}
	close(dest.release)
	_ = r.Close(context.Background())
}

// The report of drops is rate-limited: drops within an interval of the last
// report wait for the next one.
func TestDropReportsAreRateLimited(t *testing.T) {
	dest := &blocking{release: make(chan struct{})}
	interval := 300 * time.Millisecond
	r := NewRecorder(NewShared(dest), RecorderOptions{Log: logConfig(config.FormatLogfmt, config.DecisionsAll), maxEvents: 1, interval: interval})
	d := engine.Decision{Action: engine.Allow, Enforced: true, Rule: "office", Reason: engine.ReasonRule}
	burst := func() {
		r.Decision(d, sampleRequest())
		for dest.writes.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		for range 3 {
			r.Decision(d, sampleRequest())
		}
	}
	burst()
	close(dest.release)
	waitFor(t, dest, "dropped=2 dropped_total=2")
	for range 5 {
		r.Decision(d, sampleRequest())
	}
	time.Sleep(interval / 3)
	if strings.Count(dest.String(), "decision_logs_dropped") != 1 {
		t.Fatalf("a second report within the interval:\n%s", dest)
	}
	_ = r.Close(context.Background())
}

func waitFor(t *testing.T, dest interface{ String() string }, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(dest.String(), text) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in:\n%s", text, dest.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Close writes the queued events, within its context.
func TestCloseFlushes(t *testing.T) {
	dest := &buffer{}
	r := NewRecorder(NewShared(dest), RecorderOptions{Log: logConfig(config.FormatJSON, config.DecisionsAll)})
	d := engine.Decision{Action: engine.Deny, Enforced: true, Rule: "block", Reason: engine.ReasonRule}
	for range 100 {
		r.Decision(d, sampleRequest())
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(dest.lines()); got != 100 || r.Dropped() != 0 {
		t.Fatalf("%d events written, %d dropped", got, r.Dropped())
	}
	// A destination that never returns holds Close for its context only.
	stuck := &blocking{release: make(chan struct{})}
	defer close(stuck.release)
	r = NewRecorder(NewShared(stuck), RecorderOptions{Log: logConfig(config.FormatJSON, config.DecisionsAll)})
	r.Decision(d, sampleRequest())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close on a stuck destination: %v", err)
	}
}

// evaluation_error is written once per interval for each rule and cause,
// with the count of the conditions since the previous one.
func TestEvaluationErrorReports(t *testing.T) {
	dest := &buffer{}
	interval := 300 * time.Millisecond
	r := NewRecorder(NewShared(dest), RecorderOptions{Log: logConfig(config.FormatLogfmt, config.DecisionsNone), Rules: []string{"office", "admin"}, interval: interval})
	r.EvaluationError("office", expr.CauseClientIPMissing)
	waitFor(t, dest, "rule=office cause=client_ip_missing count=1")
	for range 4 {
		r.EvaluationError("office", expr.CauseClientIPMissing)
	}
	r.EvaluationError("admin", expr.CauseExpression)
	waitFor(t, dest, "rule=admin cause=expression count=1")
	if strings.Count(dest.String(), "rule=office") != 1 {
		t.Fatalf("a second report within the interval:\n%s", dest)
	}
	waitFor(t, dest, "rule=office cause=client_ip_missing count=4")
	r.EvaluationError("admin", expr.CauseSetUnavailable)
	waitFor(t, dest, "rule=admin cause=set_unavailable count=1")
	r.EvaluationError("admin", expr.CauseDatabaseError)
	waitFor(t, dest, "rule=admin cause=database_error count=1")
	// A rule that the configuration does not have is reported without a name.
	r.EvaluationError("unknown", expr.CausePanic)
	_ = r.Close(context.Background())
	want := []string{
		"level=warn msg=evaluation_error rule=office cause=client_ip_missing count=1",
		"level=warn msg=evaluation_error rule=admin cause=expression count=1",
		"level=warn msg=evaluation_error rule=office cause=client_ip_missing count=4",
		"level=warn msg=evaluation_error rule=admin cause=set_unavailable count=1",
		"level=warn msg=evaluation_error rule=admin cause=database_error count=1",
		`level=warn msg=evaluation_error rule="" cause=panic count=1`,
	}
	if got := dest.lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// panic_recovered carries the rule, the count, the error and the stack of the
// first panic since the previous report of its rule.
func TestPanicReports(t *testing.T) {
	dest := &buffer{}
	r := NewRecorder(NewShared(dest), RecorderOptions{Log: logConfig(config.FormatJSON, config.DecisionsNone), Rules: []string{"office"}, interval: time.Hour})
	var runtimeErr error
	func() {
		defer func() { runtimeErr, _ = recover().(error) }()
		var m map[string]int
		m["x"] = 1
	}()
	r.Panic(&expr.Panic{Rule: "office", Value: runtimeErr, Stack: []byte("goroutine 7 [running]:\nmain.f()\n")})
	waitFor(t, dest, "panic_recovered")
	r.Panic(&expr.Panic{Rule: "office", Value: "bad s3cr3t", Stack: []byte(strings.Repeat("s", MaxStackBytes+10))})
	r.Panic(&expr.Panic{Rule: "office", Value: "again"})
	_ = r.Close(context.Background())
	lines := dest.lines()
	if len(lines) != 2 {
		t.Fatalf("events:\n%s", dest)
	}
	var first, second struct {
		Level, Msg, Rule, Error, Stack string
		Count                          int
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first.Level != "error" || first.Rule != "office" || first.Count != 1 || first.Error != "assignment to entry in nil map" || first.Stack != "goroutine 7 [running]:\nmain.f()\n" {
		t.Errorf("first report %+v", first)
	}
	if second.Count != 2 || second.Error != "panic with a value of type string" || len(second.Stack) != MaxStackBytes {
		t.Errorf("second report: count %d, error %q, %d bytes of stack", second.Count, second.Error, len(second.Stack))
	}
	if strings.Contains(dest.String(), "s3cr3t") {
		t.Error("a report repeats the value of a panic")
	}
}

// Many goroutines record at once (run with -race).
func TestConcurrentRecording(t *testing.T) {
	dest := &buffer{}
	r := NewRecorder(NewShared(dest), RecorderOptions{Log: logConfig(config.FormatLogfmt, config.DecisionsAll), Rules: []string{"office"}})
	d := engine.Decision{Action: engine.Allow, Enforced: true, Rule: "office", Reason: engine.ReasonRule}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				r.Decision(d, sampleRequest())
				r.EvaluationError("office", expr.CauseExpression)
			}
		})
	}
	wg.Wait()
	_ = r.Close(context.Background())
	if got := strings.Count(dest.String(), "msg=decision "); uint64(got)+r.Dropped() != 1600 {
		t.Fatalf("%d events written and %d dropped, want 1600 in all", got, r.Dropped())
	}
}

// When Close cannot finish in time, it discards the queue: the events left
// are dropped and counted, and nothing of the recorder follows a write that
// another writer of the destination makes after Close returns.
func TestCloseDiscardsWhatItCannotWrite(t *testing.T) {
	dest := &blocking{release: make(chan struct{})}
	shared := NewShared(dest)
	var counted atomic.Int32
	r := NewRecorder(shared, RecorderOptions{Log: logConfig(config.FormatLogfmt, config.DecisionsAll), Rules: []string{"office"}, Dropped: func() { counted.Add(1) }, interval: time.Hour})
	d := engine.Decision{Action: engine.Allow, Enforced: true, Rule: "office", Reason: engine.ReasonRule}
	r.Decision(d, sampleRequest())
	for dest.writes.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	for range 5 {
		r.Decision(d, sampleRequest())
	}
	r.EvaluationError("office", expr.CauseExpression)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := r.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v on a blocked destination", elapsed)
	}
	if r.Dropped() != 5 || counted.Load() != 5 {
		t.Fatalf("dropped %d, counted %d; want the 5 queued events", r.Dropped(), counted.Load())
	}
	// The last event of the process, written after Close, waits for the
	// write in progress, and nothing of the recorder follows it.
	last := make(chan struct{})
	go func() {
		_, _ = shared.Write([]byte("level=info msg=shutdown_complete outcome=forced\n"))
		close(last)
	}()
	close(dest.release)
	<-last
	time.Sleep(100 * time.Millisecond)
	lines := dest.lines()
	if len(lines) != 2 || !strings.Contains(lines[0], "msg=decision ") || lines[1] != "level=info msg=shutdown_complete outcome=forced" {
		t.Fatalf("events:\n%s", strings.Join(lines, "\n"))
	}
}

// Errors of one rule among many are reported with their counts, however many
// they are, and the writer handles only the rules that have errors.
func TestEvaluationErrorStorm(t *testing.T) {
	dest := &buffer{}
	rules := make([]string, 500)
	for i := range rules {
		rules[i] = fmt.Sprintf("rule-%d", i)
	}
	r := NewRecorder(NewShared(dest), RecorderOptions{Log: logConfig(config.FormatLogfmt, config.DecisionsNone), Rules: rules, interval: time.Hour})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 5000 {
				r.EvaluationError("rule-250", expr.CauseClientIPMissing)
			}
		})
	}
	wg.Wait()
	_ = r.Close(context.Background())
	total := 0
	for _, line := range dest.lines() {
		var rule, cause string
		var count int
		if _, err := fmt.Sscanf(line, "level=warn msg=evaluation_error rule=%s cause=%s count=%d", &rule, &cause, &count); err != nil || rule != "rule-250" || cause != "client_ip_missing" {
			t.Fatalf("event %q", line)
		}
		total += count
	}
	if n := len(dest.lines()); total != 40000 || n > 2 {
		t.Fatalf("%d events count %d conditions; want at most 2 events counting 40000", n, total)
	}
}

// A flush that has ended is not abandoned by a context that has expired too:
// Close reports success, and drops nothing.
func TestCloseAfterAFlushThatEnded(t *testing.T) {
	dest := &buffer{}
	r := NewRecorder(NewShared(dest), RecorderOptions{Log: logConfig(config.FormatLogfmt, config.DecisionsAll)})
	d := engine.Decision{Action: engine.Allow, Enforced: true, Rule: "office", Reason: engine.ReasonRule}
	for range 10 {
		r.Decision(d, sampleRequest())
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	// Both the end of the flush and the context are ready: select would
	// pick either of them half of the time.
	for range 100 {
		if err := r.Close(expired); err != nil {
			t.Fatalf("Close after the flush ended: %v", err)
		}
	}
	if r.Dropped() != 0 || len(dest.lines()) != 10 {
		t.Fatalf("dropped %d, written %d of 10", r.Dropped(), len(dest.lines()))
	}
}

package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gtriggiano/tessera/internal/config"
	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/expr"
	"github.com/gtriggiano/tessera/internal/request"
)

// The bounds of the events that a Recorder writes.
const (
	// QueueEvents and QueueBytes bound the decision events waiting to be
	// written.
	QueueEvents = 4096
	QueueBytes  = 4 << 20
	// ReportInterval is the shortest time between two reports of the same
	// kind: decision_logs_dropped, and evaluation_error or panic_recovered
	// for the same rule and cause.
	ReportInterval = 10 * time.Second
	// MaxValueBytes bounds a value of a decision event that comes from the
	// request.
	MaxValueBytes = 1024
	// MaxStackBytes bounds the stack of a panic_recovered event.
	MaxStackBytes = 16 << 10
)

// maxBatchBytes bounds the events written to the destination at once.
const maxBatchBytes = 64 << 10

// RecorderOptions configures a Recorder.
type RecorderOptions struct {
	// Log is the log block of the configuration.
	Log config.Log
	// Rules are the names of the rules, which bound the reports.
	Rules []string
	// Dropped, when set, is called for each decision event dropped.
	Dropped func()

	// The bounds, which tests lower: zero values select the constants.
	maxEvents int
	maxBytes  int64
	interval  time.Duration
}

// Recorder writes the events about decisions. The decision events go through
// a bounded queue, which one goroutine writes to the destination: Decision
// never waits for the destination, and when the queue is full the event is
// dropped and counted. The same goroutine writes the reports of dropped
// events, of indeterminate conditions and of recovered panics, at most one
// per ReportInterval for each kind, rule and cause, so that a storm of them
// neither floods the destination nor slows the requests.
type Recorder struct {
	decisions string
	logger    *slog.Logger // into the queue
	reports   *slog.Logger // into the destination, from the writer goroutine
	dest      *Shared
	dropped   func()
	interval  time.Duration

	// abandoned stops every write of the recorder: the queue is discarded,
	// when Close could not finish writing it in time.
	abandoned atomic.Bool

	queue    chan *[]byte
	maxBytes int64
	queued   atomic.Int64 // bytes accepted and not yet written
	drops    atomic.Uint64

	mu     sync.RWMutex // guards closed against the enqueues in flight
	closed bool
	once   sync.Once
	stop   chan struct{}
	done   chan struct{}
	wake   chan struct{}

	// errors and panics are written once, by NewRecorder: the request path
	// reads them without a lock. A slot that has occurrences to report is
	// dirty, and waits in pending, once, until the writer takes it.
	errors  map[errorKey]*slot
	panics  map[string]*slot
	pending chan *slot

	// The state of the writer goroutine: the dirty slots whose report is
	// not due yet, in the order they became dirty.
	batch       []byte
	waiting     []*slot
	dropsSent   uint64
	dropsSentAt time.Time
}

type errorKey struct {
	rule  string
	cause expr.Cause
}

// slot counts the occurrences of evaluation_error for one rule and cause, or
// of panic_recovered for one rule.
type slot struct {
	rule  string
	cause expr.Cause // empty for panic_recovered
	total atomic.Uint64
	dirty atomic.Bool
	first atomic.Pointer[expr.Panic] // the first panic since the last report
	// sent and at belong to the writer goroutine: the total of the last
	// report, and its time.
	sent uint64
	at   time.Time
}

// NewRecorder starts the writer goroutine for dest, which other writers share.
// Close stops it.
func NewRecorder(dest *Shared, o RecorderOptions) *Recorder {
	if o.maxEvents <= 0 {
		o.maxEvents = QueueEvents
	}
	if o.maxBytes <= 0 {
		o.maxBytes = QueueBytes
	}
	if o.interval <= 0 {
		o.interval = ReportInterval
	}
	r := &Recorder{
		decisions: o.Log.Decisions,

		dest:     dest,
		dropped:  o.Dropped,
		interval: o.interval,
		queue:    make(chan *[]byte, o.maxEvents),
		maxBytes: o.maxBytes,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		wake:     make(chan struct{}, 1),
		errors:   map[errorKey]*slot{},
		panics:   map[string]*slot{},
	}
	r.logger = NewLogger(queueWriter{r}, o.Log)
	r.reports = NewLogger(reportWriter{r}, o.Log)
	// Every rule and cause has its slot from the start, and the maps are
	// never written again. The empty name stands for a failure outside the
	// condition of a rule, or a rule that the configuration does not have.
	for _, rule := range append([]string{""}, o.Rules...) {
		if _, ok := r.panics[rule]; ok {
			continue
		}
		for _, cause := range []expr.Cause{expr.CauseClientIPMissing, expr.CauseExpression, expr.CausePanic, expr.CauseSetUnavailable} {
			r.errors[errorKey{rule, cause}] = &slot{rule: rule, cause: cause}
		}
		r.panics[rule] = &slot{rule: rule}
	}
	// Each slot is in pending at most once: a send never blocks.
	r.pending = make(chan *slot, len(r.errors)+len(r.panics))
	go r.run()
	return r
}

// Decision writes the decision event of d, the decision for req, when
// log.decisions and log.level select it.
func (r *Recorder) Decision(d engine.Decision, req *request.Request) {
	if !r.logs(d) {
		return
	}
	level := slog.LevelInfo
	if d.Action == engine.Deny {
		level = slog.LevelWarn
	}
	ctx := context.Background()
	if !r.logger.Enabled(ctx, level) {
		return
	}
	clientIP := ""
	if req.ClientIP.IsValid() {
		clientIP = req.ClientIP.String()
	}
	attrs := make([]slog.Attr, 0, 13)
	attrs = append(attrs,
		slog.String("request_id", bounded(req.ID)),
		slog.String("host", bounded(req.Host)),
		slog.String("method", bounded(req.Method)),
		slog.String("path", bounded(req.Path)),
		slog.String("client_ip", clientIP),
		slog.String("action", string(d.Action)),
		slog.Bool("enforced", d.Enforced),
		slog.String("rule", d.Rule),
		slog.String("reason", string(d.Reason)),
	)
	if len(d.AuditRules) > 0 {
		names := make([]string, len(d.AuditRules))
		for i, m := range d.AuditRules {
			names[i] = m.Rule
		}
		attrs = append(attrs, slog.String("audit_rules", strings.Join(names, ",")))
	}
	if len(d.IndeterminateRules) > 0 {
		attrs = append(attrs, slog.String("indeterminate_rules", strings.Join(d.IndeterminateRules, ",")))
	}
	if d.Reason == engine.ReasonError {
		attrs = append(attrs, slog.String("error", string(d.Cause)))
	}
	attrs = append(attrs, slog.Float64("duration_ms", Milliseconds(d.Duration)))
	r.logger.LogAttrs(ctx, level, "decision", attrs...)
}

// logs reports whether log.decisions selects d.
func (r *Recorder) logs(d engine.Decision) bool {
	switch r.decisions {
	case config.DecisionsAll:
		return true
	case config.DecisionsDeny:
		if d.Action == engine.Deny {
			return true
		}
		for _, m := range d.AuditRules {
			if m.Action == engine.Deny {
				return true
			}
		}
	}
	return false
}

// Milliseconds returns d in milliseconds, rounded to the microsecond: the
// value of the duration_ms fields of the events.
func Milliseconds(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Microsecond)) / 1000
}

// bounded cuts s to MaxValueBytes, at a UTF-8 boundary.
func bounded(s string) string {
	return cut(s, MaxValueBytes)
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// EvaluationError counts a condition of rule that could not be evaluated,
// for an evaluation_error report.
func (r *Recorder) EvaluationError(rule string, cause expr.Cause) {
	s, ok := r.errors[errorKey{rule, cause}]
	if !ok {
		s = r.errors[errorKey{"", cause}]
	}
	if s != nil {
		r.count(s)
	}
}

// Panic counts a recovered panic, for a panic_recovered report that carries
// the stack of the first one since the previous report of its rule.
func (r *Recorder) Panic(p *expr.Panic) {
	s, ok := r.panics[p.Rule]
	if !ok {
		s = r.panics[""]
	}
	s.first.CompareAndSwap(nil, p)
	r.count(s)
}

// count counts an occurrence in s. Only the occurrence that makes s dirty
// hands it to the writer, and wakes it: the others cost an addition.
func (r *Recorder) count(s *slot) {
	s.total.Add(1)
	if s.dirty.CompareAndSwap(false, true) {
		r.pending <- s
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
}

// Dropped returns the number of decision events dropped so far.
func (r *Recorder) Dropped() uint64 { return r.drops.Load() }

// Close stops accepting decision events, which are dropped and counted from
// then on, and waits until the queue is written, with the reports that are
// due, or until ctx is done. Then it discards what is left: the events still
// queued are dropped and counted, and the recorder writes nothing more, so
// that an event written to the destination after Close returns comes after
// every event of the recorder. It never waits for a destination that blocks:
// a write in progress then ends when the destination returns.
func (r *Recorder) Close(ctx context.Context) error {
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		close(r.stop)
	})
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
	}
	// When both are ready, select picks either: a flush that ended by the
	// deadline has written everything.
	select {
	case <-r.done:
		return nil
	default:
	}
	r.abandoned.Store(true)
	for {
		select {
		case line := <-r.queue:
			r.queued.Add(-int64(len(*line)))
			r.drop(1)
			putLine(line)
		default:
			return ctx.Err()
		}
	}
}

// queueWriter is the destination of the logger of the decision events.
type queueWriter struct{ r *Recorder }

var linePool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

// Write enqueues a copy of p, one event, or drops it. It never waits for the
// destination, and never fails.
func (w queueWriter) Write(p []byte) (int, error) {
	r := w.r
	n := int64(len(p))
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		r.drop(1)
		return len(p), nil
	}
	// Reserve the bytes only if they fit, so that a concurrent write over
	// the budget cannot make one that fits fail.
	for {
		queued := r.queued.Load()
		if queued+n > r.maxBytes {
			r.drop(1)
			return len(p), nil
		}
		if r.queued.CompareAndSwap(queued, queued+n) {
			break
		}
	}
	line := linePool.Get().(*[]byte)
	*line = append((*line)[:0], p...)
	select {
	case r.queue <- line:
	default:
		r.queued.Add(-n)
		r.drop(1)
		putLine(line)
	}
	return len(p), nil
}

func (r *Recorder) drop(n uint64) {
	r.drops.Add(n)
	if r.dropped != nil {
		for range n {
			r.dropped()
		}
	}
}

func putLine(line *[]byte) {
	if cap(*line) <= maxBatchBytes {
		linePool.Put(line)
	}
}

func (r *Recorder) run() {
	defer close(r.done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		healthy := true
		select {
		case line := <-r.queue:
			healthy = r.flush(line)
		case <-ticker.C:
		case <-r.wake:
		case <-r.stop:
			for {
				select {
				case line := <-r.queue:
					r.flush(line)
				default:
					r.report(true)
					return
				}
			}
		}
		// A destination that fails gets no report until a write succeeds.
		if healthy {
			r.report(false)
		}
	}
}

// flush writes first and the events queued behind it, up to a batch bound,
// with one write to the destination, and reports whether it succeeded. The
// events of a failed write are counted as dropped.
func (r *Recorder) flush(first *[]byte) bool {
	r.batch = append(r.batch[:0], *first...)
	events := 1
	putLine(first)
	for len(r.batch) < maxBatchBytes {
		select {
		case line := <-r.queue:
			r.batch = append(r.batch, *line...)
			events++
			putLine(line)
			continue
		default:
		}
		break
	}
	written, err := r.dest.writeUnless(r.abandoned.Load, r.batch)
	r.queued.Add(-int64(len(r.batch)))
	if cap(r.batch) > 4*maxBatchBytes {
		r.batch = nil
	}
	if !written || err != nil {
		r.drop(uint64(events))
		return false
	}
	return true
}

// reportWriter is the destination of the reports: the shared destination,
// until the recorder is abandoned.
type reportWriter struct{ r *Recorder }

func (w reportWriter) Write(p []byte) (int, error) {
	if _, err := w.r.dest.writeUnless(w.r.abandoned.Load, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// report writes the reports that are due: the drops, and the dirty slots
// whose rule and cause had no report for an interval; all of them when
// final. It visits only the dirty slots.
func (r *Recorder) report(final bool) {
	now := time.Now()
	ctx := context.Background()
	if total := r.drops.Load(); total != r.dropsSent && (final || r.dropsSentAt.IsZero() || now.Sub(r.dropsSentAt) >= r.interval) {
		r.reports.LogAttrs(ctx, slog.LevelWarn, "decision_logs_dropped", slog.Uint64("dropped", total-r.dropsSent), slog.Uint64("dropped_total", total))
		r.dropsSent, r.dropsSentAt = total, now
	}
	for {
		select {
		case s := <-r.pending:
			r.waiting = append(r.waiting, s)
			continue
		default:
		}
		break
	}
	kept := r.waiting[:0]
	for _, s := range r.waiting {
		if !final && !s.at.IsZero() && now.Sub(s.at) < r.interval {
			kept = append(kept, s)
			continue
		}
		// Clean before reading the total: an occurrence from now on makes
		// the slot dirty again, and is reported next time.
		s.dirty.Store(false)
		total := s.total.Load()
		if total == s.sent {
			continue
		}
		count := total - s.sent
		s.sent, s.at = total, now
		if s.cause != "" {
			r.reports.LogAttrs(ctx, slog.LevelWarn, "evaluation_error", slog.String("rule", s.rule), slog.String("cause", string(s.cause)), slog.Uint64("count", count))
			continue
		}
		attrs := []slog.Attr{slog.String("rule", s.rule), slog.Uint64("count", count)}
		if first := s.first.Swap(nil); first != nil {
			attrs = append(attrs, slog.String("error", panicError(first.Value)), slog.String("stack", cut(string(first.Stack), MaxStackBytes)))
		}
		r.reports.LogAttrs(ctx, slog.LevelError, "panic_recovered", attrs...)
	}
	clear(r.waiting[len(kept):])
	r.waiting = kept
}

// panicError describes the value of a panic: the message of an error of the
// Go runtime, which holds no request data, or else the type of the value,
// which could hold anything.
func panicError(v any) string {
	if e, ok := v.(runtime.Error); ok {
		return e.Error()
	}
	return fmt.Sprintf("panic with a value of type %T", v)
}

// Shared is the destination that the log events of tessera share. Its
// writes are whole and in order: one at a time.
type Shared struct {
	mu sync.Mutex
	w  io.Writer
}

// NewShared returns the shared destination w.
func NewShared(w io.Writer) *Shared { return &Shared{w: w} }

func (s *Shared) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// writeUnless writes p unless stop reports true, which it asks in turn with
// the other writes: once stop reports true, no write of p comes after
// another write.
func (s *Shared) writeUnless(stop func() bool, p []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stop() {
		return false, nil
	}
	_, err := s.w.Write(p)
	return true, err
}

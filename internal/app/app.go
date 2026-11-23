// Package app runs tessera serve: it builds every component from a valid
// configuration, opens the gRPC and admin listeners, and shuts down on a
// signal.
//
// # Startup
//
// The sets and the databases are loaded before Run, by LoadSets and
// LoadDatabases: an invalid one is a configuration error. Run builds the
// engine on them and the metrics, logs starting, a config_warning for each
// warning of the configuration, a set_loaded for each set and a
// database_loaded for each database, then builds the gRPC server and the
// admin server before it opens any socket. It opens the gRPC listener, then
// the admin listener, resolving their host names within listenTimeout; if
// one cannot open, the other is closed, startup_failed is logged and Run
// fails. A signal received while a listener opens cancels it. Once both are
// open, Run logs listening for each, makes both report ready (/readyz 200,
// gRPC health SERVING), logs ready, starts the refreshers of the sets read
// from files and of the databases, and starts answering. A signal received
// before then starts the shutdown at once.
//
// # Sets
//
// Each set read from a file has a refresher, which checks the file every
// sets.<name>.refresh and replaces the copy of the set when the file has a
// new valid content, and marks the set unavailable while its last successful
// check, or the start of the checks before the first, is older than
// sets.<name>.max_staleness. The refreshers record what they do in the
// tessera_set_* metrics and in the set_loaded, set_load_failed,
// set_unavailable and set_available events, from their own goroutines: a
// decision only reads the store of the sets.
//
// # Databases
//
// Each database has a refresher too, the same as for the file of a set,
// which checks the file every enrichment.<kind>.refresh and replaces the
// copy of the database when the file has a new valid database. A database
// has no max_staleness: the copy loaded at startup, or the last one, stays in
// use. The refreshers record what they do in the tessera_enrichment_database_*
// metrics and in the database_loaded and database_load_failed events: a
// decision only reads the store of the databases.
//
// # Shutdown
//
// The first signal, or a listener that fails, starts the shutdown:
//
//  1. shutdown_started is logged; /readyz answers 503 and the gRPC health
//     service NOT_SERVING; not_ready is logged.
//  2. Run waits server.drain_delay, while both listeners keep answering.
//  3. The gRPC server stops gracefully: it closes its listener, asks every
//     client to close its connection and waits for the calls in progress,
//     until server.shutdown_timeout has passed since the end of step 2.
//  4. The refreshers of the sets and of the databases stop, within the same
//     deadline: a check in progress is cut short, and records nothing.
//  5. The admin server stops, within the same deadline.
//  6. The decision events still queued are written, within the same
//     deadline.
//  7. A listener that failed during the shutdown is logged as
//     listener_failed, then shutdown_complete is logged.
//
// A second signal, at any step, or the deadline of steps 3 to 6, closes
// every remaining connection at once, cancelling the calls in progress,
// stops the refreshers without waiting for a check in progress, and drops
// the decision events still queued: shutdown_complete then reports
// outcome=forced, and stays the last event.
//
// # Decisions
//
// Each decision is recorded after it is made, and the recording cannot change
// it: the metrics, the upstream header errors included, then the decision
// event, with the decision fields that the engine computes for it, which
// waits in a bounded queue that a goroutine of the recorder writes, so that a
// slow output never slows a decision. The same goroutine writes the
// rate-limited evaluation_error and panic_recovered events.
//
// # Result
//
// Run returns nil after a shutdown that a signal started and in which every
// call finished. It returns an error when a listener could not open or
// failed, and after a forced shutdown. It logs every such error before
// returning it. It also fails, before it builds anything else, when the
// policy does not compile, which a valid configuration rules out, and when
// Options.Sets or Options.Databases is nil and the sets or the databases
// cannot be loaded.
package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gtriggiano/tessera/internal/admin"
	"github.com/gtriggiano/tessera/internal/config"
	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/expr"
	"github.com/gtriggiano/tessera/internal/extauthz"
	"github.com/gtriggiano/tessera/internal/metrics"
	"github.com/gtriggiano/tessera/internal/request"
	"github.com/gtriggiano/tessera/internal/sets"
	"github.com/gtriggiano/tessera/internal/source"
	"github.com/gtriggiano/tessera/internal/telemetry"
)

// The names of the listeners in the log events.
const (
	listenerGRPC  = "grpc"
	listenerAdmin = "admin"
)

// listenTimeout bounds the opening of both listeners, which can resolve a
// host name.
const listenTimeout = 10 * time.Second

// Options supplies the process dependencies of Run.
type Options struct {
	// Logs receives the log events. Nil discards them.
	Logs io.Writer
	// Build identifies the binary in the starting event and in
	// tessera_build_info.
	Build metrics.BuildInfo
	// ConfigPath is the path of the configuration file, for the starting
	// event.
	ConfigPath string
	// ConfigHash is the hexadecimal SHA-256 of the output of tessera config
	// print, for tessera_config_info.
	ConfigHash string
	// Signals delivers the shutdown signals: the first starts a graceful
	// shutdown, and one received during the shutdown forces it. Nil delivers
	// none.
	Signals <-chan os.Signal
	// Sets are the sets of the configuration, as LoadSets loaded them. Nil
	// makes Run load them.
	Sets *Sets
	// Databases are the databases of the configuration, as LoadDatabases
	// loaded them. Nil makes Run load them.
	Databases *Databases
	// Listen opens a listener: the gRPC listener, then the admin listener.
	// ctx ends when a signal arrives or when the opening takes too long. Nil
	// uses net.ListenConfig.
	Listen func(ctx context.Context, network, address string) (net.Listener, error)

	// The hooks of the tests. decider replaces the engine; afterDrain runs at
	// the end of server.drain_delay, before the gRPC server stops; flushing
	// runs before the queued decision events are written; served runs when
	// the server of a listener returns; listenTimeout replaces the constant;
	// setTime changes the refresh and the max_staleness of every set and the
	// refresh of every database, so that a test can use less than the
	// minimum of the configuration.
	decider       extauthz.Decider
	afterDrain    func()
	flushing      func()
	served        func(listener string, err error)
	listenTimeout time.Duration
	setTime       func(time.Duration) time.Duration
}

// Run serves cfg, a loaded and valid configuration, until a shutdown. See
// the package documentation for the startup, the shutdown and the result.
func Run(cfg *config.Config, opts Options) error {
	logs := opts.Logs
	if logs == nil {
		logs = io.Discard
	}
	// The events written here and those of the recorder share the
	// destination.
	shared := telemetry.NewShared(logs)
	logs = shared
	loaded := opts.Sets
	if loaded == nil {
		var err error
		if loaded, err = LoadSets(cfg, nil); err != nil {
			return err
		}
	}
	databases := opts.Databases
	if databases == nil {
		var err error
		if databases, err = LoadDatabases(cfg); err != nil {
			return err
		}
	}
	policy := EnginePolicy(cfg)
	declared := loaded.Store.Declarations()
	m := metrics.New(opts.Build, opts.ConfigHash, policy, declared, databases.Store.Kinds())
	rules := make([]string, len(policy.Rules))
	for i, r := range policy.Rules {
		rules[i] = r.Name
	}
	// The recorder writes the decision fields that the engine computes, and
	// the engine reports its panics to the recorder: the recorder comes
	// first, and reads the fields through a function set once the engine is
	// built, before any decision.
	var fields func(engine.Decision) []engine.Field
	recorder := telemetry.NewRecorder(shared, telemetry.RecorderOptions{Log: cfg.Log, Rules: rules, Dropped: m.DecisionLogDropped, Fields: func(d engine.Decision) []engine.Field {
		if fields == nil {
			return nil
		}
		return fields(d)
	}})
	var decider extauthz.Decider = opts.decider
	if decider == nil {
		e, err := engine.New(policy, engine.Options{OnPanic: recorder.Panic, Sets: loaded.Store, Databases: databases.Store})
		if err != nil {
			_ = recorder.Close(context.Background())
			return err
		}
		decider, fields = e, e.DecisionFields
	}
	decider = observed{decider: decider, metrics: m, recorder: recorder}
	logger := telemetry.NewLogger(logs, cfg.Log)
	logger.Info("starting", "version", opts.Build.Version, "revision", opts.Build.Revision, "config_path", opts.ConfigPath)
	for _, w := range cfg.Warnings() {
		logger.Warn("config_warning", "key", w.Path, "warning", w.Problem)
	}
	// The load at startup of each set that refreshes is its first refresh;
	// SetRefreshed records nothing for the others.
	reports := newSetReports(m, logger, declared)
	snapshot := loaded.Store.Snapshot()
	for i, d := range declared {
		e, load := snapshot.Entry(i), loaded.Loads[d.Name]
		m.SetRefreshed(d.Name, source.Updated, load.Duration, load.At)
		m.SetAvailable(d.Name, e.Available)
		if e.Set != nil {
			reports.Loaded(d.Name, e.Set, load.Duration)
		}
	}
	// The load at startup of each database is its first refresh.
	dbReports := databaseReports{metrics: m, logger: logger}
	dbSnapshot := databases.Store.Snapshot()
	for _, k := range databases.Store.Kinds() {
		load := databases.Loads[k]
		m.DatabaseRefreshed(k, source.Updated, load.Duration, load.At)
		if db := dbSnapshot.Database(k); db != nil {
			dbReports.Loaded(string(k), db, load.Duration)
		}
	}

	grpcServer := extauthz.NewServer(decider, extauthz.Options{
		MaxConnections:       cfg.Server.MaxConnections,
		Refused:              m.ConnectionRefused,
		MaxConcurrentStreams: uint32(cfg.Server.MaxConcurrentStreams),
		MaxConnectionAge:     cfg.Server.MaxConnectionAge,
		ClientIP: request.ClientIPSource{
			From:        request.Source(cfg.ClientIP.From),
			Header:      cfg.ClientIP.Header,
			TrustedHops: cfg.ClientIP.TrustedHops,
		},
		OnPanic: recorder.Panic,
	})
	adminServer := admin.NewServer(m.Handler())

	grpcListener, adminListener, signalled, err := open(cfg, opts, logger)
	if err != nil {
		_ = recorder.Close(context.Background())
		return err
	}
	logger.Info("listening", "listener", listenerGRPC, "address", addressOf(grpcListener))
	logger.Info("listening", "listener", listenerAdmin, "address", addressOf(adminListener))

	adminServer.SetReady()
	grpcServer.SetServing(true)
	logger.Info("ready")
	refreshers := refresherGroups{
		source.Start(fileSets(cfg, loaded, opts.setTime), loaded.Store, reports),
		source.Start(databaseFiles(cfg, databases, opts.setTime), databaseStore{databases.Store}, dbReports),
	}

	var serving sync.WaitGroup
	failed := make(chan string, 2)
	serve := func(name string, server interface{ Serve(net.Listener) error }, l net.Listener) {
		serving.Go(func() {
			err := server.Serve(l)
			if opts.served != nil {
				opts.served(name, err)
			}
			if err != nil {
				failed <- name
			}
		})
	}
	serve(listenerGRPC, grpcServer, grpcListener)
	serve(listenerAdmin, adminServer, adminListener)

	var result error
	listenerFailed := func(name string) {
		logger.Error("listener_failed", "listener", name)
		if result == nil {
			result = errors.New("the " + name + " listener failed")
		}
	}
	if signalled {
		logger.Info("shutdown_started", "reason", "signal")
	} else {
		select {
		case <-opts.Signals:
			logger.Info("shutdown_started", "reason", "signal")
		case name := <-failed:
			listenerFailed(name)
			logger.Info("shutdown_started", "reason", "listener_failed")
		}
	}
	forcedBy := shutdown(cfg.Server, opts, logger, grpcServer, refreshers, adminServer, recorder)
	serving.Wait()
	for len(failed) > 0 {
		listenerFailed(<-failed)
	}
	if forcedBy != "" {
		logger.Warn("shutdown_complete", "outcome", "forced", "forced_by", forcedBy)
		if result == nil {
			result = errors.New("shutdown forced by " + forcedBy)
		}
	} else {
		logger.Info("shutdown_complete", "outcome", "drained")
	}
	return result
}

// open opens the gRPC listener, then the admin listener, and reports whether
// a signal arrived meanwhile. When one cannot open, it closes the other,
// logs startup_failed and returns an error.
func open(cfg *config.Config, opts Options, logger *slog.Logger) (grpcListener, adminListener net.Listener, signalled bool, err error) {
	listen := opts.Listen
	if listen == nil {
		var lc net.ListenConfig
		listen = lc.Listen
	}
	timeout := opts.listenTimeout
	if timeout == 0 {
		timeout = listenTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// A signal cancels ctx, and is remembered: it starts the shutdown once
	// both listeners are open.
	var bySignal atomic.Bool
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-opts.Signals:
			bySignal.Store(true)
			cancel()
		case <-stop:
		}
	}()
	defer func() {
		close(stop)
		<-stopped
		signalled = bySignal.Load()
	}()
	failure := func(err error) string {
		switch {
		case bySignal.Load():
			return "interrupted by a signal"
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return "timed out"
		}
		return openError(err)
	}

	network, address := "tcp", cfg.Server.Listen
	if socket, ok := strings.CutPrefix(address, config.UnixPrefix); ok {
		network, address = "unix", socket
	}
	grpcListener, err = listen(ctx, network, address)
	if err != nil {
		logger.Error("startup_failed", "listener", listenerGRPC, "address", cfg.Server.Listen, "error", failure(err))
		return nil, nil, false, errors.New("cannot open the gRPC listener")
	}
	adminListener, err = listen(ctx, "tcp", cfg.Admin.Listen)
	if err != nil {
		_ = grpcListener.Close()
		logger.Error("startup_failed", "listener", listenerAdmin, "address", cfg.Admin.Listen, "error", failure(err))
		return nil, nil, false, errors.New("cannot open the admin listener")
	}
	return grpcListener, adminListener, false, nil
}

// shutdown runs steps 1 to 6 of the shutdown, and returns what forced it:
// "signal", "timeout", or "" when every call and request finished, every
// refresher stopped and every queued decision event was written. A forced
// shutdown discards the decision events still queued, so that
// shutdown_complete is the last event.
func shutdown(cfg config.Server, opts Options, logger *slog.Logger, grpcServer *extauthz.Server, refreshers refresherGroups, adminServer *admin.Server, recorder *telemetry.Recorder) string {
	expired, cancelExpired := context.WithCancel(context.Background())
	cancelExpired()
	adminServer.SetNotReady(admin.ReasonShuttingDown)
	grpcServer.SetServing(false)
	logger.Info("not_ready")

	// A signal received from now on cancels force.
	force, cancelForce := context.WithCancel(context.Background())
	defer cancelForce()
	var bySignal atomic.Bool
	go func() {
		select {
		case <-opts.Signals:
			bySignal.Store(true)
			cancelForce()
		case <-force.Done():
		}
	}()
	forcedBy := func() string {
		if bySignal.Load() {
			return "signal"
		}
		return "timeout"
	}

	drain := time.NewTimer(cfg.DrainDelay)
	defer drain.Stop()
	select {
	case <-drain.C:
	case <-force.Done():
		grpcServer.Stop()
		_ = refreshers.Stop(expired)
		_ = adminServer.Close()
		_ = recorder.Close(expired)
		return forcedBy()
	}
	if opts.afterDrain != nil {
		opts.afterDrain()
	}

	ctx, cancel := context.WithTimeout(force, cfg.ShutdownTimeout)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()
	forced := false
	select {
	case <-stopped:
	case <-ctx.Done():
		select {
		case <-stopped:
		default:
			// The graceful stop goes on until the handlers of the cancelled
			// calls return; nothing waits for it.
			forced = true
			grpcServer.Stop()
		}
	}
	if forced {
		_ = refreshers.Stop(expired)
		_ = adminServer.Close()
		_ = recorder.Close(expired)
		return forcedBy()
	}
	if err := refreshers.Stop(ctx); err != nil {
		_ = adminServer.Close()
		_ = recorder.Close(expired)
		return forcedBy()
	}
	if err := adminServer.Shutdown(ctx); err != nil {
		_ = recorder.Close(expired)
		return forcedBy()
	}
	if opts.flushing != nil {
		opts.flushing()
	}
	// A flush that the deadline or another signal cuts short forces the
	// shutdown: the events left are dropped and counted.
	if err := recorder.Close(ctx); err != nil {
		return forcedBy()
	}
	return ""
}

// openError is the reason a listener could not open, without the address
// or any other configured value.
func openError(err error) string {
	var errno syscall.Errno
	var dns *net.DNSError
	switch {
	case errors.As(err, &errno):
		return errno.Error()
	case errors.As(err, &dns):
		return "cannot resolve the host"
	}
	return "cannot open"
}

// addressOf is the address that l is bound to, in the form of the
// configuration: host:port, or unix: followed by the path of the socket.
func addressOf(l net.Listener) string {
	if a, ok := l.Addr().(*net.UnixAddr); ok {
		return config.UnixPrefix + a.Name
	}
	return l.Addr().String()
}

// observed decides with decider, then records the decision in the metrics and
// in the log events. The recording cannot change the decision, and never
// waits for the output of the events.
type observed struct {
	decider  extauthz.Decider
	metrics  *metrics.Metrics
	recorder *telemetry.Recorder
}

func (o observed) Decide(r *request.Request) engine.Decision {
	d := o.decider.Decide(r)
	o.metrics.Decided(d)
	for _, e := range d.Errors {
		o.recorder.EvaluationError(e.Rule, e.Cause)
	}
	o.recorder.Decision(d, r)
	return d
}

// NewEngine builds the engine of a valid configuration, whose expressions
// read the sets of store and the databases of dbs. onPanic, when set,
// receives the panics recovered while an expression is evaluated.
func NewEngine(cfg *config.Config, store *sets.Store, dbs *enrichment.Store, onPanic func(*expr.Panic)) (*engine.Engine, error) {
	return engine.New(EnginePolicy(cfg), engine.Options{OnPanic: onPanic, Sets: store, Databases: dbs})
}

// refresherGroups are the refreshers of the sets and of the databases.
type refresherGroups []*source.Refreshers

// Stop stops every group, as source.Refreshers.Stop does: it asks every
// group to stop first, then waits for all of them, until ctx ends.
func (g refresherGroups) Stop(ctx context.Context) error {
	for _, rs := range g {
		rs.Cancel()
	}
	for _, rs := range g {
		if err := rs.Stop(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Sets is the first copy of every set of a configuration.
type Sets struct {
	// Store holds the copies, every one available: the decisions read it.
	Store *sets.Store
	// Files is the identity of the file that each set with a file was read
	// from, by the name of the set.
	Files map[string]source.Identity
	// Loads is the load of each set, by its name.
	Loads map[string]Load
}

// Load is the load of the first copy of a set: when it ended, and how long
// it took.
type Load struct {
	At       time.Time
	Duration time.Duration
}

// LoadSets loads every set of cfg, a valid configuration, in the order of
// the file: it reads the file of each set that has one, and builds each set
// of values. A set in replace takes that copy instead, of the type of the
// set, and its file is not read. A file that cannot be read, or whose content
// is invalid, is a *config.Error at sets.<name>.file, with the line of the
// file and the problem, never the content or the path.
func LoadSets(cfg *config.Config, replace map[string]*sets.Set) (*Sets, error) {
	decls := cfg.SetDeclarations()
	store, err := sets.NewStore(decls)
	if err != nil {
		return nil, err
	}
	out := &Sets{Store: store, Files: map[string]source.Identity{}, Loads: map[string]Load{}}
	for _, d := range decls {
		s := cfg.Sets[d.Name]
		begin := time.Now()
		set, ok := replace[d.Name]
		switch {
		case ok:
		case s.File != "":
			var id source.Identity
			set, id, err = source.ReadFile(s.File, d.Type, s.MaxEntries)
			if err != nil {
				return nil, cfg.ErrorAt("sets."+d.Name+".file", source.Problem(err))
			}
			out.Files[d.Name] = id
		default:
			values := make([]string, len(s.Values))
			for i, v := range s.Values {
				values[i] = string(v)
			}
			if set, err = sets.FromValues(d.Type, values, s.MaxEntries); err != nil {
				// A valid configuration has valid values.
				return nil, cfg.ErrorAt("sets."+d.Name+".values", err.Error())
			}
		}
		if err := store.Replace(d.Name, set); err != nil {
			return nil, err
		}
		end := time.Now()
		out.Loads[d.Name] = Load{At: end, Duration: end.Sub(begin)}
	}
	return out, nil
}

// fileSets returns the sets of cfg read from files, as their refreshers need
// them, with the identity of the copy in loaded and the start of its read.
// setTime, when not nil, changes their refresh and max_staleness.
func fileSets(cfg *config.Config, loaded *Sets, setTime func(time.Duration) time.Duration) []source.File[*sets.Set] {
	if setTime == nil {
		setTime = func(d time.Duration) time.Duration { return d }
	}
	var files []source.File[*sets.Set]
	for _, d := range cfg.SetDeclarations() {
		s := cfg.Sets[d.Name]
		if s.File == "" {
			continue
		}
		files = append(files, source.File[*sets.Set]{
			Name:         d.Name,
			Path:         s.File,
			Parse:        source.SetParse(d.Type, s.MaxEntries),
			Refresh:      setTime(s.Refresh),
			MaxStaleness: setTime(s.MaxStaleness),
			Identity:     loaded.Files[d.Name],
			Read:         loaded.Loads[d.Name].At.Add(-loaded.Loads[d.Name].Duration),
		})
	}
	return files
}

// setReports records what the refreshers of the sets do, in the metrics and
// in log events.
type setReports struct {
	metrics  *metrics.Metrics
	logger   *slog.Logger
	declared map[string]sets.Declaration
}

// newSetReports returns the reports of the sets declared, the declarations
// that m labels its series with, so that the events and the metrics name a
// set with the same type and source.
func newSetReports(m *metrics.Metrics, logger *slog.Logger, declared []sets.Declaration) setReports {
	r := setReports{metrics: m, logger: logger, declared: make(map[string]sets.Declaration, len(declared))}
	for _, d := range declared {
		r.declared[d.Name] = d
	}
	return r
}

// fields returns the first fields of an event about the set name, followed
// by more: set, set_type and set_source.
func (r setReports) fields(name string, more ...any) []any {
	d := r.declared[name]
	return append([]any{"set", name, "set_type", string(d.Type), "set_source", string(d.Source)}, more...)
}

// The Observer of the refreshers of the sets.
var _ source.Observer[*sets.Set] = setReports{}

func (r setReports) Checked(set string, result source.Result, took time.Duration, at time.Time) {
	r.metrics.SetRefreshed(set, result, took, at)
}

func (r setReports) Loaded(set string, c *sets.Set, took time.Duration) {
	r.metrics.SetEntries(set, c.Entries())
	r.logger.Info("set_loaded", r.fields(set, "entries", c.Entries(), "duration_ms", telemetry.Milliseconds(took))...)
}

func (r setReports) Failed(set string, err error) {
	r.logger.Warn("set_load_failed", r.fields(set, "error", source.Problem(err))...)
}

func (r setReports) Unavailable(set string) {
	r.metrics.SetAvailable(set, false)
	r.logger.Warn("set_unavailable", r.fields(set, "reason", "stale")...)
}

func (r setReports) Available(set string) {
	r.metrics.SetAvailable(set, true)
	r.logger.Info("set_available", r.fields(set)...)
}

// Databases is the first copy of every database of a configuration.
type Databases struct {
	// Store holds the copies: the decisions read it.
	Store *enrichment.Store
	// Files is the identity of the file that each database was read from,
	// and Loads the load of each, by kind.
	Files map[enrichment.Kind]source.Identity
	Loads map[enrichment.Kind]Load
}

// LoadDatabases reads every database of cfg, a valid configuration: geo,
// then asn. A file that cannot be read, or whose content is invalid, is a
// *config.Error at enrichment.<kind>.database, with the problem, never the
// content or the path.
func LoadDatabases(cfg *config.Config) (*Databases, error) {
	kinds := cfg.DatabaseKinds()
	store, err := enrichment.NewStore(kinds)
	if err != nil {
		return nil, err
	}
	out := &Databases{Store: store, Files: map[enrichment.Kind]source.Identity{}, Loads: map[enrichment.Kind]Load{}}
	for _, k := range kinds {
		begin := time.Now()
		db, id, err := source.ReadDatabase(cfg.Enrichment.Database(k).Database, k)
		if err != nil {
			return nil, cfg.ErrorAt("enrichment."+string(k)+".database", source.Problem(err))
		}
		if err := store.Replace(string(k), db); err != nil {
			return nil, err
		}
		end := time.Now()
		out.Files[k] = id
		out.Loads[k] = Load{At: end, Duration: end.Sub(begin)}
	}
	return out, nil
}

// databaseFiles returns the databases of cfg as their refreshers need them,
// with the identity of the copy in loaded and the start of its read.
// setTime, when not nil, changes their refresh.
func databaseFiles(cfg *config.Config, loaded *Databases, setTime func(time.Duration) time.Duration) []source.File[*enrichment.Database] {
	if setTime == nil {
		setTime = func(d time.Duration) time.Duration { return d }
	}
	var files []source.File[*enrichment.Database]
	for _, k := range loaded.Store.Kinds() {
		db := cfg.Enrichment.Database(k)
		files = append(files, source.File[*enrichment.Database]{
			Name:     string(k),
			Path:     db.Database,
			Parse:    source.DatabaseParse(k),
			Refresh:  setTime(db.Refresh),
			Identity: loaded.Files[k],
			Read:     loaded.Loads[k].At.Add(-loaded.Loads[k].Duration),
		})
	}
	return files
}

// databaseStore is the store of the databases, as their refreshers change
// it.
type databaseStore struct{ *enrichment.Store }

// errAlwaysAvailable is the error of SetAvailable on a database, which the
// refreshers never call: a database has no max_staleness.
var errAlwaysAvailable = errors.New("a database is always available")

func (databaseStore) SetAvailable(string, bool) error { return errAlwaysAvailable }

// databaseReports records what the refreshers of the databases do, in the
// metrics and in log events.
type databaseReports struct {
	metrics *metrics.Metrics
	logger  *slog.Logger
}

// The Observer of the refreshers of the databases.
var _ source.Observer[*enrichment.Database] = databaseReports{}

func (r databaseReports) Checked(name string, result source.Result, took time.Duration, at time.Time) {
	r.metrics.DatabaseRefreshed(enrichment.Kind(name), result, took, at)
}

func (r databaseReports) Loaded(name string, db *enrichment.Database, took time.Duration) {
	r.metrics.DatabaseBuilt(enrichment.Kind(name), db.Built())
	r.logger.Info("database_loaded", "database", name, "database_type", db.Type(), "build_time", db.Built().Format(time.RFC3339), "duration_ms", telemetry.Milliseconds(took))
}

func (r databaseReports) Failed(name string, err error) {
	r.logger.Warn("database_load_failed", "database", name, "error", source.Problem(err))
}

// Unavailable and Available are never called: a database has no
// max_staleness.
func (databaseReports) Unavailable(string) {}
func (databaseReports) Available(string)   {}

// EnginePolicy translates the policy of a valid configuration for the
// engine, with its upstream headers and its decision fields.
func EnginePolicy(cfg *config.Config) engine.Policy {
	p := cfg.Policy
	ep := engine.Policy{
		Audit:           p.Mode == config.ModeAudit,
		DefaultAction:   engine.Action(p.DefaultAction),
		DenyResponse:    response(p.DenyResponse),
		SecretHeaders:   cfg.SecretHeaderNames(),
		UpstreamHeaders: namedExpressions(cfg.Upstream.Headers),
		DecisionFields:  namedExpressions(cfg.Log.DecisionFields),
	}
	for _, r := range p.Rules {
		ep.Rules = append(ep.Rules, engine.Rule{
			Name:     r.Name,
			When:     string(r.When),
			Action:   engine.Action(r.Action),
			Audit:    r.Mode == config.ModeAudit,
			Response: response(r.Response),
		})
	}
	return ep
}

// namedExpressions returns the expressions of m by name, sorted.
func namedExpressions(m map[string]config.Expression) []engine.NamedExpression {
	out := make([]engine.NamedExpression, 0, len(m))
	for _, name := range slices.Sorted(maps.Keys(m)) {
		out = append(out, engine.NamedExpression{Name: name, Expression: string(m[name])})
	}
	return out
}

func response(r config.Response) engine.Response {
	out := engine.Response{Status: r.Status, Body: r.Body}
	for name, value := range r.Headers {
		out.Headers = append(out.Headers, engine.Header{Name: name, Value: value})
	}
	return out
}

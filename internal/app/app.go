// Package app runs tessera serve: it builds every component from a valid
// configuration, opens the gRPC and admin listeners, and shuts down on a
// signal.
//
// # Startup
//
// Run logs starting, then builds the engine, the metrics, the gRPC server
// and the admin server before it opens any socket. It opens the gRPC
// listener, then the admin listener, resolving their host names within
// listenTimeout; if one cannot open, the other is closed, startup_failed is
// logged and Run fails. A signal received while a listener opens cancels it.
// Once both are open, Run logs listening for each, makes both report ready
// (/readyz 200, gRPC health SERVING), logs ready, and starts answering. A
// signal received before then starts the shutdown at once.
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
//  4. The admin server stops, within the same deadline.
//  5. A listener that failed during the shutdown is logged as
//     listener_failed, then shutdown_complete is logged.
//
// A second signal, at any step, or the deadline of steps 3 and 4, closes
// every remaining connection at once, cancelling the calls in progress:
// shutdown_complete then reports outcome=forced.
//
// # Result
//
// Run returns nil after a shutdown that a signal started and in which every
// call finished. It returns an error when a listener could not open or
// failed, and after a forced shutdown. It logs every error before returning
// it.
package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gtriggiano/tessera/internal/admin"
	"github.com/gtriggiano/tessera/internal/config"
	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/extauthz"
	"github.com/gtriggiano/tessera/internal/metrics"
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
	// Listen opens a listener: the gRPC listener, then the admin listener.
	// ctx ends when a signal arrives or when the opening takes too long. Nil
	// uses net.ListenConfig.
	Listen func(ctx context.Context, network, address string) (net.Listener, error)

	// The hooks of the tests. decider replaces the engine; afterDrain runs at
	// the end of server.drain_delay, before the gRPC server stops; served
	// runs when the server of a listener returns; listenTimeout replaces the
	// constant.
	decider       extauthz.Decider
	afterDrain    func()
	served        func(listener string, err error)
	listenTimeout time.Duration
}

// Run serves cfg, a loaded and valid configuration, until a shutdown. See
// the package documentation for the startup, the shutdown and the result.
func Run(cfg *config.Config, opts Options) error {
	logs := opts.Logs
	if logs == nil {
		logs = io.Discard
	}
	logger := telemetry.NewLogger(logs, cfg.Log)
	logger.Info("starting", "version", opts.Build.Version, "revision", opts.Build.Revision, "config_path", opts.ConfigPath)

	decider := opts.decider
	if decider == nil {
		decider = engine.New(cfg.Policy.DefaultAction)
	}
	m := metrics.New(opts.Build, opts.ConfigHash)
	grpcServer := extauthz.NewServer(decider, extauthz.Options{
		MaxConnections:       cfg.Server.MaxConnections,
		Refused:              m.ConnectionRefused,
		MaxConcurrentStreams: uint32(cfg.Server.MaxConcurrentStreams),
		MaxConnectionAge:     cfg.Server.MaxConnectionAge,
	})
	adminServer := admin.NewServer(m.Handler())

	grpcListener, adminListener, signalled, err := open(cfg, opts, logger)
	if err != nil {
		return err
	}
	logger.Info("listening", "listener", listenerGRPC, "address", addressOf(grpcListener))
	logger.Info("listening", "listener", listenerAdmin, "address", addressOf(adminListener))

	adminServer.SetReady()
	grpcServer.SetServing(true)
	logger.Info("ready")

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
	forcedBy := shutdown(cfg.Server, opts, logger, grpcServer, adminServer)
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

// shutdown runs steps 1 to 4 of the shutdown, and returns what forced it:
// "signal", "timeout", or "" when every call and request finished.
func shutdown(cfg config.Server, opts Options, logger *slog.Logger, grpcServer *extauthz.Server, adminServer *admin.Server) string {
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
		_ = adminServer.Close()
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
		_ = adminServer.Close()
		return forcedBy()
	}
	if err := adminServer.Shutdown(ctx); err != nil {
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

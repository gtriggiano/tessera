// Command tessera is an external authorization service for Envoy.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/gtriggiano/tessera/internal/cli"
)

// These values may be set at build time with -ldflags -X.
var (
	version   = "dev"
	revision  = "unknown"
	buildTime = ""
)

func main() {
	// Deliver SIGINT and SIGTERM to the command rather than terminating: the
	// first cancels the running command. The buffer holds both signals.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	// A closed standard output or error must not end the process: writes
	// then fail with EPIPE, and the command exits through its normal error
	// path.
	signal.Ignore(syscall.SIGPIPE)
	os.Exit(cli.Run(context.Background(), os.Args[1:], cli.Options{
		Out:     os.Stdout,
		Err:     os.Stderr,
		Build:   cli.BuildInfo{Version: version, Revision: revision, BuildTime: buildTime},
		Signals: signals,
	}))
}

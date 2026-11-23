// Package cli implements tessera's command interface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gtriggiano/tessera/internal/app"
	"github.com/gtriggiano/tessera/internal/config"
)

// BuildInfo identifies the binary. Empty version, revision and GoVersion values
// use local development defaults; an empty BuildTime is omitted from output.
type BuildInfo struct {
	Version   string
	Revision  string
	GoVersion string
	BuildTime string
}

// Options supplies process dependencies. Nil writers discard output, and a nil
// LookupEnv reports every environment variable as unset.
type Options struct {
	Out       io.Writer
	Err       io.Writer
	LookupEnv config.LookupEnv
	Build     BuildInfo
	// Signals delivers the process's shutdown signals. The first cancels the
	// running command, like a signal-cancelled context, except for serve,
	// which receives them all. Nil delivers none.
	Signals <-chan os.Signal
	// Serve runs serve with a valid configuration. Nil uses app.Run.
	Serve func(*config.Config, app.Options) error

	// logs is Out as the caller gave it, for the log events of serve: a
	// failed write of an event must not make the command fail.
	logs io.Writer
}

// Run executes a new command tree. It returns 0 on success, 2 for usage or
// configuration errors, and 1 for runtime failures such as writing output.
func Run(ctx context.Context, args []string, opts Options) int {
	opts = defaults(opts)
	// Ends the signal watcher of a command, if any, when Run returns.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	opts.logs = opts.Out
	output := &checkedWriter{writer: opts.Out}
	opts.Out = output
	opts.Err = &checkedWriter{writer: opts.Err}
	cmd := newCommand(opts)
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		var logged loggedError
		if errors.As(err, &logged) {
			return 1
		}
		// Cobra's lookup errors include the argument. Avoid echoing arbitrary
		// user input, which may contain a mistakenly pasted credential.
		message := err.Error()
		if strings.HasPrefix(message, "unknown command ") || strings.HasPrefix(message, "unknown help topic ") {
			message = "unknown command; see tessera --help"
		}
		if _, writeErr := fmt.Fprintf(opts.Err, "tessera: %s\n", message); writeErr != nil {
			return 1
		}
		var failure runtimeError
		if errors.As(err, &failure) {
			return 1
		}
		return 2
	}
	// Cobra's built-in help renderer does not return write failures from
	// ExecuteContext, so inspect the output writer as well.
	if output.failed {
		fmt.Fprintln(opts.Err, "tessera: cannot write output")
		return 1
	}
	return 0
}

// newCommand builds an independent command tree for defaulted opts.
func newCommand(opts Options) *cobra.Command {
	root := &cobra.Command{
		Use:               "tessera",
		Short:             "External authorization service for Envoy",
		Args:              noArgs,
		SilenceErrors:     true,
		SilenceUsage:      true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return runtimeError{err}
			}
			cancelOnSignal(cmd, opts.Signals)
			return nil
		},
	}
	root.SetOut(opts.Out)
	root.SetErr(opts.Err)
	root.SetFlagErrorFunc(func(_ *cobra.Command, _ error) error {
		return errors.New("invalid command flags; see --help")
	})
	root.SetHelpCommand(&cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		RunE: func(_ *cobra.Command, args []string) error {
			target, remaining, err := root.Find(args)
			if err != nil || len(remaining) != 0 {
				return errors.New("unknown command; see tessera --help")
			}
			return help(target, nil)
		},
	})
	root.RunE = help
	root.AddCommand(serveCommand(opts), evalCommand(opts.LookupEnv), versionCommand(opts.Build), configCommand(opts.LookupEnv))
	return root
}

// cancelOnSignal makes the first signal cancel cmd's context. The watcher
// ends with that context, which Run cancels when it returns.
func cancelOnSignal(cmd *cobra.Command, signals <-chan os.Signal) {
	if signals == nil {
		return
	}
	ctx, cancel := context.WithCancel(cmd.Context())
	go func() {
		defer cancel()
		select {
		case <-signals:
		case <-ctx.Done():
		}
	}()
	cmd.SetContext(ctx)
}

func defaults(opts Options) Options {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Err == nil {
		opts.Err = io.Discard
	}
	if opts.LookupEnv == nil {
		opts.LookupEnv = func(string) (string, bool) { return "", false }
	}
	if opts.Build.Version == "" {
		opts.Build.Version = "dev"
	}
	if opts.Build.Revision == "" {
		opts.Build.Revision = "unknown"
	}
	if opts.Build.GoVersion == "" {
		opts.Build.GoVersion = runtime.Version()
	}
	if opts.Serve == nil {
		opts.Serve = app.Run
	}
	return opts
}

// versionOutput is the JSON form of version. Its field names are a public
// interface.
type versionOutput struct {
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	GoVersion string `json:"go_version"`
	BuildTime string `json:"build_time,omitempty"`
}

func versionCommand(build BuildInfo) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch format {
			case "text":
				info := fmt.Sprintf("tessera %s (revision %s; %s", build.Version, build.Revision, build.GoVersion)
				if build.BuildTime != "" {
					info += "; built " + build.BuildTime
				}
				return write(cmd.OutOrStdout(), []byte(info+")\n"))
			case "json":
				data, err := json.Marshal(versionOutput(build))
				if err != nil {
					return runtimeError{errors.New("cannot render build information")}
				}
				return write(cmd.OutOrStdout(), append(data, '\n'))
			default:
				// The value is not repeated: it is arbitrary user input.
				return errors.New("invalid --output; want text or json")
			}
		},
	}
	cmd.Flags().StringVar(&format, "output", "text", "output format: text or json")
	return cmd
}

const configFlagUsage = "configuration file (overrides TESSERA_CONFIG)"

// loadConfig reads the configuration file named by the --config flag of cmd,
// whose value is flag, when present, or else by TESSERA_CONFIG. It returns
// the path it read.
func loadConfig(cmd *cobra.Command, lookup config.LookupEnv, flag string) (string, *config.Config, error) {
	selected, ok := flag, cmd.Flags().Changed("config")
	if !ok {
		selected, ok = lookup("TESSERA_CONFIG")
	}
	switch {
	case !ok:
		return "", nil, errors.New("no configuration file; use --config or TESSERA_CONFIG")
	case selected == "":
		return "", nil, errors.New("configuration file path must not be empty")
	}
	cfg, err := config.Load(selected, lookup)
	return selected, cfg, err
}

func configCommand(lookup config.LookupEnv) *cobra.Command {
	var path string
	parent := &cobra.Command{
		Use:   "config",
		Short: "Check or print the configuration",
		Long:  "Check or print the configuration file, selected with --config or, when the flag is\nabsent, with the TESSERA_CONFIG environment variable. validate reads the files\nof the sets and the databases too; print reads only the configuration file.",
		Args:  noArgs,
		RunE:  help,
	}
	parent.PersistentFlags().StringVar(&path, "config", "", configFlagUsage)
	load := func(cmd *cobra.Command) (*config.Config, error) {
		_, cfg, err := loadConfig(cmd, lookup, path)
		return cfg, err
	}
	validate := &cobra.Command{
		Use:   "validate",
		Short: "Check the configuration",
		Long: "Check the configuration file, the file of every set and every database, as\n" +
			"serve does before it opens a socket. Write each warning on standard error,\n" +
			"which does not make the configuration invalid.\n\n" +
			"Exit status: 0 for a valid configuration, warnings or not; 2 for an invalid one.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load(cmd)
			if err != nil {
				return err
			}
			if _, err := app.LoadSets(cfg, nil); err != nil {
				return err
			}
			if _, err := app.LoadDatabases(cfg); err != nil {
				return err
			}
			for _, w := range cfg.Warnings() {
				if err := write(cmd.ErrOrStderr(), []byte("tessera: warning: "+w.String()+"\n")); err != nil {
					return err
				}
			}
			return write(cmd.OutOrStdout(), []byte("configuration valid\n"))
		},
	}
	printCommand := &cobra.Command{
		Use:   "print",
		Short: "Print the configuration with its defaults",
		Long:  "Print the configuration with its defaults applied, as YAML. Every value that\ncontains a ${...} substitution is replaced with [REDACTED].",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load(cmd)
			if err != nil {
				return err
			}
			data, err := cfg.Redacted()
			if err != nil {
				return runtimeError{errors.New("cannot render the configuration")}
			}
			return write(cmd.OutOrStdout(), data)
		},
	}
	parent.AddCommand(validate, printCommand)
	return parent
}

func noArgs(_ *cobra.Command, args []string) error {
	if len(args) != 0 {
		return errors.New("unexpected arguments; see --help")
	}
	return nil
}

func help(cmd *cobra.Command, _ []string) error {
	if err := cmd.Help(); err != nil {
		return runtimeError{err}
	}
	return nil
}

type runtimeError struct{ error }

// loggedError is a runtime failure that serve has already reported as a log
// event: Run prints nothing more.
type loggedError struct{ error }

type checkedWriter struct {
	writer io.Writer
	failed bool
}

func (w *checkedWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	if err != nil || n != len(data) {
		w.failed = true
		return n, errors.New("cannot write output")
	}
	return n, nil
}

func write(out io.Writer, data []byte) error {
	if n, err := out.Write(data); err != nil || n != len(data) {
		return runtimeError{errors.New("cannot write output")}
	}
	return nil
}

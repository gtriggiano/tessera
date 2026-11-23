package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/spf13/cobra"

	"github.com/gtriggiano/tessera/internal/app"
	"github.com/gtriggiano/tessera/internal/metrics"
)

func serveCommand(opts Options) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Answer Envoy's authorization requests",
		Long: "Answer Envoy's external authorization requests on server.listen, and the health\n" +
			"checks and metrics on admin.listen. The configuration file is selected with\n" +
			"--config or, when the flag is absent, with the TESSERA_CONFIG environment\n" +
			"variable. Every set is loaded, its file read, and every database of\n" +
			"enrichment, before a socket opens; then the file of each set is checked every\n" +
			"sets.<name>.refresh, and that of each database every\n" +
			"enrichment.<kind>.refresh. Log events go to standard output.\n\n" +
			"SIGINT or SIGTERM shuts down gracefully, within server.drain_delay and\n" +
			"server.shutdown_timeout; a second signal closes every connection at once.\n\n" +
			"Exit status: 0 after a graceful shutdown; 1 when a listener cannot open or\n" +
			"fails, or when the shutdown is forced; 2 for usage and configuration errors,\n" +
			"an invalid set or database included.",
		Args: noArgs,
		// serve receives every signal itself, so that a second one can force
		// the shutdown.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return runtimeError{err}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			selected, cfg, err := loadConfig(cmd, opts.LookupEnv, path)
			if err != nil {
				return err
			}
			// An invalid set or database stops the startup as a
			// configuration error.
			loaded, err := app.LoadSets(cfg, nil)
			if err != nil {
				return err
			}
			databases, err := app.LoadDatabases(cfg)
			if err != nil {
				return err
			}
			// tessera_config_info carries the hash of what config print
			// writes, so that a user can compute it.
			printed, err := cfg.Redacted()
			if err != nil {
				return runtimeError{errors.New("cannot render the configuration")}
			}
			hash := sha256.Sum256(printed)
			err = opts.Serve(cfg, app.Options{
				Logs:       opts.logs,
				Build:      metrics.BuildInfo{Version: opts.Build.Version, Revision: opts.Build.Revision, GoVersion: opts.Build.GoVersion},
				ConfigPath: selected,
				ConfigHash: hex.EncodeToString(hash[:]),
				Signals:    opts.Signals,
				Sets:       loaded,
				Databases:  databases,
			})
			if err != nil {
				return loggedError{err}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "config", "", configFlagUsage)
	return cmd
}

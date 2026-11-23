package main

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/fluxsim"
)

func newFluxsimCmd() *cobra.Command {
	var addr, remote string
	cmd := &cobra.Command{
		Use:   "fluxsim",
		Short: "Run the simulated Flux (GET /status, GET /clusters/{id}/objects)",
		Long: `Run the simulated Flux. It fetches the state repo from the forge every
PK_POLL_INTERVAL (3s). Each cluster follows the tag ring/<ring> when it
exists, else main; digital twins (.pk/labs, purpose twin) follow their ref.
A new revision is applied after PK_FLUX_DELAY (4s) and verified after
PK_FLUX_VERIFY_DELAY (6s). POST /clusters/{id}/fail makes the health checks
of a cluster fail (demo).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.FromEnv()
			if addr != "" {
				cfg.FluxAddr = addr
			}
			if remote == "" {
				remote = os.Getenv("PK_FLUX_REMOTE")
			}
			return fluxsim.Run(cmd.Context(), fluxsim.Options{
				Addr:            cfg.FluxAddr,
				CredentialsPath: cfg.ForgeCredentials,
				RemoteURL:       remote,
				ForgeURL:        cfg.ForgeURL,
				ForgeURLSet:     os.Getenv("PK_FORGE_URL") != "",
				Owner:           cfg.ForgeOwner,
				Repo:            cfg.ForgeRepo,
				DataDir:         cfg.DataDir,
				Interval:        cfg.PollInterval,
				ApplyDelay:      fluxEnvDuration("PK_FLUX_DELAY", 4*time.Second),
				VerifyDelay:     fluxEnvDuration("PK_FLUX_VERIFY_DELAY", 6*time.Second),
			})
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "listen address (default PK_FLUX_ADDR or 127.0.0.1:8081)")
	cmd.Flags().StringVar(&remote, "remote", "", "clone URL or path of the state repo (default from the forge credentials; PK_FLUX_REMOTE)")
	return cmd
}

func fluxEnvDuration(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(k)); err == nil && d >= 0 {
		return d
	}
	return def
}

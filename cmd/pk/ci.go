package main

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/dennisklein/platform-kit/internal/ci"
	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

func newCICmd() *cobra.Command {
	var once bool
	cmd := &cobra.Command{
		Use:   "ci",
		Short: "Run the CI policy check: post pk/policy on the head of each open pull request",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.FromEnv()
			return ci.Run(cmd.Context(), ci.Options{
				CredentialsPath: cfg.ForgeCredentials,
				DataDir:         cfg.DataDir,
				Interval:        cfg.PollInterval,
				Once:            once,
				Policy: func(mainDir func(context.Context) (string, error)) policy.Engine {
					return policy.NewEngine(mainDir)
				},
				Renderer: render.New(all.Registry()),
			})
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "run one pass and exit")
	return cmd
}

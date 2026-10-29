package main

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/seed"
)

func newForgeInitCmd() *cobra.Command {
	var reset bool
	cmd := &cobra.Command{
		Use:   "forge-init",
		Short: "Configure the forge (users, tokens, repo, protection) and push the seed state repo",
		Long: "Configure the forge (users, tokens, repo, branch protection) and push the seed\n" +
			"state repo with its history and open changes. Does nothing when the repo exists,\n" +
			"unless --reset. Env: PK_FORGE_URL, PK_FORGE_ADMIN_USER, PK_FORGE_ADMIN_PASSWORD,\n" +
			"PK_FORGE_CREDENTIALS, PK_FORGE_PUBLIC_URL (URL written for other containers).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.FromEnv()
			return seed.ForgeInit(cmd.Context(), seed.Options{
				ForgeURL:        cfg.ForgeURL,
				AdminUser:       envOr("PK_FORGE_ADMIN_USER", "pk-admin"),
				AdminPassword:   envOr("PK_FORGE_ADMIN_PASSWORD", "pk-admin-demo"),
				Owner:           cfg.ForgeOwner,
				Repo:            cfg.ForgeRepo,
				CredentialsPath: cfg.ForgeCredentials,
				CredentialsURL:  envOr("PK_FORGE_PUBLIC_URL", cfg.ForgeURL),
				Reset:           reset,
				WorkDir:         filepath.Join(cfg.DataDir, "forge-init"),
			})
		},
	}
	cmd.Flags().BoolVar(&reset, "reset", false, "delete and rebuild the state repo")
	return cmd
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

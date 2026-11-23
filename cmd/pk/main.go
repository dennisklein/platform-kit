// Command pk is platform-kit: the bench, the CI policy check, the simulated
// Flux, the forge setup and a CLI generated from the bench API.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dennisklein/platform-kit/internal/bench"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	bench.Version = version
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRoot().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "pk:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	var verbose bool
	root := &cobra.Command{
		Use:           "pk",
		Short:         "platform-kit: change the desired state of the platform, one change at a time",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			level := slog.LevelInfo
			if verbose || os.Getenv("PK_DEBUG") != "" {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
		},
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "debug logging")
	root.AddCommand(
		newBenchCmd(),
		newCICmd(),
		newFluxsimCmd(),
		newForgeInitCmd(),
		newVersionCmd(),
	)
	addAPICommands(root)
	return root
}

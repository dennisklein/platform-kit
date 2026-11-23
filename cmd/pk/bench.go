package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/dennisklein/platform-kit/internal/bench"
	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/web"
)

func newBenchCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Run the bench (local mode): API, web UI, events and MCP on one port",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.FromEnv()
			if addr != "" {
				cfg.Addr = addr
			}
			return runBench(cmd.Context(), cfg)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "listen address (default PK_ADDR or 127.0.0.1:8080)")
	return cmd
}

// wire functions fill the dependencies of one area each, in this order.
// Each may start background work bound to ctx.
var wires = []struct {
	area string
	fn   func(ctx context.Context, cfg config.Config, d *bench.Deps) error
}{
	{"kinds", wireKinds},
	{"changes", wireChanges},
	{"labs", wireLabs},
	{"llm", wireLLM},
}

func runBench(ctx context.Context, cfg config.Config) error {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	d := bench.Deps{Events: events.NewHub()}
	for _, w := range wires {
		if err := w.fn(ctx, cfg, &d); err != nil {
			return fmt.Errorf("wire %s: %w", w.area, err)
		}
	}
	srv, err := bench.New(cfg, d, web.Dist())
	if err != nil {
		return err
	}
	slog.Debug("bench data", "dir", cfg.DataDir)
	return srv.Run(ctx)
}

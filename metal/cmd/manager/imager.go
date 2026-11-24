package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/dennisklein/platform-kit/metal/internal/imagerrun"
)

// The imager Jobs run this binary too: the Talos imager prints the machine
// config it embeds, and its image has no shell to keep that out of the log.
// An init container copies the manager binary into the pod (imager-stage),
// and the build container runs the imager through it (imager-run). See
// internal/imagerrun.
const (
	cmdImagerStage = "imager-stage"
	cmdImagerRun   = "imager-run"
)

// runSubcommand runs one of the build Job's commands. ok is false when args
// name none, and the manager starts as usual.
func runSubcommand(args []string, out io.Writer) (code int, ok bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case cmdImagerStage:
		if len(args) != 2 {
			fmt.Fprintf(out, "usage: manager %s <destination>\n", cmdImagerStage)
			return 2, true
		}
		if err := imagerrun.Stage(args[1]); err != nil {
			fmt.Fprintf(out, "%s: %v\n", cmdImagerStage, err)
			return 1, true
		}
		return 0, true
	case cmdImagerRun:
		// manager imager-run <config> <imager> [args...]
		if len(args) < 3 {
			fmt.Fprintf(out, "usage: manager %s <config to embed> <imager> [imager args...]\n", cmdImagerRun)
			return 2, true
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return imagerrun.Run(ctx, args[2:], args[1], out), true
	}
	return 0, false
}

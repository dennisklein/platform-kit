package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim/fleet"
)

// simFleetCmd serves a fleet of simulated BMCs from one file: an HTTPS
// endpoint, a personality and an identity per machine, and behind each BMC
// nothing, a QEMU VM or a Talos container. It runs until interrupted, then
// powers its machines off.
func simFleetCmd(args []string) {
	fs := flag.NewFlagSet("sim fleet", flag.ExitOnError)
	config := fs.String("config", "", "fleet file (YAML): the machines, their BMC addresses, identities and backends")
	verbose := fs.Bool("verbose", false, "log every request (default: only state-changing ones)")
	_ = fs.Parse(args)
	if *config == "" {
		fmt.Fprintln(os.Stderr, "usage: pk-metal sim fleet --config <file> [--verbose]")
		os.Exit(2)
	}
	cfg, err := fleet.Load(*config)
	if err != nil {
		log.Fatalf("sim fleet: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	f, err := fleet.Start(ctx, cfg, fleet.Options{
		Wrap: func(machine string, h http.Handler) http.Handler {
			return logRequests(h, "sim["+machine+"]", *verbose)
		},
	})
	if err != nil {
		log.Fatalf("sim fleet: %v", err)
	}
	// Logged only now, so that each line means the listener is really up.
	for _, ep := range f.Endpoints() {
		account := "no accounts, open"
		if ep.Username != "" {
			account = "account " + ep.Username
		}
		log.Printf("sim fleet: %s: %s BMC on %s/redfish/v1 (%s), backend %s",
			ep.Name, ep.Personality, ep.URL, account, ep.Backend)
	}
	if url := f.MediaURL(); url != "" {
		log.Printf("sim fleet: media directory on %s/", url)
	}

	var failure error
	select {
	case <-ctx.Done():
		log.Print("sim fleet: stopping")
	case failure = <-f.Failed():
	}
	closing, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := f.Close(closing); err != nil {
		log.Printf("sim fleet: shutdown: %v", err)
	}
	if failure != nil {
		log.Fatalf("sim fleet: %v", failure)
	}
}

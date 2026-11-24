package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim/fleet"
)

// defaultStateListen is where `sim fleet` serves its state endpoint unless
// told otherwise: loopback, next to the lab's BMC ports.
const defaultStateListen = "127.0.0.1:8440"

// simFleetCmd serves a fleet of simulated BMCs from one file: an HTTPS
// endpoint, a personality and an identity per machine, and behind each BMC
// nothing, a QEMU VM or a Talos container. It runs until interrupted, then
// powers its machines off.
func simFleetCmd(args []string) {
	fs := newFlags("sim fleet")
	config := fs.String("config", "", "fleet file (YAML): the machines, their BMC addresses, identities and backends")
	verbose := fs.Bool("verbose", false, "log every request (default: only state-changing ones)")
	stateListen := fs.String("state-listen", defaultStateListen,
		"address of the read-only state endpoint (plain HTTP, GET "+fleet.StatePath+`; "" serves none); keep it on loopback`)
	parseFlags(fs, args)
	if *config == "" {
		badUsage(fs, "--config is required")
	}
	cfg, err := fleet.Load(*config)
	if err != nil {
		dief("sim fleet: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	f, err := fleet.Start(ctx, cfg, fleet.Options{
		StateListen: *stateListen,
		Wrap: func(machine string, h http.Handler) http.Handler {
			return logRequests(h, "sim["+machine+"]", *verbose)
		},
	})
	if err != nil {
		dief("sim fleet: %v", err)
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
	if url := f.StateURL(); url != "" {
		log.Printf("sim fleet: state of every machine on %s (read-only)", url)
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
		dief("sim fleet: %v", failure)
	}
}

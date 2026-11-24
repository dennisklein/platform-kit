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
	"github.com/dennisklein/platform-kit/metal/internal/machine/qemu"
)

// guardTimeout maps the --hang-timeout flag onto the fleet option: on the
// command line 0 means never, in the option it means the default.
func guardTimeout(flag time.Duration) time.Duration {
	if flag <= 0 {
		return -1
	}
	return flag
}

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
	hangTimeout := fs.Duration("hang-timeout", qemu.DefaultHangTimeout,
		"power-cycle a VM (up to three times per power-on) when its console shows only firmware screen control for this long after a power-on (0: never)")
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
		HangTimeout: guardTimeout(*hangTimeout),
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
		log.Printf("sim fleet: state of every machine on %s (read-only; watch it with: pk-metal top --fleet %s)", url, url)
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

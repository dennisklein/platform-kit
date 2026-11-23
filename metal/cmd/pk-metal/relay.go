package main

import (
	"context"
	"flag"
	"log"
	"net"

	"github.com/dennisklein/platform-kit/metal/internal/relay"
)

// relayCmd runs the L4 reverse tunnel on a box with BMC reach — an org Linux box
// at day-0, the 1034-macvlan pod in steady state, loopback under test. The
// tunnel port stays on loopback by default: reach it over `ssh -L`, never expose
// it — its DIAL verb is an open onward-dialer for whoever connects.
func relayCmd(args []string) {
	fs, front, tunnel, advertise := relayFlags()
	parseFlags(fs, args)
	if fs.NArg() != 0 {
		badUsage(fs, "relay takes no arguments, only flags")
	}

	frontLn, err := net.Listen("tcp", *front)
	if err != nil {
		die(err)
	}
	tunnelLn, err := net.Listen("tcp", *tunnel)
	if err != nil {
		die(err)
	}
	shown := *advertise
	if shown == "" {
		shown = frontLn.Addr().String()
	}
	log.Printf("relay: front %s (advertised %s), tunnel %s", frontLn.Addr(), shown, tunnelLn.Addr())
	die(relay.Serve(context.Background(), tunnelLn, frontLn, *advertise))
}

// relayFlags declares the relay's flags; split out so a test can check the
// defaults without starting listeners.
func relayFlags() (fs *flag.FlagSet, front, tunnel, advertise *string) {
	fs = newFlags("relay")
	front = fs.String("front", "127.0.0.1:8443", "BMC-facing listen address (media fetches arrive here)")
	tunnel = fs.String("tunnel", "127.0.0.1:9443", "operator-facing listen address (keep on loopback, reach over ssh -L)")
	advertise = fs.String("advertise", "", "front address as BMCs reach it (default: the bound address)")
	return fs, front, tunnel, advertise
}

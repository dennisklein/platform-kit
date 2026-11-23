package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"

	"github.com/dennisklein/platform-kit/metal/internal/relay"
)

// relayCmd runs the L4 reverse tunnel on a box with BMC reach — an org Linux box
// at day-0, the 1034-macvlan pod in steady state, loopback under test. The
// tunnel port stays on loopback by default: reach it over `ssh -L`, never expose
// it — its DIAL verb is an open onward-dialer for whoever connects.
func relayCmd(args []string) {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	front := fs.String("front", "127.0.0.1:8443", "BMC-facing listen address (media fetches arrive here)")
	tunnel := fs.String("tunnel", "127.0.0.1:9443", "operator-facing listen address (keep on loopback, reach over ssh -L)")
	advertise := fs.String("advertise", "", "front address as BMCs reach it (default: the bound address)")
	_ = fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: pk-metal relay [--front addr] [--tunnel addr] [--advertise host:port]")
		os.Exit(2)
	}

	frontLn, err := net.Listen("tcp", *front)
	if err != nil {
		log.Fatal(err)
	}
	tunnelLn, err := net.Listen("tcp", *tunnel)
	if err != nil {
		log.Fatal(err)
	}
	shown := *advertise
	if shown == "" {
		shown = frontLn.Addr().String()
	}
	log.Printf("relay: front %s (advertised %s), tunnel %s", frontLn.Addr(), shown, tunnelLn.Addr())
	log.Fatal(relay.Serve(context.Background(), tunnelLn, frontLn, *advertise))
}

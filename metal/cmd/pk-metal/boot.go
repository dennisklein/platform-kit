package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/boot"
	"github.com/dennisklein/platform-kit/metal/internal/relay"
)

// bootCmd climbs the media ladder against one BMC — the CLI's first write verb.
// The config comes from a rendered file (the render verb's output); the asset
// server lives inside this process for the grant's lifetime, either on its own
// BMC-reachable listener or through a relay front.
func bootCmd(args []string) {
	fs := flag.NewFlagSet("boot", flag.ExitOnError)
	configPath := fs.String("config", "", "rendered machine config file (required)")
	node := fs.String("node", "", "node name — names the granted .iso (required)")
	mediaID := fs.String("media", "", "media slot id (default: first insertable)")
	system := fs.String("system", "", "system id (default: the endpoint's only system)")
	ttl := fs.Duration("ttl", 10*time.Minute, "grant TTL — bounds the serve-and-wait")
	listen := fs.String("listen", "", "asset server listen address, must be BMC-reachable (ignored with --relay)")
	relayAddr := fs.String("relay", "", "relay tunnel address to serve through")
	hold := fs.Bool("hold", false, "keep serving and mounted until interrupted")
	user, insecure := connectFlags(fs)
	_ = fs.Parse(args)
	if fs.NArg() != 1 || *configPath == "" || *node == "" {
		fmt.Fprintln(os.Stderr, "usage: pk-metal boot --config <rendered.yaml> --node <name> [flags] <endpoint-url>")
		os.Exit(2)
	}
	if (*listen == "") == (*relayAddr == "") {
		fmt.Fprintln(os.Stderr, "boot: exactly one of --listen or --relay must be given")
		os.Exit(2)
	}

	cfg, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var rc *relay.Client
	if *relayAddr != "" {
		if rc, err = relay.Dial(ctx, *relayAddr); err != nil {
			log.Fatal(err)
		}
		log.Printf("boot: registered at relay %s, front %s", *relayAddr, rc.FrontAddr())
	}

	c := mustConnect(fs.Arg(0), *user, *insecure, false) // the ladder mounts, overrides, powers
	defer c.Close()

	res, err := boot.Run(ctx, boot.Options{
		Client:   c,
		NodeName: *node,
		Config:   cfg,
		MediaID:  *mediaID,
		SystemID: *system,
		TTL:      *ttl,
		Listen:   *listen,
		Relay:    rc,
		Hold:     *hold,
		Logf:     log.Printf,
	})
	if err != nil {
		log.Fatalf("boot: FAIL — %v", err)
	}
	if !res.ReadComplete {
		log.Fatalf("boot: image never fully read (%d of %d bytes served before the grant ended)",
			res.ServedBytes, res.Size)
	}
	log.Printf("boot: OK — %d B image read (%d B served)", res.Size, res.ServedBytes)
}

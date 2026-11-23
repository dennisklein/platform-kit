package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/boot"
	"github.com/dennisklein/platform-kit/metal/internal/relay"
)

func init() {
	verbHelp["boot"] = `
boot works without the operator, for the first machines of a site (day 0).
It packs a rendered machine config (from pk-metal render) into an ISO, serves
it over HTTPS under a one-time user and password, mounts it as virtual media
through the BMC, sets the next boot to that medium, and powers the machine on.

If the machine is already on, boot REBOOTS it with a hard reset: whatever runs
there is killed without a clean shutdown. boot prints its steps first and then
refuses unless you give --yes. With --no-power it only mounts the medium and
sets the next boot; you restart the machine yourself (for example from the
BMC web console) and boot waits until the image has been read.

The BMC must reach the address boot serves on: give --listen with an address
on the BMC network, or --relay to serve through pk-metal relay on a host that
the BMCs can reach. The BMC password comes from $PK_METAL_BMC_PASSWORD only.

example:
  PK_METAL_BMC_PASSWORD=... pk-metal boot --config clusterconfig/node-1.yaml \
      --node node-1 --listen 10.0.0.2:8443 https://10.0.0.11`
}

// bootCmd climbs the media ladder against one BMC — the CLI's first write verb.
// The config comes from a rendered file (the render verb's output); the asset
// server lives inside this process for the grant's lifetime, either on its own
// BMC-reachable listener or through a relay front.
func bootCmd(args []string) {
	fs := newFlags("boot")
	configPath := fs.String("config", "", "rendered machine config file (required)")
	node := fs.String("node", "", "node name; names the served .iso (required)")
	mediaID := fs.String("media", "", "virtual media slot id (default: the first one that can mount an image, CD first)")
	system := fs.String("system", "", "system id (default: the BMC's only system)")
	ttl := fs.Duration("ttl", 10*time.Minute, "how long the image is served, and so how long boot waits for the machine to read it")
	listen := fs.String("listen", "", "address to serve the image on; the BMC must reach it (not with --relay)")
	relayAddr := fs.String("relay", "", "address of a pk-metal relay to serve the image through (not with --listen)")
	hold := fs.Bool("hold", false, "keep serving and keep the medium mounted until interrupted (Ctrl-C)")
	yes := fs.Bool("yes", false, "allow a hard reset of a machine that is on")
	forceRestart := fs.Bool("force-restart", false, "same as --yes")
	noPower := fs.Bool("no-power", false, "mount the medium and set the next boot only; you restart the machine")
	user, insecure := connectFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 1 || *configPath == "" || *node == "" {
		badUsage(fs, "--config, --node and one BMC URL are required")
	}
	if (*listen == "") == (*relayAddr == "") {
		badUsage(fs, "give exactly one of --listen or --relay")
	}
	if *noPower && (*yes || *forceRestart) {
		badUsage(fs, "--no-power does not touch power, so --yes or --force-restart has no meaning with it")
	}

	cfg, err := os.ReadFile(*configPath)
	if err != nil {
		die(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var rc *relay.Client
	if *relayAddr != "" {
		if rc, err = relay.Dial(ctx, *relayAddr); err != nil {
			die(err)
		}
		log.Printf("boot: registered at relay %s, front %s", *relayAddr, rc.FrontAddr())
	}

	c := mustConnect(fs.Arg(0), *user, *insecure, false) // the ladder mounts, overrides, powers
	defer c.Close()

	res, err := boot.Run(ctx, boot.Options{
		Client:       c,
		NodeName:     *node,
		Config:       cfg,
		MediaID:      *mediaID,
		SystemID:     *system,
		TTL:          *ttl,
		Listen:       *listen,
		Relay:        rc,
		Hold:         *hold,
		AllowRestart: *yes || *forceRestart,
		NoPower:      *noPower,
		Logf:         log.Printf,
	})
	if errors.Is(err, boot.ErrMachineOn) {
		dief("%s", machineOnMessage)
	}
	if err != nil {
		dief("boot failed: %v", err)
	}
	if !res.ReadComplete {
		dief("the machine did not read the whole image (%d of %d bytes served before the time given by --ttl ran out)",
			res.ServedBytes, res.Size)
	}
	log.Printf("boot: OK, the machine read the %d byte image (%d bytes served)", res.Size, res.ServedBytes)
}

// machineOnMessage is the refusal when boot would reset a running machine.
const machineOnMessage = "the machine is on, and booting it means a hard reset that kills what runs there. " +
	"Nothing on the BMC was changed. Rerun with --yes to allow the reset, or with --no-power " +
	"to mount the medium only and restart the machine yourself"

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

func init() {
	verbHelp["relay"] = `
relay runs on a host that can reach the BMCs. BMCs download boot media from
its front address, and it hands that traffic to pk-metal boot through its
tunnel address; boot's Redfish calls to the BMCs go out the same way.

Every connection to the tunnel must prove it holds the relay's secret, so
nobody else on the host or its network can take the media traffic over or
reach the BMCs through it. Give the secret in $PK_METAL_RELAY_SECRET, or name
a file with --secret-file: when the file does not exist, relay writes a new
secret there (readable by you only) and uses it; an existing file is never
replaced. Hand the same secret to pk-metal boot.

The secret proves who connects; it does not encrypt the tunnel. Keep the
tunnel on loopback (the default) and reach it over ssh -L.

boot's onward dials go only to the targets --allow names: by default any
address on port 443, where Redfish answers. Name the BMC network to narrow
it, e.g. --allow 10.0.0.0/24:443; the relay refuses every other target.

example:
  pk-metal relay --front 10.0.0.2:8443 --allow 10.0.0.0/24:443 --secret-file ~/.config/pk-metal/relay.secret`
}

// relayCmd runs the L4 reverse tunnel on a box with BMC reach: an org Linux box
// at day-0 (before the operator runs), loopback under test. The
// tunnel port stays on loopback by default and every connection to it is
// authenticated with the shared secret: its DIAL verb dials onward into the
// BMC network, to the targets --allow names only, and its REGISTER verb takes
// over the BMCs' media fetches.
func relayCmd(args []string) {
	fs, front, tunnel, advertise, secretFile, allowFlag := relayFlags()
	parseFlags(fs, args)
	if fs.NArg() != 0 {
		badUsage(fs, "relay takes no arguments, only flags")
	}
	allow, err := relay.ParseAllow(*allowFlag)
	if err != nil {
		badUsage(fs, err.Error())
	}
	secret, err := relaySecret(*secretFile, true)
	if err != nil {
		die(err)
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
	log.Printf("relay: front %s (advertised %s), tunnel %s, onward dials to %s", frontLn.Addr(), shown, tunnelLn.Addr(), *allowFlag)
	die(relay.Serve(context.Background(), tunnelLn, frontLn, *advertise, secret, relay.WithAllow(allow)))
}

// relaySecret reads the relay's shared secret: from $PK_METAL_RELAY_SECRET
// when it is set, else from file. create lets the relay itself write a new
// secret to a file that does not exist yet; the operator side never makes one
// up, it must hold the relay's.
func relaySecret(file string, create bool) ([]byte, error) {
	if v, ok := os.LookupEnv(relay.SecretEnv); ok {
		if file != "" {
			return nil, fmt.Errorf("give the relay secret once: $%s or a secret file, not both", relay.SecretEnv)
		}
		return relay.ParseSecret([]byte(v))
	}
	if file == "" {
		return nil, fmt.Errorf("the relay needs a shared secret: set $%s, or give a secret file "+
			"(pk-metal relay --secret-file writes a new one when the file does not exist)", relay.SecretEnv)
	}
	secret, created, err := relay.LoadSecret(file, create)
	if err != nil {
		return nil, err
	}
	if created {
		log.Printf("relay: wrote a new secret to %s; give the same secret to pk-metal boot", file)
	}
	return secret, nil
}

// relayFlags declares the relay's flags; split out so a test can check the
// defaults without starting listeners.
func relayFlags() (fs *flag.FlagSet, front, tunnel, advertise, secretFile, allow *string) {
	fs = newFlags("relay")
	front = fs.String("front", "127.0.0.1:8443", "BMC-facing listen address (media fetches arrive here)")
	tunnel = fs.String("tunnel", "127.0.0.1:9443", "operator-facing listen address (keep on loopback, reach over ssh -L)")
	advertise = fs.String("advertise", "", "front address as BMCs reach it (default: the bound address)")
	secretFile = fs.String("secret-file", "", "file with the shared secret; written with a new one when missing (or set $"+relay.SecretEnv+")")
	allow = fs.String("allow", relay.DefaultAllow, "targets boot may dial through the relay, as <CIDR, IP or *>:<port or *>, comma-separated")
	return fs, front, tunnel, advertise, secretFile, allow
}

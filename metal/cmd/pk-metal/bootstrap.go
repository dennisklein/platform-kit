package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/budimanjojo/talhelper/v3/pkg/decrypt"

	"github.com/dennisklein/platform-kit/metal/internal/bootstrap"
	"github.com/dennisklein/platform-kit/metal/internal/talos/machineapi"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// bootstrapCertTTL bounds the session's client cert — one day-0 run, no file
// left behind worth stealing for long.
const bootstrapCertTTL = time.Hour

// bootstrapCmd is the day-0 rung after a machine boots: wait for it, deliver
// the config where no media channel exists (--apply-insecure — container/dev
// tiers only; on metal the config rides the BMC's virtual media), etcd-bootstrap
// the first control-plane node, wait for Ready.
func bootstrapCmd(args []string) {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	dir := fs.String("dir", "", "directory with TalosCluster/TalosNode manifests (required)")
	secret := fs.String("secret", "", "talsecret file — plain or sops-encrypted (required)")
	nodeName := fs.String("node", "", "TalosNode to bootstrap — exactly one CP, exactly once per cluster (required)")
	endpoint := fs.String("endpoint", "", "machine address (default: the node's first ipAddress)")
	patchRoot := fs.String("patch-root", "", `directory "@./" patch refs resolve against (default: --dir)`)
	applyInsecure := fs.Bool("apply-insecure", false,
		"deliver the config over the UNAUTHENTICATED maintenance API — container/dev tiers only; on metal the config rides the BMC media channel")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall deadline: waiting, apply, bootstrap retries, readiness")
	_ = fs.Parse(args)
	if fs.NArg() != 0 || *dir == "" || *secret == "" || *nodeName == "" {
		fmt.Fprintln(os.Stderr, "usage: pk-metal bootstrap --dir <crs> --secret <talsecret.yaml> --node <name> [flags]")
		os.Exit(2)
	}

	cluster, nodes, err := render.LoadDir(*dir)
	if err != nil {
		log.Fatal(err)
	}
	node := nodes[:0:0]
	for _, n := range nodes {
		if n.Name == *nodeName {
			node = append(node, n)
		}
	}
	if len(node) != 1 {
		log.Fatalf("bootstrap: %s names no TalosNode in %s", *nodeName, *dir)
	}

	target := *endpoint
	if target == "" {
		target = node[0].Spec.IPAddress
		if i := strings.IndexByte(target, ','); i >= 0 {
			target = target[:i]
		}
	}

	root := *patchRoot
	if root == "" {
		root = filepath.Clean(*dir)
	}
	bundle, err := decrypt.DecryptYamlWithSops(*secret)
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := render.Map(cluster, node, root)
	if err != nil {
		log.Fatal(err)
	}
	input, err := render.Input(cfg, bundle)
	if err != nil {
		log.Fatal(err)
	}
	machineConfig, err := render.RenderNode(cfg, *nodeName, input, true)
	if err != nil {
		log.Fatal(err)
	}
	talosconfig, err := render.Talosconfig(cfg, input, bootstrapCertTTL)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := bootstrap.Run(ctx, bootstrap.Options{
		Dial:          machineapi.Dial,
		Endpoint:      target,
		MachineConfig: machineConfig,
		Talosconfig:   talosconfig,
		ApplyInsecure: *applyInsecure,
		Logf:          log.Printf,
	}); err != nil {
		log.Fatalf("bootstrap: FAIL — %v", err)
	}
	log.Printf("bootstrap: OK — %s is running and ready", *nodeName)
}

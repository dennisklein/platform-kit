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

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bootstrap"
	"github.com/dennisklein/platform-kit/metal/internal/talos/machineapi"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// bootstrapCertTTL bounds the session's client cert: one day-0 run, nothing
// left behind worth stealing for long.
const bootstrapCertTTL = time.Hour

// bootstrapCmd is day-0 for a cluster without an operator, on the same library
// the TalosCluster reconciler runs: deliver configs where no media channel
// exists (--apply-insecure, container/dev tiers only; on metal the config
// rides the BMC's virtual media), bootstrap etcd exactly once, wait for every
// node to be Ready.
func bootstrapCmd(args []string) {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	dir := fs.String("dir", "", "directory with TalosCluster/TalosNode manifests (required)")
	secretsFile := fs.String("secrets", "", "secrets bundle from `talosctl gen secrets`, or - for stdin (required)")
	nodeName := fs.String("node", "", "drive only this TalosNode (default: every node in --dir)")
	endpoint := fs.String("endpoint", "", "address of --node (default: its spec.address or first static address)")
	factoryHost := fs.String("image-factory", render.PublicFactory, "image factory host the installer image is named on")
	applyInsecure := fs.Bool("apply-insecure", false,
		"deliver configs over the UNAUTHENTICATED maintenance API: container/dev tiers only; on metal the config rides the BMC media channel")
	timeout := fs.Duration("timeout", 15*time.Minute, "overall deadline: apply, bootstrap retries, readiness")
	_ = fs.Parse(args)
	if fs.NArg() != 0 || *dir == "" || *secretsFile == "" || (*endpoint != "" && *nodeName == "") {
		fmt.Fprintln(os.Stderr, "usage: pk-metal bootstrap --dir <manifests> --secrets <secrets.yaml|-> [--node name [--endpoint addr]] [--apply-insecure]")
		os.Exit(2)
	}

	m, bundle := loadInputs(*dir, *secretsFile)
	if *nodeName != "" {
		n := m.Node(*nodeName)
		if n == nil {
			log.Fatalf("bootstrap: %s names no TalosNode in %s", *nodeName, *dir)
		}
		m.Nodes = []osv1alpha1.TalosNode{*n}
	}
	configs, err := renderAll(context.Background(), m, bundle, render.NewFactory(*factoryHost), false)
	if err != nil {
		log.Fatal(err)
	}

	var nodes []bootstrap.Node
	var endpoints []string
	for _, n := range m.Nodes {
		addr := render.NodeAddress(&n)
		if *endpoint != "" {
			addr = *endpoint
		}
		if addr == "" {
			log.Fatalf("bootstrap: %s has no address to dial; set spec.address", n.Name)
		}
		cp := n.Spec.Role == osv1alpha1.RoleControlPlane
		if cp {
			endpoints = append(endpoints, addr)
		}
		nodes = append(nodes, bootstrap.Node{Name: n.Name, Endpoint: addr, ControlPlane: cp, MachineConfig: configs[n.Name]})
	}
	talosconfig, err := render.Talosconfig(bundle, m.Cluster.Name, endpoints, bootstrapCertTTL)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := bootstrap.Run(ctx, bootstrap.Options{
		Dial:          machineapi.Dial,
		Nodes:         nodes,
		Talosconfig:   talosconfig,
		ApplyInsecure: *applyInsecure,
		Logf:          log.Printf,
	}); err != nil {
		log.Fatalf("bootstrap: FAIL: %v", err)
	}
	log.Printf("bootstrap: OK: %s is bootstrapped and its nodes are ready", m.Cluster.Name)
}

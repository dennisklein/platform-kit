package main

import (
	"context"
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
// the TalosCluster reconciler runs: bootstrap etcd exactly once, wait for every
// node to be Ready. It sends no config: the config reaches a machine only out
// of band, on the BMC's virtual media (see boot), never over the
// unauthenticated maintenance API.
func bootstrapCmd(args []string) {
	fs := newFlags("bootstrap")
	dir := fs.String("dir", "", "directory with TalosCluster/TalosNode manifests (required)")
	secretsFile := fs.String("secrets", "", "secrets bundle from `talosctl gen secrets`, or - for stdin (required)")
	nodeName := fs.String("node", "", "drive only this TalosNode (default: every node in --dir)")
	endpoint := fs.String("endpoint", "", "address of --node (default: its spec.address or first static address)")
	timeout := fs.Duration("timeout", 15*time.Minute, "overall deadline: bootstrap retries, readiness")
	parseFlags(fs, args)
	if fs.NArg() != 0 || *dir == "" || *secretsFile == "" || (*endpoint != "" && *nodeName == "") {
		badUsage(fs, "--dir and --secrets are required, --endpoint needs --node, and bootstrap takes no other arguments")
	}

	m, bundle := loadInputs(*dir, *secretsFile)
	if *nodeName != "" {
		n := m.Node(*nodeName)
		if n == nil {
			dief("bootstrap: %s names no TalosNode in %s", *nodeName, *dir)
		}
		m.Nodes = []osv1alpha1.TalosNode{*n}
	}
	var nodes []bootstrap.Node
	var endpoints []string
	for _, n := range m.Nodes {
		addr := render.NodeAddress(&n)
		if *endpoint != "" {
			addr = *endpoint
		}
		if addr == "" {
			dief("bootstrap: %s has no address to dial; set spec.address", n.Name)
		}
		cp := n.Spec.Role == osv1alpha1.RoleControlPlane
		if cp {
			endpoints = append(endpoints, addr)
		}
		nodes = append(nodes, bootstrap.Node{Name: n.Name, Endpoint: addr, ControlPlane: cp})
	}
	talosconfig, err := render.Talosconfig(bundle, m.Cluster.Name, endpoints, bootstrapCertTTL)
	if err != nil {
		die(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := bootstrap.Run(ctx, bootstrap.Options{
		Dial:        machineapi.Dial,
		Nodes:       nodes,
		Talosconfig: talosconfig,
		Logf:        log.Printf,
	}); err != nil {
		dief("bootstrap failed: %v", err)
	}
	log.Printf("bootstrap: OK: %s is bootstrapped and its nodes are ready", m.Cluster.Name)
}

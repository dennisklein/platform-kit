// Package talos is the OS seam: reconcilers and the CLI depend on Client, never
// on the machinery gRPC plumbing. Production dials machineapi; envtest scripts
// the fake, the same discipline as the bmc seam.
//
// The maintenance service is deliberately narrow here: it binds :50000 with no
// client authentication, so on real metal the config rides the BMC's media
// channel and the insecure dial exists only for the container tier (no media)
// and day-0 liveness probing. Callers must name that choice loudly (Target.
// Insecure; the CLI's --apply-insecure).
package talos

import (
	"context"
	"errors"
)

// ErrAlreadyBootstrapped is what Bootstrap returns when the node's etcd data
// directory is not empty: the cluster was bootstrapped before. Callers treat
// it as success, which is what makes bootstrapping idempotent.
var ErrAlreadyBootstrapped = errors.New("etcd is already bootstrapped")

// Client speaks the Talos machine API to one node.
type Client interface {
	// ApplyConfiguration submits a machine config in AUTO mode: the container
	// platform applies without a reboot, metal reboots into the config.
	ApplyConfiguration(ctx context.Context, machineConfig []byte) error
	// Bootstrap starts etcd, once per cluster, on one control-plane node. It
	// returns an error wrapping ErrAlreadyBootstrapped when etcd already has
	// data on this node.
	Bootstrap(ctx context.Context) error
	// EtcdMembers lists the etcd members this node's etcd knows, by
	// hostname. It fails while etcd is not running here, which before
	// bootstrap is always.
	EtcdMembers(ctx context.Context) ([]string, error)
	// Version reports the server's version tag. It is served in maintenance
	// mode too, the cheapest liveness probe for a blank machine.
	Version(ctx context.Context) (string, error)
	// Status reports the aggregated machine stage (runtime.MachineStatus),
	// the check-in observable: Stage "running" + Ready is what revokes grants
	// and advances a Host to Provisioned.
	Status(ctx context.Context) (Status, error)
	Close() error
}

// Status is the machine's aggregated state.
type Status struct {
	Stage string // unknown | booting | installing | maintenance | running | ...
	Ready bool
	// UnmetConditions name what still blocks readiness at this stage.
	UnmetConditions []string
}

// Target says where and how to dial one node.
type Target struct {
	// Endpoint is the node address (host or host:port; :50000 when bare).
	Endpoint string
	// Insecure dials the maintenance service: encrypted, unauthenticated.
	// Mutually exclusive with Talosconfig.
	Insecure bool
	// Talosconfig is a rendered client config (CA + client cert) for the
	// configured machine.
	Talosconfig []byte
}

// Dialer opens a Client for a target. Reconcilers and the CLI take one, so
// tests can hand them the fake.
type Dialer func(ctx context.Context, t Target) (Client, error)

// Package pivot moves the pk-metal operator from one cluster to another, the
// way clusterctl move does for Cluster API: the operator that built a Talos
// cluster from a bootstrap cluster (kind) moves onto that Talos cluster and
// goes on from there.
//
// A move is a sequence of steps, each safe to run again:
//
//  1. Preflight: nothing runs or waits on the source, and the target runs no
//     manager of its own.
//  2. Pause: the source manager is scaled to zero, and the move waits until
//     no manager pod runs and the leader lease is free.
//  3. Install: the operator's own objects (CRDs, admission policies, RBAC,
//     namespace, Deployment at zero replicas) are copied from the source.
//  4. Copy: the operator's state is copied in dependency order, status
//     included, with every UID reference rewritten to the target's UIDs.
//  5. Start: the target manager is scaled up.
//  6. Verify: the new manager settles without proposing anything new.
//
// The source is left paused, not deleted. Moving back is the same move in the
// other direction.
package pivot

import (
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Markers on the operator namespace. They let a rerun continue a move, and a
// move back find its way, without a file on the workstation.
const (
	// AnnotationSource on the target's operator namespace names the cluster
	// (its kube-system UID) the operator was moved from.
	AnnotationSource = "metal.pk.io/pivot-source"
	// AnnotationMovedTo on the source's operator namespace names the cluster
	// the operator was moved to. A source carrying it is paused on purpose.
	AnnotationMovedTo = "metal.pk.io/pivoted-to"
	// AnnotationReplicas on the source's operator namespace keeps the
	// manager's replica count from before the pause.
	AnnotationReplicas = "metal.pk.io/pivot-replicas"
)

// FieldManager is the field manager the move applies the operator's own
// objects with. Status written for the controllers uses their field managers.
const FieldManager = "pk-metal-pivot"

// leaseName is the manager's leader election lease (cmd/manager).
const leaseName = "pk-metal.metal.pk.io"

// Cluster is one side of a move.
type Cluster struct {
	// Name says which cluster this is in messages: a kubeconfig context or
	// file name.
	Name   string
	Config *rest.Config
	Client client.Client
	// impersonate builds a client acting as another user; a test seam.
	impersonate func(user string, groups []string) (client.Client, error)
}

// NewCluster connects to one side of a move.
func NewCluster(name string, cfg *rest.Config, scheme *runtime.Scheme) (*Cluster, error) {
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("cannot connect to %s at %s: %w", name, cfg.Host, err)
	}
	c := &Cluster{Name: name, Config: cfg, Client: cl}
	c.impersonate = func(user string, groups []string) (client.Client, error) {
		as := rest.CopyConfig(cfg)
		as.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: groups}
		return client.New(as, client.Options{Scheme: scheme})
	}
	return c, nil
}

// as is a client of this cluster acting as user, in groups. The move replays
// who proposed and who approved each operation, which the admission policy
// checks against the request's user; that needs a kubeconfig that may
// impersonate (cluster-admin).
func (c *Cluster) as(user string, groups ...string) (client.Client, error) {
	return c.impersonate(user, groups)
}

// Options are the settings of a move.
type Options struct {
	// Namespace is the operator's namespace (pk-metal).
	Namespace string
	// Deployment is the manager's Deployment (pk-metal-manager).
	Deployment string
	// Image replaces the manager's image on the target; empty keeps the
	// source's.
	Image string
	// MediaURL replaces the manager's --media-url on the target; empty keeps
	// the source's.
	MediaURL string
	// ForceWaiting moves operations that still wait for approval; they wait
	// on the target instead.
	ForceWaiting bool
	// Timeout bounds every wait: the pause, the CRDs, the new manager.
	Timeout time.Duration
	// Settle is how long the new manager is watched for proposals it
	// should not make.
	Settle time.Duration
	// Poll is how often a wait looks again.
	Poll time.Duration
	// Log receives one line per step.
	Log io.Writer
}

func (o *Options) defaults() {
	if o.Namespace == "" {
		o.Namespace = "pk-metal"
	}
	if o.Deployment == "" {
		o.Deployment = "pk-metal-manager"
	}
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Minute
	}
	if o.Settle == 0 {
		o.Settle = 90 * time.Second
	}
	if o.Poll == 0 {
		o.Poll = 2 * time.Second
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
}

// Mover moves the operator from Source to Target.
type Mover struct {
	Source, Target *Cluster
	Options        Options

	sourceID, targetID string
}

// New prepares a move and tells the two clusters apart by the UID of their
// kube-system namespace, which no two clusters share.
func New(ctx context.Context, source, target *Cluster, o Options) (*Mover, error) {
	o.defaults()
	m := &Mover{Source: source, Target: target, Options: o}
	var err error
	if m.sourceID, err = clusterID(ctx, source); err != nil {
		return nil, err
	}
	if m.targetID, err = clusterID(ctx, target); err != nil {
		return nil, err
	}
	if m.sourceID == m.targetID {
		return nil, fmt.Errorf("%s and %s are the same cluster (kube-system has UID %s in both): name another cluster with --to",
			source.Name, target.Name, m.sourceID)
	}
	return m, nil
}

func clusterID(ctx context.Context, c *Cluster) (string, error) {
	ns := &corev1.Namespace{}
	if err := c.Client.Get(ctx, client.ObjectKey{Name: "kube-system"}, ns); err != nil {
		return "", fmt.Errorf("cannot read namespace kube-system on %s: %w", c.Name, err)
	}
	return string(ns.UID), nil
}

func (m *Mover) logf(format string, args ...any) {
	fmt.Fprintf(m.Options.Log, format+"\n", args...)
}

// wait polls cond until it reports done, an error, or the timeout passes.
// what names the wait in the timeout error.
func (m *Mover) wait(ctx context.Context, what string, cond func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, m.Options.Timeout)
	defer cancel()
	t := time.NewTicker(m.Options.Poll)
	defer t.Stop()
	for {
		done, err := cond(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up after %s waiting for %s", m.Options.Timeout, what)
		case <-t.C:
		}
	}
}

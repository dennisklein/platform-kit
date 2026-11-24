// Package bootstrap brings a Talos cluster's etcd into existence, once. Ensure
// is one observation and at most one Bootstrap call: the TalosCluster
// reconciler runs it on every pass, and Run (the CLI's day-0 ladder, for a
// cluster without an operator) loops it between delivering configs and waiting
// for the nodes to become Ready.
//
// Whether a cluster is bootstrapped is observed, never remembered: a
// control-plane member that lists etcd members, or a Kubernetes API that
// answers, proves it. The dangerous mistake is the opposite one, bootstrapping
// a second etcd beside a first that merely did not answer, so Ensure calls
// Bootstrap only when every control-plane member answers its authenticated API
// and none of them runs etcd, and then only on the first member by name. Talos
// itself refuses a second bootstrap on a node with etcd data (AlreadyExists),
// which Ensure reads as "bootstrapped".
//
// Whether a cluster WAS bootstrapped is the one fact that must be remembered.
// After every control plane is reinstalled or reset, all of them answer and
// none runs etcd, which is exactly what a cluster that was never bootstrapped
// looks like; bootstrapping it then starts an empty etcd and throws away the
// chance to restore the old one. So the caller passes when the cluster was
// first observed bootstrapped (Observation.BootstrappedAt, kept by the
// TalosCluster reconciler in status.bootstrappedAt), and once that is set
// Ensure never calls Bootstrap: it reports EtcdDataLost and points at the
// restore procedure. And even a first bootstrap is refused while any member
// runs an etcd that lists no members (EtcdDataPresent): that etcd holds data
// of a cluster that lost its quorum, and a bootstrap would start a second one
// beside it.
//
// The one thing a caller does carry between rounds is when a request was
// accepted. etcd starts some time after the call returns, and in that time an
// observer that asks etcd sees exactly what it saw before the call; without
// the request time every round would ask again. The TalosCluster reconciler
// keeps it as the transition time of Bootstrapped=Unknown/BootstrapRequested,
// so it survives a restart; Run keeps it in its loop.
package bootstrap

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/poll"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// Member is one control-plane node to observe.
type Member struct {
	Name     string
	Endpoint string
}

// Result is what one Ensure round established. Reason is one of the
// os.pk.io Bootstrapped reasons.
type Result struct {
	Bootstrapped bool
	Reason       string
	Message      string
	// RequestedAt is when the outstanding bootstrap request was accepted:
	// the Observation's value carried forward, or the moment this round's
	// call was accepted. Zero when no request is outstanding.
	RequestedAt time.Time
}

// RequestGrace is how long an accepted bootstrap request is given to show up
// as a running etcd before Ensure may ask again. etcd takes seconds to tens of
// seconds to start after the call is accepted (longer while its image is
// pulled), and until then nothing distinguishes "starting" from "never asked"
// to an observer that asks etcd. The window is generous because a repeated
// request is the expensive mistake and a slow one only delays.
const RequestGrace = 10 * time.Minute

// Observation is what the caller knows beyond the machines themselves.
type Observation struct {
	// KubernetesAnswers counts a Kubernetes API that answered as proof of a
	// bootstrapped cluster.
	KubernetesAnswers bool
	// RequestedAt is when a bootstrap request was last accepted, as the
	// caller recorded it from an earlier Result; zero if never.
	RequestedAt time.Time
	// BootstrappedAt is when the cluster was first observed bootstrapped,
	// as the caller recorded it; zero if never. Once set, Ensure only
	// observes: it never bootstraps the cluster again.
	BootstrappedAt time.Time
	// RestorePending says the cluster's etcd is to be restored from a
	// snapshot (os.pk.io/restore-pending): Ensure never bootstraps it,
	// even though it was never seen bootstrapped, and waits for the
	// approved recovery to start etcd.
	RestorePending bool
	// Now defaults to time.Now; Grace to RequestGrace.
	Now   time.Time
	Grace time.Duration
}

// Ensure observes the control-plane members and bootstraps etcd when, and only
// when, it is safe to. Errors are observations (in the Result), never
// returned: an unreachable machine is a state the next round re-observes.
//
// A request accepted less than the grace window ago is not repeated: the
// result stays BootstrapRequested, carrying the request time, until etcd is
// observed or the window passes. Only then, if the first member still runs no
// etcd, is the request sent again; Talos answers AlreadyExists if etcd got as
// far as writing data, which reads as bootstrapped.
func Ensure(ctx context.Context, dial talos.Dialer, talosconfig []byte, members []Member, obs Observation) Result {
	now := obs.Now
	if now.IsZero() {
		now = time.Now()
	}
	grace := cmp.Or(obs.Grace, RequestGrace)
	if len(members) == 0 {
		return Result{Reason: osv1alpha1.ReasonNoControlPlane, Message: "no control-plane member"}
	}
	members = slices.Clone(members)
	slices.SortFunc(members, func(a, b Member) int { return strings.Compare(a.Name, b.Name) })

	var silent, holders []string
	for _, m := range members {
		etcd, err := etcdMembers(ctx, dial, talosconfig, m.Endpoint)
		switch {
		case err == nil && len(etcd) > 0:
			return Result{Bootstrapped: true, Reason: osv1alpha1.ReasonEtcdRunning,
				Message: fmt.Sprintf("%s lists etcd members %s", m.Name, strings.Join(etcd, ", "))}
		case errors.Is(err, errUnreachable):
			silent = append(silent, m.Name)
		case errors.Is(err, errEtcdWithoutMembers):
			holders = append(holders, m.Name)
		}
	}
	if obs.KubernetesAnswers {
		return Result{Bootstrapped: true, Reason: osv1alpha1.ReasonKubernetesAnswers,
			Message: "the Kubernetes API answers"}
	}
	if !obs.BootstrappedAt.IsZero() {
		return lost(obs.BootstrappedAt, silent, holders)
	}
	if obs.RestorePending {
		return Result{Reason: osv1alpha1.ReasonRestorePending, Message: fmt.Sprintf(
			"the cluster is marked %s: the operator does not bootstrap it, an approved etcdRecover operation "+
				"restores its etcd from a snapshot (%s); remove the annotation to start an empty cluster instead",
			osv1alpha1.AnnotationRestorePending, RestoreProcedure)}
	}
	if !obs.RequestedAt.IsZero() && now.Sub(obs.RequestedAt) < grace {
		msg := fmt.Sprintf("bootstrap requested at %s; waiting for etcd on %s",
			obs.RequestedAt.UTC().Format(time.RFC3339), members[0].Name)
		if len(silent) > 0 {
			msg += "; not answering: " + strings.Join(silent, ", ")
		}
		return Result{Reason: osv1alpha1.ReasonBootstrapRequested, Message: msg, RequestedAt: obs.RequestedAt}
	}
	if len(holders) > 0 {
		return Result{Reason: osv1alpha1.ReasonEtcdDataPresent, Message: fmt.Sprintf(
			"%s run etcd but list no members: they hold the data of an etcd that lost its quorum, and a bootstrap "+
				"would start a second, empty etcd beside it. Restore that etcd (%s), or reset the EPHEMERAL "+
				"partition of those members if the data is not wanted", strings.Join(holders, ", "), RestoreProcedure)}
	}
	if len(silent) > 0 {
		return Result{Reason: osv1alpha1.ReasonAwaitingControlPlane,
			Message: "waiting for every control-plane member to answer before bootstrapping: " + strings.Join(silent, ", ")}
	}

	first := members[0]
	c, err := dial(ctx, talos.Target{Endpoint: first.Endpoint, Talosconfig: talosconfig})
	if err != nil {
		return Result{Reason: osv1alpha1.ReasonAwaitingControlPlane, Message: err.Error()}
	}
	defer func() { _ = c.Close() }()
	switch err := c.Bootstrap(ctx, talos.BootstrapOptions{}); {
	case errors.Is(err, talos.ErrAlreadyBootstrapped):
		return Result{Bootstrapped: true, Reason: osv1alpha1.ReasonEtcdRunning,
			Message: first.Name + " already holds etcd data"}
	case err != nil:
		return Result{Reason: osv1alpha1.ReasonBootstrapRefused, Message: fmt.Sprintf("%s: %v", first.Name, err)}
	}
	return Result{Reason: osv1alpha1.ReasonBootstrapRequested, Message: "bootstrap accepted by " + first.Name,
		RequestedAt: now}
}

// RestoreProcedure is where a person reads how to restore etcd; every
// refusal to bootstrap names it.
const RestoreProcedure = "https://dennisklein.github.io/platform-kit/operate/day-2/restore-etcd/"

// lost is the result for a cluster that was bootstrapped before and shows no
// etcd now. It never asks for a bootstrap: with every member answering and
// none running etcd the data is gone, and only a person can decide between
// a restore and starting over.
func lost(at time.Time, silent, holders []string) Result {
	since := at.UTC().Format(time.RFC3339)
	if len(silent) == 0 && len(holders) == 0 {
		return Result{Reason: osv1alpha1.ReasonEtcdDataLost, Message: fmt.Sprintf(
			"this cluster was bootstrapped at %s, and now every control plane answers and none runs etcd: its "+
				"etcd data is gone. The operator does not bootstrap it again, which would start an empty cluster. "+
				"Restore etcd from a snapshot (%s); to start over with an empty cluster instead, delete and "+
				"re-create the TalosCluster", since, RestoreProcedure)}
	}
	msg := fmt.Sprintf("this cluster was bootstrapped at %s and no member lists etcd members now", since)
	if len(holders) > 0 {
		msg += "; etcd runs without a member list (quorum lost?) on " + strings.Join(holders, ", ")
	}
	if len(silent) > 0 {
		msg += "; not answering: " + strings.Join(silent, ", ")
	}
	return Result{Reason: osv1alpha1.ReasonEtcdUnavailable,
		Message: msg + ". The operator never bootstraps it again; see " + RestoreProcedure + " if it does not come back"}
}

var (
	errUnreachable = errors.New("authenticated API does not answer")
	// errEtcdWithoutMembers: etcd runs (its status answers) but lists no
	// members, which is how a member whose cluster lost quorum answers.
	errEtcdWithoutMembers = errors.New("etcd runs but lists no members")
)

// etcdMembers asks one member for its etcd members. A dial or version failure
// is errUnreachable; an etcd whose status answers while its member list does
// not is errEtcdWithoutMembers; an etcd that is not running is another error.
func etcdMembers(ctx context.Context, dial talos.Dialer, talosconfig []byte, endpoint string) ([]string, error) {
	var (
		members []string
		etcdErr error
	)
	if err := talos.With(ctx, dial, talos.Target{Endpoint: endpoint, Talosconfig: talosconfig}, func(c talos.Client) error {
		if _, err := c.Version(ctx); err != nil {
			return err
		}
		var listed []talos.EtcdMember
		listed, etcdErr = c.EtcdMembers(ctx)
		for _, m := range listed {
			members = append(members, m.Hostname)
		}
		if etcdErr != nil {
			// The status is served by the member alone: it answers where
			// the member list, which needs the cluster, does not.
			if _, err := c.EtcdStatus(ctx); err == nil {
				etcdErr = fmt.Errorf("%w: %w", errEtcdWithoutMembers, etcdErr)
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("%w: %w", errUnreachable, err)
	}
	return members, etcdErr
}

// Node is one node the day-0 ladder drives.
type Node struct {
	Name         string
	Endpoint     string
	ControlPlane bool
	// MachineConfig is the node's rendered config; consumed only with
	// ApplyInsecure (on metal it already rode the media).
	MachineConfig []byte
}

// Options configure Run.
type Options struct {
	Dial  talos.Dialer
	Nodes []Node
	// Talosconfig is the cluster's client credentials for the authenticated
	// rungs.
	Talosconfig []byte
	// ApplyInsecure delivers each node's MachineConfig over the maintenance
	// service, for tiers without a media channel.
	ApplyInsecure bool
	// Poll is the probe cadence between rounds (default 2s). The overall
	// deadline is the caller's ctx.
	Poll time.Duration
	// Grace overrides RequestGrace (tests).
	Grace time.Duration
	Logf  func(format string, args ...any)
}

// Run climbs: [apply over maintenance] → etcd bootstrapped (Ensure, repeated)
// → every node running and ready. Re-runs are safe: a configured node is not
// re-applied, and a bootstrapped cluster is not bootstrapped again.
func Run(ctx context.Context, o Options) error {
	poll := cmp.Or(o.Poll, 2*time.Second)
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	if o.ApplyInsecure {
		for _, n := range o.Nodes {
			if err := o.applyInsecure(ctx, n, poll, logf); err != nil {
				return err
			}
		}
	}

	var members []Member
	for _, n := range o.Nodes {
		if n.ControlPlane {
			members = append(members, Member{Name: n.Name, Endpoint: n.Endpoint})
		}
	}
	// The request time lives only in this loop: a re-run of the CLI starts
	// without it, which is what an operator re-running day-0 by hand means.
	var last Result
	if err := wait(ctx, poll, func() error {
		r := Ensure(ctx, o.Dial, o.Talosconfig, members, Observation{RequestedAt: last.RequestedAt, Grace: o.Grace})
		if r.Reason != last.Reason || r.Message != last.Message {
			logf("bootstrap: %s: %s", r.Reason, r.Message)
		}
		last = r
		if !r.Bootstrapped {
			return errors.New(r.Message)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("bootstrap: etcd never came up: %w", err)
	}

	for _, n := range o.Nodes {
		if err := wait(ctx, poll, func() error {
			st, err := status(ctx, o.Dial, o.Talosconfig, n.Endpoint)
			if err != nil {
				return err
			}
			if st.Stage != "running" || !st.Ready {
				return fmt.Errorf("stage %q, unmet %v", st.Stage, st.UnmetConditions)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("bootstrap: %s never became ready: %w", n.Name, err)
		}
		logf("bootstrap: %s running and ready", n.Name)
	}
	return nil
}

func status(ctx context.Context, dial talos.Dialer, talosconfig []byte, endpoint string) (st talos.Status, err error) {
	err = talos.With(ctx, dial, talos.Target{Endpoint: endpoint, Talosconfig: talosconfig}, func(c talos.Client) error {
		st, err = c.Status(ctx)
		return err
	})
	return st, err
}

// applyInsecure delivers a node's config over the maintenance service, unless
// the node already answers its authenticated API (it is configured, and the
// maintenance service is gone).
func (o *Options) applyInsecure(ctx context.Context, n Node, poll time.Duration, logf func(string, ...any)) error {
	if _, err := status(ctx, o.Dial, o.Talosconfig, n.Endpoint); err == nil {
		logf("bootstrap: %s is already configured", n.Name)
		return nil
	}
	if len(n.MachineConfig) == 0 {
		return fmt.Errorf("bootstrap: apply-insecure without a machine config for %s", n.Name)
	}
	if err := wait(ctx, poll, func() error {
		return talos.With(ctx, o.Dial, talos.Target{Endpoint: n.Endpoint, Insecure: true}, func(c talos.Client) error {
			if _, err := c.Version(ctx); err != nil {
				return err
			}
			_, err := c.ApplyConfiguration(ctx, n.MachineConfig, talos.ApplyOptions{})
			return err
		})
	}); err != nil {
		return fmt.Errorf("bootstrap: maintenance service of %s never took the config: %w", n.Name, err)
	}
	logf("bootstrap: config applied to %s over the maintenance service", n.Name)
	return nil
}

// wait retries fn every poll until it succeeds or ctx ends; the last error
// wins the report.
func wait(ctx context.Context, every time.Duration, fn func() error) error {
	var last error
	if err := poll.Until(ctx, poll.Every(every), func(context.Context) (bool, error) {
		last = fn()
		return last == nil, nil
	}); err != nil {
		return fmt.Errorf("%w (last: %w)", err, last)
	}
	return nil
}

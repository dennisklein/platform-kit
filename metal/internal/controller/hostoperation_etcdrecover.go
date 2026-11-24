package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// The etcdRecover step: restore a cluster that lost all of its etcd from one
// snapshot, on one control plane. It is the only path by which a cluster
// that status.bootstrappedAt (or os.pk.io/restore-pending) keeps from being
// bootstrapped gets etcd again; the TalosCluster reconciler only observes
// the result and turns Bootstrapped True.
//
// The operator cannot read the snapshot (it is encrypted to people), so a
// person uploads it after approval, with pk-metal restore upload, which
// decrypts on their machine, checks the plaintext against the EtcdSnapshot
// record and the approved sha256, streams it to the node with their own
// talosconfig, and then sets metal.pk.io/etcd-snapshot-uploaded on the
// operation. Talos refuses an upload to a node whose etcd runs.
//
// The step re-derives where it stands from the world on every pass:
//
//   - the node lists etcd members and the upload is recorded: done (the
//     recovery this step asked for, or one it asked for before a restart);
//   - the cluster was never bootstrapped and is not marked
//     restore-pending, the snapshot record changed, or the node is not a
//     control plane of the snapshot's cluster: fail, nothing was done;
//   - a control plane does not answer, or runs etcd: wait (a person is
//     still resetting the others; the budget bounds the wait);
//   - no upload recorded: wait for it;
//   - otherwise ask Talos to bootstrap from the upload, and wait for etcd.

// +kubebuilder:rbac:groups=os.pk.io,resources=talosclusters;talosnodes;etcdsnapshots,verbs=get;list;watch

// defaultEtcdRecoverTimeout mirrors the schema default of etcdRecover.
const defaultEtcdRecoverTimeout = 2 * time.Hour

// recoverRequestGrace is how long after Talos accepted a recovery the step
// waits for etcd before it asks again. A repeated request while the first
// is restoring the snapshot is answered with AlreadyExists, or with a
// sequence error that the step waits out too.
const recoverRequestGrace = 2 * time.Minute

func etcdRecoverTimeout(a *metalv1alpha1.EtcdRecoverAction) time.Duration {
	if a.TimeoutSeconds > 0 {
		return time.Duration(a.TimeoutSeconds) * time.Second
	}
	return defaultEtcdRecoverTimeout
}

// etcdState is what one control plane said about its etcd.
type etcdState int

const (
	etcdSilent    etcdState = iota // the authenticated API did not answer
	etcdStopped                    // it answered; etcd does not run
	etcdNoQuorum                   // etcd runs but cannot list members
	etcdListening                  // etcd runs and lists members
)

// probeEtcd asks one control plane about its etcd. Each call has its own
// probeTimeout: a node whose etcd does not run can hold an etcd call until
// its deadline, and with one budget for all three the version call came too
// late as well, so a node that only lacked etcd counted as silent and the
// recovery waited for it for ever. Whether the node answers comes first.
func probeEtcd(ctx context.Context, dial talos.Dialer, talosconfig []byte, endpoint string) etcdState {
	call := func(fn func(ctx context.Context) error) error {
		cctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		return fn(cctx)
	}
	state := etcdSilent
	_ = talos.With(ctx, dial, talos.Target{Endpoint: endpoint, Talosconfig: talosconfig}, func(c talos.Client) error {
		// Only a machine that answers something counts as more than silent,
		// not one whose connection broke after the dial.
		if call(func(ctx context.Context) error { _, err := c.Version(ctx); return err }) != nil {
			return nil
		}
		state = etcdStopped
		if call(func(ctx context.Context) error {
			ms, err := c.EtcdMembers(ctx)
			if err == nil && len(ms) == 0 {
				err = errors.New("no members")
			}
			return err
		}) == nil {
			state = etcdListening
			return nil
		}
		if call(func(ctx context.Context) error { _, err := c.EtcdStatus(ctx); return err }) == nil {
			state = etcdNoQuorum
		}
		return nil
	})
	return state
}

func (x *exec) etcdRecover(ctx context.Context, a *metalv1alpha1.EtcdRecoverAction) (stepState, error) {
	r := x.r
	if r.TalosDial == nil {
		return stepState{}, errors.New("this operator has no Talos client wired, so it cannot run etcdRecover")
	}
	if hos := x.host.Spec.OS; hos == nil || hos.NodeRef.Name != a.NodeRef.Name {
		return stepState{}, fmt.Errorf("host %s does not run TalosNode %s (its spec.os names another node or none)",
			x.host.Name, a.NodeRef.Name)
	}
	node := &osv1alpha1.TalosNode{}
	if err := r.Get(ctx, types.NamespacedName{Name: a.NodeRef.Name}, node); err != nil {
		return stepState{}, fmt.Errorf("TalosNode %s: %w", a.NodeRef.Name, err)
	}
	if node.Spec.Role != osv1alpha1.RoleControlPlane {
		return stepState{}, fmt.Errorf("TalosNode %s is a %s; etcd is restored on a control plane", node.Name, node.Spec.Role)
	}
	cluster := &osv1alpha1.TalosCluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: node.Spec.ClusterRef.Name}, cluster); err != nil {
		return stepState{}, fmt.Errorf("TalosCluster %s: %w", node.Spec.ClusterRef.Name, err)
	}
	if cluster.Status.BootstrappedAt == nil && cluster.Annotations[osv1alpha1.AnnotationRestorePending] == "" {
		return stepState{}, fmt.Errorf("cluster %s was never seen bootstrapped and is not marked %s: there is no lost "+
			"etcd to restore, and the operator would bootstrap it on its own meanwhile", cluster.Name,
			osv1alpha1.AnnotationRestorePending)
	}

	snap := &osv1alpha1.EtcdSnapshot{}
	if err := r.Get(ctx, types.NamespacedName{Name: a.SnapshotRef.Name}, snap); err != nil {
		if apierrors.IsNotFound(err) {
			return stepState{}, fmt.Errorf("EtcdSnapshot %s not found", a.SnapshotRef.Name)
		}
		return stepState{}, err
	}
	switch {
	case snap.Status.SHA256 != a.SHA256:
		return stepState{}, fmt.Errorf("EtcdSnapshot %s records sha256 %q, but %s was approved", snap.Name,
			snap.Status.SHA256, a.SHA256)
	case snap.Status.ClusterName != cluster.Name:
		return stepState{}, fmt.Errorf("EtcdSnapshot %s is of cluster %q, not of %s", snap.Name,
			snap.Status.ClusterName, cluster.Name)
	case !meta.IsStatusConditionTrue(snap.Status.Conditions, osv1alpha1.ConditionVerified):
		return stepState{}, fmt.Errorf("EtcdSnapshot %s is not verified", snap.Name)
	}

	bundle, err := loadBundle(ctx, r.Client, r.CredentialsNamespace, cluster)
	if unavailable := (*bundleUnavailable)(nil); errors.As(err, &unavailable) {
		return stepState{}, errors.New(unavailable.msg)
	} else if err != nil {
		return stepState{}, err
	}
	members, err := clusterMembers(ctx, r.Client, cluster.Name)
	if err != nil {
		return stepState{}, err
	}
	var endpoints []string
	cps := map[string]string{}
	for _, n := range members {
		if addr := render.NodeAddress(&n); n.Spec.Role == osv1alpha1.RoleControlPlane && addr != "" {
			endpoints = append(endpoints, addr)
			cps[n.Name] = addr
		}
	}
	target := cps[node.Name]
	if target == "" {
		return stepState{}, fmt.Errorf("TalosNode %s has no address to dial", node.Name)
	}
	talosconfig, err := render.Talosconfig(bundle, cluster.Name, endpoints, checkinCertTTL)
	if err != nil {
		return stepState{}, fmt.Errorf("mint talosconfig for %s: %w", cluster.Name, err)
	}

	uploaded := x.op.Annotations[metalv1alpha1.AnnotationEtcdSnapshotUploaded] == a.SHA256
	if uploaded && probeEtcd(ctx, r.TalosDial, talosconfig, target) == etcdListening {
		r.recoveries.Delete(x.op.UID)
		return stepDone, nil
	}

	// Every control plane must answer, and none may run etcd: a restored
	// etcd beside a running one is two clusters, and a member that did not
	// answer may still hold data.
	var silent, running []string
	for _, name := range slices.Sorted(maps.Keys(cps)) {
		switch probeEtcd(ctx, r.TalosDial, talosconfig, cps[name]) {
		case etcdSilent:
			silent = append(silent, name)
		case etcdNoQuorum, etcdListening:
			running = append(running, name)
		}
	}
	if len(running) > 0 {
		return waitingOn("etcd still runs on %s: wipe their EPHEMERAL partition first (the restore plan's talosReset "+
			"operations), so that the restored etcd is the only one", strings.Join(running, ", ")), nil
	}
	if len(silent) > 0 {
		return waitingOn("waiting for every control plane to answer before restoring: %s does not (a control plane "+
			"that is gone for good is removed from the cluster first, see https://dennisklein.github.io/platform-kit/operate/day-2/restore-etcd/)",
			strings.Join(silent, ", ")), nil
	}

	if !uploaded {
		return waitingOn("waiting for the snapshot upload: run pk-metal restore upload %s/%s --identity <age identity file>",
			x.op.Namespace, x.op.Name), nil
	}
	if at, ok := r.recoveries.Load(x.op.UID); ok && time.Since(at.(time.Time)) < recoverRequestGrace {
		return waitingOn("Talos accepted the recovery at %s; waiting for etcd to start on %s from the snapshot",
			at.(time.Time).UTC().Format(time.RFC3339), node.Name), nil
	}
	err = talos.With(ctx, r.TalosDial, talos.Target{Endpoint: target, Talosconfig: talosconfig}, func(c talos.Client) error {
		return c.Bootstrap(ctx, talos.BootstrapOptions{RecoverEtcd: true})
	})
	switch {
	case errors.Is(err, talos.ErrAlreadyBootstrapped):
		return waitingOn("%s holds etcd data from the recovery; waiting for etcd to start", node.Name), nil
	case err != nil:
		// Not final: a node still busy with the upload or a reboot refuses
		// too. The budget turns a lasting refusal into a failure.
		return waitingOn("Talos on %s refused the recovery (retrying): %v", node.Name, err), nil
	}
	r.recoveries.Store(x.op.UID, time.Now())
	return waitingOn("Talos on %s accepted the recovery from %s; waiting for etcd to start", node.Name, snap.Name), nil
}

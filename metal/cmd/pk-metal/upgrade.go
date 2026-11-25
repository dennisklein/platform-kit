package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

func init() {
	verbHelp["upgrade"] = `
upgrade changes the version a TalosCluster asks for, then waits for the
rollout the operator proposes for it and prints its plan. Nothing on a machine
changes until a person approves that plan with pk-metal rollout approve.

  talos <cluster> <version>        for example v1.14.7: each node is drained,
                                   upgraded with the installer for that version
                                   and rebooted, control planes first
  kubernetes <cluster> <version>   for example v1.36.3: each node gets the new
                                   kubelet and control-plane images, then
                                   kube-proxy follows

Talos says which Kubernetes versions each Talos release supports and from
which releases it can be upgraded; upgrade refuses a step Talos does not
support, before it changes anything. Kubernetes moves one minor at a time:
from v1.35 the next step is v1.36, not v1.37.

examples:
  pk-metal upgrade talos lab v1.14.7
  pk-metal upgrade kubernetes lab v1.36.3`
}

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

func upgradeCmd(args []string) {
	fs := newFlags("upgrade")
	wait := fs.Duration("wait", 2*time.Minute, "how long to wait for the proposed rollout (0: do not wait)")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 3 {
		badUsage(fs, "give what to upgrade (talos or kubernetes), the cluster and the version")
	}
	what, cluster, version := fs.Arg(0), fs.Arg(1), fs.Arg(2)
	if what != "talos" && what != "kubernetes" {
		badUsage(fs, "upgrade talos or kubernetes, not "+what)
	}
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	ctx := context.Background()
	if err := upgrade(ctx, cl, what, cluster, version, *wait, time.Second*2, os.Stdout); err != nil {
		die(err)
	}
}

// upgradeCheck refuses a version step Talos does not support, before the
// spec changes. The operator checks again per node when it plans.
func upgradeCheck(c *osv1alpha1.TalosCluster, what, version string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("version %q is not of the form v1.2.3", version)
	}
	talos, k8s := c.Spec.TalosVersion, c.Spec.KubernetesVersion
	if what == "talos" {
		if render.CompareVersions(version, talos) < 0 {
			return fmt.Errorf("cluster %s asks for Talos %s; going back to %s is not an upgrade", c.Name, talos, version)
		}
		if err := render.UpgradeableFrom(talos, version); err != nil {
			return err
		}
		talos = version
	} else {
		if render.CompareVersions(version, k8s) < 0 {
			return fmt.Errorf("cluster %s asks for Kubernetes %s; going back to %s is not supported", c.Name, k8s, version)
		}
		// From what the API server reports as well as from the spec: a
		// cluster still on its way to the spec's version must not be asked
		// to skip one more minor.
		for _, from := range []string{k8s, c.Status.KubernetesVersion} {
			if from == "" {
				continue
			}
			if err := render.KubernetesUpgradeable(from, version); err != nil {
				return err
			}
		}
		k8s = version
	}
	return render.SupportedPair(talos, k8s)
}

func upgrade(ctx context.Context, cl client.Client, what, name, version string, wait, poll time.Duration, w io.Writer) error {
	c := &osv1alpha1.TalosCluster{}
	if err := cl.Get(ctx, client.ObjectKey{Name: name}, c); err != nil {
		return fmt.Errorf("read cluster %s: %w", name, err)
	}
	field, current, kind := "talosVersion", c.Spec.TalosVersion, osv1alpha1.RolloutTalosUpgrade
	if what == "kubernetes" {
		field, current, kind = "kubernetesVersion", c.Spec.KubernetesVersion, osv1alpha1.RolloutKubernetesUpgrade
	}
	if current == version {
		fmt.Fprintf(w, "cluster %s already asks for %s %s\n", name, what, version)
	} else {
		if err := upgradeCheck(c, what, version); err != nil {
			return fmt.Errorf("nothing changed: %w", err)
		}
		user, err := requester(ctx, cl)
		if err != nil {
			return fmt.Errorf("nothing changed: %w", err)
		}
		// resourceVersion: refuse when someone changed the cluster since it
		// was read and checked.
		patch, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{"uid": c.UID, "resourceVersion": c.ResourceVersion,
				"annotations": map[string]any{metalv1alpha1.AnnotationRequestedBy: user}},
			"spec": map[string]any{field: version},
		})
		if err := cl.Patch(ctx, c, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return fmt.Errorf("set %s of cluster %s: %w", field, name, err)
		}
		fmt.Fprintf(w, "cluster %s now asks for %s %s (was %s); nothing on a machine changed yet\n", name, what, version, current)
	}
	if wait <= 0 {
		fmt.Fprintln(w, "the operator proposes a rollout for it; see: pk-metal rollout list")
		return nil
	}
	deadline := time.Now().Add(wait)
	for {
		ro, err := proposedRollout(ctx, cl, name, kind, version)
		if err != nil {
			return err
		}
		if ro != nil && ro.Status.PlanHash != "" {
			fmt.Fprint(w, "\n"+renderRollout(ro))
			if ro.Spec.Approval.ApprovedBy == "" {
				fmt.Fprintf(w, "\nNothing runs until someone approves it: pk-metal rollout approve %s\n", ro.Name)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no rollout planned for %s %s on cluster %s after %s; the operator proposes one once it "+
				"has seen every node's version. See: pk-metal rollout list, and the cluster's conditions",
				what, version, name, shortDuration(wait))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// proposedRollout is the unfinished rollout of kind toward version on the
// cluster, nil while there is none.
func proposedRollout(ctx context.Context, cl client.Client, cluster string, kind osv1alpha1.RolloutKind,
	version string) (*osv1alpha1.TalosRollout, error) {
	list := &osv1alpha1.TalosRolloutList{}
	if err := cl.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list rollouts: %w", err)
	}
	for i := range list.Items {
		ro := &list.Items[i]
		if ro.Spec.ClusterRef.Name != cluster || ro.Spec.Kind != kind || ro.Spec.Target == nil || ro.Spec.Abort {
			continue
		}
		if ro.Spec.Target.TalosVersion != version && ro.Spec.Target.KubernetesVersion != version {
			continue
		}
		if c := meta.FindStatusCondition(ro.Status.Conditions, osv1alpha1.ConditionComplete); c != nil && c.Status != "Unknown" {
			continue
		}
		return ro, nil
	}
	return nil, nil
}

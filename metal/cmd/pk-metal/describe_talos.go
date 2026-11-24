package main

import (
	"fmt"
	"strings"
	"time"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// The plan lines of the Talos and Kubernetes actions. They say what happens
// to the node and its workloads in the words an approver needs: what stops,
// what is lost, how long the step waits and what is left when it gives up.
// The executor's step semantics are in docs/src/content/docs/reference/actions.mdx; keep the
// two in step.

// talosNodeRef is the TalosNode a Talos or Kubernetes action names, or "".
func talosNodeRef(a metalv1alpha1.HostOperationAction) string {
	switch {
	case a.Cordon != nil:
		return a.Cordon.NodeRef.Name
	case a.Drain != nil:
		return a.Drain.NodeRef.Name
	case a.Uncordon != nil:
		return a.Uncordon.NodeRef.Name
	case a.TalosReboot != nil:
		return a.TalosReboot.NodeRef.Name
	case a.TalosShutdown != nil:
		return a.TalosShutdown.NodeRef.Name
	case a.TalosUpgrade != nil:
		return a.TalosUpgrade.NodeRef.Name
	case a.TalosApplyConfig != nil:
		return a.TalosApplyConfig.NodeRef.Name
	case a.EtcdLeave != nil:
		return a.EtcdLeave.NodeRef.Name
	case a.EtcdRemoveMember != nil:
		return a.EtcdRemoveMember.NodeRef.Name
	case a.TalosReset != nil:
		return a.TalosReset.NodeRef.Name
	case a.WaitNodeReady != nil:
		return a.WaitNodeReady.NodeRef.Name
	case a.WaitEtcdHealthy != nil:
		return a.WaitEtcdHealthy.NodeRef.Name
	case a.AwaitCheckIn != nil:
		return a.AwaitCheckIn.NodeRef.Name
	}
	return ""
}

// nodeLabel names a node with its role and cluster when they are known.
func nodeLabel(name string, pc planContext) string {
	n := pc.Nodes[name]
	if n == nil {
		return "node " + name + " (TalosNode not found)"
	}
	role := "worker"
	if n.Spec.Role == osv1alpha1.RoleControlPlane {
		role = "control plane"
	}
	return fmt.Sprintf("node %s (%s of cluster %s)", name, role, n.Spec.ClusterRef.Name)
}

func secondsOr(s int32, def time.Duration) string {
	if s > 0 {
		return shortDuration(time.Duration(s) * time.Second)
	}
	return shortDuration(def)
}

// describeTalosAction is the plan line of a Talos or Kubernetes action; ok
// is false for the other actions.
func describeTalosAction(a metalv1alpha1.HostOperationAction, pc planContext) (string, bool) {
	node := nodeLabel(talosNodeRef(a), pc)
	switch {
	case a.Cordon != nil:
		return fmt.Sprintf("cordon %s: Kubernetes schedules no new pods there; running pods stay", node), true
	case a.Drain != nil:
		d := a.Drain
		s := fmt.Sprintf("drain %s: evict its pods through the Eviction API, so every PodDisruptionBudget holds "+
			"(DaemonSet and static pods stay); wait up to %s, then fail and leave the node cordoned",
			node, secondsOr(d.TimeoutSeconds, 15*time.Minute))
		if d.GracePeriodSeconds != nil {
			s += fmt.Sprintf("; each pod gets %ds to stop", *d.GracePeriodSeconds)
		}
		if d.DeleteEmptyDirData {
			s += "; pods with emptyDir volumes are evicted too and THAT DATA IS LOST"
		} else {
			s += "; a pod with emptyDir data fails the step"
		}
		return s, true
	case a.Uncordon != nil:
		return fmt.Sprintf("uncordon %s: Kubernetes schedules pods there again", node), true
	case a.TalosReboot != nil:
		how := "services stop and filesystems unmount first"
		if a.TalosReboot.Mode == "PowerCycle" {
			how += ", then a cold power cycle"
		}
		return fmt.Sprintf("REBOOT %s through Talos (%s); wait up to 20m for it to boot and run ready", node, how), true
	case a.TalosShutdown != nil:
		s := fmt.Sprintf("SHUT DOWN %s through Talos and wait until it stops answering; it stays off until powered on", node)
		if a.TalosShutdown.Force {
			s += " (forced: even if it cannot cordon and drain itself)"
		}
		return s, true
	case a.TalosUpgrade != nil:
		u := a.TalosUpgrade
		s := fmt.Sprintf("UPGRADE Talos on %s to %s from %s (image pinned by digest %s); the node reboots into it; "+
			"wait up to 45m for it to run %s (a node already on %s is left alone)",
			node, u.Version, imageRepo(u.Image), shortDigest(imageDigest(u.Image)), u.Version, u.Version)
		var notes []string
		if u.Stage {
			notes = append(notes, "staged: installed at the reboot")
		}
		if u.Preserve != nil && !*u.Preserve {
			notes = append(notes, "EPHEMERAL (pod data, etcd) not preserved on Talos before 1.8")
		}
		if u.Force {
			notes = append(notes, "forced: Talos skips its etcd health check")
		}
		if u.RebootMode == "PowerCycle" {
			notes = append(notes, "cold power cycle at the end")
		}
		if len(notes) > 0 {
			s += " (" + strings.Join(notes, "; ") + ")"
		}
		return s, true
	case a.TalosApplyConfig != nil:
		c := a.TalosApplyConfig
		return fmt.Sprintf("apply the machine config in Secret %s (content %s) to %s; %s",
			c.ConfigSecretRef.Name, shortDigest(c.Digest), node, applyModeText(c)), true
	case a.EtcdLeave != nil:
		return fmt.Sprintf("remove the etcd member of %s, asked on the node itself (leadership is handed over first); "+
			"etcd then has one member fewer", node), true
	case a.EtcdRemoveMember != nil:
		r := a.EtcdRemoveMember
		who := "member " + r.MemberID
		if r.Hostname != "" {
			who += " (" + r.Hostname + ")"
		}
		return fmt.Sprintf("remove etcd %s through another control plane of the cluster of %s; "+
			"it is gone for good, its node must join again to come back", who, node), true
	case a.TalosReset != nil:
		return describeReset(a.TalosReset, node), true
	case a.WaitNodeReady != nil:
		w := a.WaitNodeReady
		s := fmt.Sprintf("wait up to %s for %s to run ready in Talos and as a Kubernetes Node",
			secondsOr(w.TimeoutSeconds, 30*time.Minute), node)
		if w.Version != "" {
			s += ", on Talos " + w.Version
		}
		return s, true
	case a.WaitEtcdHealthy != nil:
		return fmt.Sprintf("wait up to %s until etcd of the cluster of %s is healthy: every member answers and votes, no alarm",
			secondsOr(a.WaitEtcdHealthy.TimeoutSeconds, 15*time.Minute), node), true
	}
	return "", false
}

func applyModeText(c *metalv1alpha1.TalosApplyConfigAction) string {
	switch c.Mode {
	case "Reboot":
		return "the node REBOOTS into it; wait up to 20m for it to run ready"
	case "Staged":
		return "staged: it takes effect at the next reboot, nothing changes now"
	case "Try":
		s := "tried: applied live and rolled back after "
		if c.TryTimeoutSeconds > 0 {
			s += shortDuration(time.Duration(c.TryTimeoutSeconds) * time.Second)
		} else {
			s += "the node's default (1m)"
		}
		return s + " unless confirmed"
	case "Auto":
		return "the node decides: live when it can, otherwise it REBOOTS"
	}
	return "applied live; the step fails if a change needs a reboot"
}

func describeReset(r *metalv1alpha1.TalosResetAction, node string) string {
	disks := "no user disk"
	if len(r.UserDisks) > 0 {
		disks = "the user disks " + strings.Join(r.UserDisks, ", ")
	}
	what := "the system disk and " + disks
	switch r.Mode {
	case "SystemDisk":
		what = "the system disk"
	case "UserDisks":
		what = disks
	}
	if len(r.SystemPartitions) > 0 {
		var parts []string
		for _, p := range r.SystemPartitions {
			parts = append(parts, string(p))
		}
		what = "the partitions " + strings.Join(parts, ", ")
	}
	s := fmt.Sprintf("WIPE %s of %s through Talos", what, node)
	if r.Graceful == nil || *r.Graceful {
		s += " (graceful: a control plane leaves etcd, the node is cordoned and drained first)"
	} else {
		s += " (not graceful: no etcd leave, no drain; a control plane's member stays listed until removed)"
	}
	if r.Reboot {
		return s + "; then it boots into maintenance mode"
	}
	return s + "; then it stays powered off"
}

// imageRepo is the image reference without its digest.
func imageRepo(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	return name
}

func imageDigest(ref string) string {
	_, d, _ := strings.Cut(ref, "@")
	return d
}

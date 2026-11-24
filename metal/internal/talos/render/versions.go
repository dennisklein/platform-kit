package render

import (
	"fmt"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/compatibility"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
)

// SupportedPair checks that a Talos release supports a Kubernetes release,
// with the support matrix machinery ships for each Talos minor. A pair it
// refuses is what the machines would refuse too: Talos validates the
// Kubernetes image tags against its own version.
func SupportedPair(talosVersion, kubernetesVersion string) error {
	tv, err := compatibility.ParseTalosVersion(&machine.VersionInfo{Tag: talosVersion})
	if err != nil {
		return fmt.Errorf("talos version %q: %w", talosVersion, err)
	}
	kv, err := compatibility.ParseKubernetesVersion(strings.TrimPrefix(kubernetesVersion, "v"))
	if err != nil {
		return fmt.Errorf("kubernetes version %q: %w", kubernetesVersion, err)
	}
	return kv.SupportedWith(tv)
}

// UpgradeableFrom checks that a node running Talos from can be upgraded (or
// downgraded) to Talos to in one step.
func UpgradeableFrom(from, to string) error {
	host, err := compatibility.ParseTalosVersion(&machine.VersionInfo{Tag: from})
	if err != nil {
		return fmt.Errorf("talos version %q: %w", from, err)
	}
	target, err := compatibility.ParseTalosVersion(&machine.VersionInfo{Tag: to})
	if err != nil {
		return fmt.Errorf("talos version %q: %w", to, err)
	}
	return target.UpgradeableFrom(host)
}

// KubernetesUpgradeable checks that a cluster on Kubernetes from can move to
// Kubernetes to in one rollout: the same minor, or the next one. Kubernetes
// supports no skipped minor (its version-skew policy), and talosctl
// upgrade-k8s refuses the same jumps; a downgrade is refused too. It is a
// few lines, so it is written here instead of taking go-kubernetes for it.
func KubernetesUpgradeable(from, to string) error {
	f, err := semver.ParseTolerant(from)
	if err != nil {
		return fmt.Errorf("kubernetes version %q: %w", from, err)
	}
	t, err := semver.ParseTolerant(to)
	if err != nil {
		return fmt.Errorf("kubernetes version %q: %w", to, err)
	}
	switch {
	case t.LT(f):
		return fmt.Errorf("kubernetes %s is older than %s: a downgrade is not supported", to, from)
	case t.Major != f.Major || t.Minor > f.Minor+1:
		return fmt.Errorf("kubernetes %s is more than one minor after %s: upgrade one minor at a time "+
			"(v%d.%d first)", to, from, f.Major, f.Minor+1)
	}
	return nil
}

// CompareVersions compares two versions (v1.14.6 and 1.14.6 alike): -1, 0 or
// +1. A version that does not parse sorts first, so that it is never taken
// for the newer one.
func CompareVersions(a, b string) int {
	va, errA := semver.ParseTolerant(a)
	vb, errB := semver.ParseTolerant(b)
	switch {
	case errA != nil && errB != nil:
		return 0
	case errA != nil:
		return -1
	case errB != nil:
		return 1
	}
	return va.Compare(vb)
}

// RenderVersion is the Talos version to render a node's config for: the
// cluster's, unless the machine runs an older release, whose contract it
// keeps until it is upgraded. lastRendered is what the config was rendered
// for before, kept while the machine says nothing (it is rebooting, or not
// handed off yet).
func RenderVersion(clusterVersion, running, lastRendered string) string {
	cur := running
	if cur == "" {
		cur = lastRendered
	}
	if cur == "" || CompareVersions(cur, clusterVersion) >= 0 {
		return clusterVersion
	}
	if _, err := Contract(cur); err != nil {
		// Older than this renderer handles: render for the cluster's
		// version, and the skew condition says the machine needs an upgrade.
		return clusterVersion
	}
	return "v" + strings.TrimPrefix(cur, "v")
}

// KubeProxyImage reads the kube-proxy image a rendered config deploys, and
// whether it deploys kube-proxy at all: what a Kubernetes upgrade moves the
// DaemonSet to, taken from the render so that patches and mirrors count.
func KubeProxyImage(machineConfig []byte) (image string, enabled bool, err error) {
	cfg, err := configloader.NewFromBytes(machineConfig)
	if err != nil {
		return "", false, fmt.Errorf("load rendered config: %w", err)
	}
	p := cfg.K8sProxyConfig()
	if p == nil {
		return "", false, nil
	}
	return p.Image(), p.Enabled(), nil
}

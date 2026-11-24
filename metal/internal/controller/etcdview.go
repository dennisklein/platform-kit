package controller

import (
	"context"
	"errors"
	"strconv"

	"github.com/siderolabs/talos/pkg/machinery/role"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// etcdView is one live reading of a cluster's etcd membership, asked of the
// control planes themselves. Adding and removing control planes is decided
// on it, never on the TalosNode count: a member left behind by a node that
// was wiped without leaving is still a member, and still counts towards the
// majority etcd needs, until something removes it.
//
// It is read through the same path as the cluster guard's (etcdHealth in
// talosaccess.go), so the guard that holds a reboot and the verdict that
// holds a removal never see two different etcds.
type etcdView struct {
	// members is the membership as the first answering control plane lists
	// it; nil when none answered.
	members []talos.EtcdMember
	// healthy holds the members whose own node answered its etcd status
	// with no error and a known leader. A member whose node does not answer
	// is not healthy.
	healthy map[uint64]bool
	// leader is the leading member's ID, zero when unknown.
	leader uint64
	// readFrom names the control plane that listed the members.
	readFrom string
}

// viewFromReport turns the guard's health report into the view the lifecycle
// verdicts read. Learners keep their own flag on the member, so healthy only
// says whether the member answers well.
func viewFromReport(rep *etcdReport) etcdView {
	v := etcdView{members: rep.members, healthy: map[uint64]bool{}, leader: rep.leader, readFrom: rep.via}
	for _, m := range rep.members {
		if _, sick := rep.sick[m.ID]; !sick {
			v.healthy[m.ID] = true
		}
	}
	return v
}

// known: some control plane listed the members.
func (v etcdView) known() bool { return v.readFrom != "" }

// member returns the member with this hostname, or nil. A TalosNode's name is
// its hostname, which is what etcd lists.
func (v etcdView) member(hostname string) *talos.EtcdMember {
	for i := range v.members {
		if v.members[i].Hostname == hostname {
			return &v.members[i]
		}
	}
	return nil
}

// voting counts the members that vote (not learners); healthyVoting those of
// them that are healthy.
func (v etcdView) voting() (voting, healthyVoting int) {
	for _, m := range v.members {
		if m.IsLearner {
			continue
		}
		voting++
		if v.healthy[m.ID] {
			healthyVoting++
		}
	}
	return voting, healthyVoting
}

// learners names the members that do not vote yet.
func (v etcdView) learners() []string {
	var out []string
	for _, m := range v.members {
		if m.IsLearner {
			out = append(out, m.Hostname)
		}
	}
	return out
}

// unhealthy names the members whose node did not report a healthy etcd.
func (v etcdView) unhealthy() []string {
	var out []string
	for _, m := range v.members {
		if !v.healthy[m.ID] {
			out = append(out, m.Hostname)
		}
	}
	return out
}

// memberIDHex is a member ID the way etcd prints it, and the way the
// etcdRemoveMember action carries it.
func memberIDHex(id uint64) string { return strconv.FormatUint(id, 16) }

// clusterEtcd reads the etcd membership of cluster through its control
// planes, with a talosconfig minted for this reading from the cluster's
// secrets bundle. It returns a view that is not known, and says why, when
// there is nothing to ask with.
func clusterEtcd(ctx context.Context, c client.Reader, dial talos.Dialer, namespace string,
	cluster *osv1alpha1.TalosCluster) (etcdView, string, error) {
	if dial == nil {
		return etcdView{}, "this operator has no Talos client configured", nil
	}
	nodes, err := clusterMembers(ctx, c, cluster.Name)
	if err != nil {
		return etcdView{}, "", err
	}
	cps := controlPlanes(nodes, "")
	acc, err := newTalosAccessAs(ctx, c, namespace, dial, cluster, role.Reader)
	if err != nil {
		if unavailable := (*bundleUnavailable)(nil); errors.As(err, &unavailable) {
			return etcdView{}, unavailable.msg, nil
		}
		return etcdView{}, "", err
	}
	rep, err := acc.etcdHealth(ctx, cps, nodes)
	if err != nil {
		// Nobody listed the members: an observation, not a failure.
		return etcdView{}, err.Error(), nil
	}
	v := viewFromReport(rep)
	return v, "", nil
}

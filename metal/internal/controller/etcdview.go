package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// etcdView is one live reading of a cluster's etcd membership, asked of the
// control planes themselves. Adding and removing control planes is decided
// on it, never on the TalosNode count: a member left behind by a node that
// was wiped without leaving is still a member, and still counts towards the
// majority etcd needs, until something removes it.
type etcdView struct {
	// members is the membership as the first answering control plane lists
	// it; nil when none answered.
	members []talos.EtcdMember
	// healthy holds the members whose own node answered its etcd status
	// with no error. A member whose node does not answer is not healthy.
	healthy map[uint64]bool
	// leader is the leading member's ID, zero when unknown.
	leader uint64
	// readFrom names the control plane that listed the members.
	readFrom string
	// silent names the control planes that did not answer.
	silent []string
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

// readEtcd asks every control plane with an address for its etcd status, and
// the first that answers for the member list. Errors are observations: a
// control plane that does not answer is named in silent, and its member is
// not healthy.
func readEtcd(ctx context.Context, dial talos.Dialer, talosconfig []byte, cps []osv1alpha1.TalosNode) etcdView {
	v := etcdView{healthy: map[uint64]bool{}}
	cps = slices.Clone(cps)
	slices.SortFunc(cps, func(a, b osv1alpha1.TalosNode) int { return strings.Compare(a.Name, b.Name) })
	leaders := map[uint64]int{}
	for i := range cps {
		n := &cps[i]
		addr := render.NodeAddress(n)
		if addr == "" {
			continue
		}
		err := talos.With(ctx, dial, talos.Target{Endpoint: addr, Talosconfig: talosconfig}, func(c talos.Client) error {
			if v.readFrom == "" {
				if members, err := c.EtcdMembers(ctx); err == nil && len(members) > 0 {
					v.members, v.readFrom = members, n.Name
				}
			}
			st, err := c.EtcdStatus(ctx)
			if err != nil {
				return err
			}
			if len(st.Errors) == 0 {
				v.healthy[st.MemberID] = true
			}
			if st.LeaderID != 0 {
				leaders[st.LeaderID]++
			}
			return nil
		})
		if err != nil {
			v.silent = append(v.silent, n.Name)
		}
	}
	// The leader most members follow; during an election they may disagree
	// for a moment, and the next reading settles it.
	best := 0
	for id, n := range leaders {
		if n > best || (n == best && id < v.leader) {
			v.leader, best = id, n
		}
	}
	return v
}

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
	var cps []osv1alpha1.TalosNode
	var endpoints []string
	for _, n := range nodes {
		if n.Spec.Role != osv1alpha1.RoleControlPlane {
			continue
		}
		cps = append(cps, n)
		if addr := render.NodeAddress(&n); addr != "" {
			endpoints = append(endpoints, addr)
		}
	}
	bundle, err := loadBundle(ctx, c, namespace, cluster)
	if err != nil {
		if unavailable := (*bundleUnavailable)(nil); errors.As(err, &unavailable) {
			return etcdView{}, unavailable.msg, nil
		}
		return etcdView{}, "", err
	}
	tcfg, err := render.Talosconfig(bundle, cluster.Name, endpoints, checkinCertTTL)
	if err != nil {
		return etcdView{}, "", fmt.Errorf("mint talosconfig for %s: %w", cluster.Name, err)
	}
	v := readEtcd(ctx, dial, tcfg, cps)
	if !v.known() {
		why := "no control plane of cluster " + cluster.Name + " listed its etcd members"
		if len(v.silent) > 0 {
			why += " (not answering: " + strings.Join(v.silent, ", ") + ")"
		}
		return v, why, nil
	}
	return v, "", nil
}

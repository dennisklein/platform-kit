package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/role"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// Reaching a Talos cluster from the operator: the machine API of one node,
// asked with a client certificate minted from the cluster's secrets bundle
// for this one question, and the cluster's Kubernetes API, asked with the
// admin kubeconfig the TalosCluster reconciler publishes. The executor's
// Talos steps and the cluster guard both go through here.

// workloadTimeout bounds one request to a workload cluster's Kubernetes API.
// A cluster whose API server is down must not hold an executor pass.
const workloadTimeout = 20 * time.Second

// talosAccess reaches the nodes of one TalosCluster.
type talosAccess struct {
	dial        talos.Dialer
	cluster     *osv1alpha1.TalosCluster
	talosconfig []byte
}

// newTalosAccess mints the admin client config for a cluster's machine API:
// the executor's, whose steps change machines. A nil dialer is a wiring
// error the caller reports.
func newTalosAccess(ctx context.Context, c client.Reader, namespace string, dial talos.Dialer,
	cluster *osv1alpha1.TalosCluster) (*talosAccess, error) {
	return newTalosAccessAs(ctx, c, namespace, dial, cluster, role.Admin)
}

// newTalosAccessAs mints the client config with another role. A caller that
// only reads (the cluster guard's etcd health) asks for os:reader, so a
// certificate that leaks from it changes nothing on a machine.
func newTalosAccessAs(ctx context.Context, c client.Reader, namespace string, dial talos.Dialer,
	cluster *osv1alpha1.TalosCluster, r role.Role) (*talosAccess, error) {
	if dial == nil {
		return nil, errors.New("this operator runs without a Talos client (no machine API access is configured)")
	}
	bundle, err := loadBundle(ctx, c, namespace, cluster)
	if err != nil {
		return nil, err
	}
	// The endpoint in the config does not matter: every dial names its own.
	tcfg, err := render.TalosconfigWithRole(bundle, cluster.Name, []string{cluster.Spec.Endpoint}, checkinCertTTL, r)
	if err != nil {
		return nil, fmt.Errorf("mint a Talos client certificate for cluster %s: %w", cluster.Name, err)
	}
	return &talosAccess{dial: dial, cluster: cluster, talosconfig: tcfg}, nil
}

// readerTalosconfig mints an os:reader client config for one look at a
// node: status, version, etcd health. Talos serves those to os:reader, and
// only the steps and the dry-run drift check need os:admin.
func readerTalosconfig(bundle *secrets.Bundle, cluster string, endpoints []string) ([]byte, error) {
	return render.TalosconfigWithRole(bundle, cluster, endpoints, checkinCertTTL, role.Reader)
}

// with dials endpoint and runs fn on the client.
func (a *talosAccess) with(ctx context.Context, endpoint string, fn func(talos.Client) error) error {
	if endpoint == "" {
		return &unreachableError{err: errors.New("no address to dial")}
	}
	c, err := a.dial(ctx, talos.Target{Endpoint: endpoint, Talosconfig: a.talosconfig})
	if err != nil {
		return &unreachableError{err: err}
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

// unreachableError: the node's machine API could not be dialed at all. For a
// step that waits for a restart this is the "down" half of it, not a failure.
type unreachableError struct{ err error }

func (e *unreachableError) Error() string { return "machine API does not answer: " + e.err.Error() }
func (e *unreachableError) Unwrap() error { return e.err }

// unavailable: the error says the node could not be asked, not that it
// refused: no dial, a connection that broke (a node going down under the
// call), a deadline. A call that failed so may or may not have reached the
// node, and a step finds out from the node on its next look.
func unavailable(err error) bool {
	var unreach *unreachableError
	var nerr net.Error
	if errors.As(err, &unreach) || errors.As(err, &nerr) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return true
	}
	return false
}

// nodeEndpoint is where a TalosNode's machine API answers.
func nodeEndpoint(n *osv1alpha1.TalosNode) string {
	if a := render.NodeAddress(n); a != "" {
		return a
	}
	return n.Status.Address
}

// workloadClient opens a client for the cluster's Kubernetes API from the
// admin kubeconfig the TalosCluster reconciler publishes.
func workloadClient(ctx context.Context, c client.Reader, namespace string, cluster *osv1alpha1.TalosCluster,
	build func(context.Context, []byte) (client.Client, error)) (client.Client, error) {
	ref := cluster.Status.KubeconfigSecretRef
	if ref == nil {
		return nil, fmt.Errorf("cluster %s has no published kubeconfig yet (status.kubeconfigSecretRef)", cluster.Name)
	}
	if ref.Namespace != namespace {
		return nil, fmt.Errorf("kubeconfig Secret %s/%s is outside the operator's namespace %s", ref.Namespace, ref.Name, namespace)
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, sec); err != nil {
		return nil, fmt.Errorf("kubeconfig of cluster %s: %w", cluster.Name, err)
	}
	kubeconfig := sec.Data[KubeconfigKey]
	if len(kubeconfig) == 0 {
		return nil, fmt.Errorf("kubeconfig Secret %s/%s carries no %s", ref.Namespace, ref.Name, KubeconfigKey)
	}
	if build == nil {
		build = buildWorkloadClient
	}
	return build(ctx, kubeconfig)
}

// buildWorkloadClient is an uncached client for a workload cluster, which
// knows the core and policy kinds a drain needs.
func buildWorkloadClient(_ context.Context, kubeconfig []byte) (client.Client, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	cfg.Timeout = workloadTimeout
	return client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
}

// etcdReport is the health of one etcd cluster, counted from its own member
// list: a member no TalosNode describes still counts, and a TalosNode that
// is not a member does not.
type etcdReport struct {
	// via is the control plane that listed the members.
	via     string
	members []talos.EtcdMember
	// sick maps a member ID to why it is not healthy: it does not answer,
	// reports errors, knows no leader.
	sick map[uint64]string
	// learners are members that do not vote yet.
	learners map[uint64]bool
	// alarms are the raised etcd alarms (NOSPACE, CORRUPT).
	alarms []string
	// leader is the member most members follow, zero when none knows one.
	// During an election they may disagree for a moment; the next reading
	// settles it.
	leader uint64
}

// healthy: the member answers, votes, and reports no error.
func (r *etcdReport) healthy(id uint64) bool {
	_, sick := r.sick[id]
	return !sick && !r.learners[id]
}

// member finds the member with hostname, or nil.
func (r *etcdReport) member(hostname string) *talos.EtcdMember {
	for i := range r.members {
		if r.members[i].Hostname == hostname {
			return &r.members[i]
		}
	}
	return nil
}

// problems lists, one phrase each, what is wrong with the members other than
// skip (0 skips none), and the alarms.
func (r *etcdReport) problems(skip uint64) []string {
	var out []string
	for _, m := range r.members {
		if m.ID == skip {
			continue
		}
		name := memberName(m)
		if why, sick := r.sick[m.ID]; sick {
			out = append(out, name+" "+why)
		} else if r.learners[m.ID] {
			out = append(out, name+" is a learner that does not vote yet")
		}
	}
	for _, a := range r.alarms {
		out = append(out, "alarm "+a)
	}
	return out
}

func memberName(m talos.EtcdMember) string {
	if m.Hostname != "" {
		return fmt.Sprintf("member %s (%x)", m.Hostname, m.ID)
	}
	return fmt.Sprintf("member %x", m.ID)
}

// etcdHealth asks the cluster's etcd how it is: the member list and alarms
// from the first control plane in via that answers, then every member's own
// status from the member itself. A member is found by its hostname (a
// TalosNode's name is its hostname), else by its client URL. It fails only
// when no control plane in via can list the members.
func (a *talosAccess) etcdHealth(ctx context.Context, via []osv1alpha1.TalosNode, nodes []osv1alpha1.TalosNode) (*etcdReport, error) {
	rep := &etcdReport{sick: map[uint64]string{}, learners: map[uint64]bool{}}
	var errs []string
	for i := range via {
		n := &via[i]
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		err := a.with(pctx, nodeEndpoint(n), func(c talos.Client) error {
			members, err := c.EtcdMembers(pctx)
			if err != nil {
				return err
			}
			alarms, err := c.EtcdAlarms(pctx)
			if err != nil {
				return err
			}
			rep.members = members
			for _, al := range alarms {
				if al.Alarm != "" && al.Alarm != "NONE" {
					rep.alarms = append(rep.alarms, fmt.Sprintf("%s on member %x", al.Alarm, al.MemberID))
				}
			}
			return nil
		})
		cancel()
		if err == nil {
			rep.via = n.Name
			break
		}
		errs = append(errs, n.Name+": "+err.Error())
	}
	if rep.via == "" {
		if len(errs) == 0 {
			return nil, fmt.Errorf("cluster %s has no control plane to ask", a.cluster.Name)
		}
		return nil, fmt.Errorf("no control plane of cluster %s lists the etcd members (%s)", a.cluster.Name, strings.Join(errs, "; "))
	}
	leaders := map[uint64]int{}
	for _, m := range rep.members {
		if m.IsLearner {
			rep.learners[m.ID] = true
		}
		endpoint := memberEndpoint(m, nodes)
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		err := a.with(pctx, endpoint, func(c talos.Client) error {
			st, err := c.EtcdStatus(pctx)
			if err != nil {
				return err
			}
			switch {
			case st.MemberID != m.ID:
				rep.sick[m.ID] = fmt.Sprintf("is not what %s runs (it runs member %x)", endpoint, st.MemberID)
			case len(st.Errors) > 0:
				rep.sick[m.ID] = "reports " + strings.Join(st.Errors, ", ")
			case st.LeaderID == 0:
				rep.sick[m.ID] = "knows no leader"
			}
			if st.IsLearner {
				rep.learners[m.ID] = true
			}
			if st.LeaderID != 0 {
				leaders[st.LeaderID]++
			}
			return nil
		})
		cancel()
		if err != nil {
			rep.sick[m.ID] = "does not answer (" + err.Error() + ")"
		}
	}
	best := 0
	for id, n := range leaders {
		if n > best || (n == best && id < rep.leader) {
			rep.leader, best = id, n
		}
	}
	return rep, nil
}

// memberEndpoint is where a member's Talos API answers: its TalosNode's
// address, else the host of its first client URL.
func memberEndpoint(m talos.EtcdMember, nodes []osv1alpha1.TalosNode) string {
	if i := slices.IndexFunc(nodes, func(n osv1alpha1.TalosNode) bool { return n.Name == m.Hostname }); i >= 0 {
		if e := nodeEndpoint(&nodes[i]); e != "" {
			return e
		}
	}
	for _, raw := range m.ClientURLs {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return ""
}

// controlPlanes are the cluster's control-plane TalosNodes, in name order,
// with the one named last moved to the end: the others are asked first.
func controlPlanes(nodes []osv1alpha1.TalosNode, last string) []osv1alpha1.TalosNode {
	var out []osv1alpha1.TalosNode
	for _, n := range nodes {
		if n.Spec.Role == osv1alpha1.RoleControlPlane {
			out = append(out, n)
		}
	}
	slices.SortStableFunc(out, func(a, b osv1alpha1.TalosNode) int {
		switch {
		case a.Name == last:
			return 1
		case b.Name == last:
			return -1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

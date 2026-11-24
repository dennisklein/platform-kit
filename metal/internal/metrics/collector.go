package metrics

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	yaml "go.yaml.in/yaml/v4"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// scrapeTimeout bounds one scrape's cache reads. They are in-memory, so this
// only matters while an informer is still starting.
const scrapeTimeout = 5 * time.Second

func desc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(Namespace, "", name), help, labels, nil)
}

var (
	descHostPhase = desc("host_phase",
		"1 for the phase each Host is in.", "host", "phase")
	descHostBlocked = desc("host_blocked",
		"1 while a Host is blocked, with the reason (status.blockedOn).", "host", "reason")
	descOperations = desc("operations",
		"HostOperations by phase and action; an operation counts once for each distinct action it carries.", "phase", "action")
	descOperationFailed = desc("operation_failed",
		"1 for each HostOperation that failed and still waits for a person (retry or delete).", "namespace", "operation", "host", "reason")
	descAwaitingApproval = desc("operation_awaiting_approval_seconds",
		"How long each HostOperation has been waiting for approval.", "namespace", "operation", "host", "reason")
	descOperationBlocked = desc("operation_blocked",
		"1 for each approved HostOperation that has not started, with the reason it waits (OperationQueued, OutsideMaintenanceWindow, …).",
		"namespace", "operation", "host", "reason")
	descBMCReachable = desc("bmc_reachable",
		"1 when a ManagementEndpoint answers Redfish, 0 when it does not; absent while not yet probed.", "endpoint")
	descOperationRunning = desc("operation_running_seconds",
		"How long each running HostOperation has been in its current step (step is the action's key).",
		"namespace", "operation", "host", "step")
	descNodeReady = desc("talosnode_ready",
		"1 when a TalosNode reports Ready, 0 otherwise.", "node", "cluster", "role")
	// The same rule as pk-metal status: a node of a bootstrapped cluster that
	// has answered once (it reported a version). A node still being installed
	// is not expected to be Ready, and its alert would page for normal work.
	descNodeExpectedReady = desc("talosnode_expected_ready",
		"1 when a TalosNode is expected to be Ready: its cluster is bootstrapped and the node reported a version.",
		"node", "cluster", "role")
	descNodeVersion = desc("talosnode_info",
		"1 per TalosNode, labelled with the Talos version it reports.", "node", "cluster", "version")
	descClusterAvailable = desc("taloscluster_available",
		"1 when a TalosCluster's Kubernetes API answers and every node is Ready.", "cluster")
	descClusterExpectedAvailable = desc("taloscluster_expected_available",
		"1 when a TalosCluster is expected to be available: it is bootstrapped.", "cluster")
	descClusterMembers = desc("taloscluster_nodes",
		"TalosCluster nodes by role and readiness.", "cluster", "role", "ready")
	descRolloutCondition = desc("rollout_condition",
		"1 for each TalosRollout that has not succeeded, with the reason it is where it is "+
			"(AwaitingApproval, PlanBlocked, WaitingForHealth, PausedAfterFirstNode, RolloutFailed, …).",
		"rollout", "cluster", "reason")
	descBackupLastSuccess = desc("etcd_backup_last_success_timestamp_seconds",
		"Unix time of the newest verified etcd snapshot of a TalosCluster, over all its EtcdBackupPolicies.", "cluster")
	// The interval makes the age readable: a policy that runs once a day is
	// not late at hour 13. Absent for a cluster without a policy.
	descBackupInterval = desc("etcd_backup_interval_seconds",
		"The shortest spec.interval of the EtcdBackupPolicies of a TalosCluster.", "cluster")
	descCredentialExpiry = desc("credentials_expiry_timestamp_seconds",
		"Unix time at which a published admin credential's client certificate expires.", "cluster", "kind")
	// The issue time is what makes the expiry readable: the operator renews
	// at a fraction of the lifetime (spec.credentials.ttl), so an alert on a
	// fixed number of days is wrong for every TTL but one.
	descCredentialIssued = desc("credentials_issued_timestamp_seconds",
		"Unix time from which a published admin credential's client certificate is valid (the one that expires first).",
		"cluster", "kind")
)

// Collector derives the scrape-time series from the cache on every scrape.
type Collector struct {
	Reader               client.Reader
	CredentialsNamespace string
	// Now is the clock; nil is time.Now. Tests fix it.
	Now func() time.Time
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		descHostPhase, descHostBlocked, descOperations, descOperationFailed, descAwaitingApproval,
		descOperationBlocked, descOperationRunning, descBMCReachable, descNodeReady, descNodeExpectedReady,
		descNodeVersion, descClusterAvailable, descClusterExpectedAvailable, descClusterMembers,
		descRolloutCondition, descBackupLastSuccess, descBackupInterval, descCredentialExpiry, descCredentialIssued,
	} {
		ch <- d
	}
}

// Collect reads each kind once. A kind that cannot be listed is skipped and
// the others are still served: one missing informer should not blank the
// whole page an alert depends on.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	c.hosts(ctx, ch)
	c.operations(ctx, ch, now)
	c.endpoints(ctx, ch)
	c.nodes(ctx, ch)
	c.clusters(ctx, ch)
	c.rollouts(ctx, ch)
	c.backups(ctx, ch)
}

func gauge(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (c *Collector) hosts(ctx context.Context, ch chan<- prometheus.Metric) {
	hosts := &metalv1alpha1.HostList{}
	if err := c.Reader.List(ctx, hosts); err != nil {
		return
	}
	for i := range hosts.Items {
		h := &hosts.Items[i]
		if h.Status.Phase != "" {
			gauge(ch, descHostPhase, 1, h.Name, string(h.Status.Phase))
		}
		if h.Status.BlockedOn != "" {
			gauge(ch, descHostBlocked, 1, h.Name, h.Status.BlockedOn)
		}
	}
}

func (c *Collector) operations(ctx context.Context, ch chan<- prometheus.Metric, now time.Time) {
	ops := &metalv1alpha1.HostOperationList{}
	if err := c.Reader.List(ctx, ops); err != nil {
		return
	}
	type key struct{ phase, action string }
	counts := map[key]int{}
	for i := range ops.Items {
		op := &ops.Items[i]
		phase := string(op.Status.Phase)
		if phase == "" {
			phase = string(metalv1alpha1.OperationProposed)
		}
		seen := map[string]bool{}
		for _, a := range op.Spec.Actions {
			if k := a.Key(); !seen[k] {
				seen[k] = true
				counts[key{phase, k}]++
			}
		}
		labels := []string{op.Namespace, op.Name, op.Spec.HostRef.Name, op.Spec.Reason}
		switch {
		case op.Status.Phase == metalv1alpha1.OperationFailed:
			gauge(ch, descOperationFailed, 1, labels...)
		case op.AwaitsApproval():
			gauge(ch, descAwaitingApproval, now.Sub(op.CreationTimestamp.Time).Seconds(), labels...)
		case op.Status.BlockedOn != "":
			gauge(ch, descOperationBlocked, 1, append(labels[:3:3], op.Status.BlockedOn)...)
		case op.Status.Phase == metalv1alpha1.OperationRunning:
			if step, since, ok := currentStep(op); ok {
				gauge(ch, descOperationRunning, now.Sub(since).Seconds(), append(labels[:3:3], step)...)
			}
		}
	}
	for k, n := range counts {
		gauge(ch, descOperations, float64(n), k.phase, k.action)
	}
}

// currentStep is the action a running operation is in and when it entered
// it: when the step before it finished, or when execution started for the
// first step. ok is false when the status does not say.
func currentStep(op *metalv1alpha1.HostOperation) (step string, since time.Time, ok bool) {
	i := len(op.Status.Steps)
	if i >= len(op.Spec.Actions) {
		return "", time.Time{}, false
	}
	switch {
	case i > 0 && op.Status.Steps[i-1].FinishedAt != nil:
		since = op.Status.Steps[i-1].FinishedAt.Time
	case i == 0 && op.Status.Baseline != nil && op.Status.Baseline.CapturedAt != nil:
		since = op.Status.Baseline.CapturedAt.Time
	default:
		return "", time.Time{}, false
	}
	return op.Spec.Actions[i].Key(), since, true
}

func (c *Collector) endpoints(ctx context.Context, ch chan<- prometheus.Metric) {
	eps := &metalv1alpha1.ManagementEndpointList{}
	if err := c.Reader.List(ctx, eps); err != nil {
		return
	}
	for i := range eps.Items {
		ep := &eps.Items[i]
		r := meta.FindStatusCondition(ep.Status.Conditions, metalv1alpha1.ConditionReachable)
		if r == nil || r.Status == metav1.ConditionUnknown {
			continue // not yet probed: no fact to report
		}
		gauge(ch, descBMCReachable, boolValue(r.Status == metav1.ConditionTrue), ep.Name)
	}
}

func (c *Collector) nodes(ctx context.Context, ch chan<- prometheus.Metric) {
	nodes := &osv1alpha1.TalosNodeList{}
	if err := c.Reader.List(ctx, nodes); err != nil {
		return
	}
	// A cluster that cannot be read counts as not bootstrapped: the node
	// is then not expected to be Ready, which errs towards no page.
	bootstrapped := map[string]bool{}
	clusters := &osv1alpha1.TalosClusterList{}
	if err := c.Reader.List(ctx, clusters); err == nil {
		for i := range clusters.Items {
			bootstrapped[clusters.Items[i].Name] = clusters.Items[i].Status.BootstrappedAt != nil
		}
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		ready := meta.IsStatusConditionTrue(n.Status.Conditions, osv1alpha1.ConditionReady)
		gauge(ch, descNodeReady, boolValue(ready), n.Name, n.Spec.ClusterRef.Name, string(n.Spec.Role))
		gauge(ch, descNodeExpectedReady, boolValue(bootstrapped[n.Spec.ClusterRef.Name] && n.Status.Version != ""),
			n.Name, n.Spec.ClusterRef.Name, string(n.Spec.Role))
		if n.Status.Version != "" {
			gauge(ch, descNodeVersion, 1, n.Name, n.Spec.ClusterRef.Name, n.Status.Version)
		}
	}
}

func (c *Collector) clusters(ctx context.Context, ch chan<- prometheus.Metric) {
	clusters := &osv1alpha1.TalosClusterList{}
	if err := c.Reader.List(ctx, clusters); err != nil {
		return
	}
	for i := range clusters.Items {
		cl := &clusters.Items[i]
		st := cl.Status
		gauge(ch, descClusterAvailable,
			boolValue(meta.IsStatusConditionTrue(st.Conditions, osv1alpha1.ConditionAvailable)), cl.Name)
		gauge(ch, descClusterExpectedAvailable, boolValue(st.BootstrappedAt != nil), cl.Name)
		gauge(ch, descClusterMembers, float64(st.ReadyControlPlanes), cl.Name, string(osv1alpha1.RoleControlPlane), "true")
		gauge(ch, descClusterMembers, float64(st.ControlPlanes-st.ReadyControlPlanes), cl.Name, string(osv1alpha1.RoleControlPlane), "false")
		gauge(ch, descClusterMembers, float64(st.ReadyWorkers), cl.Name, string(osv1alpha1.RoleWorker), "true")
		gauge(ch, descClusterMembers, float64(st.Workers-st.ReadyWorkers), cl.Name, string(osv1alpha1.RoleWorker), "false")
		if ref := st.KubeconfigSecretRef; ref != nil {
			if v, ok := c.secretValidity(ctx, ref, "kubeconfig", kubeconfigValidity); ok {
				gauge(ch, descCredentialExpiry, float64(v.NotAfter.Unix()), cl.Name, "kubeconfig")
				gauge(ch, descCredentialIssued, float64(v.NotBefore.Unix()), cl.Name, "kubeconfig")
			}
		}
		if ref := st.TalosconfigSecretRef; ref != nil {
			if v, ok := c.secretValidity(ctx, ref, "talosconfig", talosconfigValidity); ok {
				gauge(ch, descCredentialExpiry, float64(v.NotAfter.Unix()), cl.Name, "talosconfig")
				gauge(ch, descCredentialIssued, float64(v.NotBefore.Unix()), cl.Name, "talosconfig")
			}
		}
	}
}

func (c *Collector) rollouts(ctx context.Context, ch chan<- prometheus.Metric) {
	ros := &osv1alpha1.TalosRolloutList{}
	if err := c.Reader.List(ctx, ros); err != nil {
		return
	}
	for i := range ros.Items {
		ro := &ros.Items[i]
		st := RolloutStateOf(ro)
		if st.Succeeded {
			continue
		}
		gauge(ch, descRolloutCondition, 1, ro.Name, ro.Spec.ClusterRef.Name, st.Reason)
	}
}

// backups reads the backup facts from the EtcdBackupPolicies, which the
// backup controller keeps current. A cluster backed up by two policies is as
// fresh as the fresher one, and is due as often as the more frequent one
// says. A cluster whose last policy is deleted has no series any more.
func (c *Collector) backups(ctx context.Context, ch chan<- prometheus.Metric) {
	policies := &osv1alpha1.EtcdBackupPolicyList{}
	if err := c.Reader.List(ctx, policies); err != nil {
		return
	}
	type facts struct {
		last     time.Time
		interval time.Duration
	}
	byCluster := map[string]*facts{}
	for i := range policies.Items {
		p := &policies.Items[i]
		if !p.DeletionTimestamp.IsZero() {
			continue
		}
		f := byCluster[p.Spec.ClusterRef.Name]
		if f == nil {
			f = &facts{}
			byCluster[p.Spec.ClusterRef.Name] = f
		}
		if iv := p.Spec.Interval.Duration; iv > 0 && (f.interval == 0 || iv < f.interval) {
			f.interval = iv
		}
		if last := p.Status.LastSuccessfulSnapshot; last != nil && last.TakenAt.After(f.last) {
			f.last = last.TakenAt.Time
		}
	}
	for cluster, f := range byCluster {
		if f.interval > 0 {
			gauge(ch, descBackupInterval, f.interval.Seconds(), cluster)
		}
		if !f.last.IsZero() {
			gauge(ch, descBackupLastSuccess, float64(f.last.Unix()), cluster)
		}
	}
}

// validity is a client certificate's validity period.
type validity struct{ NotBefore, NotAfter time.Time }

// secretValidity reads one published credential and the validity of the
// client certificate inside it that expires first. A Secret outside the credentials namespace is not
// read: the cache does not hold it, and the operator does not publish there.
func (c *Collector) secretValidity(ctx context.Context, ref *osv1alpha1.SecretReference, key string,
	parse func([]byte) (validity, bool)) (validity, bool) {
	if c.CredentialsNamespace != "" && ref.Namespace != c.CredentialsNamespace {
		return validity{}, false
	}
	s := &corev1.Secret{}
	if err := c.Reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, s); err != nil {
		return validity{}, false
	}
	return parse(s.Data[key])
}

// kubeconfigValidity is the validity of the client certificate in a
// kubeconfig that expires first.
func kubeconfigValidity(data []byte) (validity, bool) {
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return validity{}, false
	}
	var first validity
	for _, a := range cfg.AuthInfos {
		first = earlier(first, a.ClientCertificateData)
	}
	return first, !first.NotAfter.IsZero()
}

// talosconfigValidity is the validity of the client certificate in a
// talosconfig (contexts.<name>.crt, base64 PEM) that expires first.
func talosconfigValidity(data []byte) (validity, bool) {
	var cfg struct {
		Contexts map[string]struct {
			Crt string `yaml:"crt"`
		} `yaml:"contexts"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return validity{}, false
	}
	var first validity
	for _, ctx := range cfg.Contexts {
		raw, err := base64.StdEncoding.DecodeString(ctx.Crt)
		if err != nil {
			continue
		}
		first = earlier(first, raw)
	}
	return first, !first.NotAfter.IsZero()
}

// earlier is whichever of cur and the PEM certificate expires first; cur when
// the PEM does not parse.
func earlier(cur validity, pemData []byte) validity {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return cur
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return cur
	}
	if cur.NotAfter.IsZero() || cert.NotAfter.Before(cur.NotAfter) {
		return validity{NotBefore: cert.NotBefore, NotAfter: cert.NotAfter}
	}
	return cur
}

package controller

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// The TalosNode's day-2 half: content-addressed config Secrets and their
// collection (D8), the dry-run drift check (D8), version skew (D24), and the
// tighter poll while a rollout runs (D7).

// +kubebuilder:rbac:groups="",resources=secrets,verbs=delete,namespace=pk-metal
// +kubebuilder:rbac:groups=metal.pk.io,resources=hostoperations,verbs=get;list;watch
// +kubebuilder:rbac:groups=os.pk.io,resources=talosrollouts,verbs=get;list;watch

const (
	// labelConfigNode marks a rendered config Secret with its TalosNode.
	labelConfigNode = "os.pk.io/node"
	// annotationConfigSecretDigest carries the full digest of a config Secret.
	annotationConfigSecretDigest = "os.pk.io/config-digest"

	defaultDriftInterval = 10 * time.Minute
	// rollingDriftInterval is the drift cadence while a rollout of the
	// node's cluster runs: a step is planned from it.
	rollingDriftInterval = 30 * time.Second
	// driftSummaryKeys bounds how many changed keys the summary names.
	driftSummaryKeys = 8
)

// configSecretName is the content-addressed name of a node's config Secret:
// the node and the first 12 hex digits of the config's sha256.
func configSecretName(node string, d digest.Digest) string {
	return "machineconfig-" + node + "-" + d.Encoded()[:12]
}

// configSecretNameFor is configSecretName for a digest in its string form;
// "" when it does not parse.
func configSecretNameFor(node, d string) string {
	parsed, err := digest.Parse(d)
	if err != nil {
		return ""
	}
	return configSecretName(node, parsed)
}

// writeConfigSecret creates the content-addressed Secret for a render, or
// finds it already there. It is immutable: a Secret of that name holding other
// bytes is an error, never an overwrite.
func (r *TalosNodeReconciler) writeConfigSecret(ctx context.Context, node *osv1alpha1.TalosNode,
	sum digest.Digest, machineConfig []byte) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: r.Namespace, Name: configSecretName(node.Name, sum)}
	err := r.Get(ctx, key, secret)
	if err == nil {
		if !bytes.Equal(secret.Data[asset.ConfigFilename], machineConfig) {
			return nil, fmt.Errorf("config Secret %s holds other bytes than the render it is named for", key)
		}
		return secret, nil
	}
	if client.IgnoreNotFound(err) != nil {
		return nil, err
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   key.Namespace,
			Name:        key.Name,
			Labels:      map[string]string{labelConfigNode: node.Name},
			Annotations: map[string]string{annotationConfigSecretDigest: sum.String()},
		},
		Immutable: ptr.To(true),
		Data:      map[string][]byte{asset.ConfigFilename: machineConfig},
	}
	if err := controllerutil.SetControllerReference(node, secret, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, secret); err != nil {
		// A pass that ran on a stale cache may find its own earlier write.
		if client.IgnoreAlreadyExists(err) == nil {
			return nil, fmt.Errorf("write config secret %s: raced another write; retrying", key)
		}
		return nil, fmt.Errorf("write config secret %s: %w", key, err)
	}
	logf.FromContext(ctx).Info("rendered machine config", "node", node.Name, "secret", secret.Name, "digest", sum.String())
	return secret, nil
}

// collectConfigSecrets deletes the node's config Secrets nothing needs any
// more. Kept: the current render, the config the machine was last seen to
// run, and any config an unfinished operation or rollout plan names, so that
// an approved apply always finds the bytes it was approved for.
func (r *TalosNodeReconciler) collectConfigSecrets(ctx context.Context, node *osv1alpha1.TalosNode) error {
	if node.Status.ConfigSecretRef == nil {
		return nil
	}
	keep := map[string]bool{node.Status.ConfigSecretRef.Name: true}
	if n := configSecretNameFor(node.Name, node.Status.AppliedConfigDigest); n != "" {
		keep[n] = true
	}
	ops := &metalv1alpha1.HostOperationList{}
	if err := r.List(ctx, ops); err != nil {
		return err
	}
	for i := range ops.Items {
		op := &ops.Items[i]
		if opFinished(op) {
			continue
		}
		for _, a := range op.Spec.Actions {
			if ac := a.TalosApplyConfig; ac != nil && ac.NodeRef.Name == node.Name {
				keep[ac.ConfigSecretRef.Name] = true
			}
		}
	}
	rollouts := &osv1alpha1.TalosRolloutList{}
	if err := r.List(ctx, rollouts); err != nil {
		return err
	}
	for i := range rollouts.Items {
		ro := &rollouts.Items[i]
		if ro.Spec.ClusterRef.Name != node.Spec.ClusterRef.Name || rolloutFinished(ro) {
			continue
		}
		for _, st := range ro.Status.Plan {
			if st.Node == node.Name && st.ConfigSecret != "" {
				keep[st.ConfigSecret] = true
			}
		}
	}

	secrets := &corev1.SecretList{}
	if err := r.List(ctx, secrets, client.InNamespace(r.Namespace), client.MatchingLabels{labelConfigNode: node.Name}); err != nil {
		return err
	}
	candidates := secrets.Items
	// The single, rewritten Secret of earlier operator versions.
	legacy := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: "machineconfig-" + node.Name}, legacy); err == nil {
		candidates = append(candidates, *legacy)
	} else if client.IgnoreNotFound(err) != nil {
		return err
	}
	for i := range candidates {
		s := &candidates[i]
		if keep[s.Name] || !metav1.IsControlledBy(s, node) {
			continue
		}
		if err := r.Delete(ctx, s, client.Preconditions{UID: &s.UID}); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("collect config secret %s: %w", s.Name, err)
		}
		logf.FromContext(ctx).Info("collected config secret", "node", node.Name, "secret", s.Name)
	}
	return nil
}

// observeVersion sets VersionCurrent from the version the machine reports.
func (r *TalosNodeReconciler) observeVersion(node *osv1alpha1.TalosNode, cluster *osv1alpha1.TalosCluster) {
	if cluster == nil || node.Status.Version == "" {
		return
	}
	set := func(s metav1.ConditionStatus, reason, msg string) {
		setCondition(&node.Status.Conditions, node.Generation, osv1alpha1.ConditionVersionCurrent, s, reason, msg)
	}
	want := cluster.Spec.TalosVersion
	if node.Status.Version == want {
		set(metav1.ConditionTrue, osv1alpha1.ReasonVersionCurrent, "runs Talos "+want)
		return
	}
	set(metav1.ConditionFalse, osv1alpha1.ReasonVersionSkew, fmt.Sprintf(
		"runs Talos %s, the cluster asks for %s; its config is rendered for %s until a TalosUpgrade rollout upgrades it",
		node.Status.Version, want, cmp.Or(node.Status.RenderedFor, want)))
}

// checkDrift dry-runs the current render against the machine, when one is
// due, and sets ConfigInSync. A dry run changes nothing, so it needs no
// approval. A machine that reports no change runs this render, whatever was
// applied before: that is how AppliedConfigDigest is learned, and how a
// change made behind the operator's back (talosctl apply-config) shows up.
func (r *TalosNodeReconciler) checkDrift(ctx context.Context, node *osv1alpha1.TalosNode, tcfg []byte,
	endpoint string, rendered []byte, rolling bool) {
	if rendered == nil || tcfg == nil {
		return
	}
	cur := node.Status.ConfigDigest
	interval := cmp.Or(r.DriftInterval, defaultDriftInterval)
	if rolling {
		interval = min(interval, rollingDriftInterval)
	}
	if d := node.Status.Drift; d != nil && d.ConfigDigest == cur && time.Since(d.ObservedAt.Time) < interval {
		return
	}
	set := func(s metav1.ConditionStatus, reason, msg string) {
		setCondition(&node.Status.Conditions, node.Generation, osv1alpha1.ConditionConfigInSync, s, reason, msg)
	}
	var res talos.ApplyResult
	err := talos.With(ctx, r.Dial, talos.Target{Endpoint: endpoint, Talosconfig: tcfg}, func(c talos.Client) error {
		var aerr error
		res, aerr = c.ApplyConfiguration(ctx, rendered, talos.ApplyOptions{DryRun: true})
		return aerr
	})
	if err != nil {
		set(metav1.ConditionUnknown, osv1alpha1.ReasonDriftUnknown, "dry-run apply failed: "+err.Error())
		return
	}
	changed, summary := summarizeDryRun(res.Details)
	drift := &osv1alpha1.ConfigDrift{ConfigDigest: cur, Changed: changed, Summary: summary, ObservedAt: metav1.Now()}
	if changed {
		drift.RebootRequired = rebootRequired(res)
	}
	node.Status.Drift = drift
	if !changed {
		node.Status.AppliedConfigDigest = cur
		set(metav1.ConditionTrue, osv1alpha1.ReasonInSync, "the machine runs the rendered config "+shortDigest(cur))
		return
	}
	how := "applies without a reboot"
	if drift.RebootRequired {
		how = "needs a reboot to apply"
	}
	set(metav1.ConditionFalse, osv1alpha1.ReasonConfigDrift, fmt.Sprintf(
		"the machine runs another config than the render %s (%s); it %s. A Config rollout is proposed: pk-metal rollout list",
		shortDigest(cur), summary, how))
}

// summarizeDryRun reads a dry-run report: whether the config differs, and a
// summary that counts the changed lines and names the keys they touch,
// without their values (a machine config diff carries secrets).
func summarizeDryRun(details string) (changed bool, summary string) {
	diff := details
	if _, after, ok := strings.Cut(details, "Config diff:"); ok {
		diff = after
	}
	diff = strings.TrimSpace(diff)
	if diff == "" || strings.EqualFold(diff, "No changes.") {
		return false, "no changes"
	}
	var added, removed int
	var keys []string
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		default:
			continue
		}
		key, _, _ := strings.Cut(strings.TrimSpace(line[1:]), ":")
		key = strings.TrimSpace(strings.TrimPrefix(key, "- "))
		if key != "" && len(keys) < driftSummaryKeys && !slices.Contains(keys, key) && !strings.ContainsAny(key, " \t") {
			keys = append(keys, key)
		}
	}
	if added == 0 && removed == 0 {
		return true, "the node reports a change"
	}
	summary = fmt.Sprintf("%d lines added, %d removed", added, removed)
	if len(keys) > 0 {
		summary += "; keys: " + strings.Join(keys, ", ")
	}
	return true, summary
}

// rebootRequired reads whether the node needs a reboot for a config: the mode
// it resolved an automatic apply to, or failing that its own words. When it
// says neither, a reboot is assumed: draining a node that did not need it
// costs less than surprising one that did.
func rebootRequired(res talos.ApplyResult) bool {
	switch res.Mode {
	case talos.ApplyNoReboot:
		return false
	case talos.ApplyReboot:
		return true
	}
	d := strings.ToLower(res.Details)
	switch {
	case strings.Contains(d, "without a reboot"):
		return false
	default:
		return true
	}
}

func shortDigest(d string) string {
	if parsed, err := digest.Parse(d); err == nil {
		return parsed.Encoded()[:12]
	}
	return d
}

// rolloutActive reports whether an approved rollout of the cluster runs.
func (r *TalosNodeReconciler) rolloutActive(ctx context.Context, cluster string) (bool, error) {
	rollouts := &osv1alpha1.TalosRolloutList{}
	if err := r.List(ctx, rollouts); err != nil {
		return false, err
	}
	for i := range rollouts.Items {
		ro := &rollouts.Items[i]
		if ro.Spec.ClusterRef.Name == cluster && ro.Spec.Approval.ApprovedBy != "" && !rolloutFinished(ro) {
			return true, nil
		}
	}
	return false, nil
}

// nodesForRollout re-enters a rollout's nodes when it moves: an approved
// rollout tightens their poll, and a finished one releases their configs.
func (r *TalosNodeReconciler) nodesForRollout(ctx context.Context, obj client.Object) []ctrl.Request {
	ro := obj.(*osv1alpha1.TalosRollout)
	return r.nodesForCluster(ctx, &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: ro.Spec.ClusterRef.Name}})
}

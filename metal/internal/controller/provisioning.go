package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/boot"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// The provisioning lane: a bound claim whose OS intent names a rendered node is
// proposed ONE boot operation that mounts the installer and the node's config
// medium, points the next boot at them, powers the machine and holds the media
// until the node checks in. Deleting the claim releases the machine through an
// operator-created power-off. Both live on the claim reconciler: the claim is
// what both decisions hang off, and its status has a single writer.
const (
	// releaseFinalizer holds a deleted claim until its machine is handed back.
	releaseFinalizer = "metal.pk.io/release"

	// Labels joining the operator's operations back to the claim they serve —
	// operations live in the operator's namespace, claims wherever they are.
	labelClaimNamespace = "metal.pk.io/claim-namespace"
	labelClaimName      = "metal.pk.io/claim-name"
	labelClaimUID       = "metal.pk.io/claim-uid"
	labelPurpose        = "metal.pk.io/purpose"
	purposeProvision    = "provision"
	purposeRelease      = "release"

	// annotationConfigDigest on a boot operation names the rendered config
	// it boots, so its success can be recorded with the digest.
	annotationConfigDigest = "metal.pk.io/config-digest"

	// labelProvisioning marks BootAssets the lane created for itself.
	labelProvisioning = "metal.pk.io/provisioning"

	// Operation reasons — the Approval(<reason>) domain on the Host.
	reasonProvisioning = "Provisioning"
	reasonRelease      = "Release"

	// releaseTTL keeps a release operation as a record for a day; nothing
	// guards on its existence once the claim is gone.
	releaseTTL = int32(24 * 60 * 60)

	// configDigestOf is the BootAsset artifact annotation naming the render
	// the artifact was packed from.
	configDigestOf = configDigestAnnotation
)

// provisionPlan is everything one boot operation commits to. Its hash names the
// operation, so the same plan never spawns twice and a changed render, image or
// endpoint shape is a new plan.
type provisionPlan struct {
	ConfigDigest   string   `json:"configDigest"`
	ArtifactDigest string   `json:"artifactDigest"`
	Installer      string   `json:"installer,omitempty"`
	Slots          []string `json:"slots"`
	Target         string   `json:"target,omitempty"`
	Mode           string   `json:"mode,omitempty"`
}

// provision drives the claim's Provisioned condition and, when everything a
// boot needs is in place, proposes the boot operation. hostName is the bound host.
func (r *HostClaimReconciler) provision(ctx context.Context, claim *metalv1alpha1.HostClaim, hostName string) error {
	set := func(s metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type: metalv1alpha1.ConditionProvisioned, Status: s, Reason: reason, Message: msg,
			ObservedGeneration: claim.Generation,
		})
	}
	hold := func(reason, msg string) error {
		set(metav1.ConditionFalse, reason, msg)
		return nil
	}
	ops, err := r.claimOps(ctx, claim, purposeProvision)
	if err != nil {
		return err
	}
	if claim.Spec.OS == nil {
		// Intent dropped: every boot not finished goes with it — a proposal
		// nobody approved yet, and a boot still running, which stops between
		// steps. The node a claim boots changes only through here, so no boot
		// for the old node survives into the new intent.
		meta.RemoveStatusCondition(&claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
		for i := range ops {
			if opFinished(&ops[i]) || !ops[i].DeletionTimestamp.IsZero() {
				continue
			}
			logf.FromContext(ctx).Info("stopped boot operation of a dropped OS intent", "operation", ops[i].Namespace+"/"+ops[i].Name)
			if err := r.Delete(ctx, &ops[i]); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
		return nil
	}

	host := &metalv1alpha1.Host{}
	if err := r.Get(ctx, client.ObjectKey{Name: hostName}, host); err != nil {
		return client.IgnoreNotFound(err)
	}
	node := &osv1alpha1.TalosNode{}
	if err := r.Get(ctx, client.ObjectKey{Name: claim.Spec.OS.NodeRef.Name}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return hold(metalv1alpha1.ReasonAwaitingRender, fmt.Sprintf("TalosNode %s not found", claim.Spec.OS.NodeRef.Name))
		}
		return err
	}
	// The hand-off record on the claim says the install is done: the node may
	// simply be down now, and that is not a reason to install it again. A
	// record of any other host or node is no record of this one.
	if h := claim.Status.HandOff; h != nil && h.Host == hostName && h.Node == node.Name {
		at := h.At.UTC().Format(time.RFC3339)
		msg := fmt.Sprintf("host %s runs TalosNode %s (installed %s)", hostName, node.Name, at)
		if !nodeCheckedIn(node) {
			msg = fmt.Sprintf("host %s was installed with TalosNode %s at %s; the node does not answer now, which does not start a new install",
				hostName, node.Name, at)
		}
		set(metav1.ConditionTrue, metalv1alpha1.ReasonHandedOff, msg)
		return r.withdraw(ctx, ops, "")
	}
	if nodeCheckedIn(node) {
		recordHandOff(claim, hostName, node.Name, node.Status.ConfigDigest)
		set(metav1.ConditionTrue, metalv1alpha1.ReasonHandedOff, fmt.Sprintf("host %s runs TalosNode %s", hostName, node.Name))
		return r.withdraw(ctx, ops, "")
	}
	// A claim from before the record existed: a succeeded boot operation of
	// this node onto this host is the hand-off, and becomes the record.
	for i := range ops {
		if opSucceeded(&ops[i]) && bootsNode(&ops[i], hostName, node.Name) {
			recordHandOff(claim, hostName, node.Name, ops[i].Annotations[annotationConfigDigest])
			set(metav1.ConditionTrue, metalv1alpha1.ReasonHandedOff,
				fmt.Sprintf("operation %s/%s booted host %s", ops[i].Namespace, ops[i].Name, hostName))
			return nil
		}
	}
	if !meta.IsStatusConditionTrue(host.Status.Conditions, metalv1alpha1.ConditionVerified) {
		return hold(metalv1alpha1.ReasonAwaitingVerification, fmt.Sprintf("host %s is not verified", hostName))
	}

	cluster := &osv1alpha1.TalosCluster{}
	if err := r.Get(ctx, client.ObjectKey{Name: node.Spec.ClusterRef.Name}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return hold(metalv1alpha1.ReasonAwaitingRender, fmt.Sprintf("TalosCluster %s not found", node.Spec.ClusterRef.Name))
		}
		return err
	}
	if !meta.IsStatusConditionTrue(node.Status.Conditions, osv1alpha1.ConditionRendered) || node.Status.ConfigDigest == "" {
		return hold(metalv1alpha1.ReasonAwaitingRender, fmt.Sprintf("TalosNode %s has no rendered config yet", node.Name))
	}

	shape, err := r.endpointShape(ctx, host)
	if err != nil {
		return err
	}
	if len(shape.insertable) == 0 {
		return hold(metalv1alpha1.ReasonNoBootableMedia, fmt.Sprintf("host %s advertises no insertable media slot", hostName))
	}
	bootSlot, configSlot := shape.mediaSlots()
	single := configSlot == ""
	// One slot can carry the config but not the installer next to it. That
	// only boots a machine that already runs Talos from its disk (it answers,
	// blank, in maintenance mode); a fresh one needs an install image with the
	// config embedded, which this operator does not build.
	if single && !meta.IsStatusConditionTrue(node.Status.Conditions, osv1alpha1.ConditionMachineReachable) {
		return hold(metalv1alpha1.ReasonSingleMediumUnsupported, fmt.Sprintf(
			"host %s cannot mount an installer next to the config (boot slots %v, insertable slots %v) and has no Talos to boot from disk — an embedded-config install image is not supported",
			hostName, shape.bootSlots, shape.insertable))
	}

	config, err := r.ensureConfigAsset(ctx, node)
	if err != nil {
		return err
	}
	if msg := configAssetPending(ctx, r.Client, config, node); msg != "" {
		return hold(metalv1alpha1.ReasonAwaitingBootAsset, msg)
	}
	plan := provisionPlan{ConfigDigest: node.Status.ConfigDigest, ArtifactDigest: config.Status.Digest}
	var installer *osv1alpha1.BootAsset
	if !single {
		if installer, err = r.ensureInstallerAsset(ctx, node, cluster); err != nil {
			return err
		}
		if !meta.IsStatusConditionTrue(installer.Status.Conditions, osv1alpha1.ConditionReady) || installer.Status.URL == "" {
			return hold(metalv1alpha1.ReasonAwaitingBootAsset, fmt.Sprintf("installer image %s is not Ready", installer.Name))
		}
		target, err := boot.PickTarget(shape.targets)
		if err != nil {
			return hold(metalv1alpha1.ReasonNoBootableMedia, err.Error())
		}
		plan.Installer, plan.Slots, plan.Target = installer.Status.URL, []string{bootSlot, configSlot}, target
		plan.Mode = overrideMode(shape.mode)
	} else {
		plan.Slots = shape.insertable[:1]
	}

	name := fmt.Sprintf("provision-%s-%s", hostName, hash8(planHash(plan)))
	if err := r.withdraw(ctx, ops, name); err != nil {
		return err
	}
	var current *metalv1alpha1.HostOperation
	for i := range ops {
		switch {
		case ops[i].Name == name:
			current = &ops[i]
		case opActive(&ops[i]) && ops[i].DeletionTimestamp.IsZero():
			// An older plan already running finishes first.
			return hold(metalv1alpha1.ReasonAwaitingOperation,
				fmt.Sprintf("operation %s/%s for an earlier plan is running", ops[i].Namespace, ops[i].Name))
		}
	}
	if current == nil {
		op := r.provisionOp(claim, hostName, name, plan, node, config, installer)
		if err := r.Create(ctx, op); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		logf.FromContext(ctx).Info("proposed boot operation", "operation", op.Namespace+"/"+op.Name,
			"host", hostName, "node", node.Name, "approvalRequired", *op.Spec.Approval.Required)
		return hold(metalv1alpha1.ReasonAwaitingOperation, fmt.Sprintf("boot operation %s/%s proposed", op.Namespace, op.Name))
	}

	ex := meta.FindStatusCondition(current.Status.Conditions, metalv1alpha1.ConditionExecuted)
	ref := current.Namespace + "/" + current.Name
	switch {
	case opAwaitsApproval(current):
		return hold(metalv1alpha1.ReasonAwaitingOperation, "boot operation "+ref+" awaits approval")
	case ex != nil && ex.Status == metav1.ConditionFalse:
		return hold(metalv1alpha1.ReasonOperationFailed, fmt.Sprintf(
			"boot operation %s failed: %s — delete it to try again", ref, ex.Message))
	case ex != nil && ex.Message != "":
		return hold(metalv1alpha1.ReasonAwaitingOperation, "boot operation "+ref+": "+ex.Message)
	}
	return hold(metalv1alpha1.ReasonAwaitingOperation, "boot operation "+ref+" in flight")
}

// recordHandOff writes the hand-off record onto the claim, once: a record
// for this host and node already there keeps its time.
func recordHandOff(claim *metalv1alpha1.HostClaim, hostName, nodeName, digest string) {
	if h := claim.Status.HandOff; h != nil && h.Host == hostName && h.Node == nodeName {
		return
	}
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{
		Host: hostName, Node: nodeName, ConfigDigest: digest, At: metav1.Now(),
	}
}

// overrideMode is the override mode a boot asks for, given the one the system
// serves. The media boot UEFI: a system serving another mode would take the
// override as a legacy boot, so the mode goes along; one serving UEFI already
// needs nothing, and one serving none is not asked for it (it may refuse).
func overrideMode(served string) string {
	if served != "" && served != "UEFI" {
		return "UEFI"
	}
	return ""
}

// bootsNode: the operation acts on the host and waits for the node to check in.
func bootsNode(op *metalv1alpha1.HostOperation, hostName, nodeName string) bool {
	if op.Spec.HostRef.Name != hostName {
		return false
	}
	return slices.ContainsFunc(op.Spec.Actions, func(a metalv1alpha1.HostOperationAction) bool {
		return a.AwaitCheckIn != nil && a.AwaitCheckIn.NodeRef.Name == nodeName
	})
}

// provisionOp builds the boot operation for a plan.
func (r *HostClaimReconciler) provisionOp(claim *metalv1alpha1.HostClaim, hostName, name string, plan provisionPlan,
	node *osv1alpha1.TalosNode, config, installer *osv1alpha1.BootAsset) *metalv1alpha1.HostOperation {
	configSlot := plan.Slots[len(plan.Slots)-1]
	var actions []metalv1alpha1.HostOperationAction
	for _, s := range plan.Slots {
		actions = append(actions, metalv1alpha1.HostOperationAction{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: s}})
	}
	if installer != nil {
		actions = append(actions, metalv1alpha1.HostOperationAction{InsertMedia: &metalv1alpha1.InsertMediaAction{
			MediaID: plan.Slots[0], BootAssetRef: &metalv1alpha1.Reference{Name: installer.Name},
		}})
	}
	actions = append(actions, metalv1alpha1.HostOperationAction{InsertMedia: &metalv1alpha1.InsertMediaAction{
		MediaID: configSlot, BootAssetRef: &metalv1alpha1.Reference{Name: config.Name}, Digest: plan.ArtifactDigest,
	}})
	if plan.Target != "" {
		actions = append(actions, metalv1alpha1.HostOperationAction{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{
			Target: plan.Target, Once: ptr.To(true), Mode: plan.Mode,
		}})
	}
	actions = append(actions,
		metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
		metalv1alpha1.HostOperationAction{AwaitCheckIn: &metalv1alpha1.AwaitCheckInAction{
			NodeRef: metalv1alpha1.Reference{Name: node.Name}, TimeoutSeconds: r.checkInTimeoutSeconds(),
		}},
	)
	// The media leave once the machine runs from disk: config first, it is
	// the one carrying secrets.
	for i := len(plan.Slots) - 1; i >= 0; i-- {
		actions = append(actions, metalv1alpha1.HostOperationAction{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: plan.Slots[i]}})
	}
	required := claim.Spec.Provisioning == nil || claim.Spec.Provisioning.Approval != metalv1alpha1.ProvisioningApprovalAutomatic
	return &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: r.OperationsNamespace,
			Labels:      claimLabels(claim, purposeProvision),
			Annotations: map[string]string{annotationConfigDigest: plan.ConfigDigest},
		},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:    metalv1alpha1.Reference{Name: hostName},
			ProposedBy: r.Proposer,
			Reason:     reasonProvisioning,
			PlanHash:   planHash(plan),
			Actions:    actions,
			Approval:   metalv1alpha1.OperationApproval{Required: ptr.To(required)},
		},
	}
}

func (r *HostClaimReconciler) checkInTimeoutSeconds() int32 {
	if r.CheckInTimeoutSeconds > 0 {
		return r.CheckInTimeoutSeconds
	}
	return int32(defaultCheckInTimeout.Seconds())
}

func claimLabels(claim *metalv1alpha1.HostClaim, purpose string) map[string]string {
	return map[string]string{
		labelClaimNamespace: claim.Namespace,
		labelClaimName:      claim.Name,
		labelClaimUID:       string(claim.UID),
		labelPurpose:        purpose,
	}
}

// claimOps lists the operator's operations serving this claim for one purpose.
func (r *HostClaimReconciler) claimOps(ctx context.Context, claim *metalv1alpha1.HostClaim, purpose string) ([]metalv1alpha1.HostOperation, error) {
	ops := &metalv1alpha1.HostOperationList{}
	if err := r.List(ctx, ops, client.InNamespace(r.OperationsNamespace), client.MatchingLabels{
		labelClaimUID: string(claim.UID), labelPurpose: purpose,
	}); err != nil {
		return nil, err
	}
	return ops.Items, nil
}

// withdraw retracts the claim's proposals that never got their gate, except
// keep — a superseded plan is not left in front of the approver.
func (r *HostClaimReconciler) withdraw(ctx context.Context, ops []metalv1alpha1.HostOperation, keep string) error {
	for i := range ops {
		if ops[i].Name == keep || !opAwaitsApproval(&ops[i]) {
			continue
		}
		logf.FromContext(ctx).Info("withdrew obsolete boot proposal", "operation", ops[i].Namespace+"/"+ops[i].Name)
		if err := r.Delete(ctx, &ops[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// endpointView is what a boot plan reads off the host's endpoint.
type endpointView struct {
	// insertable lists every slot with an insert mechanism, best boot slot
	// first (bmc.InsertableSlots) — what a release empties.
	insertable []string
	// bootSlots are the insertable slots an ISO boots from: optical ones,
	// then untyped ones (a slot that advertises nothing may still be one),
	// in the endpoint's order. A slot typed as something else (a USB stick)
	// is left out: the installer boots from one of these, the config medium
	// may take any other insertable slot.
	bootSlots []string
	// targets are the boot override targets the path's system allows.
	targets []string
	// mode is the override mode the path's system serves.
	mode string
}

// endpointShape reads the media slots and the boot facts of the host's first
// reachable path.
func (r *HostClaimReconciler) endpointShape(ctx context.Context, host *metalv1alpha1.Host) (endpointView, error) {
	for _, path := range host.Status.ManagedBy {
		if !path.Reachable {
			continue
		}
		ep := &metalv1alpha1.ManagementEndpoint{}
		if err := r.Get(ctx, client.ObjectKey{Name: path.EndpointRef.Name}, ep); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return endpointView{}, err
		}
		v := endpointView{}
		caps := bmc.Capabilities{MediaDialect: map[string]bmc.MediaDialect{}}
		if c := ep.Status.Capabilities; c != nil {
			for id, d := range c.MediaDialects {
				caps.MediaDialect[id] = bmc.MediaDialect(d)
			}
		}
		media := make([]bmc.VirtualMedia, 0, len(ep.Status.VirtualMedia))
		types := map[string][]string{}
		for _, m := range ep.Status.VirtualMedia {
			media = append(media, bmc.VirtualMedia{ID: m.ID, MediaTypes: m.MediaTypes})
			types[m.ID] = m.MediaTypes
		}
		v.insertable = bmc.InsertableSlots(media, caps)
		for _, id := range v.insertable {
			if t := types[id]; len(t) == 0 || slices.Contains(t, "CD") || slices.Contains(t, "DVD") {
				v.bootSlots = append(v.bootSlots, id)
			}
		}
		for _, sys := range ep.Status.Systems {
			if sys.ID == path.SystemID {
				v.targets, v.mode = sys.BootTargets, sys.BootOverrideMode
			}
		}
		return v, nil
	}
	return endpointView{}, nil
}

// mediaSlots picks the slots of a two-medium boot: the installer goes on the
// best boot slot, the config medium on the next insertable slot. The config
// medium is only read, never booted: Talos finds it by its volume label on any
// block device, so a USB-stick slot carries it as well as an optical one. Both
// are empty when the endpoint cannot hold an installer next to a config.
func (v endpointView) mediaSlots() (boot, config string) {
	if len(v.bootSlots) == 0 {
		return "", ""
	}
	for _, id := range v.insertable {
		if id != v.bootSlots[0] {
			return v.bootSlots[0], id
		}
	}
	return "", ""
}

// ensureConfigAsset finds the node's configISO BootAsset, creating one owned
// by the node when nobody authored it.
func (r *HostClaimReconciler) ensureConfigAsset(ctx context.Context, node *osv1alpha1.TalosNode) (*osv1alpha1.BootAsset, error) {
	assets := &osv1alpha1.BootAssetList{}
	if err := r.List(ctx, assets); err != nil {
		return nil, err
	}
	for i := range assets.Items {
		a := &assets.Items[i]
		if a.Spec.Strategy == osv1alpha1.StrategyConfigISO && a.Spec.NodeRef != nil && a.Spec.NodeRef.Name == node.Name &&
			a.DeletionTimestamp.IsZero() {
			return a, nil
		}
	}
	a := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-config", Labels: map[string]string{labelProvisioning: "true"}},
		Spec: osv1alpha1.BootAssetSpec{
			Strategy: osv1alpha1.StrategyConfigISO,
			NodeRef:  &osv1alpha1.Reference{Name: node.Name},
		},
	}
	if err := controllerutil.SetOwnerReference(node, a, r.Scheme()); err != nil {
		return nil, err
	}
	return a, r.createOrGet(ctx, a)
}

// bootSchematic is the schematic of the boot medium: the node's effective
// schematic plus the kernel argument that makes Talos read its configuration
// from the metal-iso volume. The argument belongs to the boot medium only; the
// installed system takes its command line from the installer image, which the
// rendered machine configuration names, so it never looks for the medium again.
func bootSchematic(node *osv1alpha1.TalosNode, cluster *osv1alpha1.TalosCluster) *osv1alpha1.Schematic {
	s := &osv1alpha1.Schematic{}
	if eff := render.EffectiveSchematic(cluster, node); eff != nil {
		s = eff.DeepCopy()
	}
	if !slices.Contains(s.Customization.ExtraKernelArgs, bootConfigKernelArg) {
		s.Customization.ExtraKernelArgs = append(s.Customization.ExtraKernelArgs, bootConfigKernelArg)
	}
	return s
}

// bootConfigKernelArg points Talos at the config ISO the operator mounts.
const bootConfigKernelArg = "talos.config=metal-iso"

// ensureInstallerAsset finds a factory BootAsset for the node's boot schematic
// and the cluster's Talos version, creating one owned by the cluster when absent.
// The factory strategy is what registers the schematic and proves the image
// exists before anyone mounts its URL.
//
// Each cluster gets its own: an asset owned by one cluster is collected with
// it, and a boot of another cluster's node that shared it would lose its
// image halfway. An asset nobody owns (authored by a person) is shared freely.
func (r *HostClaimReconciler) ensureInstallerAsset(ctx context.Context, node *osv1alpha1.TalosNode, cluster *osv1alpha1.TalosCluster) (*osv1alpha1.BootAsset, error) {
	schematic := bootSchematic(node, cluster)
	assets := &osv1alpha1.BootAssetList{}
	if err := r.List(ctx, assets); err != nil {
		return nil, err
	}
	for i := range assets.Items {
		a := &assets.Items[i]
		if a.Spec.Strategy == osv1alpha1.StrategyFactory && a.Spec.TalosVersion == cluster.Spec.TalosVersion &&
			equality.Semantic.DeepEqual(a.Spec.Schematic, schematic) && a.DeletionTimestamp.IsZero() &&
			(len(a.OwnerReferences) == 0 || ownedBy(a, cluster)) {
			return a, nil
		}
	}
	a := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{
			Name:   installerName(cluster.Name, hash8(planHash([]any{schematic, cluster.Spec.TalosVersion}))),
			Labels: map[string]string{labelProvisioning: "true"},
		},
		Spec: osv1alpha1.BootAssetSpec{
			Strategy:     osv1alpha1.StrategyFactory,
			TalosVersion: cluster.Spec.TalosVersion,
			Schematic:    schematic,
		},
	}
	if err := controllerutil.SetOwnerReference(cluster, a, r.Scheme()); err != nil {
		return nil, err
	}
	return a, r.createOrGet(ctx, a)
}

// installerName is <cluster>-installer-<hash>, the cluster part cut so the
// name stays a valid object name.
func installerName(cluster, hash string) string {
	const maxPrefix = 253 - len("-installer-") - 8
	if len(cluster) > maxPrefix {
		cluster = strings.TrimRight(cluster[:maxPrefix], "-.")
	}
	return cluster + "-installer-" + hash
}

// ownedBy: one of the object's owner references is the owner.
func ownedBy(obj metav1.Object, owner metav1.Object) bool {
	return slices.ContainsFunc(obj.GetOwnerReferences(), func(ref metav1.OwnerReference) bool {
		return ref.UID == owner.GetUID()
	})
}

func (r *HostClaimReconciler) createOrGet(ctx context.Context, a *osv1alpha1.BootAsset) error {
	err := r.Create(ctx, a)
	if apierrors.IsAlreadyExists(err) {
		return r.Get(ctx, client.ObjectKeyFromObject(a), a)
	}
	if err == nil {
		logf.FromContext(ctx).Info("created boot asset", "bootAsset", a.Name, "strategy", a.Spec.Strategy)
	}
	return err
}

// configAssetPending says why the config medium cannot be mounted yet, or ""
// when it is built from the node's current render.
func configAssetPending(ctx context.Context, c client.Client, a *osv1alpha1.BootAsset, node *osv1alpha1.TalosNode) string {
	if !meta.IsStatusConditionTrue(a.Status.Conditions, osv1alpha1.ConditionReady) || a.Status.Digest == "" ||
		a.Status.ArtifactSecretRef == nil {
		return fmt.Sprintf("config medium %s is not built yet", a.Name)
	}
	ref := a.Status.ArtifactSecretRef
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, sec); err != nil {
		return fmt.Sprintf("config medium %s: artifact %s/%s unreadable: %v", a.Name, ref.Namespace, ref.Name, err)
	}
	if got := sec.Annotations[configDigestOf]; got != node.Status.ConfigDigest {
		return fmt.Sprintf("config medium %s holds render %s, node is at %s", a.Name, short(got), short(node.Status.ConfigDigest))
	}
	return ""
}

func short(digest string) string {
	if d := strings.TrimPrefix(digest, "sha256:"); len(d) >= 8 {
		return d[:8]
	}
	return digest
}

// release hands the claim's machines back: a machine a boot operation of the
// claim actually ran on is powered off and its media ejected by an
// operator-created operation, unfinished boot operations are withdrawn, and
// only then are the bind and the hand-off cleared. A machine no boot ever
// touched is unbound as it is — powering it off would be the first thing the
// operator ever did to it. Returns whether everything is released. Wiping the
// disks is not part of it.
func (r *HostClaimReconciler) release(ctx context.Context, claim *metalv1alpha1.HostClaim, bound []metalv1alpha1.Host) (bool, error) {
	set := func(reason, msg string) {
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type: metalv1alpha1.ConditionProvisioned, Status: metav1.ConditionFalse, Reason: reason, Message: msg,
			ObservedGeneration: claim.Generation,
		})
	}
	provisionOps, err := r.claimOps(ctx, claim, purposeProvision)
	if err != nil {
		return false, err
	}
	releaseOps, err := r.claimOps(ctx, claim, purposeRelease)
	if err != nil {
		return false, err
	}

	done := true
	for i := range bound {
		host := &bound[i]
		if ref := host.Spec.ClaimRef; ref != nil && ref.UID != "" && ref.UID != claim.UID {
			continue
		}
		name := fmt.Sprintf("release-%s-%s", host.Name, short8(string(claim.UID)))
		var op *metalv1alpha1.HostOperation
		for j := range releaseOps {
			if releaseOps[j].Name == name {
				op = &releaseOps[j]
			}
		}
		// The decision to power off is made once, while the boot records are
		// still there to read; the release operation then carries it.
		if op != nil || bootRan(provisionOps, host.Name) || handedOff(claim, host.Name) {
			if op == nil {
				op = r.releaseOp(ctx, claim, host, name)
				if err := r.Create(ctx, op); err != nil && !apierrors.IsAlreadyExists(err) {
					return false, err
				}
				logf.FromContext(ctx).Info("proposed release operation", "operation", op.Namespace+"/"+op.Name, "host", host.Name)
				set(metalv1alpha1.ReasonReleasing, fmt.Sprintf("powering off host %s (operation %s/%s)", host.Name, op.Namespace, op.Name))
				done = false
				continue
			}
			ex := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
			switch {
			case ex != nil && ex.Status == metav1.ConditionFalse:
				set(metalv1alpha1.ReasonReleaseFailed, fmt.Sprintf(
					"release operation %s/%s failed: %s — delete it to retry", op.Namespace, op.Name, ex.Message))
				done = false
				continue
			case ex == nil || ex.Status != metav1.ConditionTrue:
				set(metalv1alpha1.ReasonReleasing, fmt.Sprintf("powering off host %s (operation %s/%s)", host.Name, op.Namespace, op.Name))
				done = false
				continue
			}
		}
		if err := r.releaseAll(ctx, []metalv1alpha1.Host{*host}, ""); err != nil {
			return false, err
		}
	}
	// Running boot operations stop between steps once deleted; their grants
	// die with them. They go only after the release operations exist, so a
	// failed pass never loses the record that a boot ran.
	for i := range provisionOps {
		if !opFinished(&provisionOps[i]) && provisionOps[i].DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, &provisionOps[i]); client.IgnoreNotFound(err) != nil {
				return false, err
			}
		}
	}
	if !done {
		return false, nil
	}
	// The boot records belong to a claim that no longer exists.
	for i := range provisionOps {
		if err := r.Delete(ctx, &provisionOps[i]); client.IgnoreNotFound(err) != nil {
			return false, err
		}
	}
	return true, nil
}

// bootRan: a boot operation of the claim was taken up by the executor on the
// host — it captured its baseline, the record it writes before its first
// mutating call.
func bootRan(ops []metalv1alpha1.HostOperation, hostName string) bool {
	return slices.ContainsFunc(ops, func(op metalv1alpha1.HostOperation) bool {
		return op.Spec.HostRef.Name == hostName && op.Status.Baseline != nil
	})
}

// handedOff: the claim's hand-off record names the host — an install ran on
// it even when its boot operation was deleted since.
func handedOff(claim *metalv1alpha1.HostClaim, hostName string) bool {
	return claim.Status.HandOff != nil && claim.Status.HandOff.Host == hostName
}

// releaseOp powers the machine off and empties every insertable slot. It is
// created pre-cleared: deleting the claim was the decision, and admission lets
// only the operator create such an operation.
func (r *HostClaimReconciler) releaseOp(ctx context.Context, claim *metalv1alpha1.HostClaim, host *metalv1alpha1.Host, name string) *metalv1alpha1.HostOperation {
	actions := []metalv1alpha1.HostOperationAction{{PowerOff: &metalv1alpha1.PowerOffAction{}}}
	if shape, err := r.endpointShape(ctx, host); err == nil {
		for _, s := range shape.insertable {
			actions = append(actions, metalv1alpha1.HostOperationAction{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: s}})
		}
	}
	return &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: r.OperationsNamespace,
			Labels: claimLabels(claim, purposeRelease),
		},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:                 metalv1alpha1.Reference{Name: host.Name},
			ProposedBy:              r.Proposer,
			Reason:                  reasonRelease,
			Actions:                 actions,
			Approval:                metalv1alpha1.OperationApproval{Required: ptr.To(false)},
			TTLSecondsAfterFinished: ptr.To(releaseTTL),
		},
	}
}

func short8(s string) string {
	s = strings.ReplaceAll(s, "-", "")
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// claimsForOperation maps an operation back to the claim it serves.
func claimsForOperation(_ context.Context, obj client.Object) []ctrl.Request {
	l := obj.GetLabels()
	if l[labelClaimName] == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: l[labelClaimNamespace], Name: l[labelClaimName]}}}
}

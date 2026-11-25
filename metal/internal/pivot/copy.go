package pivot

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	pkscheme "github.com/dennisklein/platform-kit/metal/internal/scheme"
)

// Keys the operator puts on objects that carry a HostClaim's UID
// (internal/controller/provisioning.go). A claim has another UID on the
// target, so each is rewritten.
const (
	labelClaimUID             = "metal.pk.io/claim-uid"
	annotationReleasePowerOff = "metal.pk.io/release-power-off"
	// impersonatedMover is the user the move creates HostClaims as: the
	// claim policy lets only an admin name a TalosNode.
	impersonatedMover = "pk-metal-pivot"
)

// movedKind is one kind of the operator's state, in the order it is copied:
// what an object refers to is copied before it, where the references allow.
type movedKind struct {
	gvk schema.GroupVersionKind
	// core: a client-go kind; only objects in the operator namespace move.
	core bool
	// namespaced: the objects live in namespaces (all of them move).
	namespaced bool
	// skip leaves an object of this kind behind.
	skip func(*unstructured.Unstructured) bool
}

var (
	metalGV = metalv1alpha1.GroupVersion
	osGV    = osv1alpha1.GroupVersion
)

// movedKinds is the copy order. Secrets come early: an endpoint's rotated
// credentials must be in place before anything could rotate them again. A
// Secret owned by an object copied later gets its owner reference in the
// second pass, before the new manager starts.
var movedKinds = []movedKind{
	{gvk: corev1.SchemeGroupVersion.WithKind("ConfigMap"), core: true, namespaced: true, skip: func(u *unstructured.Unstructured) bool {
		return u.GetName() == "kube-root-ca.crt"
	}},
	{gvk: corev1.SchemeGroupVersion.WithKind("Secret"), core: true, namespaced: true, skip: func(u *unstructured.Unstructured) bool {
		t, _, _ := unstructured.NestedString(u.Object, "type")
		return t == string(corev1.SecretTypeServiceAccountToken) || t == string(corev1.SecretTypeBootstrapToken)
	}},
	{gvk: metalGV.WithKind("Asset")},
	{gvk: metalGV.WithKind("ManagementEndpoint")},
	{gvk: metalGV.WithKind("EndpointProfile")},
	{gvk: metalGV.WithKind("BiosProfile")},
	{gvk: metalGV.WithKind("FirmwareProfile")},
	{gvk: metalGV.WithKind("BiosSchema")},
	{gvk: metalGV.WithKind("BiosSettings")},
	{gvk: metalGV.WithKind("Host")},
	{gvk: metalGV.WithKind("HostClaim"), namespaced: true},
	{gvk: metalGV.WithKind("HostOperation"), namespaced: true},
	{gvk: osGV.WithKind("TalosCluster")},
	{gvk: osGV.WithKind("TalosNode")},
	{gvk: osGV.WithKind("BootAsset")},
	{gvk: metalGV.WithKind("MaintenanceWindow")},
}

// movedKindOf finds the moved kind an owner reference names.
func movedKindOf(apiVersion, kind string) bool {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return false
	}
	return slices.Contains(movedGroups, gv.Group) ||
		slices.ContainsFunc(movedKinds, func(k movedKind) bool { return k.gvk == gv.WithKind(kind) })
}

// Report is what a copy did, and what the verification compares against.
type Report struct {
	// Copied counts the objects per kind.
	Copied map[string]int
	// UIDs maps every copied object's source UID to its target UID.
	UIDs map[types.UID]types.UID
	// Rewritten counts the UID references rewritten to target UIDs.
	Rewritten int
	// Operations are the HostOperations (namespace/name) the copy left on
	// the target: anything else there was proposed after the move.
	Operations map[string]bool
	// HandedOff are the claims (namespace/name) whose hand-off record was
	// copied: each must settle as handed off, never install again.
	HandedOff []string
	// Pruned lists the objects removed from the target because the source
	// no longer has them.
	Pruned []string
}

// sourceObject is one object read from the source, with its kind.
type sourceObject struct {
	kind movedKind
	obj  *unstructured.Unstructured
}

// rewriter maps source UIDs to target UIDs in an object about to be written.
type rewriter struct {
	uids map[types.UID]types.UID
	// claims are the UIDs of the source's HostClaims: a reference to one of
	// them must be rewritten, a reference to any other is a record of a claim
	// that is gone on both sides and stays as it is.
	claims map[types.UID]bool
}

// rewrite returns the object as it is to be written to the target: server
// fields dropped, owner references and claim UIDs rewritten. complete is
// false when a reference could not be rewritten yet (its object is copied
// later); such a reference is left out, never written with a UID the target
// does not know, which the garbage collector would act on.
func (r *rewriter) rewrite(src *unstructured.Unstructured) (out *unstructured.Unstructured, rewritten int, complete bool) {
	out = sanitize(src)
	complete = true

	var owners []metav1.OwnerReference
	for _, ref := range src.GetOwnerReferences() {
		if uid, ok := r.uids[ref.UID]; ok {
			ref.UID = uid
			owners = append(owners, ref)
			rewritten++
			continue
		}
		if movedKindOf(ref.APIVersion, ref.Kind) {
			complete = false
		}
		// An owner the move does not carry (a cert-manager request) is
		// dropped: on the target it does not exist.
	}
	out.SetOwnerReferences(owners)

	claimUID := func(v string) (string, bool) {
		if v == "" || !r.claims[types.UID(v)] {
			return v, true
		}
		if uid, ok := r.uids[types.UID(v)]; ok {
			rewritten++
			return string(uid), true
		}
		return v, false
	}
	if labels := out.GetLabels(); labels[labelClaimUID] != "" {
		v, ok := claimUID(labels[labelClaimUID])
		labels[labelClaimUID] = v
		out.SetLabels(labels)
		complete = complete && ok
	}
	if ann := out.GetAnnotations(); ann[annotationReleasePowerOff] != "" {
		v, ok := claimUID(ann[annotationReleasePowerOff])
		ann[annotationReleasePowerOff] = v
		out.SetAnnotations(ann)
		complete = complete && ok
	}
	if v, found, _ := unstructured.NestedString(out.Object, "spec", "claimRef", "uid"); found {
		nv, ok := claimUID(v)
		_ = unstructured.SetNestedField(out.Object, nv, "spec", "claimRef", "uid")
		complete = complete && ok
	}
	return out, rewritten, complete
}

// sanitize drops what the source's API server assigned.
func sanitize(src *unstructured.Unstructured) *unstructured.Unstructured {
	out := src.DeepCopy()
	for _, f := range []string{"uid", "resourceVersion", "creationTimestamp", "managedFields", "generation",
		"deletionTimestamp", "deletionGracePeriodSeconds", "selfLink"} {
		unstructured.RemoveNestedField(out.Object, "metadata", f)
	}
	return out
}

// Copy copies the operator's state from the source to the target: first
// every object with its metadata and spec, in dependency order; then once
// more, now that every UID is known, to fill in the references the first
// pass could not; then every status. It is safe to run again: an object
// already on the target is brought up to the source's version.
func (m *Mover) Copy(ctx context.Context, plan *Plan) (*Report, error) {
	rep := &Report{Copied: map[string]int{}, UIDs: map[types.UID]types.UID{}, Operations: map[string]bool{}}
	objs, err := m.readSource(ctx)
	if err != nil {
		return nil, err
	}
	rw := &rewriter{uids: rep.UIDs, claims: map[types.UID]bool{}}
	for _, so := range objs {
		if so.kind.gvk.Kind == "HostClaim" {
			rw.claims[so.obj.GetUID()] = true
		}
	}
	if err := m.ensureNamespaces(ctx, objs); err != nil {
		return nil, err
	}
	if plan != nil && plan.Prune {
		if rep.Pruned, err = m.prune(ctx, objs); err != nil {
			return nil, err
		}
	}

	for pass := 1; pass <= 2; pass++ {
		rep.Rewritten = 0
		for _, so := range objs {
			n, err := m.ensure(ctx, so, rw)
			if err != nil {
				return nil, fmt.Errorf("copy %s %s: %w", so.kind.gvk.Kind, objectRef(so.obj), err)
			}
			rep.Rewritten += n
		}
	}
	for _, so := range objs {
		if err := m.copyStatus(ctx, so, rw); err != nil {
			return nil, fmt.Errorf("copy the status of %s %s: %w", so.kind.gvk.Kind, objectRef(so.obj), err)
		}
		rep.Copied[so.kind.gvk.Kind]++
		switch so.kind.gvk.Kind {
		case "HostOperation":
			rep.Operations[objectRef(so.obj)] = true
		case "HostClaim":
			if h, found, _ := unstructured.NestedMap(so.obj.Object, "status", "handOff"); found && len(h) > 0 {
				rep.HandedOff = append(rep.HandedOff, objectRef(so.obj))
			}
		}
	}
	m.logf("copied %s; %d UID references rewritten", copiedSummary(m.movedKinds(), rep.Copied), rep.Rewritten)
	return rep, nil
}

func copiedSummary(kinds []movedKind, copied map[string]int) string {
	var parts []string
	for _, k := range kinds {
		if n := copied[k.gvk.Kind]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k.gvk.Kind))
		}
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return fmt.Sprint(parts)
}

// readSource lists every object that moves, kind by kind in copy order and
// by name within a kind, so that two runs copy in the same order.
func (m *Mover) readSource(ctx context.Context) ([]sourceObject, error) {
	var out []sourceObject
	for _, k := range m.movedKinds() {
		var items []unstructured.Unstructured
		namespaces := []string{""}
		if k.core {
			namespaces = m.coreNamespaces()
		}
		missing := false
		for _, ns := range namespaces {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(k.gvk.GroupVersion().WithKind(k.gvk.Kind + "List"))
			var opts []client.ListOption
			if k.core {
				opts = append(opts, client.InNamespace(ns))
			}
			if err := m.Source.Client.List(ctx, list, opts...); err != nil {
				if !k.core && meta.IsNoMatchError(err) {
					missing = true // an operator version without this kind: none of it to move
					break
				}
				return nil, fmt.Errorf("list %s on %s: %w", k.gvk.Kind, m.Source.Name, err)
			}
			items = append(items, list.Items...)
		}
		if missing {
			continue
		}
		sort.Slice(items, func(i, j int) bool { return objectRef(&items[i]) < objectRef(&items[j]) })
		for i := range items {
			if k.skip != nil && k.skip(&items[i]) {
				continue
			}
			items[i].SetGroupVersionKind(k.gvk)
			out = append(out, sourceObject{kind: k, obj: &items[i]})
		}
	}
	return out, nil
}

// coreNamespaces are the namespaces whose ConfigMaps and Secrets move.
func (m *Mover) coreNamespaces() []string {
	if len(m.coreNS) == 0 {
		return []string{m.Options.Namespace}
	}
	return m.coreNS
}

// ensureNamespaces creates the namespaces the claims and the credentials
// live in, with the source's labels.
func (m *Mover) ensureNamespaces(ctx context.Context, objs []sourceObject) error {
	seen := map[string]bool{m.Options.Namespace: true}
	for _, so := range objs {
		ns := so.obj.GetNamespace()
		if ns == "" || seen[ns] {
			continue
		}
		seen[ns] = true
		src := &corev1.Namespace{}
		if err := m.Source.Client.Get(ctx, client.ObjectKey{Name: ns}, src); err != nil {
			return err
		}
		dst := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: src.Labels}}
		if err := m.Target.Client.Create(ctx, dst); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create namespace %s on %s: %w", ns, m.Target.Name, err)
		}
	}
	return nil
}

// writerFor is the client an object is created with. HostClaims are created
// as an admin: the claim policy lets only an admin name a TalosNode.
// HostOperations are created as the user who proposed them: the approval
// policy checks spec.proposedBy against the request's user.
func (m *Mover) writerFor(ctx context.Context, so sourceObject) (client.Client, error) {
	switch so.kind.gvk.Kind {
	case "HostClaim":
		return m.Target.as(impersonatedMover, m.adminGroups(ctx)...)
	case "HostOperation":
		proposer, _, _ := unstructured.NestedString(so.obj.Object, "spec", "proposedBy")
		return m.Target.as(proposer, m.adminGroups(ctx)...)
	}
	return m.Target.Client, nil
}

// adminGroups are the groups an impersonated request carries: the
// operator's admin group (from the policies' parameters on the target) and
// system:masters, so that RBAC lets the replayed user write.
func (m *Mover) adminGroups(ctx context.Context) []string {
	groups := []string{"system:masters", "system:authenticated"}
	params := &corev1.ConfigMap{}
	if err := m.Target.Client.Get(ctx, client.ObjectKey{Namespace: m.Options.Namespace, Name: "hostoperation-approval"}, params); err == nil {
		if g := params.Data["adminGroup"]; g != "" {
			groups = append(groups, g)
		}
	}
	return groups
}

// ensure creates the object on the target, or brings its metadata and spec
// up to the source's. It records the object's target UID and returns how
// many references it rewrote.
func (m *Mover) ensure(ctx context.Context, so sourceObject, rw *rewriter) (int, error) {
	want, n, complete := rw.rewrite(so.obj)
	unstructured.RemoveNestedField(want.Object, "status")
	lifts := takeLifts(want)
	isOp := so.kind.gvk.Kind == "HostOperation"
	approvedBy, _, _ := unstructured.NestedString(want.Object, "spec", "approval", "approvedBy")
	if isOp {
		// Admission refuses an operation created approved; the approval is
		// replayed below, by the approver.
		unstructured.RemoveNestedField(want.Object, "spec", "approval", "approvedBy")
	}

	cur := &unstructured.Unstructured{}
	cur.SetGroupVersionKind(so.kind.gvk)
	err := m.Target.Client.Get(ctx, client.ObjectKeyFromObject(want), cur)
	switch {
	case apierrors.IsNotFound(err):
		w, werr := m.writerFor(ctx, so)
		if werr != nil {
			return 0, werr
		}
		if err := w.Create(ctx, want); err != nil {
			return 0, err
		}
		cur = want
	case err != nil:
		return 0, err
	case !complete:
		// The object is there already, and a reference in it cannot be
		// rewritten yet: the next pass updates it, and an update now would
		// write the source's UID over the target's.
	default:
		keepLifts(cur, want, lifts)
		if changed := mergeInto(cur, want, isOp); changed {
			if err := m.Target.Client.Update(ctx, cur); err != nil {
				return 0, err
			}
		}
	}
	rw.uids[so.obj.GetUID()] = cur.GetUID()
	if err := m.replayLifts(ctx, so, cur, lifts); err != nil {
		return 0, err
	}

	if isOp && approvedBy != "" {
		got, _, _ := unstructured.NestedString(cur.Object, "spec", "approval", "approvedBy")
		if got == "" {
			// The approval policy checks that approvedBy names the request's
			// user, so the approver approves again, by impersonation.
			as, err := m.Target.as(approvedBy, m.adminGroups(ctx)...)
			if err != nil {
				return 0, err
			}
			patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"approval": map[string]any{"approvedBy": approvedBy}}})
			if err := as.Patch(ctx, cur, client.RawPatch(types.MergePatchType, patch)); err != nil {
				return 0, fmt.Errorf("replay the approval by %s: %w", approvedBy, err)
			}
		} else if got != approvedBy {
			return 0, fmt.Errorf("approved by %s on %s but by %s on %s; an approval cannot change", approvedBy, m.Source.Name, got, m.Target.Name)
		}
	}
	return n, nil
}

// mergeInto brings cur's labels, annotations, finalizers, owner references
// and content up to want's and reports whether anything changed. An
// operation's approval is left to ensure.
func mergeInto(cur, want *unstructured.Unstructured, isOp bool) bool {
	changed := false
	set := func(eq bool, apply func()) {
		if !eq {
			apply()
			changed = true
		}
	}
	set(maps.Equal(cur.GetLabels(), want.GetLabels()), func() { cur.SetLabels(want.GetLabels()) })
	set(maps.Equal(cur.GetAnnotations(), want.GetAnnotations()), func() { cur.SetAnnotations(want.GetAnnotations()) })
	set(slices.Equal(cur.GetFinalizers(), want.GetFinalizers()), func() { cur.SetFinalizers(want.GetFinalizers()) })
	set(equality.Semantic.DeepEqual(cur.GetOwnerReferences(), want.GetOwnerReferences()),
		func() { cur.SetOwnerReferences(want.GetOwnerReferences()) })
	for k, v := range want.Object {
		if k == "metadata" || k == "status" || k == "apiVersion" || k == "kind" {
			continue
		}
		if isOp && k == "spec" {
			// Frozen after creation; the approval is replayed separately,
			// so the target's stays as it is.
			spec := &unstructured.Unstructured{Object: map[string]any{"spec": runtime.DeepCopyJSONValue(v)}}
			if by, found, _ := unstructured.NestedString(cur.Object, "spec", "approval", "approvedBy"); found {
				_ = unstructured.SetNestedField(spec.Object, by, "spec", "approval", "approvedBy")
			} else {
				unstructured.RemoveNestedField(spec.Object, "spec", "approval", "approvedBy")
			}
			v = spec.Object["spec"]
		}
		set(equality.Semantic.DeepEqual(cur.Object[k], v), func() { cur.Object[k] = v })
	}
	return changed
}

// copyStatus writes the source's status onto the target object. The Host's
// and the endpoint's status have several writers, each with its own field
// manager; they are applied as those writers apply them.
func (m *Mover) copyStatus(ctx context.Context, so sourceObject, rw *rewriter) error {
	status, found, _ := unstructured.NestedFieldNoCopy(so.obj.Object, "status")
	if !found || status == nil {
		return nil
	}
	cur := &unstructured.Unstructured{}
	cur.SetGroupVersionKind(so.kind.gvk)
	if err := m.Target.Client.Get(ctx, client.ObjectKeyFromObject(so.obj), cur); err != nil {
		return err
	}
	if !so.kind.core {
		typed, err := typedObject(so.obj)
		if err != nil {
			return err
		}
		if handled, err := controller.ApplyStatusShares(ctx, m.Target.Client, typed); handled || err != nil {
			return err
		}
	}
	if equality.Semantic.DeepEqual(cur.Object["status"], status) {
		return nil
	}
	cur.Object["status"] = runtime.DeepCopyJSONValue(status)
	err := m.Target.Client.Status().Update(ctx, cur)
	if apierrors.IsNotFound(err) {
		// A kind without a status subresource took its status with the
		// object itself.
		return nil
	}
	return err
}

var typedScheme = func() *runtime.Scheme {
	s, err := pkscheme.New()
	if err != nil {
		panic(err) // static registration
	}
	return s
}()

// typedObject converts an operator object to its Go type.
func typedObject(u *unstructured.Unstructured) (client.Object, error) {
	o, err := typedScheme.New(u.GroupVersionKind())
	if err != nil {
		return nil, err
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, o); err != nil {
		return nil, err
	}
	co, ok := o.(client.Object)
	if !ok {
		return nil, fmt.Errorf("%s is not an object", u.GroupVersionKind())
	}
	return co, nil
}

// prune removes from the target the operator's objects the source does not
// have: what an older copy left behind (deleted on the other side since).
// Its finalizers come off first, because no manager runs on the target to
// take them off.
func (m *Mover) prune(ctx context.Context, objs []sourceObject) ([]string, error) {
	keep := map[string]bool{}
	for _, so := range objs {
		keep[so.kind.gvk.Kind+" "+objectRef(so.obj)] = true
	}
	var pruned []string
	kinds := m.movedKinds()
	for i := len(kinds) - 1; i >= 0; i-- {
		k := kinds[i]
		if k.core {
			continue
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(k.gvk.GroupVersion().WithKind(k.gvk.Kind + "List"))
		if err := m.Target.Client.List(ctx, list); err != nil {
			if meta.IsNoMatchError(err) {
				continue // the target does not serve the kind: nothing of it to prune
			}
			return nil, err
		}
		for j := range list.Items {
			u := &list.Items[j]
			key := k.gvk.Kind + " " + objectRef(u)
			if keep[key] {
				continue
			}
			if len(u.GetFinalizers()) > 0 {
				u.SetFinalizers(nil)
				if err := m.Target.Client.Update(ctx, u); client.IgnoreNotFound(err) != nil {
					return nil, err
				}
			}
			if err := m.Target.Client.Delete(ctx, u); client.IgnoreNotFound(err) != nil {
				return nil, err
			}
			pruned = append(pruned, key)
			m.logf("removed %s from %s: %s no longer has it", key, m.Target.Name, m.Source.Name)
		}
	}
	return pruned, nil
}

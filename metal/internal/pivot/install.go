package pivot

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// operatorGroups are the API groups of the operator's own kinds.
var operatorGroups = []string{metalv1alpha1.GroupVersion.Group, osv1alpha1.GroupVersion.Group}

// Install puts the operator onto the target, from what runs on the source:
// the same manifests the source was installed with, read back from its API
// server. The order is the one a fresh install needs: the CRDs (and the wait
// until they are served), the admission policies, the cluster-wide RBAC, the
// operator namespace, and in it the service account, roles, the policies'
// parameters, the Services, the NetworkPolicies, the claims the manager mounts (empty: their
// contents do not move) and the manager's Deployment at zero replicas.
// Every object is applied server-side, so a rerun changes only what differs.
func (m *Mover) Install(ctx context.Context) error {
	o := m.Options
	src := m.Source.Client

	crds, err := listUnstructured(ctx, src, crdGVK)
	if err != nil {
		return err
	}
	var names []string
	for i := range crds {
		group, _, _ := unstructured.NestedString(crds[i].Object, "spec", "group")
		if !slices.Contains(operatorGroups, group) {
			continue
		}
		if err := m.apply(ctx, &crds[i]); err != nil {
			return err
		}
		names = append(names, crds[i].GetName())
	}
	if len(names) == 0 {
		return fmt.Errorf("%s serves none of the operator's CRDs", m.Source.Name)
	}
	if err := m.wait(ctx, "the CRDs to be served on "+m.Target.Name, func(ctx context.Context) (bool, error) {
		for _, n := range names {
			crd := &unstructured.Unstructured{}
			crd.SetGroupVersionKind(crdGVK)
			if err := m.Target.Client.Get(ctx, client.ObjectKey{Name: n}, crd); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			if !crdEstablished(crd) {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		return err
	}
	m.logf("installed %d CRDs", len(names))

	// The admission policies guard the operator's kinds; their bindings name
	// their parameters, which live in a namespace.
	policies := &admissionregistrationv1.ValidatingAdmissionPolicyList{}
	if err := src.List(ctx, policies); err != nil {
		return err
	}
	var policyNames []string
	for i := range policies.Items {
		if guardsOperator(&policies.Items[i]) {
			if err := m.applyTyped(ctx, &policies.Items[i], "ValidatingAdmissionPolicy"); err != nil {
				return err
			}
			policyNames = append(policyNames, policies.Items[i].Name)
		}
	}
	bindings := &admissionregistrationv1.ValidatingAdmissionPolicyBindingList{}
	if err := src.List(ctx, bindings); err != nil {
		return err
	}
	params := map[types.NamespacedName]bool{}
	for i := range bindings.Items {
		b := &bindings.Items[i]
		if !slices.Contains(policyNames, b.Spec.PolicyName) {
			continue
		}
		if err := m.applyTyped(ctx, b, "ValidatingAdmissionPolicyBinding"); err != nil {
			return err
		}
		if p := b.Spec.ParamRef; p != nil && p.Name != "" && p.Namespace != "" {
			params[types.NamespacedName{Namespace: p.Namespace, Name: p.Name}] = true
		}
	}
	m.logf("installed %d admission policies", len(policyNames))

	// Cluster-wide RBAC: the manager's roles, the approver role, and their
	// bindings. They name the operator, so they carry its prefix.
	crs := &rbacv1.ClusterRoleList{}
	if err := src.List(ctx, crs); err != nil {
		return err
	}
	for i := range crs.Items {
		if strings.HasPrefix(crs.Items[i].Name, "pk-metal") {
			if err := m.applyTyped(ctx, &crs.Items[i], "ClusterRole"); err != nil {
				return err
			}
		}
	}
	crbs := &rbacv1.ClusterRoleBindingList{}
	if err := src.List(ctx, crbs); err != nil {
		return err
	}
	for i := range crbs.Items {
		if strings.HasPrefix(crbs.Items[i].Name, "pk-metal") {
			if err := m.applyTyped(ctx, &crbs.Items[i], "ClusterRoleBinding"); err != nil {
				return err
			}
		}
	}

	// The namespaces: the operator's, marked as moved here from the
	// source, and the ones the policy parameters live in.
	namespaces := []string{o.Namespace}
	for p := range params {
		if !slices.Contains(namespaces, p.Namespace) {
			namespaces = append(namespaces, p.Namespace)
		}
	}
	for _, n := range namespaces {
		ns := &corev1.Namespace{}
		if err := src.Get(ctx, client.ObjectKey{Name: n}, ns); err != nil {
			return err
		}
		ns.Annotations = nil
		if err := m.applyTyped(ctx, ns, "Namespace"); err != nil {
			return err
		}
	}
	if err := m.markNamespace(ctx, m.Target, map[string]any{AnnotationSource: m.sourceID, AnnotationMovedTo: nil}); err != nil {
		return err
	}

	// In the operator namespace: the service account, its roles, the
	// policies' parameters, the Services, and the Deployment.
	sas := &corev1.ServiceAccountList{}
	if err := src.List(ctx, sas, client.InNamespace(o.Namespace)); err != nil {
		return err
	}
	for i := range sas.Items {
		if sas.Items[i].Name == "default" {
			continue
		}
		sas.Items[i].Secrets = nil
		if err := m.applyTyped(ctx, &sas.Items[i], "ServiceAccount"); err != nil {
			return err
		}
	}
	roles := &rbacv1.RoleList{}
	if err := src.List(ctx, roles, client.InNamespace(o.Namespace)); err != nil {
		return err
	}
	for i := range roles.Items {
		if err := m.applyTyped(ctx, &roles.Items[i], "Role"); err != nil {
			return err
		}
	}
	rbs := &rbacv1.RoleBindingList{}
	if err := src.List(ctx, rbs, client.InNamespace(o.Namespace)); err != nil {
		return err
	}
	for i := range rbs.Items {
		if err := m.applyTyped(ctx, &rbs.Items[i], "RoleBinding"); err != nil {
			return err
		}
	}
	for p := range params {
		cm := &corev1.ConfigMap{}
		if err := src.Get(ctx, client.ObjectKey(p), cm); err != nil {
			return fmt.Errorf("the admission policies' parameters %s: %w", p, err)
		}
		if err := m.applyTyped(ctx, cm, "ConfigMap"); err != nil {
			return err
		}
	}
	svcs := &corev1.ServiceList{}
	if err := src.List(ctx, svcs, client.InNamespace(o.Namespace)); err != nil {
		return err
	}
	for i := range svcs.Items {
		svc := &svcs.Items[i]
		// Addresses belong to the source's service network.
		svc.Spec.ClusterIP, svc.Spec.ClusterIPs = "", nil
		svc.Spec.IPFamilies, svc.Spec.IPFamilyPolicy = nil, nil
		if err := m.applyTyped(ctx, svc, "Service"); err != nil {
			return err
		}
	}
	// The NetworkPolicies: the media port answers the BMC network only, on
	// the target as on the source.
	netPolicies := &networkingv1.NetworkPolicyList{}
	if err := src.List(ctx, netPolicies, client.InNamespace(o.Namespace)); err != nil {
		return err
	}
	for i := range netPolicies.Items {
		if err := m.applyTyped(ctx, &netPolicies.Items[i], "NetworkPolicy"); err != nil {
			return err
		}
	}
	claims, err := m.claimMoves(ctx)
	if err != nil {
		return err
	}
	for _, c := range claims {
		if c.Action != ClaimCreate {
			continue
		}
		if err := m.applyTyped(ctx, targetClaim(c), "PersistentVolumeClaim"); err != nil {
			return err
		}
		m.logf("made PersistentVolumeClaim %s/%s, empty", o.Namespace, c.Name)
	}
	d := &appsv1.Deployment{}
	if err := src.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.Deployment}, d); err != nil {
		return err
	}
	if err := m.applyTyped(ctx, managerForTarget(d, o.Image, o.MediaURL, claims), "Deployment"); err != nil {
		return err
	}
	m.logf("installed the operator in namespace %s (Deployment %s at 0 replicas)", o.Namespace, o.Deployment)
	return nil
}

// managerForTarget is the source's Deployment as the target runs it: at zero
// replicas until the state is copied, with the image and media URL the
// target needs. A claim made ephemeral becomes an emptyDir, and the manager
// no longer names it to the imager Jobs: they are other pods and could not
// write into it. A claim copied keeps its name, so the flag that names it
// stays right.
func managerForTarget(src *appsv1.Deployment, image, mediaURL string, claims []ClaimMove) *appsv1.Deployment {
	d := src.DeepCopy()
	zero := int32(0)
	d.Spec.Replicas = &zero
	delete(d.Annotations, "deployment.kubernetes.io/revision")
	var ephemeral []string
	for _, c := range claims {
		if c.Action != ClaimEphemeral {
			continue
		}
		ephemeral = append(ephemeral, c.Name)
		for i := range d.Spec.Template.Spec.Volumes {
			if v := &d.Spec.Template.Spec.Volumes[i]; v.Name == c.Volume {
				v.VolumeSource = ephemeralVolume(c)
			}
		}
	}
	for i := range d.Spec.Template.Spec.Containers {
		c := &d.Spec.Template.Spec.Containers[i]
		if c.Name != "manager" {
			continue
		}
		if image != "" {
			c.Image = image
		}
		if mediaURL != "" {
			c.Args = slices.DeleteFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "--media-url=") })
			c.Args = append(c.Args, "--media-url="+mediaURL)
		}
		c.Args = slices.DeleteFunc(c.Args, func(a string) bool {
			v, ok := strings.CutPrefix(a, storeClaimFlag+"=")
			return ok && slices.Contains(ephemeral, v)
		})
	}
	return d
}

// guardsOperator: the policy matches a resource of the operator's groups,
// or the site settings ConfigMap that the other policies read.
func guardsOperator(p *admissionregistrationv1.ValidatingAdmissionPolicy) bool {
	if p.Spec.MatchConstraints == nil {
		return false
	}
	for _, r := range p.Spec.MatchConstraints.ResourceRules {
		for _, g := range r.APIGroups {
			if slices.Contains(operatorGroups, g) {
				return true
			}
		}
		if slices.Contains(r.Resources, "configmaps") && slices.Contains(r.ResourceNames, settingsConfigMap) {
			return true
		}
	}
	return false
}

func crdEstablished(crd *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
	for _, c := range conds {
		if cm, ok := c.(map[string]any); ok && cm["type"] == "Established" && cm["status"] == "True" {
			return true
		}
	}
	return false
}

func listUnstructured(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, opts ...client.ListOption) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := c.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// applyTyped applies a client-go object read from the source.
func (m *Mover) applyTyped(ctx context.Context, obj client.Object, kind string) error {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: raw}
	gvk, err := apiutilGVK(obj, kind)
	if err != nil {
		return err
	}
	u.SetGroupVersionKind(gvk)
	return m.apply(ctx, u)
}

// apiutilGVK names the group and version of a client-go object of kind.
func apiutilGVK(obj client.Object, kind string) (schema.GroupVersionKind, error) {
	gvks, _, err := typedScheme.ObjectKinds(obj)
	if err != nil || len(gvks) == 0 {
		return schema.GroupVersionKind{}, fmt.Errorf("unknown kind %s: %w", kind, err)
	}
	return gvks[0], nil
}

// apply writes obj to the target server-side, without what the source's API
// server assigned and without its status.
func (m *Mover) apply(ctx context.Context, obj *unstructured.Unstructured) error {
	u := sanitize(obj)
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "ownerReferences")
	unstructured.RemoveNestedField(u.Object, "metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")
	if err := m.Target.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %s %s on %s: %w", u.GetKind(), objectRef(u), m.Target.Name, err)
	}
	return nil
}

// markNamespace sets (a string) or removes (nil) annotations on the
// operator namespace of one side.
func (m *Mover) markNamespace(ctx context.Context, c *Cluster, annotations map[string]any) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return err
	}
	ns := &corev1.Namespace{}
	ns.Name = m.Options.Namespace
	if err := c.Client.Patch(ctx, ns, client.RawPatch(types.MergePatchType, patch)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("mark namespace %s on %s: %w", m.Options.Namespace, c.Name, err)
	}
	return nil
}

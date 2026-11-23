package controller

import (
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// Labels are the profile-matching surface (design doc § profile selection).
// Hosts carry two axes: intent labels copied verbatim from the bound claim's
// free-form map (unprefixed by schema, cleared with the claim) and discovered
// facts stamped under the API-group domain (present regardless of claim state
// — the axis for hardware baselines). Endpoints have no claim, so they carry
// discovered facts only (vendor, model, vlan — the EndpointProfile axis). One
// reconciler owns each object's labels (HostReconciler / the endpoint
// reconciler); which keys it manages is bookkept in one annotation so
// human-applied labels survive every pass.
const (
	labelDomain             = "metal.pk.io/"
	labelVendor             = labelDomain + "vendor"
	labelModel              = labelDomain + "model"
	labelVLAN               = labelDomain + "vlan"
	managedLabelsAnnotation = labelDomain + "managed-labels"
)

// stampLabels reconciles the managed subset of the host's labels and reports
// whether anything changed (the caller then persists the metadata write).
// claim is the bound claim or nil; vendor/model are the discovered facts.
func stampLabels(host *metalv1alpha1.Host, claim *metalv1alpha1.HostClaim, vendor, model string) bool {
	desired := map[string]string{}
	if v := labelValue(vendor); v != "" {
		desired[labelVendor] = v
	}
	if v := labelValue(model); v != "" {
		desired[labelModel] = v
	}
	if claim != nil {
		for k, v := range claim.Spec.Labels {
			desired[k] = string(v)
		}
	}
	return stampManagedLabels(host, desired)
}

// stampEndpointLabels reconciles the endpoint's discovered-fact labels —
// vendor and model from the manager, vlan from the asset snapshot.
func stampEndpointLabels(ep *metalv1alpha1.ManagementEndpoint) bool {
	desired := map[string]string{}
	if v := labelValue(ep.Status.Vendor); v != "" {
		desired[labelVendor] = v
	}
	if v := labelValue(ep.Status.Model); v != "" {
		desired[labelModel] = v
	}
	if snap := ep.Status.Asset; snap != nil && snap.VLAN != 0 {
		desired[labelVLAN] = strconv.Itoa(snap.VLAN)
	}
	return stampManagedLabels(ep, desired)
}

// stampManagedLabels reconciles an object's managed label subset against
// desired, bookkeeping the managed keys in one annotation, and reports
// whether anything changed.
func stampManagedLabels(obj metav1.Object, desired map[string]string) bool {
	labels, annotations := obj.GetLabels(), obj.GetAnnotations()

	changed := false
	for _, k := range strings.Split(annotations[managedLabelsAnnotation], ",") {
		if k == "" {
			continue
		}
		if _, keep := desired[k]; keep {
			continue
		}
		if _, present := labels[k]; present {
			delete(labels, k)
			changed = true
		}
	}
	for k, v := range desired {
		if labels[k] != v {
			if labels == nil {
				labels = map[string]string{}
			}
			labels[k] = v
			changed = true
		}
	}

	keys := make([]string, 0, len(desired))
	for k := range desired {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	switch want := strings.Join(keys, ","); {
	case want == "" && annotations[managedLabelsAnnotation] != "":
		delete(annotations, managedLabelsAnnotation)
		changed = true
	case want != "" && annotations[managedLabelsAnnotation] != want:
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[managedLabelsAnnotation] = want
		changed = true
	}
	if changed {
		obj.SetLabels(labels)
		obj.SetAnnotations(annotations)
	}
	return changed
}

// labelValue sanitizes a discovered fact into a legal label value: illegal
// runes become "-" (a chassis model like "2288H V5" carries a space), edges
// must be alphanumeric, 63 chars max. Labels are join keys, not display —
// fidelity stays in status.
func labelValue(s string) string {
	mapped := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, s)
	if len(mapped) > 63 {
		mapped = mapped[:63]
	}
	alnum := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
	}
	start := strings.IndexFunc(mapped, alnum)
	if start < 0 {
		return ""
	}
	end := strings.LastIndexFunc(mapped, alnum)
	return mapped[start : end+1]
}

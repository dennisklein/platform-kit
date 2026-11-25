package v1alpha1

import "slices"

// AnnotationRequestedBy on an object whose change makes the operator propose
// work names the person who last changed what the object asks for. Its
// value is a username. Admission (config/policy/requested-by.yaml) accepts
// a request that changes the intent of such an object only if the request
// also sets this annotation to the username of the request, and a request
// that changes the annotation only if it names the requester. Where the
// site allows self-approval, an object without the annotation needs none;
// one with it still names the user of each change of the intent. So the
// value, where it is set, always names the last person who changed the
// intent, or a person who took the change on as theirs.
//
// The operator copies the names of the objects behind a proposal into the
// proposal's spec.requestedBy. Where the site forbids self-approval, none
// of them may approve it: the person who bumps a cluster's version, marks
// a Host for a wipe or a claim for a reinstall cannot approve the change
// that follows, although the operator proposed it.
//
// The intent of each kind, and the kinds themselves, are listed in the
// policy. pk-metal sets the annotation for its caller; a GitOps controller
// sets its own service account name in the manifests it applies.
const AnnotationRequestedBy = "metal.pk.io/requested-by"

// RequestedBy collects the names that the annotations of objs give, sorted
// and without repeats. An object without the annotation adds nothing.
func RequestedBy(annotations ...map[string]string) []string {
	var out []string
	for _, a := range annotations {
		if v := a[AnnotationRequestedBy]; v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

package v1alpha1

import "strings"

// NamedGuardLifts are the annotations with which a person lifts a safety
// rule or decides a step of a change. Each one names who decided and why,
// in the form "<username>: <reason>". Admission
// (config/policy/guard-lift.yaml) accepts a new or changed value only in
// that form, and only with the username of the person who sets it, so that
// the object records who decided. Removing the annotation is always
// allowed: it puts the rule back.
//
// The decisions: an abort says why in its reason; the upload of an etcd
// snapshot gives the sha256 it uploaded as its reason.
var NamedGuardLifts = []string{
	AnnotationOverrideClusterGuard, AnnotationIgnoreWindow, AnnotationAckEtcdRisk,
	AnnotationAllowSelfDisruption, AnnotationWipeWaived, AnnotationAbort, AnnotationEtcdSnapshotUploaded,
}

// guardLiftSeparator divides the username from the reason. A username can
// contain a colon (system:serviceaccount:<ns>:<name>), but not a colon
// followed by a space.
const guardLiftSeparator = ": "

// GuardLiftValue is the value of a guard-lift annotation that user sets for
// the reason why. A reason that already names user is returned as it is, so
// that a value passed through twice is not stamped twice.
func GuardLiftValue(user, why string) string {
	why = strings.TrimSpace(why)
	if strings.HasPrefix(why, user+guardLiftSeparator) {
		return why
	}
	return user + guardLiftSeparator + why
}

// ParseGuardLift splits a guard-lift value into the username and the
// reason. ok is false when the value does not have the form
// "<username>: <reason>" with both parts set: a value written before
// admission asked for the form, or one that a person who bypassed admission
// wrote.
func ParseGuardLift(value string) (user, why string, ok bool) {
	user, why, found := strings.Cut(value, guardLiftSeparator)
	user, why = strings.TrimSpace(user), strings.TrimSpace(why)
	if !found || user == "" || why == "" {
		return "", strings.TrimSpace(value), false
	}
	return user, why, true
}

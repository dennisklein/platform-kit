package main

import "strings"

// The schema of a kind holds some field descriptions that the site must not
// show as they are: the upstream Kubernetes text of metav1.Condition,
// metav1.LabelSelector and the object references. It is long, speaks of API
// history and is not in the STE style of the site. docgen replaces it with one
// fixed sentence. A field with its own description keeps it.

// upstreamText maps the start of an upstream description to its replacement.
var upstreamText = []struct{ prefix, text string }{
	// metav1.Condition
	{"type of condition in CamelCase", "The type of the condition."},
	{"status of the condition, one of True, False, Unknown", "The status of the condition: True, False or Unknown."},
	{"reason contains a programmatic identifier", "A word in CamelCase that tells why the condition has its status."},
	{"message is a human readable message", "A message for people about the last change. It can be empty."},
	{"lastTransitionTime is the last time the condition transitioned", "The time of the last change of the status."},
	{"observedGeneration represents the .metadata.generation", "The generation of the object that the condition is for. A lower value than metadata.generation shows an old condition."},
	// metav1.LabelSelector
	{"matchLabels is a map of {key,value} pairs", "Labels that the object must have, as key and value pairs. All must match."},
	{"matchExpressions is a list of label selector requirements", "Label requirements that the object must meet. All must match."},
	{"key is the label key that the selector applies to", "The label key."},
	{"operator represents a key's relationship to a set of values", "In, NotIn, Exists or DoesNotExist."},
	{"values is an array of string values", "The values for In and NotIn. Exists and DoesNotExist take no values."},
	// corev1.ObjectReference, LocalObjectReference
	{"Name of the referent", "The name of the object."},
	{"Namespace of the referent", "The namespace of the object."},
	{"UID of the referent", refLeafText["uid"]},
	{"UID is a type that holds unique ID values", refLeafText["uid"]},
	{"Kind of the referent", "The kind of the object."},
	{"API version of the referent", "The API version of the object."},
}

// refLeafText holds one fixed sentence for each leaf field of an object
// reference (claimRef.name, nodeRef.uid) that has no description of its own.
var refLeafText = map[string]string{
	"name":       "The name of the object.",
	"namespace":  "The namespace of the object.",
	"uid":        "The UID of the object. An object made again with the same name has a different UID.",
	"kind":       "The kind of the object.",
	"apiVersion": "The API version of the object.",
	"key":        "The key in the object's data.",
}

// fieldDescription returns the description to show for the field name below
// parent: the replacement of an upstream text, a fixed sentence for an empty
// leaf of a reference (a parent whose name ends in "Ref"), or desc.
func fieldDescription(parent, name, desc string) string {
	for _, u := range upstreamText {
		if strings.HasPrefix(desc, u.prefix) {
			return u.text
		}
	}
	if strings.TrimSpace(desc) != "" {
		return desc
	}
	last := parent[strings.LastIndex(parent, ".")+1:]
	if !strings.HasSuffix(strings.TrimSuffix(last, "[]"), "Ref") {
		return desc
	}
	return refLeafText[name]
}

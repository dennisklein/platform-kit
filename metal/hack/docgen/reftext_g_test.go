package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFieldDescription(t *testing.T) {
	upstreamName := "Name of the referent. This field is effectively required, but due to backwards compatibility is allowed to be empty."
	upstreamUID := "UID is a type that holds unique ID values, including UUIDs.  Because we don't ONLY use UUIDs, this is an alias to string."
	upstreamGen := "observedGeneration represents the .metadata.generation that the condition was set based upon."
	cases := []struct {
		parent, name, desc, want string
	}{
		{"spec.nodeRef", "name", "", "The name of the object."},
		{"spec.credentialsRef", "name", upstreamName, "The name of the object."},
		{"spec.claimRef", "uid", upstreamUID, refLeafText["uid"]},
		{"spec.actions[].nodeRef", "namespace", "", "The namespace of the object."},
		{"status.conditions[]", "observedGeneration", upstreamGen, "The generation of the object that the condition is for. A lower value than metadata.generation shows an old condition."},
		{"spec.match", "matchLabels", "matchLabels is a map of {key,value} pairs. A single...", "Labels that the object must have, as key and value pairs. All must match."},
		// Own descriptions stay, also on a reference.
		{"spec.issuerRef", "kind", "Kind is Issuer or ClusterIssuer.", "Kind is Issuer or ClusterIssuer."},
		// Not a reference: the parent name does not end in Ref.
		{"spec.refs[]", "name", "", ""},
		{"spec.template", "name", "", ""},
		// A reference field without a fixed sentence.
		{"spec.nodeRef", "port", "", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, fieldDescription(c.parent, c.name, c.desc), "%s.%s", c.parent, c.name)
	}
}

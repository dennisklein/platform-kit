// Package cluster is the kind Cluster (pk/v1alpha1, clusters/<id>/cluster.yaml).
package cluster

import (
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

const (
	Group = "pk"
	Name  = "Cluster"
)

// Cluster is pk/v1alpha1 Cluster. The id is metadata.name, a ship name that
// never changes. Role and ring are labels, so a cluster changes role without
// moving files.
type Cluster struct {
	APIVersion string         `json:"apiVersion" yaml:"apiVersion" jsonschema:"enum=pk/v1alpha1"`
	Kind       string         `json:"kind" yaml:"kind" jsonschema:"enum=Cluster"`
	Metadata   kinds.Metadata `json:"metadata" yaml:"metadata"`
	Spec       Spec           `json:"spec" yaml:"spec"`
}

// Spec of a Cluster.
type Spec struct {
	Role        string `json:"role" yaml:"role" jsonschema:"title=Role,enum=control,enum=tenants,enum=storage" pk:"truth=intent,widget=select,label=Role,order=1"`
	Ring        string `json:"ring" yaml:"ring" jsonschema:"title=Ring,enum=lab,enum=canary,enum=prod" pk:"truth=intent,widget=select,label=Ring,order=2"`
	Description string `json:"description,omitempty" yaml:"description,omitempty" pk:"truth=intent,label=Description,order=3"`
	Endpoint    string `json:"endpoint,omitempty" yaml:"endpoint,omitempty" jsonschema:"title=API endpoint,format=uri" pk:"truth=intent,label=API endpoint,order=4"`
	// Talos is the talconfig path of the cluster.
	Talos string `json:"talos,omitempty" yaml:"talos,omitempty" jsonschema:"title=talconfig" pk:"truth=intent,label=talconfig,order=5"`
}

var versions = []kindutil.Version{{Name: "v1alpha1", Type: Cluster{}}}

// Kind returns the Cluster kind.
func Kind() kinds.Kind {
	return kinds.Kind{
		Name:      Name,
		Group:     Group,
		Version:   "v1alpha1",
		Versions:  kindutil.Names(versions...),
		Own:       true,
		Paths:     []string{"clusters/*/cluster.yaml"},
		NewPath:   func(name string) string { return "clusters/" + name + "/cluster.yaml" },
		Truth:     kinds.Intent,
		ApplyPath: kinds.ApplyNone,
		Schema:    kindutil.SchemaFunc(Group, Name, "A cluster with a ship name id, a role and a ring.", versions...),
		Validate:  validate,
		Describe:  kindutil.Describer(Name, "cluster"),
	}
}

// Package switches is the kind Switch (inventory.pk.io).
package switches

import (
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

const (
	Group = "inventory.pk.io"
	Name  = "Switch"
)

// Switch is inventory.pk.io/v1beta1 Switch (control/inventory/switches/<name>.yaml).
type Switch struct {
	APIVersion string         `json:"apiVersion" yaml:"apiVersion" jsonschema:"enum=inventory.pk.io/v1beta1"`
	Kind       string         `json:"kind" yaml:"kind" jsonschema:"enum=Switch"`
	Metadata   kinds.Metadata `json:"metadata" yaml:"metadata"`
	Spec       Spec           `json:"spec" yaml:"spec"`
}

// Spec of a Switch.
type Spec struct {
	Network string `json:"network" yaml:"network" jsonschema:"title=Network,enum=mgmt,enum=data-a,enum=data-b" pk:"truth=intent,widget=select,label=Network,order=1"`
	Ports   int    `json:"ports" yaml:"ports" jsonschema:"title=Ports,minimum=1,maximum=128,default=48" pk:"truth=asserted,owner=asset-db,label=Ports,order=2"`
	// Uplinks are ports reserved for uplinks (see control/cabling.yaml).
	Uplinks []int  `json:"uplinks,omitempty" yaml:"uplinks,omitempty" jsonschema:"title=Uplink ports" pk:"truth=intent,label=Uplink ports,order=3"`
	Rack    string `json:"rack" yaml:"rack" jsonschema:"title=Rack" pk:"truth=intent,ref=Rack,label=Rack,order=4"`
	Unit    int    `json:"unit" yaml:"unit" jsonschema:"title=Unit,minimum=1,maximum=60" pk:"truth=intent,label=Unit,order=5"`
	Size    int    `json:"size,omitempty" yaml:"size,omitempty" jsonschema:"title=Height (U),minimum=1,default=1" pk:"truth=asserted,owner=asset-db,label=Height,unit=U,order=6"`
	Model   string `json:"model,omitempty" yaml:"model,omitempty" jsonschema:"title=Model" pk:"truth=asserted,owner=asset-db,label=Model,order=7"`
}

var versions = []kindutil.Version{{Name: "v1beta1", Type: Switch{}}}

// Kind returns the Switch kind.
func Kind() kinds.Kind {
	return kinds.Kind{
		Name:      Name,
		Group:     Group,
		Version:   "v1beta1",
		Versions:  kindutil.Names(versions...),
		Own:       true,
		Paths:     []string{"control/inventory/switches/*.yaml"},
		NewPath:   func(name string) string { return "control/inventory/switches/" + name + ".yaml" },
		Truth:     kinds.Intent,
		ApplyPath: kinds.ApplyManual,
		Views:     []string{"rack"},
		Schema:    kindutil.SchemaFunc(Group, Name, "A network switch with numbered ports.", versions...),
		Validate:  validate,
		Describe:  kindutil.Describer(Name, "switch"),
	}
}

// Package cableplan is the kind CablePlan (inventory.pk.io): uplinks between
// the switches and the spine (control/cabling.yaml).
package cableplan

import (
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

const (
	Group = "inventory.pk.io"
	Name  = "CablePlan"
)

// CablePlan is inventory.pk.io/v1beta1 CablePlan.
type CablePlan struct {
	APIVersion string         `json:"apiVersion" yaml:"apiVersion" jsonschema:"enum=inventory.pk.io/v1beta1"`
	Kind       string         `json:"kind" yaml:"kind" jsonschema:"enum=CablePlan"`
	Metadata   kinds.Metadata `json:"metadata" yaml:"metadata"`
	Spec       Spec           `json:"spec" yaml:"spec"`
}

// Spec of a CablePlan.
type Spec struct {
	// Spines are devices outside the inventory (spine and out-of-band routers)
	// that uplinks may end on.
	Spines  []string `json:"spines" yaml:"spines" jsonschema:"title=Spine devices" pk:"truth=asserted,label=Spine devices,order=1"`
	Uplinks []Uplink `json:"uplinks" yaml:"uplinks" jsonschema:"title=Uplinks" pk:"truth=intent,label=Uplinks,order=2"`
}

// Uplink is one cable between a switch port and a spine port.
type Uplink struct {
	ID    string `json:"id" yaml:"id" jsonschema:"title=Cable,pattern=^[A-Z]+-[0-9]{4}$" pk:"truth=intent,label=Cable"`
	A     End    `json:"a" yaml:"a" pk:"truth=intent,widget=port-picker,label=A"`
	B     End    `json:"b" yaml:"b" pk:"truth=intent,label=B"`
	Speed string `json:"speed,omitempty" yaml:"speed,omitempty" jsonschema:"enum=1G,enum=10G,enum=25G,enum=100G,enum=400G" pk:"truth=asserted,label=Speed"`
}

// End is one end of a cable.
type End struct {
	Switch string `json:"switch" yaml:"switch" pk:"ref=Switch,label=Switch"`
	Port   int    `json:"port" yaml:"port" jsonschema:"minimum=1" pk:"label=Port"`
}

var versions = []kindutil.Version{{Name: "v1beta1", Type: CablePlan{}}}

// Kind returns the CablePlan kind.
func Kind() kinds.Kind {
	return kinds.Kind{
		Name:      Name,
		Group:     Group,
		Version:   "v1beta1",
		Versions:  kindutil.Names(versions...),
		Own:       true,
		Paths:     []string{"control/cabling.yaml"},
		NewPath:   func(string) string { return "control/cabling.yaml" },
		Truth:     kinds.Intent,
		ApplyPath: kinds.ApplyManual,
		Schema:    kindutil.SchemaFunc(Group, Name, "Uplinks between switches and the spine.", versions...),
		Validate:  validate,
		Describe:  describe,
	}
}

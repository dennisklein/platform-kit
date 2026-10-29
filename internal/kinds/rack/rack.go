// Package rack is the kind Rack (inventory.pk.io).
package rack

import (
	"strings"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

const (
	Group = "inventory.pk.io"
	Name  = "Rack"
)

// Rack is inventory.pk.io/v1beta1 Rack (control/inventory/racks/<name>.yaml).
// The name is the room label, for example GC5A-6Z.
type Rack struct {
	APIVersion string         `json:"apiVersion" yaml:"apiVersion" jsonschema:"enum=inventory.pk.io/v1beta1"`
	Kind       string         `json:"kind" yaml:"kind" jsonschema:"enum=Rack"`
	Metadata   kinds.Metadata `json:"metadata" yaml:"metadata"`
	Spec       Spec           `json:"spec" yaml:"spec"`
}

// Spec of a Rack.
type Spec struct {
	Row      string `json:"row" yaml:"row" jsonschema:"title=Row" pk:"truth=asserted,owner=asset-db,label=Row,order=1"`
	Units    int    `json:"units" yaml:"units" jsonschema:"title=Height (U),minimum=1,maximum=60,default=47" pk:"truth=asserted,owner=asset-db,label=Units,unit=U,order=2"`
	Location string `json:"location" yaml:"location" jsonschema:"title=Location,description=Building and room" pk:"truth=asserted,owner=asset-db,label=Location,order=3"`
}

var versions = []kindutil.Version{{Name: "v1beta1", Type: Rack{}}}

// Kind returns the Rack kind.
func Kind() kinds.Kind {
	return kinds.Kind{
		Name:      Name,
		Group:     Group,
		Version:   "v1beta1",
		Versions:  kindutil.Names(versions...),
		Own:       true,
		Paths:     []string{"control/inventory/racks/*.yaml"},
		NewPath:   func(name string) string { return "control/inventory/racks/" + strings.ToLower(name) + ".yaml" },
		Truth:     kinds.Asserted,
		ApplyPath: kinds.ApplyNone,
		Views:     []string{"rack"},
		Schema:    kindutil.SchemaFunc(Group, Name, "A rack with numbered units.", versions...),
		Describe:  kindutil.Describer(Name, "rack"),
	}
}

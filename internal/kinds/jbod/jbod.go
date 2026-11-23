// Package jbod is the kind Jbod (inventory.pk.io): a disk shelf attached to a
// storage node.
package jbod

import (
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

const (
	Group = "inventory.pk.io"
	Name  = "Jbod"
)

// Jbod is inventory.pk.io/v1beta1 Jbod (control/inventory/<name>.yaml, for example jb-05).
type Jbod struct {
	APIVersion string         `json:"apiVersion" yaml:"apiVersion" jsonschema:"enum=inventory.pk.io/v1beta1"`
	Kind       string         `json:"kind" yaml:"kind" jsonschema:"enum=Jbod"`
	Metadata   kinds.Metadata `json:"metadata" yaml:"metadata"`
	Spec       Spec           `json:"spec" yaml:"spec"`
}

// Spec of a Jbod.
type Spec struct {
	AttachedTo string   `json:"attachedTo" yaml:"attachedTo" jsonschema:"title=Attached to" pk:"truth=intent,ref=Node,label=Attached to,order=1"`
	Location   Location `json:"location" yaml:"location" pk:"truth=intent,widget=rack-picker,label=Location,order=2"`
	Model      string   `json:"model,omitempty" yaml:"model,omitempty" pk:"truth=asserted,owner=asset-db,label=Model,order=3"`
	Serial     string   `json:"serial,omitempty" yaml:"serial,omitempty" pk:"truth=asserted,owner=asset-db,label=Serial,order=4"`
	Drawers    []Drawer `json:"drawers" yaml:"drawers" pk:"label=Drawers,order=10"`
}

// Location in a rack.
type Location struct {
	Rack string `json:"rack" yaml:"rack" pk:"truth=intent,ref=Rack,label=Rack"`
	Unit int    `json:"unit" yaml:"unit" jsonschema:"minimum=1,maximum=60" pk:"truth=intent,label=Unit"`
	Size int    `json:"size" yaml:"size" jsonschema:"minimum=1,default=4" pk:"truth=asserted,owner=asset-db,label=Height,unit=U"`
}

// Drawer of a Jbod.
type Drawer struct {
	ID    int    `json:"id" yaml:"id" jsonschema:"minimum=0"`
	Slots []Slot `json:"slots" yaml:"slots"`
}

// SlotState is the state of a disk slot.
type SlotState string

const (
	SlotOK      SlotState = "ok"
	SlotReplace SlotState = "replace"
	SlotEmpty   SlotState = "empty"
)

// Slot is one disk slot. Setting state replace drains the OSD after merge.
type Slot struct {
	Slot   int       `json:"slot" yaml:"slot" jsonschema:"minimum=0"`
	State  SlotState `json:"state" yaml:"state" jsonschema:"enum=ok,enum=replace,enum=empty" pk:"truth=intent,widget=select,label=State"`
	Serial string    `json:"serial,omitempty" yaml:"serial,omitempty" pk:"truth=asserted,owner=asset-db,label=Disk serial"`
}

var versions = []kindutil.Version{{Name: "v1beta1", Type: Jbod{}}}

// Kind returns the Jbod kind.
func Kind() kinds.Kind {
	return kinds.Kind{
		Name:      Name,
		Group:     Group,
		Version:   "v1beta1",
		Versions:  kindutil.Names(versions...),
		Own:       true,
		Paths:     []string{"control/inventory/jb-*.yaml"},
		NewPath:   func(name string) string { return "control/inventory/" + name + ".yaml" },
		Truth:     kinds.Intent,
		ApplyPath: kinds.ApplyManual,
		Views:     []string{"rack"},
		Schema:    kindutil.SchemaFunc(Group, Name, "A disk shelf attached to a storage node.", versions...),
		Validate:  validate,
		Describe:  describe,
	}
}

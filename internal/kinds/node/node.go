// Package node is the kind Node (inventory.pk.io): one physical machine with
// its location, hardware facts and cabled ports. It renders a metal.pk.io
// Machine for the cluster it belongs to.
package node

import (
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

const (
	Group = "inventory.pk.io"
	Name  = "Node"
	// MachineAPIVersion and MachineKind are the rendered object.
	MachineAPIVersion = "metal.pk.io/v1alpha1"
	MachineKind       = "Machine"
)

// Role of a node.
type Role string

const (
	RoleControl Role = "control"
	RoleWorker  Role = "worker"
	RoleStorage Role = "storage"
	RoleRouter  Role = "router"
)

// Node is inventory.pk.io/v1beta1 Node (control/inventory/<name>.yaml).
type Node struct {
	APIVersion string         `json:"apiVersion" yaml:"apiVersion" jsonschema:"enum=inventory.pk.io/v1beta1"`
	Kind       string         `json:"kind" yaml:"kind" jsonschema:"enum=Node"`
	Metadata   kinds.Metadata `json:"metadata" yaml:"metadata"`
	Spec       Spec           `json:"spec" yaml:"spec"`
}

// Spec of a Node.
type Spec struct {
	Cluster  string   `json:"cluster" yaml:"cluster" jsonschema:"title=Cluster,description=Cluster the node belongs to" pk:"truth=intent,ref=Cluster,widget=select,label=Cluster,group=Intent,order=1"`
	Role     Role     `json:"role" yaml:"role" jsonschema:"title=Role,enum=control,enum=worker,enum=storage,enum=router" pk:"truth=intent,widget=select,label=Role,group=Intent,order=2"`
	Location Location `json:"location" yaml:"location" jsonschema:"title=Location,description=Rack and lowest unit" pk:"truth=intent,widget=rack-picker,label=Location,group=Intent,order=3"`
	Model    string   `json:"model,omitempty" yaml:"model,omitempty" jsonschema:"title=Model" pk:"truth=asserted,owner=asset-db,label=Model,group=Hardware facts,order=10"`
	Serial   string   `json:"serial,omitempty" yaml:"serial,omitempty" jsonschema:"title=Serial" pk:"truth=asserted,owner=asset-db,label=Serial,group=Hardware facts,order=11"`
	BMC      BMC      `json:"bmc" yaml:"bmc" pk:"group=Hardware facts,label=BMC,order=12"`
	Ports    []Port   `json:"ports" yaml:"ports" jsonschema:"title=Ports and cables" pk:"truth=intent,label=Ports,group=Ports and cables,order=20"`
}

// Location in a rack. A node fills units Unit .. Unit+Size-1.
type Location struct {
	Rack string `json:"rack" yaml:"rack" jsonschema:"title=Rack" pk:"truth=intent,ref=Rack,label=Rack"`
	Unit int    `json:"unit" yaml:"unit" jsonschema:"title=Unit,minimum=1,maximum=47" pk:"truth=intent,label=Unit"`
	Size int    `json:"size" yaml:"size" jsonschema:"title=Height (U),minimum=1,maximum=8,default=2" pk:"truth=asserted,owner=asset-db,label=Height,unit=U"`
}

// BMC of a node.
type BMC struct {
	MAC string `json:"mac" yaml:"mac" jsonschema:"title=BMC MAC,pattern=^([0-9a-f]{2}:){5}[0-9a-f]{2}$" pk:"truth=asserted,owner=asset-db,label=BMC MAC"`
	// Fixture is the Redfish sim fixture, derived from the emulation profile.
	Fixture string `json:"fixture,omitempty" yaml:"fixture,omitempty" jsonschema:"title=Emulation fixture,readOnly=true" pk:"truth=derived,label=Emulation"`
}

// Port is one cabled port of a node.
type Port struct {
	Name    string   `json:"name" yaml:"name" jsonschema:"title=Port,description=bmc or p1 p2 ...,pattern=^(bmc|p[0-9]+)$" pk:"truth=intent,label=Port"`
	Cable   string   `json:"cable" yaml:"cable" jsonschema:"title=Cable,pattern=^[A-Z]+-[0-9]{4}$" pk:"truth=intent,label=Cable"`
	Peer    PortPeer `json:"peer" yaml:"peer" pk:"truth=intent,widget=port-picker,label=Peer"`
	Network string   `json:"network" yaml:"network" jsonschema:"title=Network,enum=mgmt,enum=data" pk:"truth=intent,widget=select,label=Network"`
}

// PortPeer is the switch port at the other end of the cable.
type PortPeer struct {
	Switch string `json:"switch" yaml:"switch" jsonschema:"title=Switch" pk:"truth=intent,ref=Switch,label=Switch"`
	Port   int    `json:"port" yaml:"port" jsonschema:"title=Switch port,minimum=1" pk:"truth=intent,label=Switch port"`
}

// NodeV1alpha1 is the old inventory.pk.io/v1alpha1 Node with flat rack, unit
// and height fields. Convert maps it to v1beta1 location.
type NodeV1alpha1 struct {
	APIVersion string         `json:"apiVersion" yaml:"apiVersion" jsonschema:"enum=inventory.pk.io/v1alpha1"`
	Kind       string         `json:"kind" yaml:"kind" jsonschema:"enum=Node"`
	Metadata   kinds.Metadata `json:"metadata" yaml:"metadata"`
	Spec       SpecV1alpha1   `json:"spec" yaml:"spec"`
}

// SpecV1alpha1 of the old Node.
type SpecV1alpha1 struct {
	Cluster string `json:"cluster" yaml:"cluster" pk:"truth=intent,ref=Cluster"`
	Role    Role   `json:"role" yaml:"role" jsonschema:"enum=control,enum=worker,enum=storage,enum=router" pk:"truth=intent"`
	Rack    string `json:"rack" yaml:"rack" pk:"truth=intent,ref=Rack"`
	Unit    int    `json:"unit" yaml:"unit" jsonschema:"minimum=1,maximum=47" pk:"truth=intent"`
	// Height defaults to 2 when empty.
	Height int    `json:"height,omitempty" yaml:"height,omitempty" jsonschema:"minimum=1,maximum=8" pk:"truth=asserted,owner=asset-db"`
	Model  string `json:"model,omitempty" yaml:"model,omitempty" pk:"truth=asserted,owner=asset-db"`
	Serial string `json:"serial,omitempty" yaml:"serial,omitempty" pk:"truth=asserted,owner=asset-db"`
	BMC    BMC    `json:"bmc" yaml:"bmc"`
	Ports  []Port `json:"ports" yaml:"ports" pk:"truth=intent"`
}

// Machine is the rendered metal.pk.io/v1alpha1 Machine.
type Machine struct {
	APIVersion string         `json:"apiVersion" yaml:"apiVersion"`
	Kind       string         `json:"kind" yaml:"kind"`
	Metadata   kinds.Metadata `json:"metadata" yaml:"metadata"`
	Spec       MachineSpec    `json:"spec" yaml:"spec"`
}

// MachineSpec is what the metal operator needs.
type MachineSpec struct {
	Role     Role          `json:"role" yaml:"role"`
	Location string        `json:"location" yaml:"location"` // "GC5A-6Z U6"
	BMC      MachineBMC    `json:"bmc" yaml:"bmc"`
	Ports    []MachinePort `json:"ports" yaml:"ports"`
}

// MachineBMC of a Machine.
type MachineBMC struct {
	MAC string `json:"mac" yaml:"mac"`
}

// MachinePort of a Machine.
type MachinePort struct {
	Name    string `json:"name" yaml:"name"`
	Network string `json:"network" yaml:"network"`
	Peer    string `json:"peer" yaml:"peer"` // "data-a 38"
}

var versions = []kindutil.Version{
	{Name: "v1alpha1", Type: NodeV1alpha1{}},
	{Name: "v1beta1", Type: Node{}},
}

// Kind returns the Node kind.
func Kind() kinds.Kind {
	return kinds.Kind{
		Name:        Name,
		Group:       Group,
		Version:     "v1beta1",
		Versions:    kindutil.Names(versions...),
		Own:         true,
		Description: "A physical machine: location, hardware facts and cabled ports.",
		Paths:       []string{"control/inventory/*.yaml"},
		NewPath:     func(name string) string { return "control/inventory/" + name + ".yaml" },
		Truth:       kinds.Intent,
		ApplyPath:   kinds.ApplyFlux,
		Views:       []string{"node", "rack"},
		Schema:      kindutil.SchemaFunc(Group, Name, "A physical machine: location, hardware facts and cabled ports.", versions...),
		Render:      render,
		Describe:    describe,
		Validate:    validate,
		Convert:     Convert,
		Skeleton:    Skeleton,
	}
}

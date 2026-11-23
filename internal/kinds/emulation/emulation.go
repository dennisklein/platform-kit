// Package emulation is the kind EmulationProfile (pk/v1alpha1,
// .pk/emulation.yaml): it maps inventory facts (model, role) to emulators.
package emulation

import (
	"path"
	"strings"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

const (
	Group = "pk"
	Name  = "EmulationProfile"
)

// EmulationProfile is pk/v1alpha1 EmulationProfile.
type EmulationProfile struct {
	APIVersion string         `json:"apiVersion" yaml:"apiVersion" jsonschema:"enum=pk/v1alpha1"`
	Kind       string         `json:"kind" yaml:"kind" jsonschema:"enum=EmulationProfile"`
	Metadata   kinds.Metadata `json:"metadata" yaml:"metadata"`
	Spec       Spec           `json:"spec" yaml:"spec"`
}

// Spec of an EmulationProfile.
type Spec struct {
	Machines []MachineRule      `json:"machines" yaml:"machines" pk:"truth=intent,label=Machines,order=1"`
	Networks map[string]Network `json:"networks" yaml:"networks" pk:"truth=intent,label=Networks,order=2"`
	// Substrate builds the networks (containerlab).
	Substrate string `json:"substrate,omitempty" yaml:"substrate,omitempty" jsonschema:"enum=containerlab,enum=bridges,default=containerlab" pk:"truth=intent,widget=select,label=Substrate,order=3"`
	// Uplink of a lab: nat only; never route into the real site.
	Uplink string `json:"uplink,omitempty" yaml:"uplink,omitempty" jsonschema:"enum=nat,enum=none,default=nat" pk:"truth=intent,widget=select,label=Uplink,order=4"`
}

// MachineRule maps machines that match to emulators. First match wins.
type MachineRule struct {
	Match   Match      `json:"match" yaml:"match"`
	BMC     BMCEmu     `json:"bmc" yaml:"bmc"`
	Machine MachineEmu `json:"machine" yaml:"machine"`
	Disks   []DiskEmu  `json:"disks,omitempty" yaml:"disks,omitempty"`
	Emulate *bool      `json:"emulate,omitempty" yaml:"emulate,omitempty"`
}

// Match selects machines by model glob and role.
type Match struct {
	Model string `json:"model,omitempty" yaml:"model,omitempty" jsonschema:"description=Glob over spec.model"`
	Role  string `json:"role,omitempty" yaml:"role,omitempty" jsonschema:"enum=control,enum=worker,enum=storage,enum=router"`
}

// BMCEmu is the Redfish sim fixture.
type BMCEmu struct {
	Fixture string `json:"fixture" yaml:"fixture" jsonschema:"description=Captured vendor fixture (supermicro-x12 or huawei-2288h-v5)"`
}

// MachineEmu is the machine backend.
type MachineEmu struct {
	Backend   string `json:"backend" yaml:"backend" jsonschema:"enum=qemu,enum=talos-docker,enum=none"`
	CPUs      int    `json:"cpus,omitempty" yaml:"cpus,omitempty" jsonschema:"minimum=1"`
	MemoryMiB int    `json:"memoryMiB,omitempty" yaml:"memoryMiB,omitempty" jsonschema:"minimum=512"`
	Firmware  string `json:"firmware,omitempty" yaml:"firmware,omitempty" jsonschema:"enum=ovmf,enum=seabios"`
}

// DiskEmu is an emulated disk.
type DiskEmu struct {
	Name    string `json:"name" yaml:"name"`
	SizeGiB int    `json:"sizeGiB" yaml:"sizeGiB" jsonschema:"minimum=1"`
}

// Network is an emulated network (a bridge per VLAN or segment).
type Network struct {
	VLAN    int      `json:"vlan,omitempty" yaml:"vlan,omitempty" jsonschema:"minimum=1,maximum=4094"`
	IPv4    *IPv4    `json:"ipv4,omitempty" yaml:"ipv4,omitempty"`
	IPv6    *IPv6    `json:"ipv6,omitempty" yaml:"ipv6,omitempty"`
	Emulate *bool    `json:"emulate,omitempty" yaml:"emulate,omitempty" jsonschema:"description=false for parts that cannot be emulated (InfiniBand)"`
	Routers []string `json:"routers,omitempty" yaml:"routers,omitempty" jsonschema:"description=FRR containers on this network"`
}

// IPv4 settings of a network.
type IPv4 struct {
	CIDR string `json:"cidr" yaml:"cidr"`
}

// IPv6 settings of a network.
type IPv6 struct {
	Prefix string `json:"prefix" yaml:"prefix"`
	RA     bool   `json:"ra,omitempty" yaml:"ra,omitempty"`
	DHCPv6 string `json:"dhcpv6,omitempty" yaml:"dhcpv6,omitempty"`
}

var versions = []kindutil.Version{{Name: "v1alpha1", Type: EmulationProfile{}}}

// Kind returns the EmulationProfile kind.
func Kind() kinds.Kind {
	return kinds.Kind{
		Name:      Name,
		Group:     Group,
		Version:   "v1alpha1",
		Versions:  kindutil.Names(versions...),
		Own:       true,
		Paths:     []string{".pk/emulation.yaml"},
		NewPath:   func(string) string { return ".pk/emulation.yaml" },
		Truth:     kinds.Intent,
		ApplyPath: kinds.ApplyNone,
		Schema:    kindutil.SchemaFunc(Group, Name, "Maps hardware models to emulators for labs.", versions...),
	}
}

// RuleIndex returns the index of the first machine rule that matches a model
// and role, or -1. Model patterns are path globs ("Supermicro SYS-*").
func (p *EmulationProfile) RuleIndex(model, role string) int {
	if p == nil {
		return -1
	}
	for i, r := range p.Spec.Machines {
		if r.Match.Model != "" {
			if ok, _ := path.Match(r.Match.Model, model); !ok {
				continue
			}
		}
		if r.Match.Role != "" && r.Match.Role != role {
			continue
		}
		return i
	}
	return -1
}

// Describe is the derived emulation of a machine in words, for example
// "qemu + Redfish sim supermicro-x12", or "" when nothing matches.
func (r MachineRule) Describe() string {
	if r.Emulate != nil && !*r.Emulate {
		return "not emulated"
	}
	var parts []string
	if r.Machine.Backend != "" && r.Machine.Backend != "none" {
		parts = append(parts, r.Machine.Backend)
	}
	if r.BMC.Fixture != "" {
		parts = append(parts, "Redfish sim "+r.BMC.Fixture)
	}
	return strings.Join(parts, " + ")
}

// Package labkind is the kind Lab (pk/v1alpha1, .pk/labs/<name>.yaml): a thin
// file that sets the purpose (twin or change) on top of a topology.
package labkind

import (
	"encoding/json"

	"github.com/invopop/jsonschema"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

const (
	Group = "pk"
	Name  = "Lab"
)

// Lab is pk/v1alpha1 Lab.
type Lab struct {
	APIVersion string         `json:"apiVersion" yaml:"apiVersion" jsonschema:"enum=pk/v1alpha1"`
	Kind       string         `json:"kind" yaml:"kind" jsonschema:"enum=Lab"`
	Metadata   kinds.Metadata `json:"metadata" yaml:"metadata"`
	Spec       Spec           `json:"spec" yaml:"spec"`
}

// Spec of a Lab.
type Spec struct {
	Purpose        string      `json:"purpose" yaml:"purpose" jsonschema:"enum=twin,enum=change" pk:"truth=intent,widget=select,label=Purpose,order=1"`
	Topology       TopologyRef `json:"topology" yaml:"topology" pk:"truth=intent,label=Topology,order=2"`
	Follow         *Ref        `json:"follow,omitempty" yaml:"follow,omitempty" jsonschema:"description=Twin: re-sync after each merge to this ref" pk:"truth=intent,label=Follows,order=3"`
	Overlay        string      `json:"overlay,omitempty" yaml:"overlay,omitempty" jsonschema:"description=Kustomize overlay next to the live one" pk:"truth=intent,label=Overlay,order=4"`
	SubstituteFrom string      `json:"substituteFrom,omitempty" yaml:"substituteFrom,omitempty" jsonschema:"description=Flux postBuild variables" pk:"truth=intent,label=Substitute from,order=5"`
	Fidelity       string      `json:"fidelity,omitempty" yaml:"fidelity,omitempty" jsonschema:"enum=scaled,enum=replica" pk:"truth=intent,widget=select,label=Fidelity,order=6"`
	Scale          *Scale      `json:"scale,omitempty" yaml:"scale,omitempty" pk:"truth=intent,label=Scale,order=7"`
	From           string      `json:"from,omitempty" yaml:"from,omitempty" jsonschema:"description=Start from a twin snapshot (name@snapshot)" pk:"truth=intent,label=From,order=8"`
	Base           *Ref        `json:"base,omitempty" yaml:"base,omitempty" pk:"truth=intent,label=Base,order=9"`
	Change         *Ref        `json:"change,omitempty" yaml:"change,omitempty" pk:"truth=intent,label=Change,order=10"`
	Provision      string      `json:"provision,omitempty" yaml:"provision,omitempty" jsonschema:"enum=direct,enum=metal" pk:"truth=intent,widget=select,label=Provision,order=11"`
	Host           string      `json:"host,omitempty" yaml:"host,omitempty" jsonschema:"description=Lab host or local" pk:"truth=intent,label=Host,order=12"`
	Expose         []string    `json:"expose,omitempty" yaml:"expose,omitempty" pk:"truth=intent,label=Expose,order=13"`
	Lifetime       *Lifetime   `json:"lifetime,omitempty" yaml:"lifetime,omitempty" pk:"truth=intent,label=Lifetime,order=14"`
	Snapshot       string      `json:"snapshot,omitempty" yaml:"snapshot,omitempty" jsonschema:"enum=nightly,enum=none" pk:"truth=intent,label=Snapshot,order=15"`
	Scenario       []Step      `json:"scenario,omitempty" yaml:"scenario,omitempty" pk:"truth=intent,label=Scenario,order=16"`
	Results        *Results    `json:"results,omitempty" yaml:"results,omitempty" pk:"truth=intent,label=Results,order=17"`
}

// Ref is a git ref.
type Ref struct {
	Ref string `json:"ref" yaml:"ref"`
}

// Scale selects a subset of machines and a cheap backend for a twin.
type Scale struct {
	Machines []string `json:"machines,omitempty" yaml:"machines,omitempty" pk:"ref=Node"`
	Backend  string   `json:"backend,omitempty" yaml:"backend,omitempty" jsonschema:"enum=qemu,enum=talos-docker,enum=none"`
}

// Results says what a run collects and how it reports.
type Results struct {
	Collect []string `json:"collect,omitempty" yaml:"collect,omitempty"`
	Report  string   `json:"report,omitempty" yaml:"report,omitempty" jsonschema:"enum=html,enum=markdown"`
}

// Step is one scenario step. Exactly one of Check, GitOps, Action, Fault,
// Wait or Exec is set.
type Step struct {
	Name   string  `json:"name" yaml:"name"`
	Check  *Check  `json:"check,omitempty" yaml:"check,omitempty"`
	GitOps *GitOps `json:"gitops,omitempty" yaml:"gitops,omitempty"`
	Action *Action `json:"action,omitempty" yaml:"action,omitempty"`
	Fault  *Fault  `json:"fault,omitempty" yaml:"fault,omitempty"`
	Wait   *Wait   `json:"wait,omitempty" yaml:"wait,omitempty"`
	Exec   *Exec   `json:"exec,omitempty" yaml:"exec,omitempty"`
}

// Check waits until conditions hold.
type Check struct {
	Flux    string `json:"flux,omitempty" yaml:"flux,omitempty" jsonschema:"enum=all-ready"`
	Nodes   string `json:"nodes,omitempty" yaml:"nodes,omitempty" jsonschema:"enum=all-ready"`
	BGP     string `json:"bgp,omitempty" yaml:"bgp,omitempty" jsonschema:"enum=established"`
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty" jsonschema:"pattern=^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"`
}

// GitOps switches the lab to a ref.
type GitOps struct {
	SwitchTo string `json:"switchTo" yaml:"switchTo" jsonschema:"enum=base,enum=change"`
}

// Action is an imperative action, for example a BMC power action.
type Action struct {
	BMC *BMCAction `json:"bmc,omitempty" yaml:"bmc,omitempty"`
}

// BMCAction is a Redfish power action on a machine.
type BMCAction struct {
	Machine string `json:"machine" yaml:"machine" pk:"ref=Node"`
	Power   string `json:"power" yaml:"power" jsonschema:"enum=On,enum=ForceOff,enum=ForceRestart,enum=GracefulRestart,enum=PowerCycle"`
}

// Fault injects a fault.
type Fault struct {
	// Link is a network whose links go down, for example data-b.
	Link     string   `json:"link,omitempty" yaml:"link,omitempty"`
	State    string   `json:"state,omitempty" yaml:"state,omitempty" jsonschema:"enum=down,enum=up"`
	Machines []string `json:"machines,omitempty" yaml:"machines,omitempty" pk:"ref=Node"`
}

// Wait pauses.
type Wait struct {
	Duration string `json:"duration" yaml:"duration" jsonschema:"pattern=^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"`
}

// Exec runs a container image for a custom check.
type Exec struct {
	Image   string   `json:"image" yaml:"image"`
	Command []string `json:"command,omitempty" yaml:"command,omitempty"`
}

// TopologyRef is the name of a generated topology (a cluster) or an inline topology.
type TopologyRef struct {
	Name   string
	Inline map[string]any
}

func (t TopologyRef) MarshalJSON() ([]byte, error) {
	if t.Inline != nil {
		return json.Marshal(map[string]any{"inline": t.Inline})
	}
	return json.Marshal(t.Name)
}

func (t *TopologyRef) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &t.Name)
	}
	var v struct {
		Inline map[string]any `json:"inline"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	t.Inline = v.Inline
	return nil
}

// JSONSchema describes the union.
func (TopologyRef) JSONSchema() *jsonschema.Schema {
	inline := jsonschema.NewProperties()
	inline.Set("inline", &jsonschema.Schema{Type: "object"})
	return &jsonschema.Schema{OneOf: []*jsonschema.Schema{
		{Type: "string", Description: "Generated topology of a cluster"},
		{Type: "object", Properties: inline, Required: []string{"inline"}},
	}}
}

// Lifetime is persistent or a TTL.
type Lifetime struct {
	Persistent    bool
	TTL           string
	KeepOnFailure bool
}

func (l Lifetime) MarshalJSON() ([]byte, error) {
	if l.Persistent {
		return json.Marshal("persistent")
	}
	return json.Marshal(map[string]any{"ttl": l.TTL, "keepOnFailure": l.KeepOnFailure})
}

func (l *Lifetime) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		l.Persistent = s == "persistent"
		return nil
	}
	var v struct {
		TTL           string `json:"ttl"`
		KeepOnFailure bool   `json:"keepOnFailure"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	l.TTL, l.KeepOnFailure = v.TTL, v.KeepOnFailure
	return nil
}

// JSONSchema describes the union.
func (Lifetime) JSONSchema() *jsonschema.Schema {
	ttl := jsonschema.NewProperties()
	ttl.Set("ttl", &jsonschema.Schema{Type: "string", Pattern: `^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$`})
	ttl.Set("keepOnFailure", &jsonschema.Schema{Type: "boolean"})
	return &jsonschema.Schema{OneOf: []*jsonschema.Schema{
		{Type: "string", Enum: []any{"persistent"}},
		{Type: "object", Properties: ttl},
	}}
}

var versions = []kindutil.Version{{Name: "v1alpha1", Type: Lab{}}}

// Kind returns the Lab kind.
func Kind() kinds.Kind {
	return kinds.Kind{
		Name:      Name,
		Group:     Group,
		Version:   "v1alpha1",
		Versions:  kindutil.Names(versions...),
		Own:       true,
		Paths:     []string{".pk/labs/*.yaml"},
		NewPath:   func(name string) string { return ".pk/labs/" + name + ".yaml" },
		Truth:     kinds.Intent,
		ApplyPath: kinds.ApplyNone,
		Views:     []string{"lab"},
		Schema:    kindutil.SchemaFunc(Group, Name, "A lab: a purpose (twin or change) on top of a topology.", versions...),
		Validate:  validate,
		Describe:  kindutil.Describer(Name, "lab"),
	}
}

// Package lab runs labs: it generates the topology from the desired state,
// runs scenarios on a driver and writes reports. The only driver in the demo
// is sim (simulated, laptop). qemu, talos-docker and containerlab drivers
// fit the same Driver interface later.
package lab

import (
	"context"
	"time"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/labkind"
)

// LabSpec is the spec of a Lab file (.pk/labs/<name>.yaml or generated).
type LabSpec = labkind.Spec

// LabFile is a Lab document.
type LabFile = labkind.Lab

// ScenarioStep is one step of a scenario.
type ScenarioStep = labkind.Step

// StepType of a run step.
type StepType string

const (
	StepTopology StepType = "topology"
	StepBMC      StepType = "bmc"
	StepInstall  StepType = "install"
	StepJoin     StepType = "join"
	StepGitOps   StepType = "gitops"
	StepCheck    StepType = "check"
	StepFault    StepType = "fault"
	StepWait     StepType = "wait"
	StepExec     StepType = "exec"
	StepReport   StepType = "report"
)

// StepStatus of a run step.
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepPass    StepStatus = "pass"
	StepFail    StepStatus = "fail"
	StepSkipped StepStatus = "skipped"
)

// Result of a run.
type Result string

const (
	ResultRunning   Result = "running"
	ResultPass      Result = "pass"
	ResultFail      Result = "fail"
	ResultCancelled Result = "cancelled"
)

// Check is one computed check of a step.
type Check struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// Step is one step of a run.
type Step struct {
	Index      int        `json:"index"`
	Name       string     `json:"name"`
	Type       StepType   `json:"type"`
	Title      string     `json:"title"`
	Detail     string     `json:"detail,omitempty"`
	Status     StepStatus `json:"status"`
	StartedAt  time.Time  `json:"startedAt,omitzero"`
	FinishedAt time.Time  `json:"finishedAt,omitzero"`
	Output     []string   `json:"output,omitempty"`
	Checks     []Check    `json:"checks,omitempty"`
	// Scenario marks a step from the scenario (not a fixed stage).
	Scenario bool `json:"scenario,omitempty"`
	// SimulatedMs is the lab time the step stands for.
	SimulatedMs int64 `json:"simulatedMs,omitempty"`
}

// Run is one lab run. Raw results are kept in <data>/labs/<change>/run-<n>/.
type Run struct {
	ID         string    `json:"id"` // c-k7m2qz/run-1
	ChangeID   string    `json:"changeId,omitempty"`
	Number     int       `json:"number"`
	Lab        string    `json:"lab"`
	SHA        string    `json:"sha"`
	Ref        string    `json:"ref"`
	Scenario   string    `json:"scenario"`
	Driver     string    `json:"driver"`
	Host       string    `json:"host"`
	Provision  string    `json:"provision"`
	Fidelity   string    `json:"fidelity"`
	Fresh      bool      `json:"fresh"`
	Result     Result    `json:"result"`
	Summary    string    `json:"summary,omitempty"`
	Steps      []Step    `json:"steps"`
	Report     string    `json:"report,omitempty"`
	By         string    `json:"by,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	// Cluster the lab emulates and the machines under test.
	Cluster string   `json:"cluster,omitempty"`
	Nodes   []string `json:"nodes,omitempty"`
	// GatePaths are the path globs whose lab gate the run serves.
	GatePaths []string `json:"gatePaths,omitempty"`
	// Failures are the details of failed checks.
	Failures     []string `json:"failures,omitempty"`
	ChecksPassed int      `json:"checksPassed"`
	ChecksTotal  int      `json:"checksTotal"`
	SimulatedMs  int64    `json:"simulatedMs,omitempty"`
	// LabObject is the generated Lab (purpose change).
	LabObject *LabFile `json:"labObject,omitempty"`
}

// Event is a step update or the end of a run.
type Event struct {
	Run  string
	Step *Step // nil on the final event
	Done bool
	// Result and Summary are set on the final event.
	Result Result
}

// Topology is generated from control/inventory, control/cabling.yaml and
// .pk/emulation.yaml. Nobody writes it by hand.
type Topology struct {
	Name      string            `json:"name"`
	Cluster   string            `json:"cluster,omitempty"`
	Ref       string            `json:"ref"`
	SHA       string            `json:"sha,omitempty"`
	Substrate string            `json:"substrate"`
	Uplink    string            `json:"uplink"`
	Machines  []TopologyMachine `json:"machines"`
	Networks  []TopologyNetwork `json:"networks"`
	Links     []TopologyLink    `json:"links"`
	Problems  []kinds.Problem   `json:"problems,omitempty"`
}

// TopologyMachine is one emulated machine (BMC, machine backend, NICs).
type TopologyMachine struct {
	Name    string        `json:"name"`
	Cluster string        `json:"cluster,omitempty"`
	Role    string        `json:"role,omitempty"`
	Model   string        `json:"model,omitempty"`
	Rack    string        `json:"rack,omitempty"`
	Unit    int           `json:"unit,omitempty"`
	Emulate bool          `json:"emulate"`
	BMC     TopologyBMC   `json:"bmc"`
	Machine TopologyHost  `json:"machine"`
	NICs    []TopologyNIC `json:"nics"`
}

// TopologyBMC is the Redfish sim of a machine.
type TopologyBMC struct {
	Fixture string `json:"fixture"`
	MAC     string `json:"mac,omitempty"`
	Network string `json:"network"`
	Port    string `json:"port,omitempty"` // "mgmt 37"
}

// TopologyHost is the machine backend.
type TopologyHost struct {
	Backend   string         `json:"backend"`
	CPUs      int            `json:"cpus,omitempty"`
	MemoryMiB int            `json:"memoryMiB,omitempty"`
	Firmware  string         `json:"firmware,omitempty"`
	Disks     []TopologyDisk `json:"disks,omitempty"`
}

// TopologyDisk is an emulated disk.
type TopologyDisk struct {
	Name    string `json:"name"`
	SizeGiB int    `json:"sizeGiB"`
	Serial  string `json:"serial,omitempty"`
}

// TopologyNIC is a NIC on a network.
type TopologyNIC struct {
	Name    string `json:"name"`
	Network string `json:"network"`
	MAC     string `json:"mac,omitempty"`
	Peer    string `json:"peer,omitempty"` // "data-a 38"
	Cable   string `json:"cable,omitempty"`
}

// TopologyNetwork is an emulated network.
type TopologyNetwork struct {
	Name       string `json:"name"`
	VLAN       int    `json:"vlan,omitempty"`
	CIDR       string `json:"cidr,omitempty"`
	IPv6Prefix string `json:"ipv6Prefix,omitempty"`
	Emulate    bool   `json:"emulate"`
}

// TopologyLink is a cable between two ports.
type TopologyLink struct {
	A     string `json:"a"`
	B     string `json:"b"`
	Cable string `json:"cable,omitempty"`
}

// Job is what a driver runs: the Lab (scenario), the generated topology
// and the desired state of the base and the change.
type Job struct {
	Run      *Run
	Spec     LabSpec
	Topology *Topology
	// Base and Head are the desired state before and with the change. The
	// sim driver computes its checks on them; a real driver applies Head
	// and observes the machines.
	Base, Head *Model
	// Nodes are the machines under test.
	Nodes []string
}

// Driver runs labs on one kind of host. The sim driver (laptop) is the only
// one in the demo; qemu, talos-docker and containerlab drivers fit here later.
type Driver interface {
	// Name is the driver name (sim).
	Name() string
	// Start runs a job in the background. It sends a copy of each step
	// when the step changes and a final event; then it closes the channel.
	Start(ctx context.Context, job Job) (<-chan Event, error)
	// Cancel stops a run.
	Cancel(runID string) error
}

// Service is what the bench uses: change labs, lab definitions, topology.
type Service interface {
	// RunChange starts a change lab on the head of a change. The acting
	// identity comes from ctx. Progress goes out as lab.step events; at the
	// end the service writes the report into the change, records the run in
	// the metadata and posts the pk/lab commit status.
	RunChange(ctx context.Context, changeID, scenario string) (*Run, error)
	Get(ctx context.Context, runID string) (*Run, error)
	// Runs lists runs, newest first (all changes when changeID is "").
	Runs(ctx context.Context, changeID string) ([]Run, error)
	// Definitions lists the Lab files on main.
	Definitions(ctx context.Context) ([]LabFile, error)
	// Topology generates the topology of a ref (and optionally one cluster).
	Topology(ctx context.Context, ref, cluster string) (*Topology, error)
	// Scenarios lists the built-in scenario names.
	Scenarios() []string
}

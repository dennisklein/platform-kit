package lab

import (
	"encoding/json"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/kinds"
)

// RunToAPI converts a run.
func RunToAPI(r *Run) api.LabRun {
	out := api.LabRun{
		Id: r.ID, ChangeId: r.ChangeID, Number: r.Number, Lab: r.Lab, Sha: r.SHA, Ref: r.Ref, Scenario: r.Scenario,
		Driver: r.Driver, Host: r.Host, Provision: api.LabRunProvision(r.Provision), Fidelity: api.LabRunFidelity(r.Fidelity),
		Fresh: r.Fresh, Result: api.LabResult(r.Result), Summary: r.Summary, ChecksPassed: r.ChecksPassed,
		ChecksTotal: r.ChecksTotal, Steps: make([]api.LabStep, 0, len(r.Steps)), Report: r.Report, By: r.By,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, SimulatedMs: r.SimulatedMs, Cluster: r.Cluster,
		Nodes: r.Nodes, GatePaths: r.GatePaths, Failures: r.Failures,
	}
	if !r.FinishedAt.IsZero() {
		out.DurationMs = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
	}
	for _, s := range r.Steps {
		out.Steps = append(out.Steps, StepToAPI(s))
	}
	if r.LabObject != nil {
		if b, err := json.Marshal(r.LabObject); err == nil {
			_ = json.Unmarshal(b, &out.LabObject)
		}
	}
	return out
}

// StepToAPI converts a step.
func StepToAPI(s Step) api.LabStep {
	out := api.LabStep{Index: s.Index, Name: s.Name, Type: api.LabStepType(s.Type), Title: s.Title, Detail: s.Detail,
		Status: api.LabStepStatus(s.Status), StartedAt: s.StartedAt, FinishedAt: s.FinishedAt, Output: s.Output,
		Scenario: s.Scenario}
	if !s.StartedAt.IsZero() && !s.FinishedAt.IsZero() {
		out.DurationMs = s.FinishedAt.Sub(s.StartedAt).Milliseconds()
	}
	for _, c := range s.Checks {
		out.Checks = append(out.Checks, api.LabCheck{Name: c.Name, Ok: c.OK, Detail: c.Detail, Subject: c.Subject})
	}
	return out
}

// StepEvent is the payload of lab.step (API schema LabStepEvent; the
// generator skips schemas no operation uses).
type StepEvent struct {
	Run      string      `json:"run"`
	ChangeID string      `json:"changeId,omitempty"`
	Step     api.LabStep `json:"step"`
}

// StepEventToAPI is the payload of lab.step.
func StepEventToAPI(runID, changeID string, s Step) StepEvent {
	return StepEvent{Run: runID, ChangeID: changeID, Step: StepToAPI(s)}
}

// TopologyToAPI converts a topology (same JSON shape).
func TopologyToAPI(t *Topology) api.Topology {
	var out api.Topology
	if b, err := json.Marshal(t); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	if out.Machines == nil {
		out.Machines = []api.TopologyMachine{}
	}
	for i := range out.Machines {
		if out.Machines[i].Nics == nil {
			out.Machines[i].Nics = out.Machines[i].Nics[:0:0]
		}
	}
	if out.Networks == nil {
		out.Networks = []api.TopologyNetwork{}
	}
	if out.Links == nil {
		out.Links = []api.TopologyLink{}
	}
	out.Problems = ProblemsToAPI(t.Problems)
	return out
}

// ProblemsToAPI converts problems.
func ProblemsToAPI(ps []kinds.Problem) []api.Problem {
	out := make([]api.Problem, 0, len(ps))
	for _, p := range ps {
		out = append(out, api.Problem{Path: p.Path, Line: p.Line, Col: p.Col, Severity: api.ProblemSeverity(p.Severity),
			Message: p.Message, Source: api.ProblemSource(p.Source), Field: p.Field, Code: p.Code})
	}
	return out
}

// DefinitionToAPI converts a Lab file.
func DefinitionToAPI(l LabFile) api.LabDefinition {
	out := api.LabDefinition{Name: l.Metadata.Name, Purpose: api.LabDefinitionPurpose(l.Spec.Purpose),
		Path: ".pk/labs/" + l.Metadata.Name + ".yaml", Topology: l.Spec.Topology.Name}
	if l.Spec.Follow != nil {
		out.Follow = l.Spec.Follow.Ref
	}
	if l.Spec.Purpose == "twin" {
		out.Cluster = l.Metadata.Name
	}
	if b, err := json.Marshal(l.Spec); err == nil {
		_ = json.Unmarshal(b, &out.Spec)
	}
	return out
}

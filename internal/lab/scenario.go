package lab

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/dennisklein/platform-kit/internal/kinds/labkind"
)

// Built-in scenarios.
const (
	// ScenarioMetal installs fresh machines through the emulated BMCs and
	// runs node, BGP, BMC and link fault checks (inventory, Talos and
	// network changes).
	ScenarioMetal = "metal"
	// ScenarioGitOps starts from the twin snapshot, switches Flux to the
	// change and checks that everything gets ready (app changes).
	ScenarioGitOps = "gitops"
)

// MetalPaths are the paths that need the metal scenario.
var MetalPaths = []string{"control/inventory/**", "control/cabling.yaml", "control/talos/**", "clusters/*/infra/**", ".pk/emulation.yaml"}

// DefaultScenario picks the scenario for the changed paths.
func DefaultScenario(paths []string) string {
	for _, p := range paths {
		for _, g := range MetalPaths {
			if ok, _ := doublestar.Match(g, p); ok {
				return ScenarioMetal
			}
		}
	}
	return ScenarioGitOps
}

// ScenarioSteps returns the steps of a built-in scenario. nodes are the
// machines under test; fault is the network the link fault takes down.
func ScenarioSteps(name string, nodes []string, fault string) ([]ScenarioStep, error) {
	switch name {
	case ScenarioMetal:
		steps := []ScenarioStep{
			{Name: "nodes-ready", Check: &labkind.Check{Nodes: "all-ready", Timeout: "10m"}},
			{Name: "bgp-up", Check: &labkind.Check{BGP: "established", Timeout: "10m"}},
		}
		if len(nodes) > 0 {
			last := nodes[len(nodes)-1]
			steps = append(steps, ScenarioStep{Name: "power-cycle-" + last,
				Action: &labkind.Action{BMC: &labkind.BMCAction{Machine: last, Power: "PowerCycle"}}})
		}
		if fault != "" {
			steps = append(steps, ScenarioStep{Name: fault + "-down", Fault: &labkind.Fault{Link: fault, State: "down"}})
		}
		steps = append(steps, ScenarioStep{Name: "still-healthy",
			Check: &labkind.Check{Flux: "all-ready", BGP: "established", Timeout: "15m"}})
		return steps, nil
	case ScenarioGitOps:
		return []ScenarioStep{
			{Name: "baseline-healthy", Check: &labkind.Check{Flux: "all-ready"}},
			{Name: "apply-change", GitOps: &labkind.GitOps{SwitchTo: "change"}},
			{Name: "flux-ready", Check: &labkind.Check{Flux: "all-ready", Timeout: "5m"}},
			{Name: "soak", Wait: &labkind.Wait{Duration: "1m"}},
			{Name: "still-healthy", Check: &labkind.Check{Flux: "all-ready"}},
		}, nil
	}
	return nil, fmt.Errorf("unknown scenario %q (built-in: %s, %s, or a Lab file in .pk/labs)", name, ScenarioMetal, ScenarioGitOps)
}

// FaultNetwork picks the network a link fault takes down: the second data
// network of the machines under test (data-b), so traffic can move.
func FaultNetwork(m *Model, nodes []string) string {
	seen := map[string]bool{}
	var nets []string
	for _, n := range nodes {
		for _, d := range m.DataNetworks(n) {
			if !seen[d] {
				seen[d] = true
				nets = append(nets, d)
			}
		}
	}
	sort.Strings(nets)
	if len(nets) == 0 {
		return ""
	}
	return nets[len(nets)-1]
}

// StepType returns the run step type of a scenario step.
func StepTypeOf(s ScenarioStep) StepType {
	switch {
	case s.Check != nil:
		return StepCheck
	case s.GitOps != nil:
		return StepGitOps
	case s.Action != nil && s.Action.BMC != nil:
		return StepBMC
	case s.Fault != nil:
		return StepFault
	case s.Wait != nil:
		return StepWait
	case s.Exec != nil:
		return StepExec
	}
	return StepCheck
}

// StepTitle is the short title of a scenario step.
func StepTitle(s ScenarioStep) string {
	switch {
	case s.Check != nil:
		var parts []string
		if s.Check.Flux != "" {
			parts = append(parts, "Flux")
		}
		if s.Check.Nodes != "" {
			parts = append(parts, "nodes Ready")
		}
		if s.Check.BGP != "" {
			parts = append(parts, "BGP")
		}
		if len(parts) == 1 {
			return parts[0]
		}
		return "checks"
	case s.GitOps != nil:
		return "switch to " + s.GitOps.SwitchTo
	case s.Action != nil && s.Action.BMC != nil:
		return powerWords(s.Action.BMC.Power) + " " + s.Action.BMC.Machine
	case s.Fault != nil:
		state := s.Fault.State
		if state == "" {
			state = "down"
		}
		return s.Fault.Link + " link " + state
	case s.Wait != nil:
		return "wait " + s.Wait.Duration
	case s.Exec != nil:
		return "exec " + s.Exec.Image
	}
	return s.Name
}

func powerWords(p string) string {
	switch p {
	case "ForceRestart", "GracefulRestart":
		return "restart"
	case "ForceOff":
		return "power off"
	case "On":
		return "power on"
	}
	return "power cycle"
}

// Plan returns the steps of a run: the fixed stages (topology, BMC,
// install, join), the scenario steps and the report.
func Plan(spec LabSpec, cluster string) []Step {
	var steps []Step
	add := func(s Step) {
		s.Index = len(steps)
		s.Status = StepPending
		steps = append(steps, s)
	}
	add(Step{Name: "topology", Type: StepTopology, Title: "topology", Detail: "generate"})
	if cluster != "" {
		if spec.Provision == "metal" {
			add(Step{Name: "bmc", Type: StepBMC, Title: "BMC", Detail: "Redfish sim"})
			add(Step{Name: "install", Type: StepInstall, Title: "install", Detail: "Talos"})
			add(Step{Name: "join", Type: StepJoin, Title: "join", Detail: cluster})
		} else {
			add(Step{Name: "restore", Type: StepInstall, Title: "restore", Detail: "twin snapshot"})
		}
		for _, s := range spec.Scenario {
			add(Step{Name: s.Name, Type: StepTypeOf(s), Title: StepTitle(s), Scenario: true})
		}
	}
	add(Step{Name: "report", Type: StepReport, Title: "report", Detail: "pending"})
	return steps
}

// ParseDuration parses a Lab duration (5m, 1h30m, 10s).
func ParseDuration(s string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return d
}

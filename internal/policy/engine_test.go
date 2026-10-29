package policy

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/kinds"
)

func seedPolicy(t *testing.T) *Policy {
	t.Helper()
	b, err := os.ReadFile("../../seed/state/.pk/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, problems, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range problems {
		t.Errorf("seed policy problem: line %d: %s", pr.Line, pr.Message)
	}
	return p
}

func id(t *testing.T, p *Policy, user string) identity.Identity {
	t.Helper()
	i, ok := p.Identify(user)
	if !ok {
		t.Fatalf("unknown %s", user)
	}
	return i
}

func obj(kind, ns, name string) kinds.ObjectRef {
	return kinds.ObjectRef{Cluster: "defiant", APIVersion: "x/v1", Kind: kind, Namespace: ns, Name: name}
}

// The demo change: two nodes and the BGP policy, by dennis and his agent.
func demoInput(t *testing.T, p *Policy) GateInput {
	return GateInput{
		ChangeID: "c-k7m2qz",
		Author:   id(t, p, "dennis"),
		Writers:  []identity.Identity{id(t, p, "dennis"), id(t, p, "dennis-agent")},
		Paths: []string{
			".pk/changes/c-k7m2qz.yaml",
			"control/inventory/cn-035.yaml",
			"control/inventory/cn-036.yaml",
			"clusters/defiant/infra/cilium/bgp-rack-6z.yaml",
			"kb/labs/c-k7m2qz-run-1.md",
		},
		Objects: []kinds.ObjectRef{obj("Machine", "", "cn-035"), obj("Machine", "", "cn-036"), obj("CiliumBGPPeeringPolicy", "", "rack-6z")},
		Lab:     LabState{Status: "pass", Run: "c-k7m2qz/run-1", SHA: "abc", CoversHead: true},
		Target:  "main",
	}
}

func TestDemoChangeGates(t *testing.T) {
	p := seedPolicy(t)
	in := demoInput(t, p)
	g := p.Evaluate(in)
	if g.Risk.Level != RiskHigh {
		t.Errorf("risk %s", g.Risk.Level)
	}
	if g.Risk.Reasons[0].Reason != "a change to a node is high" {
		t.Errorf("first reason %+v", g.Risk.Reasons)
	}
	if g.Approvals.Need != 2 || g.Approvals.Have != 0 || !g.LabGate.Required || g.LabGate.Status != "met" {
		t.Errorf("approvals %+v lab %+v", g.Approvals, g.LabGate)
	}
	if !g.PathRights.OK {
		t.Errorf("path rights: %+v", g.PathRights)
	}
	if !slices.Equal(g.Approvals.Eligible, []string{"anna", "jonas"}) {
		t.Errorf("eligible %v", g.Approvals.Eligible)
	}
	names := []string{}
	for _, r := range g.MatchedRules {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"inventory", "network", "docs"}) {
		t.Errorf("matched rules %v", names)
	}
	if g.Mergeable {
		t.Error("mergeable without approvals")
	}

	in.Reviews = []Review{{User: "dennis-agent"}, {User: "dennis"}, {User: "anna"}, {User: "anna"}, {User: "mara"}}
	g = p.Evaluate(in)
	if g.Approvals.Have != 1 || len(g.Approvals.Ignored) != 3 {
		t.Errorf("have %d ignored %+v", g.Approvals.Have, g.Approvals.Ignored)
	}
	reasons := map[string]string{}
	for _, a := range g.Approvals.Ignored {
		reasons[a.User] = a.Reason
	}
	if reasons["dennis-agent"] != "agents never approve" || reasons["dennis"] == "" || reasons["mara"] == "" {
		t.Errorf("reasons %v", reasons)
	}

	in.Reviews = append(in.Reviews, Review{User: "jonas"})
	g = p.Evaluate(in)
	if !g.Mergeable || g.Approvals.Have != 2 {
		t.Errorf("want mergeable: %v", g.Blockers)
	}

	in.Behind = 1
	g = p.Evaluate(in)
	if g.Mergeable || g.Rebased.OK || !strings.Contains(strings.Join(g.Blockers, ";"), "behind main by 1, rebase first") {
		t.Errorf("behind: %v", g.Blockers)
	}

	in.Behind = 0
	in.Lab = LabState{Status: "pass", Run: "c-k7m2qz/run-1", SHA: "old", CoversHead: false}
	g = p.Evaluate(in)
	if g.LabGate.Status != "needed" || g.Mergeable {
		t.Errorf("old lab run: %+v", g.LabGate)
	}
	// After a rebase the change service may report the old run without a status.
	in.Lab = LabState{Run: "c-k7m2qz/run-1", SHA: "old"}
	if g = p.Evaluate(in); g.LabGate.Status != "needed" || !strings.Contains(g.LabGate.Detail, "older head") {
		t.Errorf("rebased lab run: %+v", g.LabGate)
	}
	in.Lab = LabState{Status: "fail", Run: "c-k7m2qz/run-2", CoversHead: true}
	if g = p.Evaluate(in); g.LabGate.Status != "failed" {
		t.Errorf("failed lab: %+v", g.LabGate)
	}
}

func TestAppRiskLow(t *testing.T) {
	p := seedPolicy(t)
	g := p.Evaluate(GateInput{
		Author:  id(t, p, "jonas"),
		Paths:   []string{"clusters/defiant/apps/podinfo/deployment.yaml"},
		Objects: []kinds.ObjectRef{obj("Deployment", "podinfo", "podinfo")},
		Reviews: []Review{{User: "mara"}},
	})
	if g.Risk.Level != RiskLow || g.Approvals.Need != 1 || g.LabGate.Required || g.LabGate.Status != "na" {
		t.Errorf("risk %s need %d lab %+v", g.Risk.Level, g.Approvals.Need, g.LabGate)
	}
	if !g.Mergeable {
		t.Errorf("mara (team podinfo) approves the app change: %v", g.Blockers)
	}
	// A cilium HelmRelease in an app path is high: 2 approvals and the lab.
	g = p.Evaluate(GateInput{
		Author:  id(t, p, "jonas"),
		Paths:   []string{"clusters/defiant/apps/podinfo/x.yaml"},
		Objects: []kinds.ObjectRef{obj("HelmRelease", "kube-system", "cilium")},
	})
	if g.Risk.Level != RiskHigh || g.Approvals.Need != 2 || !g.LabGate.Required {
		t.Errorf("cilium release: %+v %+v", g.Risk, g.Approvals)
	}
}

func TestPathRights(t *testing.T) {
	p := seedPolicy(t)
	e := NewEngine(nil)
	check := func(user, path string, want bool) {
		t.Helper()
		r := e.CanWrite(p, id(t, p, user), []string{path})
		if len(r) != 1 || r[0].OK != want {
			t.Errorf("%s writes %s: %+v, want ok=%v", user, path, r, want)
		}
	}
	check("mara", "clusters/defiant/apps/podinfo/deployment.yaml", true)
	check("mara", "control/inventory/cn-035.yaml", false)
	check("jonas", ".pk/policy.yaml", false)
	check("dennis", ".pk/policy.yaml", true)
	check("dennis-agent", ".pk/policy.yaml", false)
	check("dennis-agent", "control/inventory/cn-035.yaml", true)
	check("sync-bot", "control/inventory/cn-027.yaml", true)
	check("sync-bot", "kb/runbooks/add-node.md", false)
	check("vera", "kb/runbooks/add-node.md", false)
	if r := e.CanWrite(p, id(t, p, "vera"), []string{".pk/changes/c-k7m2qz.yaml"}); len(r) != 0 {
		t.Errorf("metadata files match no rule: %+v", r)
	}
}

func TestLoadProblems(t *testing.T) {
	src := `apiVersion: pk/v1alpha1
kind: Policy
people:
  - {id: dennis, name: D, email: d@pk.local, roles: [owner]}
agents:
  - {id: x-agent, actsFor: nobody, can: [push]}
rules:
  - name: all
    paths: ["**"]
    writers: [owner, wizard]
    approvers: [owner, agent]
    approvals: 2
    risk: extreme
`
	_, problems, err := Load([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, p := range problems {
		msgs = append(msgs, p.Message)
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{"rules[0].risk: must be one of low, medium, high", "acts for nobody", "writer wizard", "agents and bots never approve", "only 1 people can approve"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	for _, p := range problems {
		if p.Line == 0 {
			t.Errorf("no line: %+v", p)
		}
	}
}

func TestLabCoversHead(t *testing.T) {
	if !LabCoversHead("c-k7m2qz", []string{".pk/changes/c-k7m2qz.yaml", "kb/labs/c-k7m2qz-run-1.md"}) {
		t.Error("report and metadata only should cover")
	}
	if LabCoversHead("c-k7m2qz", []string{"kb/labs/c-other1-run-1.md"}) {
		t.Error("another change's report does not cover")
	}
	if LabCoversHead("c-k7m2qz", []string{"control/inventory/cn-035.yaml"}) {
		t.Error("an inventory edit does not cover")
	}
}

func TestForgeRules(t *testing.T) {
	p := seedPolicy(t)
	fr := p.ForgeRules()
	bp := fr.BranchProtection
	if !slices.Equal(bp.MergeWhitelist, []string{"dennis", "anna", "jonas"}) || !slices.Equal(bp.RequiredChecks, []string{"pk/policy"}) || !bp.BlockOnOutdated || bp.EnablePush {
		t.Errorf("branch protection %+v", bp)
	}
	if !slices.Contains(bp.ApprovalsWhitelist, "mara") || slices.Contains(bp.ApprovalsWhitelist, "vera") {
		t.Errorf("approvals whitelist %v", bp.ApprovalsWhitelist)
	}
	co := fr.CodeOwners
	if !strings.Contains(co, `^control/inventory/.*$ @dennis @anna @jonas`) || !strings.Contains(co, `^\.pk/policy\.yaml$ @dennis @anna`) {
		t.Errorf("codeowners:\n%s", co)
	}
	// The default rule comes first (Gitea: last match wins).
	if strings.Index(co, "rule default") > strings.Index(co, "rule policy") {
		t.Errorf("order:\n%s", co)
	}
	if g := GlobRegexp("clusters/*/infra/cilium/**"); g != `^clusters/[^/]*/infra/cilium/.*$` {
		t.Errorf("glob %s", g)
	}
}

func TestFastPath(t *testing.T) {
	p := seedPolicy(t)
	in := demoInput(t, p)
	in.Revert = true
	in.Lab = LabState{}
	in.Reviews = []Review{{User: "anna"}}
	g := p.Evaluate(in)
	if !g.FastPath || g.Approvals.Need != 1 || g.LabGate.Required || !g.Mergeable {
		t.Errorf("fast path %+v %v", g.Approvals, g.Blockers)
	}
}

package advisor

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readSeed(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "seed", "state", p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const bgpPath = "clusters/defiant/infra/cilium/bgp-rack-6z.yaml"

func cn(name string, unit int, ports string) string {
	return `apiVersion: inventory.pk.io/v1beta1
kind: Node
metadata:
  name: ` + name + `
spec:
  cluster: defiant
  role: worker
  location: {rack: GC5A-6Z, unit: ` + itoa(unit) + `, size: 2}
  bmc: {mac: "3c:ec:ef:a1:0c:35"}
  ports:
` + ports
}

func itoa(n int) string {
	return strings.TrimSpace(strings.Repeat(" ", 0) + string(rune('0'+n/10)) + string(rune('0'+n%10)))
}

const cn035Ports = `    - {name: bmc, cable: MGT-0141, peer: {switch: mgmt, port: 37}, network: mgmt}
    - {name: p1, cable: DATA-0201, peer: {switch: data-b, port: 37}, network: data}
    - {name: p2, cable: DATA-0202, peer: {switch: data-a, port: 38}, network: data}
`

func TestParseAndGaps(t *testing.T) {
	pols := ParseBGP(bgpPath, readSeed(t, bgpPath))
	if len(pols) != 1 || pols[0].Name != "rack-6z" || pols[0].Rack != "GC5A-6Z" || pols[0].Cluster != "defiant" || len(pols[0].Hostnames) != 10 {
		t.Fatalf("policy %+v", pols)
	}
	nodes := append(ParseNodes("control/inventory/cn-035.yaml", cn("cn-035", 6, cn035Ports)),
		ParseNodes("control/inventory/cn-036.yaml", cn("cn-036", 8, cn035Ports))...)
	nodes = append(nodes, ParseNodes("control/inventory/cn-025.yaml", readSeed(t, "control/inventory/cn-025.yaml"))...)
	if len(nodes) != 3 || nodes[0].Rack != "GC5A-6Z" || nodes[0].Unit != 6 || len(nodes[0].DataPorts()) != 2 {
		t.Fatalf("nodes %+v", nodes)
	}
	gaps := FindBGPGaps(nodes, pols)
	if len(gaps) != 1 || !reflect.DeepEqual(gaps[0].Names(), []string{"cn-035", "cn-036"}) {
		t.Fatalf("gaps %+v", gaps)
	}
	// cn-003 is in GC5A-6Y: no gap.
	n3 := ParseNodes("control/inventory/cn-003.yaml", readSeed(t, "control/inventory/cn-003.yaml"))
	if g := FindBGPGaps(n3, pols); len(g) != 0 {
		t.Fatalf("cn-003 gap %+v", g)
	}
}

func TestAddHostnames(t *testing.T) {
	in := readSeed(t, bgpPath)
	out, err := AddHostnames(in, "rack-6z", []string{"cn-035", "cn-036", "cn-034"})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(in, "          - cn-034\n", "          - cn-034\n          - cn-035\n          - cn-036\n", 1)
	if out != want {
		t.Fatalf("got\n%s", out)
	}
	pols := ParseBGP(bgpPath, out)
	if !pols[0].Selects("cn-036") || len(pols[0].Peers) != 1 {
		t.Fatalf("parse back %+v", pols)
	}
	same, _ := AddHostnames(out, "rack-6z", []string{"cn-035"})
	if same != out {
		t.Fatal("re-adding changed the file")
	}
	if _, err := AddHostnames(in, "rack-9x", []string{"cn-035"}); err == nil {
		t.Fatal("unknown policy accepted")
	}
	flow := "apiVersion: cilium.io/v2alpha1\nkind: CiliumBGPPeeringPolicy\nmetadata: {name: p}\nspec:\n  nodeSelector:\n    matchExpressions:\n      - {key: kubernetes.io/hostname, operator: In, values: [cn-001]}\n"
	out, err = AddHostnames(flow, "p", []string{"cn-002"})
	if err != nil || !strings.Contains(out, "values: [cn-001, cn-002]}") {
		t.Fatalf("flow: %v\n%s", err, out)
	}
}

func TestScopeAndText(t *testing.T) {
	for p, want := range map[string]string{
		"control/inventory/cn-035.yaml":                  "inventory",
		"clusters/defiant/infra/cilium/bgp-rack-6z.yaml": "cilium",
		"clusters/defiant/apps/podinfo/deployment.yaml":  "podinfo",
		".pk/policy.yaml":                                "policy",
		"kb/runbooks/add-node.md":                        "kb",
		"control/cabling.yaml":                           "cabling",
	} {
		if got := ScopeOf(p); got != want {
			t.Errorf("%s: %s, want %s", p, got, want)
		}
	}
	if got := EntityNames("Add cn-035 and cn-036 to defiant, c-k7m2qz w-114 sn-05 cn-035"); !reflect.DeepEqual(got, []string{"cn-035", "cn-036", "sn-05"}) {
		t.Errorf("names %v", got)
	}
	if got := Units("in GC5A-6Z U6 and U8"); !reflect.DeepEqual(got, []int{6, 8}) {
		t.Errorf("units %v", got)
	}
	if got := PortsInText("cn-003 p2: data-b 37 to data-a 37"); len(got) != 2 || got[1] != (SwitchPort{"data-a", 37}) {
		t.Errorf("ports %v", got)
	}
}

func TestChangedRegions(t *testing.T) {
	mine := "--- a/f\n+++ b/f\n@@ -20,6 +20,8 @@\n           - cn-032\n           - cn-033\n           - cn-034\n+          - cn-035\n+          - cn-036\n   virtualRouters:\n     - localASN: 64512\n       exportPodCIDR: true\n"
	theirs := "--- a/f\n+++ b/f\n@@ -26,3 +26,7 @@\n       neighbors:\n         - peerAddress: 10.6.0.1/32\n           peerASN: 65000\n+        - peerAddress: 10.6.0.3/32\n+          peerASN: 65101\n"
	a, b := changedRegions(mine), changedRegions(theirs)
	if len(a) != 1 || a[0] != [2]int{23, 23} || len(b) != 1 || b[0] != [2]int{29, 29} {
		t.Fatalf("regions %v %v", a, b)
	}
	repl := "@@ -5,3 +5,3 @@\n a\n-b\n+B\n c\n"
	if r := changedRegions(repl); len(r) != 1 || r[0] != [2]int{6, 7} {
		t.Fatalf("replace %v", r)
	}
}

func TestParseCommitMessage(t *testing.T) {
	cases := []struct{ in, subject, body string }{
		{"feat(inventory): add cn-035\n\n- one\n- two", "feat(inventory): add cn-035", "- one\n- two"},
		{"Here is the message:\n\n```\nfix(cilium): peer cn-036 in rack-6z.\n* added cn-036\n```", "fix(cilium): peer cn-036 in rack-6z", "- added cn-036"},
		{"chore(inventory): update cn-036\n\n- cn-036 moves to U4\n\nCo-Authored-By: Someone <x@y>\nSigned-off-by: a <b>", "chore(inventory): update cn-036", "- cn-036 moves to U4"},
	}
	for _, c := range cases {
		s, b, ok := parseCommitMessage(c.in)
		if !ok || s != c.subject || strings.Join(b, "\n") != c.body {
			t.Errorf("%q: got %v %q %q", c.in, ok, s, b)
		}
	}
	if _, _, ok := parseCommitMessage("Change c-x: Add cn-035\n\n- a"); ok {
		t.Error("no Conventional Commit line must fail")
	}
}

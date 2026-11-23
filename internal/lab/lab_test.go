package lab

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
	"github.com/dennisklein/platform-kit/internal/render"
)

const seed = "../../seed/state"

func nodeYAML(name string, unit, mgmt, datab, dataa int) string {
	r := strings.NewReplacer("NAME", name, "UNIT", itoa(unit), "MGMT", itoa(mgmt), "DATAB", itoa(datab), "DATAA", itoa(dataa),
		"N3", itoa(200+unit), "N4", itoa(300+unit))
	return r.Replace(`apiVersion: inventory.pk.io/v1beta1
kind: Node
metadata:
  name: NAME
spec:
  cluster: defiant
  role: worker
  location:
    rack: GC5A-6Z
    unit: UNIT
    size: 2
  model: Supermicro SYS-220U-TNR
  bmc:
    mac: 3c:ec:ef:a1:0c:99
  ports:
    - {name: bmc, cable: MGT-01MGMT, peer: {switch: mgmt, port: MGMT}, network: mgmt}
    - {name: p1, cable: DATA-0N3, peer: {switch: data-b, port: DATAB}, network: data}
    - {name: p2, cable: DATA-0N4, peer: {switch: data-a, port: DATAA}, network: data}
`)
}

func itoa(i int) string {
	s := ""
	if i == 0 {
		return "0"
	}
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

func seedFiles(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(seed, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(seed, p)
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

const bgpPath = "clusters/defiant/infra/cilium/bgp-rack-6z.yaml"

// withBGPNodes adds hostnames to the rack-6z policy (after cn-034).
func withBGPNodes(t *testing.T, files map[string][]byte, names ...string) {
	t.Helper()
	s := string(files[bgpPath])
	add := ""
	for _, n := range names {
		add += "\n          - " + n
	}
	if !strings.Contains(s, "- cn-034") {
		t.Fatal("seed BGP policy changed")
	}
	files[bgpPath] = []byte(strings.Replace(s, "- cn-034", "- cn-034"+add, 1))
}

// withNeighbors adds BGP neighbors at the bottom of the policy (c-p4x8ma).
func withNeighbors(files map[string][]byte, addrs ...string) {
	s := strings.TrimRight(string(files[bgpPath]), "\n")
	for _, a := range addrs {
		s += "\n        - peerAddress: " + a + "/32\n          peerASN: 65010"
	}
	files[bgpPath] = []byte(s + "\n")
}

func model(t *testing.T, files map[string][]byte) *Model {
	t.Helper()
	reg := all.Registry()
	r := repo.FromFiles(files, reg)
	m, err := ModelOf(context.Background(), render.New(reg), r, render.Source{Ref: "test"}, "defiant")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func demoFiles(t *testing.T, bgp ...string) map[string][]byte {
	files := seedFiles(t)
	files["control/inventory/cn-035.yaml"] = []byte(nodeYAML("cn-035", 6, 37, 37, 38))
	files["control/inventory/cn-036.yaml"] = []byte(nodeYAML("cn-036", 8, 39, 39, 40))
	withBGPNodes(t, files, bgp...)
	return files
}

func TestBGPMissingNode(t *testing.T) {
	m := model(t, demoFiles(t, "cn-035"))
	res := m.BGP([]string{"cn-035", "cn-036"}, Faults{})
	var fails []string
	for _, c := range res.Checks {
		if !c.OK {
			fails = append(fails, c.Detail)
		}
	}
	if len(fails) != 1 || fails[0] != "cn-036: no BGP peering policy selects it" {
		t.Fatalf("fails = %q", fails)
	}
	if res.Up != 1 || res.Total != 1 {
		t.Fatalf("sessions %d of %d, want 1 of 1", res.Up, res.Total)
	}
}

func TestBGPBothNodes(t *testing.T) {
	m := model(t, demoFiles(t, "cn-035", "cn-036"))
	res := m.BGP([]string{"cn-035", "cn-036"}, Faults{})
	for _, c := range res.Checks {
		if !c.OK {
			t.Errorf("fail: %s", c.Detail)
		}
	}
	if res.Up != 2 {
		t.Fatalf("peers up = %d, want 2", res.Up)
	}
	if got := res.Checks[2].Detail; got != "BGP session to 10.6.0.1 established (2 nodes via data-a)" {
		t.Fatalf("session detail = %q", got)
	}
	// data-a down: the router is on data-a, so the sessions drop.
	res = m.BGP([]string{"cn-035"}, Faults{"data-a": true})
	if res.Up != 0 || res.Checks[1].OK || !strings.Contains(res.Checks[1].Detail, "data-a link down") {
		t.Fatalf("with data-a down: %+v", res)
	}
}

func TestBGPUnknownNeighbor(t *testing.T) {
	files := seedFiles(t)
	withNeighbors(files, "10.6.0.3", "10.6.0.4")
	m := model(t, files)
	res := m.BGP(m.MachineNames(), Faults{})
	var fails []string
	for _, c := range res.Checks {
		if !c.OK {
			fails = append(fails, c.Detail)
		}
	}
	want := []string{
		"BGP session to 10.6.0.3 not established (no router at 10.6.0.3 in network data-a)",
		"BGP session to 10.6.0.4 not established (no router at 10.6.0.4 in network data-a)",
	}
	if strings.Join(fails, "|") != strings.Join(want, "|") {
		t.Fatalf("fails = %q", fails)
	}
	// cn-003 is in GC5A-6Y without a policy: fine.
	for _, c := range res.Checks {
		if c.Subject == "cn-003" && (!c.OK || c.Detail != "cn-003: rack GC5A-6Y has no BGP peering policy") {
			t.Fatalf("cn-003: %+v", c)
		}
	}
}

func TestNodeReady(t *testing.T) {
	files := demoFiles(t, "cn-035", "cn-036")
	m := model(t, files)
	for _, n := range []string{"cn-035", "cn-036", "cn-025"} {
		if c := m.NodeReady(n); !c.OK {
			t.Errorf("%s: %s", n, c.Detail)
		}
	}
	if c := m.NodeReady("cn-035"); c.Detail != "cn-035: Ready, GC5A-6Z U6, 3 ports" {
		t.Errorf("detail = %q", c.Detail)
	}
	// data-a 37 belongs to cn-003.
	files["control/inventory/cn-035.yaml"] = []byte(nodeYAML("cn-035", 6, 37, 37, 37))
	if c := model(t, files).NodeReady("cn-035"); c.OK || c.Detail != "cn-035: port data-a 37 in use by cn-003" {
		t.Errorf("port conflict: %+v", c)
	}
	// U10 is cn-025.
	files["control/inventory/cn-035.yaml"] = []byte(nodeYAML("cn-035", 10, 37, 37, 38))
	if c := model(t, files).NodeReady("cn-035"); c.OK || !strings.Contains(c.Detail, "is used by cn-025") {
		t.Errorf("unit overlap: %+v", c)
	}
	// Other cluster: no Machine in defiant.
	files["control/inventory/cn-035.yaml"] = []byte(strings.Replace(nodeYAML("cn-035", 6, 37, 37, 38), "cluster: defiant", "cluster: pegasus", 1))
	if c := model(t, files).NodeReady("cn-035"); c.OK || c.Detail != `cn-035: belongs to cluster "pegasus", not defiant` {
		t.Errorf("cluster: %+v", c)
	}
}

func TestPowerCycleAndLinkDown(t *testing.T) {
	m := model(t, demoFiles(t, "cn-035", "cn-036"))
	c, d := m.PowerCycle("cn-036", "PowerCycle")
	if !c.OK || Human(d) != "3m 10s" || !strings.HasPrefix(c.Detail, "cn-036: back in 3m 10s") {
		t.Fatalf("power cycle: %+v %s", c, d)
	}
	c = m.LinkDown("cn-035", Faults{"data-b": true})
	if !c.OK || c.Detail != "cn-035: traffic via data-a" {
		t.Fatalf("link down: %+v", c)
	}
	// A node with only data-b loses traffic.
	files := demoFiles(t, "cn-035", "cn-036")
	s := strings.Replace(string(files["control/inventory/cn-036.yaml"]), "    - {name: p2, cable: DATA-0308, peer: {switch: data-a, port: 40}, network: data}\n", "", 1)
	files["control/inventory/cn-036.yaml"] = []byte(s)
	m = model(t, files)
	if c := m.LinkDown("cn-036", Faults{"data-b": true}); c.OK || c.Detail != "cn-036: only data-b, traffic lost" {
		t.Fatalf("single link: %+v", c)
	}
	// No BMC cable.
	s = strings.Replace(s, "    - {name: bmc, cable: MGT-0139, peer: {switch: mgmt, port: 39}, network: mgmt}\n", "", 1)
	files["control/inventory/cn-036.yaml"] = []byte(s)
	if c, _ := model(t, files).PowerCycle("cn-036", ""); c.OK || c.Detail != "cn-036: no BMC cable" {
		t.Fatalf("no bmc: %+v", c)
	}
}

func TestFluxReady(t *testing.T) {
	files := seedFiles(t)
	m := model(t, files)
	if c := m.FluxReady(); !c.OK {
		t.Fatalf("seed: %s", c.Detail)
	}
	p := "clusters/defiant/infra/cilium/helmrelease.yaml"
	files[p] = []byte(strings.Replace(string(files[p]), "interval: 5m", "interval: 5 minutes", 1))
	c := model(t, files).FluxReady()
	if c.OK || !strings.Contains(c.Detail, "helmrelease.yaml") || !strings.Contains(c.Detail, "duration") {
		t.Fatalf("bad interval: %+v", c)
	}
}

func TestSelects(t *testing.T) {
	sel := map[string]any{
		"matchLabels":      map[string]any{"role": "worker"},
		"matchExpressions": []any{map[string]any{"key": "kubernetes.io/hostname", "operator": "In", "values": []any{"a", "b"}}},
	}
	if !Selects(sel, map[string]string{"role": "worker", "kubernetes.io/hostname": "a"}) {
		t.Error("a")
	}
	if Selects(sel, map[string]string{"role": "worker", "kubernetes.io/hostname": "c"}) {
		t.Error("c")
	}
	if Selects(sel, map[string]string{"kubernetes.io/hostname": "a"}) {
		t.Error("no role")
	}
	if !Selects(map[string]any{}, map[string]string{}) {
		t.Error("empty")
	}
	notIn := map[string]any{"matchExpressions": []any{map[string]any{"key": "x", "operator": "NotIn", "values": []any{"1"}},
		map[string]any{"key": "y", "operator": "DoesNotExist"}}}
	if !Selects(notIn, map[string]string{"x": "2"}) || Selects(notIn, map[string]string{"x": "1"}) || Selects(notIn, map[string]string{"y": ""}) {
		t.Error("NotIn/DoesNotExist")
	}
}

func TestTopology(t *testing.T) {
	m := model(t, demoFiles(t, "cn-035", "cn-036"))
	topo := GenerateTopology(m, "defiant", "c-k7m2qz")
	if got := topo.Summary(); got != "defiant: 13 machines, 3 networks (ib not emulated)" {
		t.Fatalf("summary = %q", got)
	}
	tm := topo.Machine("cn-035")
	if tm == nil || tm.BMC.Fixture != "supermicro-x12" || tm.BMC.Port != "mgmt 37" || tm.Machine.Backend != "qemu" || !tm.Emulate {
		t.Fatalf("cn-035 = %+v", tm)
	}
	if len(tm.NICs) != 2 || tm.NICs[0].Network != "data-b" || tm.NICs[0].Peer != "data-b 37" || tm.NICs[1].Network != "data-a" {
		t.Fatalf("NICs = %+v", tm.NICs)
	}
	if len(tm.Machine.Disks) != 1 || tm.Machine.Disks[0].SizeGiB != 32 {
		t.Fatalf("disks = %+v", tm.Machine.Disks)
	}
	if len(topo.Problems) != 0 {
		t.Fatalf("problems = %+v", topo.Problems)
	}
	var uplink bool
	for _, l := range topo.Links {
		if l.Cable == "UPL-0002" && l.A == "data-a:47" && l.B == "spine-1:11" {
			uplink = true
		}
	}
	if !uplink {
		t.Fatal("uplink UPL-0002 missing")
	}
	// All clusters: sn-05 uses the Huawei fixture.
	all := GenerateTopology(m, "", "main")
	if sn := all.Machine("sn-05"); sn == nil || sn.BMC.Fixture != "huawei-2288h-v5" {
		t.Fatalf("sn-05 = %+v", sn)
	}
	// Unknown model: a warning, the machine is not emulated.
	files := demoFiles(t, "cn-035", "cn-036")
	files["control/inventory/cn-036.yaml"] = []byte(strings.Replace(string(files["control/inventory/cn-036.yaml"]), "Supermicro SYS-220U-TNR", "Dell R650", 1))
	topo = GenerateTopology(model(t, files), "defiant", "x")
	if len(topo.Problems) != 1 || topo.Problems[0].Message != `cn-036: no emulation rule matches model "Dell R650"` || topo.Machine("cn-036").Emulate {
		t.Fatalf("problems = %+v", topo.Problems)
	}
}

func TestDefaultScenario(t *testing.T) {
	if DefaultScenario([]string{"control/inventory/cn-035.yaml"}) != ScenarioMetal {
		t.Error("inventory")
	}
	if DefaultScenario([]string{"clusters/defiant/infra/cilium/bgp-rack-6z.yaml"}) != ScenarioMetal {
		t.Error("cilium")
	}
	if DefaultScenario([]string{"clusters/defiant/apps/podinfo/deployment.yaml", ".pk/changes/c-x.yaml"}) != ScenarioGitOps {
		t.Error("apps")
	}
}

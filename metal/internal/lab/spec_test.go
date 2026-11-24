package lab_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/lab"
)

const minimal = `
cluster: { name: lab, talosVersion: v1.14.6, kubernetesVersion: v1.36.2, vip: 10.77.0.10 }
controlPlanes: 1
workers: 2
controlPlane: { memoryMiB: 4096, cpus: 2 }
worker: { memoryMiB: 2048, cpus: 2 }
`

func TestParseSize(t *testing.T) {
	cp, w, err := lab.ParseSize("3cp+2w")
	require.NoError(t, err)
	assert.Equal(t, 3, cp)
	assert.Equal(t, 2, w)

	for _, bad := range []string{"", "3", "3cp", "3cp+2", "3+2", "cp+w", "-1cp+2w", "3cp + 2w", "3CP+2W"} {
		_, _, err := lab.ParseSize(bad)
		assert.Error(t, err, "%q", bad)
	}
}

func TestSpecRejects(t *testing.T) {
	for name, tc := range map[string]struct{ spec, want string }{
		"unknown key":         {minimal + "workerz: 3\n", "workerz"},
		"no control plane":    {strings.Replace(minimal, "controlPlanes: 1", "controlPlanes: 0", 1), "at least one"},
		"even control planes": {strings.Replace(minimal, "controlPlanes: 1", "controlPlanes: 2", 1), "odd number"},
		"negative workers":    {strings.Replace(minimal, "workers: 2", "workers: -1", 1), "negative"},
		"too many":            {strings.Replace(minimal, "workers: 2", "workers: 99", 1), "more than the lab supports"},
		"vip outside":         {strings.Replace(minimal, "10.77.0.10", "10.78.0.10", 1), "not an address in the lab network"},
		"vip is gateway":      {strings.Replace(minimal, "10.77.0.10", "10.77.0.1", 1), "workstation's address"},
		"vip is a node":       {strings.Replace(minimal, "10.77.0.10", "10.77.0.12", 1), "address of node-2"},
		"bad version":         {strings.Replace(minimal, "v1.14.6", "1.14", 1), "talosVersion"},
		"bad name":            {strings.Replace(minimal, "name: lab", "name: Lab_1", 1), "cluster.name"},
		"small memory":        {strings.Replace(minimal, "memoryMiB: 2048", "memoryMiB: 512", 1), "worker.memoryMiB"},
		"no cpus":             {strings.Replace(minimal, "cpus: 2 }\nworker", "cpus: 0 }\nworker", 1), "controlPlane.cpus"},
		"override name":       {minimal + "nodes: { node3: { cpus: 4 } }\n", `"node3" is not a machine name`},
		"override memory":     {minimal + "nodes: { node-2: { memoryMiB: 100 } }\n", "nodes.node-2.memoryMiB"},
		"empty patch":         {minimal + "patches: [ { inline: '' } ]\n", "patches[0]"},
		"workload name":       {minimal + "workload: { name: lab, vip: 10.77.0.9, controlPlanes: 1 }\n", "name of the first cluster"},
		"workload vip":        {minimal + "workload: { name: work, vip: 10.77.0.10, controlPlanes: 1 }\n", "first cluster's VIP"},
		"workload vip a node": {minimal + "workload: { name: work, vip: 10.77.0.14, controlPlanes: 1 }\n", "workload.vip 10.77.0.14 is the address of node-4"},
		"workload even":       {minimal + "workload: { name: work, vip: 10.77.0.9, controlPlanes: 2 }\n", "workload.controlPlanes is 2"},
		"workload too many":   {minimal + "workload: { name: work, vip: 10.77.0.9, controlPlanes: 1, workers: 97 }\n", "101 machines"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := lab.Parse([]byte(tc.spec))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestResizeValidates(t *testing.T) {
	s, err := lab.Parse([]byte(minimal))
	require.NoError(t, err)
	assert.ErrorContains(t, s.Resize("2cp+1w"), "odd number")
	assert.ErrorContains(t, s.Resize("three"), "<control planes>cp+<workers>w")
}

// Identities are a function of the index alone, so that the fleet, the
// manifests and the ledger agree on them by construction.
func TestPlanDerivesIdentities(t *testing.T) {
	s, err := lab.Parse([]byte(minimal + `
nodes:
  node-2: { personality: supermicro-x12, cpus: 4 }
  node-12: { memoryMiB: 8192 }
  node-40: { personality: huawei-2288h-v5 } # beyond the size: ignored
`))
	require.NoError(t, err)
	require.NoError(t, s.Resize("3cp+9w"))
	nodes := s.Plan()
	require.Len(t, nodes, 12)

	n2 := nodes[1]
	assert.Equal(t, "controlplane", n2.Role)
	assert.Equal(t, "supermicro-x12", n2.Personality)
	assert.Equal(t, 4, n2.CPUs)
	assert.Equal(t, 4096, n2.MemoryMiB, "an override keeps what it does not set")

	n12 := nodes[11]
	assert.Equal(t, lab.Node{
		Index: 12, Name: "node-12", Role: "worker", Personality: "generic",
		MemoryMiB: 8192, CPUs: 2,
		Serial: "PKLAB0012", CID: "PKLAB-0012",
		UUID:   "9b1c000c-0000-4000-8000-00000000000c",
		BMCMAC: "02:77:00:00:01:0c", NICMAC: "52:54:00:77:00:1c",
		DiskSerial: "pklab-disk-12", Tap: "pktap12",
		Listen: "10.77.0.1:8452", Address: n12.Address,
	}, n12)
	assert.Equal(t, "10.77.0.22", n12.Address.String())
}

// A patch comes out as the same string it went in as, whatever its shape.
func TestPatchesRoundTrip(t *testing.T) {
	odd := "  leading: spaces\nno: final newline"
	s, err := lab.Parse([]byte(minimal))
	require.NoError(t, err)
	s.Patches = []lab.Patch{{Inline: odd}, {Inline: "a: 1\n"}}
	files, err := lab.Render(s, "spec")
	require.NoError(t, err)

	docs := yamlDocs(t, files[lab.FileTalos])
	spec := docs[0].(map[string]any)["spec"].(map[string]any)
	assert.Equal(t, []any{
		map[string]any{"inline": odd},
		map[string]any{"inline": "a: 1\n"},
	}, spec["patches"])
}

func TestSetWorkload(t *testing.T) {
	s, err := lab.Parse([]byte(minimal))
	require.NoError(t, err)
	assert.Nil(t, s.Workload)
	assert.Equal(t, 3, s.Machines())
	require.NoError(t, s.SetWorkload("1cp+2w"))
	require.NotNil(t, s.Workload)
	assert.Equal(t, lab.Workload{Name: "work", VIP: "10.77.0.9", ControlPlanes: 1, Workers: 2}, *s.Workload)
	assert.Equal(t, 6, s.Machines())
	assert.Equal(t, lab.DefaultWorkload.ControlPlanes, 1, "the default is not changed")
	require.Error(t, s.SetWorkload("2cp+0w"))
	require.Error(t, s.SetWorkload("1cp"))
}

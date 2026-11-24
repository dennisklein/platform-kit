package render_test

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// Every leaf field of the CRD's schematic must change the schematic id. A field
// that Schematic leaves behind cannot change the id, so two schematics that
// differ only in it would share one image. The test walks the type, so a field
// added to the CRD later is covered without anyone editing this test.
func TestSchematicCoversEveryField(t *testing.T) {
	base, err := render.Schematic(&osv1alpha1.Schematic{}).ID()
	require.NoError(t, err)

	var leaves [][]int
	var walk func(typ reflect.Type, path []int)
	walk = func(typ reflect.Type, path []int) {
		for i := range typ.NumField() {
			f := typ.Field(i)
			p := append(slices.Clone(path), i)
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, p)
				continue
			}
			leaves = append(leaves, p)
		}
	}
	walk(reflect.TypeFor[osv1alpha1.Schematic](), nil)
	require.NotEmpty(t, leaves)

	for _, path := range leaves {
		name := reflect.TypeFor[osv1alpha1.Schematic]().FieldByIndex(path).Name
		t.Run(name, func(t *testing.T) {
			s := &osv1alpha1.Schematic{}
			field := reflect.ValueOf(s).Elem().FieldByIndex(path)
			setNonZero(t, field)
			id, err := render.Schematic(s).ID()
			require.NoError(t, err)
			assert.NotEqual(t, base, id, "field %s does not reach the schematic id", name)
		})
	}
}

// setNonZero gives a leaf field a value that is not its zero value.
func setNonZero(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("siderolabs/example")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		setNonZero(t, elem)
		v.Set(reflect.Append(v, elem))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		setNonZero(t, k)
		setNonZero(t, e)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		setNonZero(t, p.Elem())
		v.Set(p)
	default:
		t.Fatalf("setNonZero: add a case for %s", v.Kind())
	}
}

// genPair varies the lab sample along every axis the renderer reads from the
// kinds: role, labels, taints, disk matchers, link settings, CNI, scheduling,
// mirrors and SANs. Every pair it builds is one the API admits.
func genPair(r *rand.Rand, m *render.Manifests) (*osv1alpha1.TalosCluster, *osv1alpha1.TalosNode) {
	c := m.Cluster.DeepCopy()
	n := m.Node("node-1").DeepCopy()
	pick := func(p float64) bool { return r.Float64() < p }

	if pick(0.5) {
		n.Spec.Role = osv1alpha1.RoleWorker
	}
	if pick(0.5) {
		n.Spec.Labels = map[string]string{}
		for i := range r.IntN(8) {
			n.Spec.Labels[fmt.Sprintf("pk.io/l%d", i)] = fmt.Sprintf("v%d", r.IntN(3))
		}
	}
	effects := []osv1alpha1.TaintEffect{"NoSchedule", "PreferNoSchedule", "NoExecute"}
	for i := range r.IntN(3) {
		t := osv1alpha1.Taint{Key: fmt.Sprintf("pk.io/t%d", i), Effect: effects[r.IntN(len(effects))]}
		if pick(0.5) {
			t.Value = "yes"
		}
		n.Spec.Taints = append(n.Spec.Taints, t)
	}
	sel := &n.Spec.InstallDiskSelector
	if pick(0.3) {
		sel.Model = "QEMU*"
	}
	if pick(0.3) {
		sel.BusPath = "/pci0000:00/*"
	}
	if pick(0.3) {
		sel.Match = "disk.size > 10u * GB"
	}
	link := &n.Spec.Network.Links[0]
	if pick(0.3) {
		link.DHCP = true
	}
	if pick(0.3) {
		link.MTU = 9000
	}
	if pick(0.3) {
		link.Routes = append(link.Routes, osv1alpha1.Route{Network: "10.99.0.0/16", Gateway: "10.77.0.254", Metric: 100})
	}
	switch r.IntN(4) {
	case 0:
		c.Spec.Network.CNI = nil
	case 1:
		c.Spec.Network.CNI = &osv1alpha1.CNI{Name: osv1alpha1.CNINone}
	case 2:
		c.Spec.Network.CNI = &osv1alpha1.CNI{Name: osv1alpha1.CNICustom,
			URLs: []string{"https://cni.example.test/a.yaml", "https://cni.example.test/b.yaml"}}
	}
	if pick(0.5) {
		c.Spec.ControlPlane.AllowScheduling = true
	}
	if pick(0.3) {
		c.Spec.RegistryMirrors = []osv1alpha1.RegistryMirror{
			{Registry: "docker.io", Endpoints: []string{"https://mirror.example.test"}}}
	}
	if pick(0.3) {
		c.Spec.CertSANs = []string{"api.example.test", "10.77.0.10"}
	}
	if pick(0.3) {
		n.Spec.Schematic = &osv1alpha1.Schematic{Customization: osv1alpha1.SchematicCustomization{
			SystemExtensions: osv1alpha1.SystemExtensions{OfficialExtensions: []string{"siderolabs/iscsi-tools"}}}}
	}
	return c, n
}

// Generated cluster and node pairs render the same bytes every time, and pass
// Talos' own validation in both modes.
func TestRenderProperties(t *testing.T) {
	m := samples(t)
	bundle := testBundle(t)
	cases := 40
	if testing.Short() {
		cases = 8
	}
	for i := range cases {
		r := rand.New(rand.NewPCG(2, uint64(i)))
		c, n := genPair(r, m)
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			installer, err := render.NewFactory("").InstallerImage(render.EffectiveSchematic(c, n), c.Spec.TalosVersion)
			require.NoError(t, err)
			cp, err := render.ResolvePatches(c.Spec.Patches, m.ConfigMapReader())
			require.NoError(t, err)
			in := render.Input{Cluster: c, Node: n, Bundle: bundle, InstallerImage: installer, ClusterPatches: cp}

			first, err := render.Node(in)
			require.NoError(t, err, "cluster %+v node %+v", c.Spec, n.Spec)
			for range 3 {
				again, err := render.Node(in)
				require.NoError(t, err)
				require.Equal(t, string(first), string(again), "the render is a function of its input")
			}

			loaded, err := configloader.NewFromBytes(first)
			require.NoError(t, err)
			assert.NoError(t, render.Validate(loaded, render.Metal))
			assert.NoError(t, render.Validate(loaded, render.Container))
		})
	}
}

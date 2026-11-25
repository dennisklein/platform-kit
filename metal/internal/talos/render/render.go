package render

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	tconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/config/types/k8s"
	"github.com/siderolabs/talos/pkg/machinery/config/types/meta"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	"github.com/siderolabs/talos/pkg/machinery/config/validation"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// Input is everything one node's configuration derives from. Patches arrive
// resolved: the caller reads ConfigMap references (from the API server, or
// from manifests on disk) so that rendering stays a pure function.
type Input struct {
	Cluster *osv1alpha1.TalosCluster
	Node    *osv1alpha1.TalosNode
	Bundle  *secrets.Bundle

	// InstallerImage is the installer reference the config installs from;
	// Factory.InstallerImage computes it for the node's effective schematic.
	InstallerImage string

	// ClusterPatches and NodePatches are the resolved patch texts, applied in
	// that order.
	ClusterPatches [][]byte
	NodePatches    [][]byte
}

// Mode is the runtime mode a rendered config is validated for.
type Mode struct {
	name      string
	install   bool
	container bool
}

var (
	// Metal is a machine that boots from media and installs to disk: the only
	// mode the operator renders for.
	Metal = Mode{name: "metal", install: true}
	// Container is a Talos container, which never installs. Tests validate the
	// same render there too, so a config that only passes because of the
	// install requirement does not go unnoticed.
	Container = Mode{name: "container", container: true}
)

func (m Mode) String() string        { return m.name }
func (m Mode) RequiresInstall() bool { return m.install }
func (m Mode) InContainer() bool     { return m.container }

var _ validation.RuntimeMode = Mode{}

// MinTalosVersion is the oldest release whose configuration contract this
// package renders. Older contracts put networking, install and Kubernetes
// settings into the v1alpha1 document, which this renderer never writes.
const MinTalosVersion = "v1.14.0"

// Node renders one node's machine configuration, validated for metal.
func Node(in Input) ([]byte, error) {
	cfg, err := Build(in)
	if err != nil {
		return nil, err
	}
	if err := Validate(cfg, Metal); err != nil {
		return nil, err
	}
	return cfg.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
}

// Validate runs Talos' own client-side validation in the given mode.
func Validate(cfg tconfig.Provider, mode Mode) error {
	if _, err := cfg.ValidateAsClient(mode); err != nil {
		return fmt.Errorf("invalid config for %s: %w", mode, err)
	}
	return nil
}

// Build renders the configuration without validating it.
func Build(in Input) (tconfig.Provider, error) {
	c, n := in.Cluster, in.Node
	if c == nil || n == nil || in.Bundle == nil {
		return nil, errors.New("render needs a cluster, a node and a secrets bundle")
	}
	if n.Spec.ClusterRef.Name != c.Name {
		return nil, fmt.Errorf("node %s is a member of %q, not %q", n.Name, n.Spec.ClusterRef.Name, c.Name)
	}
	contract, err := Contract(c.Spec.TalosVersion)
	if err != nil {
		return nil, err
	}
	if in.InstallerImage == "" {
		return nil, errors.New("no installer image")
	}

	sans := slices.Clone(c.Spec.CertSANs)
	if vip := controlPlaneVIP(c); vip != "" && !slices.Contains(sans, vip) {
		sans = append(sans, vip)
	}
	opts := []generate.Option{
		generate.WithVersionContract(contract),
		generate.WithSecretsBundle(in.Bundle),
		generate.WithInstallImage(in.InstallerImage),
		generate.WithAdditionalSubjectAltNames(sans),
		generate.WithAllowSchedulingOnControlPlanes(c.Spec.ControlPlane != nil && c.Spec.ControlPlane.AllowScheduling),
	}
	for _, m := range c.Spec.RegistryMirrors {
		opts = append(opts, generate.WithRegistryMirror(m.Registry, m.Endpoints...))
	}

	gen, err := generate.NewInput(c.Name, c.Spec.Endpoint, strings.TrimPrefix(c.Spec.KubernetesVersion, "v"), opts...)
	if err != nil {
		return nil, err
	}
	if len(c.Spec.Network.PodSubnets) > 0 {
		gen.PodNet = c.Spec.Network.PodSubnets
	}
	if len(c.Spec.Network.ServiceSubnets) > 0 {
		gen.ServiceNet = c.Spec.Network.ServiceSubnets
	}
	for _, subnet := range slices.Concat(gen.PodNet, gen.ServiceNet) {
		// generate parses these with MustParsePrefix: a typo would panic the
		// reconciler instead of failing the render.
		if _, err := netip.ParsePrefix(subnet); err != nil {
			return nil, fmt.Errorf("network subnet %q: %w", subnet, err)
		}
	}

	machineType := machine.TypeWorker
	if n.Spec.Role == osv1alpha1.RoleControlPlane {
		machineType = machine.TypeControlPlane
	}
	base, err := gen.Config(machineType)
	if err != nil {
		return nil, err
	}

	docs, err := adjust(base.Documents(), c, n)
	if err != nil {
		return nil, err
	}
	extra, err := nodeDocuments(c, n, in.InstallerImage)
	if err != nil {
		return nil, err
	}
	cfg, err := container.New(append(docs, extra...)...)
	if err != nil {
		return nil, err
	}

	patched, err := applyPatches(cfg, in.ClusterPatches)
	if err != nil {
		return nil, err
	}
	if machineType != machine.TypeControlPlane {
		if patched, err = dropControlPlaneOnly(patched); err != nil {
			return nil, err
		}
	}
	return applyPatches(patched, in.NodePatches)
}

// dropControlPlaneOnly removes from a worker's configuration the documents
// that Talos allows on control planes only (KubeAuditPolicyConfig,
// KubeAPIServerConfig and the like). It runs after the cluster's patches,
// which apply to every node: such a document there is meant for the control
// planes, and Talos would refuse the worker's configuration with it. A
// node's own patches come after, so a control-plane document in a worker's
// patches still fails the render.
func dropControlPlaneOnly(cfg tconfig.Provider) (tconfig.Provider, error) {
	docs := cfg.Documents()
	kept := slices.DeleteFunc(slices.Clone(docs), func(d config.Document) bool {
		_, only := d.(container.ControlplaneOnlyConfig)
		return only
	})
	if len(kept) == len(docs) {
		return cfg, nil
	}
	return container.New(kept...)
}

// Contract parses a Talos version into its configuration contract and refuses
// anything older than MinTalosVersion.
func Contract(talosVersion string) (*tconfig.VersionContract, error) {
	contract, err := tconfig.ParseContractFromVersion(talosVersion)
	if err != nil {
		return nil, err
	}
	if !contract.Greater(tconfig.TalosVersion1_13) {
		return nil, fmt.Errorf("talosVersion %s: this renderer targets %s or later", talosVersion, MinTalosVersion)
	}
	return contract, nil
}

// adjust edits the generated documents where the kinds say something the
// generator options cannot: the fixed hostname, the node's labels and taints,
// and the CNI choice.
func adjust(docs []config.Document, c *osv1alpha1.TalosCluster, n *osv1alpha1.TalosNode) ([]config.Document, error) {
	out := make([]config.Document, 0, len(docs))
	for _, doc := range docs {
		switch d := doc.(type) {
		case *network.HostnameConfigV1Alpha1:
			// The generator picks a stable random name; the node's name is
			// the one the cluster, the Host and every person know it by.
			d.ConfigAuto = nil
			d.ConfigHostname = n.Name
		case *k8s.KubeNodeConfigV1Alpha1:
			if len(n.Spec.Labels) > 0 && d.LabelsConfig == nil {
				d.LabelsConfig = map[string]string{}
			}
			for k, v := range n.Spec.Labels {
				d.LabelsConfig[k] = v
			}
			if len(n.Spec.Taints) > 0 && d.TaintsConfig == nil {
				d.TaintsConfig = map[string]string{}
			}
			for _, t := range n.Spec.Taints {
				v := string(t.Effect)
				if t.Value != "" {
					v = t.Value + ":" + v
				}
				d.TaintsConfig[t.Key] = v
			}
		case *k8s.KubeFlannelCNIConfigV1Alpha1:
			// Flannel is deployed exactly when this document exists.
			if cni := c.Spec.Network.CNI; cni != nil && cni.Name != osv1alpha1.CNIFlannel {
				continue
			}
		}
		out = append(out, doc)
	}

	// The manifests are control-plane work: workers never apply them, and
	// Talos refuses the document on a worker.
	if cni := c.Spec.Network.CNI; cni != nil && cni.Name == osv1alpha1.CNICustom && n.Spec.Role == osv1alpha1.RoleControlPlane {
		for i, raw := range cni.URLs {
			u, err := url.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("cni url %q: %w", raw, err)
			}
			m := k8s.NewKubeExternalManifestConfigV1Alpha1()
			m.MetaName = "custom-cni-" + strconv.Itoa(i)
			m.URLSpec = meta.URL{URL: u}
			out = append(out, m)
		}
	}
	return out, nil
}

// nodeDocuments renders what only the node object knows: the install target
// and its links, plus the VIP a control-plane member announces.
func nodeDocuments(c *osv1alpha1.TalosCluster, n *osv1alpha1.TalosNode, installer string) ([]config.Document, error) {
	match, err := DiskMatch(n.Spec.InstallDiskSelector)
	if err != nil {
		return nil, err
	}
	install := runtime.NewUnattendedInstallConfigV1Alpha1()
	install.Installer.Image = installer
	install.ProvisioningSpec.DiskSelector.Match = match
	// Never wipe on its own: reinstalling over a disk is a decision for the
	// HostOperation that reprovisions the machine, not a side effect of a
	// config that happens to reach it.
	install.ProvisioningSpec.Wipe = new(false)
	docs := []config.Document{install}

	for _, l := range n.Spec.Network.Links {
		alias := network.NewLinkAliasConfigV1Alpha1(l.Name)
		expr, err := cel.ParseBooleanExpression(LinkMatch(l.HardwareAddr), celenv.LinkLocator())
		if err != nil {
			return nil, fmt.Errorf("link %s: %w", l.Name, err)
		}
		alias.Selector.Match = expr

		link := network.NewLinkConfigV1Alpha1(l.Name)
		link.LinkUp = new(true)
		link.LinkMTU = uint32(l.MTU)
		for _, a := range l.Addresses {
			p, err := netip.ParsePrefix(a)
			if err != nil {
				return nil, fmt.Errorf("link %s address %q: want CIDR form: %w", l.Name, a, err)
			}
			link.LinkAddresses = append(link.LinkAddresses, network.AddressConfig{AddressAddress: p})
		}
		for _, r := range l.Routes {
			rc := network.RouteConfig{RouteMetric: uint32(r.Metric)}
			if r.Network != "" {
				p, err := netip.ParsePrefix(r.Network)
				if err != nil {
					return nil, fmt.Errorf("link %s route %q: %w", l.Name, r.Network, err)
				}
				// A default route is no destination at all in Talos' terms.
				if p.Bits() != 0 {
					rc.RouteDestination = meta.Prefix{Prefix: p}
				}
			}
			if r.Gateway != "" {
				gw, err := netip.ParseAddr(r.Gateway)
				if err != nil {
					return nil, fmt.Errorf("link %s route gateway %q: %w", l.Name, r.Gateway, err)
				}
				rc.RouteGateway = meta.Addr{Addr: gw}
			}
			link.LinkRoutes = append(link.LinkRoutes, rc)
		}
		docs = append(docs, alias, link)
		if l.DHCP {
			docs = append(docs, network.NewDHCPv4ConfigV1Alpha1(l.Name))
		}
	}

	if vip := controlPlaneVIP(c); vip != "" && n.Spec.Role == osv1alpha1.RoleControlPlane {
		link, err := vipLink(vip, n)
		if err != nil {
			return nil, err
		}
		v := network.NewLayer2VIPConfigV1Alpha1(vip)
		v.LinkName = link
		docs = append(docs, v)
	}
	return docs, nil
}

// vipLink picks the link that announces the VIP: the one whose static address
// shares a subnet with it. Gratuitous ARP for an address outside the link's
// subnet reaches nobody who would route to it.
func vipLink(vip string, n *osv1alpha1.TalosNode) (string, error) {
	addr, err := netip.ParseAddr(vip)
	if err != nil {
		return "", fmt.Errorf("controlPlane.vip %q: %w", vip, err)
	}
	for _, l := range n.Spec.Network.Links {
		for _, a := range l.Addresses {
			if p, err := netip.ParsePrefix(a); err == nil && p.Masked().Contains(addr) {
				return l.Name, nil
			}
		}
	}
	return "", fmt.Errorf("controlPlane.vip %s: no link of %s has a static address in its subnet", vip, n.Name)
}

func controlPlaneVIP(c *osv1alpha1.TalosCluster) string {
	if c.Spec.ControlPlane == nil {
		return ""
	}
	return c.Spec.ControlPlane.VIP
}

// LinkMatch is the LinkAliasConfig selector for a link's MAC address.
//
// Talos fills permanent_addr from the ETHTOOL_GPERMADDR ioctl and has no
// fallback, so a driver that reports no permanent address leaves it empty.
// virtio-net does report one when the device offers VIRTIO_NET_F_MAC, which
// QEMU does by default, but a NIC without it would never match. The second
// arm matches the current address of a physical link (kind "") that has no
// permanent one. It excludes bonds, bridges and VLANs, which take a member's
// MAC as theirs: an alias must match exactly one link, and those would
// otherwise match alongside their member.
func LinkMatch(hardwareAddr string) string {
	mac := strings.ToLower(hardwareAddr)
	return fmt.Sprintf(`mac(link.permanent_addr) == %q || (size(link.permanent_addr) == 0 && link.kind == "" && mac(link.hardware_addr) == %q)`,
		mac, mac)
}

// DiskMatch compiles the install disk selector into the CEL expression
// UnattendedInstallConfig matches disks with. Every set matcher must hold.
//
// A string matcher with a * uses glob(). Talos's glob() knows only *: ? and
// [ match themselves, so a value with them but no * is compared as it is,
// which is what glob() would do with it too. Match takes a full expression,
// matches() included, for anything a * cannot say.
func DiskMatch(s osv1alpha1.DiskSelector) (cel.Expression, error) {
	var terms []string
	field := func(name, value string) {
		if value == "" {
			return
		}
		if strings.Contains(value, "*") {
			terms = append(terms, fmt.Sprintf("glob(%q, disk.%s)", value, name))
			return
		}
		terms = append(terms, fmt.Sprintf("disk.%s == %q", name, value))
	}
	field("serial", s.Serial)
	field("model", s.Model)
	field("wwid", s.WWID)
	field("bus_path", s.BusPath)
	if s.Match != "" {
		terms = append(terms, "("+s.Match+")")
	}
	if len(terms) == 0 {
		return cel.Expression{}, errors.New("installDiskSelector matches nothing: set at least one matcher")
	}
	expr, err := cel.ParseBooleanExpression(strings.Join(terms, " && "), celenv.DiskLocator())
	if err != nil {
		return cel.Expression{}, fmt.Errorf("installDiskSelector: %w", err)
	}
	return expr, nil
}

// applyPatches applies the lists of patches in order. Only strategic
// merge patches are accepted: a JSON6902 patch addresses one document by path,
// and a multi-document config is not one document.
func applyPatches(cfg tconfig.Provider, lists ...[][]byte) (tconfig.Provider, error) {
	var patches []configpatcher.Patch
	for _, list := range lists {
		for i, raw := range list {
			p, err := configpatcher.LoadPatch(raw)
			if err != nil {
				return nil, fmt.Errorf("patch %d: %w", i, err)
			}
			if _, ok := p.(configpatcher.StrategicMergePatch); !ok {
				return nil, fmt.Errorf("patch %d: only strategic merge patches apply to a multi-document config", i)
			}
			patches = append(patches, p)
		}
	}
	if len(patches) == 0 {
		return cfg, nil
	}
	out, err := configpatcher.Apply(configpatcher.WithConfig(cfg), patches)
	if err != nil {
		return nil, err
	}
	return out.Config()
}

// NodeAddress is where a node's Talos API is reached: spec.address, or the
// first static address of its first link that has one. Empty when the node
// only takes DHCP addresses and names none.
func NodeAddress(n *osv1alpha1.TalosNode) string {
	if n.Spec.Address != "" {
		return n.Spec.Address
	}
	for _, l := range n.Spec.Network.Links {
		for _, a := range l.Addresses {
			if p, err := netip.ParsePrefix(a); err == nil {
				return p.Addr().String()
			}
		}
	}
	return ""
}

// EffectiveSchematic is the schematic a node's installer is built from: its
// own when set, the cluster's otherwise. Nil is the vanilla image.
func EffectiveSchematic(c *osv1alpha1.TalosCluster, n *osv1alpha1.TalosNode) *osv1alpha1.Schematic {
	if n.Spec.Schematic != nil {
		return n.Spec.Schematic
	}
	return c.Spec.Schematic
}

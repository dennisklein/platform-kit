// Package render turns the os.pk.io CRs into Talos machine configs by
// filling talhelper's own config structs and running its generate pipeline — the
// engine decision: parity with `talhelper genconfig` holds by construction, not by
// reimplementation. The mapping is deliberately narrow: it carries exactly the fields
// the raven talconfig uses, and Convert refuses to translate a talconfig that uses
// anything the CR schema does not carry yet (the round-trip guard), so nothing is
// ever dropped silently.
package render

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/budimanjojo/talhelper/v3/pkg/config"
	"github.com/budimanjojo/talhelper/v3/pkg/generate"
	"github.com/siderolabs/image-factory/pkg/schematic"
	"github.com/siderolabs/talos/pkg/machinery/config/types/v1alpha1"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"gopkg.in/yaml.v3"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// LoadTalconfig reads a talconfig.yaml the way the talhelper CLI does: env
// substitution from envFiles, "@./" refs made absolute, node groups merged,
// validated. The source of Convert.
func LoadTalconfig(path string, envFiles []string) (*config.TalhelperConfig, error) {
	return config.LoadAndValidateFromFile(path, envFiles, false)
}

// Map fills talhelper's TalhelperConfig from the CRs — the CR-to-IR half of the
// engine. Relative "@" patch references resolve against patchRoot (the directory
// that holds patches/: control/talos on the CLI, the ConfigMap mount in-cluster).
// The node-group merge and validation mirror talhelper's own loader sequence so a
// Map-built config is indistinguishable from a file-loaded one.
func Map(cluster *osv1alpha1.TalosCluster, nodes []osv1alpha1.TalosNode, patchRoot string) (*config.TalhelperConfig, error) {
	c := &config.TalhelperConfig{
		ClusterName:                    cluster.Name,
		TalosVersion:                   cluster.Spec.TalosVersion,
		KubernetesVersion:              cluster.Spec.KubernetesVersion,
		Endpoint:                       cluster.Spec.Endpoint,
		AllowSchedulingOnControlPlanes: cluster.Spec.AllowSchedulingOnControlPlanes,
		AdditionalApiServerCertSans:    cluster.Spec.AdditionalApiServerCertSans,
		AdditionalMachineCertSans:      cluster.Spec.AdditionalMachineCertSans,
		ClusterPodNets:                 cluster.Spec.ClusterPodNets,
		ClusterSvcNets:                 cluster.Spec.ClusterSvcNets,
		Patches:                        resolvePatches(cluster.Spec.Patches, patchRoot),
	}

	for i := range nodes {
		n := &nodes[i]
		if n.Spec.ClusterRef.Name != cluster.Name {
			return nil, fmt.Errorf("node %s references cluster %q, rendering %q", n.Name, n.Spec.ClusterRef.Name, cluster.Name)
		}
		node := config.Node{
			Hostname:     n.Name,
			IPAddress:    n.Spec.IPAddress,
			ControlPlane: n.Spec.ControlPlane,
		}
		if s := n.Spec.Schematic; s != nil {
			node.Schematic = &schematic.Schematic{Customization: schematic.Customization{
				SystemExtensions: schematic.SystemExtensions{
					OfficialExtensions: s.Customization.SystemExtensions.OfficialExtensions,
				},
				ExtraKernelArgs: s.Customization.ExtraKernelArgs,
			}}
		}
		if d := n.Spec.InstallDiskSelector; d != nil {
			node.InstallDiskSelector = &v1alpha1.InstallDiskSelector{
				Serial:  d.Serial,
				Model:   d.Model,
				Name:    d.Name,
				BusPath: d.BusPath,
			}
		}
		node.NodeLabels = n.Spec.NodeLabels
		node.Patches = resolvePatches(n.Spec.Patches, patchRoot)
		for _, ni := range n.Spec.NetworkInterfaces {
			dev := &v1alpha1.Device{
				DeviceInterface: ni.Interface,
				DeviceDHCP:      ni.DHCP,
				DeviceMTU:       ni.MTU,
				DeviceAddresses: ni.Addresses,
			}
			if sel := ni.DeviceSelector; sel != nil {
				dev.DeviceSelector = &v1alpha1.NetworkDeviceSelector{
					NetworkDeviceHardwareAddress: sel.HardwareAddr,
					NetworkDeviceBus:             sel.BusPath,
				}
			}
			for _, r := range ni.Routes {
				dev.DeviceRoutes = append(dev.DeviceRoutes, &v1alpha1.Route{
					RouteNetwork: r.Network,
					RouteGateway: r.Gateway,
					RouteMetric:  r.Metric,
				})
			}
			if ni.VIP != nil {
				dev.DeviceVIPConfig = &v1alpha1.DeviceVIPConfig{SharedIP: ni.VIP.IP}
			}
			node.NetworkInterfaces = append(node.NetworkInterfaces, dev)
		}
		c.Nodes = append(c.Nodes, node)
	}

	// Mirror the loader: merge (empty) node groups, then validate.
	for k := range c.Nodes {
		node := &c.Nodes[k]
		if node.ControlPlane {
			node.OverrideGlobalCfg(c.ControlPlane)
		} else {
			node.OverrideGlobalCfg(c.Worker)
		}
	}
	errs, _ := c.Validate()
	if len(errs) > 0 {
		var msgs []string
		for _, e := range errs {
			msgs = append(msgs, fmt.Sprintf("%s: %v", e.Field, e.Message))
		}
		return nil, fmt.Errorf("invalid config: %s", strings.Join(msgs, "; "))
	}
	return c, nil
}

// Render runs talhelper's generate pipeline with the CLI's own defaults (the spike
// call sequence): per-node machine configs plus a talosconfig into outDir. The
// secret bundle comes from secretFile verbatim — talhelper sops-decrypts encrypted
// files in memory, plain YAML passes through, so sops never enters this codebase.
func Render(c *config.TalhelperConfig, outDir, secretFile string, offline bool) error {
	return generate.GenerateConfig(c, false, outDir, secretFile, "metal", offline, false,
		constants.TalosAPIDefaultCertificateValidityDuration)
}

// Convert splits a loaded talconfig into the CR pair — the cutover migration tool and the
// parity harness's front half. sourceDir is the talconfig's directory: the loader
// made "@" references absolute, Convert makes them "@./" relative again so the CRs
// stay portable. It then maps the CRs back and requires the round trip to reproduce
// the input exactly — a talconfig field the CR schema does not carry is an error
// here, never a silent drop.
func Convert(c *config.TalhelperConfig, sourceDir string) (*osv1alpha1.TalosCluster, []osv1alpha1.TalosNode, error) {
	absDir, err := filepath.Abs(sourceDir)
	if err != nil {
		return nil, nil, err
	}

	cluster := &osv1alpha1.TalosCluster{}
	cluster.Name = c.ClusterName
	cluster.TypeMeta = clusterTypeMeta()
	cluster.Spec = osv1alpha1.TalosClusterSpec{
		TalosVersion:                   c.TalosVersion,
		KubernetesVersion:              c.KubernetesVersion,
		Endpoint:                       c.Endpoint,
		AllowSchedulingOnControlPlanes: c.AllowSchedulingOnControlPlanes,
		ClusterPodNets:                 c.ClusterPodNets,
		ClusterSvcNets:                 c.ClusterSvcNets,
		AdditionalApiServerCertSans:    c.AdditionalApiServerCertSans,
		AdditionalMachineCertSans:      c.AdditionalMachineCertSans,
		Patches:                        relativizePatches(c.Patches, absDir),
	}

	var nodes []osv1alpha1.TalosNode
	for i := range c.Nodes {
		n := &c.Nodes[i]
		node := osv1alpha1.TalosNode{}
		node.Name = n.Hostname
		node.TypeMeta = nodeTypeMeta()
		node.Spec = osv1alpha1.TalosNodeSpec{
			ClusterRef:   osv1alpha1.Reference{Name: c.ClusterName},
			IPAddress:    n.IPAddress,
			ControlPlane: n.ControlPlane,
			NodeLabels:   n.NodeLabels,
			Patches:      relativizePatches(n.Patches, absDir),
		}
		if s := n.Schematic; s != nil {
			node.Spec.Schematic = &osv1alpha1.Schematic{Customization: osv1alpha1.SchematicCustomization{
				SystemExtensions: osv1alpha1.SystemExtensions{
					OfficialExtensions: s.Customization.SystemExtensions.OfficialExtensions,
				},
				ExtraKernelArgs: s.Customization.ExtraKernelArgs,
			}}
		}
		if d := n.InstallDiskSelector; d != nil {
			node.Spec.InstallDiskSelector = &osv1alpha1.InstallDiskSelector{
				Serial:  d.Serial,
				Model:   d.Model,
				Name:    d.Name,
				BusPath: d.BusPath,
			}
		}
		for _, dev := range n.NetworkInterfaces {
			ni := osv1alpha1.NetworkInterface{
				Interface: dev.DeviceInterface,
				DHCP:      dev.DeviceDHCP,
				MTU:       dev.DeviceMTU,
				Addresses: dev.DeviceAddresses,
			}
			if sel := dev.DeviceSelector; sel != nil {
				ni.DeviceSelector = &osv1alpha1.DeviceSelector{
					HardwareAddr: sel.NetworkDeviceHardwareAddress,
					BusPath:      sel.NetworkDeviceBus,
				}
			}
			for _, r := range dev.DeviceRoutes {
				ni.Routes = append(ni.Routes, osv1alpha1.Route{
					Network: r.RouteNetwork,
					Gateway: r.RouteGateway,
					Metric:  r.RouteMetric,
				})
			}
			if dev.DeviceVIPConfig != nil {
				ni.VIP = &osv1alpha1.VIP{IP: dev.DeviceVIPConfig.SharedIP}
			}
			node.Spec.NetworkInterfaces = append(node.Spec.NetworkInterfaces, ni)
		}
		nodes = append(nodes, node)
	}

	back, err := Map(cluster, nodes, absDir)
	if err != nil {
		return nil, nil, fmt.Errorf("round-trip guard: mapping the converted CRs back failed: %w", err)
	}
	want, err := yaml.Marshal(c)
	if err != nil {
		return nil, nil, err
	}
	got, err := yaml.Marshal(back)
	if err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(want, got) {
		return nil, nil, fmt.Errorf(
			"round-trip guard: the talconfig uses fields the CR schema does not carry yet — first divergence:\n%s",
			firstDiff(string(want), string(got)))
	}
	return cluster, nodes, nil
}

// resolvePatches makes relative "@" file references absolute against root —
// talhelper's own rule (pkg/substitute): only "@"-prefixed strings are file
// references, everything else is an inline patch.
func resolvePatches(patches []string, root string) []string {
	if len(patches) == 0 {
		return nil
	}
	out := make([]string, len(patches))
	for i, p := range patches {
		path, found := strings.CutPrefix(p, "@")
		if found && path != "" && !filepath.IsAbs(path) {
			p = "@" + filepath.Join(root, path)
		}
		out[i] = p
	}
	return out
}

// relativizePatches is the inverse: "@<absDir>/x" becomes "@./x".
func relativizePatches(patches []string, absDir string) []string {
	if len(patches) == 0 {
		return nil
	}
	out := make([]string, len(patches))
	for i, p := range patches {
		if rest, found := strings.CutPrefix(p, "@"+absDir+"/"); found {
			p = "@./" + rest
		}
		out[i] = p
	}
	return out
}

func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range min(len(w), len(g)) {
		if w[i] != g[i] {
			return fmt.Sprintf("line %d:\n  talconfig: %s\n  round-trip: %s", i+1, w[i], g[i])
		}
	}
	return fmt.Sprintf("line %d: one side ends early", min(len(w), len(g))+1)
}

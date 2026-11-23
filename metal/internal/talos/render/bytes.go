package render

import (
	"bytes"
	"fmt"

	"github.com/budimanjojo/talhelper/v3/pkg/config"
	"github.com/budimanjojo/talhelper/v3/pkg/patcher"
	"github.com/budimanjojo/talhelper/v3/pkg/substitute"
	thtalos "github.com/budimanjojo/talhelper/v3/pkg/talos"
	tconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"gopkg.in/yaml.v3"
)

// Input builds the generate input from the secrets bundle as bytes — talhelper's
// NewClusterInput with the file read replaced by what the operator gets from the
// cluster secrets Secret. Everything after the read mirrors talhelper verbatim
// (env substitution, strict decode, its option sequence for metal mode);
// TestRenderNodeAgainstFiles locks both entrances to the same output bytes.
func Input(c *config.TalhelperConfig, secretsBundle []byte) (*generate.Input, error) {
	vc, err := tconfig.ParseContractFromVersion(c.GetTalosVersion())
	if err != nil {
		return nil, err
	}
	if len(secretsBundle) == 0 {
		return nil, fmt.Errorf("secrets bundle is empty")
	}
	substituted, err := substitute.SubstituteEnvFromByte(secretsBundle)
	if err != nil {
		return nil, err
	}
	var sb *secrets.Bundle
	dec := yaml.NewDecoder(bytes.NewReader(substituted))
	dec.KnownFields(true)
	if err := dec.Decode(&sb); err != nil {
		return nil, fmt.Errorf("decode secrets bundle: %w", err)
	}
	sb.Clock = secrets.NewClock()

	opts := []generate.Option{
		generate.WithVersionContract(vc),
		generate.WithSecretsBundle(sb),
		generate.WithInstallImage("ghcr.io/siderolabs/installer:" + c.GetTalosVersion()),
	}
	if c.AllowSchedulingOnMasters || c.AllowSchedulingOnControlPlanes {
		opts = append(opts, generate.WithAllowSchedulingOnControlPlanes(true))
	}
	if c.Domain != "" {
		opts = append(opts, generate.WithDNSDomain(c.Domain))
	}

	input, err := generate.NewInput(c.ClusterName, c.Endpoint, c.GetK8sVersion(), opts...)
	if err != nil {
		return nil, err
	}
	input.PodNet = c.GetClusterPodNets()
	input.ServiceNet = c.GetClusterSvcNets()
	input.AdditionalMachineCertSANs = c.AdditionalMachineCertSans
	input.AdditionalSubjectAltNames = c.AdditionalApiServerCertSans
	return input, nil
}

// RenderNode renders one node's machine config in memory — the per-node body of
// talhelper's GenerateConfig without the file writes (and without the
// talosconfig, whose client cert is minted per run). The mode is always metal:
// these configs exist to reach hardware.
func RenderNode(c *config.TalhelperConfig, hostname string, input *generate.Input, offline bool) ([]byte, error) {
	// Map never fills these — they exist on file-loaded talconfigs only. Refuse
	// rather than render something GenerateConfig would have treated differently.
	if c.CNIConfig != nil || len(c.ClusterInlineManifests) > 0 {
		return nil, fmt.Errorf("cniConfig/clusterInlineManifests are not carried by the CR schema")
	}

	var node *config.Node
	for i := range c.Nodes {
		if c.Nodes[i].Hostname == hostname {
			node = &c.Nodes[i]
			break
		}
	}
	if node == nil {
		return nil, fmt.Errorf("no node %q in cluster %s", hostname, c.ClusterName)
	}
	if len(node.ExtraManifests) > 0 {
		return nil, fmt.Errorf("extraManifests are not carried by the CR schema")
	}

	rawcfg, err := thtalos.GenerateNodeConfig(node, input, c.GetImageFactory(), offline)
	if err != nil {
		return nil, err
	}
	cfg, err := rawcfg.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		return nil, err
	}
	cfg, err = thtalos.AddMultiDocs(node, "metal", cfg, input.Options.VersionContract)
	if err != nil {
		return nil, err
	}
	if len(node.Patches) != 0 {
		if cfg, err = patcher.PatchesPatcher(node.Patches, cfg); err != nil {
			return nil, err
		}
	}
	if len(c.Patches) > 0 {
		if cfg, err = patcher.PatchesPatcher(c.Patches, cfg); err != nil {
			return nil, err
		}
	}
	if err := thtalos.ValidateConfigFromBytes(cfg, "metal"); err != nil {
		return nil, err
	}
	return reencodeYaml(cfg)
}

// reencodeYaml is talhelper's final normalization pass (2-space indent),
// reproduced because it is unexported there — parity depends on it.
func reencodeYaml(input []byte) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(input))
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)

	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, err
		}
		if err := enc.Encode(&node); err != nil {
			return nil, err
		}
	}
	// Close flushes — it must precede the buffer read, not sit in a defer.
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

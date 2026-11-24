package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// renderCmd: TalosCluster/TalosNode manifests in, Talos machine configs out,
// through the same renderer the operator runs. Day-0 shape: it reads manifest
// files (ConfigMaps for patches included), no API server involved. The secrets
// bundle is read from a file or stdin, so a sealed bundle can be decrypted
// straight into it without plaintext touching disk.
func renderCmd(args []string) {
	fs := newFlags("render")
	dir := fs.String("dir", "", "directory with TalosCluster/TalosNode (and patch ConfigMap) manifests (required)")
	secretsFile := fs.String("secrets", "", "secrets bundle from `talosctl gen secrets`, or - for stdin (required)")
	out := fs.String("out", "clusterconfig", "output directory: <node>.yaml per node, talosconfig, kubeconfig")
	factoryHost := fs.String("image-factory", render.PublicFactory, "image factory host the installer image is named on")
	register := fs.Bool("register", false, "tell the image factory about each schematic (needs network); off computes ids offline")
	credsTTL := fs.Duration("credentials-ttl", 24*time.Hour,
		"lifetime of the admin talosconfig and kubeconfig (at most 720h); they cannot be revoked short of a CA rotation")
	noCreds := fs.Bool("no-credentials", false, "write the node configs only, no admin talosconfig or kubeconfig")
	parseFlags(fs, args)
	if fs.NArg() != 0 || *dir == "" || *secretsFile == "" {
		badUsage(fs, "--dir and --secrets are required, and render takes no other arguments")
	}
	if !*noCreds && (*credsTTL <= 0 || *credsTTL > render.MaxCredentialTTL) {
		badUsage(fs, fmt.Sprintf("--credentials-ttl must be more than 0 and at most %s", render.MaxCredentialTTL))
	}

	m, bundle := loadInputs(*dir, *secretsFile)
	configs, err := renderAll(context.Background(), m, bundle, render.NewFactory(*factoryHost), *register)
	if err != nil {
		die(err)
	}
	files := map[string][]byte{}
	for name, cfg := range configs {
		files[name+".yaml"] = cfg
	}
	what := fmt.Sprintf("%d node configs", len(configs))
	if !*noCreds {
		tc, kc, err := adminCredentials(m, bundle, *credsTTL)
		if err != nil {
			die(err)
		}
		files["talosconfig"], files["kubeconfig"] = tc, kc
		what += fmt.Sprintf(", talosconfig and kubeconfig (valid %s)", *credsTTL)
	}
	if err := writeSecretFiles(*out, files); err != nil {
		die(err)
	}
	fmt.Printf("rendered %s for %s into %s\n", what, m.Cluster.Name, *out)
}

// adminCredentials mints the cluster's admin talosconfig (for the control
// planes' addresses) and kubeconfig, both valid for ttl.
func adminCredentials(m *render.Manifests, bundle *secrets.Bundle, ttl time.Duration) (talosconfig, kubeconfig []byte, err error) {
	var endpoints []string
	for _, n := range m.Nodes {
		if n.Spec.Role == osv1alpha1.RoleControlPlane {
			if addr := render.NodeAddress(&n); addr != "" {
				endpoints = append(endpoints, addr)
			}
		}
	}
	if talosconfig, err = render.Talosconfig(bundle, m.Cluster.Name, endpoints, ttl); err != nil {
		return nil, nil, err
	}
	if kubeconfig, err = render.Kubeconfig(bundle, m.Cluster.Name, m.Cluster.Spec.Endpoint, ttl); err != nil {
		return nil, nil, err
	}
	return talosconfig, kubeconfig, nil
}

// loadInputs reads the manifests and the secrets bundle, or exits.
func loadInputs(dir, secretsFile string) (*render.Manifests, *secrets.Bundle) {
	m, err := render.LoadDir(dir)
	if err != nil {
		die(err)
	}
	var data []byte
	if secretsFile == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(secretsFile)
	}
	if err != nil {
		die(err)
	}
	bundle, err := render.LoadBundle(data, m.Cluster.Spec.TalosVersion)
	if err != nil {
		die(err)
	}
	return m, bundle
}

// renderAll renders every node in the manifests, keyed by node name.
func renderAll(ctx context.Context, m *render.Manifests, bundle *secrets.Bundle, f *render.Factory, register bool) (map[string][]byte, error) {
	read := m.ConfigMapReader()
	clusterPatches, err := render.ResolvePatches(m.Cluster.Spec.Patches, read)
	if err != nil {
		return nil, fmt.Errorf("cluster %s: %w", m.Cluster.Name, err)
	}
	out := map[string][]byte{}
	for i := range m.Nodes {
		n := &m.Nodes[i]
		schematic := render.EffectiveSchematic(m.Cluster, n)
		if register {
			if _, err := f.Register(ctx, schematic); err != nil {
				return nil, fmt.Errorf("node %s: %w", n.Name, err)
			}
		}
		installer, err := f.InstallerImage(schematic, m.Cluster.Spec.TalosVersion)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", n.Name, err)
		}
		nodePatches, err := render.ResolvePatches(n.Spec.Patches, read)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", n.Name, err)
		}
		cfg, err := render.Node(render.Input{
			Cluster: m.Cluster, Node: n, Bundle: bundle, InstallerImage: installer,
			ClusterPatches: clusterPatches, NodePatches: nodePatches,
		})
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", n.Name, err)
		}
		out[n.Name] = cfg
	}
	return out, nil
}

// writeSecretFiles writes files (name to content) into dir, owner-only:
// every output here carries key material. Modes are set after the write, so
// a directory or file that already existed with wider permissions is
// narrowed too (MkdirAll and WriteFile keep an existing mode).
func writeSecretFiles(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	}
	return nil
}

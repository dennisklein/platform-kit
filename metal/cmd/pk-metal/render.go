package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// renderCmd: TalosCluster/TalosNode CR manifests in, Talos machine
// configs out — `talhelper genconfig` with CRs instead of talconfig.yaml, byte-
// identical output (the parity gate). Day-0 shape: reads manifest FILES, no API
// server; the secret file may be sops-encrypted (decrypted in memory by talhelper's
// machinery, plaintext never touches disk here).
func renderCmd(args []string) {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	dir := fs.String("dir", "", "directory with TalosCluster/TalosNode manifests (required)")
	secret := fs.String("secret", "", "talsecret file — plain or sops-encrypted (required)")
	out := fs.String("out", "clusterconfig", "output directory")
	patchRoot := fs.String("patch-root", "", `directory "@./" patch refs resolve against (default: --dir)`)
	offline := fs.Bool("offline", true, "compute schematic IDs locally instead of asking the image factory")
	_ = fs.Parse(args)
	if fs.NArg() != 0 || *dir == "" || *secret == "" {
		fmt.Fprintln(os.Stderr, "usage: pk-metal render --dir <cr-dir> --secret <talsecret.yaml> [--out clusterconfig] [--patch-root dir] [--offline=false]")
		os.Exit(2)
	}
	if *patchRoot == "" {
		*patchRoot = *dir
	}

	cluster, nodes, err := render.LoadDir(*dir)
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := render.Map(cluster, nodes, *patchRoot)
	if err != nil {
		log.Fatal(err)
	}
	if err := render.Render(cfg, *out, *secret, *offline); err != nil {
		log.Fatal(err)
	}
}

// convertCmd translates talconfig.yaml into the CR pair — the parity harness's
// front half today, the migration tool at cutover. Refuses (rather than drops) any
// talconfig field the CR schema does not carry.
func convertCmd(args []string) {
	fs := flag.NewFlagSet("convert", flag.ExitOnError)
	talosDir := fs.String("talos-dir", "", "directory with talconfig.yaml (+ talenv files, patches/) (required)")
	out := fs.String("out", "", "directory to write the CR manifests into (required)")
	var envFiles stringsFlag
	fs.Var(&envFiles, "env-file", "env file for ${VAR} substitution, repeatable (default: talenv.yaml, talenv.inventory.yaml if present)")
	_ = fs.Parse(args)
	if fs.NArg() != 0 || *talosDir == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: pk-metal convert --talos-dir <dir> --out <dir> [--env-file talenv.yaml]...")
		os.Exit(2)
	}
	if len(envFiles) == 0 {
		for _, name := range []string{"talenv.yaml", "talenv.inventory.yaml"} {
			if _, err := os.Stat(filepath.Join(*talosDir, name)); err == nil {
				envFiles = append(envFiles, filepath.Join(*talosDir, name))
			}
		}
	}

	cfg, err := render.LoadTalconfig(filepath.Join(*talosDir, "talconfig.yaml"), envFiles)
	if err != nil {
		log.Fatal(err)
	}
	cluster, nodes, err := render.Convert(cfg, *talosDir)
	if err != nil {
		log.Fatal(err)
	}
	if err := render.WriteFiles(*out, cluster, nodes); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("converted %s into %d manifests in %s\n", cfg.ClusterName, 1+len(nodes), *out)
}

// stringsFlag is a repeatable string flag.
type stringsFlag []string

func (s *stringsFlag) String() string { return fmt.Sprint([]string(*s)) }
func (s *stringsFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// docgen writes the reference data of the documentation site as JSON: one file
// per CRD with every field and its validation rules, one file per pk-metal
// command with its flags, the RBAC roles and the markers they come from, the
// named controllers, the
// HostOperation actions, and the phase, condition, reason and annotation
// constants of the API. It also checks the shape of the hand-written glossary.
// The site renders its reference tables from these files,
// so a table cannot disagree with the code. Runs as part of `mise run generate`.
//
// usage (from metal/): go run ./hack/docgen [-pk-metal bin/pk-metal] -out ../docs/src/data/generated
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	var cfg config
	flag.StringVar(&cfg.root, "root", "..", "repository root; source paths in the output are relative to it")
	flag.StringVar(&cfg.out, "out", "../docs/src/data/generated", "output directory (docgen owns it: stale JSON files in it are removed)")
	flag.StringVar(&cfg.crdDir, "crds", "config/crd", "directory of the generated CRD manifests")
	flag.StringVar(&cfg.rbacDir, "rbac", "config/rbac", "directory of the RBAC manifests")
	flag.StringVar(&cfg.generatedRole, "rbac-role", "pk-metal-manager", "name of the role controller-gen writes from the +kubebuilder:rbac markers")
	flag.StringVar(&cfg.controllerDir, "controllers", "internal/controller", "controller package: its Named(...) controllers")
	flag.StringVar(&cfg.glossary, "glossary", "../docs/src/data/glossary.yaml", "hand-written glossary to check (empty: skip the check)")
	flag.StringVar(&cfg.pkMetal, "pk-metal", "", "pk-metal binary to read the help from (default: build ./cmd/pk-metal into a temporary directory)")
	apiDirs := flag.String("api", "api/v1alpha1,api/os/v1alpha1", "comma-separated API packages: actions, phases, conditions and enums")
	scanDirs := flag.String("scan", "api,internal,cmd", "comma-separated trees searched for +kubebuilder:rbac markers and metal.pk.io/os.pk.io keys")
	flag.Parse()
	cfg.apiDirs = splitList(*apiDirs)
	cfg.scanDirs = splitList(*scanDirs)

	if cfg.pkMetal == "" {
		tmp, err := os.MkdirTemp("", "docgen-")
		if err != nil {
			fail(err)
		}
		defer os.RemoveAll(tmp)
		cfg.pkMetal = filepath.Join(tmp, "pk-metal")
		build := exec.Command("go", "build", "-o", cfg.pkMetal, "./cmd/pk-metal")
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			fail(fmt.Errorf("build pk-metal: %w", err))
		}
	}
	if err := run(cfg, execHelp(cfg.pkMetal)); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
	os.Exit(1)
}

// config names the inputs (relative to the working directory, metal/) and the
// output directory.
type config struct {
	root          string
	out           string
	crdDir        string
	rbacDir       string
	controllerDir string
	generatedRole string
	pkMetal       string
	glossary      string
	apiDirs       []string
	scanDirs      []string
}

// run reads every input first and writes only when all of them parsed, so a
// failure never leaves a half-written data directory behind.
func run(cfg config, help helpFunc) error {
	if cfg.glossary != "" {
		if err := checkGlossary(cfg.glossary); err != nil {
			return err
		}
	}
	files := map[string]any{}

	crds, err := readCRDs(cfg)
	if err != nil {
		return err
	}
	for _, c := range crds {
		files[filepath.Join("crds", lowerKind(c.Kind)+".json")] = c
	}

	index, cmds, err := readCLI(help)
	if err != nil {
		return fmt.Errorf("pk-metal help: %w", err)
	}
	files[filepath.Join("cli", "index.json")] = index
	for _, c := range cmds {
		files[filepath.Join("cli", c.Name+".json")] = c
	}

	rbac, err := readRBAC(cfg)
	if err != nil {
		return err
	}
	files["rbac.json"] = rbac

	ctrls, err := readControllers(cfg)
	if err != nil {
		return err
	}
	files["controllers.json"] = ctrls

	src, err := readGoSources(cfg)
	if err != nil {
		return err
	}
	if err := src.actions.fillFromCRD(crds); err != nil {
		return err
	}
	files["actions.json"] = src.actions
	files["phases.json"] = src.phases
	files["enums.json"] = src.enums
	files["conditions.json"] = src.conditions
	files["keys.json"] = src.keys

	return writeAll(cfg.out, files)
}

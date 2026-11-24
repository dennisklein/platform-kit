// labrender renders the virtual lab from its one spec (lab/lab.yaml) into a
// directory: the fleet file, the manifests, the tap list, the operator's ledger
// and lab.env for lab/lab.sh. lab/lab.sh runs it on every lab:up and lab:doctor;
// the logic lives in internal/lab.
//
// usage: go run ./hack/labrender --spec <lab.yaml> --out <dir> [--nodes <cp>cp+<w>w] [--workload <cp>cp+<w>w]
package main

import (
	"flag"
	"fmt"
	"os"
	"slices"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/lab"
)

func main() {
	spec := flag.String("spec", "", "the lab spec (lab/lab.yaml)")
	out := flag.String("out", "", "directory to write the rendered files into")
	nodes := flag.String("nodes", "", "override the spec's size: <control planes>cp+<workers>w, for example 3cp+2w")
	workload := flag.String("workload", "", "add or size the second cluster of the pivot round: <control planes>cp+<workers>w")
	source := flag.String("source", "", "how the generated headers name the spec (default: --spec)")
	flag.Parse()
	if *spec == "" || *out == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: labrender --spec <lab.yaml> --out <dir> [--nodes <cp>cp+<w>w] [--workload <cp>cp+<w>w]")
		os.Exit(2)
	}
	if *source == "" {
		*source = *spec
	}
	if err := run(*spec, *out, *nodes, *workload, *source); err != nil {
		fmt.Fprintf(os.Stderr, "labrender: %v\n", err)
		os.Exit(1)
	}
}

func run(specPath, out, nodes, workload, source string) error {
	s, err := lab.Load(specPath)
	if err != nil {
		return err
	}
	if nodes != "" {
		if err := s.Resize(nodes); err != nil {
			return fmt.Errorf("PK_LAB_NODES: %w", err)
		}
	}
	if workload != "" {
		if err := s.SetWorkload(workload); err != nil {
			return fmt.Errorf("PK_LAB_WORKLOAD: %w", err)
		}
	}
	// A personality that names no fixture would only fail once the fleet
	// starts, after the network and the kind cluster are up.
	known := sim.Fixtures()
	for _, n := range s.Plan() {
		if !slices.Contains(known, n.Personality) {
			return fmt.Errorf("%s: %s: personality %q is not one of %v", specPath, n.Name, n.Personality, known)
		}
	}
	files, err := lab.Render(s, source)
	if err != nil {
		return err
	}
	return lab.Write(out, files)
}

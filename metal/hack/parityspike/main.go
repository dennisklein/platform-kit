// The parity spike (throwaway, hack/): prove pk-metal can reproduce
// `talhelper genconfig` output by importing talhelper's own packages — parity by
// construction — before deciding how much of the node/network mapping to reimplement
// natively on pkg/machinery. Mirrors cmd/genconfig.go of talhelper v3.1.13 exactly
// (same flag defaults, same call sequence).
//
//	go run ./hack/parityspike -dir <copy-of-control/talos> -out clusterconfig-spike
//
// The talosconfig client cert is minted per run (crt-ttl wall clock) and is expected
// to differ; machine configs must be byte-identical.
package main

import (
	"flag"
	"log"
	"os"

	"github.com/budimanjojo/talhelper/v3/pkg/config"
	"github.com/budimanjojo/talhelper/v3/pkg/generate"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

func main() {
	dir := flag.String("dir", ".", "directory with talconfig.yaml, talenv.yaml, talsecret.yaml, patches/")
	out := flag.String("out", "clusterconfig-spike", "output directory, relative to -dir")
	flag.Parse()

	if err := os.Chdir(*dir); err != nil { // relative @./patches/ refs resolve like the CLI
		log.Fatalf("chdir: %v", err)
	}
	cfg, err := config.LoadAndValidateFromFile("talconfig.yaml", []string{"talenv.yaml"}, true)
	if err != nil {
		log.Fatalf("load talconfig: %v", err)
	}
	err = generate.GenerateConfig(cfg, false, *out, "talsecret.yaml", "metal",
		false, false, constants.TalosAPIDefaultCertificateValidityDuration)
	if err != nil {
		log.Fatalf("generate: %v", err)
	}
	log.Printf("rendered into %s", *out)
}

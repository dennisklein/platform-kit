// pk-metal is the operator's terminal twin: the day-0 executor and the typed successor
// to tools/*.sh (ADR 0012). Verbs arrive with the features they front — see usage()
// for the current set.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/inventory"
)

// Stamped by the Makefile: -ldflags "-X main.version=<sha8>".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "sim":
		simCmd(os.Args[2:])
	case "inventory":
		inventoryCmd(os.Args[2:])
	case "check":
		checkCmd(os.Args[2:])
	case "status":
		statusCmd(os.Args[2:])
	case "approve":
		approveCmd(os.Args[2:])
	case "render":
		renderCmd(os.Args[2:])
	case "relay":
		relayCmd(os.Args[2:])
	case "boot":
		bootCmd(os.Args[2:])
	case "bootstrap":
		bootstrapCmd(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "pk-metal: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// connectFlags returns the shared endpoint/auth flag set for read-only verbs. The
// password comes ONLY from PK_METAL_BMC_PASSWORD (never argv — the bmc-inventory.sh rule).
func connectFlags(fs *flag.FlagSet) (user *string, insecure *bool) {
	user = fs.String("user", "admin", "BMC username (password via PK_METAL_BMC_PASSWORD)")
	insecure = fs.Bool("insecure", false, "skip TLS verification")
	return
}

func mustConnect(endpoint string, user string, insecure, readOnly bool) bmc.Client {
	c, err := gofishclient.Connect(context.Background(), gofishclient.Config{
		Endpoint: endpoint,
		Username: user,
		Password: os.Getenv("PK_METAL_BMC_PASSWORD"),
		Insecure: insecure,
		ReadOnly: readOnly,
	})
	if err != nil {
		log.Fatal(err)
	}
	return c
}

func inventoryCmd(args []string) {
	fs := flag.NewFlagSet("inventory", flag.ExitOnError)
	node := fs.String("node", "", "node name for the fact file (default: derived from the endpoint host)")
	address := fs.String("address", "", "address label for the fact file (default: endpoint host)")
	user, insecure := connectFlags(fs)
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: pk-metal inventory [flags] <endpoint-url>")
		os.Exit(2)
	}
	endpoint := fs.Arg(0)
	host := endpointHost(endpoint)
	if *address == "" {
		*address = host
	}
	if *node == "" {
		*node = host
	}

	c := mustConnect(endpoint, *user, *insecure, true) // inventory is read-only by construction
	defer c.Close()
	facts, err := inventory.Gather(context.Background(), c)
	if err != nil {
		log.Fatal(err)
	}
	inventory.Write(os.Stdout, *node, *address, time.Now(), facts)
}

func checkCmd(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	user, insecure := connectFlags(fs)
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: pk-metal check [flags] <endpoint-url>")
		os.Exit(2)
	}
	c := mustConnect(fs.Arg(0), *user, *insecure, true) // check is read-only by construction
	defer c.Close()
	ctx := context.Background()

	m, err := c.Manager(ctx)
	if err != nil {
		log.Fatalf("check: FAIL — %v", err)
	}
	fmt.Printf("manager:  %s fw %s\n", orDash(m.Vendor), orDash(m.FirmwareVersion))
	caps, err := c.Capabilities(ctx)
	if err != nil {
		log.Fatalf("check: FAIL — %v", err)
	}
	for _, id := range slices.Sorted(maps.Keys(caps.MediaDialect)) {
		fmt.Printf("media:    %-14s dialect=%s\n", id, caps.MediaDialect[id])
	}
	systems, err := c.Systems(ctx)
	if err != nil {
		log.Fatalf("check: FAIL — %v", err)
	}
	for _, s := range systems {
		fmt.Printf("system:   %s serial=%s power=%s\n", s.ID, orDash(s.SerialNumber), s.PowerState)
	}
	fmt.Println("check:    OK (read-only)")
}

func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return endpoint
	}
	return u.Hostname()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func simCmd(args []string) {
	if len(args) < 1 || args[0] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: pk-metal sim serve --fixture <name|path> [--listen addr]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("sim serve", flag.ExitOnError)
	fixture := fs.String("fixture", "supermicro-x12", "embedded fixture name or a path to a fixture YAML")
	listen := fs.String("listen", "127.0.0.1:8000", "listen address")
	serveTLS := fs.Bool("tls", false, "serve HTTPS with an ephemeral self-signed certificate (what spec.address requires)")
	verbose := fs.Bool("verbose", false, "log every request (default: only state-changing ones)")
	_ = fs.Parse(args[1:])

	f, err := sim.LoadFixture(*fixture)
	if err != nil {
		log.Fatal(err)
	}
	// Bind BEFORE logging, so the "serving" line means the listener is really up.
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	scheme := "http"
	if *serveTLS {
		cert, err := asset.EphemeralCert()
		if err != nil {
			log.Fatal(err)
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		scheme = "https"
	}
	log.Printf("sim: serving %s (%s) on %s://%s/redfish/v1", f.Name, f.Vendor, scheme, ln.Addr())
	log.Fatal(http.Serve(ln, logRequests(sim.NewServer(sim.NewState(f)), *verbose)))
}

// logRequests is the sim's console: state-changing requests — power, media, boot,
// the events a demo or a debugging session watches for — always log, with their
// (tiny, Redfish-action) bodies; read-only poll traffic only with verbose.
func logRequests(next http.Handler, verbose bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body string
		if r.Method != http.MethodGet && r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			body = strings.TrimSpace(string(b))
		}
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.Method == http.MethodGet && !verbose {
			return
		}
		line := fmt.Sprintf("sim: %s %s -> %d", r.Method, r.URL.Path, rec.code)
		if body != "" {
			line += " " + body
		}
		log.Print(line)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: pk-metal <command>

commands:
  version      print the build version (git sha8)
  sim serve    serve a simulated Redfish endpoint from a fixture
  inventory    Redfish inventory -> fact-file YAML on stdout (read-only)
  check        preflight an endpoint: reachability, dialects, systems (read-only)
  status       render the rack from the cluster: hosts, endpoints, pending claims
  approve      approve a HostOperation as yourself (VAP-gated)
  render       TalosCluster/TalosNode manifests -> Talos 1.14 machine configs, talosconfig, kubeconfig
  relay        L4 reverse tunnel on a BMC-reachable box (media in, Redfish out)
  boot         media boot ladder: grant-serve a config ISO, mount, override, power
  bootstrap    day-0 without an operator: [apply configs], bootstrap etcd once, wait Ready`)
}

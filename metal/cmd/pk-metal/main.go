// pk-metal is the operator's terminal twin: the day-0 executor and the typed
// command line for the BMC and the cluster. The verbs are listed in cli.go's command table.
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
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/inventory"
)

// Stamped at build time with -ldflags "-X main.version=<tag>", by `mise run build`
// and by the image build.
var version = "dev"

// connectFlags returns the shared endpoint/auth flag set for read-only verbs. The
// password comes ONLY from PK_METAL_BMC_PASSWORD (never argv, which other users of the machine can read).
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
		die(err)
	}
	return c
}

func inventoryCmd(args []string) {
	fs := newFlags("inventory")
	node := fs.String("node", "", "node name for the fact file (default: derived from the endpoint host)")
	address := fs.String("address", "", "address label for the fact file (default: endpoint host)")
	user, insecure := connectFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 1 {
		badUsage(fs, "give exactly one BMC URL, for example https://10.0.0.5")
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
		die(err)
	}
	inventory.Write(os.Stdout, *node, *address, time.Now(), facts)
}

func checkCmd(args []string) {
	fs := newFlags("check")
	user, insecure := connectFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 1 {
		badUsage(fs, "give exactly one BMC URL, for example https://10.0.0.5")
	}
	c := mustConnect(fs.Arg(0), *user, *insecure, true) // check is read-only by construction
	defer c.Close()
	ctx := context.Background()

	m, err := c.Manager(ctx)
	if err != nil {
		dief("check failed: %v", err)
	}
	fmt.Printf("manager:  %s fw %s\n", orDash(m.Vendor), orDash(m.FirmwareVersion))
	caps, err := c.Capabilities(ctx)
	if err != nil {
		dief("check failed: %v", err)
	}
	for _, id := range slices.Sorted(maps.Keys(caps.MediaDialect)) {
		fmt.Printf("media:    %-14s dialect=%s\n", id, caps.MediaDialect[id])
	}
	systems, err := c.Systems(ctx)
	if err != nil {
		dief("check failed: %v", err)
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
	if len(args) > 0 && args[0] == "fleet" {
		simFleetCmd(args[1:])
		return
	}
	if len(args) < 1 || args[0] != "serve" {
		w, code := os.Stderr, 1
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			w, code = os.Stdout, 0
		}
		fmt.Fprintln(w, `pk-metal sim: run simulated BMCs (Redfish) for tests and demos

usage: pk-metal sim serve [--fixture <name|path>] [--listen addr] [--tls] [--verbose]
       pk-metal sim fleet --config <file> [--verbose] [--state-listen addr]

run: pk-metal sim serve -h, or pk-metal sim fleet -h, for the flags`)
		os.Exit(code)
	}
	fs := newFlags("sim serve")
	fixture := fs.String("fixture", "generic",
		"embedded fixture name ("+strings.Join(sim.Fixtures(), ", ")+") or a path to a fixture YAML")
	listen := fs.String("listen", "127.0.0.1:8000", "listen address")
	serveTLS := fs.Bool("tls", false, "serve HTTPS with an ephemeral self-signed certificate (what spec.address requires)")
	verbose := fs.Bool("verbose", false, "log every request (default: only state-changing ones)")
	parseFlags(fs, args[1:])

	f, err := sim.LoadFixture(*fixture)
	if err != nil {
		die(err)
	}
	// Bind BEFORE logging, so the "serving" line means the listener is really up.
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		die(err)
	}
	scheme := "http"
	if *serveTLS {
		cert, err := asset.EphemeralCert()
		if err != nil {
			die(err)
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		scheme = "https"
	}
	account := "no accounts, open"
	if len(f.Accounts) > 0 {
		// A fixture value, not a secret: the factory account is how anybody
		// gets in at all.
		account = "account " + f.Accounts[0].Username + " / " + f.Accounts[0].Password
	}
	log.Printf("sim: serving %s (%s) on %s://%s/redfish/v1 (%s)", f.Name, f.Vendor, scheme, ln.Addr(), account)
	die(http.Serve(ln, logRequests(sim.NewServer(sim.NewState(f)), "sim", *verbose)))
}

// logRequests is the sim's console: state-changing requests — power, media, boot,
// the events a demo or a debugging session watches for — always log, with their
// (tiny, Redfish-action) JSON bodies; read-only poll traffic only with verbose.
// A body that is not JSON — a firmware image, a certificate upload — is not
// read here at all, and what is logged has its secrets blanked.
func logRequests(next http.Handler, prefix string, verbose bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body string
		if r.Method != http.MethodGet && r.Body != nil &&
			strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			body = loggable(b)
		}
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.Method == http.MethodGet && !verbose {
			return
		}
		line := fmt.Sprintf("%s: %s %s -> %d", prefix, r.Method, r.URL.Path, rec.code)
		if body != "" {
			line += " " + body
		}
		log.Print(line)
	})
}

// Secrets a Redfish request body can carry: a Password property (session
// login, account PATCH, media credentials) and userinfo in an image URL.
var (
	passwordValue = regexp.MustCompile(`("Password"\s*:\s*)"(?:[^"\\]|\\.)*"`)
	urlUserinfo   = regexp.MustCompile(`://[^/@\s"]+@`)
)

// loggable renders a request body for the console: secrets blanked, one line,
// cut to a readable length.
func loggable(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	s = passwordValue.ReplaceAllString(s, `${1}"***"`)
	s = urlUserinfo.ReplaceAllString(s, "://***@")
	if len(s) > 512 {
		s = s[:512] + "…"
	}
	return s
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// manager is the in-cluster pk-metal operator: the Asset materializer,
// the ManagementEndpoint reconciler, the Host projection, the claim bind, the
// HostOperation executor behind its VAP approval gate, and the OS payload lane:
// TalosNode configs rendered into Secrets, BootAsset artifacts built from them and
// served to BMCs by the media service under per-mount grants, TalosClusters
// bootstrapped and their admin credentials published.
// Runs equally against kind (day-0, `go run` with a kubeconfig) and in-cluster —
// the git-native handover means the process carries no state worth pivoting.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/cert"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/k8supgrade"
	"github.com/dennisklein/platform-kit/metal/internal/ledger"
	"github.com/dennisklein/platform-kit/metal/internal/metrics"
	"github.com/dennisklein/platform-kit/metal/internal/oci"
	pkscheme "github.com/dennisklein/platform-kit/metal/internal/scheme"
	"github.com/dennisklein/platform-kit/metal/internal/talos/machineapi"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// Stamped at build time with -ldflags "-X main.version=<tag>", by `mise run build`
// and by the image build.
var version = "dev"

// Leader election keeps its Lease in the namespace the manager runs in, and
// client-go records the election's events there through the core events API.
// The blank line below keeps this block off main's documentation: the generator
// reads these markers only from a comment that documents nothing.
//
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;create;update,namespace=pk-metal
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch,namespace=pk-metal
//
// The manager reads its own Pod once, to learn the image it runs from: the
// imager Jobs run a copy of this binary (see imager.go).
//
// +kubebuilder:rbac:groups="",resources=pods,verbs=get,namespace=pk-metal

func main() {
	if code, ok := runSubcommand(os.Args[1:], os.Stderr); ok {
		os.Exit(code)
	}
	c, err := parseFlags(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "manager:", err)
		os.Exit(2)
	}
	if c.showVersion {
		fmt.Println(version)
		return
	}

	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("manager")

	led, err := ledger.Load(c.ledgerPath)
	if err != nil {
		log.Error(err, "load ledger")
		os.Exit(1)
	}
	log.Info("ledger loaded", "path", c.ledgerPath, "entries", led.Len(), "version", version)
	// A bad line is skipped, not fatal: the other machines keep being managed,
	// and an Asset naming a skipped MAC says so in its Materialized condition.
	for _, p := range led.Problems() {
		log.Error(fmt.Errorf("%s", p), "ledger entry skipped; fix it in the ledger ConfigMap and restart the manager", "path", c.ledgerPath)
	}

	// One factory for both lanes: the boot medium's URL and the installer image
	// inside a rendered config have to name the same one, or a node installs
	// from somewhere nobody registered a schematic with.
	factory := render.NewFactory(c.imageFactory)
	log.Info("image factory", "host", c.imageFactory)

	tlsDefault := c.endpointTLS()

	var registryClient *http.Client
	if c.registryCA != "" {
		pool, err := cert.NewPool(c.registryCA)
		if err != nil {
			log.Error(err, "load registry CA")
			os.Exit(1)
		}
		// Clone the default transport so a site proxy and the dial and TLS
		// handshake timeouts still apply; only the trust roots change.
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{RootCAs: pool}
		registryClient = &http.Client{Transport: t}
	}

	scheme, err := pkscheme.New()
	if err != nil {
		log.Error(err, "build scheme")
		os.Exit(1)
	}

	restCfg := ctrl.GetConfigOrDie()
	// Operations the operator proposes carry its own name; admission checks it,
	// and the policy's params must name the same identity.
	proposer, err := whoAmI(restCfg, scheme)
	if err != nil {
		log.Error(err, "who am I")
		os.Exit(1)
	}
	log.Info("operator identity", "username", proposer)

	var media *asset.Service
	var mediaCertReloader *asset.Reloading
	if c.mediaURL != "" {
		certs, err := mediaCertificates(restCfg, scheme, c.credsNS, c.mediaCert,
			func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...), "secret", c.mediaCert) })
		if err != nil {
			log.Error(err, "media certificate")
			os.Exit(1)
		}
		if media, err = asset.NewService(c.mediaListen, c.mediaURL, tls.Certificate{}); err != nil {
			log.Error(err, "media service")
			os.Exit(1)
		}
		media.Certs = certs
		mediaCertReloader, _ = certs.(*asset.Reloading)
		log.Info("media service", "listen", c.mediaListen, "url", c.mediaURL, "certSecret", c.mediaCert)
	}
	var store *asset.Store
	helper := c.imagerHelper
	if c.mediaStore != "" {
		if store, err = asset.NewStore(c.mediaStore); err != nil {
			log.Error(err, "media store")
			os.Exit(1)
		}
		if helper == "" && c.storeClaim != "" {
			if helper, err = ownImage(restCfg, scheme); err != nil {
				log.Error(err, "find the manager's own image for the imager Jobs; set --imager-helper-image")
				os.Exit(1)
			}
		}
		log.Info("media store", "dir", c.mediaStore, "claim", c.storeClaim, "imager", c.imagerRepo,
			"pinned", c.imagerPins.String(), "helper", helper)
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cacheOptions(c.credsNS),
		Metrics:                metricsserver.Options{BindAddress: c.metricsAddr},
		HealthProbeBindAddress: c.probeAddr,
		LeaderElection:         c.leaderElect,
		LeaderElectionID:       "pk-metal.metal.pk.io",
		// Give the lease back on a clean stop. pk-metal pivot scales this
		// manager to zero and waits for the lease to be free before another
		// cluster's manager may act; without this it waits out the lease.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		log.Error(err, "new manager")
		os.Exit(1)
	}

	// The domain gauges read the manager's cache at scrape time and are served
	// on the same /metrics endpoint as controller-runtime's own.
	if err := metrics.Register(ctrlmetrics.Registry, mgr.GetCache(), c.credsNS); err != nil {
		log.Error(err, "register metrics")
		os.Exit(1)
	}

	// Each TalosCluster names its own secrets bundle (spec.secretsRef); a
	// cluster whose bundle is missing holds its nodes at SecretsUnavailable.
	if err := controller.SetupAll(mgr, controller.Options{
		CredentialsNamespace: c.credsNS,
		OperationsNamespace:  c.credsNS,
		Proposer:             proposer,
		Ledger:               led,
		// The standard install mounts the ledger from this ConfigMap; the
		// path is what this process actually read.
		LedgerSource:           fmt.Sprintf("the ledger (%s, from ledger.yaml in the pk-metal-ledger ConfigMap in namespace %s)", c.ledgerPath, c.credsNS),
		LedgerReload:           fmt.Sprintf("The manager reads the ledger when it starts: after an edit, run kubectl -n %s rollout restart deployment/pk-metal-manager.", c.credsNS),
		TLS:                    tlsDefault,
		PollInterval:           c.poll,
		CheckInTimeout:         c.checkIn,
		ReleaseShutdownTimeout: c.shutdown,
		RegistryClient:         registryClient,
		Media:                  media,
		MediaStore:             store,
		Imager: &controller.Imager{Repository: c.imagerRepo, Digests: c.imagerPins, StoreClaim: c.storeClaim,
			HelperImage: helper},
		Factory:          factory,
		TalosDial:        machineapi.Dial,
		EtcdBackupDir:    c.backupDir,
		EtcdBackupVolume: c.backupClaim,
		NodeName:         c.nodeName,
		// Installer images are public (the image factory, ghcr.io): pinned
		// over the default transport, which honours the proxy environment.
		ResolveImage: func(ctx context.Context, ref string) (string, error) {
			return oci.Resolve(ctx, &http.Client{Timeout: 30 * time.Second}, ref)
		},
		Workload: k8supgrade.Dial,
	}, controller.AllLanes...); err != nil {
		log.Error(err, "setup reconcilers")
		os.Exit(1)
	}

	if mediaCertReloader != nil {
		if err := mgr.Add(mediaCertReloader); err != nil {
			log.Error(err, "media certificate reloader")
			os.Exit(1)
		}
	}

	if c.probeAddr != "0" {
		if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
			log.Error(err, "add healthz")
			os.Exit(1)
		}
		if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
			log.Error(err, "add readyz")
			os.Exit(1)
		}
	}

	log.Info("starting", "credentialsNamespace", c.credsNS, "pollInterval", c.poll, "nodeName", c.nodeName)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}

// ownImage reads the manager's own Pod (POD_NAMESPACE and POD_NAME, from the
// downward API) and returns the image of its manager container. The imager
// Jobs run beside the manager, so that image is already on their node.
func ownImage(cfg *rest.Config, scheme *runtime.Scheme) (string, error) {
	ns, name := os.Getenv("POD_NAMESPACE"), os.Getenv("POD_NAME")
	if ns == "" || name == "" {
		return "", errors.New("POD_NAMESPACE and POD_NAME are not set (the media-store component sets them)")
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return "", err
	}
	pod := &corev1.Pod{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, pod); err != nil {
		return "", fmt.Errorf("pod %s/%s: %w", ns, name, err)
	}
	for _, ctr := range pod.Spec.Containers {
		if ctr.Name == "manager" {
			return ctr.Image, nil
		}
	}
	return "", fmt.Errorf("pod %s/%s has no container named manager", ns, name)
}

// whoAmI asks the apiserver which username this process acts as.
func whoAmI(cfg *rest.Config, scheme *runtime.Scheme) (string, error) {
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return "", err
	}
	ssr := &authenticationv1.SelfSubjectReview{}
	if err := cl.Create(context.Background(), ssr); err != nil {
		return "", err
	}
	return ssr.Status.UserInfo.Username, nil
}

// mediaCertReload is how often the media certificate Secret is read again.
// cert-manager renews well before expiry, so a minute's delay costs nothing.
const mediaCertReload = time.Minute

// mediaCertificates is where the media service's certificate comes from: the
// named Secret, read again every mediaCertReload so a renewed certificate is
// served without a restart, or, when none is named, a self-signed one that is
// replaced before it expires. A named Secret is never swapped for a
// self-signed certificate: while it cannot be read or does not parse, the last
// good certificate stays in service.
func mediaCertificates(cfg *rest.Config, scheme *runtime.Scheme, namespace, name string, logf func(string, ...any)) (asset.CertSource, error) {
	if name == "" {
		return asset.NewSelfSigned()
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	r, err := asset.NewReloading(context.Background(), secretKeyPair(cl, types.NamespacedName{Namespace: namespace, Name: name}), mediaCertReload)
	if err != nil {
		return nil, err
	}
	r.Logf = logf
	return r, nil
}

// secretKeyPair loads a kubernetes.io/tls Secret's key pair.
func secretKeyPair(cl client.Reader, key types.NamespacedName) func(context.Context) (tls.Certificate, error) {
	return func(ctx context.Context) (tls.Certificate, error) {
		sec := &corev1.Secret{}
		if err := cl.Get(ctx, key, sec); err != nil {
			return tls.Certificate{}, fmt.Errorf("secret %s: %w", key, err)
		}
		pair, err := tls.X509KeyPair(sec.Data[corev1.TLSCertKey], sec.Data[corev1.TLSPrivateKeyKey])
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("secret %s: %w", key, err)
		}
		return pair, nil
	}
}

// config is what the command line sets.
type config struct {
	ledgerPath   string
	credsNS      string
	caSecret     string
	bmcInsecure  bool
	poll         time.Duration
	imageFactory string
	registryCA   string
	metricsAddr  string
	probeAddr    string
	leaderElect  bool
	mediaListen  string
	mediaURL     string
	mediaCert    string
	mediaStore   string
	storeClaim   string
	backupDir    string
	backupClaim  string
	imagerRepo   string
	imagerPins   imagerDigests
	imagerHelper string
	checkIn      time.Duration
	shutdown     time.Duration
	nodeName     string
	showVersion  bool
}

// parseFlags reads the command line. It returns flag.ErrHelp after printing
// the usage for -h, and an error that names the fix for a value the manager
// cannot run with.
func parseFlags(args []string, output io.Writer) (*config, error) {
	c := &config{}
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&c.ledgerPath, "ledger", "",
		"path to the ledger file (required): YAML that maps each BMC MAC address to the BMC's https URL; entries that do not parse are logged and skipped")
	fs.StringVar(&c.credsNS, "credentials-namespace", "pk-metal",
		"namespace for the operator's Secrets: BMC logins and CAs are read from it, and node configs and cluster credentials are written to it")
	fs.StringVar(&c.caSecret, "bmc-ca-secret", "",
		"Secret (key ca.crt) with the CA that signed your BMCs' HTTPS certificates; empty trusts the system CAs")
	fs.StringVar(&c.imageFactory, "image-factory", render.PublicFactory,
		"host of the Talos image factory that builds and serves install images; a self-hosted factory works the same way")
	fs.StringVar(&c.registryCA, "registry-ca", "",
		"PEM file with the CA of the registry that serves firmware images; empty trusts the system CAs")
	fs.BoolVar(&c.bmcInsecure, "bmc-insecure", false,
		"accept any BMC certificate without checking it; for simulated BMCs in a lab only")
	fs.DurationVar(&c.poll, "poll-interval", 5*time.Minute, "how often each BMC is asked again what it reports")
	fs.StringVar(&c.metricsAddr, "metrics-bind-address", "0", "metrics address ('0' disables)")
	fs.StringVar(&c.probeAddr, "health-probe-bind-address", "0", "health probe address ('0' disables)")
	fs.BoolVar(&c.leaderElect, "leader-elect", false, "enable leader election")
	fs.StringVar(&c.mediaListen, "media-listen", ":8443",
		"address the media service listens on (HTTPS); BMCs download boot images from it")
	fs.StringVar(&c.mediaURL, "media-url", "",
		"https://host[:port] at which BMCs reach the media service (a NodePort or a load balancer); empty turns the media service off, and with it installing machines")
	fs.StringVar(&c.mediaCert, "media-cert-secret", "",
		"Secret (keys tls.crt, tls.key) with the media service's certificate, read again every minute so a renewed one is served without a restart; empty uses a self-signed one, replaced 10 days before it expires (BMCs do not check it)")
	fs.StringVar(&c.mediaStore, "media-store", "",
		"directory of the media store, where installers with a node's config built in rest (for BMCs that hold one virtual medium); empty: such machines are not installed")
	fs.StringVar(&c.storeClaim, "media-store-claim", "",
		"PersistentVolumeClaim mounted at --media-store; the imager Jobs mount it to write the installers they build. Empty with --media-store: the store is not a claim (an emptyDir) and no installer is built")
	fs.StringVar(&c.backupDir, "etcd-backup-dir", "",
		"directory of the etcd backup volume, where EtcdBackupPolicies with a pvc sink store encrypted snapshots; empty: pvc sinks are unavailable (s3 sinks need nothing here)")
	fs.StringVar(&c.backupClaim, "etcd-backup-claim", "pk-metal-etcd-backups",
		"PersistentVolumeClaim mounted at --etcd-backup-dir, which the snapshots' pvc:// locations name")
	fs.StringVar(&c.imagerRepo, "imager-repository", controller.DefaultImagerRepository,
		"where the Talos imager image is pulled from; a mirror must carry the same digests")
	c.imagerPins = imagerDigests(controller.DefaultImagerDigests())
	fs.StringVar(&c.imagerHelper, "imager-helper-image", "",
		"image with this manager binary for the imager Jobs, which run the imager through it; empty: the image this manager's own Pod runs")
	fs.Var(&c.imagerPins, "imager-digest",
		"pin the imager image of a Talos release as <version>=sha256:<digest> (repeatable); the manager knows the digest of the release it renders for")
	fs.DurationVar(&c.checkIn, "checkin-timeout", 30*time.Minute,
		"how long an install waits for the machine to come up and answer before it fails")
	fs.DurationVar(&c.shutdown, "release-shutdown-timeout", 5*time.Minute,
		"when a HostClaim is deleted, how long its machine's OS gets to shut down before the machine is forced off")
	fs.StringVar(&c.nodeName, "node-name", os.Getenv("NODE_NAME"),
		"the Kubernetes node this manager runs on (default: $NODE_NAME, which the Deployment sets from the pod's node); "+
			"the operator will not reboot the machine it runs on, nor a control plane of its own cluster, without the host's metal.pk.io/allow-self-disruption annotation")
	fs.BoolVar(&c.showVersion, "version", false, "print the build version and exit")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q: every setting is a flag, see manager -h", fs.Arg(0))
	}
	if c.showVersion {
		return c, nil
	}
	if c.ledgerPath == "" {
		return nil, errors.New("--ledger is required: the path of the ledger file, e.g. --ledger=/etc/pk-metal/ledger.yaml")
	}
	// The release operation carries it in seconds, and its schema allows an hour.
	if c.shutdown < time.Second || c.shutdown > time.Hour {
		return nil, fmt.Errorf("--release-shutdown-timeout must be between 1s and 1h, got %s", c.shutdown)
	}
	if c.poll <= 0 {
		return nil, fmt.Errorf("--poll-interval must be positive, got %s", c.poll)
	}
	if c.checkIn <= 0 {
		return nil, fmt.Errorf("--checkin-timeout must be positive, got %s", c.checkIn)
	}
	// A store without a claim is an emptyDir (pk-metal pivot
	// --media-store=ephemeral): it serves, and the BootAssets that need a
	// build say that nothing builds into it. A claim without a store names
	// a volume the manager does not use.
	if c.storeClaim != "" && c.mediaStore == "" {
		return nil, errors.New("--media-store-claim needs --media-store: the directory the manager mounts the claim at, next to the claim the imager Jobs mount")
	}
	return c, nil
}

// imagerDigests is the --imager-digest flag: Talos release -> image digest.
type imagerDigests map[string]string

func (d imagerDigests) String() string {
	var out []string
	for _, v := range slices.Sorted(maps.Keys(d)) {
		out = append(out, v+"="+d[v])
	}
	return strings.Join(out, ",")
}

func (d imagerDigests) Set(s string) error {
	v, dg, ok := strings.Cut(s, "=")
	if !ok || !strings.HasPrefix(v, "v") || !strings.HasPrefix(dg, "sha256:") || len(dg) != len("sha256:")+64 {
		return fmt.Errorf("want <talos version>=sha256:<64 hex digits>, e.g. v1.14.6=sha256:%s", strings.Repeat("0", 64))
	}
	d[v] = dg
	return nil
}

// endpointTLS is the TLS default for BMCs whose endpoint names none: a CA
// Secret wins over --bmc-insecure, and neither trusts the system CAs (nil).
func (c *config) endpointTLS() *metalv1alpha1.EndpointTLS {
	switch {
	case c.caSecret != "":
		return &metalv1alpha1.EndpointTLS{CASecretRef: &corev1.LocalObjectReference{Name: c.caSecret}}
	case c.bmcInsecure:
		return &metalv1alpha1.EndpointTLS{InsecureSkipVerify: true}
	}
	return nil
}

// cacheOptions confines the Secret and ConfigMap informers to the operator's
// namespace. Every Secret and ConfigMap the reconcilers read or write lives
// there: credentials, secrets bundles, patches, rendered configs. The
// confinement is what lets those permissions be a namespaced Role. An
// unrestricted informer lists and watches the kind in every namespace and
// needs a ClusterRole to do it, which for Secrets means reading every Secret
// in the cluster.
func cacheOptions(namespace string) cache.Options {
	return cache.Options{ByObject: controller.CacheByObject(namespace)}
}

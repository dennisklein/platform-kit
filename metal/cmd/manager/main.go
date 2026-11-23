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
	"crypto/x509"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/ledger"
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

func main() {
	var (
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
		checkIn      time.Duration
	)
	flag.StringVar(&ledgerPath, "ledger", "",
		"path to the MAC→URL BMC ledger YAML (required; projection of the bmc-access dnsmasq ledger)")
	flag.StringVar(&credsNS, "credentials-namespace", "pk-metal",
		"namespace holding endpoint credential and CA secrets; receives rendered configs and published cluster credentials")
	flag.StringVar(&caSecret, "bmc-ca-secret", "",
		"secret (key ca.crt) that materialized endpoints pin for TLS (live: internal-ca)")
	flag.StringVar(&imageFactory, "image-factory", render.PublicFactory,
		"host of the Talos image factory: it serves the install images, and is told about the schematics naming them (a self-hosted one speaks the same API)")
	flag.StringVar(&registryCA, "registry-ca", "",
		"PEM file verifying the OCI registry serving firmware images (live: the registry's internal CA); empty uses the system store")
	flag.BoolVar(&bmcInsecure, "bmc-insecure", false,
		"materialize endpoints with insecureSkipVerify (lab/sim only)")
	flag.DurationVar(&poll, "poll-interval", 5*time.Minute, "endpoint re-observation cadence")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "metrics address ('0' disables)")
	flag.StringVar(&probeAddr, "health-probe-bind-address", "0", "health probe address ('0' disables)")
	flag.BoolVar(&leaderElect, "leader-elect", false, "enable leader election")
	flag.StringVar(&mediaListen, "media-listen", ":8443",
		"address the media service binds (HTTPS; BMCs pull granted boot media from it)")
	flag.StringVar(&mediaURL, "media-url", "",
		"https://host[:port] BMCs reach the media service at (a NodePort, a load balancer); empty disables the media service and with it in-cluster provisioning")
	flag.StringVar(&mediaCert, "media-cert-secret", "",
		"secret (keys tls.crt, tls.key) the media service serves; empty mints an ephemeral self-signed certificate (BMCs do not verify media TLS)")
	flag.DurationVar(&checkIn, "checkin-timeout", 30*time.Minute,
		"how long a boot operation waits for the node to check in before it fails")
	showVersion := flag.Bool("version", false, "print the build version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if ledgerPath == "" {
		fmt.Fprintln(os.Stderr, "manager: --ledger is required")
		os.Exit(2)
	}

	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("manager")

	led, err := ledger.Load(ledgerPath)
	if err != nil {
		log.Error(err, "load ledger")
		os.Exit(1)
	}
	log.Info("ledger loaded", "path", ledgerPath, "entries", led.Len(), "version", version)
	// A bad line is skipped, not fatal: the other machines keep being managed,
	// and an Asset naming a skipped MAC says so in its Materialized condition.
	for _, p := range led.Problems() {
		log.Error(fmt.Errorf("%s", p), "ledger entry skipped; fix it in the ledger ConfigMap and restart the manager", "path", ledgerPath)
	}

	// One factory for both lanes: the boot medium's URL and the installer image
	// inside a rendered config have to name the same one, or a node installs
	// from somewhere nobody registered a schematic with.
	factory := render.NewFactory(imageFactory)
	log.Info("image factory", "host", imageFactory)

	var tlsDefault *metalv1alpha1.EndpointTLS
	switch {
	case caSecret != "":
		tlsDefault = &metalv1alpha1.EndpointTLS{CASecretRef: &corev1.LocalObjectReference{Name: caSecret}}
	case bmcInsecure:
		tlsDefault = &metalv1alpha1.EndpointTLS{InsecureSkipVerify: true}
	}

	var registryClient *http.Client
	if registryCA != "" {
		pem, err := os.ReadFile(registryCA)
		if err != nil {
			log.Error(err, "read registry CA")
			os.Exit(1)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			log.Error(fmt.Errorf("no certificates in %s", registryCA), "parse registry CA")
			os.Exit(1)
		}
		registryClient = &http.Client{
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		}
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Error(err, "add client-go scheme")
		os.Exit(1)
	}
	if err := metalv1alpha1.AddToScheme(scheme); err != nil {
		log.Error(err, "add metal scheme")
		os.Exit(1)
	}
	if err := osv1alpha1.AddToScheme(scheme); err != nil {
		log.Error(err, "add os scheme")
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
	if mediaURL != "" {
		cert, err := mediaCertificate(restCfg, scheme, credsNS, mediaCert)
		if err != nil {
			log.Error(err, "media certificate")
			os.Exit(1)
		}
		if media, err = asset.NewService(mediaListen, mediaURL, cert); err != nil {
			log.Error(err, "media service")
			os.Exit(1)
		}
		log.Info("media service", "listen", mediaListen, "url", mediaURL, "certSecret", mediaCert)
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		// Every Secret and ConfigMap the reconcilers read or write lives in the
		// operator's namespace: credentials, secrets bundles, patches, rendered
		// configs. Confining the informers to it is what lets those permissions
		// be a namespaced Role. An unrestricted informer lists and watches the
		// kind in every namespace and needs a ClusterRole to do it, which for
		// Secrets means reading every Secret in the cluster.
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Secret{}:    {Namespaces: map[string]cache.Config{credsNS: {}}},
				&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{credsNS: {}}},
			},
		},
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "pk-metal.metal.pk.io",
	})
	if err != nil {
		log.Error(err, "new manager")
		os.Exit(1)
	}

	if err := (&controller.AssetReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Ledger:   led,
		TLS:      tlsDefault,
		Recorder: mgr.GetEventRecorder("pk-metal"),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup asset reconciler")
		os.Exit(1)
	}
	if err := (&controller.ManagementEndpointReconciler{
		Client:               mgr.GetClient(),
		CredentialsNamespace: credsNS,
		PollInterval:         poll,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup managementendpoint reconciler")
		os.Exit(1)
	}
	if err := (&controller.HostReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup host reconciler")
		os.Exit(1)
	}
	if err := (&controller.HostClaimReconciler{
		Client:                mgr.GetClient(),
		OperationsNamespace:   credsNS,
		Proposer:              proposer,
		CheckInTimeoutSeconds: int32(checkIn.Seconds()),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup hostclaim reconciler")
		os.Exit(1)
	}
	if err := (&controller.ProtectionReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup protection reconciler")
		os.Exit(1)
	}
	if err := (&controller.HostOperationReconciler{
		Client:               mgr.GetClient(),
		CredentialsNamespace: credsNS,
		RegistryClient:       registryClient,
		Media:                media,
		Recorder:             mgr.GetEventRecorder("pk-metal"),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup hostoperation reconciler")
		os.Exit(1)
	}
	if media != nil {
		if err := mgr.Add(media); err != nil {
			log.Error(err, "add media service")
			os.Exit(1)
		}
	}
	if err := (&controller.ConvergenceReconciler{
		Client:               mgr.GetClient(),
		CredentialsNamespace: credsNS,
		OperationsNamespace:  credsNS,
		Proposer:             proposer,
		PollInterval:         poll,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup convergence reconciler")
		os.Exit(1)
	}
	if err := (&controller.EndpointConvergenceReconciler{
		Client:               mgr.GetClient(),
		CredentialsNamespace: credsNS,
		PollInterval:         poll,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup endpoint convergence reconciler")
		os.Exit(1)
	}
	// Each TalosCluster names its own secrets bundle (spec.secretsRef); a
	// cluster whose bundle is missing holds its nodes at SecretsUnavailable.
	if err := (&controller.TalosNodeReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Namespace: credsNS,
		Factory:   factory,
		Dial:      machineapi.Dial,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup talosnode reconciler")
		os.Exit(1)
	}
	if err := (&controller.TalosClusterReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Namespace: credsNS,
		Dial:      machineapi.Dial,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup taloscluster reconciler")
		os.Exit(1)
	}
	if err := (&controller.BootAssetReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Namespace: credsNS,
		Factory:   factory,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup bootasset reconciler")
		os.Exit(1)
	}

	if probeAddr != "0" {
		if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
			log.Error(err, "add healthz")
			os.Exit(1)
		}
		if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
			log.Error(err, "add readyz")
			os.Exit(1)
		}
	}

	log.Info("starting", "credentialsNamespace", credsNS, "pollInterval", poll)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
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

// mediaCertificate loads the media service's serving certificate from a
// Secret, or mints an ephemeral one when none is named.
func mediaCertificate(cfg *rest.Config, scheme *runtime.Scheme, namespace, name string) (tls.Certificate, error) {
	if name == "" {
		return asset.EphemeralCert()
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return tls.Certificate{}, err
	}
	sec := &corev1.Secret{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, sec); err != nil {
		return tls.Certificate{}, fmt.Errorf("secret %s/%s: %w", namespace, name, err)
	}
	return tls.X509KeyPair(sec.Data[corev1.TLSCertKey], sec.Data[corev1.TLSPrivateKeyKey])
}

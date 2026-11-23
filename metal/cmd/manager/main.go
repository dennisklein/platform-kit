// manager is the in-cluster pk-metal operator (ADR 0012): the Asset materializer,
// the ManagementEndpoint reconciler, the Host projection, the claim bind, the
// HostOperation executor behind its VAP approval gate, and the OS payload lane —
// TalosNode configs rendered into Secrets, BootAsset artifacts built from them.
// Runs equally against kind (day-0, `go run` with a kubeconfig) and in-cluster —
// the git-native handover means the process carries no state worth pivoting.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/ledger"
	"github.com/dennisklein/platform-kit/metal/internal/talos/machineapi"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// Stamped by the Makefile: -ldflags "-X main.version=<sha8>".
var version = "dev"

func main() {
	var (
		ledgerPath   string
		credsNS      string
		caSecret     string
		bmcInsecure  bool
		poll         time.Duration
		talosSecrets string
		patchRoot    string
		imageFactory string
		registryCA   string
		metricsAddr  string
		probeAddr    string
		leaderElect  bool
	)
	flag.StringVar(&ledgerPath, "ledger", "",
		"path to the MAC→URL BMC ledger YAML (required; projection of the bmc-access dnsmasq ledger)")
	flag.StringVar(&credsNS, "credentials-namespace", "pk-metal",
		"namespace holding endpoint credential and CA secrets, the cluster secrets bundle, and rendered configs")
	flag.StringVar(&talosSecrets, "talos-secrets-secret", "",
		"secret (key talsecret.yaml) with the cluster secrets bundle; empty disables config rendering")
	flag.StringVar(&patchRoot, "talos-patch-root", "",
		`directory "@./" patch references resolve against (the mounted mirror of control/talos)`)
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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
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
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup hostclaim reconciler")
		os.Exit(1)
	}
	if err := (&controller.HostOperationReconciler{
		Client:               mgr.GetClient(),
		CredentialsNamespace: credsNS,
		RegistryClient:       registryClient,
		Recorder:             mgr.GetEventRecorder("pk-metal"),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup hostoperation reconciler")
		os.Exit(1)
	}
	if err := (&controller.ConvergenceReconciler{
		Client:               mgr.GetClient(),
		CredentialsNamespace: credsNS,
		OperationsNamespace:  credsNS,
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
	// The render lane needs the cluster secrets bundle; without it TalosNodes
	// stay spec-only and configISO BootAssets hold at ConfigNotRendered.
	if talosSecrets != "" {
		if err := (&controller.TalosNodeReconciler{
			Client:        mgr.GetClient(),
			Scheme:        mgr.GetScheme(),
			SecretsSecret: talosSecrets,
			Namespace:     credsNS,
			PatchRoot:     patchRoot,
			Factory:       factory,
			Dial:          machineapi.Dial,
		}).SetupWithManager(mgr); err != nil {
			log.Error(err, "setup talosnode reconciler")
			os.Exit(1)
		}
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

package controller

import (
	"cmp"
	"fmt"
	"net/http"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/k8supgrade"
	"github.com/dennisklein/platform-kit/metal/internal/ledger"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// Lane names one reconciler that SetupAll can wire. The manager wires every
// lane; a test suite picks the lanes it exercises, so it observes only what
// those lanes do.
type Lane int

// The lanes, in the order SetupAll wires them.
const (
	LaneAsset Lane = iota
	LaneEndpoint
	LaneHost
	LaneHostClaim
	LaneProtection
	LaneHostOperation
	LaneConvergence
	LaneEndpointConvergence
	LaneTalosNode
	LaneTalosCluster
	LaneBootAsset
	LaneEtcdBackup
	LaneTalosRollout
	LaneMaintenanceWindow
	LaneSettings
	laneCount
)

// AllLanes is every lane: what the manager runs.
var AllLanes = []Lane{
	LaneAsset, LaneEndpoint, LaneHost, LaneHostClaim, LaneProtection, LaneHostOperation,
	LaneConvergence, LaneEndpointConvergence, LaneTalosNode, LaneTalosCluster, LaneBootAsset,
	LaneEtcdBackup,
	LaneTalosRollout,
	LaneMaintenanceWindow,
	LaneSettings,
}

// Options is what the reconcilers are configured with. The manager fills it
// from its flags. A test fills what its lanes need; a field left at its zero
// value leaves the matching reconciler field at its zero value, which is the
// reconciler's own default.
type Options struct {
	// CredentialsNamespace is the operator's namespace: credential and CA
	// Secrets are read from it, and rendered configs, artifacts and cluster
	// credentials are written to it.
	CredentialsNamespace string
	// OperationsNamespace is where HostOperations are proposed.
	OperationsNamespace string
	// Proposer is the operator's own username, stamped as proposedBy.
	Proposer string
	// EventSource names the event recorder; empty uses "pk-metal".
	EventSource string
	// Audit writes the audit trail of the operation, claim, convergence,
	// node and rollout lanes; nil writes none.
	Audit *audit.Logger

	// Ledger, LedgerSource, LedgerReload and TLS configure the Asset lane.
	Ledger       *ledger.Ledger
	LedgerSource string
	LedgerReload string
	TLS          *metalv1alpha1.EndpointTLS

	// PollInterval is the re-observation cadence of the endpoint and
	// convergence lanes.
	PollInterval time.Duration

	// CheckInTimeout and ReleaseShutdownTimeout configure the HostClaim lane.
	CheckInTimeout         time.Duration
	ReleaseShutdownTimeout time.Duration

	// RegistryClient, Media and WaitInterval configure the HostOperation
	// lane. A non-nil Media is also added to the manager as a runnable.
	RegistryClient *http.Client
	Media          *asset.Service
	WaitInterval   time.Duration
	// GuardInterval paces the next look at an operation the self or
	// cluster guard holds; zero is 30s.
	GuardInterval time.Duration

	// MediaStore holds the artifacts too large for a Secret; the BootAsset
	// lane writes them, the HostOperation lane serves them under grants.
	// Imager builds them. Either nil leaves embeddedInstaller assets held.
	MediaStore *asset.Store
	Imager     *Imager

	// Factory, TalosDial, ProbeKubernetes, CheckInterval and ReadyInterval
	// configure the OS lanes.
	Factory         *render.Factory
	TalosDial       talos.Dialer
	ProbeKubernetes KubernetesProber
	CheckInterval   time.Duration
	ReadyInterval   time.Duration

	// EtcdBackupDir is where the manager's etcd backup volume is mounted
	// (empty: none, pvc sinks are unavailable); EtcdBackupVolume names it in
	// the snapshots' pvc:// locations.
	EtcdBackupDir    string
	EtcdBackupVolume string

	// ResolveImage, Workload and GateInterval configure the rollout lane;
	// DriftInterval paces the TalosNode lane's dry-run drift check.
	ResolveImage  ImageResolver
	Workload      k8supgrade.Dialer
	GateInterval  time.Duration
	DriftInterval time.Duration

	// SettingsInterval paces the settings lane's check of the admission
	// gate; zero is one minute.
	SettingsInterval time.Duration

	// NodeName is the Kubernetes node the manager's pod runs on. When set,
	// the claim, operation and convergence lanes refuse to reboot the
	// machines the operator itself runs on (SelfGuard).
	NodeName string
}

// CacheByObject confines the manager's cache to the operator's namespace for
// the kinds whose permissions are a Role there and nowhere else: Secrets,
// ConfigMaps and the imager Jobs. A cluster-wide informer for them would be
// refused.
func CacheByObject(namespace string) map[client.Object]cache.ByObject {
	ns := map[string]cache.Config{namespace: {}}
	return map[client.Object]cache.ByObject{
		&corev1.Secret{}:    {Namespaces: ns},
		&corev1.ConfigMap{}: {Namespaces: ns},
		&batchv1.Job{}:      {Namespaces: ns},
	}
}

// reconciler is what every lane's reconciler offers SetupAll.
type reconciler interface{ SetupWithManager(ctrl.Manager) error }

// reconcilers sets up several reconcilers as one lane.
type reconcilers []reconciler

func (rs reconcilers) SetupWithManager(mgr ctrl.Manager) error {
	for _, r := range rs {
		if err := r.SetupWithManager(mgr); err != nil {
			return err
		}
	}
	return nil
}

// SetupAll wires the chosen lanes into mgr with o, in lane order. It is the
// one place that builds the reconcilers, so the manager and the test suites
// run the same configuration.
func SetupAll(mgr ctrl.Manager, o Options, lanes ...Lane) error {
	if len(lanes) == 0 {
		return fmt.Errorf("no lanes to set up")
	}
	var want [laneCount]bool
	for _, l := range lanes {
		if l < 0 || l >= laneCount {
			return fmt.Errorf("unknown lane %d", l)
		}
		want[l] = true
	}
	for l := range laneCount {
		if !want[l] {
			continue
		}
		name, r := o.reconciler(mgr, l)
		if err := r.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setup %s reconciler: %w", name, err)
		}
		if l == LaneHostOperation && o.Media != nil {
			o.Media.Grants.Record = mediaAuditor(o.Audit)
			if err := mgr.Add(o.Media); err != nil {
				return fmt.Errorf("add media service: %w", err)
			}
		}
	}
	return nil
}

// reconciler builds the reconciler of one lane from o.
func (o Options) reconciler(mgr ctrl.Manager, l Lane) (string, reconciler) {
	c, scheme := mgr.GetClient(), mgr.GetScheme()
	var self *SelfGuard
	if o.NodeName != "" {
		self = &SelfGuard{Reader: mgr.GetAPIReader(), NodeName: o.NodeName, Dial: o.TalosDial, Namespace: o.CredentialsNamespace}
	}
	recorder := func() events.EventRecorder {
		return mgr.GetEventRecorder(cmp.Or(o.EventSource, "pk-metal"))
	}
	switch l {
	case LaneAsset:
		return "asset", &AssetReconciler{
			Client: c, Scheme: scheme, Ledger: o.Ledger,
			LedgerSource: o.LedgerSource, LedgerReload: o.LedgerReload,
			TLS: o.TLS, Recorder: recorder(),
		}
	case LaneEndpoint:
		return "managementendpoint", &ManagementEndpointReconciler{
			Client: c, CredentialsNamespace: o.CredentialsNamespace, PollInterval: o.PollInterval, Audit: o.Audit,
		}
	case LaneHost:
		return "host", &HostReconciler{Client: c}
	case LaneHostClaim:
		claims := &HostClaimReconciler{
			Client: c, OperationsNamespace: o.OperationsNamespace, Proposer: o.Proposer, Audit: o.Audit,
			CheckInTimeoutSeconds:  int32(o.CheckInTimeout.Seconds()),
			ReleaseShutdownTimeout: o.ReleaseShutdownTimeout,
			Self:                   self,
			Dial:                   o.TalosDial, CredentialsNamespace: o.CredentialsNamespace,
		}
		// Wipes of released machines share the claims' configuration.
		return "hostclaim", reconcilers{claims, &hostWipeReconciler{claims}}
	case LaneProtection:
		return "protection", &ProtectionReconciler{Client: c}
	case LaneHostOperation:
		return "hostoperation", &HostOperationReconciler{
			Client: c, CredentialsNamespace: o.CredentialsNamespace,
			RegistryClient: o.RegistryClient, Media: o.Media, Store: o.MediaStore,
			Recorder: recorder(), WaitInterval: o.WaitInterval, Self: self,
			TalosDial: o.TalosDial, GuardInterval: o.GuardInterval, Audit: o.Audit,
			Operator: o.Proposer,
			// The cluster guard holds for every Talos cluster, wherever the
			// operator runs; SelfGuard adds the rules for its own.
			Guard: &ClusterGuard{Reader: mgr.GetAPIReader(), Dial: o.TalosDial, Namespace: o.CredentialsNamespace},
		}
	case LaneConvergence:
		return "convergence", &ConvergenceReconciler{
			Client: c, CredentialsNamespace: o.CredentialsNamespace,
			OperationsNamespace: o.OperationsNamespace, Proposer: o.Proposer,
			PollInterval: o.PollInterval, Self: self, Audit: o.Audit,
		}
	case LaneEndpointConvergence:
		return "endpoint convergence", &EndpointConvergenceReconciler{
			Client: c, CredentialsNamespace: o.CredentialsNamespace, PollInterval: o.PollInterval, Audit: o.Audit,
		}
	case LaneTalosNode:
		// The member finalizer rides with the node lane: every setup that
		// renders nodes also holds their deletion until they left.
		return "talosnode", reconcilers{&TalosNodeReconciler{
			Client: c, Scheme: scheme, Namespace: o.CredentialsNamespace,
			Factory: o.Factory, Dial: o.TalosDial,
			CheckInterval: o.CheckInterval, ReadyInterval: o.ReadyInterval, DriftInterval: o.DriftInterval,
		}, &TalosNodeMemberReconciler{
			Client: c, Dial: o.TalosDial, CredentialsNamespace: o.CredentialsNamespace,
			OperationsNamespace: o.OperationsNamespace, Audit: o.Audit,
		}}
	case LaneTalosCluster:
		return "taloscluster", &TalosClusterReconciler{
			Client: c, Scheme: scheme, Namespace: o.CredentialsNamespace,
			Dial: o.TalosDial, ProbeKubernetes: o.ProbeKubernetes,
			CheckInterval: o.CheckInterval, ReadyInterval: o.ReadyInterval,
		}
	case LaneEtcdBackup:
		return "etcd backup", &EtcdBackups{
			Client: c, Namespace: o.CredentialsNamespace, Dial: o.TalosDial,
			Dir: o.EtcdBackupDir, VolumeName: o.EtcdBackupVolume, Self: self,
			RetryInterval: o.CheckInterval,
		}
	case LaneTalosRollout:
		return "talosrollout", rolloutLane{
			run: &TalosRolloutReconciler{
				Client: c, Scheme: scheme, Namespace: o.CredentialsNamespace,
				OperationsNamespace: o.OperationsNamespace, Proposer: o.Proposer,
				Factory: o.Factory, ResolveImage: o.ResolveImage, Dial: o.TalosDial,
				Workload: o.Workload, GateInterval: o.GateInterval, Audit: o.Audit,
			},
			propose: &RolloutProposer{Client: c, Proposer: o.Proposer, Namespace: o.CredentialsNamespace},
		}
	case LaneMaintenanceWindow:
		return "maintenancewindow", &MaintenanceWindowReconciler{Client: c}
	case LaneSettings:
		return "settings", &SettingsReconciler{Client: c, Reader: mgr.GetAPIReader(),
			Namespace: o.CredentialsNamespace, Audit: o.Audit, Interval: o.SettingsInterval}
	default: // LaneBootAsset; SetupAll has checked the range
		return "bootasset", &BootAssetReconciler{
			Client: c, Scheme: scheme, Namespace: o.CredentialsNamespace, Factory: o.Factory,
			Store: o.MediaStore, Imager: o.Imager,
		}
	}
}

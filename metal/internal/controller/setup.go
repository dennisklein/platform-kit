package controller

import (
	"cmp"
	"fmt"
	"net/http"
	"time"

	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
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
	laneCount
)

// AllLanes is every lane: what the manager runs.
var AllLanes = []Lane{
	LaneAsset, LaneEndpoint, LaneHost, LaneHostClaim, LaneProtection, LaneHostOperation,
	LaneConvergence, LaneEndpointConvergence, LaneTalosNode, LaneTalosCluster, LaneBootAsset,
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

	// Factory, TalosDial, ProbeKubernetes, CheckInterval and ReadyInterval
	// configure the OS lanes.
	Factory         *render.Factory
	TalosDial       talos.Dialer
	ProbeKubernetes KubernetesProber
	CheckInterval   time.Duration
	ReadyInterval   time.Duration
}

// reconciler is what every lane's reconciler offers SetupAll.
type reconciler interface{ SetupWithManager(ctrl.Manager) error }

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
			Client: c, CredentialsNamespace: o.CredentialsNamespace, PollInterval: o.PollInterval,
		}
	case LaneHost:
		return "host", &HostReconciler{Client: c}
	case LaneHostClaim:
		return "hostclaim", &HostClaimReconciler{
			Client: c, OperationsNamespace: o.OperationsNamespace, Proposer: o.Proposer,
			CheckInTimeoutSeconds:  int32(o.CheckInTimeout.Seconds()),
			ReleaseShutdownTimeout: o.ReleaseShutdownTimeout,
		}
	case LaneProtection:
		return "protection", &ProtectionReconciler{Client: c}
	case LaneHostOperation:
		return "hostoperation", &HostOperationReconciler{
			Client: c, CredentialsNamespace: o.CredentialsNamespace,
			RegistryClient: o.RegistryClient, Media: o.Media,
			Recorder: recorder(), WaitInterval: o.WaitInterval,
		}
	case LaneConvergence:
		return "convergence", &ConvergenceReconciler{
			Client: c, CredentialsNamespace: o.CredentialsNamespace,
			OperationsNamespace: o.OperationsNamespace, Proposer: o.Proposer,
			PollInterval: o.PollInterval,
		}
	case LaneEndpointConvergence:
		return "endpoint convergence", &EndpointConvergenceReconciler{
			Client: c, CredentialsNamespace: o.CredentialsNamespace, PollInterval: o.PollInterval,
		}
	case LaneTalosNode:
		return "talosnode", &TalosNodeReconciler{
			Client: c, Scheme: scheme, Namespace: o.CredentialsNamespace,
			Factory: o.Factory, Dial: o.TalosDial,
			CheckInterval: o.CheckInterval, ReadyInterval: o.ReadyInterval,
		}
	case LaneTalosCluster:
		return "taloscluster", &TalosClusterReconciler{
			Client: c, Scheme: scheme, Namespace: o.CredentialsNamespace,
			Dial: o.TalosDial, ProbeKubernetes: o.ProbeKubernetes,
			CheckInterval: o.CheckInterval, ReadyInterval: o.ReadyInterval,
		}
	default: // LaneBootAsset; SetupAll has checked the range
		return "bootasset", &BootAssetReconciler{
			Client: c, Scheme: scheme, Namespace: o.CredentialsNamespace, Factory: o.Factory,
		}
	}
}

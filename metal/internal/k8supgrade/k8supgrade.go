// Package k8supgrade is the narrow part of `talosctl upgrade-k8s` that does
// not ride a machine config: moving the kube-proxy DaemonSet to the new
// version's image, and reading whether the cluster's nodes are Ready. The
// control-plane static pods and the kubelets move with the rendered config,
// applied without a reboot node by node; Talos itself restarts them.
//
// It talks to the workload cluster through client-go with the admin
// kubeconfig the operator publishes, never through a talosctl binary or the
// main Talos module: each change is a step the rollout records.
package k8supgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// FieldManager owns the kube-proxy image field the upgrade sets.
const FieldManager = "pk-metal-k8s-upgrade"

// The kube-proxy DaemonSet Talos deploys, and its one container.
const (
	kubeProxyNamespace = "kube-system"
	kubeProxyName      = "kube-proxy"
	kubeProxyContainer = "kube-proxy"
)

// Workload is what a rollout needs from a workload cluster's Kubernetes API.
type Workload interface {
	// NodesReady reports, by node name, whether each Node is Ready, read
	// now (not from a cache): a rollout gates on it between nodes.
	NodesReady(ctx context.Context) (map[string]bool, error)
	// SyncKubeProxy moves the kube-proxy DaemonSet to image and reports
	// whether its rollout is done. A cluster without kube-proxy is done.
	SyncKubeProxy(ctx context.Context, image string) (done bool, msg string, err error)
}

// Dialer opens a Workload from a kubeconfig.
type Dialer func(kubeconfig []byte) (Workload, error)

// Dial is the production Dialer.
func Dial(kubeconfig []byte) (Workload, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 15 * time.Second
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &workload{cs: cs}, nil
}

// New wraps a clientset; tests hand it a fake one.
func New(cs kubernetes.Interface) Workload { return &workload{cs: cs} }

type workload struct{ cs kubernetes.Interface }

func (w *workload) NodesReady(ctx context.Context) (map[string]bool, error) {
	nodes, err := w.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	out := make(map[string]bool, len(nodes.Items))
	for _, n := range nodes.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				ready = c.Status == corev1.ConditionTrue
			}
		}
		out[n.Name] = ready
	}
	return out, nil
}

func (w *workload) SyncKubeProxy(ctx context.Context, image string) (bool, string, error) {
	ds, err := w.cs.AppsV1().DaemonSets(kubeProxyNamespace).Get(ctx, kubeProxyName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, "the cluster runs no kube-proxy DaemonSet", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("get kube-proxy: %w", err)
	}
	var current string
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == kubeProxyContainer {
			current = c.Image
		}
	}
	if current == "" {
		return false, "", fmt.Errorf("kube-proxy DaemonSet has no %s container; not changing it", kubeProxyContainer)
	}
	if current != image {
		// A strategic merge patch keyed on the container name sets the
		// image and touches nothing else of the DaemonSet Talos manages.
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []map[string]any{{"name": kubeProxyContainer, "image": image}},
		}}}})
		ds, err = w.cs.AppsV1().DaemonSets(kubeProxyNamespace).Patch(ctx, kubeProxyName, types.StrategicMergePatchType, patch,
			metav1.PatchOptions{FieldManager: FieldManager})
		if err != nil {
			return false, "", fmt.Errorf("patch kube-proxy image to %s: %w", image, err)
		}
	}
	done, msg := rolledOut(ds)
	return done, msg, nil
}

// rolledOut reads a DaemonSet's rollout the way kubectl rollout status does.
func rolledOut(ds *appsv1.DaemonSet) (bool, string) {
	st := ds.Status
	switch {
	case st.ObservedGeneration < ds.Generation:
		return false, "kube-proxy: waiting for the DaemonSet controller to see the new image"
	case st.UpdatedNumberScheduled < st.DesiredNumberScheduled:
		return false, fmt.Sprintf("kube-proxy: %d of %d pods updated", st.UpdatedNumberScheduled, st.DesiredNumberScheduled)
	case st.NumberAvailable < st.DesiredNumberScheduled:
		return false, fmt.Sprintf("kube-proxy: %d of %d updated pods available", st.NumberAvailable, st.DesiredNumberScheduled)
	}
	return true, fmt.Sprintf("kube-proxy: %d pods run the new image", st.DesiredNumberScheduled)
}

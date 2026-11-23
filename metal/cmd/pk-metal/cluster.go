package main

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// newScheme knows every kind the CLI reads or writes.
func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, metalv1alpha1.AddToScheme, osv1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			panic(err) // static registration; fails only on a programming error
		}
	}
	return s
}

// newClient connects to the cluster that --kubeconfig/--context name, or the
// default the kubectl rules pick ($KUBECONFIG, ~/.kube/config, in-cluster).
func newClient() (client.Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = global.kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: global.context}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot load a kubeconfig: %w", err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return nil, fmt.Errorf("cannot connect to the cluster at %s: %w", cfg.Host, err)
	}
	return cl, nil
}

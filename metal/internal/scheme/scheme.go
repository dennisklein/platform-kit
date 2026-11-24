// Package scheme is the one runtime scheme of the operator: the client-go
// kinds plus both API groups. The manager, the CLI and every test that talks
// to an API server use it, so they all know the same kinds. It lives here and
// not under api/, because the API packages stay a leaf that imports nothing of
// the operator.
package scheme

import (
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

var builder = runtime.NewSchemeBuilder(
	clientgoscheme.AddToScheme,
	metalv1alpha1.AddToScheme,
	osv1alpha1.AddToScheme,
)

// AddToScheme registers every kind the operator reads or writes.
var AddToScheme = builder.AddToScheme

// New returns a fresh scheme with every kind the operator reads or writes.
// Registration is static, so an error is a programming error.
func New() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

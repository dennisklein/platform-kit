// Package all builds the kind registry with every compiled-in kind.
package all

import (
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/article"
	"github.com/dennisklein/platform-kit/internal/kinds/cableplan"
	"github.com/dennisklein/platform-kit/internal/kinds/cluster"
	"github.com/dennisklein/platform-kit/internal/kinds/emulation"
	"github.com/dennisklein/platform-kit/internal/kinds/jbod"
	"github.com/dennisklein/platform-kit/internal/kinds/labkind"
	"github.com/dennisklein/platform-kit/internal/kinds/node"
	"github.com/dennisklein/platform-kit/internal/kinds/overlays"
	"github.com/dennisklein/platform-kit/internal/kinds/policykind"
	"github.com/dennisklein/platform-kit/internal/kinds/rack"
	"github.com/dennisklein/platform-kit/internal/kinds/switches"
	"github.com/dennisklein/platform-kit/internal/kinds/work"
)

// Own returns the own kinds (Go types).
func Own() []kinds.Kind {
	return []kinds.Kind{
		node.Kind(),
		rack.Kind(),
		switches.Kind(),
		jbod.Kind(),
		cableplan.Kind(),
		cluster.Kind(),
		article.Kind(),
		work.ItemKind(),
		work.MilestoneKind(),
		policykind.Kind(),
		emulation.Kind(),
		labkind.Kind(),
	}
}

// Overlays returns the third-party kinds (CRD schema subsets with UI hints):
// HelmRelease, Kustomization, CiliumBGPPeeringPolicy, Deployment, Service,
// Namespace.
func Overlays() []kinds.Kind {
	return overlays.All()
}

// Registry returns the registry of all kinds.
func Registry() *kinds.MapRegistry {
	return kinds.NewRegistry(append(Own(), Overlays()...)...)
}

package labkind

import (
	"fmt"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

func validate(r kinds.Repo) []kinds.Problem {
	var out []kinds.Problem
	for _, d := range r.ByKind(Name) {
		if d.Group() != Group {
			continue
		}
		if t, ok := kindutil.Get(d.Data, "spec", "topology").(string); ok && t != "" && r.Find("Cluster", t) == nil {
			out = append(out, kindutil.RefProblem(d, "spec.topology", kinds.SevError, "unknown cluster %s", t))
		}
		for i, m := range kindutil.Strings(d.Data, "spec", "scale", "machines") {
			if r.Find("Node", m) == nil {
				out = append(out, kindutil.RefProblem(d, fmt.Sprintf("spec.scale.machines[%d]", i), kinds.SevError, "unknown node %s", m))
			}
		}
	}
	return out
}

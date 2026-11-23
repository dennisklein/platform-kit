package cluster

import (
	"io/fs"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

func validate(r kinds.Repo) []kinds.Problem {
	var out []kinds.Problem
	for _, d := range r.ByKind(Name) {
		if d.Group() != Group {
			continue
		}
		if t := kindutil.Str(d.Data, "spec", "talos"); t != "" && r.FS() != nil {
			if _, err := fs.Stat(r.FS(), t); err != nil {
				out = append(out, kindutil.RefProblem(d, "spec.talos", kinds.SevWarning, "talconfig %s does not exist", t))
			}
		}
		if _, err := fs.Stat(r.FS(), "clusters/"+d.Name+"/flux.yaml"); err != nil && r.FS() != nil {
			out = append(out, kindutil.Problem(d, "metadata.name", kinds.SevWarning, "no-flux", "clusters/%s/flux.yaml is missing: nothing renders for %s", d.Name, d.Name))
		}
	}
	return out
}

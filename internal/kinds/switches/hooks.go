package switches

import (
	"fmt"
	"strings"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
	"github.com/dennisklein/platform-kit/internal/kinds/node"
)

func validate(r kinds.Repo) []kinds.Problem {
	inv := node.LoadInventory(r)
	var out []kinds.Problem
	for _, d := range r.ByKind(Name) {
		if d.Group() != Group {
			continue
		}
		rack := kindutil.Str(d.Data, "spec", "rack")
		if rack != "" {
			if _, ok := inv.Racks[strings.ToUpper(rack)]; !ok {
				out = append(out, kindutil.RefProblem(d, "spec.rack", kinds.SevError, "unknown rack %s", rack))
			}
		}
		sw := inv.Switches[d.Name]
		for i, u := range sw.Uplinks {
			if u < 1 || u > sw.Ports {
				out = append(out, kindutil.Problem(d, fmt.Sprintf("spec.uplinks[%d]", i), kinds.SevError, "port-range", "uplink port %d is not in 1 to %d", u, sw.Ports))
			}
		}
		for _, o := range inv.InRack(rack) {
			if o.Kind != Name || o.Name != d.Name {
				continue
			}
			if over := inv.Overlaps(o); len(over) > 0 {
				out = append(out, kindutil.Problem(d, "spec.unit", kinds.SevError, "unit-occupied", "unit %d in %s is used by %s", max(o.Unit, over[0].Unit), rack, over[0].Name))
			}
		}
	}
	return out
}

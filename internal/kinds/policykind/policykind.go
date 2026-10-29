// Package policykind registers the policy file (.pk/policy.yaml) as kind Policy.
package policykind

import (
	"encoding/json"
	"fmt"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
	"github.com/dennisklein/platform-kit/internal/policy"
)

// Kind returns the Policy kind.
func Kind() kinds.Kind {
	return kinds.Kind{
		Name:        "Policy",
		Group:       "pk",
		Version:     "v1alpha1",
		Versions:    []string{"v1alpha1"},
		Own:         true,
		Description: "People, agents, bots, path rules and risk rules. Only owners approve changes to it.",
		Paths:       []string{policy.Path},
		NewPath:     func(string) string { return policy.Path },
		Truth:       kinds.Intent,
		ApplyPath:   kinds.ApplySync,
		Views:       []string{"policy"},
		Schema: func(v string) (json.RawMessage, error) {
			if v != "" && v != "v1alpha1" {
				return nil, fmt.Errorf("Policy: unknown version %q", v)
			}
			return policy.Schema()
		},
		Validate: func(r kinds.Repo) []kinds.Problem {
			var out []kinds.Problem
			for _, d := range r.ByKind("Policy") {
				if d.Path != policy.Path || d.Node == nil {
					continue
				}
				out = append(out, policy.CheckDoc(d.Node)...)
			}
			return out
		},
		Describe: kindutil.Describer("Policy", "policy"),
	}
}

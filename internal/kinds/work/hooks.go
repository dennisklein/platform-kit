package work

import (
	"fmt"
	"regexp"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

var changeID = regexp.MustCompile(`^c-[a-z0-9]{6}$`)

func validate(r kinds.Repo) []kinds.Problem {
	milestones := map[string]bool{}
	for _, d := range r.ByKind(MilestoneName) {
		var ml MilestoneList
		if d.Decode(&ml) == nil {
			for _, m := range ml.Milestones {
				milestones[m.ID] = true
			}
		}
	}
	people := map[string]bool{}
	for _, d := range r.ByKind("Policy") {
		if l, ok := d.Data["people"].([]any); ok {
			for _, p := range l {
				if m, ok := p.(map[string]any); ok {
					people[kindutil.Str(m, "id")] = true
				}
			}
		}
	}
	var out []kinds.Problem
	for _, d := range r.ByKind(ItemName) {
		var w WorkItem
		if d.Decode(&w) != nil {
			continue
		}
		if w.Milestone != "" && !milestones[w.Milestone] {
			out = append(out, kindutil.RefProblem(d, "milestone", kinds.SevError, "unknown milestone %s", w.Milestone))
		}
		if w.Owner != "" && len(people) > 0 && !people[w.Owner] {
			out = append(out, kindutil.RefProblem(d, "owner", kinds.SevError, "%s is not a person in the policy", w.Owner))
		}
		for i, a := range w.Assets {
			if kindutil.FindEntity(r, a) == nil {
				out = append(out, kindutil.RefProblem(d, fmt.Sprintf("assets[%d]", i), kinds.SevInfo, "%s does not exist yet", a))
			}
		}
		for i, k := range w.KB {
			if r.Find("Article", k) == nil {
				out = append(out, kindutil.RefProblem(d, fmt.Sprintf("kb[%d]", i), kinds.SevError, "unknown article %s", k))
			}
		}
		for i, c := range w.Changes {
			if !changeID.MatchString(c) {
				out = append(out, kindutil.RefProblem(d, fmt.Sprintf("changes[%d]", i), kinds.SevError, "%s is not a change id", c))
			}
		}
	}
	return out
}

func describeItem(old, new *kinds.Doc) []kinds.SemanticLine {
	if old == nil || new == nil {
		return kindutil.Describer(ItemName, "work item")(old, new)
	}
	var a, b WorkItem
	if old.Decode(&a) != nil || new.Decode(&b) != nil {
		return nil
	}
	var out []kinds.SemanticLine
	had := map[string]bool{}
	for _, c := range a.Changes {
		had[c] = true
	}
	for _, c := range b.Changes {
		if !had[c] {
			line, _ := kindutil.Pos(new, "changes")
			out = append(out, kinds.SemanticLine{Subject: b.ID, Text: "linked to " + c, Op: kinds.OpChange, Path: new.Path, Line: line, Kind: ItemName})
		}
	}
	oldData := copyWithout(old.Data, "changes")
	newData := copyWithout(new.Data, "changes")
	for _, t := range kindutil.FieldLines(oldData, newData, 3) {
		out = append(out, kinds.SemanticLine{Subject: b.ID, Text: t, Op: kinds.OpChange, Path: new.Path, Line: new.Line, Kind: ItemName})
	}
	return out
}

func copyWithout(m map[string]any, key string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

// ItemSkeleton is a new work item.
func ItemSkeleton(name string) ([]byte, error) {
	return []byte(fmt.Sprintf("apiVersion: %s/v1alpha1\nkind: WorkItem\nid: %s\ntitle: \"\"\nmilestone: \"\"\nowner: \"\"\nassets: []\nchanges: []\nkb: []\n", Group, name)), nil
}

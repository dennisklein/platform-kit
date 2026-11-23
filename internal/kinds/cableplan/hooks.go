package cableplan

import (
	"fmt"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
	"github.com/dennisklein/platform-kit/internal/kinds/node"
)

func validate(r kinds.Repo) []kinds.Problem {
	inv := node.LoadInventory(r)
	var out []kinds.Problem
	for _, u := range inv.Ports {
		if u.OwnerKind != Name {
			continue
		}
		msg, sub := inv.PortProblem(u)
		if msg == "" {
			continue
		}
		field := u.Field
		switch sub {
		case "switch", "port":
			field += "." + sub
		case "cable":
			field = field[:len(field)-2] + ".id"
		}
		if sub == "switch" {
			out = append(out, kindutil.RefProblem(u.Doc, field, kinds.SevError, "%s", msg))
			continue
		}
		out = append(out, kindutil.Problem(u.Doc, field, kinds.SevError, "port-"+sub, "%s", msg))
	}
	return out
}

func describe(old, new *kinds.Doc) []kinds.SemanticLine {
	if old == nil || new == nil {
		return kindutil.Describer(Name, "cable plan")(old, new)
	}
	var a, b CablePlan
	if old.Decode(&a) != nil || new.Decode(&b) != nil {
		return nil
	}
	end := func(e End) string { return fmt.Sprintf("%s %d", e.Switch, e.Port) }
	prev := map[string]Uplink{}
	for _, u := range a.Spec.Uplinks {
		prev[u.ID] = u
	}
	var out []kinds.SemanticLine
	for i, u := range b.Spec.Uplinks {
		line, _ := kindutil.Pos(new, fmt.Sprintf("spec.uplinks[%d]", i))
		o, ok := prev[u.ID]
		delete(prev, u.ID)
		switch {
		case !ok:
			text := fmt.Sprintf("new uplink %s to %s", end(u.A), end(u.B))
			if u.Speed != "" {
				text += ", " + u.Speed
			}
			out = append(out, kinds.SemanticLine{Subject: u.ID, Text: text, Op: kinds.OpAdd, Path: new.Path, Line: line, Kind: Name})
		case o.A != u.A || o.B != u.B:
			out = append(out, kinds.SemanticLine{Subject: u.ID, Text: fmt.Sprintf("%s to %s, was %s to %s", end(u.A), end(u.B), end(o.A), end(o.B)), Op: kinds.OpChange, Path: new.Path, Line: line, Kind: Name})
		case o.Speed != u.Speed:
			out = append(out, kinds.SemanticLine{Subject: u.ID, Text: fmt.Sprintf("speed %s to %s", o.Speed, u.Speed), Op: kinds.OpChange, Path: new.Path, Line: line, Kind: Name})
		}
	}
	for id, u := range prev {
		out = append(out, kinds.SemanticLine{Subject: id, Text: fmt.Sprintf("uplink removed (was %s to %s)", end(u.A), end(u.B)), Op: kinds.OpDelete, Path: new.Path, Kind: Name})
	}
	return out
}

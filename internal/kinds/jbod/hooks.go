package jbod

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
		if a := kindutil.Str(d.Data, "spec", "attachedTo"); a != "" && r.Find("Node", a) == nil {
			out = append(out, kindutil.RefProblem(d, "spec.attachedTo", kinds.SevError, "unknown node %s", a))
		}
		rack := kindutil.Str(d.Data, "spec", "location", "rack")
		if rack != "" {
			if _, ok := inv.Racks[strings.ToUpper(rack)]; !ok {
				out = append(out, kindutil.RefProblem(d, "spec.location.rack", kinds.SevError, "unknown rack %s", rack))
			}
		}
		for _, o := range inv.InRack(rack) {
			if o.Kind != Name || o.Name != d.Name {
				continue
			}
			if over := inv.Overlaps(o); len(over) > 0 {
				out = append(out, kindutil.Problem(d, "spec.location.unit", kinds.SevError, "unit-occupied", "unit %d in %s is used by %s", max(o.Unit, over[0].Unit), rack, over[0].Name))
			}
		}
		serials := map[string]string{}
		var j Jbod
		if err := d.Decode(&j); err != nil {
			continue
		}
		for di, dr := range j.Spec.Drawers {
			for si, s := range dr.Slots {
				if s.Serial == "" {
					continue
				}
				where := fmt.Sprintf("drawer %d slot %d", dr.ID, s.Slot)
				if prev, ok := serials[s.Serial]; ok {
					out = append(out, kindutil.Problem(d, fmt.Sprintf("spec.drawers[%d].slots[%d].serial", di, si), kinds.SevError, "duplicate", "disk serial %s is also in %s", s.Serial, prev))
				}
				serials[s.Serial] = where
			}
		}
	}
	return out
}

func describe(old, new *kinds.Doc) []kinds.SemanticLine {
	if old == nil || new == nil {
		return kindutil.Describer(Name, "disk shelf")(old, new)
	}
	var a, b Jbod
	if old.Decode(&a) != nil || new.Decode(&b) != nil {
		return kindutil.Describer(Name, "disk shelf")(old, new)
	}
	type key struct{ drawer, slot int }
	prev := map[key]Slot{}
	for _, dr := range a.Spec.Drawers {
		for _, s := range dr.Slots {
			prev[key{dr.ID, s.Slot}] = s
		}
	}
	var out []kinds.SemanticLine
	for di, dr := range b.Spec.Drawers {
		for si, s := range dr.Slots {
			o, ok := prev[key{dr.ID, s.Slot}]
			subject := fmt.Sprintf("%s drawer %d slot %d", new.Name, dr.ID, s.Slot)
			line, _ := kindutil.Pos(new, fmt.Sprintf("spec.drawers[%d].slots[%d]", di, si))
			l := kinds.SemanticLine{Subject: subject, Op: kinds.OpChange, Path: new.Path, Line: line, Kind: Name}
			switch {
			case !ok:
				l.Op, l.Text = kinds.OpAdd, fmt.Sprintf("new slot, %s", s.State)
				out = append(out, l)
				continue
			case o.State != s.State:
				l.Text = fmt.Sprintf("%s to %s", o.State, s.State)
				if s.State == SlotReplace {
					l.Text += "; the OSD drains after the merge"
				}
				out = append(out, l)
			}
			if o.Serial != s.Serial {
				l.Text = fmt.Sprintf("new disk serial %s", s.Serial)
				if s.Serial == "" {
					l.Text = "disk serial removed"
				}
				out = append(out, l)
			}
		}
	}
	if len(out) == 0 {
		return kindutil.Describer(Name, "disk shelf")(old, new)
	}
	return out
}

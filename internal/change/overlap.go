package change

import (
	"context"
	"sort"

	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

// Overlap: two open changes whose impacts touch the same rendered object
// (cluster, apiVersion, kind, namespace, name). The objects come from the
// gate evaluation (Renderer.Impact of merge base vs head), so overlap costs
// no extra render. No lock: both changes list each other.

type gatesEntry struct {
	gates   *policy.Gates
	objects []render.ObjectRef
}

type objEntry struct {
	title, author string
	state         State
	keys          map[render.ObjectKey]render.ObjectRef
}

// setObjects remembers the changed objects of an open change.
func (s *Svc) setObjects(d *derived, objs []render.ObjectRef) {
	e := objEntry{title: d.ch.Title, author: d.ch.Author, keys: map[render.ObjectKey]render.ObjectRef{}}
	for _, o := range objs {
		e.keys[render.KeyOf(o)] = o
	}
	s.cmu.Lock()
	// Keep the last derived state until noteState sets the new one.
	if old, ok := s.objects[d.rec.id]; ok {
		e.state = old.state
	}
	s.objects[d.rec.id] = e
	s.cmu.Unlock()
}

// forgetObjects drops a change that is no longer open.
func (s *Svc) forgetObjects(id string) {
	s.cmu.Lock()
	delete(s.objects, id)
	s.cmu.Unlock()
}

// noteState records the derived state for the overlap details.
func (s *Svc) noteState(id string, st State) {
	s.cmu.Lock()
	if e, ok := s.objects[id]; ok {
		e.state = st
		s.objects[id] = e
	}
	s.cmu.Unlock()
}

// overlaps returns the open changes that touch objects of id, from the
// objects known so far.
func (s *Svc) overlaps(id string) []Overlap {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	me, ok := s.objects[id]
	if !ok || len(me.keys) == 0 {
		return nil
	}
	var out []Overlap
	for oid, e := range s.objects {
		if oid == id {
			continue
		}
		var objs []string
		for k := range e.keys {
			if _, ok := me.keys[k]; ok {
				objs = append(objs, string(k))
			}
		}
		if len(objs) == 0 {
			continue
		}
		sort.Strings(objs)
		out = append(out, Overlap{Change: oid, Title: e.title, By: e.author, State: e.state, Objects: objs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Change < out[j].Change })
	return out
}

// OverlapIDs returns the ids of the overlapping changes.
func overlapIDs(ov []Overlap) []string {
	var ids []string
	for _, o := range ov {
		ids = append(ids, o.Change)
	}
	return ids
}

// ObjectRefOf parses an object key cluster/apiVersion/kind/namespace/name
// (apiVersion may contain a slash).
func ObjectRefOf(key string) render.ObjectRef {
	parts := splitKey(key)
	var r render.ObjectRef
	switch len(parts) {
	case 5:
		r.Cluster, r.APIVersion, r.Kind, r.Namespace, r.Name = parts[0], parts[1], parts[2], parts[3], parts[4]
	case 6:
		r.Cluster, r.APIVersion, r.Kind, r.Namespace, r.Name = parts[0], parts[1]+"/"+parts[2], parts[3], parts[4], parts[5]
	default:
		r.Name = key
	}
	return r
}

func splitKey(k string) []string {
	var out []string
	start := 0
	for i := 0; i < len(k); i++ {
		if k[i] == '/' {
			out = append(out, k[start:i])
			start = i + 1
		}
	}
	return append(out, k[start:])
}

// warmObjects derives open changes that have no objects yet, so that
// overlap of a single change sees all others.
func (s *Svc) warmObjects(ctx context.Context, recs map[string]*record, me string) {
	for id, r := range recs {
		if r.merge != nil || r.headRev() == "" {
			continue
		}
		s.cmu.Lock()
		_, ok := s.objects[id]
		s.cmu.Unlock()
		if ok {
			continue
		}
		if d, err := s.derive(ctx, r, me, false); err == nil && !d.ch.State.Open() {
			s.forgetObjects(id)
		}
	}
}

// freshOverlaps derives the overlapping changes again, so each entry has
// the state of now (an entry may be from before the forge state loaded,
// just after a restart). Changes that are no longer open drop out.
func (s *Svc) freshOverlaps(ctx context.Context, recs map[string]*record, me string, ov []Overlap) []Overlap {
	out := ov[:0:0]
	for _, o := range ov {
		r, ok := recs[o.Change]
		if !ok {
			continue
		}
		d, err := s.derive(ctx, r, me, false)
		if err != nil {
			out = append(out, o)
			continue
		}
		if !d.ch.State.Open() {
			continue
		}
		o.State, o.Title, o.By = d.ch.State, d.ch.Title, d.ch.Author
		out = append(out, o)
	}
	return out
}

package fluxsim

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/labkind"
	"github.com/dennisklein/platform-kit/internal/kinds/node"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
	"github.com/dennisklein/platform-kit/internal/lab"
	"github.com/dennisklein/platform-kit/internal/render"
)

// Heads are the refs fluxsim follows.
type Heads struct {
	Main string
	// Rings maps ring name to the commit of tag ring/<name>.
	Rings map[string]string
}

// Source is the state repo as fluxsim sees it.
type Source interface {
	// Fetch updates the clone and returns the heads.
	Fetch(ctx context.Context) (Heads, error)
	// Tree returns a directory with the tree of a commit.
	Tree(ctx context.Context, sha string) (string, error)
	// Change returns the Pk-Change trailer of a commit ("" when none).
	Change(ctx context.Context, sha string) string
}

// Engine is the simulated Flux: per cluster it follows a ref, applies the
// render after ApplyDelay and verifies after VerifyDelay. A Machine is
// Ready unless the demo sets the cluster failing.
type Engine struct {
	src         Source
	reg         kinds.Registry
	rd          render.Renderer
	applyDelay  time.Duration
	verifyDelay time.Duration
	statePath   string
	log         *slog.Logger
	now         func() time.Time

	mu       sync.Mutex
	heads    Heads
	fetchErr string
	fetched  time.Time
	clusters map[string]*cstate
}

type cstate struct {
	ID       string    `json:"id"`
	Role     string    `json:"role"`
	Ring     string    `json:"ring"`
	Path     string    `json:"path"`
	Twin     string    `json:"twin,omitempty"`
	Follow   string    `json:"follow"`
	Target   string    `json:"target"`
	Applied  *Revision `json:"applied,omitempty"`
	Verified *Revision `json:"verified,omitempty"`
	Failing  bool      `json:"failing"`
	Healthy  bool      `json:"healthy"`
	Message  string    `json:"message"`
	// FailedSHA is a revision whose apply failed (not retried until the target moves).
	FailedSHA   string    `json:"failedSha,omitempty"`
	LastApplied time.Time `json:"lastApplied,omitzero"`

	pending      *Revision
	pendingSince time.Time
	verifyDue    time.Time
	objects      []Object
	kusts        []KustomizationStatus
}

// EngineOptions configure an Engine.
type EngineOptions struct {
	Source      Source
	Registry    kinds.Registry
	Renderer    render.Renderer
	ApplyDelay  time.Duration
	VerifyDelay time.Duration
	// StatePath keeps applied and verified revisions across restarts.
	StatePath string
	Log       *slog.Logger
	Now       func() time.Time
}

// NewEngine returns an engine and loads its saved state.
func NewEngine(o EngineOptions) *Engine {
	e := &Engine{src: o.Source, reg: o.Registry, rd: o.Renderer, applyDelay: o.ApplyDelay, verifyDelay: o.VerifyDelay,
		statePath: o.StatePath, log: o.Log, now: o.Now, clusters: map[string]*cstate{}}
	if e.log == nil {
		e.log = slog.Default()
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.rd == nil {
		e.rd = render.New(o.Registry)
	}
	e.load()
	return e
}

func (e *Engine) load() {
	if e.statePath == "" {
		return
	}
	b, err := os.ReadFile(e.statePath)
	if err != nil {
		return
	}
	var cs []*cstate
	if err := json.Unmarshal(b, &cs); err != nil {
		return
	}
	for _, c := range cs {
		e.clusters[c.ID] = c
	}
}

func (e *Engine) saveLocked() {
	if e.statePath == "" {
		return
	}
	cs := make([]*cstate, 0, len(e.clusters))
	for _, id := range e.idsLocked() {
		cs = append(cs, e.clusters[id])
	}
	b, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(e.statePath), 0o755)
	tmp := e.statePath + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, e.statePath)
	}
}

func (e *Engine) idsLocked() []string {
	ids := make([]string, 0, len(e.clusters))
	for id := range e.clusters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Sync fetches the state repo and sets the target revision of each cluster:
// the ring tag ring/<ring> if it exists, else main. Twins follow their ref.
func (e *Engine) Sync(ctx context.Context) error {
	heads, err := e.src.Fetch(ctx)
	if err != nil {
		e.mu.Lock()
		e.fetchErr = err.Error()
		e.mu.Unlock()
		return err
	}
	dir, err := e.src.Tree(ctx, heads.Main)
	if err != nil {
		return err
	}
	r, err := repo.Load(dir, e.reg)
	if err != nil {
		return err
	}
	clusters := render.ClustersOf(r)
	type twin struct {
		name, cluster, follow string
	}
	var twins []twin
	for _, d := range r.ByKind(labkind.Name) {
		var l labkind.Lab
		if d.Decode(&l) != nil || l.Spec.Purpose != "twin" || l.Spec.Topology.Name == "" {
			continue
		}
		f := "main"
		if l.Spec.Follow != nil && l.Spec.Follow.Ref != "" {
			f = l.Spec.Follow.Ref
		}
		twins = append(twins, twin{name: d.Name, cluster: l.Spec.Topology.Name, follow: f})
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.heads, e.fetchErr, e.fetched = heads, "", e.now()
	seen := map[string]bool{}
	roles := map[string]render.Cluster{}
	for _, c := range clusters {
		roles[c.ID] = c
		seen[c.ID] = true
		st := e.state(c.ID)
		st.Role, st.Ring, st.Path, st.Twin = c.Role, c.Ring, c.Path, ""
		st.Follow, st.Target = "main", heads.Main
		if sha, ok := heads.Rings[c.Ring]; ok && c.Ring != "" {
			st.Follow, st.Target = "ring/"+c.Ring, sha
		}
	}
	for _, t := range twins {
		seen[t.name] = true
		st := e.state(t.name)
		st.Twin, st.Ring, st.Role = t.cluster, "lab", roles[t.cluster].Role
		st.Path = ".pk/labs/" + t.name + ".yaml"
		st.Follow, st.Target = t.follow, heads.Main
		if ring, ok := strings.CutPrefix(t.follow, "ring/"); ok {
			if sha, ok := heads.Rings[ring]; ok {
				st.Target = sha
			}
		}
	}
	for id := range e.clusters {
		if !seen[id] {
			delete(e.clusters, id)
		}
	}
	e.saveLocked()
	return nil
}

func (e *Engine) state(id string) *cstate {
	st := e.clusters[id]
	if st == nil {
		st = &cstate{ID: id}
		e.clusters[id] = st
	}
	return st
}

// work is a render to do outside the lock.
type work struct {
	id, sha, cluster, follow string
	restore                  bool
}

// Step advances the state machines: pending → applied (after ApplyDelay)
// → verified (after VerifyDelay). It renders outside the lock.
func (e *Engine) Step(ctx context.Context) {
	now := e.now()
	var todo []work
	var pend []work
	e.mu.Lock()
	for _, id := range e.idsLocked() {
		c := e.clusters[id]
		cluster := c.ID
		if c.Twin != "" {
			cluster = c.Twin
		}
		if c.Applied != nil && c.objects == nil && c.Applied.SHA != "" {
			// Restored from the saved state: rebuild the object store.
			todo = append(todo, work{id: id, sha: c.Applied.SHA, cluster: cluster, follow: c.Follow, restore: true})
		}
		if c.Target != "" && (c.Applied == nil || c.Target != c.Applied.SHA) && c.Target != c.FailedSHA {
			switch {
			case c.pending == nil || c.pending.SHA != c.Target:
				c.pending = &Revision{SHA: c.Target, At: now}
				c.pendingSince = now
				c.Message = "Reconciling " + revName(c.Follow, c.Target)
				pend = append(pend, work{id: id, sha: c.Target})
			case now.Sub(c.pendingSince) >= e.applyDelay:
				todo = append(todo, work{id: id, sha: c.Target, cluster: cluster, follow: c.Follow})
			}
		}
		if c.Applied != nil && !c.verifyDue.IsZero() && !now.Before(c.verifyDue) {
			e.verifyLocked(c, now)
		}
	}
	e.mu.Unlock()

	for _, w := range pend {
		ch := e.src.Change(ctx, w.sha)
		e.mu.Lock()
		if c := e.clusters[w.id]; c != nil && c.pending != nil && c.pending.SHA == w.sha {
			c.pending.Change = ch
		}
		e.mu.Unlock()
	}
	for _, w := range todo {
		e.apply(ctx, w)
	}
}

// apply renders a revision of a cluster and applies it to the object store.
func (e *Engine) apply(ctx context.Context, w work) {
	var objs []Object
	var kusts []KustomizationStatus
	failure := ""
	dir, err := e.src.Tree(ctx, w.sha)
	if err != nil {
		failure = "source: " + err.Error()
	} else if r, err := repo.Load(dir, e.reg); err != nil {
		failure = "source: " + err.Error()
	} else {
		m, err := lab.ModelOf(ctx, e.rd, r, render.Source{Ref: w.sha, SHA: w.sha, Dir: dir, Key: w.sha}, w.cluster)
		if err != nil {
			failure = err.Error()
		} else {
			if c := m.FluxReady(); !c.OK {
				failure = c.Detail
			}
			objs, kusts = objectsOf(m.Set, w.follow, w.sha)
		}
	}
	change := ""
	if failure == "" && !w.restore {
		change = e.src.Change(ctx, w.sha)
	}
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.clusters[w.id]
	if c == nil {
		return
	}
	if w.restore {
		if c.Applied != nil && c.Applied.SHA == w.sha && c.objects == nil {
			ready := c.Verified != nil && c.Verified.SHA == w.sha && !c.Failing
			for i := range objs {
				objs[i].Ready = ready
			}
			for i := range kusts {
				kusts[i].Ready = ready
				if ready {
					kusts[i].Message = "Applied revision: " + kusts[i].Revision
				}
			}
			c.objects, c.kusts = nonNil(objs), kusts
			if c.Verified == nil || c.Verified.SHA != c.Applied.SHA {
				c.verifyDue = now.Add(e.verifyDelay)
			}
		}
		return
	}
	if c.Target != w.sha {
		return // the target moved while rendering
	}
	c.pending = nil
	if failure != "" {
		c.FailedSHA = w.sha
		c.Healthy = false
		c.Message = "Kustomization failed at " + revName(c.Follow, w.sha) + ": " + failure
		for i := range c.kusts {
			c.kusts[i].Ready = false
			c.kusts[i].Message = failure
		}
		e.log.Info("flux apply failed", "cluster", w.id, "sha", short(w.sha), "err", failure)
		e.saveLocked()
		return
	}
	prev := map[string]bool{}
	for _, o := range c.objects {
		prev[objKey(o)] = true
	}
	for i := range objs {
		objs[i].Ready = false
	}
	c.objects, c.kusts = nonNil(objs), kusts
	c.FailedSHA = ""
	c.Applied = &Revision{SHA: w.sha, At: now, Change: change}
	c.LastApplied = now
	c.verifyDue = now.Add(e.verifyDelay)
	c.Message = "Applied revision " + revName(c.Follow, w.sha) + ", health checks running"
	newMachines := 0
	for _, o := range objs {
		if o.Kind == node.MachineKind && !prev[objKey(o)] {
			newMachines++
		}
	}
	e.log.Info("flux applied", "cluster", w.id, "sha", short(w.sha), "change", change, "objects", len(objs), "newMachines", newMachines)
	e.saveLocked()
}

func nonNil(o []Object) []Object {
	if o == nil {
		return []Object{}
	}
	return o
}

func objKey(o Object) string {
	return o.APIVersion + "/" + o.Kind + "/" + o.Namespace + "/" + o.Name
}

// verifyLocked runs the health checks of the applied revision.
func (e *Engine) verifyLocked(c *cstate, now time.Time) {
	c.verifyDue = time.Time{}
	if c.Failing {
		c.Healthy = false
		bad := e.failObjectsLocked(c)
		c.Message = fmt.Sprintf("Health check failed after %s: %s not Ready (demo fault)", revName(c.Follow, c.Applied.SHA), bad)
		e.saveLocked()
		return
	}
	for i := range c.objects {
		c.objects[i].Ready = true
	}
	for i := range c.kusts {
		c.kusts[i].Ready = true
		c.kusts[i].Message = "Applied revision: " + c.kusts[i].Revision
	}
	v := *c.Applied
	v.At = now
	c.Verified = &v
	c.Healthy = true
	c.Message = "Applied revision " + revName(c.Follow, c.Applied.SHA) + ", health checks pass"
	e.log.Info("flux verified", "cluster", c.ID, "sha", short(c.Applied.SHA))
	e.saveLocked()
}

// failObjectsLocked marks the Machines of the last apply not Ready (the
// newest ones, else the first) and returns their names.
func (e *Engine) failObjectsLocked(c *cstate) string {
	var machines []int
	for i, o := range c.objects {
		if o.Kind == node.MachineKind {
			machines = append(machines, i)
		}
	}
	if len(machines) == 0 {
		for i := range c.kusts {
			c.kusts[i].Ready = false
			c.kusts[i].Message = "health check failed (demo fault)"
		}
		if len(c.kusts) > 0 {
			return "Kustomization/" + c.kusts[len(c.kusts)-1].Name
		}
		return "the cluster"
	}
	// The highest machine name is the newest node (cn-036).
	last := machines[0]
	for _, i := range machines {
		if c.objects[i].Name > c.objects[last].Name {
			last = i
		}
	}
	c.objects[last].Ready = false
	for i := range c.kusts {
		if c.kusts[i].Name == c.objects[last].Kustomization {
			c.kusts[i].Ready = false
			c.kusts[i].Message = "health check failed: Machine/" + c.objects[last].Name + " not Ready"
		}
	}
	return "Machine/" + c.objects[last].Name
}

// SetFailing injects (true) or heals (false) the demo fault of a cluster.
func (e *Engine) SetFailing(id string, failing bool) (*ClusterStatus, error) {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.clusters[id]
	if c == nil {
		return nil, fmt.Errorf("no cluster %q", id)
	}
	c.Failing = failing
	if c.Applied != nil {
		if failing {
			if c.verifyDue.IsZero() {
				c.Healthy = false
				bad := e.failObjectsLocked(c)
				c.Message = bad + " not Ready (demo fault)"
			}
		} else {
			// Heal: run the health checks again.
			c.verifyDue = now.Add(e.verifyDelay)
			c.Message = "Health checks running after the fault was healed"
		}
	}
	e.saveLocked()
	st := e.statusLocked(c)
	return &st, nil
}

func (e *Engine) statusLocked(c *cstate) ClusterStatus {
	st := ClusterStatus{ID: c.ID, Role: c.Role, Ring: c.Ring, Ref: c.Follow, Healthy: c.Healthy, Failing: c.Failing,
		Objects: len(c.objects), Kustomizations: append([]KustomizationStatus{}, c.kusts...), Message: c.Message,
		Twin: c.Twin, LastApplied: c.LastApplied}
	if c.Applied != nil {
		a := *c.Applied
		st.Applied = &a
	}
	if c.Verified != nil {
		v := *c.Verified
		st.Verified = &v
	}
	if c.pending != nil {
		p := *c.pending
		st.Pending = &p
	}
	return st
}

// Status returns the state of all clusters and twins.
func (e *Engine) Status() *Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := &Status{Main: e.heads.Main, Clusters: []ClusterStatus{}, Twins: []ClusterStatus{}, Error: e.fetchErr, At: e.now().UTC()}
	if len(e.heads.Rings) > 0 {
		out.Rings = map[string]string{}
		for k, v := range e.heads.Rings {
			out.Rings[k] = v
		}
	}
	for _, id := range e.idsLocked() {
		c := e.clusters[id]
		if c.Twin != "" {
			out.Twins = append(out.Twins, e.statusLocked(c))
		} else {
			out.Clusters = append(out.Clusters, e.statusLocked(c))
		}
	}
	return out
}

// Objects returns the applied objects of a cluster.
func (e *Engine) Objects(id string) ([]Object, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.clusters[id]
	if c == nil {
		return nil, fmt.Errorf("no cluster %q", id)
	}
	return append([]Object{}, c.objects...), nil
}

// objectsOf lists the objects and Kustomizations of a render.
func objectsOf(set *render.ObjectSet, follow, sha string) ([]Object, []KustomizationStatus) {
	if follow == "" {
		follow = "main"
	}
	var objs []Object
	for _, k := range set.Keys() {
		o := set.Objects[k]
		objs = append(objs, Object{APIVersion: o.APIVersion, Kind: o.Kind, Namespace: o.Namespace, Name: o.Name, Kustomization: o.Kustomization})
	}
	kusts := []KustomizationStatus{}
	for _, name := range set.Order {
		kusts = append(kusts, KustomizationStatus{Name: name, Path: set.Kustomizations[name], Revision: follow + "@sha1:" + sha})
	}
	return objs, kusts
}

func revName(follow, sha string) string {
	if follow == "" {
		follow = "main"
	}
	return follow + "@sha1:" + short(sha)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

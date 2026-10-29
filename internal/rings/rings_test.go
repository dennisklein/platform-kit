package rings

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/audit"
	"github.com/dennisklein/platform-kit/internal/fluxsim"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/policy"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@pk.local",
		"-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// world is a forge (a bare repo), a work clone that merges to main and a
// fake fluxsim whose clusters the test moves.
type world struct {
	t      *testing.T
	origin string
	work   string
	mu     sync.Mutex
	now    time.Time
	cs     []fluxsim.ClusterStatus
	audits []audit.Entry
}

func (w *world) Append(ctx context.Context, e audit.Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.audits = append(w.audits, e)
	return nil
}

func (w *world) List(ctx context.Context, limit int) ([]audit.Entry, error) { return nil, nil }

func (w *world) auditLines() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, e := range w.audits {
		out = append(out, e.Actor+" "+e.Action+" "+e.Target+" "+e.Detail)
	}
	return out
}

func newWorld(t *testing.T) *world {
	root := t.TempDir()
	w := &world{t: t, origin: filepath.Join(root, "origin.git"), work: filepath.Join(root, "work"),
		now: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)}
	git(t, root, "init", "-q", "--bare", w.origin)
	git(t, root, "clone", "-q", w.origin, w.work)
	git(t, w.work, "checkout", "-q", "-b", "main")
	pol, err := os.ReadFile("../../seed/state/.pk/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w.commit("c-base00", map[string]string{".pk/policy.yaml": string(pol)})
	w.cs = []fluxsim.ClusterStatus{
		{ID: "defiant", Ring: "canary", Role: "tenants"},
		{ID: "pegasus", Ring: "prod", Role: "storage"},
		{ID: "phoenix", Ring: "prod", Role: "compute"},
	}
	return w
}

func (w *world) commit(change string, files map[string]string) string {
	for p, c := range files {
		full := filepath.Join(w.work, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			w.t.Fatal(err)
		}
	}
	git(w.t, w.work, "add", "-A")
	git(w.t, w.work, "commit", "-q", "-m", "Merge change "+change+"\n\nPk-Change: "+change)
	git(w.t, w.work, "push", "-q", "origin", "main")
	return git(w.t, w.work, "rev-parse", "HEAD")
}

func (w *world) tag(ring string) string {
	out, err := exec.Command("git", "-C", w.origin, "rev-parse", "-q", "--verify", "refs/tags/ring/"+ring).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// verify makes the clusters of a ring healthy and verified at sha.
func (w *world) verify(ring, sha string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.cs {
		if w.cs[i].Ring == ring {
			rev := &fluxsim.Revision{SHA: sha, At: w.now}
			w.cs[i].Applied, w.cs[i].Verified, w.cs[i].Healthy = rev, rev, true
		}
	}
}

func (w *world) advance(d time.Duration) {
	w.mu.Lock()
	w.now = w.now.Add(d)
	w.mu.Unlock()
}

func (w *world) clock() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

func (w *world) Status(ctx context.Context) (*fluxsim.Status, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := &fluxsim.Status{Main: strings.TrimSpace(git(w.t, w.origin, "rev-parse", "refs/heads/main")), Rings: map[string]string{}}
	for _, r := range []string{"canary", "prod"} {
		if sha := w.tag(r); sha != "" {
			st.Rings[r] = sha
		}
	}
	for _, c := range w.cs {
		c.Ref = "main"
		if _, ok := st.Rings[c.Ring]; ok {
			c.Ref = "ring/" + c.Ring
		}
		st.Clusters = append(st.Clusters, c)
	}
	return st, nil
}

func (w *world) Objects(ctx context.Context, cluster string) ([]fluxsim.Object, error) {
	return nil, nil
}
func (w *world) SetFailing(ctx context.Context, cluster string, failing bool) (*fluxsim.ClusterStatus, error) {
	return nil, nil
}

func (w *world) service(dir string) *Svc {
	pol := policy.NewEngine(func(ctx context.Context) (string, error) { return w.work, nil })
	s, err := New(Options{Dir: dir, Git: &GitRepo{Dir: filepath.Join(dir, "git"), URL: w.origin, MinFetch: -1},
		Flux: w, Policy: pol, Soak: time.Minute, Now: w.clock, Audit: w})
	if err != nil {
		w.t.Fatal(err)
	}
	return s
}

func ringOf(t *testing.T, s *Svc, name string) Ring {
	t.Helper()
	rs, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := find(rs, name)
	if i < 0 {
		t.Fatalf("no ring %s in %+v", name, rs)
	}
	return rs[i]
}

var (
	dennis = identity.Identity{User: "dennis", Kind: identity.Human}
	mara   = identity.Identity{User: "mara", Kind: identity.Human}
	agent  = identity.Identity{User: "dennis-agent", Kind: identity.Agent, ActsFor: "dennis"}
)

func TestRings(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	dir := t.TempDir()
	s := w.service(dir)
	c1 := w.commit("c-app001", map[string]string{"clusters/defiant/apps/podinfo/release.yaml": "v: 1\n"})
	w.verify("canary", c1)
	w.verify("prod", c1)

	// Rings off: no tags, everything follows main.
	rs, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Ring != "canary" || rs[1].Ring != "prod" || rs[0].Following != "main" || rs[0].Gate != GateManual ||
		!strings.HasPrefix(rs[0].Reason, "rings off") || strings.Join(rs[1].Clusters, ",") != "pegasus,phoenix" || !rs[1].Verified {
		t.Fatalf("rings off = %+v", rs)
	}
	if _, err := s.Promote(ctx, agent, "canary", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent promote: %v", err)
	}
	for _, ring := range []string{"canary", "prod"} {
		r, err := s.Promote(ctx, dennis, ring, "")
		if err != nil {
			t.Fatal(err)
		}
		if r.Tag != "ring/"+ring || r.SHA != c1 || w.tag(ring) != c1 || r.Change != "c-app001" {
			t.Fatalf("promote %s = %+v", ring, r)
		}
	}

	// A low-risk merge: canary takes it after the soak, prod after canary verified plus the soak.
	c2 := w.commit("c-app002", map[string]string{"clusters/defiant/apps/podinfo/release.yaml": "v: 2\n"})
	can := ringOf(t, s, "canary")
	if can.Gate != GateAuto || can.Candidate != c2 || can.CandidateChange != "c-app002" || !can.SoakUntil.Equal(w.clock().Add(time.Minute)) {
		t.Fatalf("canary auto = %+v", can)
	}
	if got, err := s.Tick(ctx); err != nil || got != "" {
		t.Fatalf("tick in soak: %q %v", got, err)
	}
	w.advance(61 * time.Second)
	if got, err := s.Tick(ctx); err != nil || got != "canary" || w.tag("canary") != c2 {
		t.Fatalf("tick after soak: %q %v tag %s", got, err, w.tag("canary"))
	}
	if p := ringOf(t, s, "prod"); p.Gate != GateNone || !strings.Contains(p.Reason, "waiting for ring canary") {
		t.Fatalf("prod waits = %+v", p)
	}
	w.verify("canary", c2)
	if p := ringOf(t, s, "prod"); p.Gate != GateAuto || p.Candidate != c2 {
		t.Fatalf("prod auto = %+v", p)
	}
	w.advance(61 * time.Second)
	if got, err := s.Tick(ctx); err != nil || got != "prod" || w.tag("prod") != c2 {
		t.Fatalf("tick prod: %q %v", got, err)
	}
	w.verify("prod", c2)
	short2 := c2[:7]
	if a := w.auditLines(); len(a) != 2 ||
		a[0] != "pk-ci ring.promote ring/canary "+short2+" c-app002 (auto gate: low-risk paths only, soak 1m)" ||
		a[1] != "pk-ci ring.promote ring/prod "+short2+" c-app002 (auto gate: low-risk paths only, soak 1m)" {
		t.Fatalf("audit = %q", a)
	}

	// A high-risk merge waits for a click by an approver of the path.
	c3 := w.commit("c-inv003", map[string]string{"control/inventory/cn-035.yaml": "name: cn-035\n"})
	can = ringOf(t, s, "canary")
	if can.Gate != GateManual || can.Reason != "high-risk paths: control/inventory/cn-035.yaml (inventory)" {
		t.Fatalf("canary manual = %+v", can)
	}
	w.advance(10 * time.Minute)
	if got, _ := s.Tick(ctx); got != "" {
		t.Fatalf("manual gate auto-promoted %s", got)
	}
	if _, err := s.Promote(ctx, mara, "canary", ""); !errors.Is(err, ErrForbidden) ||
		!strings.Contains(err.Error(), "mara may not promote control/inventory/cn-035.yaml (rule inventory needs platform-admin)") {
		t.Fatalf("mara promote: %v", err)
	}
	if _, err := s.Promote(ctx, dennis, "prod", c3); !errors.Is(err, ErrConflict) {
		t.Fatalf("prod before canary: %v", err)
	}
	if r, err := s.Promote(ctx, dennis, "canary", c3[:8]); err != nil || r.SHA != c3 {
		t.Fatalf("dennis promote: %+v %v", r, err)
	}
	w.verify("canary", c3)

	// Hold canary: prod stops, the hold survives a restart.
	if _, err := s.Hold(ctx, mara, "canary"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("mara hold: %v", err)
	}
	if r, err := s.Hold(ctx, dennis, "canary"); err != nil || !r.Held || r.HeldBy != "dennis" {
		t.Fatalf("hold: %+v %v", r, err)
	}
	s = w.service(dir)
	if p := ringOf(t, s, "prod"); p.Gate != GateNone || p.Reason != "ring canary is held by dennis" {
		t.Fatalf("prod after hold = %+v", p)
	}
	if _, err := s.Promote(ctx, dennis, "prod", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("promote after held ring: %v", err)
	}

	// The revert merges; promoting the held ring ends the hold once verified.
	c4 := w.commit("c-rev004", map[string]string{"control/inventory/cn-035.yaml": "name: cn-035\nrevert: true\n"})
	if r, err := s.Promote(ctx, dennis, "canary", ""); err != nil || r.SHA != c4 || !r.Held {
		t.Fatalf("promote held: %+v %v", r, err)
	}
	if _, err := s.Tick(ctx); err != nil || !ringOf(t, s, "canary").Held {
		t.Fatalf("released before verify: %v", err)
	}
	w.verify("canary", c4)
	if _, err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if c := ringOf(t, s, "canary"); c.Held {
		t.Fatalf("still held = %+v", c)
	}
	if a := w.auditLines(); a[len(a)-1] != "pk-ci ring.release ring/canary hold of dennis ended: "+c4[:7]+" verified" {
		t.Fatalf("audit = %q", a)
	}
	if p := ringOf(t, s, "prod"); p.Gate != GateManual || p.Candidate != c4 {
		t.Fatalf("prod after release = %+v", p)
	}

	// Release by hand.
	if _, err := s.Hold(ctx, dennis, "prod"); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Release(ctx, dennis, "prod"); err != nil || r.Held {
		t.Fatalf("release: %+v %v", r, err)
	}
	if _, err := s.Promote(ctx, dennis, "nope", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ring: %v", err)
	}
}

func TestHoldPinsRingsOnMain(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	s := w.service(t.TempDir())
	c1 := w.commit("c-app001", map[string]string{"clusters/defiant/apps/a.yaml": "a\n"})
	w.verify("canary", c1)
	w.verify("prod", c1)
	w.commit("c-app002", map[string]string{"clusters/defiant/apps/a.yaml": "b\n"})
	if _, err := s.Hold(ctx, dennis, "canary"); err != nil {
		t.Fatal(err)
	}
	if w.tag("canary") != c1 || w.tag("prod") != c1 {
		t.Fatalf("pins: canary %s prod %s, want %s", w.tag("canary"), w.tag("prod"), c1)
	}
}

func TestDurationText(t *testing.T) {
	for d, want := range map[time.Duration]string{time.Minute: "1m", 10 * time.Second: "10s", 90 * time.Second: "1m30s", time.Hour: "1h"} {
		if got := durationText(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

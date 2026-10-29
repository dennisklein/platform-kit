// Package rings moves the ring tags (ring/<name>) that the clusters of a
// ring follow (P2). Each ring takes the revision that the previous ring
// verified (the first ring takes main). The gate is automatic after the
// soak time when every changed path has a low-risk path rule, else a person
// who may approve those paths promotes by hand. An admin can hold a ring:
// the ring keeps its tag and the next rings stop.
package rings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/internal/audit"
	"github.com/dennisklein/platform-kit/internal/fluxsim"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/policy"
)

// Gates.
const (
	GateAuto   = "auto"
	GateManual = "manual"
	GateNone   = "none"
)

// Errors.
var (
	ErrNotFound  = errors.New("ring not found")
	ErrForbidden = errors.New("forbidden")
	ErrConflict  = errors.New("conflict")
)

// Git is the state repo as the ring service needs it.
type Git interface {
	// Refresh fetches main and the ring tags from the forge (throttled,
	// unless a wanted commit is missing).
	Refresh(ctx context.Context, want ...string) error
	// Resolve expands a revision to a full sha.
	Resolve(ctx context.Context, rev string) (string, error)
	// IsAncestor reports whether a is an ancestor of b (or equal).
	IsAncestor(ctx context.Context, a, b string) bool
	// Paths lists the paths changed between from and to.
	Paths(ctx context.Context, from, to string) ([]string, error)
	// Change returns the Pk-Change trailer of a commit.
	Change(ctx context.Context, sha string) string
	// SetTag points ring/<ring> at sha on the forge, pushed as user.
	SetTag(ctx context.Context, user, ring, sha string) error
}

// Ring is the state of one ring.
type Ring struct {
	Ring     string
	Clusters []string
	// Tag is ring/<ring> when the tag exists; SHA is its commit and Change
	// the Pk-Change of that commit.
	Tag, SHA, Change string
	// Following is what the clusters follow: the tag, or main (rings off).
	Following string
	// Current is the revision the ring follows (the tag or main head).
	Current string
	// Verified: all clusters of the ring are healthy and verified Current.
	Verified   bool
	VerifiedAt time.Time

	Held   bool
	HeldBy string
	HeldAt time.Time

	Candidate, CandidateChange string
	Gate                       string
	Reason                     string
	SoakUntil                  time.Time
	// Paths changed from Current to Candidate.
	Paths []string
}

// Service is what the bench needs.
type Service interface {
	List(ctx context.Context) ([]Ring, error)
	Promote(ctx context.Context, who identity.Identity, ring, sha string) (*Ring, error)
	Hold(ctx context.Context, who identity.Identity, ring string) (*Ring, error)
	Release(ctx context.Context, who identity.Identity, ring string) (*Ring, error)
	// HeldBy returns who holds a ring ("" when it is not held).
	HeldBy(ring string) string
}

// Options configure the service.
type Options struct {
	// Dir holds rings.json (holds survive restarts).
	Dir    string
	Git    Git
	Flux   fluxsim.Client
	Policy policy.Engine
	// Soak is how long a ring waits after the previous ring verified a
	// low-risk revision (PK_RING_SOAK, default 1m).
	Soak time.Duration
	// AutoUser pushes automatic promotions (default pk-ci).
	AutoUser string
	// Audit records what the automatic gate does (promotions, hold ends);
	// the bench audits the actions of people itself.
	Audit audit.Log
	Now   func() time.Time
	Log   *slog.Logger
}

type hold struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
	// ReleaseAt ends the hold when the ring verified this revision (a
	// promotion of the held ring, for example to a revert).
	ReleaseAt string `json:"releaseAt,omitempty"`
}

type stateFile struct {
	Holds map[string]hold `json:"holds"`
}

// Svc implements Service.
type Svc struct {
	o Options

	mu    sync.Mutex
	holds map[string]hold
	// seen is when a main head was first seen (soak of the first ring).
	seen map[string]time.Time
	// pushed are tags this service pushed that fluxsim does not show yet.
	pushed map[string]pushedTag
}

type pushedTag struct {
	sha string
	at  time.Time
}

// New returns the service and loads the holds.
func New(o Options) (*Svc, error) {
	if o.Git == nil || o.Flux == nil {
		return nil, errors.New("rings: git and flux are required")
	}
	if o.Soak <= 0 {
		o.Soak = time.Minute
	}
	if o.AutoUser == "" {
		o.AutoUser = "pk-ci"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	s := &Svc{o: o, holds: map[string]hold{}, seen: map[string]time.Time{}, pushed: map[string]pushedTag{}}
	if o.Dir != "" {
		if b, err := os.ReadFile(s.path()); err == nil {
			var st stateFile
			if err := json.Unmarshal(b, &st); err != nil {
				return nil, fmt.Errorf("%s: %w", s.path(), err)
			}
			for k, v := range st.Holds {
				s.holds[k] = v
			}
		}
	}
	return s, nil
}

func (s *Svc) path() string { return filepath.Join(s.o.Dir, "rings.json") }

func (s *Svc) saveLocked() error {
	if s.o.Dir == "" {
		return nil
	}
	b, err := json.MarshalIndent(stateFile{Holds: s.holds}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.o.Dir, 0o755); err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

// RingRank orders rings: lab, canary, prod, then others by name.
func RingRank(r string) int {
	switch r {
	case "lab":
		return 0
	case "canary":
		return 1
	case "prod":
		return 2
	}
	return 3
}

// List returns the rings in rollout order.
func (s *Svc) List(ctx context.Context) ([]Ring, error) {
	st, err := s.o.Flux.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("fluxsim: %w", err)
	}
	_ = s.o.Git.Refresh(ctx, wanted(st)...)
	var p *policy.Policy
	if s.o.Policy != nil {
		p, _ = s.o.Policy.Current(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked(ctx, st, p), nil
}

func (s *Svc) listLocked(ctx context.Context, st *fluxsim.Status, p *policy.Policy) []Ring {
	now := s.o.Now()
	byRing := map[string][]fluxsim.ClusterStatus{}
	for _, c := range st.Clusters {
		if c.Ring == "" || c.Twin != "" {
			continue
		}
		byRing[c.Ring] = append(byRing[c.Ring], c)
	}
	names := make([]string, 0, len(byRing))
	for n := range byRing {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if RingRank(names[i]) != RingRank(names[j]) {
			return RingRank(names[i]) < RingRank(names[j])
		}
		return names[i] < names[j]
	})
	tags := map[string]string{}
	for k, v := range st.Rings {
		tags[k] = v
	}
	for k, pt := range s.pushed {
		if tags[k] == pt.sha || now.Sub(pt.at) > time.Minute {
			delete(s.pushed, k)
			continue
		}
		tags[k] = pt.sha
	}
	if st.Main != "" {
		if _, ok := s.seen[st.Main]; !ok {
			s.seen[st.Main] = now
		}
	}

	out := make([]Ring, 0, len(names))
	for i, name := range names {
		cs := byRing[name]
		r := Ring{Ring: name, Following: "main", Current: st.Main}
		for _, c := range cs {
			r.Clusters = append(r.Clusters, c.ID)
		}
		sort.Strings(r.Clusters)
		if sha, ok := tags[name]; ok {
			r.Tag, r.SHA, r.Following, r.Current = "ring/"+name, sha, "ring/"+name, sha
			r.Change = s.o.Git.Change(ctx, sha)
		}
		r.Verified = r.Current != ""
		for _, c := range cs {
			if c.Verified == nil || c.Verified.SHA != r.Current || !c.Healthy {
				r.Verified = false
				break
			}
			if c.Verified.At.After(r.VerifiedAt) {
				r.VerifiedAt = c.Verified.At
			}
		}
		if !r.Verified {
			r.VerifiedAt = time.Time{}
		}
		if h, ok := s.holds[name]; ok {
			r.Held, r.HeldBy, r.HeldAt = true, h.By, h.At
		}

		// Candidate: main for the first ring, else what the previous ring verified.
		var since time.Time
		switch {
		case i == 0:
			r.Candidate, since = st.Main, s.seen[st.Main]
		case out[i-1].Held:
			r.Gate, r.Reason = GateNone, fmt.Sprintf("ring %s is held by %s", out[i-1].Ring, out[i-1].HeldBy)
		case !out[i-1].Verified:
			r.Gate, r.Reason = GateNone, fmt.Sprintf("waiting for ring %s to verify %s", out[i-1].Ring, short(out[i-1].Current))
		default:
			r.Candidate, since = out[i-1].Current, out[i-1].VerifiedAt
		}
		if r.Candidate != "" {
			r.CandidateChange = s.o.Git.Change(ctx, r.Candidate)
			s.gate(ctx, p, &r, since)
		}
		if r.Held && r.Gate == GateAuto {
			r.Gate = GateManual
		}
		if r.Held {
			r.Reason = strings.TrimSpace(fmt.Sprintf("held by %s since %s. %s", r.HeldBy, r.HeldAt.Format("15:04"), r.Reason))
		}
		out = append(out, r)
	}
	return out
}

// gate sets Gate, Reason, Paths and SoakUntil for a candidate.
func (s *Svc) gate(ctx context.Context, p *policy.Policy, r *Ring, since time.Time) {
	switch {
	case r.Tag == "":
		// A person turns the ring on: the first promote sets the tag.
		r.Gate, r.Reason = GateManual, "Rings are off: the clusters follow main. Promote pins ring/"+r.Ring+"."
		return
	case r.Candidate == r.Current:
		r.Gate, r.Reason = GateNone, "up to date"
		return
	case s.o.Git.IsAncestor(ctx, r.Candidate, r.Current):
		r.Gate, r.Reason = GateNone, "ahead of the candidate"
		return
	case !s.o.Git.IsAncestor(ctx, r.Current, r.Candidate):
		r.Gate, r.Reason = GateManual, "not a fast-forward of "+r.Tag
		return
	}
	paths, err := s.o.Git.Paths(ctx, r.Current, r.Candidate)
	if err != nil {
		r.Gate, r.Reason = GateManual, "cannot diff: "+err.Error()
		return
	}
	r.Paths = paths
	if len(paths) == 0 {
		r.Gate, r.Reason = GateAuto, "no file changes"
	} else if risky := riskyPaths(p, paths); len(risky) > 0 {
		r.Gate, r.Reason = GateManual, risky
		return
	} else {
		r.Gate, r.Reason = GateAuto, "low-risk paths only"
	}
	if since.IsZero() {
		since = s.o.Now()
	}
	r.SoakUntil = since.Add(s.o.Soak)
}

// riskyPaths describes the paths whose rule is not low risk, for example
// "high-risk paths: control/inventory/cn-035.yaml (inventory)". Without a
// policy every path counts as risky.
func riskyPaths(p *policy.Policy, paths []string) string {
	if p == nil {
		return "no policy: every promotion is manual"
	}
	level := policy.RiskLow
	var items []string
	for _, path := range paths {
		r := p.MatchRule(path)
		risk, name := policy.RiskHigh, "no rule"
		if r != nil {
			risk, name = r.Risk, r.Name
		}
		if risk == policy.RiskLow {
			continue
		}
		if risk.Rank() > level.Rank() {
			level = risk
		}
		items = append(items, fmt.Sprintf("%s (%s)", path, name))
	}
	if len(items) == 0 {
		return ""
	}
	more := ""
	if len(items) > 3 {
		more = fmt.Sprintf(" and %d more", len(items)-3)
		items = items[:3]
	}
	return fmt.Sprintf("%s-risk paths: %s%s", level, strings.Join(items, ", "), more)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func find(rs []Ring, name string) int {
	for i := range rs {
		if rs[i].Ring == name {
			return i
		}
	}
	return -1
}

// holds reports whether a person holds one of the principals (a role or
// team:<team>).
func holds(pe *policy.Person, principals []string) bool {
	for _, pr := range principals {
		if t, ok := strings.CutPrefix(pr, "team:"); ok {
			for _, x := range pe.Teams {
				if x == t {
					return true
				}
			}
			continue
		}
		for _, x := range pe.Roles {
			if x == pr {
				return true
			}
		}
	}
	return false
}

var adminRoles = []string{"owner", "platform-admin"}

func (s *Svc) person(ctx context.Context, who identity.Identity, verb string) (*policy.Policy, *policy.Person, error) {
	if !who.IsHuman() {
		return nil, nil, fmt.Errorf("%w: only people %s rings", ErrForbidden, verb)
	}
	if s.o.Policy == nil {
		return nil, nil, nil
	}
	p, err := s.o.Policy.Current(ctx)
	if err != nil {
		return nil, nil, err
	}
	pe := p.Person(who.User)
	if pe == nil {
		return nil, nil, fmt.Errorf("%w: %s is not in the policy", ErrForbidden, who.User)
	}
	return p, pe, nil
}

// wanted are the commits fluxsim knows: main and the ring tags.
func wanted(st *fluxsim.Status) []string {
	out := []string{st.Main}
	for _, sha := range st.Rings {
		out = append(out, sha)
	}
	return out
}

func (s *Svc) state(ctx context.Context) (*fluxsim.Status, *policy.Policy, error) {
	st, err := s.o.Flux.Status(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("fluxsim: %w", err)
	}
	if err := s.o.Git.Refresh(ctx, wanted(st)...); err != nil {
		return nil, nil, fmt.Errorf("fetch: %w", err)
	}
	var p *policy.Policy
	if s.o.Policy != nil {
		p, _ = s.o.Policy.Current(ctx)
	}
	return st, p, nil
}

// Promote moves ring/<ring> to sha (default the candidate). A person needs
// to be an approver of every changed path's rule. A ring after a held ring
// cannot be promoted; promoting a held ring ends the hold once the ring
// verified the new revision (the revert case).
func (s *Svc) Promote(ctx context.Context, who identity.Identity, ring, sha string) (*Ring, error) {
	_, pe, err := s.person(ctx, who, "promote")
	if err != nil {
		return nil, err
	}
	return s.promote(ctx, who.User, pe, ring, sha)
}

func (s *Svc) promote(ctx context.Context, user string, pe *policy.Person, ring, sha string) (*Ring, error) {
	st, p, err := s.state(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.listLocked(ctx, st, p)
	i := find(rs, ring)
	if i < 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ring)
	}
	r := rs[i]
	for _, prev := range rs[:i] {
		if prev.Held {
			return nil, fmt.Errorf("%w: ring %s is held by %s", ErrConflict, prev.Ring, prev.HeldBy)
		}
	}
	if sha == "" {
		sha = r.Candidate
		if sha == "" {
			return nil, fmt.Errorf("%w: nothing to promote: %s", ErrConflict, r.Reason)
		}
	} else {
		full, err := s.o.Git.Resolve(ctx, sha)
		if err != nil {
			return nil, fmt.Errorf("%w: unknown revision %s", ErrNotFound, sha)
		}
		sha = full
		if !s.o.Git.IsAncestor(ctx, sha, st.Main) {
			return nil, fmt.Errorf("%w: %s is not on main", ErrConflict, short(sha))
		}
		if i > 0 && rs[i-1].Current != "" && !s.o.Git.IsAncestor(ctx, sha, rs[i-1].Current) {
			return nil, fmt.Errorf("%w: ring %s has not taken %s yet", ErrConflict, rs[i-1].Ring, short(sha))
		}
	}
	if r.Tag != "" && sha == r.SHA {
		return &r, nil
	}
	if pe != nil && p != nil {
		from := r.Current
		paths, err := s.o.Git.Paths(ctx, from, sha)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			rule := p.MatchRule(path)
			if rule != nil && !holds(pe, rule.Approvers) {
				return nil, fmt.Errorf("%w: %s may not promote %s (rule %s needs %s)", ErrForbidden, pe.ID, path,
					rule.Name, strings.Join(rule.Approvers, " or "))
			}
		}
	}
	if err := s.o.Git.SetTag(ctx, user, ring, sha); err != nil {
		return nil, fmt.Errorf("push ring/%s: %w", ring, err)
	}
	s.o.Log.Info("ring promoted", "ring", ring, "sha", short(sha), "by", user)
	s.pushed[ring] = pushedTag{sha: sha, at: s.o.Now()}
	if h, ok := s.holds[ring]; ok {
		h.ReleaseAt = sha
		s.holds[ring] = h
		_ = s.saveLocked()
	}
	rs = s.listLocked(ctx, st, p)
	out := rs[find(rs, ring)]
	return &out, nil
}

// Hold stops a ring: its tag stays and the next rings stop. Rings that
// follow main (no tag) from this ring on get a tag at what they run now.
func (s *Svc) Hold(ctx context.Context, who identity.Identity, ring string) (*Ring, error) {
	_, pe, err := s.person(ctx, who, "hold")
	if err != nil {
		return nil, err
	}
	if pe != nil && !holds(pe, adminRoles) {
		return nil, fmt.Errorf("%w: only an owner or platform-admin holds a ring", ErrForbidden)
	}
	st, p, err := s.state(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.listLocked(ctx, st, p)
	i := find(rs, ring)
	if i < 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ring)
	}
	if !rs[i].Held {
		// Pin rings that follow main, so that the hold stops them.
		for _, r := range rs[i:] {
			if r.Tag != "" {
				continue
			}
			sha := running(st, r)
			if sha == "" {
				continue
			}
			if err := s.o.Git.SetTag(ctx, who.User, r.Ring, sha); err != nil {
				return nil, fmt.Errorf("pin ring/%s: %w", r.Ring, err)
			}
			s.pushed[r.Ring] = pushedTag{sha: sha, at: s.o.Now()}
		}
		s.holds[ring] = hold{By: who.User, At: s.o.Now().UTC()}
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
		s.o.Log.Info("ring held", "ring", ring, "by", who.User)
	}
	rs = s.listLocked(ctx, st, p)
	out := rs[find(rs, ring)]
	return &out, nil
}

// running is the revision the clusters of a ring run: the verified
// revision they share, else the first applied revision, else main.
func running(st *fluxsim.Status, r Ring) string {
	var verified, applied string
	same := true
	for _, c := range st.Clusters {
		if c.Ring != r.Ring || c.Twin != "" {
			continue
		}
		if c.Verified == nil || (verified != "" && c.Verified.SHA != verified) {
			same = false
		} else {
			verified = c.Verified.SHA
		}
		if applied == "" && c.Applied != nil {
			applied = c.Applied.SHA
		}
	}
	switch {
	case same && verified != "":
		return verified
	case applied != "":
		return applied
	}
	return st.Main
}

// HeldBy returns who holds a ring.
func (s *Svc) HeldBy(ring string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holds[ring].By
}

// Release ends a hold.
func (s *Svc) Release(ctx context.Context, who identity.Identity, ring string) (*Ring, error) {
	_, pe, err := s.person(ctx, who, "release")
	if err != nil {
		return nil, err
	}
	if pe != nil && !holds(pe, adminRoles) {
		return nil, fmt.Errorf("%w: only an owner or platform-admin releases a ring", ErrForbidden)
	}
	st, p, err := s.state(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.listLocked(ctx, st, p)
	if find(rs, ring) < 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ring)
	}
	if _, ok := s.holds[ring]; ok {
		delete(s.holds, ring)
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
		s.o.Log.Info("ring released", "ring", ring, "by", who.User)
	}
	rs = s.listLocked(ctx, st, p)
	out := rs[find(rs, ring)]
	return &out, nil
}

// Tick runs the automatic gate once: it ends holds whose release revision
// is verified and promotes at most one ring whose gate is auto and whose
// soak is over. It returns the promoted ring ("" when none).
func (s *Svc) Tick(ctx context.Context) (string, error) {
	st, p, err := s.state(ctx)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	rs := s.listLocked(ctx, st, p)
	changed := false
	for _, r := range rs {
		if h, ok := s.holds[r.Ring]; ok && h.ReleaseAt != "" && r.Verified && r.Current == h.ReleaseAt {
			delete(s.holds, r.Ring)
			changed = true
			s.o.Log.Info("ring released: promoted revision verified", "ring", r.Ring, "sha", short(h.ReleaseAt))
		}
	}
	var released []Ring
	if changed {
		_ = s.saveLocked()
		for _, r := range rs {
			if r.Held {
				if _, still := s.holds[r.Ring]; !still {
					released = append(released, r)
				}
			}
		}
		rs = s.listLocked(ctx, st, p)
	}
	now := s.o.Now()
	var next *Ring
	for i := range rs {
		r := rs[i]
		if r.Held {
			break
		}
		if r.Gate == GateAuto && r.Candidate != "" && !now.Before(r.SoakUntil) {
			next = &r
			break
		}
	}
	s.mu.Unlock()
	for _, r := range released {
		s.audit(ctx, "ring.release", r.Ring, fmt.Sprintf("hold of %s ended: %s verified", r.HeldBy, short(r.Current)))
	}
	if next == nil {
		return "", nil
	}
	if _, err := s.promote(ctx, s.o.AutoUser, nil, next.Ring, next.Candidate); err != nil {
		return "", err
	}
	soak := durationText(s.o.Soak)
	detail := fmt.Sprintf("%s (auto gate: %s, soak %s)", short(next.Candidate), next.Reason, soak)
	if next.CandidateChange != "" {
		detail = fmt.Sprintf("%s %s (auto gate: %s, soak %s)", short(next.Candidate), next.CandidateChange, next.Reason, soak)
	}
	s.audit(ctx, "ring.promote", next.Ring, detail)
	return next.Ring, nil
}

// durationText is 1m for 1m0s and 1h for 1h0m0s.
func durationText(d time.Duration) string {
	t := d.String()
	if strings.HasSuffix(t, "m0s") {
		t = strings.TrimSuffix(t, "0s")
	}
	if strings.HasSuffix(t, "h0m") {
		t = strings.TrimSuffix(t, "0m")
	}
	return t
}

func (s *Svc) audit(ctx context.Context, action, ring, detail string) {
	if s.o.Audit == nil {
		return
	}
	_ = s.o.Audit.Append(ctx, audit.Entry{Actor: s.o.AutoUser, ActorKind: "bot", Action: action, Target: "ring/" + ring, Detail: detail})
}

// Loop runs Tick every interval until ctx ends.
func (s *Svc) Loop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var last string
	for {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := s.Tick(c)
		cancel()
		if err != nil && err.Error() != last {
			s.o.Log.Warn("ring gate", "err", err)
		}
		last = ""
		if err != nil {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

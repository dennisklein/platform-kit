package change

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/forge"
)

// forgeState is the last poll of the forge.
type forgeState struct {
	prs      []forge.PR
	reviews  map[int][]forge.Review
	statuses map[string][]forge.CommitStatus
	at       time.Time
	err      error
}

func (s *Svc) cachedReviews(number int) []forge.Review {
	s.fmu.RLock()
	defer s.fmu.RUnlock()
	return s.fs.reviews[number]
}

func (s *Svc) cachedStatuses(sha string) []forge.CommitStatus {
	s.fmu.RLock()
	defer s.fmu.RUnlock()
	return s.fs.statuses[sha]
}

// pollForge reads pull requests, reviews and statuses of open pull requests.
func (s *Svc) pollForge(ctx context.Context) error {
	if s.o.Forge == nil {
		return nil
	}
	prs, err := s.o.Forge.ListPRs(ctx, forge.PRAll)
	if err != nil {
		s.fmu.Lock()
		s.fs.err = err
		s.fmu.Unlock()
		return err
	}
	st := forgeState{prs: prs, reviews: map[int][]forge.Review{}, statuses: map[string][]forge.CommitStatus{}, at: s.o.Now()}
	for _, pr := range prs {
		if pr.State != forge.PROpen {
			continue
		}
		if rs, err := s.o.Forge.ListReviews(ctx, pr.Number); err == nil {
			st.reviews[pr.Number] = rs
		}
		if pr.HeadSHA != "" {
			if ss, err := s.o.Forge.Statuses(ctx, pr.HeadSHA); err == nil {
				st.statuses[pr.HeadSHA] = ss
			}
		}
	}
	s.fmu.Lock()
	s.fs = st
	s.fmu.Unlock()
	s.saveForgeCache(st)
	return nil
}

// forgeCacheFile keeps the last forge poll across restarts: a bench that
// starts while the forge is down still knows the merge requests, reviews
// and statuses (the change list stays complete, shown as offline).
type forgeCacheFile struct {
	PRs      []forge.PR                      `json:"prs"`
	Reviews  map[int][]forge.Review          `json:"reviews"`
	Statuses map[string][]forge.CommitStatus `json:"statuses"`
	At       time.Time                       `json:"at"`
}

func (s *Svc) forgeCachePath() string { return filepath.Join(s.o.DataDir, "forge-cache.json") }

func (s *Svc) saveForgeCache(st forgeState) {
	b, err := json.Marshal(forgeCacheFile{PRs: st.prs, Reviews: st.reviews, Statuses: st.statuses})
	if err != nil {
		return
	}
	s.fmu.Lock()
	same := bytes.Equal(b, s.fsSaved)
	if !same {
		s.fsSaved = b
	}
	s.fmu.Unlock()
	if same {
		return
	}
	tmp := s.forgeCachePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.forgeCachePath())
}

// loadForgeCache fills an empty forge cache from the last saved poll.
func (s *Svc) loadForgeCache() bool {
	b, err := os.ReadFile(s.forgeCachePath())
	if err != nil {
		return false
	}
	var f forgeCacheFile
	if json.Unmarshal(b, &f) != nil {
		return false
	}
	s.fmu.Lock()
	defer s.fmu.Unlock()
	if len(s.fs.prs) > 0 {
		return false
	}
	if f.Reviews == nil {
		f.Reviews = map[int][]forge.Review{}
	}
	if f.Statuses == nil {
		f.Statuses = map[string][]forge.CommitStatus{}
	}
	s.fs = forgeState{prs: f.PRs, reviews: f.Reviews, statuses: f.Statuses, at: f.At, err: s.fs.err}
	return true
}

// forgeOfflineAfter is the number of failed refresh rounds in a row after
// which the forge counts as offline: one slow or dropped request does not
// turn the top bar pill to "offline".
const forgeOfflineAfter = 2

// ForgeOffline reports that the fetch or forge poll failed in the last
// forgeOfflineAfter refresh rounds: lists and changes come from the local
// clone and the cached forge state.
func (s *Svc) ForgeOffline() bool {
	s.fmu.RLock()
	defer s.fmu.RUnlock()
	return (s.fs.err != nil || s.fetchErr != nil) && s.forgeFails >= forgeOfflineAfter
}

// refreshPR updates one pull request in the cache after an own action.
func (s *Svc) refreshPR(ctx context.Context, number int) {
	if s.o.Forge == nil || number == 0 {
		return
	}
	pr, err := s.o.Forge.GetPR(ctx, number)
	if err != nil {
		return
	}
	rs, _ := s.o.Forge.ListReviews(ctx, number)
	var ss []forge.CommitStatus
	if pr.HeadSHA != "" {
		ss, _ = s.o.Forge.Statuses(ctx, pr.HeadSHA)
	}
	s.fmu.Lock()
	defer s.fmu.Unlock()
	found := false
	for i := range s.fs.prs {
		if s.fs.prs[i].Number == number {
			s.fs.prs[i] = *pr
			found = true
		}
	}
	if !found {
		s.fs.prs = append(s.fs.prs, *pr)
	}
	if s.fs.reviews == nil {
		s.fs.reviews = map[int][]forge.Review{}
		s.fs.statuses = map[string][]forge.CommitStatus{}
	}
	s.fs.reviews[number] = rs
	if pr.HeadSHA != "" {
		s.fs.statuses[pr.HeadSHA] = ss
	}
}

// ForgeError returns the error of the last forge poll (nil when it worked).
func (s *Svc) ForgeError() error {
	s.fmu.RLock()
	defer s.fmu.RUnlock()
	return s.fs.err
}

// Refresh fetches, moves the main worktree, polls the forge and publishes
// events for what changed.
func (s *Svc) Refresh(ctx context.Context) error {
	if err := s.ensure(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	fetchErr := s.fetchLocked(ctx)
	s.fmu.Lock()
	s.fetchErr = fetchErr
	s.fmu.Unlock()
	ferr := fetchErr
	if ferr == nil {
		ferr = s.updateMainLocked(ctx)
	}
	s.mu.Unlock()
	perr := s.pollForge(ctx)
	s.fmu.Lock()
	if fetchErr != nil || perr != nil {
		s.forgeFails++
	} else {
		s.forgeFails = 0
	}
	s.fmu.Unlock()
	s.publishChanges(ctx)
	if ferr != nil {
		return ferr
	}
	return perr
}

// Run refreshes every interval until ctx ends.
func (s *Svc) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	failing := false
	for {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.Refresh(rctx)
		cancel()
		if err != nil && !failing {
			s.log.Warn("refresh", "err", err)
		} else if err == nil && failing {
			s.log.Info("refresh works again")
		}
		failing = err != nil
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// fp is the fingerprint of a change for event detection.
type fp struct {
	git, pr, reviews, statuses, merge string
}

func (s *Svc) fingerprint(r *record) fp {
	f := fp{git: r.localSHA + "|" + r.remoteSHA + "|" + s.mainH}
	if r.pr != nil {
		f.pr = fmt.Sprintf("%d|%s|%v|%s|%s", r.pr.Number, r.pr.State, r.pr.Draft, r.pr.HeadSHA, r.pr.Base)
		var keys []string
		for _, rv := range s.cachedReviews(r.pr.Number) {
			keys = append(keys, rv.User+":"+string(rv.State)+":"+rv.SHA)
		}
		sort.Strings(keys)
		f.reviews = strings.Join(keys, ",")
		for _, st := range s.cachedStatuses(r.pr.HeadSHA) {
			f.statuses += st.Context + "=" + string(st.State) + ";"
		}
	}
	if r.merge != nil {
		f.merge = r.merge.SHA
	}
	return f
}

// publishChanges sends change.updated and forge.updated for changes whose
// git or forge state moved since the last call.
func (s *Svc) publishChanges(ctx context.Context) {
	recs, err := s.records(ctx)
	if err != nil {
		return
	}
	s.seenMu.Lock()
	first := len(s.seen) == 0
	type upd struct {
		r      *record
		reason string
		forge  string
	}
	var todo []upd
	for id, r := range recs {
		cur := s.fingerprint(r)
		old, had := s.seen[id]
		s.seen[id] = cur
		if first || (had && old == cur) {
			continue
		}
		if had && old.merge != "" && old.merge == cur.merge {
			continue // merged changes only move with main
		}
		u := upd{r: r, reason: "refresh"}
		if !had {
			u.reason = "new"
		}
		switch {
		case cur.merge != "" && old.merge == "":
			u.forge = "merge"
		case cur.pr != old.pr:
			u.forge = "pr"
		case cur.reviews != old.reviews:
			u.forge = "review"
		case cur.statuses != old.statuses:
			u.forge = "status"
		}
		if u.forge != "" {
			u.reason = u.forge
		}
		todo = append(todo, u)
	}
	s.seenMu.Unlock()
	for _, u := range todo {
		if u.forge != "" {
			var mr *MR
			sha := ""
			if u.r.pr != nil {
				mr = &MR{Number: u.r.pr.Number, URL: u.r.pr.URL, Draft: u.r.pr.Draft, State: string(u.r.pr.State), Title: u.r.pr.Title}
				sha = u.r.pr.HeadSHA
			}
			s.o.Events.Publish(events.ForgeUpdated, forgeEvent(u.forge, u.r.id, mr, sha))
		}
		s.publishChange(ctx, u.r, s.o.User, u.reason)
	}
}

// publishChange sends change.updated with the summary.
func (s *Svc) publishChange(ctx context.Context, r *record, me, reason string) {
	if d, err := s.derive(ctx, r, me, false); err == nil {
		s.o.Events.Publish(events.ChangeUpdated, changeEvent(d.ch.Summary, reason))
		return
	}
	s.o.Events.Publish(events.ChangeUpdated, ChangeEvent{ChangeID: r.id, Reason: reason})
}

// notify publishes an event for one change after an own action.
func (s *Svc) notify(ctx context.Context, t events.Type, id, reason string) {
	r, err := s.record(ctx, id)
	if err != nil {
		s.o.Events.Publish(t, ChangeEvent{ChangeID: id, Reason: reason})
		return
	}
	s.seenMu.Lock()
	s.seen[id] = s.fingerprint(r)
	s.seenMu.Unlock()
	if d, err := s.derive(ctx, r, s.me(ctx), false); err == nil {
		s.o.Events.Publish(t, changeEvent(d.ch.Summary, reason))
		return
	}
	s.o.Events.Publish(t, ChangeEvent{ChangeID: id, Reason: reason})
}

// Notify publishes change.updated for one change now (the bench calls it
// after file saves, so the rail shows "dirty" without waiting for the poll).
func (s *Svc) Notify(ctx context.Context, id, reason string) {
	s.notify(ctx, events.ChangeUpdated, id, reason)
}

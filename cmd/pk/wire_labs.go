package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/dennisklein/platform-kit/internal/audit"
	"github.com/dennisklein/platform-kit/internal/bench"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/fluxsim"
	"github.com/dennisklein/platform-kit/internal/lab"
	"github.com/dennisklein/platform-kit/internal/render"
	"github.com/dennisklein/platform-kit/internal/rings"
)

// wireLabs sets the lab service and the fluxsim client (area C).
func wireLabs(ctx context.Context, cfg config.Config, d *bench.Deps) error {
	log := slog.Default().With("component", "lab")
	d.Flux = fluxsim.NewClient(cfg.FluxURL)
	if d.Changes != nil {
		svc, err := lab.New(lab.Options{
			Dir:      cfg.LabsDir(),
			Speed:    cfg.LabSpeed,
			Changes:  &labChanges{svc: d.Changes, events: d.Events},
			Registry: d.Kinds,
			Renderer: d.Renderer,
			Policy:   d.Policy,
			Events:   d.Events,
			Log:      log,
		})
		if err != nil {
			return err
		}
		// Runs from before the last forge rebuild belong to the old state.
		if since, ok := forgeGeneration(cfg); ok {
			forgetLabRuns(svc, since, log)
		}
		d.Labs = svc
	}
	if d.Audit == nil {
		// The same log the bench would create, shared with the ring gate.
		d.Audit = audit.NewFileLog(filepath.Join(cfg.StateDir(), "audit.jsonl"))
	}
	rs, err := rings.New(rings.Options{
		Audit: d.Audit,
		Dir:   filepath.Join(cfg.StateDir(), "rings"),
		Git: &rings.GitRepo{Dir: filepath.Join(cfg.DataDir, "rings"), ForgeURL: cfg.ForgeURL, Owner: cfg.ForgeOwner,
			Repo: cfg.ForgeRepo, ForgeURLSet: os.Getenv("PK_FORGE_URL") != "", CredentialsPath: cfg.ForgeCredentials,
			URL: os.Getenv("PK_FLUX_REMOTE")},
		Flux:   d.Flux,
		Policy: d.Policy,
		Soak:   fluxEnvDuration("PK_RING_SOAK", time.Minute),
		Log:    slog.Default().With("component", "rings"),
	})
	if err != nil {
		return err
	}
	d.Rings = rs
	go rs.Loop(ctx, cfg.PollInterval)
	go watchFlux(ctx, d.Flux, d.Events, cfg.PollInterval)
	return nil
}

// labChanges adapts the change service to what the lab service needs (the
// change package imports lab, so lab cannot import change).
type labChanges struct {
	svc    change.Service
	events events.Publisher
}

func (a *labChanges) Info(ctx context.Context, id string) (*lab.ChangeInfo, error) {
	ch, err := a.svc.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	info := &lab.ChangeInfo{ID: ch.ID, Title: ch.Title, Author: ch.Author, Branch: ch.Branch, HeadSHA: ch.HeadSHA,
		Worktree: ch.Worktree}
	for _, r := range ch.LabRuns {
		info.LabRuns = append(info.LabRuns, r.Run)
	}
	return info, nil
}

func (a *labChanges) MainDir(ctx context.Context) (string, error) { return a.svc.MainDir(ctx) }

func (a *labChanges) Sources(ctx context.Context, id string) (render.Source, render.Source, error) {
	return a.svc.Sources(ctx, id)
}

func (a *labChanges) Source(ctx context.Context, ref string) (render.Source, error) {
	return a.svc.Source(ctx, ref)
}

func (a *labChanges) RecordLabRun(ctx context.Context, id string, run lab.RunRef, report []byte) error {
	return a.svc.RecordLabRun(ctx, id, change.LabRunRef{Run: run.Run, SHA: run.SHA, Result: change.LabResult(run.Result),
		Report: run.Report}, report)
}

func (a *labChanges) Updated(ctx context.Context, id, reason string) {
	if a.events == nil {
		return
	}
	ev := change.ChangeEvent{ChangeID: id, Reason: reason}
	if ch, err := a.svc.Get(ctx, id); err == nil {
		s := change.SummaryToAPI(ch.Summary)
		ev.Change = &s
	}
	a.events.Publish(events.ChangeUpdated, ev)
}

// watchFlux polls fluxsim and publishes flux.updated (data: api.Cluster)
// for each cluster or twin whose state changed.
func watchFlux(ctx context.Context, fc fluxsim.Client, pub events.Publisher, interval time.Duration) {
	if fc == nil || pub == nil {
		return
	}
	if interval <= 0 || interval > 2*time.Second {
		interval = 2 * time.Second
	}
	last := map[string]fluxsim.ClusterStatus{}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		st, err := fc.Status(c)
		cancel()
		if err == nil {
			for _, cs := range append(st.Clusters, st.Twins...) {
				prev, ok := last[cs.ID]
				if ok && sameFlux(prev, cs) {
					continue
				}
				last[cs.ID] = cs
				pub.Publish(events.FluxUpdated, bench.FluxClusterToAPI(&cs))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func sameFlux(a, b fluxsim.ClusterStatus) bool {
	a.LastApplied, b.LastApplied = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

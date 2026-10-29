package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/api"
	"github.com/dennisklein/platform-kit/internal/advisor"
	"github.com/dennisklein/platform-kit/internal/bench"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/chat"
	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/inproc"
	"github.com/dennisklein/platform-kit/internal/llm"
	"github.com/dennisklein/platform-kit/internal/mcp"
)

// wireLLM sets the LLM client, log, index, redactor, advisors, chat and the
// MCP handler (area D).
//
// The advisors, the chat and MCP call the bench API in process. The API
// handler exists only after bench.New, so they share a Ref that the MCP
// factory (called by bench.New) sets.
func wireLLM(ctx context.Context, cfg config.Config, d *bench.Deps) error {
	log := slog.Default().With("component", "llm")
	if err := os.MkdirAll(cfg.LLMDir(), 0o700); err != nil {
		return err
	}
	red := llm.NewRedactor()
	d.Redactor = red
	client := llm.New(llm.Config{BaseURL: cfg.LLMBaseURL, Model: cfg.LLMModel, APIKey: cfg.LLMAPIKey, EmbedModel: cfg.LLMEmbedModel, Timeout: cfg.LLMTimeout}, red)
	d.LLM = client
	lg := llm.NewFileLog(filepath.Join(cfg.LLMDir(), "log.jsonl"))
	d.LLMLog = lg

	idx := llm.NewIndex(collector(d, red, log), client)
	d.Search = idx
	go idx.Watch(ctx, 10*time.Second)

	ref := &inproc.Ref{}
	apiClient := inproc.New(ref, bench.BasePath)
	tools := &advisor.APITools{API: apiClient, Index: idx}
	cs, err := chat.New(chat.Options{Tools: tools, LLM: client, Log: lg, Events: d.Events,
		Path: filepath.Join(cfg.LLMDir(), "suggestions.json")})
	if err != nil {
		return err
	}
	d.Chat = cs
	d.Advisors = advisor.New(advisor.Options{Tools: tools, LLM: client, Log: lg, Store: cs, Kinds: d.Kinds})

	d.MCP = func(h http.Handler) http.Handler {
		ref.Set(h)
		m, err := mcp.New(mcp.Options{Spec: api.Spec, API: h, BasePath: bench.BasePath, Events: d.Events,
			Version: version, Logger: slog.Default().With("component", "mcp")})
		if err != nil {
			log.Error("mcp disabled", "err", err)
			return nil
		}
		return m
	}
	if cfg.LLMBaseURL == "" {
		log.Info("LLM mock mode (PK_LLM_BASE_URL unset)")
	} else {
		log.Info("LLM", "url", cfg.LLMBaseURL, "model", cfg.LLMModel, "embeddings", cfg.LLMEmbedModel)
	}
	return nil
}

// collector reads the documents of the search index: main (kb and YAML),
// the merge trailers of main, the merge request discussions and the lab
// runs of open changes. The version is main's commit plus a fingerprint of
// the forge and lab state, so the index rebuilds when either changes.
func collector(d *bench.Deps, red llm.Redactor, log *slog.Logger) llm.Collector {
	return func(ctx context.Context) ([]llm.Document, string, error) {
		if d.Changes == nil {
			return nil, "", fmt.Errorf("no change service")
		}
		dir, err := d.Changes.MainDir(ctx)
		if err != nil {
			return nil, "", err
		}
		repo := gitx.Open(dir)
		head, err := repo.Head(ctx)
		if err != nil {
			return nil, "", err
		}
		fp := fnv.New64a()
		var extra []llm.Document

		if d.Forge != nil {
			prs, err := d.Forge.ListPRs(ctx, forge.PRAll)
			if err != nil {
				log.Debug("index: pull requests", "err", err)
			}
			for _, pr := range prs {
				fmt.Fprintf(fp, "pr%d:%s:%s;", pr.Number, pr.UpdatedAt.Format(time.RFC3339Nano), pr.State)
				cs, err := d.Forge.ListComments(ctx, pr.Number)
				if err != nil {
					continue
				}
				var comments []string
				for _, c := range cs {
					comments = append(comments, c.User+": "+c.Body)
					fmt.Fprintf(fp, "c%d;", c.ID)
				}
				extra = append(extra, llm.MRDoc(pr.Number, changeOfBranch(pr.Head), pr.Title, pr.Body, comments, red))
			}
		}
		if d.Labs != nil {
			sums, err := d.Changes.List(ctx, false)
			if err == nil {
				for _, s := range sums {
					runs, err := d.Labs.Runs(ctx, s.ID)
					if err != nil {
						continue
					}
					for _, r := range runs {
						fmt.Fprintf(fp, "run%s:%s;", r.ID, r.Result)
						text := fmt.Sprintf("lab run %s of %s: %s\n%s\n%s\n%s", r.ID, s.ID, r.Result, r.Summary,
							strings.Join(r.Failures, "\n"), r.Report)
						if red != nil {
							text, _ = red.Redact(text)
						}
						extra = append(extra, llm.Document{Kind: "lab", Ref: r.ID, Title: fmt.Sprintf("lab %s %s (%s)", r.Lab, r.ID, r.Result),
							Text: text, Change: s.ID})
					}
				}
			}
		}
		version := fmt.Sprintf("%s/%x", head, fp.Sum64())
		if version == llm.KnownVersion(ctx) {
			return nil, version, nil // unchanged: the index keeps what it has
		}

		docs, err := llm.RepoDocs(dir, red)
		if err != nil {
			return nil, "", err
		}
		entries, err := repo.Log(ctx, "HEAD", "--first-parent", "-n", "500")
		if err == nil {
			docs = append(docs, llm.TrailerDocs(entries, red)...)
		}
		docs = append(docs, extra...)
		return docs, version, nil
	}
}

// changeOfBranch returns the change id of a change branch.
func changeOfBranch(branch string) string {
	id, _, _ := change.ParseBranch(branch)
	return id
}

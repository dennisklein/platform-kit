package lab

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// store keeps raw results in <dir>/<change>/run-<n>/: run.json (the run
// with steps and checks), lab.yaml, topology.json and report.md.
type store struct{ dir string }

func (s store) runDir(id string) string {
	return filepath.Join(s.dir, filepath.FromSlash(id))
}

func writeAtomic(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s store) saveRun(r *Run) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.runDir(r.ID), "run.json"), b)
}

func (s store) saveFile(id, name string, b []byte) error {
	return writeAtomic(filepath.Join(s.runDir(id), name), b)
}

func (s store) saveJSON(id, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return s.saveFile(id, name, b)
}

func (s store) saveYAML(id, name string, v any) error {
	b, err := OrderedYAML(v)
	if err != nil {
		return err
	}
	return s.saveFile(id, name, b)
}

// load reads all runs. Runs that were running when the bench stopped are
// marked cancelled.
func (s store) load() ([]*Run, error) {
	paths, err := filepath.Glob(filepath.Join(s.dir, "*", "run-*", "run.json"))
	if err != nil {
		return nil, err
	}
	var out []*Run
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r Run
		if err := json.Unmarshal(b, &r); err != nil || r.ID == "" {
			continue
		}
		if r.Result == ResultRunning {
			r.Result = ResultCancelled
			r.Summary = "cancelled: the bench stopped during the run"
			if r.FinishedAt.IsZero() {
				r.FinishedAt = time.Now().UTC()
			}
			for i := range r.Steps {
				if r.Steps[i].Status == StepRunning || r.Steps[i].Status == StepPending {
					r.Steps[i].Status = StepSkipped
				}
			}
			_ = s.saveRun(&r)
		}
		out = append(out, &r)
	}
	sortRuns(out)
	return out, nil
}

// sortRuns sorts newest first.
func sortRuns(rs []*Run) {
	sort.SliceStable(rs, func(i, j int) bool {
		if !rs[i].StartedAt.Equal(rs[j].StartedAt) {
			return rs[i].StartedAt.After(rs[j].StartedAt)
		}
		return rs[i].ID > rs[j].ID
	})
}

// RunID is the id of run n of a change.
func RunID(changeID string, n int) string { return fmt.Sprintf("%s/run-%d", changeID, n) }

// RunNumber parses the number of a run id (c-k7m2qz/run-3 → 3).
func RunNumber(id string) int {
	i := strings.LastIndex(id, "/run-")
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(id[i+5:])
	return n
}

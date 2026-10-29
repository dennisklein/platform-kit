// Package audit is the append-only audit trail of the bench (agent grants,
// LLM log deletes, merges, break-glass records).
package audit

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry is one audit record.
type Entry struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Actor     string    `json:"actor"`
	ActorKind string    `json:"actorKind,omitempty"`
	Action    string    `json:"action"`
	Target    string    `json:"target,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// Common actions.
const (
	AgentApprove = "agent.approve"
	AgentRevoke  = "agent.revoke"
	LLMDelete    = "llm.delete"
	LLMExport    = "llm.export"
	ChangeMerge  = "change.merge"
	ChangeClose  = "change.close"
	DemoApprove  = "demo.approve"
)

// Log is the audit trail.
type Log interface {
	Append(ctx context.Context, e Entry) error
	// List returns the newest entries first.
	List(ctx context.Context, limit int) ([]Entry, error)
}

// FileLog stores entries as JSON lines in one file.
type FileLog struct {
	mu   sync.Mutex
	path string
}

// NewFileLog returns a log that appends to path (created on first write).
func NewFileLog(path string) *FileLog { return &FileLog{path: path} }

func (l *FileLog) Append(_ context.Context, e Entry) error {
	if e.ID == "" {
		e.ID = newID()
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (l *FileLog) List(_ context.Context, limit int) ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	if out == nil {
		out = []Entry{}
	}
	return out, nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "a-" + hex.EncodeToString(b)
}

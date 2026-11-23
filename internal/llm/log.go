package llm

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned for an unknown log entry.
var ErrNotFound = errors.New("llm log entry not found")

// FileLog is the LLM log as JSON lines (<data>/llm/log.jsonl). New entries
// are appended; a rating or a delete rewrites the file atomically.
type FileLog struct {
	mu      sync.Mutex
	path    string
	entries []LogEntry // oldest first
	loaded  bool
	// torn: the file does not end with a newline.
	torn bool
	now  func() time.Time
}

// NewFileLog returns the log at path (created on the first append).
func NewFileLog(path string) *FileLog {
	return &FileLog{path: path, now: time.Now}
}

func (l *FileLog) load() error {
	if l.loaded {
		return nil
	}
	b, err := os.ReadFile(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		l.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e LogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// A torn last line (crash during append) is skipped.
			continue
		}
		l.entries = append(l.entries, e)
	}
	l.torn = len(b) > 0 && b[len(b)-1] != '\n'
	l.loaded = true
	return nil
}

// NewLogID returns a new entry id.
func NewLogID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "l-" + string(b)
}

func (l *FileLog) Append(ctx context.Context, e LogEntry) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return "", err
	}
	if e.ID == "" {
		e.ID = NewLogID()
	}
	if e.Time.IsZero() {
		e.Time = l.now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	line := append(b, '\n')
	if l.torn {
		line = append([]byte{'\n'}, line...)
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	l.torn = false
	l.entries = append(l.entries, e)
	return e.ID, nil
}

func (l *FileLog) Get(ctx context.Context, id string) (*LogEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return nil, err
	}
	for i := range l.entries {
		if l.entries[i].ID == id {
			e := l.entries[i]
			return &e, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
}

// List returns entries newest first. All words of q must match the prompt,
// answer, user, surface, recipe, change or model (case-insensitive). A word
// key:value filters a field: user, surface, recipe, change, model, rating.
func (l *FileLog) List(ctx context.Context, q string, limit int) ([]LogEntry, int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return nil, 0, err
	}
	words := strings.Fields(strings.ToLower(q))
	out := []LogEntry{}
	total := 0
	for i := len(l.entries) - 1; i >= 0; i-- {
		e := l.entries[i]
		if !matchEntry(e, words) {
			continue
		}
		total++
		if limit <= 0 || len(out) < limit {
			out = append(out, e)
		}
	}
	return out, total, nil
}

func matchEntry(e LogEntry, words []string) bool {
	if len(words) == 0 {
		return true
	}
	fields := map[string]string{
		"user": e.User, "surface": string(e.Surface), "recipe": e.Recipe,
		"change": e.ChangeID, "model": e.Model, "rating": e.Rating,
	}
	var hay string
	for _, w := range words {
		if k, v, ok := strings.Cut(w, ":"); ok {
			if f, known := fields[k]; known {
				if !strings.EqualFold(f, v) {
					return false
				}
				continue
			}
		}
		if hay == "" {
			hay = strings.ToLower(strings.Join([]string{e.Prompt, e.Answer, e.User, string(e.Surface), e.Recipe, e.ChangeID, e.Model, e.ID}, "\n"))
		}
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

// Rate sets the rating (up, down, or "" to clear).
func (l *FileLog) Rate(ctx context.Context, id, rating string) (*LogEntry, error) {
	if rating != "up" && rating != "down" && rating != "" {
		return nil, fmt.Errorf("rating must be up or down, not %q", rating)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return nil, err
	}
	for i := range l.entries {
		if l.entries[i].ID == id {
			old := l.entries[i].Rating
			l.entries[i].Rating = rating
			if err := l.rewrite(); err != nil {
				l.entries[i].Rating = old
				return nil, err
			}
			e := l.entries[i]
			return &e, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
}

func (l *FileLog) Delete(ctx context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return err
	}
	for i := range l.entries {
		if l.entries[i].ID == id {
			old := l.entries
			l.entries = append(append([]LogEntry(nil), old[:i]...), old[i+1:]...)
			if err := l.rewrite(); err != nil {
				l.entries = old
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

// Export writes all entries as JSON lines, oldest first.
func (l *FileLog) Export(ctx context.Context, w io.Writer) error {
	l.mu.Lock()
	if err := l.load(); err != nil {
		l.mu.Unlock()
		return err
	}
	entries := append([]LogEntry(nil), l.entries...)
	l.mu.Unlock()
	enc := json.NewEncoder(w)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

func (l *FileLog) rewrite() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	for _, e := range l.entries {
		if err := enc.Encode(e); err != nil {
			f.Close()
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

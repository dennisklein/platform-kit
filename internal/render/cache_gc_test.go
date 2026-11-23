package render

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/kinds/all"
)

// Rendered trees must not stay alive after the loader and render caches let
// them go (a global hash map once kept every saved tree, ~1 MiB each).
func TestRenderedTreesAreCollected(t *testing.T) {
	src := "../../seed/state"
	dst := t.TempDir()
	if err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	e := New(all.Registry())
	var freed atomic.Int32
	const saves = 120
	for i := 0; i < saves; i++ {
		f := filepath.Join(dst, "kb", "scratch.md")
		if err := os.WriteFile(f, []byte(fmt.Sprintf("# save %d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(f, time.Now(), time.Now().Add(time.Duration(i)*time.Second))
		r, err := e.Load(Source{Dir: dst})
		if err != nil {
			t.Fatal(err)
		}
		runtime.SetFinalizer(r, func(any) { freed.Add(1) })
		e.RenderRepo(context.Background(), r, "defiant")
	}
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	// The loader keeps up to 64 trees; the rest must be collected.
	if got := freed.Load(); got < saves-64-8 {
		t.Errorf("only %d of %d parsed trees were collected", got, saves)
	}
}

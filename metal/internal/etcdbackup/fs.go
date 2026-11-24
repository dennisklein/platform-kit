package etcdbackup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// FileSink keeps objects as files under a directory: the manager's backup
// volume (config/etcd-backup mounts a PersistentVolumeClaim there).
type FileSink struct {
	// Root is the directory objects live under.
	Root string
	// Name is how URLs name the volume (the claim's name).
	Name string
}

// path maps a key to a file under Root, refusing anything that would leave
// it: keys are built from object names, but a sink must not trust that.
func (s FileSink) path(key string) (string, error) {
	clean := path.Clean("/" + key)[1:]
	if key == "" || clean != key || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return "", fmt.Errorf("backup key %q is not a clean relative path", key)
	}
	return filepath.Join(s.Root, filepath.FromSlash(key)), nil
}

// Put writes to a temporary file beside the target and renames it into
// place once synced, so a crash leaves a stray .partial file, never a short
// object under the real name.
func (s FileSink) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	dst, err := s.path(key)
	if err != nil {
		return 0, err
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("backup volume: %w", err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".partial-*")
	if err != nil {
		return 0, fmt.Errorf("backup volume: %w", err)
	}
	tmp := f.Name()
	done := false
	defer func() {
		if !done {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	n, err := io.Copy(f, r)
	if err != nil {
		return n, fmt.Errorf("write %s: %w", s.URL(key), err)
	}
	if err := f.Sync(); err != nil {
		return n, fmt.Errorf("sync %s: %w", s.URL(key), err)
	}
	if err := f.Close(); err != nil {
		return n, fmt.Errorf("close %s: %w", s.URL(key), err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return n, fmt.Errorf("store %s: %w", s.URL(key), err)
	}
	done = true
	// The rename is durable only once the directory is synced too.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return n, nil
}

func (s FileSink) Open(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", s.URL(key), ErrNotFound)
	}
	return f, err
}

func (s FileSink) Delete(_ context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete %s: %w", s.URL(key), err)
	}
	return nil
}

// List walks the directory a prefix names; partial files are not objects.
func (s FileSink) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	err := filepath.WalkDir(s.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(s.Root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, Object{Key: key, Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})
	return out, err
}

func (s FileSink) URL(key string) string { return "pvc://" + s.Name + "/" + key }

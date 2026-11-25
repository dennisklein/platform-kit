package asset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Store is the media server's disk: the place for boot artifacts too large for
// a Secret. An installer with a node's config embedded is several hundred
// megabytes, and a Secret holds one. The store is a directory on a volume that
// only the operator's namespace mounts (the manager, and the build Jobs it
// starts); it holds secrets in the same sense as the config Secrets do, and
// the files are readable by their owner only.
//
// Layout:
//
//	builds/<job>/          what one build Job writes (the imager's output directory)
//	assets/<asset>/<key>.iso  the artifact of a BootAsset, one per build key:
//	                       an imported build, or an image the manager downloaded
//	                       (Fetch)
//
// A build writes into its own directory and the manager imports the result
// with a rename, so an artifact is either absent or complete, never half
// written. Files are never rewritten in place: a new build is a new key.
type Store struct {
	Dir string
}

const (
	storeBuilds = "builds"
	storeAssets = "assets"
	// storeMode keeps the directories to their owner: the manager and the
	// build Jobs run as the same user.
	storeMode = 0o700
	// artifactMode makes an imported artifact owner-read-only.
	artifactMode = 0o400
)

// NewStore opens the store rooted at dir, creating its directories. It fails
// when dir cannot be written, so a misconfigured volume shows at start, not at
// the first build.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("media store: no directory")
	}
	s := &Store{Dir: dir}
	for _, d := range []string{storeBuilds, storeAssets} {
		if err := os.MkdirAll(filepath.Join(dir, d), storeMode); err != nil {
			return nil, fmt.Errorf("media store %s: %w", dir, err)
		}
	}
	probe, err := os.CreateTemp(filepath.Join(dir, storeBuilds), ".probe-")
	if err != nil {
		return nil, fmt.Errorf("media store %s is not writable: %w", dir, err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	return s, nil
}

// BuildDir is where a build job writes its output, relative to the store root:
// a Job mounts the store's volume and passes this path to the builder.
func BuildDir(job string) string {
	return filepath.Join(storeBuilds, job)
}

// PrepareBuild creates an empty, owner-only output directory for a build.
// Whatever an earlier attempt of the same job left there goes first.
func (s *Store) PrepareBuild(job string) error {
	if err := checkElem(job); err != nil {
		return err
	}
	dir := filepath.Join(s.Dir, BuildDir(job))
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Mkdir(dir, storeMode)
}

// Import moves the one .iso a build job wrote into the asset's place for key,
// and reports its digest and size. The build directory goes with it. An
// artifact already imported for that key is kept, and its digest reported.
func (s *Store) Import(job, asset, key string) (string, int64, error) {
	for _, e := range []string{job, asset, key} {
		if err := checkElem(e); err != nil {
			return "", 0, err
		}
	}
	dst := s.path(asset, key)
	if _, err := os.Stat(dst); err == nil {
		_ = os.RemoveAll(filepath.Join(s.Dir, BuildDir(job)))
		return s.Digest(asset, key)
	}
	buildDir := filepath.Join(s.Dir, BuildDir(job))
	isos, err := filepath.Glob(filepath.Join(buildDir, "*.iso"))
	if err != nil {
		return "", 0, err
	}
	if len(isos) != 1 {
		return "", 0, fmt.Errorf("build %s left %d .iso files in %s, want exactly one", job, len(isos), BuildDir(job))
	}
	if err := os.MkdirAll(filepath.Dir(dst), storeMode); err != nil {
		return "", 0, err
	}
	if err := os.Chmod(isos[0], artifactMode); err != nil {
		return "", 0, err
	}
	// A rename within one volume: the artifact appears whole or not at all.
	if err := os.Rename(isos[0], dst); err != nil {
		return "", 0, err
	}
	if err := os.RemoveAll(buildDir); err != nil {
		return "", 0, err
	}
	return s.Digest(asset, key)
}

// MaxFetch bounds what Fetch writes: an install image is a few hundred
// megabytes, and a server that sends without end must not fill the volume.
const MaxFetch = 4 << 30

// Fetch downloads the image at url into the asset's place for key, and
// reports its digest and size. With want set, the bytes must have that
// digest (sha256:<hex>), or nothing is kept. The download goes to a
// temporary file next to the artifact and is renamed into place only when
// it is complete and checked, so an artifact is never half written. The
// caller's client decides whose certificates the download trusts.
func (s *Store) Fetch(ctx context.Context, hc *http.Client, url, asset, key, want string) (string, int64, error) {
	for _, e := range []string{asset, key} {
		if err := checkElem(e); err != nil {
			return "", 0, err
		}
	}
	dir := filepath.Join(s.Dir, storeAssets, asset)
	if err := os.MkdirAll(dir, storeMode); err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	// A dot name: Prune and Sweep pass over it only by asset, and a
	// leftover of a dead process is removed with its asset.
	tmp, err := os.CreateTemp(dir, ".fetch-*")
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op once renamed
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, MaxFetch+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return "", 0, fmt.Errorf("GET %s: %w", url, err)
	case n > MaxFetch:
		return "", 0, fmt.Errorf("GET %s: larger than %d bytes", url, int64(MaxFetch))
	case resp.ContentLength >= 0 && n != resp.ContentLength:
		return "", 0, fmt.Errorf("GET %s: %d of %d bytes", url, n, resp.ContentLength)
	}
	sum := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if want != "" && sum != want {
		return "", 0, fmt.Errorf("GET %s: the image is %s, want %s", url, sum, want)
	}
	if err := os.Chmod(tmp.Name(), artifactMode); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp.Name(), s.path(asset, key)); err != nil {
		return "", 0, err
	}
	return sum, n, nil
}

// Has reports whether an artifact for the asset's key is in the store.
func (s *Store) Has(asset, key string) bool {
	if checkElem(asset) != nil || checkElem(key) != nil {
		return false
	}
	st, err := os.Stat(s.path(asset, key))
	return err == nil && st.Mode().IsRegular()
}

// Remove deletes the artifact for the asset's key, if there is one. It is how
// a file that is not what was built leaves the store before it is built
// again.
func (s *Store) Remove(asset, key string) error {
	for _, e := range []string{asset, key} {
		if err := checkElem(e); err != nil {
			return err
		}
	}
	if err := os.Remove(s.path(asset, key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Digest hashes the artifact: sha256:<hex> and its size.
func (s *Store) Digest(asset, key string) (string, int64, error) {
	f, err := s.open(asset, key)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	return digestOf(f)
}

// Open opens the artifact for serving and checks it against the digest the
// operation was approved with. The file is read once to hash it; the caller
// gets it back rewound. A mismatch closes it and fails: those are bytes
// nobody approved.
func (s *Store) Open(asset, key, pinned string) (*os.File, error) {
	f, err := s.open(asset, key)
	if err != nil {
		return nil, err
	}
	got, _, err := digestOf(f)
	if err == nil && pinned != "" && got != pinned {
		err = fmt.Errorf("artifact %s/%s is %s, approved %s", asset, key, got, pinned)
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Prune removes the asset's artifacts other than keep. A grant serving one of
// them keeps its open file until it ends; the name is gone at once.
func (s *Store) Prune(asset, keep string) error {
	if err := checkElem(asset); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(s.Dir, storeAssets, asset))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.Name() == keep+".iso" {
			continue
		}
		errs = append(errs, os.RemoveAll(filepath.Join(s.Dir, storeAssets, asset, e.Name())))
	}
	return errors.Join(errs...)
}

// RemoveAsset removes every artifact of the asset and its directory. A
// grant serving one of them keeps its open file until it ends.
func (s *Store) RemoveAsset(asset string) error {
	if err := checkElem(asset); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(s.Dir, storeAssets, asset))
}

// Sweep removes what nothing claims any more: the artifacts of assets for
// which keepAsset says no, and the build directories of jobs for which
// keepBuild says no. It is how artifacts of deleted BootAssets and the output
// of deleted or failed builds leave the disk.
func (s *Store) Sweep(keepAsset, keepBuild func(name string) bool) error {
	var errs []error
	for _, area := range []struct {
		dir  string
		keep func(string) bool
	}{{storeAssets, keepAsset}, {storeBuilds, keepBuild}} {
		entries, err := os.ReadDir(filepath.Join(s.Dir, area.dir))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") || area.keep(e.Name()) {
				continue
			}
			errs = append(errs, os.RemoveAll(filepath.Join(s.Dir, area.dir, e.Name())))
		}
	}
	return errors.Join(errs...)
}

func (s *Store) path(asset, key string) string {
	return filepath.Join(s.Dir, storeAssets, asset, key+".iso")
}

func (s *Store) open(asset, key string) (*os.File, error) {
	for _, e := range []string{asset, key} {
		if err := checkElem(e); err != nil {
			return nil, err
		}
	}
	return os.Open(s.path(asset, key))
}

func digestOf(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, nil
}

// checkElem refuses a name that is not one plain path element. Names come
// from Kubernetes object names, which are safe already; the check keeps a
// wrong caller from reaching outside the store.
func checkElem(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("media store: %q is not a plain name", name)
	}
	return nil
}

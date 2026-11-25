package sim

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// Virtual media: a slot holds an image URL, the BMC probes the URL at mount
// time and reads the image in full when the host boots. Small images are held
// in memory — that is the granted, secret-bearing config medium, which must
// never touch a disk. Large public images go through the MediaCache.

type mountedMedia struct {
	imageURL string                // as the BMC stores it — userinfo intact on the vmm dialect
	creds    *bmc.MediaCredentials // fetch credentials, whichever way they arrived
	size     int64                 // Content-Length at mount time; -1 when the server named none
	image    []byte                // the full pull a power-on performs, when held in memory
	path     string                // the same pull when it went to the cache instead
}

// mediaTransport skips certificate verification, mirroring the deployed
// posture: neither captured vendor can be given a trust anchor for media, and
// VerifyCertificate=false is the specification's default.
var mediaTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the observed BMC posture
	ResponseHeaderTimeout: 30 * time.Second,
}

// mediaProbe makes the mount-time requests, which must answer promptly;
// mediaPull reads whole images and is bounded by its context alone — an
// installer image takes as long as the link allows.
var (
	mediaProbe = &http.Client{Timeout: 10 * time.Second, Transport: mediaTransport}
	mediaPull  = &http.Client{Transport: mediaTransport}
)

func (s *State) Media(context.Context) ([]bmc.VirtualMedia, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return nil, err
	}
	var out []bmc.VirtualMedia
	for _, vm := range s.f.VirtualMedia {
		m := bmc.VirtualMedia{
			ID:             vm.ID,
			MediaTypes:     vm.MediaTypes,
			InsertStandard: vm.InsertStandard,
			OemActions:     vm.OemActions,
		}
		if mm := s.media[vm.ID]; mm != nil {
			m.Inserted, m.ImageURL = true, mm.imageURL
		}
		out = append(out, m)
	}
	return out, nil
}

// InsertMedia validates the URL, then performs the mount-time fetch a real BMC
// performs: every dialect HEADs the image, the standard dialect additionally
// sniffs the first sector. Nobody reads the full content before boot — that
// pull happens at power-on.
func (s *State) InsertMedia(ctx context.Context, mediaID, imageURL string, creds *bmc.MediaCredentials) error {
	s.mu.Lock()
	dialect, err := s.insertable(mediaID)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	u, err := url.Parse(imageURL)
	if err != nil {
		return refuse("media %q: unparsable image url", mediaID)
	}
	if u.Scheme != "https" {
		// Plain http is refused before dialing; https-only is the posture.
		return refuse("media %q: image url must be https", mediaID)
	}
	if u.RawQuery != "" || !strings.HasSuffix(u.Path, ".iso") {
		return refuse("media %q: incorrect file format", mediaID)
	}
	switch {
	case dialect == bmc.DialectStandard && u.User != nil:
		return refuse("media %q: userinfo rejected at the Image schema", mediaID)
	case dialect == bmc.DialectVmmControl && u.User != nil:
		pw, _ := u.User.Password()
		creds = &bmc.MediaCredentials{Username: u.User.Username(), Password: pw}
	}

	size, err := fetchMount(ctx, u, creds, dialect)
	if err != nil {
		// The observed iBMC failure shape for an unfetchable image. The
		// image goes back in MessageArgs as it was given, userinfo and all,
		// as Redfish services commonly echo an offending value: whoever
		// records this error must mask it (internal/redact).
		e := refuse("media %q: failed to connect to virtual media: %v", mediaID, err)
		e.Args = []string{imageURL}
		return e
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.insertable(mediaID); err != nil { // raced with another insert
		return err
	}
	s.media[mediaID] = &mountedMedia{imageURL: imageURL, creds: creds, size: size}
	return nil
}

// insertable checks slot existence, dialect, and vacancy under the caller's lock.
func (s *State) insertable(mediaID string) (bmc.MediaDialect, error) {
	if err := s.reachable(); err != nil {
		return "", err
	}
	for i, vm := range s.f.VirtualMedia {
		if vm.ID != mediaID {
			continue
		}
		if d := s.f.dialect(i); d != bmc.DialectNone {
			if s.media[mediaID] != nil {
				return "", refuse("media %q already inserted", mediaID)
			}
			return d, nil
		}
		return "", refuse("media %q advertises no insert mechanism", mediaID)
	}
	return "", notFound("VirtualMedia", mediaID)
}

func (s *State) EjectMedia(_ context.Context, mediaID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	if s.media[mediaID] == nil {
		return refuse("media %q not inserted", mediaID)
	}
	delete(s.media, mediaID)
	return nil
}

// BootImage reports the bytes the last power-on pulled from a mounted medium —
// the sim's evidence of what the node booted. Nil until a boot happened, and
// for an image that went to the media cache (see BootImagePath).
func (s *State) BootImage(mediaID string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mm := s.media[mediaID]; mm != nil {
		return mm.image
	}
	return nil
}

// BootImagePath reports the cache file the last power-on resolved a mounted
// medium to; empty when the image was pulled into memory or not at all.
func (s *State) BootImagePath(mediaID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mm := s.media[mediaID]; mm != nil {
		return mm.path
	}
	return ""
}

// SetMediaCache gives the BMC an on-disk store for large public images. Wire
// before serving; several states may share one cache.
func (s *State) SetMediaCache(c *MediaCache) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache = c
}

// fetchMount is the mount-time probe; it reports the image size the server
// declared.
func fetchMount(ctx context.Context, u *url.URL, creds *bmc.MediaCredentials, dialect bmc.MediaDialect) (int64, error) {
	resp, err := mediaRequest(ctx, mediaProbe, http.MethodHead, u, creds, "")
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	if dialect == bmc.DialectStandard {
		sniff, err := mediaRequest(ctx, mediaProbe, http.MethodGet, u, creds, "bytes=0-511")
		if err != nil {
			return 0, err
		}
		_, _ = io.Copy(io.Discard, sniff.Body)
		_ = sniff.Body.Close()
	}
	return resp.ContentLength, nil
}

// mediaRequest is one media request; the credentials travel as Basic auth
// (which is how both credential forms arrive at the asset server), never in
// the dialed URL. A non-2xx answer is an error; on success the caller owns the
// body.
func mediaRequest(ctx context.Context, hc *http.Client, method string, u *url.URL, creds *bmc.MediaCredentials, rangeHeader string) (*http.Response, error) {
	clean := *u
	clean.User = nil
	req, err := http.NewRequestWithContext(ctx, method, clean.String(), nil)
	if err != nil {
		return nil, err
	}
	if creds != nil {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp, nil
}

// bootFetch is the boot-time pull of one mounted medium. A failed pull leaves
// the medium without content — the node could not read it and does not boot
// from it.
func (s *State) bootFetch(ctx context.Context, mm *mountedMedia) {
	s.mu.Lock()
	raw, creds, size, cache := mm.imageURL, mm.creds, mm.size, s.cache
	s.mu.Unlock()
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	var image []byte
	var path string
	if cache.caches(creds, size) {
		// The cache fill outlives the request that triggered it: a caller
		// that gives up on a long first download must not throw it away.
		path, err = cache.fetch(context.WithoutCancel(ctx), u, size)
	} else {
		var resp *http.Response
		if resp, err = mediaRequest(ctx, mediaPull, http.MethodGet, u, creds, ""); err == nil {
			image, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		mm.image, mm.path = nil, ""
		return
	}
	mm.image, mm.path = image, path
}

// MediaCache keeps large public images on disk so that repeated boots, and
// several machines booting the same installer, download it once. An entry is
// keyed by the digest of the image URL plus the size the server declares: a
// changed image behind the same URL has a different length and misses.
//
// Only images mounted WITHOUT credentials are cached. Credentials mark a grant
// — the per-node config medium with its secrets — and that stays in memory.
type MediaCache struct {
	dir string
	// MinSize is the smallest image worth a file (default 8 MiB).
	MinSize int64

	mu      sync.Mutex
	filling map[string]*sync.Mutex // one fill per entry at a time
}

// NewMediaCache returns a cache rooted at dir; the directory is created on the
// first fill.
func NewMediaCache(dir string) *MediaCache {
	return &MediaCache{dir: dir, MinSize: 8 << 20, filling: map[string]*sync.Mutex{}}
}

// caches decides whether an image goes through the cache. A nil cache caches
// nothing.
func (c *MediaCache) caches(creds *bmc.MediaCredentials, size int64) bool {
	return c != nil && creds == nil && size >= c.MinSize
}

// entry is the cache file for a URL and size. The token of a grant URL is
// left out of the key: the operator serves a factory installer under a new
// grant for each mount, and the same installer must hit the same entry.
func (c *MediaCache) entry(u *url.URL, size int64) string {
	path := u.Path
	if rest, ok := strings.CutPrefix(path, "/grants/"); ok {
		if _, file, ok := strings.Cut(rest, "/"); ok {
			path = "/grants/" + file
		}
	}
	sum := sha256.Sum256([]byte(u.Scheme + "://" + u.Host + path))
	return filepath.Join(c.dir, fmt.Sprintf("%s-%d.iso", hex.EncodeToString(sum[:16]), size))
}

// fetch returns the cache file for the image, downloading it first when it is
// absent. The download lands in a temporary file and is renamed into place
// only when its length matches, so a torn transfer never becomes an entry.
func (c *MediaCache) fetch(ctx context.Context, u *url.URL, size int64) (string, error) {
	path := c.entry(u, size)
	c.mu.Lock()
	fill := c.filling[path]
	if fill == nil {
		fill = &sync.Mutex{}
		c.filling[path] = fill
	}
	c.mu.Unlock()
	fill.Lock()
	defer fill.Unlock()

	if st, err := os.Stat(path); err == nil && st.Size() == size {
		return path, nil
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return "", err
	}
	resp, err := mediaRequest(ctx, mediaPull, http.MethodGet, u, nil, "")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	tmp, err := os.CreateTemp(c.dir, "fill-*.part")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op once renamed
	n, err := io.Copy(tmp, resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("image is %d bytes, the server declared %d", n, size)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

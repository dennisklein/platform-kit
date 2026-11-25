package controller

import (
	"context"
	"net/http"
	"sync"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/redact"
)

// fetchTimeout bounds one download of an install image into the media
// store. The image factory builds an image on its first request, which can
// take minutes, and the image is a few hundred megabytes.
const fetchTimeout = 30 * time.Minute

// mediaFetcher downloads the images that the media service serves but
// nothing builds: the factory installer, and an image an operation names
// by URL and digest. A BMC never fetches those from their origin. The
// operator downloads them over TLS it verifies, keeps them in the media
// store, and serves them under a grant, pinned to their digest, like the
// media it builds.
//
// A download runs in the background, because it takes longer than a
// reconcile may block. The caller asks again until it is done: start
// starts one when none runs for the asset and key, and reports the result
// of a finished one once.
type mediaFetcher struct {
	mu   sync.Mutex
	jobs map[string]*fetchJob
}

type fetchJob struct {
	done bool
	sum  string
	size int64
	err  error
}

// fetchState is what start reports: a download runs, or it ended with the
// digest and size of the stored image or with an error.
type fetchState struct {
	running bool
	sum     string
	size    int64
	err     error
}

// start downloads url into the store under name and key unless that
// download runs already, and reports where it is. want, when set, is the
// digest the image must have. hc decides whose certificates count.
func (f *mediaFetcher) start(store *asset.Store, hc *http.Client, url, name, key, want string) fetchState {
	id := name + "/" + key
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jobs == nil {
		f.jobs = map[string]*fetchJob{}
	}
	if j, ok := f.jobs[id]; ok {
		if !j.done {
			return fetchState{running: true}
		}
		delete(f.jobs, id)
		return fetchState{sum: j.sum, size: j.size, err: j.err}
	}
	j := &fetchJob{}
	f.jobs[id] = j
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		log := logf.Log.WithName("media-fetch").WithValues("asset", name, "url", redact.String(url))
		log.Info("downloading an image into the media store")
		sum, size, err := store.Fetch(ctx, hc, url, name, key, want)
		if err != nil {
			err = redact.Error(err)
			log.Error(err, "download failed")
		} else {
			log.Info("downloaded an image into the media store", "digest", sum, "bytes", size)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		j.done, j.sum, j.size, j.err = true, sum, size, err
	}()
	return fetchState{running: true}
}

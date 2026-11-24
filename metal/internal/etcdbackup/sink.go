// Package etcdbackup takes etcd snapshots off a Talos cluster and keeps them
// somewhere else, encrypted: the stream from Talos' EtcdSnapshot is checked
// as it passes (Verifier), encrypted with age to public recipients (Encrypt)
// and written to a Sink, without the plaintext ever reaching a disk on the
// way. The operator holds no private key, so it can store a snapshot but
// never read one back; a person decrypts on their own machine, for a restore
// (pk-metal restore) or to inspect one (pk-metal backup decrypt).
//
// Two sinks exist. An S3-compatible bucket is the production one: off the
// cluster, so it survives the cluster. A directory on a volume mounted into
// the manager (FileSink, the "pvc" sink) is for a lab and for clusters other
// than the one the operator runs on: a backup kept on the very etcd it backs
// up is lost with it, which is why the controller refuses that combination.
package etcdbackup

import (
	"context"
	"errors"
	"io"
	"time"
)

// Object is one stored object as a sink lists it.
type Object struct {
	Key     string
	Size    int64
	ModTime time.Time
}

// ErrNotFound is what Open returns for a key the sink does not hold.
var ErrNotFound = errors.New("no such object in the backup sink")

// Sink stores snapshot ciphertexts under keys (slash-separated paths).
type Sink interface {
	// Put stores everything r yields under key and returns the bytes
	// stored. An object becomes visible under key only once complete: a
	// failed Put leaves nothing half-written behind under that key.
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	// Open reads an object back; ErrNotFound when there is none.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes an object; a missing object is success.
	Delete(ctx context.Context, key string) error
	// List lists the objects under a key prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
	// URL names where key lives, for status and for people
	// (s3://bucket/key, pvc://claim/key).
	URL(key string) string
}

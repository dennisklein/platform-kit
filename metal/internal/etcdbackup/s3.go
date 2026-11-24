package etcdbackup

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config says how to reach one bucket of an S3-compatible store (AWS S3,
// MinIO, Ceph RGW, Garage, ...).
type S3Config struct {
	// Endpoint is the store's URL: https://host[:port]. http:// is accepted
	// for a store on a trusted network (a lab); the ciphertext is encrypted
	// either way, but the credentials are not.
	Endpoint string
	Bucket   string
	// Region is the bucket's region; empty lets the client ask the store.
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	// CA, PEM, replaces the system roots for the endpoint's certificate.
	CA []byte
	// PathStyle forces path-style requests (https://host/bucket/key), which
	// most stores other than AWS need; the default asks the client to tell
	// from the endpoint.
	PathStyle bool
}

// partSize is the multipart chunk the client buffers in memory while the
// snapshot streams in: the size is not known up front. 16 MiB parts reach
// the S3 limit of 10000 parts at 160 GiB, far above etcd's 8 GiB quota.
const partSize = 16 << 20

// S3Sink stores objects in an S3-compatible bucket through minio-go.
type S3Sink struct {
	client *minio.Client
	bucket string
}

// NewS3Sink validates c and returns a sink for its bucket. It does not
// contact the store.
func NewS3Sink(c S3Config) (*S3Sink, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("S3 endpoint %q: want https://host[:port] with no path", c.Endpoint)
	}
	if c.Bucket == "" {
		return nil, errors.New("S3 sink: no bucket")
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return nil, errors.New("S3 sink: the credentials Secret lacks accessKeyID or secretAccessKey")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if len(c.CA) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(c.CA) {
			return nil, errors.New("S3 sink: the CA holds no PEM certificate")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	lookup := minio.BucketLookupAuto
	if c.PathStyle {
		lookup = minio.BucketLookupPath
	}
	cl, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(c.AccessKeyID, c.SecretAccessKey, ""),
		Secure:       u.Scheme == "https",
		Transport:    tr,
		Region:       c.Region,
		BucketLookup: lookup,
		// A failed upload is retried by the next scheduled snapshot, with
		// a fresh stream; retrying inside one would need the stream again.
		MaxRetries: 3,
	})
	if err != nil {
		return nil, fmt.Errorf("S3 sink: %w", err)
	}
	return &S3Sink{client: cl, bucket: c.Bucket}, nil
}

// Put streams r as a multipart upload. S3 makes an object visible only when
// the upload completes, and minio-go aborts the upload on error.
func (s *S3Sink) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	info, err := s.client.PutObject(ctx, s.bucket, key, r, -1, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
		PartSize:    partSize,
	})
	if err != nil {
		return info.Size, fmt.Errorf("upload %s: %w", s.URL(key), err)
	}
	return info.Size, nil
}

func (s *S3Sink) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err != nil {
		if minio.ToErrorResponse(err).StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%s: %w", s.URL(key), ErrNotFound)
		}
		return nil, fmt.Errorf("stat %s: %w", s.URL(key), err)
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", s.URL(key), err)
	}
	return obj, nil
}

func (s *S3Sink) Delete(ctx context.Context, key string) error {
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil && minio.ToErrorResponse(err).StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete %s: %w", s.URL(key), err)
	}
	return nil
}

func (s *S3Sink) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	for o := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", s.bucket, prefix, o.Err)
		}
		if strings.HasSuffix(o.Key, "/") {
			continue
		}
		out = append(out, Object{Key: o.Key, Size: o.Size, ModTime: o.LastModified})
	}
	return out, nil
}

func (s *S3Sink) URL(key string) string { return "s3://" + s.bucket + "/" + key }

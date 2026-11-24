package etcdbackup

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fakeS3 is the slice of the S3 API minio-go uses for a streamed upload, a
// download and a listing, held in memory. It checks no signatures: what is
// under test is the sink's use of the client, not S3.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte         // bucket/key
	uploads map[string]map[int][]byte // uploadID -> parts
	aborted int
	failPut bool // every part upload answers 500
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
}

func (f *fakeS3) object(path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[path]
	return b, ok
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)

	switch {
	case r.Method == http.MethodGet && key == "" && q.Get("list-type") == "2":
		type content struct {
			Key          string
			Size         int64
			LastModified string
			ETag         string
		}
		res := struct {
			XMLName     xml.Name `xml:"ListBucketResult"`
			Name        string
			Prefix      string
			IsTruncated bool
			Contents    []content
		}{Name: bucket, Prefix: q.Get("prefix")}
		for _, k := range slices.Sorted(func(yield func(string) bool) {
			for k := range f.objects {
				if !yield(k) {
					return
				}
			}
		}) {
			b, rest, _ := strings.Cut(k, "/")
			if b == bucket && strings.HasPrefix(rest, res.Prefix) {
				res.Contents = append(res.Contents, content{Key: rest, Size: int64(len(f.objects[k])),
					LastModified: time.Now().UTC().Format(time.RFC3339), ETag: `"x"`})
			}
		}
		_ = xml.NewEncoder(w).Encode(res)
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := strconv.Itoa(len(f.uploads) + 1)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`,
			bucket, key, id)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		if f.failPut {
			http.Error(w, "<Error><Code>InternalError</Code></Error>", http.StatusInternalServerError)
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.uploads[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"part%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		parts := f.uploads[q.Get("uploadId")]
		var all []byte
		for i := 1; i <= len(parts); i++ {
			all = append(all, parts[i]...)
		}
		f.objects[path] = all
		delete(f.uploads, q.Get("uploadId"))
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>"done"</ETag></CompleteMultipartUploadResult>`,
			bucket, key)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(f.uploads, q.Get("uploadId"))
		f.aborted++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		f.objects[path] = body
		w.Header().Set("ETag", `"single"`)
	case r.Method == http.MethodHead || r.Method == http.MethodGet:
		b, ok := f.objects[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			if r.Method == http.MethodGet {
				fmt.Fprint(w, `<Error><Code>NoSuchKey</Code></Error>`)
			}
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"x"`)
		if r.Method == http.MethodGet {
			_, _ = io.Copy(w, bytes.NewReader(b))
		}
	case r.Method == http.MethodDelete:
		delete(f.objects, path)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "not modelled", http.StatusNotImplemented)
	}
}

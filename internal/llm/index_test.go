package llm

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/gitx"
)

func TestTokenize(t *testing.T) {
	got := Tokenize("Why does cn-003 use port 37 on data-a? Peer 10.6.0.3/32, cilium.io/v2alpha1")
	want := []string{"cn-003", "cn", "003", "port", "37", "data-a", "data", "peer", "10.6.0.3/32", "10.6.0.3", "cilium.io/v2alpha1", "cilium", "io", "v2alpha1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func seedDocs(t *testing.T) []Document {
	t.Helper()
	docs, err := RepoDocs(filepath.Join("..", "..", "seed", "state"), NewRedactor())
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) < 20 {
		t.Fatalf("only %d docs", len(docs))
	}
	merge := gitx.LogEntry{
		SHA: "9f2c1e4b7a0d3c5e8f1a", Subject: "Merge change c-2hd7vw: Move cn-003 to data-a 37 (!2)",
		Body:       "free data-b ports for rack 6Z\n\nPk-Change: c-2hd7vw\nApproved-by: anna, dennis\nReviewed-in: !2",
		CommitDate: time.Date(2026, 9, 12, 10, 15, 0, 0, time.UTC), Committer: "jonas",
	}
	merge.Trailers = gitx.ParseTrailers(merge.Subject + "\n\n" + merge.Body)
	docs = append(docs, TrailerDocs([]gitx.LogEntry{merge}, nil)...)
	docs = append(docs, MRDoc(98, "c-p4x8ma", "BGP peers for org-a and org-b", "Peer rack 6Z", []string{"anna: lab fails, 10.6.0.3 not established"}, nil))
	return docs
}

func TestIndexBM25(t *testing.T) {
	docs := seedDocs(t)
	ix := NewIndex(func(context.Context) ([]Document, string, error) { return docs, "v1", nil }, NewMock(nil))
	ctx := context.Background()
	hits, err := ix.Search(ctx, "Why does cn-003 use port 37 on data-a?", 5)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Mode() != "bm25" || len(hits) == 0 {
		t.Fatalf("mode %s hits %d", ix.Mode(), len(hits))
	}
	found := false
	for _, h := range hits[:min(3, len(hits))] {
		if h.Kind == "trailer" && h.Ref == "c-2hd7vw" {
			found = true
		}
	}
	if !found {
		for _, h := range hits {
			t.Logf("%s %s %.3f %s", h.Kind, h.Ref, h.Score, h.Snippet)
		}
		t.Fatal("trailer c-2hd7vw not in the top 3")
	}
	hits, _ = ix.Search(ctx, "replace failed disk jbod", 3)
	if len(hits) == 0 || !strings.HasPrefix(hits[0].Ref, "kb/") {
		t.Fatalf("kb hit: %+v", hits)
	}
	hits, _ = ix.Search(ctx, "10.6.0.3 not established", 3)
	if len(hits) == 0 {
		t.Fatal("no hit for the lab failure")
	}
	// No secrets in the index.
	for _, c := range ix.chunks {
		if strings.Contains(c.text, "ENC[") {
			t.Fatalf("SOPS value indexed in %s", c.doc.Ref)
		}
	}
}

type fakeEmbedder struct{ m *Mock }

func (f *fakeEmbedder) Mock() bool { return false }
func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, s := range texts {
		s = strings.ToLower(s)
		out[i] = []float32{float32(strings.Count(s, "disk")), float32(strings.Count(s, "bgp")), 0.1}
	}
	return out, nil
}

func TestIndexEmbeddings(t *testing.T) {
	docs := seedDocs(t)
	ix := NewIndex(func(context.Context) ([]Document, string, error) { return docs, "v1", nil }, &fakeEmbedder{m: NewMock(nil)})
	if err := ix.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ix.Mode() != "embeddings" {
		t.Fatalf("mode %s", ix.Mode())
	}
	hits, _ := ix.Search(context.Background(), "bgp", 3)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
}

func TestIndexRebuildOnVersion(t *testing.T) {
	calls, version := 0, "alpha"
	ix := NewIndex(func(context.Context) ([]Document, string, error) {
		calls++
		return []Document{{Kind: "kb", Ref: "kb/x.md", Title: "x", Text: "version " + version}}, version, nil
	}, nil)
	ctx := context.Background()
	ix.Rebuild(ctx)
	ix.Rebuild(ctx)
	if n, _, v := ix.Stats(); n != 1 || v != "alpha" {
		t.Fatalf("stats %d %s", n, v)
	}
	version = "beta"
	ix.Rebuild(ctx)
	hits, _ := ix.Search(ctx, "beta", 1)
	if len(hits) != 1 || !strings.Contains(hits[0].Snippet, "version beta") {
		t.Fatalf("not rebuilt: %+v", hits)
	}
}

func (f *fakeEmbedder) Model() string { return "fake" }
func (f *fakeEmbedder) Complete(ctx context.Context, req Request) (*Response, error) {
	return f.m.Complete(ctx, req)
}
func (f *fakeEmbedder) Ping(context.Context) error { return nil }

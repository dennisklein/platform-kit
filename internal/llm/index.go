package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Collector returns the documents to index and a version key of the
// sources (for example the main head sha plus the PR fingerprint). Rebuild
// skips the work when the key did not change.
type Collector func(ctx context.Context) (docs []Document, version string, err error)

type knownKey struct{}

// KnownVersion returns the version the index holds, in the context of a
// Collector call ("" on a forced rebuild). A collector that computes the
// version cheaply can return it with no documents when it equals this one.
func KnownVersion(ctx context.Context) string {
	v, _ := ctx.Value(knownKey{}).(string)
	return v
}

// MemIndex is the retrieval index in memory: BM25 over chunks, plus cosine
// similarity of embeddings when the client has an embedding model (hybrid
// score). It is rebuilt from the collector, on start and when the sources
// change.
type MemIndex struct {
	collect Collector
	client  Client
	log     *slog.Logger

	mu        sync.RWMutex
	chunks    []chunk
	df        map[string]int
	avgLen    float64
	version   string
	builtAt   time.Time
	embedded  bool
	embedErr  string
	embCache  map[string][]float32 // content hash → vector
	rebuildMu sync.Mutex
}

type chunk struct {
	doc  Document
	text string
	tf   map[string]int
	n    int
	hash string
	vec  []float32
}

// NewIndex returns an empty index. client may be nil (BM25 only).
func NewIndex(collect Collector, client Client) *MemIndex {
	return &MemIndex{collect: collect, client: client, df: map[string]int{}, embCache: map[string][]float32{},
		log: slog.Default().With("component", "llm-index")}
}

// Mode is bm25 or embeddings.
func (ix *MemIndex) Mode() string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if ix.embedded {
		return "embeddings"
	}
	return "bm25"
}

// Stats returns the number of chunks, the build time and the source version.
func (ix *MemIndex) Stats() (chunks int, builtAt time.Time, version string) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.chunks), ix.builtAt, ix.version
}

// Rebuild re-reads all sources. It does nothing when the version is unchanged.
func (ix *MemIndex) Rebuild(ctx context.Context) error {
	return ix.rebuild(ctx, false)
}

// ForceRebuild re-reads and re-indexes all sources.
func (ix *MemIndex) ForceRebuild(ctx context.Context) error {
	return ix.rebuild(ctx, true)
}

func (ix *MemIndex) rebuild(ctx context.Context, force bool) error {
	ix.rebuildMu.Lock()
	defer ix.rebuildMu.Unlock()
	ix.mu.RLock()
	known := ix.version
	ix.mu.RUnlock()
	cctx := ctx
	if !force {
		cctx = context.WithValue(ctx, knownKey{}, known)
	}
	docs, version, err := ix.collect(cctx)
	if err != nil {
		return err
	}
	same := version != "" && version == known && !force
	if same {
		return nil
	}
	chunks := make([]chunk, 0, len(docs))
	df := map[string]int{}
	total := 0
	for _, d := range docs {
		for _, part := range splitDoc(d) {
			toks := Tokenize(d.Title + "\n" + d.Title + "\n" + part) // the title counts twice
			if len(toks) == 0 {
				continue
			}
			c := chunk{doc: d, text: part, tf: map[string]int{}, n: len(toks)}
			for _, t := range toks {
				c.tf[t]++
			}
			for t := range c.tf {
				df[t]++
			}
			h := sha256.Sum256([]byte(d.Title + "\x00" + part))
			c.hash = hex.EncodeToString(h[:8])
			total += len(toks)
			chunks = append(chunks, c)
		}
	}
	embedded, embErr := ix.embed(ctx, chunks)
	ix.mu.Lock()
	ix.chunks, ix.df, ix.version, ix.builtAt = chunks, df, version, time.Now()
	ix.avgLen = 1
	if len(chunks) > 0 {
		ix.avgLen = float64(total) / float64(len(chunks))
	}
	ix.embedded, ix.embedErr = embedded, embErr
	ix.mu.Unlock()
	ix.log.Debug("index rebuilt", "docs", len(docs), "chunks", len(chunks), "mode", ix.Mode(), "version", version)
	return nil
}

// embed fills chunk vectors (cached by content hash). It reports whether all
// chunks have vectors.
func (ix *MemIndex) embed(ctx context.Context, chunks []chunk) (bool, string) {
	if ix.client == nil || ix.client.Mock() || len(chunks) == 0 {
		return false, ""
	}
	var todo []int
	for i := range chunks {
		if v, ok := ix.embCache[chunks[i].hash]; ok {
			chunks[i].vec = v
		} else {
			todo = append(todo, i)
		}
	}
	const batch = 32
	for start := 0; start < len(todo); start += batch {
		end := min(start+batch, len(todo))
		texts := make([]string, 0, end-start)
		for _, i := range todo[start:end] {
			texts = append(texts, truncate(chunks[i].doc.Title+"\n"+chunks[i].text, 6000))
		}
		vecs, err := ix.client.Embed(ctx, texts)
		if err != nil {
			if err != ErrNoEmbeddings {
				ix.log.Warn("embeddings failed, using BM25", "err", err)
			}
			return false, err.Error()
		}
		for k, i := range todo[start:end] {
			chunks[i].vec = vecs[k]
			ix.embCache[chunks[i].hash] = vecs[k]
		}
	}
	return true, ""
}

// Watch rebuilds the index every interval when the sources changed, until
// ctx ends.
func (ix *MemIndex) Watch(ctx context.Context, every time.Duration) {
	if err := ix.Rebuild(ctx); err != nil {
		ix.log.Warn("index build", "err", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := ix.Rebuild(ctx); err != nil && ctx.Err() == nil {
				ix.log.Debug("index rebuild", "err", err)
			}
		}
	}
}

// Search returns the best hits for q.
func (ix *MemIndex) Search(ctx context.Context, q string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 10
	}
	ix.mu.RLock()
	empty := len(ix.chunks) == 0 && ix.builtAt.IsZero()
	ix.mu.RUnlock()
	if empty {
		if err := ix.Rebuild(ctx); err != nil {
			return nil, err
		}
	}
	terms := Tokenize(q)
	ix.mu.RLock()
	chunks, df, avg, embedded := ix.chunks, ix.df, ix.avgLen, ix.embedded
	ix.mu.RUnlock()
	if len(chunks) == 0 {
		return []Hit{}, nil
	}

	scores := make([]float64, len(chunks))
	maxBM := 0.0
	if len(terms) > 0 {
		const k1, b = 1.2, 0.75
		n := float64(len(chunks))
		uniq := map[string]bool{}
		for _, t := range terms {
			uniq[t] = true
		}
		for t := range uniq {
			d := float64(df[t])
			if d == 0 {
				continue
			}
			idf := math.Log(1 + (n-d+0.5)/(d+0.5))
			for i := range chunks {
				f := float64(chunks[i].tf[t])
				if f == 0 {
					continue
				}
				scores[i] += idf * f * (k1 + 1) / (f + k1*(1-b+b*float64(chunks[i].n)/avg))
			}
		}
		for _, s := range scores {
			maxBM = max(maxBM, s)
		}
	}

	if embedded && ix.client != nil {
		if qv, err := ix.client.Embed(ctx, []string{q}); err == nil && len(qv) == 1 {
			for i := range chunks {
				cos := cosine(qv[0], chunks[i].vec)
				bm := 0.0
				if maxBM > 0 {
					bm = scores[i] / maxBM
				}
				// Hybrid: meaning from the embedding, exact ids (cn-003,
				// c-2hd7vw) from BM25.
				scores[i] = 0.6*cos + 0.4*bm
			}
		}
	}

	idx := make([]int, 0, len(chunks))
	for i, s := range scores {
		if s > 0 {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })
	hits := []Hit{}
	seen := map[string]bool{}
	for _, i := range idx {
		c := chunks[i]
		key := c.doc.Kind + "\x00" + c.doc.Ref
		if seen[key] {
			continue // one hit per document
		}
		seen[key] = true
		hits = append(hits, Hit{Document: c.doc, Snippet: Snippet(c.text, terms, 280), Score: math.Round(scores[i]*1000) / 1000})
		if len(hits) == limit {
			break
		}
	}
	return hits, nil
}

func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// splitDoc cuts long Markdown at headings and long other texts into windows.
func splitDoc(d Document) []string {
	const max = 2000
	text := strings.TrimSpace(d.Text)
	if len(text) <= max {
		return []string{text}
	}
	var parts []string
	if strings.HasSuffix(d.Path, ".md") || d.Kind == "kb" || d.Kind == "lab" {
		var cur strings.Builder
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "#") && cur.Len() > 200 {
				parts = append(parts, cur.String())
				cur.Reset()
			}
			cur.WriteString(line + "\n")
		}
		parts = append(parts, cur.String())
	} else {
		parts = []string{text}
	}
	var out []string
	for _, p := range parts {
		for len(p) > max {
			cut := strings.LastIndex(p[:max], "\n")
			if cut < max/2 {
				cut = max
			}
			out = append(out, p[:cut])
			p = p[cut:]
		}
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true,
	"does": true, "do": true, "for": true, "from": true, "has": true, "have": true, "how": true,
	"in": true, "is": true, "it": true, "its": true, "of": true, "on": true, "or": true, "that": true,
	"the": true, "this": true, "to": true, "was": true, "were": true, "what": true, "when": true,
	"where": true, "which": true, "who": true, "why": true, "will": true, "with": true, "use": true,
	"uses": true, "used": true, "can": true, "i": true, "we": true, "you": true, "me": true, "my": true,
}

// Tokenize splits text into lower-case search terms. Compound ids keep their
// whole form and their parts: "cn-003" gives cn-003, cn, 003; "data-a"
// gives data-a, data; IPs stay whole.
func Tokenize(s string) []string {
	s = strings.ToLower(s)
	var out []string
	emit := func(t string) {
		t = strings.Trim(t, ".-_/:")
		if t == "" || stopwords[t] {
			return
		}
		if len(t) == 1 && !unicode.IsDigit(rune(t[0])) {
			return
		}
		out = append(out, t)
	}
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' || r == '/')
	})
	for _, f := range fields {
		f = strings.Trim(f, ".-_/")
		if f == "" {
			continue
		}
		emit(f)
		if isNumericDotted(f) {
			// 10.6.0.3/32 also matches 10.6.0.3.
			if ip, _, ok := strings.Cut(f, "/"); ok {
				emit(ip)
			}
			continue
		}
		if strings.ContainsAny(f, "-_/.") {
			parts := strings.FieldsFunc(f, func(r rune) bool { return r == '-' || r == '_' || r == '/' || r == '.' })
			if len(parts) > 1 {
				for _, p := range parts {
					emit(p)
				}
			}
		}
	}
	return out
}

func isNumericDotted(s string) bool {
	for _, r := range s {
		if r != '.' && r != '/' && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// Snippet returns the part of text around the most query terms.
func Snippet(text string, terms []string, max int) string {
	lines := strings.Split(text, "\n")
	want := map[string]bool{}
	for _, t := range terms {
		want[t] = true
	}
	best, bestScore := 0, -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "" || strings.TrimSpace(l) == "---" {
			continue
		}
		score := 0
		for _, t := range Tokenize(l) {
			if want[t] {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	var b strings.Builder
	for i := best; i < len(lines) && b.Len() < max; i++ {
		l := strings.TrimSpace(lines[i])
		if l == "" || l == "---" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(" · ")
		}
		b.WriteString(l)
	}
	return truncate(b.String(), max)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

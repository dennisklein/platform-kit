package etcdbackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/etcdbackup/etcdbackuptest"
)

// fixture is a snapshot of a real bbolt database with etcd's buckets, written
// by bbolt itself (tx.WriteTo, as etcd streams it) with the sha256 trailer:
// the verifier's reading of bbolt's format is checked against bbolt, not
// only against etcdbackuptest's own idea of it.
func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/etcd-snapshot.db")
	require.NoError(t, err)
	return b
}

func verify(b []byte, chunk int) Report {
	v := NewVerifier()
	for len(b) > 0 {
		n := min(chunk, len(b))
		_, _ = v.Write(b[:n])
		b = b[n:]
	}
	return v.Report()
}

func TestVerifierAcceptsIntactSnapshots(t *testing.T) {
	for name, snap := range map[string][]byte{
		"bbolt fixture": fixture(t),
		"synthesized":   etcdbackuptest.Snapshot(7),
	} {
		// The chunking must not matter: the 32-byte lag and the head buffer
		// are the parts most likely to go wrong at a boundary.
		for _, chunk := range []int{1, 7, 31, 32, 33, 4096, 1 << 20} {
			r := verify(snap, chunk)
			require.NoError(t, r.Valid(), "%s in chunks of %d", name, chunk)
			assert.True(t, r.HasTrailer)
			assert.True(t, r.TrailerOK)
			assert.Equal(t, uint32(4096), r.PageSize)
			assert.Equal(t, int64(len(snap)), r.Size)
			sum := sha256.Sum256(snap)
			assert.Equal(t, hex.EncodeToString(sum[:]), r.SHA256)
		}
	}
	assert.Equal(t, uint64(8), verify(etcdbackuptest.Snapshot(7), 4096).TxID, "the newer meta page wins")
}

func TestVerifierRefusesDamagedSnapshots(t *testing.T) {
	good := fixture(t)
	flip := func(off int) []byte {
		b := bytes.Clone(good)
		b[off] ^= 0xff
		return b
	}
	bothMetas := bytes.Clone(good)
	bothMetas[16] ^= 0xff
	bothMetas[4096+16] ^= 0xff

	for name, tc := range map[string]struct {
		snap []byte
		want string
	}{
		"empty":                 {nil, "empty"},
		"cut short":             {good[:len(good)-100], "no integrity hash"},
		"no trailer":            {good[:len(good)-32], "no integrity hash"},
		"a byte changed":        {flip(5000), "does not match"},
		"trailer changed":       {flip(len(good) - 1), "does not match"},
		"not bbolt":             {withTrailer(bytes.Repeat([]byte{1}, 8192)), "not a bbolt database"},
		"both meta pages bad":   {withTrailer(bothMetas[:len(bothMetas)-32]), "not a bbolt database"},
		"pages counted missing": {withTrailer(good[:3*4096]), "more than"},
	} {
		err := verify(tc.snap, 1000).Valid()
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), tc.want, name)
	}

	// One damaged meta page is what bbolt survives too: the other is used.
	oneMeta := bytes.Clone(good[:len(good)-32])
	oneMeta[16] ^= 0xff
	require.NoError(t, verify(withTrailer(oneMeta), 512).Valid())
}

func withTrailer(db []byte) []byte {
	sum := sha256.Sum256(db)
	return append(bytes.Clone(db), sum[:]...)
}

func testIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	return id
}

func TestEncryptionRoundTrip(t *testing.T) {
	alice, bob := testIdentity(t), testIdentity(t)
	recipients, err := ParseRecipients([]string{alice.Recipient().String(), " " + bob.Recipient().String() + "\n"})
	require.NoError(t, err)

	snap := fixture(t)
	var cipher bytes.Buffer
	w, err := Encrypt(&cipher, recipients)
	require.NoError(t, err)
	_, err = w.Write(snap)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	assert.False(t, bytes.Contains(cipher.Bytes(), snap[:64]), "the ciphertext must not carry the plaintext")

	// Either recipient's identity decrypts it, and the plaintext verifies.
	for _, id := range []age.Identity{alice, bob} {
		var plain bytes.Buffer
		rep, err := Restore(bytes.NewReader(cipher.Bytes()), []age.Identity{id}, &plain)
		require.NoError(t, err)
		require.NoError(t, rep.Valid())
		assert.Equal(t, snap, plain.Bytes())
	}

	// Somebody else's identity does not.
	_, err = Restore(bytes.NewReader(cipher.Bytes()), []age.Identity{testIdentity(t)}, io.Discard)
	require.Error(t, err)

	// A changed ciphertext fails as it is read, before a restore can use it.
	bad := bytes.Clone(cipher.Bytes())
	bad[len(bad)-40] ^= 1
	_, err = Restore(bytes.NewReader(bad), []age.Identity{alice}, io.Discard)
	require.Error(t, err)
}

func TestParseRecipientsRefusesSecretsAndJunk(t *testing.T) {
	id := testIdentity(t)
	_, err := ParseRecipients([]string{id.String()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret key")
	assert.NotContains(t, err.Error(), id.String(), "a refused secret key is never echoed")

	_, err = ParseRecipients(nil)
	require.Error(t, err)
	_, err = ParseRecipients([]string{"ssh-ed25519 AAAA"})
	require.Error(t, err)

	ids, err := ParseIdentities(strings.NewReader("# created: now\n" + id.String() + "\n"))
	require.NoError(t, err)
	require.Len(t, ids, 1)
}

// sinkContract is what every sink must do.
func sinkContract(t *testing.T, s Sink) {
	t.Helper()
	ctx := context.Background()
	data := bytes.Repeat([]byte("ciphertext"), 3000)

	n, err := s.Put(ctx, "lab/one.db.age", bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), n)
	_, err = s.Put(ctx, "lab/two.db.age", strings.NewReader("2"))
	require.NoError(t, err)
	_, err = s.Put(ctx, "other/three.db.age", strings.NewReader("3"))
	require.NoError(t, err)

	size, sum, err := ReadBack(ctx, s, "lab/one.db.age")
	require.NoError(t, err)
	want := sha256.Sum256(data)
	assert.Equal(t, int64(len(data)), size)
	assert.Equal(t, hex.EncodeToString(want[:]), sum)

	objs, err := s.List(ctx, "lab/")
	require.NoError(t, err)
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	assert.ElementsMatch(t, []string{"lab/one.db.age", "lab/two.db.age"}, keys)

	require.NoError(t, s.Delete(ctx, "lab/one.db.age"))
	require.NoError(t, s.Delete(ctx, "lab/one.db.age"), "deleting what is gone is success")
	_, err = s.Open(ctx, "lab/one.db.age")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestFileSink(t *testing.T) {
	root := t.TempDir()
	s := FileSink{Root: root, Name: "backups"}
	sinkContract(t, s)
	assert.Equal(t, "pvc://backups/lab/x.db.age", s.URL("lab/x.db.age"))

	for _, key := range []string{"", "../escape", "/abs", "a/../../b", "a//b"} {
		_, err := s.Put(context.Background(), key, strings.NewReader("x"))
		require.Error(t, err, "key %q", key)
	}

	// A stream that fails leaves no object and no partial file behind.
	_, err := s.Put(context.Background(), "lab/broken.db.age", io.MultiReader(strings.NewReader("half"), failingReader{}))
	require.Error(t, err)
	entries, err := os.ReadDir(filepath.Join(root, "lab"))
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), "broken")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("stream broke") }

func newTestS3(t *testing.T) (*fakeS3, S3Config) {
	t.Helper()
	fake := newFakeS3()
	srv := httptest.NewTLSServer(fake)
	t.Cleanup(srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return fake, S3Config{
		Endpoint: srv.URL, Bucket: "backups", Region: "us-east-1",
		AccessKeyID: "test", SecretAccessKey: "test-secret", CA: ca, PathStyle: true,
	}
}

func TestS3Sink(t *testing.T) {
	_, cfg := newTestS3(t)
	s, err := NewS3Sink(cfg)
	require.NoError(t, err)
	sinkContract(t, s)
	assert.Equal(t, "s3://backups/lab/x.db.age", s.URL("lab/x.db.age"))

	for name, bad := range map[string]S3Config{
		"no scheme":      {Endpoint: "minio:9000", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"},
		"a path":         {Endpoint: "https://minio:9000/x", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"},
		"no bucket":      {Endpoint: "https://minio:9000", AccessKeyID: "a", SecretAccessKey: "s"},
		"no credentials": {Endpoint: "https://minio:9000", Bucket: "b"},
		"junk CA":        {Endpoint: "https://minio:9000", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", CA: []byte("x")},
	} {
		_, err := NewS3Sink(bad)
		require.Error(t, err, name)
	}
}

func snapshotOf(b []byte) SnapshotFunc {
	return func(_ context.Context, w io.Writer) (int64, error) {
		return io.Copy(w, bytes.NewReader(b))
	}
}

func TestStoreStreamsEncryptedVerifiedSnapshots(t *testing.T) {
	id := testIdentity(t)
	recipients, err := ParseRecipients([]string{id.Recipient().String()})
	require.NoError(t, err)
	snap := fixture(t)

	fake, cfg := newTestS3(t)
	s3, err := NewS3Sink(cfg)
	require.NoError(t, err)

	for name, sink := range map[string]Sink{"pvc": FileSink{Root: t.TempDir(), Name: "b"}, "s3": s3} {
		ctx := context.Background()
		st, err := Store(ctx, sink, "lab/snap.db.age", recipients, snapshotOf(snap))
		require.NoError(t, err, name)
		require.NoError(t, st.Plain.Valid(), name)

		size, sum, err := ReadBack(ctx, sink, "lab/snap.db.age")
		require.NoError(t, err, name)
		assert.Equal(t, st.CipherSize, size, name)
		assert.Equal(t, st.CipherSHA256, sum, name)

		rc, err := sink.Open(ctx, "lab/snap.db.age")
		require.NoError(t, err, name)
		var plain bytes.Buffer
		rep, err := Restore(rc, []age.Identity{id}, &plain)
		_ = rc.Close()
		require.NoError(t, err, name)
		assert.Equal(t, st.Plain.SHA256, rep.SHA256, name)
		assert.Equal(t, snap, plain.Bytes(), name)
	}

	// A snapshot stream that breaks halfway leaves nothing in the sink.
	broken := func(_ context.Context, w io.Writer) (int64, error) {
		n, _ := w.Write(snap[:5000])
		return int64(n), errors.New("connection reset")
	}
	fs := FileSink{Root: t.TempDir(), Name: "b"}
	_, err = Store(context.Background(), fs, "lab/broken.db.age", recipients, broken)
	require.ErrorContains(t, err, "connection reset")
	objs, err := fs.List(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, objs)

	_, err = Store(context.Background(), s3, "lab/broken.db.age", recipients, broken)
	require.Error(t, err)
	_, ok := fake.object("backups/lab/broken.db.age")
	assert.False(t, ok, "no object for a broken stream")

	// A sink that refuses the upload fails the store, and the snapshot
	// stream is not left hanging on a full pipe.
	fake.failPut = true
	done := make(chan error, 1)
	go func() {
		_, err := Store(context.Background(), s3, "lab/refused.db.age", recipients, snapshotOf(bytes.Repeat(snap, 1000)))
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Minute):
		t.Fatal("Store hung on a sink that refused the upload")
	}
}

func TestPrune(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	recs := []Record{
		{"v1", at(1), Verified}, {"v2", at(2), Verified}, {"v3", at(3), Verified},
		{"v4", at(30), Verified}, {"f1", at(4), Failed}, {"f2", at(5), Failed},
		{"f3", at(6), Failed}, {"run", at(100), InProgress},
	}
	assert.Equal(t, []string{"v4", "f3", "v3"}, Prune(recs, Retention{Count: 2, KeepFailed: 2}, now))
	assert.Equal(t, []string{"v4"}, Prune(recs, Retention{Count: 10, MaxAge: 24 * time.Hour, KeepFailed: 3}, now))
	assert.Empty(t, Prune(recs, Retention{KeepFailed: 3}, now), "no limits prune nothing")

	// The newest verified snapshot stays even when it is too old.
	old := []Record{{"v", at(1000), Verified}, {"f", at(1), Failed}}
	assert.Empty(t, Prune(old, Retention{Count: 1, MaxAge: time.Hour, KeepFailed: 1}, now))
}

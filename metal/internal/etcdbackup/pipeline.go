package etcdbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"filippo.io/age"
)

// Stored is what one snapshot became.
type Stored struct {
	// Plain is the verification of the plaintext stream.
	Plain Report
	// CipherSize and CipherSHA256 (hex) describe the object in the sink.
	CipherSize   int64
	CipherSHA256 string
}

// SnapshotFunc streams a snapshot into w: talos.Client.EtcdSnapshot.
type SnapshotFunc func(ctx context.Context, w io.Writer) (int64, error)

// Store streams one snapshot through the verifier and age encryption into
// the sink under key. The plaintext exists only in the pipe's buffers. On
// any failure the partial object is deleted (best effort) and the error
// returned; the Stored value is filled as far as the stream got.
func Store(ctx context.Context, sink Sink, key string, recipients []age.Recipient, snapshot SnapshotFunc) (Stored, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr, pw := io.Pipe()
	type putResult struct {
		n   int64
		err error
	}
	put := make(chan putResult, 1)
	go func() {
		n, err := sink.Put(ctx, key, pr)
		// Unblock the writer side when the sink gives up early.
		_ = pr.CloseWithError(cmpErr(err, io.ErrClosedPipe))
		put <- putResult{n, err}
	}()

	cipherHash := sha256.New()
	cipher := &counter{w: io.MultiWriter(pw, cipherHash)}
	verifier := NewVerifier()
	streamErr := func() error {
		enc, err := Encrypt(cipher, recipients)
		if err != nil {
			return err
		}
		if _, err := snapshot(ctx, io.MultiWriter(verifier, enc)); err != nil {
			return fmt.Errorf("etcd snapshot: %w", err)
		}
		if err := enc.Close(); err != nil {
			return fmt.Errorf("age encrypt: %w", err)
		}
		return nil
	}()
	// nil closes with EOF, and the sink commits; an error makes the sink's
	// read fail, and it abandons the object (an S3 multipart upload is
	// aborted with the still-live context).
	_ = pw.CloseWithError(streamErr)
	res := <-put

	out := Stored{Plain: verifier.Report(), CipherSize: cipher.n, CipherSHA256: hex.EncodeToString(cipherHash.Sum(nil))}
	err := streamErr
	if err == nil && res.err != nil {
		err = res.err
	}
	if err == nil && res.n != cipher.n {
		err = fmt.Errorf("the sink stored %d bytes of %d", res.n, cipher.n)
	}
	if err != nil {
		// A fresh context: the stream's may be cancelled already.
		_ = sink.Delete(context.WithoutCancel(ctx), key)
		return out, err
	}
	return out, nil
}

// ReadBack hashes the object stored under key, the check that the sink
// holds what was sent. Only the ciphertext is read: the operator cannot
// decrypt it.
func ReadBack(ctx context.Context, sink Sink, key string) (size int64, sha string, err error) {
	rc, err := sink.Open(ctx, key)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return n, "", fmt.Errorf("read back %s: %w", sink.URL(key), err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// Restore decrypts the object under key (or any ciphertext r) and copies
// the plaintext into w, verifying it on the way; it runs on an admin's
// machine. The returned Report says whether the plaintext is an intact etcd
// snapshot; the caller compares Report.SHA256 with the recorded one.
func Restore(r io.Reader, identities []age.Identity, w io.Writer) (Report, error) {
	plain, err := Decrypt(r, identities)
	if err != nil {
		return Report{}, err
	}
	v := NewVerifier()
	if _, err := io.Copy(io.MultiWriter(v, w), plain); err != nil {
		return v.Report(), fmt.Errorf("decrypt: %w", err)
	}
	return v.Report(), nil
}

type counter struct {
	w io.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func cmpErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

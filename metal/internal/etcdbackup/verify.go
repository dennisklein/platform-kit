package etcdbackup

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
)

// The etcd snapshot format, as far as verification reads it.
//
// etcd's Snapshot RPC (which Talos' EtcdSnapshot relays) streams the bbolt
// database file and then the sha256 of those bytes, 32 bytes. etcdutl
// recognises the trailer by the total size: a bbolt file is a whole number
// of pages, so a size that leaves 32 over a multiple of 512 carries one.
// Talos' recover checks the same trailer unless told to skip it.
//
// A bbolt file starts with two meta pages (page 0 and page 1). Each holds a
// magic number, the format version, the page size, the high-water page id,
// the transaction id and an FNV-1a checksum over the fields before it; bbolt
// opens the file from the valid meta page with the higher transaction id.
// Both live in the first two pages, so a streaming check needs to keep only
// those, never the whole database: the plaintext holds every Secret of the
// cluster, and this check runs inside the operator, which must not write it
// to disk.
const (
	trailerSize = sha256.Size
	// boltMagic and boltVersion are bbolt's file magic and format version.
	boltMagic   = 0xED0CDAED
	boltVersion = 2
	// boltMetaFlag is the page flag of a meta page.
	boltMetaFlag = 0x04
	// pageHeaderSize is bbolt's page header: id u64, flags u16, count
	// u16, overflow u32.
	pageHeaderSize = 16
	// metaSize is the meta struct up to and including its checksum.
	metaSize = 64
	// maxPageSize bounds what is buffered before the second meta page can
	// be read. bbolt uses the OS page size; 64 KiB covers every platform
	// etcd runs on.
	maxPageSize = 64 << 10
)

// Report is what verification established about one snapshot stream.
type Report struct {
	// Size is the whole stream's length in bytes, trailer included: what
	// was uploaded, and what a restore downloads.
	Size int64
	// SHA256 is the hex sha256 of the whole stream, trailer included.
	SHA256 string
	// HasTrailer: the stream ends in etcd's integrity hash.
	HasTrailer bool
	// TrailerOK: that hash equals the sha256 of the bytes before it.
	TrailerOK bool
	// PageSize, PageCount and TxID come from the newest valid bbolt meta
	// page. PageCount is bbolt's high-water mark, the pages in use.
	PageSize  uint32
	PageCount uint64
	TxID      uint64
}

// Valid reports whether the stream is an intact etcd snapshot: a bbolt file
// with a valid meta page, whole pages, and etcd's integrity hash matching.
// The error says what is wrong.
func (r Report) Valid() error {
	switch {
	case r.Size == 0:
		return errors.New("the snapshot is empty")
	case !r.HasTrailer:
		return fmt.Errorf("the snapshot carries no integrity hash: %d bytes is not a whole number of 512-byte "+
			"blocks plus the 32-byte sha256 etcd appends", r.Size)
	case !r.TrailerOK:
		return errors.New("the snapshot's integrity hash does not match its content: it was cut short or changed")
	case r.PageSize == 0:
		return errors.New("the snapshot is not a bbolt database: neither meta page is valid")
	}
	db := r.Size - trailerSize
	if db%int64(r.PageSize) != 0 {
		return fmt.Errorf("the database is %d bytes, not a whole number of %d-byte pages", db, r.PageSize)
	}
	if r.PageCount*uint64(r.PageSize) > uint64(db) {
		return fmt.Errorf("the meta page counts %d pages of %d bytes, more than the %d bytes present",
			r.PageCount, r.PageSize, db)
	}
	return nil
}

// Verifier checks a snapshot as it streams past: it is an io.Writer that
// keeps the first two pages and a 32-byte lag, nothing else.
type Verifier struct {
	whole hash.Hash // every byte
	body  hash.Hash // every byte but the last 32
	head  []byte    // the first bytes, up to two pages
	lag   []byte    // the last up to 32 bytes, not yet in body
	size  int64
}

// NewVerifier returns a Verifier for one stream.
func NewVerifier() *Verifier {
	return &Verifier{whole: sha256.New(), body: sha256.New()}
}

// Write takes the next bytes of the stream. It never fails.
func (v *Verifier) Write(p []byte) (int, error) {
	n := len(p)
	v.size += int64(n)
	v.whole.Write(p)
	if room := 2*maxPageSize - len(v.head); room > 0 {
		v.head = append(v.head, p[:min(room, len(p))]...)
	}
	// Keep the newest 32 bytes back from body: they may be the trailer.
	if len(p) >= trailerSize {
		v.body.Write(v.lag)
		v.body.Write(p[:len(p)-trailerSize])
		v.lag = append(v.lag[:0], p[len(p)-trailerSize:]...)
		return n, nil
	}
	buf := append(append(make([]byte, 0, len(v.lag)+len(p)), v.lag...), p...)
	if over := len(buf) - trailerSize; over > 0 {
		v.body.Write(buf[:over])
		buf = buf[over:]
	}
	v.lag = append(v.lag[:0], buf...)
	return n, nil
}

// Report finishes the stream and reports what it found.
func (v *Verifier) Report() Report {
	r := Report{Size: v.size, SHA256: hex.EncodeToString(v.whole.Sum(nil))}
	if v.size >= trailerSize && v.size%512 == trailerSize && len(v.lag) == trailerSize {
		r.HasTrailer = true
		r.TrailerOK = string(v.body.Sum(nil)) == string(v.lag)
	}
	if m, ok := newestMeta(v.head); ok {
		r.PageSize, r.PageCount, r.TxID = m.pageSize, m.pgid, m.txid
	}
	return r
}

type boltMeta struct {
	pageSize uint32
	pgid     uint64
	txid     uint64
}

// newestMeta reads the two meta pages from the head of the file and returns
// the valid one with the higher transaction id, the one bbolt opens.
func newestMeta(head []byte) (boltMeta, bool) {
	m0, ok0 := readMeta(head, 0, 0)
	pageSize := uint32(0)
	if ok0 {
		pageSize = m0.pageSize
	} else if len(head) >= pageHeaderSize+metaSize {
		// Page 0 is damaged; its page size field may still be right, which
		// is how bbolt itself finds page 1.
		pageSize = binary.LittleEndian.Uint32(head[pageHeaderSize+8:])
	}
	var m1 boltMeta
	ok1 := false
	if pageSize >= 512 && pageSize <= maxPageSize && pageSize&(pageSize-1) == 0 {
		m1, ok1 = readMeta(head, int(pageSize), 1)
		ok1 = ok1 && m1.pageSize == pageSize
	}
	switch {
	case ok0 && ok1:
		if m1.txid > m0.txid {
			return m1, true
		}
		return m0, true
	case ok0:
		return m0, true
	case ok1:
		return m1, true
	}
	return boltMeta{}, false
}

// readMeta parses the meta page at off, which must carry page id want.
func readMeta(head []byte, off int, want uint64) (boltMeta, bool) {
	if len(head) < off+pageHeaderSize+metaSize {
		return boltMeta{}, false
	}
	p := head[off:]
	le := binary.LittleEndian
	if le.Uint64(p[0:]) != want || le.Uint16(p[8:])&boltMetaFlag == 0 {
		return boltMeta{}, false
	}
	m := p[pageHeaderSize : pageHeaderSize+metaSize]
	if le.Uint32(m[0:]) != boltMagic || le.Uint32(m[4:]) != boltVersion {
		return boltMeta{}, false
	}
	sum := fnv.New64a()
	sum.Write(m[:56])
	if sum.Sum64() != le.Uint64(m[56:]) {
		return boltMeta{}, false
	}
	meta := boltMeta{pageSize: le.Uint32(m[8:]), pgid: le.Uint64(m[40:]), txid: le.Uint64(m[48:])}
	if meta.pageSize < 512 || meta.pageSize > maxPageSize {
		return boltMeta{}, false
	}
	return meta, true
}

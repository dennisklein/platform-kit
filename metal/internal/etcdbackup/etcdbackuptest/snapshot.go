// Package etcdbackuptest builds etcd snapshots for tests: the smallest bbolt
// file bbolt itself would create (two meta pages, an empty freelist, an empty
// root leaf), followed by the sha256 trailer etcd's Snapshot RPC appends. The
// fake Talos client streams one, so envtest exercises the same verification
// a real snapshot passes.
package etcdbackuptest

import (
	"crypto/sha256"
	"encoding/binary"
	"hash/fnv"
)

const pageSize = 4096

// Snapshot returns a valid etcd snapshot whose newest meta page carries
// transaction id txid: different ids give different bytes and hashes.
func Snapshot(txid uint64) []byte {
	db := make([]byte, 4*pageSize)
	le := binary.LittleEndian
	for i := range 2 {
		p := db[i*pageSize:]
		le.PutUint64(p[0:], uint64(i)) // page id
		le.PutUint16(p[8:], 0x04)      // meta page
		m := p[16:]
		le.PutUint32(m[0:], 0xED0CDAED) // magic
		le.PutUint32(m[4:], 2)          // version
		le.PutUint32(m[8:], pageSize)
		le.PutUint64(m[16:], 3) // root bucket page
		le.PutUint64(m[32:], 2) // freelist page
		le.PutUint64(m[40:], 4) // high-water mark
		le.PutUint64(m[48:], txid+uint64(i))
		h := fnv.New64a()
		h.Write(m[:56])
		le.PutUint64(m[56:], h.Sum64())
	}
	le.PutUint64(db[2*pageSize:], 2)
	le.PutUint16(db[2*pageSize+8:], 0x10) // freelist
	le.PutUint64(db[3*pageSize:], 3)
	le.PutUint16(db[3*pageSize+8:], 0x02) // leaf
	sum := sha256.Sum256(db)
	return append(db, sum[:]...)
}

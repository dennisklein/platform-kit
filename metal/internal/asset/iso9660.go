// Package asset builds boot artifacts and serves them to BMC media pulls. Serving
// is grant-bounded: every fetch
// URL of a secret-bearing artifact is minted per mount — a high-entropy token in
// the URL path (both vendors reject query strings), a TTL, optional media
// credentials, revocation on node check-in / eject / expiry. TLS terminates here,
// never at the relay.
package asset

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// ConfigFilename is the one file on the config medium — machinery's name for what
// talos.config=metal-iso reads. Doubles as the data key of rendered config Secrets.
const ConfigFilename = constants.ConfigFilename

const sectorSize = 2048

// BuildConfigISO packs one rendered machine config into the medium Talos mounts
// when booted with talos.config=metal-iso: an ISO9660 volume labeled metal-iso
// holding a single config.yaml. Written entirely in memory — rendered configs
// live in Secrets and on the BMC media channel, never on a filesystem (which is
// why this is a ~150-line writer instead of a library: the available ISO9660
// writers stage content through temp files). All timestamps are left unset, so
// the same config packs to the same bytes.
func BuildConfigISO(machineConfig []byte) ([]byte, error) {
	if len(machineConfig) == 0 {
		return nil, fmt.Errorf("refusing to pack an empty machine config")
	}
	return writeISO(constants.MetalConfigISOLabel, ConfigFilename, machineConfig)
}

// writeISO lays out a minimal single-file ISO9660 volume (ECMA-119): system area,
// PVD, terminator, L/M path tables, root directory, file extent. The file
// identifier is stored lowercase with the mandatory ";1" version — the Linux
// iso9660 driver's default name mapping (map=normal) lowercases and strips the
// version either way, so the mounted volume shows the name verbatim.
func writeISO(label, filename string, content []byte) ([]byte, error) {
	if len(label) > 32 {
		return nil, fmt.Errorf("volume label %q exceeds 32 characters", label)
	}
	fileID := filename + ";1"
	if 33+len(fileID) > 255 {
		return nil, fmt.Errorf("file name %q too long for a directory record", filename)
	}

	const (
		pvdLBA     = 16
		termLBA    = 17
		lPathLBA   = 18
		mPathLBA   = 19
		rootDirLBA = 20
		fileLBA    = 21
	)
	fileSectors := (len(content) + sectorSize - 1) / sectorSize
	totalSectors := fileLBA + fileSectors

	img := make([]byte, totalSectors*sectorSize)
	sector := func(lba int) []byte { return img[lba*sectorSize : (lba+1)*sectorSize] }

	// Primary Volume Descriptor (field positions per ECMA-119 § 8.4, 0-based).
	pvd := sector(pvdLBA)
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	pvd[6] = 1
	padField(pvd[8:40], "")     // system identifier
	padField(pvd[40:72], label) // volume identifier — verbatim; blkid trims the padding
	both32(pvd[80:], uint32(totalSectors))
	both16(pvd[120:], 1)          // volume set size
	both16(pvd[124:], 1)          // volume sequence number
	both16(pvd[128:], sectorSize) // logical block size
	both32(pvd[132:], pathTableLen)
	binary.LittleEndian.PutUint32(pvd[140:], lPathLBA)
	binary.BigEndian.PutUint32(pvd[148:], mPathLBA)
	copy(pvd[156:190], dirRecord([]byte{0}, rootDirLBA, sectorSize, flagDirectory))
	padField(pvd[190:318], "") // volume set identifier
	padField(pvd[318:446], "") // publisher
	padField(pvd[446:574], "") // data preparer
	padField(pvd[574:702], "") // application
	padField(pvd[702:739], "") // copyright file
	padField(pvd[739:776], "") // abstract file
	padField(pvd[776:813], "") // bibliographic file
	for _, off := range []int{813, 830, 847, 864} {
		copy(pvd[off:], "0000000000000000") // date unspecified (§ 8.4.26.1)
	}
	pvd[881] = 1 // file structure version

	term := sector(termLBA)
	term[0] = 255
	copy(term[1:6], "CD001")
	term[6] = 1

	pathTable(sector(lPathLBA), rootDirLBA, binary.LittleEndian)
	pathTable(sector(mPathLBA), rootDirLBA, binary.BigEndian)

	root := sector(rootDirLBA)
	n := copy(root, dirRecord([]byte{0}, rootDirLBA, sectorSize, flagDirectory))     // "."
	n += copy(root[n:], dirRecord([]byte{1}, rootDirLBA, sectorSize, flagDirectory)) // ".."
	copy(root[n:], dirRecord([]byte(fileID), fileLBA, uint32(len(content)), 0))

	copy(img[fileLBA*sectorSize:], content)
	return img, nil
}

const flagDirectory = 0x02

// pathTableLen is the size of the one-entry (root) path table: 8 fixed bytes, a
// 1-byte identifier, and its pad to even length (ECMA-119 § 9.4).
const pathTableLen = 10

func pathTable(dst []byte, rootLBA uint32, order binary.ByteOrder) {
	dst[0] = 1 // identifier length
	order.PutUint32(dst[2:], rootLBA)
	order.PutUint16(dst[6:], 1) // parent directory number
	// dst[8] identifier 0x00, dst[9] pad — already zero.
}

// dirRecord encodes one directory record (ECMA-119 § 9.1). The 7-byte recording
// time stays zero — "not specified" — which keeps the image reproducible.
func dirRecord(fileID []byte, lba, size uint32, flags byte) []byte {
	recLen := 33 + len(fileID)
	if recLen%2 == 1 {
		recLen++ // records are padded to even length
	}
	r := make([]byte, recLen)
	r[0] = byte(recLen)
	both32(r[2:], lba)
	both32(r[10:], size)
	r[25] = flags
	both16(r[28:], 1) // volume sequence number
	r[32] = byte(len(fileID))
	copy(r[33:], fileID)
	return r
}

// both16/both32 write the ECMA-119 both-byte-order forms (LE then BE).
func both16(dst []byte, v uint16) {
	binary.LittleEndian.PutUint16(dst, v)
	binary.BigEndian.PutUint16(dst[2:], v)
}

func both32(dst []byte, v uint32) {
	binary.LittleEndian.PutUint32(dst, v)
	binary.BigEndian.PutUint32(dst[4:], v)
}

// padField writes an a-characters field: content left-justified, space-padded.
func padField(dst []byte, s string) {
	copy(dst, s)
	copy(dst[len(s):], bytes.Repeat([]byte{' '}, len(dst)-len(s)))
}

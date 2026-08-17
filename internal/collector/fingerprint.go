package collector

import (
	"hash/crc64"
	"os"
)

// fingerprintBytes is how much of a file's head is hashed to identify it.
//
// The number matters in one direction only: a file must reach this size before
// it can be fingerprinted at all, so a very large value would leave short-lived
// files unprotected. 1 KiB is past the first few lines of essentially any real
// log format, which is what makes collisions between two genuinely different
// files vanishingly unlikely, while still being reached within moments on any
// file that sees traffic.
const fingerprintBytes = 1024

// crc64Table is built once. crc64 is used rather than a cryptographic hash
// because this defends against accidental collision, not against an attacker
// crafting a log file to match — and it is roughly an order of magnitude
// cheaper on the open path.
var crc64Table = crc64.MakeTable(crc64.ISO)

// fileFingerprint hashes the first fingerprintBytes of f.
//
// This exists because device+inode alone is not a stable identity for a file.
// Inodes are recycled: delete a log file and the next file created on that
// filesystem can be handed the same inode number. The registry then matches a
// brand-new file against the offset of a completely different one and seeks
// straight past its opening content — silently, because from the agent's point
// of view everything looks consistent. On a host with heavy log rotation that
// is not a rare event.
//
// The head of an append-only file never changes, so once a file is big enough
// to fingerprint the value is stable for its whole life. That is the property
// this relies on. Below that size no fingerprint is reported at all, rather
// than one computed over a prefix that will change as the file grows — a
// fingerprint that mutates would condemn the file as "replaced" on every
// restart and re-send it from the beginning each time.
//
// Reads via ReadAt so the caller's file position is untouched.
func fileFingerprint(f *os.File) (uint64, bool) {
	if f == nil {
		return 0, false
	}
	buf := make([]byte, fingerprintBytes)
	n, err := f.ReadAt(buf, 0)
	if n < fingerprintBytes {
		// Includes io.EOF on a short file. Not an error worth reporting: it
		// simply means this file cannot be identified this way yet.
		return 0, false
	}
	if err != nil {
		return 0, false
	}
	return crc64.Checksum(buf, crc64Table), true
}

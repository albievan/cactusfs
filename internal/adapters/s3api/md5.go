package s3api

import (
	"crypto/md5"
	"hash"
)

// newETagHash returns the hash S3 clients expect behind an ETag and a
// Content-MD5 header. MD5 is used here as the checksum the S3 protocol
// prescribes, never as a security primitive: integrity at rest is AES-GCM
// authentication plus SHA-256 block checksums, and request integrity is the
// SigV4 signature. This file is the only place MD5 appears, and the only one
// for which .golangci.yml silences gosec's weak-hash findings.
func newETagHash() hash.Hash {
	return md5.New()
}

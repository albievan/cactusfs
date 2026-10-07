# 0002. Encryption at rest: streaming AEAD and envelope keys

Date: 2026-10-07
Status: accepted

## Context

`CLAUDE.md` §3 requires every object to be encrypted before erasure coding with
AES-256-GCM as a streaming AEAD: fixed-size segments, a unique nonce per segment, the
segment index bound into the AAD so that reordering and truncation are detected, and a
data key per object wrapped by a KMS key. Only the standard library `crypto/*` and
`golang.org/x/crypto` may be used. Range reads must not require decrypting the whole
object.

## Decision

**Scheme name:** `AES256-GCM-STREAM-V1`, recorded with every object version.
Implemented in `internal/domain/object/crypto.go`.

**Keys.** Each write generates a fresh 256-bit data key through `ports.KMS`. The
plaintext key lives in memory for the duration of the request; only the wrapped key is
stored, in the object version's metadata, with the ID of the KMS key that wrapped it. The
KMS context (AAD of the wrap) is the object's 16-byte data ID.

**Segments.** The plaintext is cut into segments of 64 KiB (`SegmentSize`, stored per
object; at most 16 MiB). Only the last segment may be shorter. An empty object is one
empty segment. Each segment is sealed independently:

```
nonce      = nonce prefix (4 random bytes, stored per object) || segment index (uint64, big endian)
AAD        = segment index (uint64, big endian) || final (1 byte: 1 for the last segment) || data ID (16 bytes)
ciphertext = AES-256-GCM(data key, nonce, plaintext segment, AAD)      -- 16-byte tag appended
```

The encrypted stream is the concatenation of the sealed segments. Its length is
`size + 16 × max(1, ceil(size / SegmentSize))`.

**Why this is safe.**

- *Nonce uniqueness:* a data key encrypts one object once, and the index is unique within
  it, so a (key, nonce) pair never repeats. The random prefix is defence in depth.
- *Reordering and substitution:* the index is in the AAD (and the nonce), so a segment
  only authenticates at its own position.
- *Truncation:* the last segment is sealed with `final = 1`. A stream cut at any point —
  including exactly on a segment boundary with the recorded size lowered to match — ends
  in a segment that was not sealed as final and fails.
- *Moving ciphertext between objects:* the data ID is in the AAD, and each object has its
  own key.
- *Never return unverified bytes:* the reader authenticates a whole segment before
  releasing any byte of it.

**Range reads.** Segment `i` starts at `i × (SegmentSize + 16)` in the encrypted stream,
so a read decrypts only the segments that cover the range.

**Sealed secrets.** API key secrets use the same envelope pattern
(`internal/domain/iam/secret.go`): a fresh data key from the KMS, AES-256-GCM with a
random 96-bit nonce, AAD = the access key ID.

**No fallback.** There is no code path that stores an object or a secret without a KMS.

## Consequences

- Changing segment layout, nonce or AAD construction is a new scheme name
  (`…-V2`) that readers select by the stored scheme; V1 objects stay readable. It needs a
  superseding ADR, a migration plan and an upgrade test.
- Re-wrapping data keys under a new KMS key (rotation) touches metadata only, never
  object data. Not implemented yet.
- SSE-KMS and SSE-C (milestone 6.5) change who supplies or wraps the data key, not the
  stream format.
- Overhead is 16 bytes per 64 KiB (0.02 %).
- The plaintext size of an object is visible from the stored size. This is accepted.

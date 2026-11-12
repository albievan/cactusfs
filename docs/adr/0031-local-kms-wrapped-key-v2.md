# ADR-0031: Wrapped keys of the local KMS, version 2 - a key of its own for every data key

- **Status:** accepted and implemented (audit finding L34, 2026-11-12)
- **Date:** 2026-11-12
- **Changes:** the wrapped-key format of `internal/adapters/kms/local` (ADR-0002, "Keys")

## Context

The local KMS (`internal/adapters/kms/local`) wraps every data key it makes - one per
object version, per part of a multipart upload, per API key secret and per sealed
setting - under its master key. Version 1 of a wrapped key was

```
version (1 byte, 1) || nonce (12 bytes) || AES-256-GCM(master key, nonce, data key, key ID || aad)
```

with a fresh random nonce each time. AES-GCM with random 96-bit nonces is safe for at
most about 2^32 messages under one key (NIST SP 800-38D, 8.3): past that the chance of
two nonces colliding - which gives away the authentication key and the XOR of the two
data keys - is no longer negligible. Nothing bounded the number of wraps, and the master
key of a local key file is never rotated. A long-lived standalone node, or a development
cluster sharing one key file, could get there.

The wrapped key is stored with every object version and every sealed secret, in the
Raft-replicated metadata. Changing it is a change of an on-disk format.

## Decision

A new wrapped key is of version 2:

```
version (1 byte, 2) || salt (32 bytes) || nonce (12 bytes) ||
AES-256-GCM(K, nonce, data key, key ID || aad)

K = HKDF-SHA256(secret = master key, salt = salt, info = "cactusfs kms/local wrap v2 " || key ID, 32 bytes)
```

- The salt and the nonce are drawn from the operating system's random source with the
  data key, in one read.
- Every data key is sealed under a key of its own. Two wraps share a key only when they
  share a 256-bit salt, so the 2^32 bound applies to wraps per *salt*, of which there is
  one; the bound on the master key becomes that of HKDF, which has none that matters
  here. The nonce is kept random rather than fixed, at the cost of 12 bytes, so that a
  salt repeated through a broken random source does not at once repeat a (key, nonce)
  pair.
- The key ID is in the HKDF context as well as in the associated data, so a wrapped key
  of one key file does not open under another even if the master keys were equal.
- Only the standard library is used: `crypto/hkdf`, `crypto/sha256`, `crypto/aes`,
  `crypto/cipher`.
- A wrapped key is 93 bytes instead of 61.

**Reading.** `UnwrapDataKey` reads both versions, chosen by the first byte. Version 1
keys are opened as before, under the master key itself; there is no migration that
rewraps them, because a version 1 key that exists is no weaker for being read, and
rewrapping every object version would rewrite the metadata of the whole deployment for
nothing. A version byte that is neither is refused, as before. A version 2 key relabelled
as version 1 does not open (the tests check it).

**Writing.** Every key wrapped from this release on is of version 2.

## Rolling upgrades

The local KMS is for a single node, and for a cluster only in development (the chart
requires `dev.localKMS`; the compose cluster uses it). In a cluster that shares one key
file and is upgraded one node at a time, a node of the new release writes version 2 keys
that a node of the old one cannot open until it is upgraded too: such a node fails to
read the new objects with `ErrKeyUnavailable`, and nothing is lost or damaged. Production
clusters use Vault Transit, whose format this does not touch. Writing version 1 until
every node is upgraded would need the adapter to know the versions of the other nodes,
which a KMS adapter has no port for; for a development-only mode it was not judged worth
one. **Open for Albie:** whether a development cluster on the local KMS must be rollable
across this change; if so, a setting that keeps writing version 1 until the last node is
upgraded is the way.

## Consequences

- `internal/adapters/kms/local/testdata/wrap-v1` holds a key file and a data key wrapped
  by the version 1 code before this change; `TestWrappedKeysOfVersion1StillUnwrap`
  opens them with this one. They must never be regenerated.
- `FuzzUnwrapDataKey` checks that no malformed wrapped key panics or opens.
- A further version adds a case to `UnwrapDataKey` and keeps the two that exist.

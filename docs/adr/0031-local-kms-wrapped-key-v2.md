# ADR-0031: Wrapped keys of the local KMS, version 2 - a key of its own for every data key

- **Status:** accepted and implemented (audit finding L34, 2026-11-12); amended 2026-11-13 (rolling upgrades)
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

**Writing.** A key is wrapped in version 2 once every node that may read it reads
version 2, and in version 1 until then (amended 2026-11-13, below). A node on its own -
standalone - is every node there is, and writes version 2 from its first key.

## Rolling upgrades

*Amended 2026-11-13 (review of the audit fixes).* As first accepted, every key was
wrapped in version 2 at once, so in a cluster sharing one key file (the compose
cluster runs `CACTUSFS_KMS=local`) and upgraded one node at a time, a node of the
release before could not open keys wrapped by an upgraded node until it was upgraded
too. That broke the rule that release N runs alongside N+1. Now:

- Version 2 came with the release of metadata entry schema 6 (ADR-0030). A cluster whose
  metadata level - the oldest `MetadataVersion` every member of the Raft group has
  registered, `raftmeta.Store.MetadataLevel` - is 6 or more has no member that cannot
  read version 2.
- `local.KMS.WriteVersion2When(ready)` makes the adapter wrap in version 1 until `ready`
  says yes, and in version 2 from then on, for good: a member of the release before
  cannot join a cluster at level 6 (it stops at the first entry of schema 6). The
  composition root (`cmd/cactusfs/node.go`) sets `ready` to "the metadata store is open
  and its level is at least 6", before any key is made. While the store cannot tell, the
  answer is no: version 1 is read by every release.
- The adapter keeps no port of its own for this; the composition root, which sees both
  the KMS and the metadata store, decides. A `KMS` nobody gates (tools, tests) writes
  version 2.
- Version 1 keys written during an upgrade are not rewrapped later, for the reason given
  under **Reading**; a cluster is at most one upgrade's worth of keys behind the
  bound this ADR is about.
- `TestVersion1NodesKeepWorkingMidUpgrade` (a stand-in for the release before, reading
  version 1 only, opens every key made mid-upgrade; version 2 after, for good),
  `TestRollingUpgradeToSchema6` (the level stays 5 until the last node registers 6) and
  `TestWrapVersion2FollowsTheMetadataLevel` (a standalone node writes version 2 at once)
  check it.

Production clusters use Vault Transit, whose format this does not touch.

## Consequences

- `internal/adapters/kms/local/testdata/wrap-v1` holds a key file and a data key wrapped
  by the version 1 code before this change; `TestWrappedKeysOfVersion1StillUnwrap`
  opens them with this one. They must never be regenerated.
- `FuzzUnwrapDataKey` checks that no malformed wrapped key panics or opens.
- A further version adds a case to `UnwrapDataKey` and keeps the two that exist.

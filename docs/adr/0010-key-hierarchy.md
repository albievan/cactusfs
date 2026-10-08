# ADR-0010: Key hierarchy - one data key, several uses

- **Status:** accepted; the multipart part is implemented (PLAN 6.1). SSE-KMS and SSE-C
  (PLAN 6.5) will extend this record, not replace it.
- **Date:** 2026-10-08
- **Milestone:** 6
- **Builds on:** ADR-0002 (streaming AEAD, envelope keys)

## Context

ADR-0002 gives every object one data key from the KMS, wrapped by the KMS key and bound
to the object's data ID, and encrypts the object's one stream with it. A multipart
upload does not fit: its parts arrive separately, in any order, possibly at once through
different gateways, and a part can be sent again. Each part has to be durable and
verifiable on its own before anybody knows which parts the object will consist of.

Two things must not happen. A key must never encrypt two streams with nonces that can
meet: the stream construction makes nonces from a random prefix and a counter, and with
ten thousand parts, each sent any number of times, one key for all of them would rest on
the prefixes never colliding. And a KMS round trip per part would make the KMS - Vault,
in a cluster - a bottleneck of every large upload, and its availability a condition of
every part.

## Decision

### One data key per upload, one derived key per part

- Beginning an upload asks the KMS for **one data key**, wrapped and bound to a context
  that names the upload ID *and that this is an upload's key*
  (`"cactusfs/upload/v1/" ‖ upload ID`), as an object's key is bound to its data ID. A
  key wrapped for a stream does not unwrap as an upload's key, nor the reverse. The upload ID
  becomes the data ID of the version the upload makes, so the wrapped key moves from the
  upload to the version unchanged and stays bound to it.
- That key **encrypts nothing**. The record says so: `key_mode: part-hkdf-v1`. A reader
  that finds a key mode on a version that is not made of parts, or none on one that is,
  refuses the version.
- The key of a part is `HKDF-SHA256(upload key, info = "cactusfs/part/v1/" ‖ part number
  ‖ "/" ‖ part data ID)`, 32 bytes. The part's stream is the stream of ADR-0002 under
  that key, with the part's data ID as associated data and a random nonce prefix of its
  own. Sending a part again makes new data with a new ID, and therefore a new key: no
  key ever encrypts two streams.
- A gateway unwraps the upload's key once per request that needs it (a part, a
  completion, a read) and derives from it in memory. Keys are cleared after use; the
  key held for the length of a read is cleared when the body is closed.

### The manifest seal

A part's stream authenticates the part: its bytes, their order, its end. It says nothing
about the object. Somebody who can write the metadata state - a stolen metadata drive, a
node that proposes entries - could otherwise serve an object with a part dropped from
the end, taken out of the middle, moved, or swapped for a part of the same upload that
the client did not list, and every byte returned would authenticate. For one stream,
ADR-0002 rules that out ("reordering and truncation are detected"); the same has to hold
for an object made of several.

- A second key is derived from the upload's key:
  `HKDF-SHA256(upload key, info = "cactusfs/manifest/v1")`.
- When an upload is completed, the gateway computes for every listed part a **seal**:
  `HMAC-SHA256(seal key, index ‖ count ‖ part number ‖ offset ‖ size ‖ total size ‖
  part data ID)`, the numbers in decimal and each closed by a separator. The seals go
  into the completion entry and are stored with the parts of the object.
- A read verifies the seal of every part **before a byte of that part is returned**,
  against the count and the size the version record states, and checks that each part
  begins where the one before it ended. A part without its seal, or with the seal of
  another place, another count or another total, is an integrity error.
- An object that says it is empty is checked too: it must have exactly one part, empty,
  sealed as the only part of an object of no bytes, whose stored stream authenticates.
  Otherwise a record changed to "size 0" would be served as an empty object.
- The state machine cannot check a seal - it has no key - and does not need to. It
  checks that each listed part is the stored one, by data ID, so that a seal made for
  one upload of a part is never stored with another.

What the seal does not do: bind the object to its name. As with a plain object, a whole
version record put under another key reads as what it is. That is unchanged from
ADR-0002 and outside what the metadata state can be defended against by encryption.

## Alternatives rejected

- **One key for every part, random nonce prefixes.** Rests the confidentiality of large
  uploads on a birthday bound, for no saving but one HKDF call per part.
- **A KMS data key per part.** A Vault round trip, and a wrapped key in the state, per
  part: up to ten thousand of each per object.
- **Re-encrypting at completion into one stream.** Completing would copy the object - a
  five-terabyte one included - and need twice its space.
- **One MAC over the whole list in the version record.** A ranged read would have to
  load ten thousand part records to check it. A seal per part, each naming the count and
  the total, gives the same guarantee and is checked with the parts a read touches.
- **No seal.** Stricter is the rule (CLAUDE.md §3) when a security rule is ambiguous.

## Consequences

- `internal/domain/object/multipart.go` holds the derivations (`PartKey`, `Manifest`);
  they use `crypto/hkdf`, `crypto/hmac` and `crypto/sha256` of the standard library and
  nothing else.
- A version record gains `encryption.key_mode` and `parts`; a part record carries its
  number, data ID, nonce prefix and seal. These are additions to entry schema 2
  (ADR-0008), which no release has shipped yet; a binary of schema 1 refuses the entries
  that carry them.
- Rotating the KMS key re-wraps one key per multipart object, as for any object.
- SSE-C (6.5) will combine the client's key with the KMS data key by the same HKDF,
  under another info string, and SSE-KMS will bind an encryption context when the key
  is wrapped. Both are uses of the hierarchy above and will be added here.

## Tests

- Property: part keys differ whenever the number or the data ID differs, never equal the
  upload's key, and are reproducible (`TestPartKeys`).
- A seal holds for exactly one part, place, count, total and upload key
  (`TestManifestSealsThePlaceOfAPart`).
- The stored bytes of a part open under its derived key and under no other, the
  upload's own included (`TestPartsAreEncryptedUnderKeysOfTheirOwn`).
- Thirteen ways of changing the part list or the version record of a stored object -
  swap, drop from either end, substitute, renumber, resize, relabel - each fail the
  read, and no byte returned before the failure is wrong (`TestGetWithTamperedParts`).

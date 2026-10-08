# ADR-0010: Key hierarchy - one data key, several uses

- **Status:** accepted and implemented (PLAN 6.1: multipart; PLAN 6.5: SSE-KMS, SSE-C)
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

### SSE-S3, SSE-KMS and SSE-C

Every object is encrypted, whatever a client says (ADR-0002). What a client can say is
whose key it takes to read it. The record of every version and upload names the mode.

- **SSE-S3** (`AES256`, or nothing) is what there always was: a data key from the
  deployment's KMS, wrapped and bound to the data ID.
- **SSE-KMS** (`aws:kms`) is the same key from the same KMS: the deployment has one KMS
  key, and a request that names another is refused. What it adds is the **encryption
  context**. The context is written in one canonical form, stored with the version, and
  bound into what the KMS wraps the key under, together with the mode. A record whose
  context or mode was changed names another binding, and its key does not unwrap. A
  reader does not send the context; it is in the record, as with AWS. Bucket keys and
  dual-layer encryption answer 501.
- **SSE-C** adds a key the client holds. The object is encrypted - or, for an upload,
  its part keys are derived - under
  `HKDF-SHA256(secret = customer key, salt = KMS data key, info = "cactusfs/sse-c/v1")`.
  It takes both keys: the client's key alone, with the drives and the metadata, reads
  nothing without the KMS, and the deployment alone reads nothing without the client.
  **The customer key is never stored and never logged, and neither is a digest of it.**
  S3 has clients send an MD5 of the key; it is checked against the key and echoed in
  the answer to the request that carried it, and goes no further - stored, it would let
  anybody with the metadata test guesses of a key at one MD5 each. What is stored is a
  check value, sixteen bytes derived by HKDF from the derived key for that purpose
  alone, by which a later request with the wrong key is told (`403 AccessDenied`) from
  damaged data, in constant time. Testing a guess against it takes the KMS data key.
- **Nothing stored about an SSE-C object is a digest of its plaintext.** Its entity
  tag - and that of each part of an SSE-C upload - is not the MD5 of the content but an
  HMAC of it under a key derived from the derived key, of the same length: the same
  content under the same key has the same tag, and nobody without both keys can
  confirm a guess of the content from a listing, an event or the metadata. The
  checksums a client declares for a part are verified and not kept, so a checksum
  listed when such an upload is completed is not compared; the entity tags are.
- Reading an SSE-C object, or its metadata, needs the key; a request without it is a
  `400`. Deleting, listing and locking do not: they touch no plaintext.
- Completing an SSE-C upload, and listing its parts, need no key, as in S3. A client
  that sends one - the SDKs' transfer managers do - has it checked. That completing
  works without is why the manifest seal is made under the KMS data key and not under
  the combined one: against a party that holds the KMS key the seal of an SSE-C object
  proves nothing, and that party cannot read the object either way.
- **A customer key is accepted only over TLS, only in headers, and only where it is
  read.** Not on the plaintext development listener; not in a query string, signed or
  not, because a URL is logged wherever it goes (a presigned URL for an SSE-C object
  works with the key in signed headers); and the encryption headers on an operation
  that does not read them are a `400`, not ignored.
- The management API and the console cannot read an SSE-C object, or its metadata -
  they have no key to offer - and say so with a `400`.
- An encryption header that is given twice, or empty, is refused wherever it arrives;
  whether one is there is never judged by its first value.
- The mode, the context and the check value are fields of entry schema 2: a
  put that names a mode is written in schema 2, and an entry of schema 1 that carries
  them is applied as a binary of schema 1 would apply it, without.

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

## Tests

- SSE-C and SSE-KMS: the derived key takes both keys and is neither
  (`TestCustomerDataKey`); the customer key is in no record, on no drive and in no log,
  and a record relabelled in five ways opens with no key (`TestCustomerKeys`); a
  changed encryption context does not unwrap (`TestSSEKMS`); uploads
  (`TestMultipartWithCustomerKey`); over HTTP with TLS, without it, and presigned; the
  AWS SDK against the built binary.

- Property: part keys differ whenever the number or the data ID differs, never equal the
  upload's key, and are reproducible (`TestPartKeys`).
- A seal holds for exactly one part, place, count, total and upload key
  (`TestManifestSealsThePlaceOfAPart`).
- The stored bytes of a part open under its derived key and under no other, the
  upload's own included (`TestPartsAreEncryptedUnderKeysOfTheirOwn`).
- Thirteen ways of changing the part list or the version record of a stored object -
  swap, drop from either end, substitute, renumber, resize, relabel - each fail the
  read, and no byte returned before the failure is wrong (`TestGetWithTamperedParts`).

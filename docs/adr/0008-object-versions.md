# ADR-0008: Entry schema 2, the leader's time, and object versions in the metadata state

- **Status:** accepted and implemented (PLAN 6.0, 6.2)
- **Date:** 2026-10-08
- **Milestone:** 6
- **Supersedes:** the single-version object index of ADR-0003, in part

## Context

Milestone 6 adds versioning, Object Lock, multipart uploads and bucket configuration.
Until now every release only *added* Raft command types to entry schema 1. An older
binary stops on a command it does not know, which is safe. Milestone 6 changes what
existing commands mean and carry: a put into a versioned bucket keeps the version it
replaces, a delete adds a marker, a bucket is created with Object Lock. Entries are
decoded leniently, so an older binary would not stop on such an entry - it would drop
the field it does not know and apply something else. That is silent divergence.

Object Lock also needs a time that every replica agrees on. A retention period has
passed or it has not; replicas that each asked their own clock could disagree, and the
state machine must be deterministic.

## Decisions

### Entry schema 2

- An entry is still one version byte and a JSON document. Schema 2 adds the field `at`
  and the commands of milestone 6. `MetadataVersion` is 2.
- Schema 1 stays the floor: a binary of this release applies entries of both, and
  writes schema 1 for everything schema 1 can express while any member of the cluster
  has not registered as applying schema 2. This is the rule of ADR-0005, now used for
  the first time: the level of a cluster is the lowest version its members registered,
  and a member that has not registered counts as 1.
- A node on its own has no other members, so it writes schema 2 from the start.
- A command of milestone 6 proposed while the cluster is at level 1 is refused as
  unavailable ("the cluster is being upgraded"), definitely and before anything is
  logged. Nothing of milestone 6 works until every member runs this release.

### State schema 2

- The state database records its schema. It stays 1 until the first entry of schema 2
  is applied, which sets it to 2 in the same transaction.
- So a cluster that is being upgraded, or was upgraded and has not used anything new
  yet, has a state and snapshots the previous release still reads: a rollback is
  possible until then. After the first schema 2 entry the previous release refuses the
  state (it accepts schema 1 only) instead of misreading it. There is no way back from
  there, as there cannot be: the state holds what the previous release cannot express.
- Existing data needs no rewrite. A state of schema 1 opens unchanged.

### The time of an entry

- The **metadata leader** stamps every entry with its clock when it logs it, whoever
  proposed it. A proposer's clock, a gateway's clock and a follower's clock decide
  nothing.
- The state machine keeps the time of the newest entry it applied (`meta/time`) and
  gives every entry the later of its stamp and that time. **Time never goes back**: a
  leader with a slow clock, or a new leader whose clock is behind the old one's, cannot
  move it. Every replica computes the same time for the same entry from the log alone.
- The state machine never reads a clock.
- What this does not prevent: a voter whose clock is **ahead** becomes leader and moves
  time forward, ending retention periods early. Voters need synchronised clocks; this is
  in the threat model. A stricter rule - taking, for a decision that depends on time
  having passed, the earliest clock among a majority of voters - is not built. See open
  questions in PLAN.md.

### Object versions

- `objects/<bucket>/<key>` keeps its key and its value. It holds the current version of
  a key whenever the newest version is not a delete marker. Listing and reading the
  current version are exactly as fast as before, and never see delete markers.
- A new nested bucket `versions/<bucket>` holds everything else: versions that are no
  longer current, and delete markers. Its keys are
  `enc(key) ‖ ^sequence (8 bytes) ‖ version ID (16 bytes)`, where `enc` is an
  order-preserving, prefix-free encoding of the object key (an object key may contain a
  zero byte, so a plain separator would be ambiguous), the sequence is the Raft index of
  the entry that committed the version, inverted so that the newest sorts first, and the
  version ID is sixteen zero bytes for the null version.
- A version record gains `versioned`, `seq` and `delete_marker`. A record without
  `versioned` is the null version: **every object written before this release is
  already a correct null version**, with sequence 0, which sorts oldest.
- What a put or a delete does is decided by the state machine from the versioning state
  of the bucket at the moment the entry is applied, not by the proposer: a put that
  races a change of the versioning state lands on one side or the other, on every
  replica alike.
- The shard reference index (`data`) counts every version that refers to stored data,
  current or not. When it is rebuilt it walks `versions` (and, from PLAN 6.1, the parts
  of uploads) as well as `objects`. A rebuild that forgot them would let the collector
  delete the shards of every noncurrent version; this has a test of its own.
- A key can have any number of versions, and nothing may cost time in proportion to
  that number - least of all an entry being applied, with every other change waiting. A
  third nested bucket, `versionids/<bucket>`, maps `enc(key) ‖ version ID` to the
  sequence, so that a version is found by its name directly; the null version, which a
  put or a delete under suspended versioning must find, is found the same way. The
  index is derived from `versions` and rebuilt when missing. Scans read straight from
  the cursor and can resume after a named version.
- An ID a key already has is refused for another version of that key. IDs are random
  and do not repeat; the rule keeps a mistaken entry from making a version unreachable.
- Two commands are new: `bucket.versioning.put` and `object.version.delete`, which
  deletes an object (making a delete marker where the bucket keeps versions) or removes
  one named version for good. The `object.delete` of schema 1 stays for logs written
  before, and is refused in a bucket that keeps versions. While a member of the
  previous release holds a cluster at schema 1 no bucket can keep versions, and a
  delete is written the old way.
- A delete marker's ID and time come from the proposer, as every identifier does; its
  place in the order of versions is the Raft index of the entry.
- Removing a version for good is its own permission (`s3:DeleteObjectVersion`), and so
  is reading one by name (`s3:GetObjectVersion`). The built-in `read-write` role can
  delete objects and cannot remove versions: while versioning is **enabled**, a holder
  of that role, or of its stolen key, destroys nothing. While it is **suspended**, a put
  or a delete replaces the null version, as S3 defines it; versions with IDs stay. The
  role cannot suspend versioning (`s3:PutBucketVersioning`), and suspending is audited.
- The body of DeleteObjects and of PutBucketVersioning must be covered by the request's
  signature - by its SHA-256, by signed chunks, or by a digest in a signed header. An
  unsigned payload or a presigned URL is refused for these two: whoever held the URL
  would choose what is destroyed. AWS accepts both; this is the stricter reading.
- Request bodies that decide what is destroyed are parsed strictly: an unknown element,
  a repeated setting, an element inside one of its own kind, text beside elements, a
  second document or a document type declaration is malformed, never skipped.
- Once the state holds what only schema 2 expresses, a member that has not registered
  its version does not take the cluster back to schema 1: a member of the previous
  release would have stopped at the first such entry.
- Scrubbing walks every version, and healing works on a named version: an old version
  that rots is found like a current one.

## Upgrade

- Rolling upgrade from the previous release: nodes are replaced one by one; schema 2 is
  not written until the last has registered. Tested with nodes of both levels side by
  side (`TestRollingUpgrade`, `TestTheLeaderStampsEntries`).
- A state written by the previous release opens, and its objects are null versions.
  `TestUpgradeFromThePreviousRelease` runs this binary on a deployment written by the
  previous release's binary (committed under `test/integration/testdata`), reads every
  object, turns versioning on and writes over them.
- The state machine is held against an independent in-memory model over random
  sequences of puts, deletes, version deletes and versioning changes
  (`TestVersionsAgainstAModel`), with the index invariant and the reference counts
  checked from the raw state after every step.

## Known limits

- A version listing resumed after a version that was removed in the meantime goes on
  with the next key: where the removed version was cannot be told from its name. The
  rest of that key's versions are missing from that listing. Scrubbing resumes the same
  way and checks them on its next pass.
- A GET or HEAD of an object whose newest version is a delete marker answers 404
  without the `x-amz-delete-marker` header, and a HEAD of a marker by its ID answers
  405 without it.
- Shards of a version that was removed are deleted without asking the reference index.
  No path gives two versions the same stored data today; CopyObject (PLAN 6.9) must
  change this first.
- Healing by name and healing "the current version" of the same key do not exclude each
  other.

## Consequences

- Every entry is decoded and encoded once more on the leader, to stamp it.
- A deployment cannot be rolled back to the previous release once it has applied an
  entry of schema 2.
- The "extend schema 1" decisions of milestones 4 and 5 stand for what they covered:
  commands that only add. This ADR is the rule for commands that change.

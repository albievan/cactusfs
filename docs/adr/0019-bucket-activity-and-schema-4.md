# ADR-0019: Who created a bucket, its latest uploads and downloads - and entry schema 4

- **Status:** accepted; being implemented (PLAN G.1-G.10)
- **Date:** 2026-10-12
- **Builds on:** ADR-0003 (metadata), ADR-0005 (cluster, levels), ADR-0008 (the leader's
  time), ADR-0016 (entry schema 3, the level rule as it stands, object access)

## Context

Albie wants the buckets as a grid: for each bucket its name, the number of objects, its
size, when it was created and by whom, the policies attached to it, and "the last time
any object was uploaded and downloaded from the bucket and who uploaded and downloaded
the recent objects".

Two of these are not in the state.

- **A bucket has no creator.** `bucket.create` records a name and a time. The audit trail
  knows who created a bucket through the management API, and nothing of one created
  through S3.
- **Nothing is kept per bucket of what happens in it.** Who uploaded is recorded on a
  version, who read in the record of a version (ADR-0016). "The latest in this bucket"
  would be a walk of every version of the bucket and of every record of reads.

And two properties of the metadata store decide how they can be added.

- **Entries are decoded leniently.** A field a binary does not know is dropped without a
  word. A member of the release before would apply a `bucket.create` that carries
  `created_by` and store the bucket without it: another bucket record than the others
  hold, and nothing would say so.
- **A snapshot is the bbolt file.** A member that is sent a snapshot holds, byte for
  byte, what the member that took it held. So whatever is derived while entries are
  applied must be derived by every member alike, from the log alone - never from
  something a member happens to know when it applies the entry, such as the level of
  the cluster at that moment.

## Decision

### Entry schema 4

No new command. One new field, and one new meaning of the schema byte.

| For | New in the log | New in the state (bbolt) | A member of schema 3 |
|---|---|---|---|
| The creator | `created_by` on `bucket.create` | `created_by` in the bucket record | never meets the entry: it is written only at level 4. Below, the bucket is created without |
| Latest use | nothing: `object.put`, `upload.complete` and `object.reads.add` are **written as schema 4** once the level is 4, with the fields they always had | `bucketactivity`: key = bucket name, value a JSON record with `"v":1` | never meets the entry. Below level 4 the three are written as before (schema 1, 2 or 3), and no member keeps a record |

- An entry is of schema 4 when it is a `bucket.create` that says who (`versionOf`), **or**
  one of the three entries above proposed while the cluster is at level 4 (`writtenAt`).
  Everything else is written as before, at the schema it always had.
- **The schema byte is the switch.** Every member that applies an `object.put`, an
  `upload.complete` or an `object.reads.add` whose schema byte is 4 or above updates the
  record of the bucket; every member that applies one below 4 does not. The byte is in
  the log, so all members decide alike, a replayed log gives the same state, and a
  snapshot holds what its log would have produced. A member of the release before
  cannot be asked to keep a record it does not know of - and is not: it never applies
  such an entry.
- What is of schema 4 in an entry that claims an older schema is treated exactly as the
  binary of that schema treats the same bytes: `created_by` of a `bucket.create` is
  stripped when the entry is applied (`fsm.Apply`), and the bucket record that is stored
  is byte for byte the one the release of schema 3 stores.
- The record is: `{"v":1,"uploads":[{"at","by","key"}],"downloads":[{"at","by","presigned","key"}]}`
  - times in RFC 3339 with nanoseconds, in UTC; the fields in this order; `uploads` and
  `downloads` left out when empty, and `presigned` when false. The bytes are pinned by
  `internal/adapters/raftmeta/testdata/bucket-activity-record.golden.json`: a change
  that makes two releases store other bytes needs a rise of the entry schema.
- `MetadataVersion` is 4: a node of this release announces it when it registers.

### Level rule and state schema

Unchanged in form (ADR-0008, ADR-0016).

- Nothing of schema 4 is proposed until every member has registered that it knows
  schema 4. Until then a bucket is created without its creator (`heldTo` drops the
  field), and a put, a completion and a batch of reads are written as they always were.
  **Nothing is refused that worked before, and nothing is refused at all for want of
  schema 4**: what it adds is left out.
- A proposer that knows the cluster a moment late and writes an entry as schema 4 is
  refused by the leader, which logged nothing, and writes it again below the schema
  that was refused (`Store.propose`), as with schema 3.
- A member that meets an entry of a schema it does not know stops, as before. A member
  of schema 3 stops at an `object.put` of schema 4 although it could read every field
  of it: what the entry means, it does not know.
- The state becomes schema 4 with the first schema-4 entry in the log - whether it took
  effect, was refused, or was applied as nothing - for the reason ADR-0016 gives: the
  binary before this one would stop at that entry when it replayed the log. That binary
  then refuses the state, and a snapshot of it. Until that first entry the state is
  unchanged, and the bucket `bucketactivity` does not exist: it is created when first
  written.
- Once the state is of schema 4, a member that has not registered - one that was just
  added, or whose record was removed - no longer holds the cluster below 4: no member
  of the release before can be in it (`Store.entryVersion`).

### Migration: none

A schema-3 state opens as it is and is not modified by being opened, read, or written
with anything but the three entries and a bucket with its creator. Proved by the
fixture `test/integration/testdata/schema-3`, written by the binary of commit d2d6861,
and `TestUpgradeFromSchema3ToSchema4` (the state says 3 after this binary opened and
read it, 4 after the first upload, and everything the manifest lists reads at either);
by `TestUpgradeFromSchema3`, unchanged; and by a rolling upgrade of a cluster in a test
(`TestRollingUpgradeToSchema4`).

Buckets that exist have no creator and say "not recorded"; their latest use is not
known until the next upload or download. **There is no backfill** (see Alternatives).

### Semantics

- **Creator.** `bucket.Bucket.CreatedBy` is who created the bucket, in the words the
  audit trail uses for actors (`root`, `user/<name>`, `key/<access key ID>`), like
  `object.Version.CreatedBy`. It is said once, at creation, and nothing changes it. A
  name of more than 256 bytes, or of more than one line, is refused before anything is
  proposed, and by the state machine if it gets there (`bucket.ValidateCreator`: bytes
  only, so that every build decides alike).
- **An upload** is a version that is committed: by a put, by a copy (the destination;
  it arrives as an `object.put`), or by the completion of a multipart upload, once, at
  completion. Not a delete marker, a change of tags, retention or legal hold, the
  creation of a multipart upload, a part, or a put that was refused. The row says who
  committed the version (`created_by` of the entry; empty when not said).
- **A download** is an item of a batch of reads with `downloads > 0` that can be
  recorded (`object.Read`) and names a bucket there is. The source of a copy is not a
  download. Readers are named as ADR-0016 names them: the actor; `anonymous`; and for a
  presigned URL the signer, marked presigned and kept apart from the signer's own
  reads. Whether the version that was read is still there is not asked - bytes left the
  bucket - although the record of that version (`access`) is the version's or nobody's.
- **Three and three.** At most `bucket.MaxRecentUploads = 3` uploads and
  `bucket.MaxRecentDownloads = 3` downloads are kept, newest first. A key has one row
  among the uploads; a key, a reader and whether the reader came presigned have one
  row among the downloads; the later takes the place of the earlier and comes to the
  front. Of two rows of the same time the one recorded later comes first - a batch of
  reads is one entry and one time, so its last items are the latest. Six keys of at
  most 1 024 bytes and six names of at most 256 bound the record.
- **Time is the entry's** (ADR-0008): what the leader stamped, never earlier than the
  entry before it, the same on every member - never a clock read while applying. A
  download's time is that of the flush it arrived in, coarse by up to the flush
  interval (ADR-0016).
- **A row outlives its object**: deleting or overwriting an object removes no row. A
  record goes with its bucket, in the transaction that deletes the bucket, and a bucket
  created under the name later begins with nothing - with one exception that is
  accepted: a batch of reads names a bucket by its name alone, so reads of a bucket that
  was deleted, flushed after a bucket of the same name was created (within a flush
  interval; longer while there is no leader), are credited to the new bucket: a key and
  a reader of the old one are then shown to who may see the new one's. Telling the two
  apart needs the read to say which bucket it was of, a field of the entry - a schema of
  its own.
- **A record that cannot be read** - damaged, or of a `v` this binary does not know -
  stops the state machine, as a damaged record of reads and every other stored record
  does. A member that went on with a record of its own making would hold other bytes
  than the others and say nothing.
- Read through `ports.BucketActivityStore.BucketActivities(names)`: one read
  transaction, a point read a bucket, the zero record for a bucket without one; at most
  10 000 names a call.

### Who may see it

Decided in the use case (PLAN G.7), not here: the creator and the activity are for
holders of `admin:GetObjectAccess` on the bucket; the keys in the activity additionally
need `s3:ListBucket`. A row of the activity names an object, so each row is shown only
where the caller also holds `admin:GetObjectAccess` on that object - the request
`/object/access` is gated by (ADR-0016) - and is otherwise left out whole, so that a
deny by key holds in the list. No new action. No event is published for the record: an event
must not carry who read what (ADR-0016).

### Rollback

Possible until the first schema-4 entry is applied; from then on the release before
refuses the state. **In practice that is the first upload, the first flushed download,
or the first bucket created after the last member was upgraded**: at level 4 every put,
copy and completion is an entry of schema 4. A fully upgraded cluster that takes writes
cannot be rolled back. To keep a way back, hold one member at the previous release: the
cluster then stays at level 3, loses nothing it had, and records neither creators nor
latest use.

Three facts for whoever operates it. The rise is automatic at the last registration:
there is no step that finalises it and no switch that holds it, other than a member
left at the release before. A node that runs alone is at level 4 from its first start
with this release: its first upload, or its first download at the next flush or at a
graceful stop, ends the way back. And a cluster that was fully upgraded and is then
rolled back does not lose anything, but no node starts until it is rolled forward
again - it is an outage, not a loss.

A member that joins is not asked for its release before it is added: one of the
release before, joined to a cluster whose state is 4, stops at the first entry of
schema 4 or at the snapshot it is sent. It fails safe; it does not diverge.

## Consequences

- **Cost.** One read and one write of a record - a few hundred bytes as a rule, some tens of kilobytes at the very most, when all six keys are of the longest and of bytes JSON writes as six - in the
  transaction of every put, copy and completion, and one a bucket - not an item - for a
  batch of reads. A record that would not change is not written.
- **It is not evidence**, as the record of reads is not (ADR-0016): a crash loses up to
  an interval of downloads, a compromised gateway reports what it likes, and nothing of
  it is in the hash-chained audit trail.
- Metadata is not encrypted at rest: the creator, object keys and reader names sit in
  the state like the list of readers and the keys themselves.
- "Latest" is since level 4. A bucket that is only read from, or not used at all,
  shows nothing; so does every bucket for the time a cluster is held at level 3.
- At level 4 the proposer can no longer tell from an entry's content which schema it is
  written in: `writtenAt` takes the level. Every place that compared `versionOf` with
  the level compares what `writtenAt` gives.
- The rules of `bucket.Activity` - the bounds, what a row is the only one of, where a
  row of the same time goes, what is left out - are part of the replicated format, like
  those of `object.Access`.
- The known debt of ADR-0016 (two checks of schema 2 that ask the Unicode tables) is as
  it was. What schema 4 added to the state machine looks at bytes and lengths only.

## Alternatives

- **A scan on demand** - walk the versions and the records of reads of a bucket when
  the grid is asked for. Wrong beyond the 100 000 keys a walk is bounded to, forgets
  what was deleted or overwritten (the record of reads goes with its version), and is
  a second walk under the read lock for every row of a list that is not paged.
- **Folding it into the usage walk** (`GET /buckets/{bucket}/usage`) - the same walk,
  with the same bound and the same forgetting, and it would make the count and the size
  wait for it.
- **The audit trail as the source of the creator** - it has no record for a bucket
  created through S3, drops records under flood, is read by a walk of the chain, and
  its retention is not the bucket's.
- **A new command for a backfill** - an entry that sets the creator or seeds the record
  of buckets that exist. It would have to be computed from one member's state by a
  walk, or inside apply by a walk of every bucket in one transaction; and for the
  creator there is nothing to compute it from. Left out: old buckets say "not
  recorded".
- **A new field on the three entries** instead of the schema byte alone - it would say
  nothing the byte does not, and a lenient decoder of schema 3 would drop it.
- **Keeping the record at whatever level** - a member of schema 3 would not, and a
  snapshot taken on one would be sent to members that do.

# ADR-0016: Who uploaded and who read an object - and entry schema 3

- **Status:** accepted; being implemented (PLAN S.1-S.4, B.1-B.5)
- **Date:** 2026-10-12
- **Builds on:** ADR-0003 (metadata), ADR-0005 (cluster, levels), ADR-0008 (entry schema 2,
  the leader's time), ADR-0012 (multipart)
- **With:** ADR-0017 (sub-buckets), ADR-0018 (tag rules and catalogue), which share the
  schema

## Context

Albie wants to see, for an object: when it was uploaded and by whom, how often it was
downloaded, and by whom. Today a version records its time and nothing of its writer,
and a read leaves no trace anywhere but in request metrics.

Recording who wrote is one field. Recording reads is a different matter: a read must
never wait for bookkeeping, fail because of it, or be made slower by a quorum write -
and "who read what" is sensitive in itself.

Sub-buckets (ADR-0017) and tag rules (ADR-0018) also add to the replicated state. A
member of a cluster that does not know a command, or a kind of bucket configuration,
must never meet one: it would refuse what the others accept and the replicas would
part. So all three land as ONE rise of the entry schema.

## Decision

### Entry schema 3

| For | New in the log | New in the state (bbolt) |
|---|---|---|
| Object access | `created_by` on `object.put` and `upload.complete`; command `object.reads.add` | `access/<bucket>`, keyed like the version index; `accessflush/<node>` |
| Sub-buckets | commands `subbucket.put`, `subbucket.delete` | `subbuckets/<bucket>/<name>` |
| Tagging | `bucket.config.put` of kind `tagrules`; `tags` on `upload.create`; commands `tag.definition.put`, `tag.definition.delete` | the `tagrules` document with the bucket's other configuration; `tagdefs/<key>` |

- An entry is of schema 3 when it is one of the new commands, **or** an old command that
  carries what is new: a put or completion with `created_by`, an upload created with
  tags, a bucket configuration of kind `tagrules`. Everything else is written as before,
  at the schema it always had.
- The level rule of ADR-0008 stands: nothing of schema 3 is proposed until every member
  has registered that it knows schema 3. Until then a write that needs it is refused
  with "unavailable" (sub-buckets, tag rules, definitions) or is made without it (the
  uploader is not recorded; reads wait in memory). Nothing is refused that worked
  before.
- A member that meets an entry of a schema it does not know stops, as before. Fields of
  schema 3 found in an entry that claims an older schema are ignored, as ADR-0008 does
  for schema 1.
- The state becomes schema 3 with the first schema-3 entry applied; the binary before
  this one then refuses it. Until that first entry the state is unchanged and the
  previous binary still opens it: rolling back is possible until first use.
- **Migration: none.** A schema-2 state opens as it is. Proved by a fixture written by
  the schema-2 binary (`test/integration/testdata/schema-2`, commit 098749f) and
  `TestUpgradeFromSchema2`, and by a rolling upgrade of a cluster in a test.
- New buckets of bbolt are created when first written. A bucket's deletion removes its
  sub-buckets and access records with it; a version removed for good takes its access
  record in the same transaction.

### The uploader

`object.Version.CreatedBy` is who committed the version, in the words the audit trail
uses for actors (`root`, `user/<name>`, `key/<access key ID>`): whoever put the object,
copied it there, or completed its multipart upload. Versions written before have none
and say so. The time of upload is the version's `ModifiedAt`, as always.

### Reads

- Each gateway counts in memory: `{bucket, key, version, reader, presigned} -> downloads,
  copies`. A read is counted when the body it was given is closed.
- Every 30 seconds (`--access-flush-interval`, at least 5 s) and on a graceful stop, the
  counts are proposed as `object.reads.add`, at most 256 items an entry. **Nothing on the
  path of a GET proposes, waits or can fail for this.** With no leader the counts wait,
  up to 10 000 aggregates; beyond that they are dropped and
  `cactusfs_object_access_dropped_total` says how many.
- A batch carries the node's ID, a number chosen at its start and a sequence number. The
  state keeps the last per node and applies a repeated batch as nothing: a flush whose
  outcome was unknown is simply sent again.
- A version keeps totals and at most 32 readers by name, each with its counts and the
  time of its last read; the reader who read longest ago is folded into "others"
  (`object.Access`). The time is the leader's, of the entry (ADR-0008): coarse by up to
  the flush interval, and the same on every member.

**What counts as a download:** a GET answered 200 or 206 through S3, presigned or not,
and a download through the management API - each ranged GET by itself, and a transfer
that was cut short, because bytes left the store. **A copy's source** is counted apart,
as a copy, once the destination is committed. **What does not count:** HEAD, 304, 412, a
read that could not be opened, and everything the store reads for itself (healing,
scrubbing, collection, lifecycle).

**Who is the reader:** the actor, as above. Without credentials: `anonymous`, one row.
With a presigned URL: the signer, marked presigned and kept apart from the signer's own
reads - whoever held the URL read, not necessarily the signer. No address is kept:
it is personal data, and without bound.

### Who may see it

A new action, `admin:GetObjectAccess`, for the uploader, the counts and the readers
alike. Reading the object does not imply it. Of the built-in roles `cluster-admin` and
`auditor` hold it. `GET /api/v1/buckets/{bucket}/object/access?key=` serves it; the
object list and the object's metadata are unchanged, and so is the S3 API. No request
resets or edits a record. No event is published for a flush: an event must not carry
who read what to a webhook.

## Consequences

- **It is not evidence.** A crash loses up to an interval of counts; a gateway that is
  compromised can report what it likes; the record is not part of the hash-chained
  audit trail. The console calls the figures what they are.
- Counts are of a version. In a bucket that keeps no versions an overwrite starts at
  zero, and deleting the object deletes its record - who then deleted it is not
  recorded either, as S3 deletes are not audited today.
- A client that downloads in parallel ranges counts once a range.
- Metadata is not encrypted at rest; the list of readers sits there, like object keys.

## Alternatives

- **An entry per read** - a quorum write for every GET, a log that grows with read
  traffic, and reads that fail or lose their count with no leader.
- **The audit trail** - one record a read, never deleted, and already dropped under
  flood; a count would be a walk of the chain.

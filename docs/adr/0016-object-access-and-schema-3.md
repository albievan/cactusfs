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
- A member that meets an entry of a schema it does not know stops, as before. What is
  of schema 3 in an entry that claims an older schema is treated exactly as the schema-2
  binary treats the same bytes, so that the two never part: `created_by` and the `tags`
  of an upload are stripped when the entry is applied; a `bucket.config.put` of kind
  `tagrules` is refused, as a kind there is not; and one of the five new commands stops
  the member, as a command it does not know.
- The state becomes schema 3 with the first schema-3 entry in the log - whether it took
  effect, was refused, or was applied as nothing (a batch of reads from a node that is
  not registered): the binary before this one would stop at that entry when it replayed
  the log, whatever became of it, so a state that still called itself schema 2 would
  promise what it cannot keep. The binary before this one then refuses the state. Until that first entry the state is unchanged and the
  previous binary still opens it: rolling back is possible until first use. **First use
  is, in practice, the first upload:** every put, copy and completion carries
  `created_by` once the cluster is at level 3, so a fully upgraded cluster that takes
  writes cannot be rolled back. To keep a way back, hold one member at the previous
  release.
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

A copy made in parts (UploadPartCopy) counts its source once a part that is committed,
as a download in ranges counts once a range - and those counts stay if the upload is
then aborted. A copy of an object onto itself, where versions are not kept, is counted
for nobody: the version it read is gone with the commit.

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

**Listing (added 2026-10-12).** The object list can carry part of it. Asked with
`access=true`, `GET /api/v1/buckets/{bucket}/objects` says of each object of the page
who uploaded its current version and when, how often it was downloaded and copied, and
when last - the five figures, never the readers. This replaces "the object list is
unchanged" above; the object's metadata and the S3 API stay as they are.

- The list is gated by `s3:ListBucket`, as before. `admin:GetObjectAccess` gates
  nothing here: it decides what the page carries. It is therefore asked through the
  authorizer's `Allowed`, and a caller without it gets the plain page, with
  `access_included: false`, and no denial in the audit trail - nobody was refused what
  they asked for by name.
- It is asked for every object of the page, with the request the single read is gated
  by: a policy may give or deny the action by key, and the list must never say of an
  object what `/object/access` would refuse. It is asked for the bucket as well:
  `access_included` is true when the caller holds the action on the bucket or on at
  least one object of the page, so that a page of folders does not lose its columns.
- The records of a page are read in one call and one read transaction
  (`ObjectAccessStore.ObjectAccesses`), by point reads. An object replaced or deleted
  between the listing and that read has zero counts: the record is of the version
  that was listed, or nobody's.
- Nothing is added to the log or to the state: no rise of the entry schema.
- The figures are as fresh as the listing. The console does not ask again by itself: a
  download made from the screen shows after the next flush and the next listing.

The action is one of the `admin:Get...` family. A policy written before this release
that grants `admin:Get*` or `admin:*` therefore grants it too: whoever keeps such a
policy for a "read-only administrator" should know that it now shows who read what.

What a node counted is discarded, without a trace in the metric of dropped reads, when
the node is no longer registered in the cluster and still serves: the cluster takes
counts only from nodes it knows.

## Consequences

- **It is not evidence.** A crash loses up to an interval of counts; a gateway that is
  compromised can report what it likes; the record is not part of the hash-chained
  audit trail. The console calls the figures what they are.
- Counts are of a version. In a bucket that keeps no versions an overwrite starts at
  zero, and deleting the object deletes its record - who then deleted it is not
  recorded either, as S3 deletes are not audited today.
- A client that downloads in parallel ranges counts once a range.
- Metadata is not encrypted at rest; the list of readers sits there, like object keys.

## Known debt

What an entry is refused for must not depend on the build that applies it. The checks
schema 3 added to the state machine hold to that: they count tags, bytes and items and
compare bytes (the tags of an upload, a tag definition, a sub-bucket's name, a read),
and what a tag or a name may consist of is checked by the proposer. Two checks of
schema 2 do not: `object.put` (`applyPutObject`, `versions.go`) and `object.tags.put`
(`applySetTags`, `bucketcfg.go`) refuse tags with `object.ValidateTags`, which asks the
Unicode tables of the Go release the binary was built with whether a character is a
letter, a digit or a space. Two releases built with different Go versions could
disagree about an entry whose tags hold a character that only the newer tables know,
and their replicas would part. The use cases validate tags with the same function
before they propose, so it takes a proposer of a newer build beside a member of an
older one, and such a character. They are left as they are here: changing what an
entry of schema 2 is refused for is itself a change of the replicated format, and needs
its own rise of the entry schema.

## Alternatives

- **An entry per read** - a quorum write for every GET, a log that grows with read
  traffic, and reads that fail or lose their count with no leader.
- **The audit trail** - one record a read, never deleted, and already dropped under
  flood; a count would be a walk of the chain.

A listing that may show the facts and cannot read them fails, as a read that fails
does: it is not answered as if there were none. Who may not see them is not affected.
A version replaced between the listing and the reading of its record is shown with its
own uploader and no counts, until the list is read again.

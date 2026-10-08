# ADR-0012: Multipart uploads

- **Status:** accepted and implemented (PLAN 6.1)
- **Date:** 2026-10-08
- **Milestone:** 6
- **Builds on:** ADR-0008 (versions, entry schema 2), ADR-0009 (Object Lock),
  ADR-0010 (keys of parts, the manifest seal)

## Context

S3 clients send anything large as a multipart upload: begin, send parts in any order and
in parallel, list them, then complete with the list of parts that make the object, or
abort. The SDKs and the AWS CLI switch to it on their own above a few megabytes, so
without it the store cannot hold a large file. The write path of the architecture has to
hold for it unchanged: nothing is acknowledged before it is durable on a write quorum
and committed through Raft, and the Raft commit is the only switch of visibility.

## Decision

### A part is stored like an object; an object can be made of parts

- A part is one encrypted stream (ADR-0010), erasure-coded and written under a data ID
  of its own, flushed on a write quorum, and then recorded by a Raft entry. Only then is
  it acknowledged. The same function writes a whole object and a part.
- Completing an upload writes no data. One Raft entry turns the upload into a version
  whose bytes are those of the listed parts in order. **Nothing is copied.** The version
  record says how many parts it has; where they are is kept beside it.
- A read walks the parts that the range touches, verifying the seal of each before
  reading it. Healing and scrubbing walk every part of a version as they walk the one
  extent of a plain version. Shard storage works on an *extent* - a data ID and a
  layout - and does not know which of the two it belongs to.

### In the metadata state

```
uploads/<bucket>      upload ID                    -> the upload: key, headers, lock, wrapped key
uploadkeys/<bucket>   enc(key) ‖ upload ID         -> nothing; the order ListMultipartUploads needs
parts/<bucket>        upload ID ‖ part number      -> a part of an upload in progress
extents/<bucket>      version data ID ‖ offset     -> a part of a completed object, with its seal
```

- The data ID of the version an upload makes **is the upload ID**. The parts of the
  object are keyed by the offset of their first byte, so the part that holds a byte is
  found with one seek, and a ranged read of a ten-thousand-part object reads the records
  of the parts it needs and no others.
- Four commands, all of entry schema 2: `upload.create`, `upload.part.put`,
  `upload.complete`, `upload.abort`.
- **The `data` index holds every ID in use**: of plain versions' data, of parts' data,
  and of uploads and the versions they became. An entry that proposes an ID for
  something new is refused when the ID is in the index. That is the rule ADR-0009 needed
  ("stored data belongs to one version"), extended: a part cannot name the data of a
  locked version, an upload cannot take the ID of a locked multipart version, and a put
  cannot say it is made of parts at all.

### Completion is decided by the state machine

- Whether the listed parts make an object - ascending numbers, each part stored with
  the entity tag (and any checksum) the client lists, every part but the last at least
  5 MiB, at most 5 TiB in all - is one function of the domain,
  `object.CheckCompletion`. The use case calls it early, to answer without proposing an
  entry; the state machine calls it again against the parts **as the entry finds them**,
  and that call is the one that counts.
- The entry names, for each listed part, the data the gateway sealed. If a part was
  uploaded again in between, the stored data is another and the entry is refused as a
  conflict; the gateway looks again and retries, three times at most.
- The version is committed exactly as a put commits one: the same function decides what
  the bucket's versioning state does with the version before it and what Object Lock
  gives the new one. The lock headers of the request that *began* the upload are kept
  with it; whether a retention can still be kept is judged at completion, on the time of
  that entry (ADR-0009).
- Parts that were uploaded and not listed, and the upload itself, go in the same entry.
- A client that did not hear the answer completes again. The upload is gone by then; if
  the current version of the key is the one this upload made, that is the answer.

### What becomes of data nobody refers to

A part that was uploaded again, the parts a completion did not list, the parts of an
aborted upload and the parts of a multipart version that was removed are dropped from
the index by the entry that makes them unreferenced. The entry's answer names them - up
to a thousand in all, so that it fits what a node accepts from the leader - and the gateway that proposed it removes their shards at once, after
asking the index, as it does for a plain version (ADR-0009). What an answer does not
name, or a gateway does not get to, the collector finds, as it finds all unreferenced
data.

### Parts in progress are data like any other

A part is acknowledged long before its upload is completed, and an upload can take days.
A part committed on a write quorum but not on every drive is queued for healing at once,
as an object is; the scrubber walks the parts of every upload in progress after the
versions of a bucket. Healing a part asks the reference index before it removes anything
it rebuilt, because the part may have been sent again, or its upload completed, while it
was at work.

### Deviations from Amazon S3, and limits

- **Deleting a bucket removes its uploads in progress.** S3 documents that a bucket must
  hold no objects; uploads in progress are not objects, and clients and test suites
  delete buckets that have them. Their parts are dropped in the same entry.
- **Uploads of one key are listed in order of upload ID**, not of the time they began.
- **`UploadPartCopy`, and reading by `partNumber`, answer 501.** UploadPartCopy comes
  with CopyObject (PLAN 6.9).
- **The lock headers are read when the upload begins and refused on its other
  requests**, where S3 ignores them.
- **`CompleteMultipartUpload` must have a body its signature covers**, like every
  request whose body decides what is destroyed or made (ADR-0008).
- **A per-part checksum listed at completion is compared with the one that was verified
  when the part was stored.** There is no checksum of the whole object in the answer.
- **`x-amz-checksum-type` answers 501**: no checksum of the whole object is made.
- Applying a completion, an abort or a bucket deletion costs time in proportion to the
  number of parts, in one entry: ten thousand parts are ten thousand small records.

## Consequences

- `internal/domain/object/multipart.go`, `internal/app/multipart.go`,
  `internal/adapters/raftmeta/uploads.go`, `internal/adapters/s3api/multipart.go`.
- `ports.UploadStore`; `ports.ObjectIndex.ObjectParts`; `PutResult` and `DeleteResult`
  name removed parts.
- New actions: `s3:AbortMultipartUpload`, `s3:ListMultipartUploadParts`,
  `s3:ListBucketMultipartUploads`. Beginning an upload, sending parts and completing are
  `s3:PutObject`. The built-in `read-write` role has all of them: who may begin an
  upload may see and end it.
- A state written by this binary has four more top-level buckets. They are created
  empty when an older state is opened, and nothing in an older state changes: the
  upgrade test begins and completes an upload on a deployment written by the previous
  release.

## Tests

- The metadata contract, run against the Raft store on one node, against every node of
  a cluster, and against the in-memory store the use cases are tested with: parts,
  replacement, completion and what it refuses, an empty last part, versioned and locked
  buckets, abort, listing, and the reference index after each.
- The property test of ADR-0009 - no entry removes a compliance-locked version - runs
  with a locked version made of parts, and with entries that try to take its ID or the
  data of its parts.
- Use cases: round trip and sixty ranges across the boundaries of parts and segments;
  refusals; abort; healing and scrubbing part by part; a part that is not acknowledged
  before it is committed; retried completion; permissions; Object Lock.
- Over HTTP, the operations and forty ways of asking wrongly; a fuzz test of the
  completion document.
- The AWS SDK for Go against the built binary, with its default checksums.
- Four containers: every step of an upload through another node.

Not tested, and known: no test has more parts than one page of part records (a thousand
at completion and healing, sixteen on a read), because a part that is not the last is
5 MiB; the chaos suite does not interrupt a multipart upload; two writers of one part
number at once are covered only by the state machine's rule, not by a concurrent test.

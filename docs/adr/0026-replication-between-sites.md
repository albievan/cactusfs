# ADR-0026: Replication between sites, and the conflict rule

- **Status:** accepted (2026-11-17, PLAN 8.1.11): implemented as written below, with
  the code of PLAN 8.1.1 to 8.1.10 and the gate of each - replica mode and the replica
  requests and questions of the destination (`app.ReplicationService`,
  `internal/adapters/s3api/replica.go`), the client of another site
  (`internal/adapters/s3client`), the replication configuration in S3 and the management
  API, the replicator with the later changes of a version (`app.Replicator`), the resync
  (`app.ResyncWorker`), the status of a bucket's replication with its metrics and alerts,
  two sites in a container test (`TestSiteReplication`) and under chaos
  (`TestChaosReplication`), and the console. No step changed the entry schema, anything
  on disk or the crypto design: schema 5 (ADR-0023) carried all of it. The points under
  "Open, for the owner" are not answered: their defaults are what is built; the questions
  the work added are there (8 to 22) and in PLAN ("Report of 8C"). It was proposed on
  2026-10-12
- **Date:** 2026-10-12
- **Revised:** 2026-10-12, to the owner-side rulings of that day (PLAN, Decisions: "the
  holes the writing of ADR-0023 to 0026 found", points 1 to 5 and 7) and to the domain
  and the ports of PLAN 8.0.4 and 8.0.5; 2026-10-13, to the state machine of PLAN 8.0.9:
  the order of two versions at one sequence, what is decided before a key is looked at,
  and two holes it found (the null version of before versions; a retention that has
  passed); and to the review of PLAN 8.0.8 and 8.0.9 (ruled by the owner, 2026-10-13):
  the copy of a marker has an ID, a result by version is of a bucket that is sent to
  the target, when a resync clears the mark of its target, what "the order is total"
  holds for, and what a lifecycle delete of a late proposer loses; 2026-11-16, to the
  destination as PLAN 8.1.1 built it ("The destination, as built"), to the
  configuration as PLAN 8.1.3 built it and its review amended it ("Configuration", "As
  built"), and to the replicator as PLAN 8.1.4 wrote it and its review amended it, with
  the later changes of PLAN 8.1.5 ("The replicator, as built"); to the resync as
  PLAN 8.1.6 wrote it and its review and verification amended it ("Resync, as built"; open 10 to 14);
  and to the status and the telemetry as PLAN 8.1.7 wrote them and its review amended them
  ("Status and telemetry, as built"; open 15, and 16 closed); to the two-site and chaos
  tests of PLAN 8.1.8 and 8.1.9 and the console of PLAN 8.1.10 ("Two sites, chaos and the
  console, as built"; open 17); 2026-11-17, at its acceptance (PLAN 8.1.11): fail-back
  said as the code does it - a replica version is never sent on, so what crosses back is
  what the new source wrote itself (open 18) - and the open items brought to one list
  (18 to 21 added; 22 at its review)
- **Builds on:** ADR-0023 (entry schema 5: the outbox, targets, outbound connections,
  `high`), ADR-0008 (versions and their sequence), ADR-0009 (Object Lock and the clock
  that decides), ADR-0010 (key hierarchy), ADR-0012 (multipart uploads), ADR-0013
  (lifecycle and its service principal)

## Context

CLAUDE.md, section 4: hybrid means separate clusters per site, linked by asynchronous
replication of buckets - **never one Raft quorum stretched across a WAN**. So two
deployments, each with its own metadata group, its own certificate authority, its own
key service and its own users, have to be made to hold the same versions of a bucket.

Whatever links them is asynchronous, so for a while the two sides differ; and if both
sides took writes, they would differ in ways nobody could order. Something has to say
what happens then. The honest answers are few: decide by clocks, which two sites do
not share; keep both and let somebody choose; or let it not happen.

## Decision

### The conflict rule: there are no conflicts

**Replication is active-passive.** A bucket is written either by clients, or by
exactly one source deployment.

- A **replica bucket** is one with `replica_from` set to the deployment ID of its
  source (`bucket.replica.put`, `admin:PutBucketReplicaMode`). A deployment ID is, for
  this, 1 to 64 letters, digits and hyphens (`object.ValidateReplicaSource`).
- **In a replica bucket, every entry that would make a version or a delete marker is
  refused unless it carries `replica` from that source.** The refusal is in apply
  (`ErrInvalidBucketState`, `409 InvalidBucketState`), so it holds whoever asks - the
  root identity too - and whichever node took the request. The state decides, not the
  caller.
- **Removing a version for good is judged as everywhere**: by the permissions and the
  locks of the replica's own site. So the destination's own lifecycle rules expire its
  noncurrent versions.
- **Promotion** is turning replica mode off, by an administrator of the destination,
  audited. From then on the bucket takes writes and nothing more from the source. That
  is the whole of failing over.
- A bucket has a replication configuration or is a replica, **never both**; both need
  versioning `Enabled`, which cannot be suspended while either holds. Replica mode
  also needs an exact usage record (ADR-0023), which is where `high` is kept.
- **A bucket becomes a replica only while it holds no version at all.**
  `bucket.replica.put` with a source is refused unless the bucket's exact usage record
  says `versions == 0` - delete markers are versions, and the check is free. The
  sequences of two deployments are unrelated numbers: a bucket with a version of its own
  at sequence 5 000 that took its source's versions at 300 would keep the old local
  version the **current** one of its key until the source's sequences passed it, with
  nothing lost and nothing saying so.
- What `bucket.replica.put` with a source answers, in the order it is decided
  (`ports.ReplicationStore.SetBucketReplica`): `ErrBucketNotFound`; `ErrInvalidArgument`
  for a source that is no deployment ID; **nothing changed and no error when the source
  is the one the bucket has**; `ErrInvalidBucketState` when the bucket does not keep
  versions, or has a replication configuration of its own; `ErrConflict` while its usage
  record is not exact; `ErrBucketNotEmpty` when the record says it holds a version
  (`409 BucketNotEmpty` in the management API). Promoting a bucket that is no replica
  changes nothing and is no error.
- **Fail-back is a procedure, not a feature, and it is into an empty bucket**: at the
  old source, make a new, empty bucket a replica of the new source, give the new source
  a configuration that names it, resync, review, promote. The bucket the old source had
  cannot be made a replica as it stands - a version it held and had not sent could
  outrank what the new source wrote since - and nothing does any of this by itself.
  **What crosses back is what the new source wrote itself.** Promotion changes nothing
  of the versions a bucket holds, and the versions it took from its source keep the
  status `REPLICA`, which neither the outbox nor a resync ever sends on ("What is
  replicated": one hop; `bucket.ReplicationConfig.Replicates`, `app.resyncs`). So the
  new, empty bucket at the old source receives the versions written at the new source
  since its promotion, and not those that came from the old source before: those are
  in the old source's own bucket, where they were made, or - if that site was lost -
  only in the promoted bucket, under their IDs, and no replication carries them (open,
  18). *Corrected at PLAN 8.1.11: this paragraph, and "Consequences", said that
  everything crosses again, which the code never did.*

### The source is an S3 client of the other site

The node that leads the source's metadata group sends, as an S3 client, over TLS, to
the endpoint of a target of kind `s3`. Never node RPC, never a shared quorum, never a
certificate of the other site's node CA. Each site keeps its own CA, its own KMS and
its own IAM; all the other site sees of this one is an API key it issued itself.

The client is a small one of our own (`internal/adapters/s3client`) on the request
signer the server already has, moved to `internal/adapters/sigv4`, and on the dialer of
ADR-0023. It speaks path-style.

As built (PLAN 8.1.2): `s3client.New(target, secrets, egress, options)`, the options
carrying this deployment's ID (sent in `x-cactusfs-replica-source` with every probe),
the build's version for the User-Agent and a clock. Every header a request carries is
signed, the replica headers among them; a document is signed by its SHA-256, an object
or a part is sent as signed chunks (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, 64 KiB a
chunk). The target's secret is opened through the sealed opener for each request and
cleared once it is signed; a body of chunks holds the signing key of the day and clears
it when it ends. `-mtime` is written with nine digits of the second
(`2006-01-02T15:04:05.000000000Z`). What the other site answers is classed by status and
S3 code: `QuotaExceeded` is `quota`, `NoSuchBucket` `no_such_bucket`,
`InvalidBucketState` and a 501 `not_replica`, any other 401 or 403 and a refused
signature `auth`. **`rejected` - the version FAILED, never sent again - is kept for what
is known to be of the version itself**: a 413, `EntityTooLarge`, `KeyTooLongError`,
`InvalidTag`, `MetadataTooLarge`; `NoSuchKey` and `NoSuchVersion` of a later change of
the version; and `InvalidArgument` of `PutRetention`, which the other site judges by its
own clock. The other `InvalidArgument` cases this ADR names (a sequence out of range, a
marker without an ID, a retention that has passed) are refused by the client before it
sends, so an `InvalidArgument` of a copy is one of the request ("the headers ... can be
read"), not of the version. Every other 4xx (`InvalidRequest`,
`XAmzContentSHA256Mismatch`, `BadDigest`, 405, 412, ...), another 404 or 409, a 5xx, a
408 and a 429 are `server`, which holds the target and loses nothing. **A 409
`OperationAborted` of a replica put, completion or marker is "maybe there"**: the server
gives that code to every `ErrConflict`, some of which commit nothing, so the client asks
`HeadVersion` and counts the write done only when the version is there with the entity
tag it sent (a marker: there, with none); otherwise `server`. The same holds for an
`OperationAborted` in the body of a completion's 200, and a 200 whose body is no
`CompleteMultipartUploadResult` is `server`. A 404 `NoSuchUpload` of an abort is
success. A HEAD has no body, so a 404 of `HeadVersion` is "not there" and one of the
bucket probe is `no_such_bucket`; a 405 of `HeadVersion` is a delete marker, which is
there; a 2xx without an entity tag is `server`. A 409 of the bucket probe's HEAD or of
`HeadVersion` is `not_replica` (PLAN 8.1.1a: a question is answered 409 only for a
bucket that is no replica of the source). The later changes (`PutTags`, `PutRetention`,
`PutLegalHold`) carry `x-cactusfs-replica-source` too, and a `403 ObjectLocked` of one
is `rejected` - that change alone fails - while one of a copy is `server` ("Object Lock
refusals of a source's request, as built"). A version of a customer's key is refused
before anything is sent (`sse_c`), and a retention that ends within 15 minutes of the
source's clock is left out of the copy ("Open", 7). The client refuses an egress client
that follows redirects, and once a call has returned nothing reads the body it was
given.

### The replica request

The unit is **the object version**, with its **version ID, its modification time and
its sequence at the source preserved**. A replica request is an ordinary S3 request
with headers of our own:

| Request | Headers | Becomes |
|---|---|---|
| `PutObject` | `x-cactusfs-replica-source`, `-version-id`, `-seq`, `-mtime`, `-etag` | `object.put` with `replica: {from, seq, null}` |
| `CreateMultipartUpload` | `x-cactusfs-replica-source` | `upload.create` with `replica_from` |
| `UploadPart` | none; the upload is a replica upload | `upload.part.put` |
| `CompleteMultipartUpload` | all five | `upload.complete` with `replica: {from, seq, null}` |
| `DeleteObject` (no `versionId`) | `x-cactusfs-replica-source`, `-version-id`, `-seq`, `-mtime` | `object.version.delete` that makes the marker, with `replica` |

- `-source` is the deployment ID of the sender; `-version-id` the ID the version has
  at the source (32 hexadecimal characters), **or `null`** for the null version of the
  key; `-seq` its sequence there, in decimal, from 1 to 2^62 (`object.MaxReplicaSeq`);
  `-mtime` its modification time in RFC 3339 with nanoseconds, UTC; `-etag` its entity
  tag, which the destination compares with the one it computes and refuses on a
  difference.
- Content type and the other headers that are stored, user metadata
  (`x-amz-meta-*`), tags (`x-amz-tagging`), retention and legal hold
  (`x-amz-object-lock-*`) travel as in any put.
- **`replica` is `{from, seq, null}`** (`object.Replica`; ADR-0023): the source, the
  sequence the version has there, and whether it is the null version. **A null version
  is stored as the null version of its key at the destination, at the source's
  sequence.** Replication needs versioning `Enabled`, so no version made under a
  configuration is null; a null version is one that was written before the source
  bucket kept versions, and it crosses by a resync. `replica` is said by whoever commits
  the version and is not kept: what is stored of it is the version's `Seq` and its
  status, `REPLICA`.
- **The copy of a delete marker has an ID that is not all zeros**, whether it is the
  null version of its key or not: one without is refused with `ErrInvalidArgument` and
  changes nothing (ADR-0023, "A delete marker has an ID"). A marker of all zeros would
  share its place with the null version of its key, and what the destination holds
  would depend on which of the two arrived first. No source sends one: a marker is made
  with a random ID, also where it becomes the null version of its key.

**The destination refuses a replica request unless every one of these holds** - each
refusal with nothing stored:

| Condition | Otherwise |
|---|---|
| every `x-cactusfs-replica-*` header that is sent is among `SignedHeaders` | `403 AccessDenied` |
| the payload is signed: its SHA-256 in `x-amz-content-sha256`, or signed chunks. Never `UNSIGNED-PAYLOAD`, never a presigned URL | `403 AccessDenied` |
| the caller holds `s3:ReplicateObject` on the object - `s3:ReplicateDelete` for the marker | `403 AccessDenied` |
| the headers the request needs are all there and can be read | `400 InvalidArgument` |
| the bucket is a replica, of that source | `409 InvalidBucketState` |
| the version ID is not one the key already has | `409` (`ErrConflict`): **the source reads this as done** |
| the level is 5 | `503 ServiceUnavailable` |

**`s3:ReplicateObject` and `s3:ReplicateDelete` are granted only by a statement that
names them.** No wildcard includes them - not `s3:*`, not `*` - as with
`s3:BypassGovernanceRetention`: `cluster-admin` and `bucket-admin` do not hold them,
and a bucket policy cannot grant them. A replica request is the one way to make a
version with an ID and a time of the caller's choosing; nobody gets that by having
"everything".

In the code (`iam.Action.byNameOnly`): an Allow covers the two only by naming them, in
a policy of the user; a Deny may still cover them with a wildcard, since denying more is
never wrong; a bucket policy that allows one is refused when it is put
(`ParseBucketPolicy`), and one that was stored all the same grants nothing
(`BucketPolicy.judge`). Whoever may write a bucket's policy cannot give them to
themselves.

A replica header on a bucket that is no replica is refused (`409`), never passed over:
the request is not quietly stored as an ordinary put. A site that does not know the
headers at all would do exactly that, which is why the source asks first (below).

### The destination, as built (PLAN 8.1.1)

Where the text above leaves a choice, the destination takes the stricter reading:

- **The conditions are judged in this order**, each refusal with nothing stored: the
  replica headers are signed (403), the payload is signed (403) - both judged by the S3
  adapter before a byte of the body is read, each audited as a failed authentication
  (`ReplicaHeaderNotSigned`, `ReplicaPayloadNotSigned`) under the key that signed - the
  headers can be read (400), then the use case asks the authorizer for the replicate
  action (403) and checks the bucket is the replica of that source (409) before any
  shard is written; the state checks it again in apply. A malformed header of a caller
  that holds no replicate action is so told 400 rather than 403: it learns nothing but
  that its own headers are malformed.
- **"Signed payload" is the SHA-256 in `x-amz-content-sha256` or signed chunks**, and
  nothing else: not `Content-MD5` or `x-amz-checksum-sha256` beside `UNSIGNED-PAYLOAD`,
  which bind an ordinary body, and not `STREAMING-UNSIGNED-PAYLOAD-TRAILER`.
- **The headers are read exactly**: each once; the kind of request takes exactly its
  headers (a marker with an entity tag, or an upload begun with a sequence, is 400); a
  sequence is decimal digits without a sign or a leading zero; a time is RFC 3339 that
  ends in `Z`, with up to nine fractional digits; an entity tag is 32 lower-case
  hexadecimal characters, then `-` and 1 to 10 000 for a version of parts, quoted or not;
  an `x-cactusfs-replica-*` header that is none of the five is 400. On any request but
  the four of the table, the questions and the later changes of the next sections -
  UploadPart, the delete of a version, a copy, a read, a listing - a replica header is
  refused, never passed over, with **`400 ReplicaHeaderNotTaken`** (since the review of
  PLAN 8.1.1a; it was `400 InvalidArgument`). The code is its own, and not one AWS uses,
  because `s3client` classes an `InvalidArgument` of a retention as `rejected` - the
  other deployment judging the date by its clock - so a destination that refused the
  source header on `?retention` with `InvalidArgument` would make a change FAILED for
  good for what is only a disagreement of releases; `ReplicaHeaderNotTaken` is `server`
  for every call, which holds the target and loses nothing. PLAN 8.1.1 and 8.1.1a land
  in the same commit series and the same release, so no destination of a release
  answers the source header on a later change with `InvalidArgument`.
- **A copy asks for no encryption of its own**: SSE-KMS or SSE-C with replica headers is
  400 (SSE-S3 is the default and asks for nothing).
- **A replica upload is its source's**: a part of it, its abort and the listing of its
  parts need `s3:ReplicateObject`, and `s3:PutObject`, `s3:AbortMultipartUpload` and
  `s3:ListMultipartUploadParts` do not do; any other upload needs the ordinary action.
  Which of the two a caller is asked for first is chosen, before the upload is read, by
  whether it holds the replicate action (`Authorizer.Allowed`, not recorded - the port
  says so); the gate is `Authorize` either way, and a caller that holds both is asked for
  both where the upload needs it. A part of a replica upload must have a signed payload
  (403, audited as a denial `ReplicaPayloadNotSigned`), and UploadPartCopy into one is
  400: the source never sends one. A part of an ordinary upload begun before its bucket
  became a replica is 409 `InvalidBucketState` (decided by the use case; the state would
  refuse the completion anyway).
- **A time** has at most nine fractional digits and a point, never a comma; a replica
  header in another case than net/http gives it (one written by hand) is 400.
- **The copy of a marker that is the null version of its key** carries
  `x-cactusfs-replica-version-id: null`, and is given a random ID of its own here, as
  every marker has one.
- **A completion sent again** after its upload was completed answers the version it
  made, when what is there under its ID has its entity tag, sequence and time and was
  made by that upload - another entity tag is 400 `BadDigest`, anything else 409; a conflict at completion is answered 409 only when the version is there, and
  503 when the parts changed underneath, so that a source never reads parts that changed
  as a version that arrived.
- **Every copy committed is in the audit trail** (`s3:ReplicateObject` or
  `s3:ReplicateDelete`, with the operation, the source, the version and the sequence),
  as a retention lifted is: it makes a version with an ID and a time of the caller's
  choosing. That is what lets PLAN 8.1.9 count the puts a destination took.
- `x-amz-replication-status` is answered on GET and HEAD from the stored status.

The questions of the next section - the bucket probe, `HeadVersion`, the listing and
the abort of replica uploads - are answered as that section says, with the choices
written there under "As built (PLAN 8.1.1a)".

### Object Lock refusals of a source's request, as built (PLAN 8.1.1a)

Object Lock at the destination refuses a change of a version - a compliance retention
shortened or removed, a governance one without the bypass - with `ErrObjectLocked`,
which the S3 API answers `403 AccessDenied`, as AWS does. That is also the answer to a
key that is refused, which `s3client` classes `auth` and which stalls the whole target.
For a source the two must differ: a change that Object Lock forbids is never made
however often it is sent, so it must fail alone (PLAN 8.1.5), while a refused key holds
every version until an operator acts.

- **A request that carried a replica header that was taken** - a copy, an upload begun
  or completed, a marker, a question, or a later change with the source (below) - and
  is refused by Object Lock is answered **`403 ObjectLocked`**: the status any refusal by
  Object Lock has, and a code of its own. The status stays 403 so that nothing but the
  code differs from what an ordinary client is told; the code is not one AWS uses, so no
  client takes it for something else. Every other request, the source's own included
  when it does not carry the header, is answered as before.
- **The later changes carry the source.** `PutTags`, `PutRetention` and `PutLegalHold`
  send a signed `x-cactusfs-replica-source`, and the destination takes the header on
  `PUT ?tagging`, `DELETE ?tagging`, `PUT ?retention` and `PUT ?legal-hold` **with a
  `versionId`** (a change of the current version, or without the version, is 400). With
  it the header and the payload must be signed (403, audited as for a copy), the bucket
  must be the replica of exactly that source (409 `InvalidBucketState`), and a bypass of
  a governance retention is refused (400): a source carries what its version has and
  lifts no lock. The action asked is the ordinary one (`s3:PutObjectTagging`,
  `s3:DeleteObjectTagging`, `s3:PutObjectRetention`, `s3:PutObjectLegalHold`), as
  "Credentials" says.
- **`s3client` classes `ObjectLocked` of a later change `rejected`** - that change is
  FAILED and the target goes on - and of a copy `server`: no copy is refused by Object
  Lock today (a retention that has passed is left out, "Open" 7), and should one be, it
  is held and asked again rather than passed or taken for the key. `AccessDenied` stays
  `auth`.

### What the source asks before it sends, and what it is told

The replication client asks four things of the destination that are no replica
requests: `Check` (the probe of the bucket), `HeadVersion` (is this version there), the
listing of its own replica uploads of a key, and their abort. As ordinary S3 requests
they would need `s3:ListBucket`, `s3:GetBucketVersioning`, `s3:GetObjectVersion` - which
is also the permission to read the version - `s3:ListBucketMultipartUploads` and
`s3:AbortMultipartUpload`, and a key that held those could read every object of the
replica bucket.

**These requests, when they carry a signed `x-cactusfs-replica-source`, are judged as
`s3:ReplicateObject`**, and answer no more than the source needs:

| Probe | Answers | Never |
|---|---|---|
| the bucket (`Check`) | its versioning, in `x-cactusfs-replica-source` the deployment it is a replica of, and in `x-cactusfs-replica-object-lock` whether it has Object Lock (`ports.SiteBucket{Versioning, ReplicaOf, ObjectLock}`; the last since the re-review of PLAN 8.1.4) | a listing |
| a version (`HeadVersion`) | whether it is there, and its entity tag | a GET; any other header of the version; a byte of the object |
| the replica uploads of a key (`ListReplicaUploads`) | the IDs of the uploads in progress whose `replica_from` is that source | any other upload |
| the abort of an upload (`AbortUpload`) | done, for an upload whose `replica_from` is that source | the abort of any other |

The key therefore needs nothing beyond the replicate actions and those of later changes
(below), and still cannot read, list or remove.

As built (PLAN 8.1.1a), the stricter reading each time:

- **Each question is refused unless the bucket is the replica of exactly that source**,
  as a copy is, in the same order: the header signed (403), the payload signed (403),
  both audited as failed authentications; the header readable and alone - a question
  takes `x-cactusfs-replica-source` and no other replica header (400); then
  `s3:ReplicateObject` (403, a denial in the trail like any other); then the bucket
  (404 `NoSuchBucket`) and its source (409 `InvalidBucketState`). A bucket probe of a
  bucket that is no replica of the source is therefore refused rather than answered
  without the header; `s3client` reads a 409 of either probe, and of `HeadVersion`, as
  `not_replica` (a HEAD has no body to name the code). The destination keeps that true:
  `ReplicaBucket` and `ReplicaVersion` answer any other conflict on their way
  (`ErrConflict`, which would be 409 `OperationAborted`) as `ErrUnavailable`, 503, which
  the source asks again (since the review of 8.1.1a).
- **The bucket probes** (HEAD of the bucket, `GET ?versioning`) are judged on the bucket
  (`arn:aws:s3:::<bucket>`); the questions of a key on the object
  (`arn:aws:s3:::<bucket>/<key>`). The policy of the replicate key names both, for each
  replica bucket of the source: `arn:aws:s3:::<bucket>` and `arn:aws:s3:::<bucket>/*`.
- **`HeadVersion`** is taken only with a `versionId` (a HEAD without one is 400, as are a
  condition and a customer key beside the header). It answers 200 with `ETag` and no
  other header of the version - no type, size, time, version ID, metadata, tag count,
  encryption, lock or replication status - 404 for a version or key that is not there,
  and 405 for a delete marker.
- **The listing** takes the key as its `prefix` and lists the uploads of exactly that key
  whose `replica_from` is the source - never another key, never a common prefix (a
  `delimiter` is 400), never an upload that is no copy or is another source's. It pages
  as `ListMultipartUploads` does, by `key-marker` (the key) and `upload-id-marker`; a
  marker before the key lists from its first upload, the key without an upload marker
  or a marker after it lists nothing. Each upload's `Initiator`, like its `Owner`, is
  the caller: which identity of the destination began an upload is not the source's to
  learn (since the review of 8.1.1a).
- **The abort** removes an upload of the key whose `replica_from` is the source. Any
  other upload is, to the source, not there (404 `NoSuchUpload`, which the client takes
  as gone): the answer tells it nothing of uploads that are not its own, and the upload
  stays.
- **The same key without the header** is an ordinary caller: it holds no read, listing or
  abort action, and is refused (403).

**The source sends nothing until the destination's bucket probe has answered with the
source's own deployment ID in `x-cactusfs-replica-source`.** A deployment of the release
before, and any other S3 service, passes over headers it does not know: it would store a
replica put as an ordinary put, with a new version ID, a new time and no sequence, and
answer `200`. Such a site never gives that answer to the probe - only a release that
knows replica mode does - so it is never written to. An answer without the header, or
with another deployment's ID, is the class `not_replica`, which stalls the target.

### Order: by the source's sequence

**A replica version is stored at the sequence it has at the source**, not at the index
of the entry that brought it. It is the current version of its key exactly when no
version of the key has a greater sequence.

So the order of arrival does not matter. A retry, the live stream and a resync can
interleave and arrive in any order: the same set of replica entries gives the same
`objects`, `versions` and `versionids`, byte for byte (the property test of PLAN
8.0.9). And a request that is sent twice is refused the second time with `ErrConflict`,
which the source takes as "it is there" - a refusal changes nothing, so nothing is
counted twice.

**The order is total.** A deployment that numbers its versions itself never has two of
one key at one sequence, but a source at fault may send two. Of two versions at one
sequence the newer is the one whose version ID is the lesser, byte for byte, the null
version before every other - which is the order of the keys of `versions/<bucket>`
(the key, the inverted sequence, the ID). Without it the current version of such a key
would be whichever copy arrived first.

**It is total over versions that differ in their sequence or their ID** - which is every
two versions a source that works sends. Two copies under ONE version ID with different
contents are not ordered by anything: the first to arrive is kept and the later is
refused with `ErrConflict`, which its source reads as "it is there". So for those, and
only for those, the destination depends on the order of arrival. Only a source at fault
sends them - an ID is random and names one version - and the destination cannot tell
which of the two is the version: it keeps one and says nothing false of the other, whose
status at the source becomes `COMPLETED` for contents the destination does not have.
Accepted; a destination that compared the entity tag of the copy with the one it holds
could answer `rejected` instead, and that is the client's to add (PLAN 8.1.4), not the
state's. As built, the replicator asks first (`HeadVersion`): a version that is there
with another entity tag is `FAILED` (`rejected`) and not sent ("The replicator, as
built").

**What the state says of the bucket is decided before anything of the key is looked
at**, and before anything is made: an ordinary write into a replica bucket, a copy from
another deployment, a copy into a bucket that is no replica, and a replica delete that
names a version (`ErrInvalidArgument`: removing a version is never the copy of
anything) are refused first, whatever the key holds, and leave the state as it was, byte
for byte.

In the usage of the bucket a copy is one version of its size more, `high` rises to its
sequence, and what its key counts for among the objects of the bucket is read from where
the copy was put: an older copy that arrives late is not the current version. A copy is
held to the quota of the destination like any put.

After promotion a version made at the destination gets
`Seq = max(index, high + 1)` (ADR-0023), so it is the newest of its key whatever
sequences the source had reached.

**A replica's sequence is from 1 to 2^62** (`object.MaxReplicaSeq`), and
`object.Replica.Validate` - the replicated rule, asked in apply - refuses anything else
with `ErrInvalidArgument` (400 to the source, class `rejected`). Without the bound a
source that was taken over, or one with a fault, could send one version at 2^64-1:
`high + 1` would wrap to 0, every later local version of that key would be older than
it, and no delete marker made at the destination could ever hide it. With it there are
2^62 sequences above every replica, and `high + 1` saturates rather than wraps
(`bucket.Usage.NextSeq`). A sequence is the index of an entry of a deployment's own
log, so no deployment reaches the bound by itself; but a bucket that was promoted above
a replica at 2^62 gives its own versions sequences above the bound, and a third
deployment would refuse those as replicas. That takes a source that sent 2^62 on
purpose, and is accepted.

### What is replicated, and what is not

| | Replicated | Why |
|---|---|---|
| a new version a rule matches, with its content type, user metadata, tags, retention and legal hold | yes | |
| a delete marker made by a caller | when the rule says so (`DeleteMarkerReplication`) | S3's own default is no |
| a delete marker made by a lifecycle rule (`by` is `service/lifecycle`) | **no** - but see below for one written at the upgrade | each site expires by its own rules |
| a later change of the tags, retention or legal hold of a version | yes (PLAN 8.1.5, "Later changes, as built"): as the version has them when the row is sent; a refusal there is `FAILED` | |
| **a version removed for good** | **never** | a source that was taken over must not be able to purge the destination |
| a version encrypted with a key of the customer's (SSE-C) | **no**: status `FAILED`, class `sse_c` | the key is not ours to use |
| a version that is itself a replica | **no** - also after its bucket was promoted (open, 18) | no loops; one hop |
| a version that was there before the configuration | only by a resync | |
| the configuration of the bucket, its policy, IAM | no | each site is administered by itself |
| who uploaded (`created_by`) | no | the destination records the replication key as the uploader |

**Encryption.** The source decrypts and sends inside TLS; the destination encrypts
again under its own data key and its own KMS key. No key and no wrapped key crosses.
An SSE-KMS version arrives as one of the destination's default encryption.

**Large objects.** A version made of parts is sent as a multipart upload with the same
part boundaries: the entity tag is then the same by construction, and it is the only
way past 5 GiB. Stale replica uploads of a key are aborted before a new one is begun.
An object is resumed from the durable cursor - whole; a part is resumed only within
one leader's time.

**A lifecycle delete written as schema 4 loses `by`.** Who deleted is carried by an
entry of schema 5 and left out below (ADR-0023). A proposer that learns of level 5 a
moment late - the lifecycle worker of a node that has not yet seen the last member
register - writes its delete as schema 4, without `by`; the state machine then cannot
tell it from a caller's, and where the deciding rule sends markers on, the marker of
that lifecycle delete is replicated like a caller's. The window is the moment between
the last registration and the proposer seeing it, once in the life of a cluster, and
needs a replication configuration to exist already - which needs level 5 itself, so in
practice a delete that was decided before the upgrade ended and proposed after the
first configuration was put. Nothing is lost: the destination gets a marker its own
lifecycle rules would have made, earlier than it would have.

### Status

Stored on the version, shown as `x-amz-replication-status` on GET and HEAD:

| Value | Written |
|---|---|
| `PENDING` | at commit, when a rule matched and a row for `r:<target>` was appended |
| `COMPLETED` | by a result of `outbox.ack`, when the other site took the version or said it had it |
| `FAILED` | by a result of `outbox.ack`, for a version that cannot be replicated: SSE-C (class `sse_c`), or one the other site refuses for itself (400, 413; class `rejected`) - or a later change of it so refused (PLAN 8.1.5) |
| `REPLICA` | at the destination, on arrival |
| none | no rule matched |

A status is written only on a version that still exists, and never over `REPLICA`.

**A result names a row or a version** (ADR-0023): `{seq, status}` for the version of a
row that is still in the outbox and was for this target as replication,
`{b, k, vid, status}` for the version itself - **of a bucket whose stored replication
configuration names the target that acknowledges, and of no other**: a result for a
version of a bucket that is not sent to that target changes nothing, does not refuse the
acknowledgement, and is counted in its answer, which the worker logs. The live stream reports by row. **A resync reports by version**: what it
sends was there before the configuration, or its row was dropped at the bound, so there
is no row to name, and without this such a version would stay `PENDING`, or without a
status, although the other site has it. Results are taken only from a target of kind `s3`.

**`pending` and `failed` of a bucket are exact.** The bucket's usage record counts its
versions whose status is `PENDING` and `FAILED` (`replication_pending`,
`replication_failed`, ADR-0023), kept by every commit, every result and every removal,
on every member alike. The status of a bucket's replication reads them from there: they
are of the bucket, whatever other buckets use its target, and they are true after a
change of leader - not "what the leader gave up on since it came to lead". What a target
shows of itself (`GET /targets/{name}/status`) is still the leader's memory, apart from
`lost` and `resync_needed`, which the cursor holds.

**A failure of the version is not a failure of the target.** `FAILED` passes the row.
What stalls the target and passes nothing: `auth` (the key is refused),
`no_such_bucket`, `versioning` (not `Enabled` there), `not_replica` (the bucket there
is no replica of this source), `quota` (the destination is full), `object_lock` (a lock
to send to a bucket there without Object Lock; re-review of PLAN 8.1.4) and every
failure of the network. A stalled target is retried with the pauses of ADR-0025, is `stalled` in
its status, and fires `CactusFSReplicationLag`.

Status is stored, not derived from the cursor: after a rule was edited, a target
changed or rows were dropped, a status derived from "is the cursor past it" would call
versions replicated that never were.

### The replicator, as built (PLAN 8.1.4, its review, and PLAN 8.1.5)

`app.Replicator` is the `Deliverer` of the consumers `r:<target>`; the delivery worker of
ADR-0023 hands it the rows of the outbox. Where the text above leaves a choice it takes
the stricter reading. No entry, state or on-disk format and no contract changed:
statuses go through `outbox.ack` by row, as the worker's acknowledgements always did.
One port changed: `ports.SiteClient` gained `ListParts`, and `ports.SitePart` a `Size`
(review of 8.1.4).

- **Who reads.** The version is read through the ObjectService as the replication
  service (`iam.ServiceReplication`, actor `service/replication`), which may
  `s3:GetObjectVersion` and nothing else - no read of a current version by its key, no
  listing, no write; a bucket policy neither binds nor widens it, as for the lifecycle
  service. Each read (`ReplicationVersion` - a delete marker too - `OpenReplicationSpan`
  and `ReplicationParts`) names the version by its bucket, key and reference, reads it
  again and is authorized on that object, decrypts and authenticates every byte as a GET
  does, and is not counted as a download.
- **What is judged, in this order.** A row not for the target as replication, or of a
  target that is not another deployment, is held (`internal`), as a row nothing
  delivers is. A removal for good is passed (never replicated). Then the bucket: one
  the target no longer admits, or whose stored configuration was removed or names
  another target now, is passed **with no status** - the version stays `PENDING`
  (open, 8). The configuration names the bucket of the other deployment. Then the
  version: one that is gone is passed with no status; one that arrived as a replica is
  passed; a row and a version that disagree on being a marker are held. A marker is sent
  only while the deciding rule copies markers and its row does not say
  `by: service/lifecycle` (`ReplicationConfig.Replicates`, as the state decided when
  the row was appended). A version of a customer's key is `FAILED` (`sse_c`) without a
  byte read; one at a sequence above `object.MaxReplicaSeq` (a bucket promoted above a
  replica at the bound) is `FAILED` (`rejected`). **A null version is sent at its own
  sequence, and at 1 when that is 0** (`max(seq, 1)`): a null version made while its
  bucket's versioning was suspended has a sequence of its own, which orders it among the
  versions of its key there as here; only the null version of before versions, stored
  at 0, takes the default of "Open" 6.
- **The probe.** Nothing is sent to a bucket until `Check` says it keeps versions
  (`versioning` otherwise) and is a replica of this deployment (`not_replica`). It also
  says whether the bucket has Object Lock (re-review of 8.1.4: the destination answers
  the probe with `x-cactusfs-replica-object-lock: Enabled`): a version that carries a
  lock - a legal hold, or a retention beyond the margin of "Open" 7 - and a later change
  that sets one are not sent to a bucket without it; the target holds with the class
  **`object_lock`**, which stalls it and is shown in its status until an operator gives
  the bucket there Object Lock or points the rule elsewhere. A change that lifts a lock
  is nothing to carry there, and passed. The answer is taken for a minute, by the
  target's name, the generation of its connection (below) and the bucket, and
  forgotten after any failure there.
- **There already?** Before the bytes of a version are sent, `HeadVersion` asks for it:
  with the entity tag of the version it is `COMPLETED` and nothing is sent - which also
  keeps a new leader from sending its last batch's bytes again; with another entity tag
  it is `FAILED` (`rejected`): the other deployment holds other bytes under the
  version's ID, and takes no others ("Order").
- **Sending.** A version in one PUT; a delete marker with `PutMarker`; a version of
  parts as an upload of the same parts, each with its number and size, read in order
  from `ObjectParts` (a part missing or out of place is `internal` - this side's fault -
  and held). **An upload goes on** (review of 8.1.4): the replicator remembers, within
  its process, the upload begun for a version (by target, the generation of its
  connection, the bucket there and the version), with the entity tag each part sent was
  given; the next
  attempt asks the other deployment for the parts of that upload (`ListParts`, which the
  destination takes without a replica header and judges, for a replica upload, as
  `s3:ReplicateObject`) and sends only those that are not there with the entity tag and
  size they were sent with. The other uploads this deployment left of the key are
  aborted first; that one is not. An upload the other deployment no longer has is
  begun again. An upload is forgotten once completed, once refused for good (and then
  aborted), and the oldest beyond 1 024 uploads or 100 000 parts remembered all together
  - then aborted there, as a best effort, through the client it was begun through.
  **Only an idle upload is forgotten for the bounds** (final verification of 8.1.4): an
  upload an attempt is sending through is pinned, and is never forgotten or aborted;
  when the uploads in flight alone are beyond the bounds, the bounds are exceeded for
  as long as they last - at most `replication-workers` uploads a target - rather than
  abort work in flight, which would only be begun again. A new
  leader begins again from the first part:
  "a part is resumed only within one leader's time". The result: no error `COMPLETED`;
  a failure whose class passes (`rejected`, `sse_c`) `FAILED`; any other holds the
  target and reports nothing. **A version that never finishes holds its target** -
  sent in one request, over a link too slow for its deadline ("Deadlines"), or one that
  fails every time.
- **Later changes, as built (PLAN 8.1.5).** A row of `tags`, `retention` or
  `legalhold` carries the version's tags, retention or legal hold **as the version has
  them now** (`PutTags` - none removes them - `PutRetention`, `PutLegalHold`), so that
  changes sent in the order of the outbox leave the version there as it is here. **The
  version there is asked for first** (`HeadVersion`; re-review of 8.1.4): a change is
  made only to the version this deployment sent, with its entity tag - one that is not
  there, or holds other bytes under the version's ID, is `FAILED` and nothing is sent,
  so a retention or a legal hold is never put on a version that is not this one's. The
  question is asked rather than the version's status read, because statuses are
  acknowledged in batches and may not yet say what became of the version. A
  refusal the other deployment gives for itself - Object Lock's `403 ObjectLocked` (a
  retention shortened or removed, which the source may do with a bypass and the
  destination never takes from a source), a version that is not there, a date it does
  not take - is `FAILED` for the version and the target goes on; a change it takes
  reports **no status**: the version there is as it was when its own row was sent, and
  a change never makes a version `COMPLETED` that its row did not. A retention that ends
  within `object.RetentionMargin` (15 minutes) of the source's clock is nothing to
  carry - the rule a copy follows ("Open", 7) - and is passed with no status; a change
  of a version of a customer's key, which never crossed, is passed (`sse_c`); every
  other failure holds the target.
- **Order and parallelism.** The worker hands `replication-workers` rows over at once
  in waves: never two rows of one object **at the destination** - the target, the
  bucket there that the configuration names, and the key (`Replicator.ObjectOf`; a row
  whose configuration cannot be read goes alone) - stricter than "of one version",
  because the uploads of a key are aborted before a new one is begun, and two versions
  of one key side by side would abort each other's. The rows of one object go in the
  order of the outbox, so a later change never arrives before its version. A wave ends
  before the next begins; the cursor never passes a row that was not taken or passed;
  nothing of a wave that a stop or a lost lead cuts off is acknowledged, so a new leader
  sends again at most one batch and one wave. Each row is paused by its own failures,
  and the target is shown by the row that has failed most.
- **A target that changes.** When a refresh finds the target's connection changed - its
  endpoint, certificates, region or credentials (`delivery.Target.SameConnection`) - or
  a bucket it admitted no longer admitted (the lists compared as sets: one widened or
  put in another order changes nothing), what is in flight to it ends (each attempt runs
  under a context of the run that the refresh cancels) and the row is sent again at once
  to the target as it is; the client of the old one sends nothing more. The replicator
  gives the target's connection a new **generation** then, and when the target was made
  again under its name: what it probed and the uploads it remembered of the connection
  before are forgotten, not resumed at the endpoint the target had (re-review of 8.1.4).
  An attempt made with an older state of the target - by its time of definition, then
  by when it was changed: one that began before the change, or the loop of a target
  deleted since - gets a generation of its own and changes nothing of what is known of
  the target as it is; and at every refresh the worker tells the replicator which
  targets there are (`app.TargetKeeper`), so that it forgets those that are gone (final
  verification of 8.1.4).
- **Rate.** The bytes read to be sent, by every version being sent, are held together
  to `replication-rate-mib`: each read is charged at the rate and waits its turn, with
  no burst above it, so no 10 s see more than 10 s of it and one read (64 KiB) more for
  each version in flight.
- **Deadlines** (as the re-review of 8.1.4 amended them). An attempt at a version has
  the deadline of any attempt (10 s + 2 x `delivery-timeout`) and a day, or the time its
  size takes at 64 KiB/s when that is longer - or at `replication-rate-mib` shared by
  every version that may be sent at once (`replication-workers` to each of the 256
  targets a deployment can have) when that is slower still
  (`app.ReplicationAttemptTimeout`). The bound is generous on purpose: a version sent
  in **one** request cannot go on from where it was cut off, and a deadline that assumed
  a pace would end it on every attempt over a slow link and hold its target for good.
  What ends a transfer that has stopped moving is the egress client, whose connections
  may make no progress for longer than `delivery-timeout`, and a lead that is lost ends
  the attempt at once. **Only a version of parts gets further at each attempt**, going
  on from the parts that arrived; a version in one request starts again from its first
  byte.
- **The client** is made by the composition root's source of publishers
  (`cmd/cactusfs/publishers.go`, `Site`): `s3client.New` over the target's own egress
  client (its certificates the roots, observed by `telemetry.ObserveEgress`), the
  sealed opener of the KMS, this deployment's ID (the topology's) and the build's
  version; kept while the target is the same connection, replaced when it changes, a
  target made again under its name told apart by its time of definition, closed when it
  is gone and when the node stops leading or closes - as a publisher. It is behind
  `telemetry.InstrumentSiteClient` (port `site`), added here because the composition
  root's test refuses an undecorated port; the metrics of replication are PLAN 8.1.7's.
- **Lifecycle markers told apart.** Every delete now says who deleted
  (`DeleteOptions.By`, the actor as the audit trail names it), as a put says who put; a
  delete of the lifecycle worker says `service/lifecycle`, so the state appends no row
  for its marker. A notification of a delete now names who deleted in
  `userIdentity.principalId`, as ADR-0025 says it does.

### Configuration

S3's: `PutBucketReplication`, `GetBucketReplication`, `DeleteBucketReplication`
(`PUT`, `GET`, `DELETE /{bucket}?replication`), authorized as
`s3:PutReplicationConfiguration` (put and delete, as in S3) and
`s3:GetReplicationConfiguration`. The management API has the same document as JSON
under `/buckets/{bucket}/replication`.

**Accepted XML**, decoded strictly:

```xml
<ReplicationConfiguration>
  <Role>arn:cactusfs:replication:::TARGET</Role>
  <Rule>
    <ID>name-of-the-rule</ID>
    <Status>Enabled</Status>
    <Priority>1</Priority>
    <Filter><Prefix>logs/</Prefix></Filter>
    <DeleteMarkerReplication><Status>Disabled</Status></DeleteMarkerReplication>
    <Destination><Bucket>arn:aws:s3:::BUCKET-AT-THE-OTHER-SITE</Bucket></Destination>
  </Rule>
</ReplicationConfiguration>
```

- `<Role>` names the target: there is no role to assume, and S3's document has no
  other place for "through what". The target must exist, be of kind `s3` and admit the
  bucket.
- A rule has `ID`, `Status` (`Enabled`, `Disabled`), `Priority`, a prefix - as
  `<Prefix>` directly in the rule or as `<Filter><Prefix>` - and
  `DeleteMarkerReplication`. Of the enabled rules whose prefix matches a key, the one
  of the greatest priority decides.
- **One target and one destination bucket a source bucket**: every rule names the same
  `<Bucket>`.
- **One destination a source bucket** (review of PLAN 8.1.4): a bucket there takes the
  versions of one bucket here. A configuration that names a target and a bucket there
  that another bucket's configuration names already is refused (`400
  InvalidArgument`) in the one sentence every refusal of a target has, which says
  nothing of the other bucket, by the use case (`ReplicationService.refuseShared`, which
  reads the other configurations only when the target counts a document besides the
  bucket's own, and stops once it has read as many as the target counts; an index of
  the named pairs would be a field more in the replicated state, a schema change).
  Two source buckets in one replica bucket would interleave two unrelated sequences of
  versions there. It is not judged in apply - the applied format is unchanged - so two
  configurations put at the same moment through two nodes may both pass it (open, 9).

**Refused:**

| What | Answer |
|---|---|
| `Filter/Tag`, `Filter/And`, `SourceSelectionCriteria`, `ExistingObjectReplication`, `Destination/StorageClass`, `Destination/EncryptionConfiguration`, `Destination/ReplicationTime`, `Destination/Metrics`, `Destination/Account`, `Destination/AccessControlTranslation` | `501 NotImplemented` |
| rules that name different destination buckets | `501 NotImplemented` |
| a role ARN of another form; a target that is not there, is not of kind `s3`, or does not admit the bucket | `400 InvalidArgument` |
| the bucket's versioning is not `Enabled` | `400 InvalidRequest` |
| the bucket is a replica | `409 InvalidBucketState` |
| suspending versioning while the configuration is there | `409 InvalidBucketState` |
| `GET` when there is none | `404 ReplicationConfigurationNotFoundError` |
| anything else that is not the document above | `400 MalformedXML` |
| any of it below level 5 | `503 ServiceUnavailable` |

**As built (PLAN 8.1.3, and its review)**, the stricter reading where the text above
leaves a choice:

- **A configuration and the replicator are released together.** Once a configuration
  is put, every version a rule matches appends a row for `r:<target>` to the outbox,
  and only the replicator (PLAN 8.1.4) delivers such rows. A release with the one and
  not the other would hold the target's cursor on the first row, stop the trimming of
  the outbox for every target, let it grow towards its bound, and leave the matched
  versions `PENDING`. So PLAN 8.1.3 and 8.1.4 landed in the same commit series
  (bc47af00 to f9f24bf2) and land in the same release, as 8.1.1 and 8.1.1a did.

- **Who and where.** `app.ReplicationService.Configuration`, `SetConfiguration` and
  `DeleteConfiguration` ask the single authorizer for `s3:GetReplicationConfiguration`
  or `s3:PutReplicationConfiguration` on the bucket (a denial is in the trail like any
  other), judge, propose and audit (`s3:PutReplicationConfiguration` with the target,
  the destination bucket and the number of rules, or `replication: removed`). The S3
  adapter (`s3api/replication.go`) and the management API only carry the document; the
  store judges the bucket and the target again in apply, and the applied entry is told
  as `bucket.configuration-changed`. No entry, on-disk format or contract changed:
  `bucket.config.put` of kind `replication` (schema 5) and `/buckets/{bucket}/replication`
  (PLAN 8.0.3) held everything.
- **The order of the judgements**, each refusal with nothing proposed: the document
  (the adapter's `MalformedXML`, `501` and `InvalidArgument`, then
  `bucket.ReplicationConfig.Validate`); the bucket is there (`404 NoSuchBucket`); it is
  no replica (`409 InvalidBucketState`); its versioning is `Enabled`
  (`domain.ErrVersioningRequired`, `400 InvalidRequest` in S3, `400 InvalidArgument` in
  the management API); then the target. A bucket that is a replica keeps versions, so
  the second of the two bucket answers is never hidden by the first.
- **The target** must exist, be of kind `s3` and admit the bucket - a target always
  restricts: one whose `buckets` names none admits none. Which of the three it fails is
  not told: one sentence for all, as for notification rules, so that a bucket's
  administrator learns nothing of targets not open to the bucket. A target the store
  cannot read is passed on as that failure, never taken for one that is not there. The
  store, which judges again when the change is committed, refuses the three in one
  sentence too (since the review of 8.1.3; the text only, not the decision), so that a
  change that raced the use case's own check tells no more.
- **No two enabled rules of one priority may match one key** (since the review of
  8.1.3): two such rules of which one's prefix begins the other's - an empty prefix
  begins every prefix - are refused with `400 InvalidArgument` by the use case, before
  the document is made. Which of them decided would otherwise be settled by the order
  they were put in, and nothing the caller said; in the first form of the schema every
  rule has the priority 0, so any two enabled rules that overlap there are refused. It
  is the use case's rule and not `bucket.ReplicationConfig.Validate`'s: what every
  member applies is unchanged, and `Decides` still settles a tie of a document stored
  before.
- **The document is read strictly.** `<Role>` and at least one `<Rule>` are required, and
  each rule's `<Status>` (`Enabled` or `Disabled`, exactly) and
  `<Destination><Bucket>`: missing, any of them is `400 MalformedXML`, as is a
  `DeleteMarkerReplication` whose `Status` is neither. `<Role>` must begin with
  `arn:cactusfs:replication:::` and the bucket with `arn:aws:s3:::` (`400
  InvalidArgument` otherwise). A `<Priority>` is decimal digits without a sign, a space
  or a leading zero, from 0 to 2^31 - 1 (`400 InvalidArgument` otherwise); left out, it
  is 0. An `<ID>` that is there and empty is `400 InvalidArgument`; one left out is
  given `rule-<n>`, the least `n` the configuration has not taken, as a notification
  rule is. `<Prefix>` in the rule and `<Filter>` in the same rule is `400
  InvalidArgument`; an empty `<Filter>` is every key. **A rule says which keys it is
  for, and one document has one form** (since the review of 8.1.3): a rule with neither
  `<Prefix>` nor `<Filter>`, a document whose rules are of both forms, and a rule of the
  later form (`<Filter>`) without `<DeleteMarkerReplication>` - which S3 also requires
  of it - are `400 MalformedXML`. The elements of the first table
  are `501` wherever they are, before anything else is read; so is the object lock token
  header (`x-amz-bucket-object-lock-token`), as every `x-amz-*` header this API does not
  know.
- **Delete markers are not copied unless a rule says so.** A rule of the later form must
  say (above). S3 copies them by default for a rule of the first form (a `<Prefix>` in
  the rule, no `<Filter>`) that says nothing of them; here such a rule copies none, since
  a marker that crosses can hide a version at the destination.
- **GET writes every rule in the later form**: `ID`, `Status`, `Priority`,
  `<Filter><Prefix>` (empty for every key), `DeleteMarkerReplication` and
  `Destination`, in the order the rules were put; what it writes, put again, is the same
  configuration. DELETE answers `204`, also for a bucket without one.
- **The management API** answers a bucket without a configuration `404 NoSuchEntity`
  (the bucket that is not there is `404 NoSuchBucket`), and a body of no rules `400`: a
  configuration is removed with `DELETE`, never by an empty `PUT`. Each rule must give
  `id`, `enabled`, `priority` and `delete_markers`, as the contract requires (`400`
  otherwise; since the review of 8.1.3): the generated decoder would take one left out
  for its zero value, so the adapter refuses the body before it is decoded
  (`requiredItemFields`, held to the contract by a test).

**The canonical document**, which is replicated and matched in apply
(`bucket.ReplicationConfig.Document`; the plan did not write it down):

    {"v":1,"target","bucket","rules":[{"id","enabled","priority","prefix","markers"}]}

with `prefix` left out when empty and `enabled` and `markers` when false; `priority`
always written; rules in the order they were put. A document has 1 to 100 rules
(`bucket.MaxReplicationRules`) and at most 64 KiB; an `id` is 1 to 255 bytes and unique,
a `priority` from 0 to 2^31 - 1, a prefix at most 1 024 bytes. **Of two rules of one
priority the one that was put first decides** (`Decides`). Its bytes are pinned by the
golden file `internal/domain/bucket/testdata/replication.golden.json`, and
`bucket.ParseReplicationConfig` takes the bytes `Document` writes and no others.

`Match(event, key)` gives the consumer `r:<target>` when a rule decides for the key;
`Replicates(event, key, by, replica, status)` leaves out what `Match` cannot see - a
version that is itself a replica, a marker made by `service/lifecycle`
(`bucket.LifecycleActor`), and a later change of a version that has no status. Both are
part of the replicated format. A row for `r:<target>` is appended for a `put`, a `complete`, a
`marker` when the deciding rule has `markers` and the marker is not the lifecycle
service's, and for `tags`, `retention` and `legalhold` of a version whose status is not
empty - **never for a `delete`**, and never for a version that arrived as a replica.

### Resync

`POST /api/v1/buckets/{bucket}/replication/resync` (`admin:ResyncReplication`) starts,
or starts again, a walk on the node that leads: 500 versions a pass, oldest key first;
for each it asks the other site whether it has the version (`HeadVersion`: there or
not, and its entity tag), and sends it when not. Each version it has dealt with is
reported by a result that names the version (`{b, k, vid, status}`), so that it says
`COMPLETED` or `FAILED` like one the live stream sent. **A null version is sent as the
null version** - `x-cactusfs-replica-version-id: null`, `replica.null` true - and is
stored as the null version of its key there, at the source's sequence: what S3 does for
objects that were there before. The cursor is stored after every pass
(`replication.resync.put`, in `resync/<bucket>`: `bucket.Resync`, with `state` `running`
or `done`, stored with `"v":1` before it like every record), so a new leader goes on from
it. The record needs its bucket and its target to be there, and goes with its bucket. A record whose `state` is `done` clears
`resync_needed` of its target **when the stored replication configuration of its bucket
names that target**, and not otherwise: a walk through a bucket that is not sent there
has sent it nothing.

**The mark is of the target, not of the bucket.** `resync_needed` is one flag on the
cursor of a target, and a resync is of one bucket: the record of one bucket that is done
clears the mark for every bucket that is sent to that target, though another of them
may have lost rows at the bound too. That is the limit of schema 5, and it is kept
outside the state: **the worker resyncs every bucket whose replication configuration
names the target, and writes `done` for the last of them only when all are through**
(PLAN 8.1.6); until then it writes `running`. A mark for each pair of bucket and target
would be a field more in the state, and a schema 6 (ADR-0023, "Open", 6).

It is the way versions from before the configuration get across, and the way a target
that lost rows at the bound of the outbox is made whole.

### Resync, as built (PLAN 8.1.6)

No entry, state, on-disk format, port or contract changed: the record, its entry and
its clearing of `resync_needed` are schema 5's (PLAN 8.0.9), the result by version is
`outbox.ack`'s, and the endpoint was in `admin.yaml` since PLAN 8.0.3.

- **Who starts it.** `ReplicationService.Resync` needs `admin:ResyncReplication` on the
  bucket (the table of "Who": `cluster-admin` only - a bucket's administrator, who holds
  `s3:*`, cannot put this load on the link and on the other deployment), refuses a
  bucket without a configuration (404 `NoSuchEntity`) or whose target is no longer
  another deployment open to it, stores `{bucket, target the configuration names,
  running}` with no cursor and no counts - over whatever was stored, so asking again
  starts again - and is audited (`admin:ResyncReplication`, details `target` and
  `resync: started`). Any node takes the request; the walk is the leader's.
- **Who walks it.** `app.ResyncWorker`, on the node that leads and nowhere else; as the
  delivery worker, it looks every second, and a lead that is lost ends what is in flight
  at once and forgets what it kept in memory. Each round gives every running resync one
  pass.
- **The order.** "Oldest key first" is read as **the order of the version index**: keys
  in order, the versions of a key from the newest - the order the cursor (`key`,
  `version`) can name, and what `ObjectIndex.ScanVersions` walks. The order of sending
  changes nothing at the destination, which orders by the source's sequence ("Order").
  A cursor whose version is gone begins again at its key.
- **What is sent.** A version a rule of the configuration matches (`Match`); a delete
  marker **only when the deciding rule copies markers and the state gave it a status
  when it was made**: a marker without one was made before the configuration or by the
  lifecycle service, which a stored marker cannot tell apart (it keeps no `by`), and a
  marker that crosses can hide a version there - the stricter reading (open, 14); a
  version that arrived as a replica never.
- **`FAILED` is final** (review of PLAN 8.1.6). A version that is `FAILED` is neither
  sent nor asked for nor given another status by a resync, whatever the other deployment
  holds: a change never lifts `FAILED`, and neither does a resync. And as statuses are
  acknowledged at the end of a pass, each version is read again just before: one that
  became `FAILED` meanwhile (the outbox gave up on a change of it), is gone, or is a
  replica, is left out of the acknowledgement, so that a resync never puts a status over
  a newer `FAILED`. The description of `failed` in `admin.yaml` (`BucketReplicationStatus`:
  "falls only when such a version is removed or a resync replicates it") said otherwise;
  PLAN 8.1.7 corrected it ("falls only when such a version is removed: `FAILED` is final").
- **One sender.** Each version goes through the `Replicator` the outbox goes through
  (`Replicator.Resend`): the same reads as the replication service, classes, probe, Object
  Lock hold, uploads that go on, rate and attempt deadlines. It asks first, **a delete
  marker too** (`HeadVersion`; a marker there is there with no entity tag - a row of
  `SiteClientContract`, and of the client's test against the real handler), and sends
  only what is not there; with another entity tag it is `FAILED`. What it counts as
  `sent` is what the other deployment lacked.
- **Never one object twice at once; the outbox does not wait for a resync, but for one
  that it has ended three times.** The replicator lets one send to an object there - the
  name `ObjectOf` gives it: the target, the bucket there and the key - run at a time,
  whoever sends. **A resync waits for nobody**: an object being sent is
  `app.ErrResyncObjectBusy` at once, and the walk stops at that version (no failure
  counted, what came before stored) and comes back to it a second later - the walks pause
  no longer than until the next walk is due. **The outbox waits for another row**, and a
  resync's send to the object runs under a context the outbox ends (`errPreempted`) when
  it comes to send that object; the resync's answer is then `app.ErrResyncPreempted` (an
  `ErrResyncObjectBusy`), what it had sent is found there when it comes back, and
  `ResyncWorker.Progress` counts it (`Preempted`). **After `app.ResyncPreemptions` (3)
  such ends of the send of one version** (verification of PLAN 8.1.6), the outbox waits
  for the next send of that version instead of ending it: a key the outbox writes to again
  and again would otherwise keep a large old version of it from ever crossing. The count
  is the replicator's, in memory, by object and version; it goes when that version's send
  ends by itself, and all counts go beyond 1 024 versions. A row that waits for another
  send beyond its own attempt's deadline fails as a `timeout`, which holds the target and
  is never `FAILED`. Uploads of parts go on from the parts that arrived, so a resync's
  upload that is ended loses no part.
- **A configuration that stops sending the bucket stops the walk.** When the bucket's
  configuration no longer names the target, or the target no longer admits the bucket,
  while a pass walks (review of PLAN 8.1.6), the resync's answer is
  `app.ErrResyncConfiguration` - where a row of the outbox is passed with no status - and
  the pass stops at that version, storing only what came before it; the walk then waits
  (`configuration`, or `target`) and goes on from there when the configuration names the
  target again: no version is passed over.
- **Statuses, then the cursor.** After each pass the statuses of the versions it dealt
  with are acknowledged by version (`{b, k, vid, status}`, with an `upto` behind every
  cursor, which moves nothing), and then the record is stored with the last version dealt
  with and the counts - unless it is what is stored already, which is neither read again
  nor proposed again. A cursor that is stored is therefore a pass whose statuses were
  taken; a pass cut off by the loss of the lead stores nothing, and the next leader walks
  it again and finds there what arrived (`sent` does not count those twice - nor at all:
  it is the count of the leader that sent them, lost with its pass). Before storing, the
  record is read again: one stored otherwise meanwhile - a resync asked again - is not
  overwritten (a narrow race remains between that read and the proposal: a store with a
  compare would be a schema change).
- **Failures.** A failure whose class passes (`rejected`, `sse_c`) is the version's:
  `FAILED`, counted, and the walk goes on. Any other holds the walk at the version, as it
  holds the target: what the pass dealt with before is stored, and the version is tried
  again after the pauses of delivery (`delivery.Backoff`). While held, or waiting for a
  configuration, a target, an object or the other buckets, the record says `running` - as
  the contract says - and what the leader keeps of the walk says why:
  `ResyncWorker.Progress` gives the state of its own sending (`ok`, `retrying`, `stalled`,
  judged as a target's), the class of its last failure, `waiting` (`configuration`,
  `target`, `object`, `others`), and how often it began again (`Restarts`) and had a send
  ended by the outbox (`Preempted`). PLAN 8.1.7 shows it in the metrics, not in the
  status of a bucket ("Status and telemetry, as built").
- **The end, rows lost meanwhile, and the mark.** A pass of fewer than 500 versions ends
  the walk; what follows is decided at the end of the round, once for each target, with
  the targets' cursors read then - after every pass of the round. **When the target lost
  rows since the walk began** (`Cursor.Lost` higher than when this node began the walk or
  took it over) the walk begins again from the first version: a row lost behind its
  cursor would never be sent (review of PLAN 8.1.6; what a change of leader leaves out is
  open, 13). **There is no bound on how often: a resync never ends while its target keeps
  losing rows** - from its second restart each is logged as a warning, and `Restarts`
  counts them (verification of PLAN 8.1.6). Otherwise, when the target does not need a
  resync, the record is stored `done`. When it does, the worker starts the resync of every
  other bucket **that the target admits** and whose configuration names it, and that has
  no running resync of it (audited as the deployment's own: actor `system`, details
  `reason: resync_needed`); it stores `done` for all of them only once every one has been
  through in this leader's time and the target lost no row since the earliest of them
  began, and otherwise begins every one of them again; meanwhile where each that waits
  came to is stored, once. A configuration that cannot be read could name the target:
  nothing is concluded then, the walks wait, and that is logged once until it clears.
- **What it costs.** A round reads the targets, and - every `app.ResyncScanEvery` (10 s)
  - the record of every bucket that a target named by a replication configuration admits,
  a consistent read each; the rounds between read only the records of the resyncs that
  are running. While no bucket has a replication configuration - no target of kind `s3` is
  named by one (`Target.Refs`) - no bucket is read at all, and a bucket no such target
  admits never is (its resync could only wait). The end of the round reads the cursors
  once, and for each target that needs a resync finds the buckets sent to it once, reading
  the configuration of no bucket the target does not admit and stopping once `Refs` of
  them were found; the records it needs of them are those the round read already, and of
  a bucket whose resync is not running one read, once (verification of PLAN 8.1.6: a round
  costs what the target's buckets are, not their square). A round that moved nothing - no
  version looked at, nothing done, started or begun again - proposes nothing and is
  followed by a pause of 10 s at most.
- **What a resync does not carry.** A later change of a version that is already there
  (its tags, retention or legal hold) whose row was lost at the bound: the question asked
  is whether the version is there, not what it carries. Open below, 10.

### Status and telemetry, as built (PLAN 8.1.7)

Written 2026-11-16 and amended by its review and verification the same day (PLAN,
Decisions "8.1.7 review", "8.1.7 verification"); its gate passed that day. No schema,
entry, on-disk, port or Go contract change; the descriptions of `last_error_class` (both
schemas), of `failed`, of `target_state` and of `TargetStatus.state` in `admin.yaml` were
corrected, which changes the comments of the generated code and the web client - each
regenerated on the test host 2026-11-16 (description-only diffs verified).

- **The status of a bucket** (`ReplicationService.Status`, behind
  `GET /buckets/{bucket}/replication/status`; `admin:GetReplicationStatus` on the bucket,
  which `cluster-admin` and `auditor` hold and a `bucket-admin` does not). Any node
  answers, each part from where it is true there. The configuration, the record of the
  resync and the cursors of the targets are consistent reads of the metadata - `lost` and
  `resync_needed` are the target's cursor's (`delivery.Cursor`), whatever release leads
  (review); `pending` and `failed` come from the bucket's usage record
  (`replication_pending`, `replication_failed`), read from the node's own copy **after**
  those reads, so never older than they are; the target's `target_state`,
  `last_error_class` and `oldest_pending_at` come from the node that leads, through
  `ports.DeliveryStatusReader` - the question
  `GET /targets/{name}/status` asks, within 5 s, with the same rules: only words where a
  word belongs, `unsupported` for a leader that cannot be asked, 503 with none to ask or
  one that does not answer or does not know the target yet. 404 without a configuration.
- **The resync in the status is the stored record**, `state`, `scanned`, `sent`, on
  every node alike. What the leader keeps of a walk in memory
  (`ResyncWorker.Progress`: the state and class of its own sending, why it waits, its
  restarts and the sends the outbox ended) is not part of it: it differs from one
  leader to the next, a follower could read it only through a new call of node RPC (a
  change of `api/proto`), and the contract says the resync is the same whichever node
  answers. A resync held, waiting or begun again still says `running`, as the contract
  says. **A resync held is shown all the same, as its target's** (review): a resync sends
  to the target as the outbox does, so the delivery worker shows each target no better
  than its worst held resync (`DeliveryWorkerConfig.Resyncs`, read from
  `ResyncWorker.ResyncHeld` when a status is asked): `retrying` or `stalled`, judged as a
  target's are, with the class of the failure, until the resync's next attempt succeeds,
  the walk ends, or it waits for its configuration or its target - a walk that waits for
  either is held by no failure of a target, and its failures are forgotten (verification
  of PLAN 8.1.7) - in `GET /targets/{name}/status`, in the status of every bucket sent
  there and in `cactusfs_delivery_targets`, on every node, as the leader answers them;
  without it a target that refuses a resync while its outbox is empty said `ok`. Whether
  the contract should gain the rest - restarts and sends the outbox ended - is open, 15.
- **Metrics**, none naming a bucket, a key or a target:
  `cactusfs_replication_bytes_total` (bytes of versions and parts read into requests to
  other deployments, counted by `telemetry.InstrumentSiteClient` as the client reads
  each body, whatever became of the request - the traffic the rate bounds);
  `cactusfs_replication_versions_total{result}` (`completed`, `failed`: every status the
  replicator gives a version, from a row or a resync, `ReplicatorConfig.OnVersion`);
  `cactusfs_replication_lag_entries` and `cactusfs_replication_lag_seconds` (the rows
  that wait for targets of kind `s3`, together, and the age of the oldest - the
  leader's, from the delivery worker's summary, absent elsewhere);
  `cactusfs_replication_resyncs{state}` (`walking`, `held`, `waiting`;
  `ResyncWorker.Summary`, the leader's) and
  `cactusfs_replication_resync_events_total{event}` (`held`, `preempted`, `restarted`;
  `ResyncWorkerConfig.OnEvent`). `object_lock` is now a class of
  `cactusfs_delivery_attempts_total` rather than `other`.
- **The alerts.** `CactusFSReplicationLag`: the age of the oldest row for another
  deployment above 900 s - `cactusfs_replication_lag_seconds`, or, where no node of the
  job exposes it (a leader of an older release, during a rolling upgrade),
  `cactusfs_delivery_oldest_pending_seconds{kind="s3"}`, the same number (review) - at
  once, kept firing for 15 minutes: the threshold and hold of `CactusFSDeliveryStalled`,
  which no longer covers the kind `s3` (one cause, one alert). A stalled target fires it,
  as "Status" above says, and so does a link slower than the writes, or one version that
  takes longer than 15 minutes to cross. `CactusFSReplicationResyncHeld` (review):
  `max by (job) (cactusfs_replication_resyncs{state="held"}) > 0` for 15 minutes - the
  hold of `CactusFSDeliveryStalled` - fires on a resync held whether or not the outbox
  has anything for its target. Tested with `promtool test rules`; the chart carries the
  same file.
- **Decorators.** Every port the status reads is decorated (`InstrumentBucketUsage`,
  `InstrumentDeliveryStatus`, `InstrumentReplication`, `InstrumentMetadata` behind the
  bucket store); the composition root's walk holds it to that.

### Two sites, chaos and the console, as built (PLAN 8.1.8 to 8.1.10)

No product code changed for the two tests, and none of the schema, of anything on disk
or of the Go contract for the console; the contract gained one optional field.

- **Two sites** (`TestSiteReplication`, `test/integration/replication_test.go`; PLAN
  8.1.8). Site A is a cluster of four nodes, site B one standalone node with four drives
  (`clustertest.StartStandalone`), each with its own CA, key file and root. B's bucket is
  made the replica of A's `deployment_id` (`GET /cluster`) before anything is in it, both
  buckets with Object Lock. The key of A at B is that of a user `site-a` whose role holds
  one policy - `s3:ReplicateObject`, `s3:ReplicateDelete`, `s3:PutObjectTagging`,
  `s3:DeleteObjectTagging`, `s3:PutObjectRetention`, `s3:PutObjectLegalHold` on
  `arn:aws:s3:::<bucket>` and `arn:aws:s3:::<bucket>/*` - and whose key carries the same
  document as its inline policy ("Credentials"); the test shows it cannot read, list or
  remove there, write to a bucket that is no replica, or call B's management API. A's
  target names `https://<B>:9000`, B's CA, the region and the key (`auth.mode` `sigv4`).
  Asserted by the two listings of versions, in their document order: 500 versions, 50
  delete markers, a 12 MiB version of parts and a tagged, retained version equal in ID,
  order, size, entity tag, time, tags and retention, the bytes read back from B; a later
  change of tags and retention carried; an SSE-C version `FAILED` at A and absent at B;
  a version removed for good at A still at B; B paused for a minute (the target not
  `ok`, class `timeout`, the puts of the minute pending), then everything arrives; a
  version from before the configuration crosses only by a resync; an ordinary put into
  B's bucket is 409; after promotion a put there is the newest version.
- **Chaos** (`TestChaosReplication`, `test/chaos/replication_test.go`; PLAN 8.1.9). While
  300 versions are written to A, A's leader is killed twice and B is cut off for a minute
  (blackhole routes, `Cluster.CutOff`) and joined again; afterwards every acknowledged
  version is in A's listing under its key and entity tag, both listings are equal, every
  version says `COMPLETED`, B's audit trail holds exactly one committed copy of each
  (`s3:ReplicateObject`) and no other, and the PutObject requests B authenticated exceed
  the versions by at most 100 in all. A copy that arrives again is refused ("Order") and
  writes no record, so the audit trail counts copies and B's
  `cactusfs_s3_requests_total{operation="PutObject"}` counts sends.
- **The console** (PLAN 8.1.10; `web/src/screens/BucketReplication.tsx`). A bucket's
  details show its replication - target, bucket there, rules, and the status
  (`target_state` with its class, pending, failed, lost, the resync record,
  `resync_needed`) - with **Resync** behind a question, and its replica mode with
  **Promote** behind a typed confirmation of the bucket's name. The configuration and
  making a bucket a replica are shown, not edited there (the management API and S3 set
  them). The object browser shows the replication status of an object's current version,
  from the new optional `ObjectMetadata.replication_status` (`PENDING`, `COMPLETED`,
  `FAILED`, `REPLICA`; additive, `oasdiff` clean); an older version's is open, 17.

### Limits

`replication-workers` versions are sent at once (4; 1 to 32). `replication-rate-mib`
bounds the bytes sent, averaged over 10 s (0: not limited). There is no promise of
time: replication is as late as the link and the other site make it, and
`cactusfs_replication_lag_seconds` says how late.

### Credentials, and what they can do

The key the source uses is an API key **of the destination**, held by the source as the
sealed secret of a target (ADR-0023). The destination's administrator makes it for a
user whose policy names, for the replica buckets of this source - each bucket and its
objects, `arn:aws:s3:::<bucket>` and `arn:aws:s3:::<bucket>/*`: `s3:ReplicateObject`,
`s3:ReplicateDelete`, and - for later changes - `s3:PutObjectTagging`,
`s3:DeleteObjectTagging`, `s3:PutObjectRetention`, `s3:PutObjectLegalHold`. That is all
it needs: the probes are judged as `s3:ReplicateObject`.

**What whoever holds that key can do:** add versions and delete markers to the replica
buckets of one source - and so fill them, which is what a quota at the destination is
for (ADR-0024) - and change tags and extend locks of versions there: put a compliance
retention of any length, or a legal hold, on any version of those buckets, which nobody
can lift before its date (open, 22); learn whether a
version is there and its entity tag, whether the bucket keeps versions and whose replica
it is; and list and abort the replica uploads of that source. **What they
cannot do:** read an object, list a bucket, remove a version, shorten a compliance
retention, write to a bucket that is no replica of that source, or do anything at all
to the destination's users, keys and configuration.

**A source that was taken over** can therefore add to the destination and hide current
versions behind delete markers; it cannot remove what the destination holds. Every
version it hid is still there, by its ID.

### Who

| Action | Resource | Built-in roles that hold it |
|---|---|---|
| `s3:GetReplicationConfiguration`, `s3:PutReplicationConfiguration` | the bucket | `cluster-admin`, `bucket-admin` (`s3:*`) |
| `admin:GetReplicationStatus` | the bucket | `cluster-admin`, `auditor` |
| `admin:ResyncReplication` | the bucket | `cluster-admin` |
| `admin:GetBucketReplicaMode` | the bucket | `cluster-admin`, `auditor` |
| `admin:PutBucketReplicaMode` | the bucket | `cluster-admin` |
| `s3:ReplicateObject`, `s3:ReplicateDelete` | the object | **none**; by name only |

A `bucket-admin` can point its bucket at a target the administrator opened to that
bucket, and at nothing else. It cannot make a bucket a replica or promote one.

### Clocks

The modification time of a replica is the source's, and is **shown, never decided
on**. A retention is judged, at each site, by that site's leader (ADR-0009): a
retain-until date that travels is a date, and two sites whose clocks differ agree on
the date and differ, by that much, on the moment it passes. Accepted.

## Consequences

- No conflict can arise, at the price of one writer: a replica bucket is read-only for
  clients until it is promoted.
- **Fail-over loses what had not been sent.** Replication is asynchronous; what the
  source acknowledged in its last moments may not be at the destination. The lag is
  measured.
- A destination can be promoted while the source lives. Then both take writes and the
  two part for good; nothing prevents it but the administrator.
- **Fail-back is into an empty bucket, with a resync, and carries only what the new
  source wrote.** A bucket that holds a version - the old source's own bucket, or any
  bucket that was ever written to - cannot be made a replica; what it held stays where
  it is, under its old name or deleted by hand. What crosses into the new bucket is what
  the promoted bucket wrote itself: the versions it had taken as a replica are never
  sent on (open, 18). That is the price of sequences that mean something at one site
  only, and of one hop; the README says so where it says what fail-back is not
  ("Replication between sites").
- A bucket has to be made a replica before anything is put into it: there is no turning
  an existing copy into a replica.
- The replication key can ask whether a version exists and what its entity tag is. That
  is less than reading, and more than nothing.
- A destination that does not know replica mode is never written to; it shows as
  `not_replica`, like a bucket that is no replica of this source.
- A stalled target stalls every bucket that uses it.
- A multipart object starts again from its first part after a change of leader. An
  object of terabytes over a poor link, with leaders that change, may never arrive.
- The endpoint a target names is a balancer or a node on the S3 port, with TLS passed
  through and no cap on a request below 5 GiB (`docs/load-balancing.md`, "Another
  deployment that replicates through the balancer").
- After a promotion, the old source's configuration fails: the bucket there is no
  replica of it any more, so its target stalls (`not_replica`) - and with it every bucket
  sent through that target - until the configuration is removed at the old source.
- Version IDs are the same at both sites, so a client that fails over keeps the IDs it
  had.

## Alternatives

- **Active-active, the last writer wins by time and site** - two sites have no clock
  in common, and "wins" means an acknowledged write is silently not the current one.
- **New version IDs at the destination, with a mapping** - a third index, and a client
  that fails over holds IDs that mean nothing there.
- **Order of arrival** - a retry or a resync would make an old version the current one.
- **Status derived from the cursor** - see Status.
- **`aws-sdk-go-v2` as the client** - a large graph of dependencies in the binary for a
  dozen calls, and the replica headers and the dial policy are simpler in our own code.
- **Replicating permanent deletes** - a source that was taken over would purge the
  copy that exists to survive it.

## Open, for the owner

Defaults are applied until answered.

1. **Active-passive or active-active?** Default: active-passive, replica mode enforced
   by the state.
2. **Must version IDs be the same at both sites?** Default: yes.
3. **Is a client of our own preferred to `aws-sdk-go-v2`?** Default: yes.
4. **Should a replica bucket refuse the root identity's ordinary writes too?** Default:
   yes; the state decides, not the caller.
5. **Does the root identity hold `s3:ReplicateObject` and `s3:ReplicateDelete`?**
   Default: yes, exactly as it holds `s3:BypassGovernanceRetention` - root is allowed
   before any policy is read and has none that could name an action
   (`iam.Decide`, pinned by `TestRootHoldsTheActionsNoWildcardGrants`). What root cannot
   do is what the state refuses to everybody: a replica into a bucket that is no replica
   of that source. The stricter answer - root holds none of the three actions that are
   granted by name only, and a user must be given them - would change what root can do
   to a governance retention today, and is the owner's to choose. As built (PLAN 8.1.1,
   8.1.1a), the default: root may also ask the questions of any source ("What the source
   asks"), in a bucket that is the replica of that source.
6. **The null version of before versions cannot cross as it stands** (found at PLAN
   8.0.9, for PLAN 8.1.6). A version written while its bucket kept no versions is stored
   at sequence 0, as it always was; a replica's sequence is from 1 (`Replica.Validate`).
   The ruling says that a null version is resynced "at the source's sequence", which for
   exactly the versions it is about is 0, and refused. Default until ruled: the source
   sends such a version at sequence 1 - below every version its bucket made since it
   keeps versions, whose sequences are indexes of entries far above - and nothing of the
   replicated format changes. As built (PLAN 8.1.4): `max(seq, 1)` - a null version
   written while versioning was suspended has a sequence of its own, at which it is
   sent, and only one stored at 0 takes the default.
7. **A retention that has passed at the destination refuses the copy** (found at PLAN
   8.0.9, for PLAN 8.1.4). A copy travels with its retention, and Object Lock at the
   destination refuses a retain-until date that is not in the future, by its own clock
   - so a version whose retention ended before it was sent is refused with
   `ErrInvalidArgument`, which the source reads as `rejected` and `FAILED`. Default
   until ruled: the source leaves a retention out that has passed by its own clock -
   with a margin (PLAN 8.1.2 review): one that ends within 15 minutes of the source's
   clock (`s3client.RetentionMargin`, the skew a SigV4 signature is taken within) is
   left out too, so that clocks that differ and the time a copy takes to send do not
   make the destination refuse it. The price: such a copy is unretained at the
   destination for the at most 15 minutes its retention had left; its legal hold goes
   with it all the same. As built, the margin is `object.RetentionMargin` (since the
   re-review of PLAN 8.1.4; `s3client.RetentionMargin` is the same constant), and a
   later change of a retention that ends within it is passed too.
8. **A row whose bucket is no longer sent to its target** (found at PLAN 8.1.4). A
   configuration removed, or pointed at another target, before its rows were sent
   leaves their versions `PENDING`, counted in `replication_pending`, although nothing
   will send them: the replicator passes such rows with no status. The stricter
   reading for the counts would be `FAILED`, which this ADR keeps for a version that
   cannot be replicated at all. Default: no status; a resync of the new configuration
   settles them.
9. **Two configurations naming one bucket there, put at the same moment** (found at
   the review of PLAN 8.1.4). The rule "one destination a source bucket" is the use
   case's: two puts that race through two nodes may each read the other's absence, and
   both be committed. Judging it in apply would change what every member applies - a
   schema change. Default: the use case's check; an operator who finds two buckets
   sent to one there removes one configuration.
10. **A later change lost at the bound is not resynced** (found at PLAN 8.1.6). A
    resync asks whether a version is there and sends it when not; a version that is
    there already keeps the tags, retention and legal hold it had when it crossed, and
    a later change of it whose row was dropped is not carried. Carrying them would mean
    a `PutTags`, `PutRetention` and `PutLegalHold` for every version walked - three
    requests more where there is one now, and a retention there that Object Lock
    refuses to shorten. Default: not carried; the next change of the version carries
    all of it, as a change carries the version as it is now.
11. **The walk is one version at a time** (PLAN 8.1.6). The outbox sends
    `replication-workers` at once; a resync sends one version after another, each after
    its question - a round trip a version, so a bucket of millions of small versions
    over a link of 10 ms takes hours. Default: one at a time, which keeps the cursor
    simple (a pass is stored up to the version it is held at); waves as the outbox has
    them would be a change of the worker only.
12. **What a pass sent before a change of leader is not counted** (PLAN 8.1.6). `sent`
    of the record is the count of the passes that were stored; the versions a pass sent
    before its lead was lost are found there by the next leader, which does not send or
    count them. Default: `sent` is a lower bound, as the contract's "versions the other
    deployment did not have and was sent" reads most strictly.
13. **Rows lost before a change of leader are not seen by the next** (review of PLAN
    8.1.6). A walk begins again when its target lost rows since it began, as the node
    that walks it saw `Cursor.Lost` when it began - or when it took the walk over. Rows
    lost while an earlier leader walked, behind the cursor it stored, are counted in
    `Lost` before the new leader first reads it, and the walk is `done` without them.
    `resync_needed` was set by them all the same; the next resync asked for sends them.
    Closing the gap means keeping `Lost` at the beginning of the walk in its record - a
    field more in `replication.resync.put`, so a schema change. Default: the gap, and
    `resync_needed` cleared by a walk that did not see the loss.
14. **A user's delete marker made before the configuration is never resynced** (review
    of PLAN 8.1.6; for Albie). A resync sends a marker only when the state gave it a
    status when it was made, because a stored marker does not say who made it and a
    lifecycle marker must not cross. So a marker a user made before the bucket had a
    configuration stays here only; S3 Batch Replication copies such a marker. Keeping
    who made a marker would be a field more of a version - a schema change. Default:
    not sent - a marker that crosses can hide a version at the destination.

15. **A resync's restarts and preemptions are not in the status** (PLAN 8.1.7, narrowed
    by its review). The contract's `ReplicationResync` has `state`, `scanned` and `sent`;
    a resync held by a failure shows as its target's state and class (above), but its
    restarts, the sends the outbox ended and why a walk waits (configuration, target,
    object, others) are the leader's memory, shown in the metrics
    (`cactusfs_replication_resyncs{state}`, `cactusfs_replication_resync_events_total{event}`)
    and the log. Showing them per bucket means new fields in the contract and a call of
    node RPC that asks the leader (a follower cannot read them otherwise). Default: not in
    the status; the console of PLAN 8.1.10 is the natural place to decide it. PLAN 8.1.10
    kept the default: the console shows the stored record (`state`, `scanned`, `sent`)
    and its target's state and class, and nothing of the leader's memory.
16. *Closed by the review of PLAN 8.1.7:* a resync that stays held has an alert,
    `CactusFSReplicationResyncHeld` (held for 15 minutes), and shows in its target's
    status.
17. **The admin API tells the replication status of the current version only** (PLAN
    8.1.10). `ObjectMetadata.replication_status` (optional; `PENDING`, `COMPLETED`,
    `FAILED`, `REPLICA`) is the status of the version `GET /buckets/{bucket}/object/metadata`
    reads, which is the current one: that operation takes no `version_id`, and the admin
    API lists no versions. The S3 API tells any version's (`x-amz-replication-status` on
    GET and HEAD with `versionId`). Telling an older version's through the admin API
    needs `version_id` on `/object/metadata`, judged as `s3:GetObjectVersion` as
    `/object/preview` is, and a listing of versions for the object browser to choose
    from - an additive contract change. Default: the current version only, as the object
    browser works today.
18. **Fail-back carries only what the promoted bucket wrote itself** (found at PLAN
    8.1.11, writing the README's fail-back from the code). Promotion turns replica mode
    off and changes nothing of the versions: those that arrived from the source keep
    `REPLICA`, and a replica is never sent on - not by the outbox
    (`ReplicationConfig.Replicates`, part of the replicated format) and not by a resync
    (`app.resyncs`). A fail-back into a new, empty bucket at the old source therefore
    receives the versions written at the new source since its promotion, and none of the
    history that came from the old source. While the old site and its bucket survive,
    that history is there; when the old site was lost, it exists at the new source only,
    as replicas, and the only way to carry it on is to write it again (an S3 copy within
    the bucket, which makes new versions with new IDs and times). Carrying it would mean
    sending a replica on after its bucket's promotion - a change of `Replicates` and of
    the resync, and of the rule "one hop", so a schema change and a new ruling. Default:
    not carried; the README says so.
19. **The start of a resync is audited after its commit, and may be overwritten in a
    narrow race** (review of PLAN 8.1.6). `replication.resync.put` carries no audit
    record, so `ReplicationService.Resync` writes the record once the start is committed:
    a node that stops in between leaves a resync started and unaudited, as a change of a
    bucket's policy, CORS or lifecycle can be. And between the walk's read of the record
    (`save`) and its proposal, a resync asked again can be overwritten by the pass before
    it, so that the walk goes on from its cursor instead of from the first version. Both
    need a field more in the entry or a store with a compare - a schema change. Default:
    as built.
20. **A rule of the first form that says nothing of delete markers copies none**
    (PLAN 8.1.3 (6)). S3 copies them by default for such a rule; here a marker crosses
    only when a rule says so, since a marker that crosses can hide a version at the
    destination. A client that relies on S3's default sees markers stay at the source.
    Default: the stricter reading; README and `GET ?replication` (which writes every rule
    in the later form, with `DeleteMarkerReplication` said) show what was kept.
21. **`CactusFSDeliveryStalled` no longer covers another deployment** (PLAN 8.1.7). The
    alert now reads `cactusfs_delivery_oldest_pending_seconds{kind!="s3"}`, and the same
    moment for a target of kind `s3` fires `CactusFSReplicationLag` - one cause, one
    alert. An operator who routed `CactusFSDeliveryStalled` for every target must route
    `CactusFSReplicationLag` (and `CactusFSReplicationResyncHeld`) as well. Default: as
    built.
22. **The replication key can lock the replica for good** (review of PLAN 8.1.11). The
    key holds `s3:PutObjectRetention`, `s3:PutObjectLegalHold`, `s3:PutObjectTagging`
    and `s3:DeleteObjectTagging` on the replica bucket so that later changes can be
    carried ("Credentials"), and the destination takes a later change without
    `x-cactusfs-replica-source` as an ordinary one (`ObjectService.admitChange` judges
    the source only when the header is there). So whoever holds the key can put a
    compliance retention of any length, or extend one, and legal holds on any version of
    the replica bucket, and rewrite or remove tags there: storage that nobody,
    `cluster-admin` included, can free before the date. Should a replica bucket take a
    later change from the source's key only with the signed source header - or take
    later changes from no key but with it? Either is a change of the destination's
    rules (not of the entry schema). Default: as built; "What whoever holds that key can
    do" above says "extend locks".

Ruled on 2026-10-12 and now in the text above: the probes of the replication client are
judged as `s3:ReplicateObject` and answer existence and entity tag only; replica mode
only on a bucket without versions, and fail-back into an empty one; results of
`outbox.ack` that name a version, for what a resync sends; null versions resynced as the
null version at the source's sequence; `pending` and `failed` of a bucket from two
counters of the usage record; and the source sending nothing until the bucket probe has
answered its own deployment ID.

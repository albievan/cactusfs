# ADR-0026: Replication between sites, and the conflict rule

- **Status:** proposed (2026-10-12). It becomes accepted when the code of PLAN 8C has
  landed and the gates have seen it (PLAN 8.1.11)
- **Date:** 2026-10-12
- **Revised:** 2026-10-12, to the owner-side rulings of that day (PLAN, Decisions: "the
  holes the writing of ADR-0023 to 0026 found", points 1 to 5 and 7) and to the domain
  and the ports of PLAN 8.0.4 and 8.0.5
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
  old source, make a new, empty bucket a replica of the new source, resync all of it,
  review, promote. The bucket the old source had cannot be made a replica as it stands -
  a version it held and had not sent could outrank what the new source wrote since - and
  nothing does any of this by itself.

### The source is an S3 client of the other site

The node that leads the source's metadata group sends, as an S3 client, over TLS, to
the endpoint of a target of kind `s3`. Never node RPC, never a shared quorum, never a
certificate of the other site's node CA. Each site keeps its own CA, its own KMS and
its own IAM; all the other site sees of this one is an API key it issued itself.

The client is a small one of our own (`internal/adapters/s3client`) on the request
signer the server already has, moved to `internal/adapters/sigv4`, and on the dialer of
ADR-0023. It speaks path-style.

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
| the bucket (`Check`) | its versioning, and in `x-cactusfs-replica-source` the deployment it is a replica of (`ports.SiteBucket{Versioning, ReplicaOf}`) | a listing |
| a version (`HeadVersion`) | whether it is there, and its entity tag | a GET; any other header of the version; a byte of the object |
| the replica uploads of a key (`ListReplicaUploads`) | the IDs of the uploads in progress whose `replica_from` is that source | any other upload |
| the abort of an upload (`AbortUpload`) | done, for an upload whose `replica_from` is that source | the abort of any other |

The key therefore needs nothing beyond the replicate actions and those of later changes
(below), and still cannot read, list or remove.

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
which the source takes as "it is there".

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
| a delete marker made by a lifecycle rule (`by` is `service/lifecycle`) | **no** | each site expires by its own rules |
| a later change of the tags, retention or legal hold of a version | yes (PLAN 8.1.5; the first thing to be cut) | |
| **a version removed for good** | **never** | a source that was taken over must not be able to purge the destination |
| a version encrypted with a key of the customer's (SSE-C) | **no**: status `FAILED`, class `sse_c` | the key is not ours to use |
| a version that is itself a replica | **no** | no loops; one hop |
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

### Status

Stored on the version, shown as `x-amz-replication-status` on GET and HEAD:

| Value | Written |
|---|---|
| `PENDING` | at commit, when a rule matched and a row for `r:<target>` was appended |
| `COMPLETED` | by a result of `outbox.ack`, when the other site took the version or said it had it |
| `FAILED` | by a result of `outbox.ack`, for a version that cannot be replicated: SSE-C (class `sse_c`), or one the other site refuses for itself (400, 413; class `rejected`) |
| `REPLICA` | at the destination, on arrival |
| none | no rule matched |

A status is written only on a version that still exists, and never over `REPLICA`.

**A result names a row or a version** (ADR-0023): `{seq, status}` for the version of a
row that is still in the outbox and was for this target, `{b, k, vid, status}` for the
version itself. The live stream reports by row. **A resync reports by version**: what it
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
is no replica of this source), `quota` (the destination is full) and every failure of
the network. A stalled target is retried with the pauses of ADR-0025, is `stalled` in
its status, and fires `CactusFSReplicationLag`.

Status is stored, not derived from the cursor: after a rule was edited, a target
changed or rows were dropped, a status derived from "is the cursor past it" would call
versions replicated that never were.

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
or `done`), so a new leader goes on from it. A record whose `state` is `done` clears
`resync_needed` of its target.

It is the way versions from before the configuration get across, and the way a target
that lost rows at the bound of the outbox is made whole.

### Limits

`replication-workers` versions are sent at once (4; 1 to 32). `replication-rate-mib`
bounds the bytes sent, averaged over 10 s (0: not limited). There is no promise of
time: replication is as late as the link and the other site make it, and
`cactusfs_replication_lag_seconds` says how late.

### Credentials, and what they can do

The key the source uses is an API key **of the destination**, held by the source as the
sealed secret of a target (ADR-0023). The destination's administrator makes it for a
user whose policy names, for the replica buckets of this source: `s3:ReplicateObject`,
`s3:ReplicateDelete`, and - for later changes - `s3:PutObjectTagging`,
`s3:PutObjectRetention`, `s3:PutObjectLegalHold`. That is all it needs: the probes are
judged as `s3:ReplicateObject`.

**What whoever holds that key can do:** add versions and delete markers to the replica
buckets of one source - and so fill them, which is what a quota at the destination is
for (ADR-0024) - and change tags and extend locks of versions there; learn whether a
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
- **Fail-back is into an empty bucket, with a full resync.** A bucket that holds a
  version - the old source's own bucket, or any bucket that was ever written to - cannot
  be made a replica; what it held stays where it is, under its old name or deleted by
  hand, and everything crosses again. That is the price of sequences that mean
  something at one site only; the README is to say so where it says what fail-back is
  not (PLAN 8.1.11).
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
  through and no cap on a request below 5 GiB (`docs/load-balancing.md`).
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
   to a governance retention today, and is the owner's to choose.

Ruled on 2026-10-12 and now in the text above: the probes of the replication client are
judged as `s3:ReplicateObject` and answer existence and entity tag only; replica mode
only on a bucket without versions, and fail-back into an empty one; results of
`outbox.ack` that name a version, for what a resync sends; null versions resynced as the
null version at the source's sequence; `pending` and `failed` of a bucket from two
counters of the usage record; and the source sending nothing until the bucket probe has
answered its own deployment ID.

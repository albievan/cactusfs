# ADR-0024: Quotas

- **Status:** proposed (2026-10-12). It becomes accepted when the code of PLAN 8A has
  landed and the gates have seen it (PLAN 8.3.6)
- **Date:** 2026-10-12
- **Revised:** 2026-10-12, to the owner-side rulings of that day (PLAN, Decisions: "the
  holes the writing of ADR-0023 to 0026 found", point 8) and to the domain and the two
  mappers of PLAN 8.0.4
- **Builds on:** ADR-0023 (entry schema 5, the usage record), ADR-0008 (versions),
  ADR-0009 (Object Lock), ADR-0012 (multipart uploads), ADR-0013 (lifecycle)

## Context

The threat model has had a row "one tenant fills the cluster" with nothing behind it.
A bucket could grow until the drives were full, and then every bucket's writes failed.

Two things make a limit harder than it looks.

- **Writes race.** Two writers, each within what is left, are together over it. The
  nodes that take the two requests do not know of each other; only where entries are
  ordered is there one answer.
- **What a bucket keeps is not what a listing shows.** With versioning, every overwrite
  and every delete keeps what was there. A multipart upload that is never completed
  keeps its parts. A limit on the current versions would be no limit at all.

## Decision

### What a quota is

A **hard limit, in bytes, on one bucket**: `quota_bytes` in the bucket record, from 1 to
2^62 (`bucket.MaxQuotaBytes`, `bucket.ValidateQuota`); 0 is no quota, and is not valid as
one. What counts against it is

    used = stored_bytes + upload_bytes

of the bucket's usage record (ADR-0023): every stored version, current or not, by the
size of the object as it was sent; delete markers as nothing; and the parts of uploads
that are neither completed nor aborted. Not the bytes on the drives, which are more by
the parity and are the operator's concern, not the tenant's. Not the record's
`replication_pending` and `replication_failed` either: they count versions, for the
status of replication (ADR-0026).

There is no quota by number of objects, by user or by tenant, and no soft quota.

### Where it is enforced

**In apply, and that is the authority.** `applyPutObject` and `applyPutPart` refuse
with `domain.ErrQuotaExceeded` (wire code `quota_exceeded`) when the **net** change of
the entry is positive and `used + net > quota_bytes`. Net is what the entry adds minus
what the same entry frees. A refused entry changes nothing.

The decision is one function of the domain, `bucket.Usage.Admits(quota_bytes, delta)`,
and it is part of the replicated format: a change is admitted when the bucket has no
quota, or when its net (`UsageDelta.Net`: the change of `stored_bytes` plus the change of
`upload_bytes`) is 0 or less, however full the bucket is; and otherwise exactly when
`used <= quota_bytes` and `net <= quota_bytes - used`. Written so, nothing overflows.

**Counters at their bounds.** No count of the usage record goes below 0 or past
`math.MaxInt64`: a change that would take one there leaves it at the bound, on every
member alike (ADR-0023), and `used` and the net of a change are held to the same bounds.
For the quota that means: a count is held at 0 only when more was taken from it than
was ever counted, which no change of a real bucket does - it is a fault of whoever kept
the record, and a recount finds it as drift; and a count held at `MaxInt64` is over every
quota there can be, since a quota is at most 2^62 - the bucket takes nothing that adds,
and can still be emptied. A clamp begins a recount in the entry that clamped
(`Usage.Add` says so and returns the record with a count begun; ADR-0023), is counted
in `cactusfs_bucket_usage_clamped_total`, and the recount puts right what was clamped
and reports it as drift. Until it ends the quota is judged by the clamped figure.

| Entry | Adds | Frees | Can be refused |
|---|---|---|---|
| put, versioning `Enabled` | the size | nothing | yes |
| put, bucket without versions or `Suspended` | the size | the null version it replaces | yes, when it is larger than what it replaces |
| copy | as a put (it arrives as one) | as a put | yes |
| part | the size of the part | the part of the same number it replaces | yes |
| completion of an upload | the size of the version, out of `upload_bytes` into `stored_bytes` | the parts that were not used; the version it replaces where versions are not kept | **never**: it does not raise `used` |
| abort | nothing | the parts | never |
| delete that makes a marker | a version of no bytes | nothing | never |
| delete of a version for good | nothing | the size | never |
| what a lifecycle rule removes | nothing | as a delete | never |
| a replica put (ADR-0026) | the size | nothing | yes: the quota of a replica bucket is its own site's |
| tags, retention, legal hold | nothing | nothing | never |

**A full bucket can always be emptied**: nothing that removes is ever refused, and a
delete that makes a marker costs nothing.

**A pre-check in the use case**, before a shard is written: `ObjectService.put`,
`uploadPart`, `Copy` and `CopyPart` ask `QuotaService.Admit` with the declared length,
answered from a local read. It is advice, and exists so that a request that cannot
succeed does not first write its shards. With a length that is not known it refuses
only when the bucket is full already.

**The race** of two writers each within the quota is settled in apply: one entry is
applied first, the other is refused. The loser's shards are removed as for any commit
that was refused - `ObjectService.write` does that today - and until then by the
collector. So the quota is never passed, and what a refused write costs for a moment is
drive space, not quota.

### Setting it

- `bucket.quota.put` sets or removes (`bytes` 0). It is refused with `ErrConflict`
  (409) unless the bucket's usage record is exact: a bucket made by a release of schema
  5 is exact from the start; one that was there before is counted without being asked
  (ADR-0023).
- A quota below what the bucket holds is accepted. Nothing is removed; nothing more is
  taken until enough was deleted.
- Below level 5 everything of quotas answers "unavailable" (503).

### The error

- **S3: `403 QuotaExceeded`**, message "The bucket quota has been reached.".
  `QuotaExceeded` is no code of AWS S3, which has no bucket quota; it is the code Ceph's
  gateway uses, and clients that know quotas know it.
- **Management API: 403 `application/problem+json`, `code` `QuotaExceeded`**, `title`
  "The bucket quota has been reached", `type` `about:blank` like every problem.
- Both are in the code as of PLAN 8.0.4 - `s3api`'s table of errors and `adminapi`'s
  `problemOf`, the only two places where `domain.ErrQuotaExceeded` becomes HTTP - and a
  test in each adapter holds every sentinel of `domain.Sentinels()` to a row.
- **Why 403 and not 507.** An SDK does not retry a 4xx: the caller is told at once, and
  once. A 507 reads as a fault of the server, is retried with backoff, and tells the
  wrong person. The S3 compatibility suite gains a test that the SDK makes one attempt.
- A refusal is distinguishable from a denial by its code, never by its status: the
  refusal says nothing a caller who may write does not already know.

### Who

| Action | Resource | Built-in roles that hold it |
|---|---|---|
| `admin:GetBucketQuota` | the bucket, `arn:aws:s3:::<bucket>` | `cluster-admin` (`admin:*`), `auditor` (`admin:Get*`) |
| `admin:PutBucketQuota` | the bucket | `cluster-admin` |
| `admin:DeleteBucketQuota` | the bucket | `cluster-admin` |
| `admin:RecountBucketUsage` | the bucket | `cluster-admin` |

- **`bucket-admin` holds none of them.** `s3:*` on a bucket does not lift the bucket's
  limit: these are `admin:` actions on purpose.
- A bucket policy cannot grant them either: a bucket policy grants `s3:` actions only.
- Reading the usage (`s3:ListBucket`) tells the quota, in `quota_bytes` of the usage:
  who may fill a bucket may know how full it may get.
- A change carries its audit record in the same entry. A refusal by the authorizer is
  audited as every denial is.

### What is told

- `cactusfs.bucket.quota-changed.v1` for a change. **No event for a refused write**: a
  refused entry changed nothing, and nothing is published for what did not happen.
- Metrics, none with a bucket's name: `cactusfs_quota_refusals_total`,
  `cactusfs_buckets_quota{state="set|over_90|full"}`, `cactusfs_bucket_usage_counting`,
  `cactusfs_bucket_usage_drift_total`. Alerts `CactusFSBucketQuotaFull` and
  `CactusFSBucketUsageDrift`.
- **A refused write is counted in `cactusfs_quota_refusals_total` and is not an audit
  event.** The trail holds admin mutations, authentication failures and authorization
  denials; a quota refusal is none of the three - it is a limit and no denial - and a
  tenant at its limit would otherwise write the trail full. Ruled so on 2026-10-12, and
  left open for the owner (open point 4).

### The S3 surface

Nothing is added to the S3 API but the error. There is no S3 request that reads or
sets a quota; that is the management API's (`/buckets/{bucket}/quota`,
`/buckets/{bucket}/usage`, `/buckets/{bucket}/usage/recount`).

```
HTTP/1.1 403 Forbidden
<Error><Code>QuotaExceeded</Code><Message>The bucket quota has been reached.</Message>...</Error>
```

It is answered by PutObject, CopyObject, UploadPart and UploadPartCopy, and by nothing
else.

### Object Lock

A locked version cannot be removed to make room, by anyone, and that is by design: a
bucket that is full of retained versions stays full until they expire. Whoever sets a
quota on a bucket with a default retention has said how much may be retained.

## Consequences

- The row of the threat model becomes implemented, with what it does not cover said:
  the capacity of the cluster as a whole (the sum of all quotas may exceed it), and the
  bytes of parity.
- A quota cannot be set on an old bucket until it has been counted: seconds for most
  buckets, minutes for one of hundreds of millions of versions.
- An upload that is never completed counts until it is aborted. A lifecycle rule that
  aborts incomplete uploads is what gives the bytes back.
- A put of unknown length into a bucket that is nearly full writes its shards and is
  then refused.
- A source that replicates into a bucket with a quota stalls when the quota is reached
  (ADR-0026): that is the destination protecting itself.

## Alternatives

- **Deciding in the use case only** - two writers pass it together.
- **Counting current versions only** - no limit where versions are kept, which is where
  it is needed.
- **Not counting unfinished uploads** - an upload that is never completed would be the
  way round every quota.
- **`507 Insufficient Storage`** - retried by SDKs; reads as the server's fault.
- **`s3:PutBucketQuota`** - `bucket-admin` would hold it by `s3:*`.
- **A quota in bytes on the drives** - the tenant cannot know the parity, and it
  changes with the erasure set.

## Open, for the owner

Defaults are applied until answered.

1. **What does a quota count?** Default: every stored version and the parts of
   unfinished uploads, by the size of the object as it was sent; no quota by number of
   objects.
2. **Is `403 QuotaExceeded` the error?** Default: yes.
3. **The plan calls the problem of the management API `quota-exceeded` (PLAN 8.3.2); the
   contract and the mapper say `code: QuotaExceeded`.** Every problem of this API has
   `type: about:blank` and a `code` in the spelling of the S3 codes (`AccessDenied`,
   `NoSuchBucket`); the contract and the code follow that. If a `type` URI of its own is
   wanted for this problem, it would be the first, and wants deciding for all of them.
4. **Should a refused write be audited?** Default, as ruled on 2026-10-12: no - it is
   counted in `cactusfs_quota_refusals_total` and is no audit event. The stricter
   reading would write a record for each, and a tenant at its limit could then fill
   the trail.

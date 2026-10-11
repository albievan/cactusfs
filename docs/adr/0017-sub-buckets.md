# ADR-0017: Sub-buckets

- **Status:** accepted; being implemented (PLAN C.1-C.8)
- **Date:** 2026-10-12
- **Builds on:** ADR-0006 (the single authorizer), ADR-0011 (bucket policies), ADR-0016
  (entry schema 3)

## Context

Albie: "Allow 10 sub buckets per bucket and each bucket must be controlled by
policies." S3 has no such thing; a bucket has keys, and a key may contain slashes.

## Decision

- **A sub-bucket is a named first-level prefix of its bucket**, registered with the
  bucket. Its objects are the keys that begin with its name and a slash. S3 clients
  address them as they always have - `s3://bucket/sub/key` - and nothing in the index
  or on the drives changes: it is a range of keys. No path is derived from it.
- Its name is 1 to 63 lower-case letters, digits and hyphens. No slash: sub-buckets do
  not nest. **At most 10 a bucket**, counted where the entry is applied.
- Objects may still be put at the top level of the bucket, outside every sub-bucket.
  Registering a sub-bucket over keys that exist adopts them. A sub-bucket is deleted
  only when it is empty - of versions, markers and uploads in progress - checked where
  the entry is applied, as for a bucket.
- **Its policy** is written in the language of bucket policies (ADR-0011) and confined:
  resources are the sub-bucket's keys (`arn:aws:s3:::bucket/sub/*`), or the bucket for
  listing; actions are those on objects and listing; it cannot grant
  `s3:BypassGovernanceRetention`, cannot allow anybody anonymous, and cannot deny
  listing - a listing of the whole bucket would still show the keys, so the denial
  would promise what it cannot keep.
- **One decision, three sources.** The single authorizer judges a request with the
  caller's identity policies, the bucket's policy and, when the key (or the prefix of a
  listing) lies in a registered sub-bucket, that sub-bucket's policy: a deny in any
  refuses; an allow in any allows; the bound of an API key still applies after. A
  sub-bucket's policy can grant what the bucket's is silent about, never what it
  denies. Root is bound by none of them, as before.
- **Actions:** `s3:ListSubBuckets`, `s3:PutSubBucket` (create, and set its policy),
  `s3:DeleteSubBucket`, `s3:GetSubBucketPolicy`. Whoever holds `s3:*` holds them. Who
  may list a bucket's sub-buckets sees all; who may not sees those whose keys they may
  list.
- "Each bucket must be controlled by policies" is taken to mean: **a policy can be
  attached to every bucket and every sub-bucket, and edited in the console** (the
  management API gains the endpoints for both). Access is denied by default without
  one; requiring one at creation would refuse S3's `CreateBucket`.
- Stored by commands `subbucket.put` and `subbucket.delete` in `subbuckets/<bucket>/`
  (ADR-0016), not as a kind of bucket configuration: ten policies do not fit one
  document, and the limit and the emptiness must be decided where the entry is applied.

## Consequences

- `s3:PutSubBucket` is worth what `s3:PutBucketPolicy` is, for its prefix.
- A listing of the bucket shows the keys inside sub-buckets to whoever may list the
  bucket.
- A third source of permissions is a third place to look when asking why something was
  allowed; the decision's log names the source.

## Alternatives

- **Child buckets with names of their own** - breaks S3's naming and virtual-hosted
  addressing, and makes eleven buckets of one.
- **Nothing new: prefixes and policy conditions** - nothing to list, limit or manage,
  and the policy language has no condition on prefixes today.

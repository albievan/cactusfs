# ADR-0011: Bucket policies, anonymous access, cross-origin rules and tags

- **Status:** accepted and implemented (PLAN 6.7)
- **Date:** 2026-10-08
- **Milestone:** 6
- **Builds on:** ADR-0006 (policies, the single authorizer), ADR-0008 (entry schema 2)

## Context

CLAUDE.md §3: "EVERY inbound request is authenticated and passes through the single
`ports.Authorizer` - no authorization logic in handlers, no anonymous access unless a
bucket policy grants it." Until now nothing could grant it: a request without
credentials was refused before it reached the authorizer. S3 clients expect three more
things of a bucket: a policy attached to the bucket, cross-origin rules for browsers,
and tags.

## Decision

### One configuration store for a bucket

A bucket carries configuration documents by kind - `policy`, `cors`, `tagging` - each
stored whole and replaced whole by one Raft command, `bucket.config.put` (schema 2).
They live in the bucket's record and go with the bucket. The state machine checks the
kind and the size (64 KiB); what a document means is the business of the use case that
writes it and of whatever reads it. Lifecycle rules (PLAN 6.6) will be a fourth kind.

### Bucket policy

- The policy language of ADR-0006 with one more field per statement, **Principal**:
  `"*"`, `{"AWS": "*"}`, or users of this deployment as `arn:aws:iam:::user/<name>`.
  Parsing is as strict as for identity policies: a field twice, a field in another
  case, an unknown field (`NotPrincipal`, a `Service` principal) are errors.
- A bucket policy is **about its bucket only**: every resource must be the bucket or in
  it, and every action an `s3:` action. It cannot reach another bucket or the
  management API.
- **Decision.** For a user: the user's policies and the bucket's are judged together.
  A Deny in either refuses; otherwise an Allow in either allows. An API key's inline
  policy still bounds it afterwards - a key never exceeds its owner, and never what its
  own policy says, whatever a bucket offers.
- **A bucket policy cannot allow `s3:BypassGovernanceRetention` at all**, by name or by
  wildcard; such a document is refused. The bypass is withheld from every holder of
  `s3:*` (ADR-0009), and `s3:PutBucketPolicy` is something `s3:*` holds: a policy that
  could grant the bypass would let a bucket administrator - or a stolen key of one -
  give it to themselves in one request. It comes from a policy of the user, by name. A
  bucket policy may Deny it.
- **The root identity is bound by no bucket policy**, as by no other. A policy that
  denies everyone cannot lock the deployment out of its own bucket.
- **Anonymous access.** A request without credentials is the principal *nobody*. The
  authorizer allows nobody exactly this: an action from a fixed list of **reads** -
  GetObject, GetObjectVersion, ListBucket, ListBucketVersions, GetBucketLocation - that
  a statement with Principal `*` of the bucket's policy allows and none denies.
  **Nothing that writes, deletes or configures is ever anonymous, whatever a policy
  says.** This is stricter than S3, where a policy can make a bucket world-writable.
- Only `GET` and `HEAD` requests are taken as anonymous at all; any other method without
  credentials is refused before its body is read, as before. That is a rule of
  admission in the S3 adapter, not a decision: the authorizer alone refuses anonymous
  writes too. It means an unsigned write is recorded as a failed authentication and an
  unsigned read as a denied authorization.
- The authorizer reads the bucket's policy for every request about a bucket by a
  principal it could bind, **once per request**: a DeleteObjects of a thousand keys is
  judged against one reading. The read is a consistent one - on a follower it asks the
  leader - because a policy that was just tightened must bind the next request.
  **A policy that cannot be read, or does not parse, refuses the request** (as a
  failure of the server, `500`, not of the client): it might have denied it. Root can
  still read, replace and remove it.
- Setting and removing a policy are audited, with whether the new policy opens anything
  to everyone.

### Cross-origin rules

- `PUT/GET/DELETE ?cors`, the rules of the S3 reference: origins, methods, request
  headers (each pattern with at most one `*`), exposed headers, max age.
- A **preflight** (`OPTIONS`) is answered without credentials, from the rules of the
  bucket alone. One that is not allowed gets one answer - `403 AccessForbidden` -
  whatever the reason: no rule matches, the bucket has no rules, the bucket does not
  exist. A refused preflight tells nobody whether a bucket exists. An allowed one, and
  the cross-origin headers on any other answer - a refusal included, as browsers need
  them - do tell that the bucket has rules for that origin; that is what the rules are
  for, and what S3 does.
- The rules sent to browsers are read from **this node's own copy** of the state,
  without asking the leader: these answers are given before anybody has authenticated,
  and rules a moment old harm nobody. A caller without credentials cannot make a node
  do a consistent read that way.
- `Vary: Origin` is sent whenever the bucket has rules, matched or not, and a preflight
  varies on what it asked.
- On other requests with an `Origin`, a matching rule adds `Access-Control-Allow-Origin`
  (the origin, or `*` for a rule that names every origin) and the exposed headers.
  Authentication and authorization are untouched. `Access-Control-Allow-Credentials`
  is never sent: requests are authenticated by signature, not by cookie.
- Header names - in rules, and those a preflight asks for, which are echoed - must be
  HTTP tokens.

### Tags

- Bucket tags are the `tagging` document (at most 50). Object tags (at most 10) are
  kept with the version and changed by `object.tags.put` (schema 2), by version;
  `x-amz-tagging` on PutObject sets them at creation and needs `s3:PutObjectTagging`
  besides `s3:PutObject`. A locked version can be tagged: a lock keeps the data and the
  version, not its labels.
- A tag key given twice is refused, in the document and in the header. Keys and values
  are letters, digits, spaces and `+ - = . _ : / @`, as in S3. The state machine checks
  tags on every entry that brings them, a put included.
- Changing a bucket's tags, CORS rules or policy is audited.

### Deviations from Amazon S3, and limits

- A bucket policy cannot open writing or deleting to anonymous callers.
- Principals are `*` or users of this deployment; there are no accounts, roles or
  services to name. `NotPrincipal`, `NotAction` and `NotResource` are not supported.
- Tags on CreateMultipartUpload (`x-amz-tagging`) and the tagging directive of a copy
  answer 501.
- The tags of a named version are read and changed with the same three actions as those
  of the current one; S3 has `...VersionTagging` actions for them. Setting an empty tag
  set needs `s3:DeleteObjectTagging`.
- The configuration event names the bucket, not which document changed.
- A bucket policy cannot allow the governance bypass (above).
- The condition keys of ADR-0006 apply; there are none on tags.
- Every request about a bucket costs one more consistent read of the bucket's record.
  There is no cache across requests to go stale.

## Consequences

- `internal/domain/iam/bucketpolicy.go`, `internal/domain/bucket/config.go`,
  `internal/domain/object/tags.go`; `BucketService` and `ObjectService` use cases;
  `internal/adapters/s3api/bucketcfg.go`; `internal/adapters/raftmeta/bucketcfg.go`.
- `app.Authorizer.WithBucketPolicies`; `iam.Request.BucketPolicy`.
- New actions: `s3:{Get,Put,Delete}BucketPolicy`, `s3:{Get,Put}BucketCORS`,
  `s3:{Get,Put}BucketTagging`, `s3:{Get,Put,Delete}ObjectTagging`.
- Events `cactusfs.bucket.configuration-changed.v1` and
  `cactusfs.object.tags-changed.v1`.

## Tests

- Policy parsing: eight documents that are policies and twenty that are not.
- Twenty-seven decisions over anonymous callers, named users, users with policies of
  their own, bounded keys and root; a fuzz test whose invariant is that no policy lets
  an anonymous caller do anything outside the list of reads.
- The authorizer refuses when the policy is unavailable or damaged, except root.
- Over HTTP: an anonymous caller reads and lists what a policy opens and is refused
  eleven other things the same policy "allows"; preflights, with every refusal
  compared for sameness; tags by version.
- The metadata contract on all three stores; the AWS SDK against the built binary.

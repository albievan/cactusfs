# ADR-0013: Lifecycle rules, and a principal for what the node does by itself

- **Status:** accepted and implemented (PLAN 6.6)
- **Date:** 2026-10-10
- **Milestone:** 6
- **Builds on:** ADR-0006 (the single authorizer), ADR-0008 (versions), ADR-0009 (Object
  Lock), ADR-0011 (bucket configuration), ADR-0012 (multipart uploads)

## Context

A lifecycle rule deletes data on a schedule, with no client asking at that moment.
That is the one place where the store destroys by itself, in a product whose first
constraint is that a locked version cannot be destroyed by anyone. Two shortcuts offer
themselves and both are wrong: letting the worker write to the metadata store directly,
past the use cases; and letting it act as root, the identity no policy binds.

## Decision

### Rules

- `PUT/GET/DELETE ?lifecycle`. A rule has a prefix, a status, and any of: expiration
  of the current version after a number of days or on a date; removal of older versions
  a number of days after they were replaced; removal of a delete marker that is all
  that is left of its key; abort of multipart uploads a number of days after they
  began. Rules are a fourth kind of bucket configuration (ADR-0011) and go with the
  bucket. Setting and removing them is audited.
- Days are counted as S3 counts them: something is due at the first midnight UTC that
  is at least that many days after. Never earlier than the days in full, never more
  than a day later. A rule names at most 100 000 days, and the due time is found by
  the calendar: a product of durations overflows a little above that, and a rule
  meaning "centuries" would have expired everything at once. A record that has lost
  its time is never due.
- A configuration is read by where its elements are, not only by their names: a
  `<Prefix>` inside `<Expiration>` is a malformed document, not a rule without a
  prefix. The same reader now reads CORS rules, tag sets, DeleteObjects,
  CompleteMultipartUpload and the Object Lock configuration.
- **Rules only remove.** There are no storage classes, so `Transition` and
  `NoncurrentVersionTransition` are not implemented; nor are filters on tags or size,
  nor `NewerNoncurrentVersions`. A configuration that carries any of them is refused
  whole (`501`): read without the part that is not understood, a rule would remove more
  than its author meant.

### The worker

- One node carries out rules at a time: the one that leads the metadata group, as with
  scrubbing. A pass walks the versions and the uploads of every bucket that has rules,
  in batches, and removes nothing while a scan is reading the state.
- **It removes nothing by a way of its own.** Every removal is `ObjectService.Delete`
  or `ObjectService.AbortUpload` - what a client would have called. Expiring a current
  version is a delete without a version: where versions are kept that makes a delete
  marker and removes nothing; the version goes later, by the rule for older versions,
  if there is one.
- **Every removal is conditional on what was judged.** A pass decides on one reading
  of the state and removes afterwards; in between, a client may write the key again.
  So the delete carries the version ID of the version the decision was made about -
  the one it names, or the newest of the key - and the state machine carries it out
  only if that is still the version there (`ports.DeleteOptions.Expect`, a field of the
  `object.version.delete` entry of schema 2, which no release has shipped). Otherwise
  it refuses, nothing is deleted, and the pass counts the removal as skipped. Without
  this, an object overwritten a moment before its old self expired would be deleted
  new; and in a bucket with suspended versioning "version null" would name the
  client's fresh object.
- Removals decided on one batch of a walk are carried out after the next batch has
  been read, so that the walk never resumes after a version it has just removed: a key
  with thousands of old versions loses them in one pass.
- Rules that cannot be read or do not validate remove nothing, and do not stop the
  rules of other buckets.
- What a pass did is counted and logged whether or not it came to its end - a node
  that stops leading in the middle reports what it removed before.

### The lifecycle service principal

- The worker acts as `iam.Principal{Service: "lifecycle"}`. A principal is a service
  only when it is nothing else, and **no request can authenticate as one**: nothing in
  an inbound adapter sets the field.
- The authorizer allows that service exactly three actions - `s3:DeleteObject`,
  `s3:DeleteObjectVersion`, `s3:AbortMultipartUpload` - and no policy widens or narrows
  that: identity policies do not apply to it, and bucket policies are not consulted,
  as S3's lifecycle is not subject to them.
- **`s3:BypassGovernanceRetention` is not among them**, and the worker never asks for
  the bypass. Whether a version may go is decided where it always is: by the state
  machine, on the leader's time (ADR-0009). A retained or held version that a rule says
  is to go stays; the pass counts it as kept and goes on.

## Alternatives rejected

- **The worker as root.** Root may lift a governance retention. One wrong flag in the
  worker would then delete retained data on a schedule.
- **Direct writes to the metadata store.** Would bypass the authorizer and the shard
  cleanup of the use cases, and make a second path to deletion to keep correct.
- **Enforcing expiry lazily on read.** Simple, but nothing would ever free the space of
  what nobody reads again.

## Deviations from Amazon S3, and limits

- No transitions; no filters on tags or object size; no `NewerNoncurrentVersions`.
- **An expiration date in the past expires everything under the prefix on the next
  pass**, as in S3. It is accepted; whoever may set rules may delete.
- **Whether something is due is judged on the leading node's wall clock.** A leader
  whose clock jumps far ahead expires everything no lock keeps. Locks are judged on
  the entry's time (ADR-0009) and are not affected. Open question for Albie: refuse to
  act when the clock is far ahead of the last applied entry's time.
- Responses do not carry `x-amz-expiration`.
- Removals by rules are counted in `cactusfs_lifecycle_actions_total` and logged per
  pass; they are not audit records one by one, as object deletes by clients are not.
- A pass costs a walk of every version of every bucket that has rules, once per
  `--lifecycle-interval` (an hour by default; `0` switches the worker off).
- The time that decides whether something is due is the leading node's clock; the time
  that decides whether a lock still holds is the entry's (ADR-0009).

## Tests

- The rules: validation; when each kind is due, to the second around midnight; a
  property test that nothing is ever due before its days have passed in full, and that
  disabled rules and other prefixes never apply.
- The service principal: three actions allowed, eleven refused, with and without a
  bucket policy that denies everything; a service that is also anything else is refused.
- The worker over the use cases: a plain bucket; a versioned bucket through expiry,
  removal of old versions and of the last marker; versions under compliance,
  governance and legal hold that a rule says are to go and that stay, however often it
  tries - and that test fails if the worker is made to act as root with the bypass;
  walks that span batches; damaged rules; a node that does not lead.
- A client writing between the worker's look and its removal, in four states of a
  bucket: what it wrote stays, and the test fails without the condition.
- Suspended versioning; a key with 1200 old versions; removals that fail; a node that
  stops leading after one removal.
- Over HTTP, thirty-five configurations that are refused, eight of them a known
  element in the wrong place; a fuzz test; the AWS SDK.
- The conditional delete in the metadata contract, on all three stores.

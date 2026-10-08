# ADR-0009: Object Lock - where it is enforced, and the clock that decides

- **Status:** accepted and implemented (PLAN 6.4)
- **Date:** 2026-10-08
- **Milestone:** 6
- **Builds on:** ADR-0008 (versions, entry schema 2, the leader's time)

## Context

CLAUDE.md §3: "Versioning and Object Lock (governance and compliance). A
compliance-locked version cannot be deleted by anyone, `cluster-admin` included." That
is the product's answer to ransomware with stolen administrator credentials, so it has
to hold against the strongest caller the product has: the root credential, and any
gateway that proposes metadata entries.

## Decisions

### The rules are a few lines in the domain, and they are part of the format

`object.Lock` holds a retention (mode and date) and a legal hold. Two functions decide
everything: `CheckDelete` and `CheckRetention`.

- A version under **legal hold** is not removed. Nothing but taking the hold off changes
  that.
- A version under **compliance** retention that has not ended is not removed, and its
  retention can only be extended, in compliance mode. No argument of either function
  changes that: there is no parameter for it.
- A version under **governance** retention that has not ended is removed, or its
  retention shortened or removed, only with the bypass.
- A retention can always be extended, and governance can always be made compliance.
- A retention has ended when the time of the decision is not before its date.

These functions are called by the state machine, so what they answer is part of the
stored format: they do not change between releases. A property test runs them over
arbitrary inputs.

### Enforced in the metadata state machine

The use cases check too, to refuse early and to authorize. The check that counts is
made when the entry is applied:

- It is the only place that is atomic with the state. A check in a gateway races with a
  retention being set through another gateway.
- Every node may propose entries. A check outside the state machine would trust every
  proposer.
- The entry that removes a version carries one thing the state machine takes on trust:
  `bypass_governance`, which says that the proposer authorized the caller to lift a
  governance retention. It lifts governance and nothing else. Whoever can forge it - a
  node with a valid certificate - can lift governance; nobody can lift compliance or a
  legal hold, because no field does.

### The time of an entry decides

"Has the retention ended" is asked of the time the metadata leader stamped the entry
with, held monotonic by the state machine (ADR-0008). Every replica gives the same
answer from the log alone. An entry without a time, and the delete of entry schema 1,
never reach a locked version.

**What this does not prevent:** a voter whose clock is ahead, once it leads, moves time
forward and ends retentions early. Voters need synchronised clocks. The stricter rule -
for a removal that is allowed only because time has passed, take the earliest clock of a
majority of voters - is an open question for Albie.

### Object Lock belongs to a bucket from its creation

- A bucket is created with Object Lock (`x-amz-bucket-object-lock-enabled: true`) or
  without, and that never changes. There is no command that turns it on later: turning
  it on for a bucket that already has unlocked versions is a different feature.
- Such a bucket keeps versions from the start, and versioning cannot be suspended on
  it: a suspended put would replace the null version, and with it what a lock was meant
  to keep.
- A bucket that holds any version cannot be deleted (ADR-0008), so a locked version
  keeps its bucket.
- A bucket may give its new versions a default retention (a mode, and days or years).
  It is applied by the state machine with the time of the entry. A day is 24 hours and a
  year 365 days: fixed, so that every replica computes the same date.

### Permissions

- Locking is a permission on top of writing: `s3:PutObjectRetention` and
  `s3:PutObjectLegalHold` are asked of a put that carries a retention or a hold. A
  caller without them can still put into a locked bucket, and gets the bucket's default.
- `s3:BypassGovernanceRetention` is **never granted by a wildcard**. A statement that
  allows `*` or `s3:*` does not allow it; only one that names it does. So no built-in
  role bypasses governance, `cluster-admin` included, and a policy has to say so in as
  many words. A Deny covers it however it is written. This is stricter than AWS and is
  flagged for Albie.
- A request that asks for the bypass without the permission is refused. It is never
  carried out without the bypass.
- The root credential is not bound by policies and may bypass governance. It cannot
  bypass compliance: nothing can.

### What a lock does not stop

- **Deleting the object** in the S3 sense: that adds a delete marker and removes
  nothing. The locked version stays under it.
- **The bytes on the drives.** Object Lock is a rule of the metadata. Whoever can write
  to the drives of enough nodes, or to the metadata of a majority of voters, can destroy
  data whatever its lock says. This is in the threat model.
- **A stolen node certificate** can send shard deletes over node RPC.

## Consequences

- A put with a lock into a bucket created without Object Lock is refused, not stored
  unlocked.
- GET and HEAD return the lock of a version in headers to whoever may read the version.
  AWS asks for `s3:GetObjectRetention` for that; here reading the version is enough.
  The retention and legal-hold operations ask for their own permissions.
- A deployment that used Object Lock cannot be rolled back to the previous release
  (ADR-0008).
- Lifecycle rules (PLAN 6.6) must delete through the same entry and never set the
  bypass.

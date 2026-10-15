# Architecture decision records

An ADR records a decision that is expensive to reverse. Day-to-day decisions go in the
`## Decisions` table of `PLAN.md` instead.

An ADR is **required**, together with a migration and an upgrade test, before changing:

- the on-disk shard or metadata format,
- the Raft entry schema,
- the cryptographic design,
- where the object index lives (for example, a move to sharded Raft groups).

## Format

File name: `NNNN-short-title.md`, numbered in order from `0001`.

```
# NNNN. Title

Date: YYYY-MM-DD
Status: proposed | accepted | superseded by NNNN

## Context
What forces the decision.

## Decision
What we will do.

## Consequences
What becomes easier, what becomes harder, how upgrades and rollbacks work.
```

An accepted ADR is never edited to change its meaning; it is superseded by a new one.

## Index

| ADR | Title | Status |
|---|---|---|
| [0001](0001-on-disk-format.md) | On-disk format of drives and shards | accepted; implemented |
| [0002](0002-encryption.md) | Encryption at rest: streaming AEAD and envelope keys | accepted; implemented |
| [0003](0003-metadata.md) | Metadata: embedded Raft with a bbolt state machine | accepted; implemented; the single voter is superseded by 0005 |
| [0004](0004-erasure-and-topology.md) | Erasure coding, drive topology and self-healing | accepted; implemented; the one-node topology is superseded by 0005 |
| [0005](0005-cluster.md) | Cluster: replicated metadata, node RPC and distributed erasure sets | accepted; implemented |
| [0006](0006-iam.md) | Identity and access: policies, API keys, audit trail, Vault | accepted; implemented; what it says of the length of sessions and of a changed password is superseded by 0027; the rule of `Decide` is refined by 0028 (ownership of a version as a source of an Allow, for previews only) |
| [0007](0007-admin-api.md) | Management API, its documentation and the console | accepted; implemented |
| [0008](0008-object-versions.md) | Entry schema 2, the leader's time, and object versions in the metadata state | accepted; implemented |
| [0009](0009-object-lock.md) | Object Lock - where it is enforced, and the clock that decides | accepted; implemented |
| [0010](0010-key-hierarchy.md) | Key hierarchy - one data key, several uses | accepted; implemented |
| [0011](0011-bucket-policy-cors-tags.md) | Bucket policies, anonymous access, cross-origin rules and tags | accepted; implemented |
| [0012](0012-multipart-uploads.md) | Multipart uploads | accepted; implemented |
| [0013](0013-lifecycle.md) | Lifecycle rules, and a principal for what the node does by itself | accepted; implemented |
| [0014](0014-default-console-user.md) | A default console user | accepted; implemented |
| [0015](0015-helm-chart.md) | The Helm chart - a StatefulSet for every node | accepted; implemented |
| [0016](0016-object-access-and-schema-3.md) | Who uploaded and who read an object - and entry schema 3 | accepted; implemented; the rolling-upgrade test from schema 2 is not written |
| [0017](0017-sub-buckets.md) | Sub-buckets | accepted; **not implemented** beyond the domain rules, the port and the replicated store |
| [0018](0018-tag-rules-and-catalogue.md) | Tag rules, lower-case tags, and a catalogue of tags | accepted; **not implemented** beyond the domain rules, the port and the replicated store |
| [0019](0019-bucket-activity-and-schema-4.md) | Who created a bucket, its latest uploads and downloads - and entry schema 4 | accepted; implemented |
| [0020](0020-certificate-rotation.md) | Certificate rotation without restart | accepted; implemented |
| [0021](0021-sbom-and-signatures.md) | SBOMs and image signatures, made and verified offline | accepted; the gate is implemented; releases are signed keyless by the release workflow, which is written and has not run: no release is published yet |
| [0022](0022-settings.md) | Settings - declared once, shown for every node | accepted; implemented |
| [0023](0023-schema-5-usage-outbox-targets.md) | Entry schema 5 - bucket usage, the delivery outbox, targets, outbound connections | accepted; entry schema 5 is implemented in the state machine and the store and is announced (`MetadataVersion` 5), its bytes frozen; **not implemented:** the worker that counts unasked, the delivery worker, targets as a use case, outbound connections (PLAN 8.0.12 to 8.0.14, 8.3.1) |
| [0024](0024-quotas.md) | Quotas | proposed; **not implemented** beyond the contract (PLAN 8A) |
| [0025](0025-bucket-notifications.md) | Bucket notifications and the export of the audit trail | proposed; **not implemented** beyond the contracts (PLAN 8B) |
| [0026](0026-replication-between-sites.md) | Replication between sites, and the conflict rule | proposed; **not implemented** beyond the contracts (PLAN 8C) |
| [0027](0027-session-lock.md) | A console session that locks while it uploads | accepted; implemented, the server and the console (PLAN L.0 to L.11) |
| [0028](0028-object-preview.md) | Previews of objects in the console | accepted; **being implemented** (PLAN Goal O) |

"Implemented" is the state of the code on 2026-10-15; `PLAN.md` has the step behind each.
The ADRs of 0007 and later write their title as `ADR-NNNN: Title` and their status in a
list at the top; the format above is that of the first six.

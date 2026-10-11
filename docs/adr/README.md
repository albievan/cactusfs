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
| [0001](0001-on-disk-format.md) | On-disk format of drives and shards | accepted |
| [0002](0002-encryption.md) | Encryption at rest: streaming AEAD and envelope keys | accepted |
| [0003](0003-metadata.md) | Metadata: embedded Raft with a bbolt state machine | accepted; the single voter is superseded by 0005 |
| [0004](0004-erasure-and-topology.md) | Erasure coding, drive topology and self-healing | accepted; the one-node topology is superseded by 0005 |
| [0005](0005-cluster.md) | Cluster: replicated metadata, node RPC and distributed erasure sets | accepted |
| [0006](0006-iam.md) | Identity and access: policies, API keys, audit trail, Vault | accepted |
| [0007](0007-admin-api.md) | Management API, its documentation and the console | accepted |
| [0008](0008-object-versions.md) | Entry schema 2, the leader's time, and object versions in the metadata state | accepted |
| [0009](0009-object-lock.md) | Object Lock: where it is enforced, and the clock that decides | accepted |
| [0010](0010-key-hierarchy.md) | Key hierarchy: one data key, several uses | accepted |
| [0011](0011-bucket-policy-cors-tags.md) | Bucket policies, anonymous access, cross-origin rules and tags | accepted |
| [0012](0012-multipart-uploads.md) | Multipart uploads | accepted |
| [0013](0013-lifecycle.md) | Lifecycle rules, and a principal for what the node does by itself | accepted |
| [0014](0014-default-console-user.md) | A default console user | accepted |
| [0015](0015-helm-chart.md) | The Helm chart: a StatefulSet for every node | accepted |

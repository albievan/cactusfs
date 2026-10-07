# 0003. Metadata: embedded Raft with a bbolt state machine

Date: 2026-10-07
Status: accepted

## Context

IAM, bucket configuration and the object index must be replicated without an external
database (`CLAUDE.md` §2, §11), reads and listings must be strongly consistent, a write
may be acknowledged only after it is committed through Raft, and Raft entries must carry
a version from the first release so that version N runs alongside N+1. Standalone mode
is a cluster of one, not a second implementation.

## Decision

Implemented in `internal/adapters/raftmeta` with `hashicorp/raft`, `raft-boltdb/v2`
and `go.etcd.io/bbolt` (MPL-2.0, MPL-2.0, MIT).

### Files in the metadata directory

```
node.json      stable node identity (random, generated once)
raft.db        Raft log and stable store (raft-boltdb)
snapshots/     Raft snapshots (two retained)
state.db       the state machine's own bbolt database
```

### Log entries, schema version 1

One version byte (`1`) followed by JSON:

```json
{ "type": "object.put", "put_object": { … } }
```

Commands: `bucket.create`, `bucket.delete`, `object.put`, `object.delete`, `root.set`.
Exactly one payload is present, the one `type` names.

- **Deterministic.** Timestamps, identifiers, wrapped keys and shard locations are
  produced by the proposer and carried in the entry. Applying an entry never reads a
  clock, randomness or a key.
- **Erasure-shaped from version 1.** `object.put` carries the encryption parameters
  (scheme, KMS key ID, wrapped key, nonce prefix, segment size) and the layout (data and
  parity shard counts, stripe size, stored size, drive and size per shard). Milestone 2
  changes the values, not the schema.
- **Unknown means stop.** An entry with another version byte, an unknown command or a
  payload that does not match stops the state machine and makes the node unready. It is
  never skipped: skipping would silently diverge the replica.
- **Refusals are results, not failures.** `bucket.create` of an existing name,
  `bucket.delete` of a non-empty bucket, `object.put` into a missing bucket and a second
  `root.set` are decided inside the state machine, so the check and the change are
  atomic and every replica decides alike.

### State machine

`state.db` buckets: `meta` (`applied`, `schema`), `buckets`, `objects` (one nested
bucket per S3 bucket, keyed by object key), `iam` (`root`).

- **Exactly once.** Each entry's effect and the `applied` index are written in one bbolt
  transaction. Raft replays committed entries after a restart; an entry whose index is
  not above `applied` is skipped.
- **Durable on its own.** `state.db` is flushed on every applied entry, so
  `NoSnapshotRestoreOnStart` is set: restoring the latest snapshot at start would move
  the state back in time.
- **Snapshots** are the bbolt file as of one read transaction, opened in `Snapshot()`
  (when no apply runs) and streamed in `Persist()`. `Restore()` writes the incoming file
  beside the current one, flushes it, proves it opens, then swaps it in.
- **Reads** go to `state.db` directly. On a single voter a write is applied before it is
  acknowledged, so reads are read-after-write consistent.

### Standalone

One voter, bootstrapped on first start, on Raft's in-memory transport with a fixed
address equal to the node ID.

## Consequences

- **Cluster (PLAN 3.3)** replaces the transport with node RPC over mTLS and adds voters
  with `AddVoter`; the log, snapshot and state formats stay. Reads on followers will
  need a read index or leader lease; today's direct reads are only correct on the
  leader, which the readiness check enforces.
- **Throughput** is bounded by one bbolt flush and one log flush per entry. Batching
  (`raft.BatchingFSM`) is compatible with the format and is deferred.
- **The object index** lives in the same Raft group as everything else. It sits behind
  `ports.ObjectIndex`; moving it to sharded groups needs a superseding ADR.
- **A new command** is added under schema version 1 only if older binaries never see it
  in a mixed cluster; otherwise it needs version 2 and the rolling-upgrade rule: N+1
  reads N, and N+1 does not write version N+1 entries until every node runs N+1.
- Raft logs through a small `slog` bridge, so its messages share the JSON log stream.

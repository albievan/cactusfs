# ADR-0030: A node that leaves is revoked - and entry schema 6

- **Status:** accepted (2026-11-12): entry schema 6 is in the codec and the state machine,
  the store proposes and reads it, and `MetadataVersion` is 6. The bytes of schema 6
  are frozen with this step (`testdata/schema6.golden.txt`, "Frozen", below). Not built:
  an operator's command that takes another node out of the cluster, and a way to take a
  node off the record (both under "Consequences")
- **Date:** 2026-11-12
- **Builds on:** ADR-0003 (metadata), ADR-0005 (cluster, node RPC, the Raft port),
  ADR-0016 (the level rule), ADR-0019 (the schema byte), ADR-0020 (certificate
  rotation is not revocation), ADR-0023 (entry schema 5, its state schema and its
  rollback)

## Context

A node's identity is its certificate of the node CA (ADR-0005). Every node RPC is
authorized as the node the certificate names, and a node joins, records and removes
itself only. When a node leaves (node RPC `Leave`), the leader takes it out of the Raft
configuration and deletes its record (`node.remove`). Nothing else is written. So, after
it has left:

- its certificate, still valid until it expires, still authorizes every node RPC:
  `Propose` (buckets, objects, the topology), shards (`WriteShard`, `ReadShard`,
  `DeleteShard`), `Settings`, and `Join` - it can come back as if it had merely started
  late;
- the Raft connections it opened while it was a voter stay open: the Raft listener
  judges a node at the handshake only.

The threat model offered "remove the node" as the remedy for a compromised node key
next to rotating the CA. It was no remedy. There is no revocation list (ADR-0020), and
whatever the node CA signed is a node.

The record that a node left has to be the same on every member, so it is replicated
through Raft. A member of a release that does not know it would apply the entry
differently or not at all (ADR-0016): it is a new command, and so a new entry schema.

## Decision

### Entry schema 6

`MetadataVersion` is 6. One command is new; nothing old changes.

| Command | Entry field | Payload | A member at level 5 |
|---|---|---|---|
| `node.revoke` | `revoke_node` | `{id}`: a node ID (`cluster.ValidateNodeID`) | never meets it: below level 6 the proposer answers "unavailable" and logs nothing. If it met one it would stop, as at any command it does not know |

`node.revoke` is of schema 6 by what it is (`versionOf`) and is never written below 6.
An entry of schema 5 or less that names it does not decode. Who may propose it is
decided as for `node.remove`: its subject is the node it revokes (`Store.Subject`), so
a node revokes itself only (`cluster:RemoveNode`).

### The state

A bucket of bbolt is new: `revokednodes`, `<node id>` -> `{"id", "at"}` (JSON), `at`
being the time the leader gave the entry. The first `node.revoke` that is applied makes
it, and marks the state as schema 6 in the same transaction - whether it took effect
or was refused (an ID that is no node ID changes nothing else). Revoking a node twice
changes nothing. Nothing removes a record: a node does not come back under its name.

Until the first `node.revoke` a state of schema 5 is the state the release of schema 5
wrote, and that release opens it and its snapshots. Afterwards it refuses both
(`openState`, as for every rise). The level rule gains the same floor as the others: a
state of schema 6 has no member of the release before, which stopped at the entry, so a
member that has not registered yet does not take the cluster back below 6
(`Store.entryVersion`).

### Leaving

`Leave` (node RPC, on the leader) does four things, in this order:

1. it checks that the cluster writes schema 6 (`Store.CanRevoke`) - while a member of
   the release of schema 5 remains it answers "unavailable" and does nothing: a node
   cannot leave in the middle of a rolling upgrade, and is told so;
2. it takes the node out of the Raft configuration;
3. `node.remove`, which deletes the node's record;
4. `node.revoke`.

Every step can be taken again, and only the node itself may ask for them. So the
revocation comes last: a node revoked first could not ask again to finish what failed
half-way, and a leader that leaves - it steps down at step 2 - could not have the next
leader write the rest. Between steps 2 and 4 the node is out of the group and not yet
revoked; the Raft port no longer admits it (it is no voter), and a `Leave` that fails
there is asked again. A `Leave` that succeeded has revoked the node.

### Who refuses a revoked node, and where

- **Node RPC.** The server reads, for every call, whether the caller is revoked - from
  this node's copy of the state, without asking the leader - and marks the principal
  (`iam.Principal.NodeRevoked`). `iam.Decide` refuses such a principal everything, and
  the refusal is audited like any other. A record that cannot be read refuses the call
  ("unavailable"). This covers `Propose`, the shard calls, `Settings`, `Info`, `Join`
  and `Leave`.
- **Join.** The leader refuses to add a revoked node to the group, whoever asks
  (`metaRPC.Join`).
- **The Raft port.** The listener admits no revoked node, and asks again, every five
  seconds, about each connection it handed out (`nodetls`, `recheckInterval`): the
  connections of a node that is no longer a voter, or that was revoked, are closed.

### Frozen

`testdata/schema6.golden.txt` pins the bytes of `node.revoke` as it is logged and of the
record as it is stored; `TestTheEntryOfSchema6IsPinned` and `TestRevokingANode` hold the
code to it, `TestTheGoldenFileOfSchema6IsFrozen` holds it to its SHA-256. A change of it
needs a schema 7.

## Consequences

- **Rolling upgrade.** As for every rise: nodes of this release register 6; until all
  have, the cluster writes at most 5, and nothing of schema 6 exists. Only a node that
  leaves writes schema 6, so a cluster that is upgraded and where nobody leaves stays
  one the release of schema 5 opens. `TestRollingUpgradeToSchema6` takes a cluster of
  the release of schema 5 to this one a node at a time and revokes a node only at the
  end; `TestRevokingANode` and `TestAStateOfSchema6IsOpenedOnlyByABinaryOfSchema6` show
  the release of schema 5 stopping at the entry, its state untouched, and refusing a
  state of schema 6.
- **Rollback.** Before the first `node.revoke`: as for schema 5, with every node stopped
  first (a node taken back alone is counted at 6 until it registers again). After it:
  none. The way out of a node that stopped at the entry is to start it on this release.
- **No migration.** Nothing that exists is rewritten: the new bucket is made by the
  first entry that needs it.
- **Re-admission.** A node that left comes back under a new node ID - a new certificate
  of the node CA that names it - or not at all; replacing the node CA (every node's
  certificate) also ends it. No command takes a node off the record; one would be an
  entry of this schema and an action of the management API, and nobody has needed it.
- **Who can make a node leave.** Only the node itself: `Leave` is authorized as
  `cluster:RemoveNode` on the caller's own ID, and no operator command calls it yet.
  So a node whose key is compromised cannot be revoked by an operator through this; the
  remedy for a compromised node key remains replacing the node CA. An operator's
  command to remove another node (an `admin:` action through the management API) is the
  next step and is not built.
- **Lag.** A member refuses a revoked node once it has applied the entry, not before: a
  follower that is behind serves a call or two more. The leader, which applied it
  first, refuses at once.
- **Connections.** A gRPC connection of a revoked node is not closed; every call on it is
  refused. A Raft connection is closed within five seconds.
- **Cost.** One read of bbolt per node RPC call, without a lock of Raft.

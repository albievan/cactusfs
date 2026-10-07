# 0005. Cluster: replicated metadata, node RPC and distributed erasure sets

Date: 2026-10-07
Status: accepted

Supersedes parts of [0003](0003-metadata.md) (the single voter on an in-memory
transport) and of [0004](0004-erasure-and-topology.md) (a topology of one node's drives).
Everything else in both stands.

## Context

Milestones 1 and 2 built a node that runs alone: a Raft group of one voter, drives that
are all local. A cluster needs the same metadata on several nodes, shards on other
nodes' drives, and answers to what happens when nodes cannot reach each other. CLAUDE.md
fixes the frame: embedded Raft with 3 or 5 voters, node RPC over mutual TLS 1.3, shards
of one object on different drives and nodes, no acknowledgement before a write quorum
and a Raft commit, read-after-write, one binary and one code path for both modes.

## Decision

### Every node holds the metadata; `meta` nodes vote

Every node of a cluster runs Raft. Nodes with the `meta` role are voters (3 or 5);
every other node is a non-voter that receives the log. So every node has the state in
its own bbolt database and reads it locally; nobody proxies reads.

- **Changes** are proposed where the request arrives and forwarded to the leader over
  node RPC (`MetaService.Propose`). The leader's state machine answers; the answer
  travels back. The caller is told one of three things, and the write path depends on
  the difference: committed; definitely not committed (refused, or the leader was never
  reached — retried until `leader wait`, 10 s, then 503); or *outcome unknown* (the
  request was sent and no answer came), in which case nothing the entry refers to is
  cleaned up (ADR-0003).
- **Reads are linearizable on every node.** Before a read, a node obtains a read index
  from the leader (`MetaService.ReadIndex`) and waits until its own state machine has
  applied it. The index is the index of the last *command* the leader's state machine
  has applied, taken after (once per term) a barrier has made sure the leader holds
  everything from earlier terms; a change is acknowledged only after the leader applied
  it, so the index covers every acknowledged change.
- **The leader proves that it still leads by asking the voters for their term**
  (`MetaService.Term`) and needs a majority, itself included, in its own term. Raft's
  `VerifyLeader` is deliberately not used: it counts the answer of any server the leader
  replicates to, non-voters included, so a leader cut off from the other voters but not
  from a storage node would pass, and serve stale reads. Likewise `raft.AppliedIndex` is
  not used to wait: it advances when an entry is handed to the state machine, not when
  it has been applied.
- A node that cannot reach a majority answers nothing — no read, no write — rather than
  answer from what it knew. Tested with a real network partition (`test/chaos`).
- **Fail-stop per node.** A node whose state machine cannot apply an entry leaves the
  Raft group until it is restarted, so that the others elect a leader that works.
- **Rolling upgrades.** Every node records the newest entry format its binary applies
  (`metadata_version`). No node writes an entry newer than the oldest of the members,
  an unregistered member counting as the oldest; the leader checks again. A cluster can
  therefore run binaries of two releases side by side, and a new format is used only
  when the last node has been upgraded. Downgrading after that is not supported.

Followers learn of a commit with the leader's next message; `CommitTimeout` is 10 ms in a
cluster, so a read on a node that does not lead costs one round trip to the leader, one
round of term confirmations and at most that wait.

### Node RPC

gRPC over mutual TLS 1.3 on port 9002; Raft's own transport, with the same credentials,
on 9003. The contract is `api/proto/cactusfs/node/v1/node.proto`, checked by `buf lint`
and `buf breaking`; generated code is committed.

- **Identity is the certificate.** A node certificate is issued by the cluster's node CA
  and names the node: its ID is the certificate's first DNS name. A node that dials
  another verifies that the certificate names the node it meant (`ServerName` is the node
  ID, not the address); a node that is dialled learns the caller's ID from the
  certificate. Addresses are never trusted. Raft members are recorded as
  `<node id>@<host:port>` so that the Raft transport knows which certificate to expect.
- **The node CA signs node certificates and nothing else**: whatever it signs is a node.
  The certificate that S3 clients see comes from another CA.
- **Every call passes the authorizer** as a node principal (`cluster:NodeRPC`). A node
  joins and leaves only as itself, and only it may propose an entry that sets or removes
  its own record (`cluster:RegisterNode`, `cluster:RemoveNode`): the others find it by
  that record and ask it about its writes in progress. Nodes have no access to objects
  through S3, and no user, root included, may speak node RPC.
- **The Raft port admits voters only.** Raft trusts what a message says about its
  sender, so the listener completes the handshake itself (with a deadline) and hands a
  connection on only if the certificate names a voter of the group — or, on a node that
  knows no group yet, a node of its `--peers`.
- **What a node certificate is still trusted with.** Any node may propose changes to
  buckets, objects and the topology: that is the work of a gateway and of a node
  replacing a drive. A stolen node certificate is therefore a breach of the cluster's
  metadata, not only of one node. Narrowing that by role is IAM work (milestone 4).
- There is no plaintext node RPC or Raft, in development mode either.
- **Every response carries an error field.** A call that reached the node and was
  answered is a successful RPC; a failed RPC means the node was not reached or did not
  answer. Domain errors cross as stable names (`shard_not_found`, `not_leader`, …).
- A drive on another node is a `ports.ShardStore` like a local one and passes the same
  contract tests. Bytes are verified by the node that stores them and travel under TLS.
  A shard is written as a stream that ends with an explicit commit; a stream that ends
  without one leaves nothing, and the commit is answered only after the flush.

### Forming a cluster

A cluster is formed once, from the complete list of its nodes (`--peers`, the same on
every node). Each node describes itself to the others over node RPC. When all have
answered and none knows of a deployment, the voters bootstrap the Raft group with the
identical member list; the elected leader then asks every node for its drives and
records the topology. Formation refuses fewer than 4 storage nodes and any number of
voters other than 3 or 5. Once any node knows of a deployment, a node that is not part
of the group asks the leader to add it; it never forms a second group. The leader gives
a vote only to a node that has one already (a voter that started late); the voters are
fixed at formation, and a deployment created by a node on its own cannot be turned into
a cluster.

Each node records itself (`node.set`: addresses, roles, labels, metadata version). Nodes
find each other in that record, and in `--peers` before it exists.

### Distributed erasure sets

`topology.set` slots carry the node of their drive. A new topology is planned by dealing
the drives out one node at a time, in an order that alternates zones, then racks, then
nodes; an erasure set is a run of that deal, so its drives are as far apart as the
cluster allows. The number of whole nodes that can be lost is computed
(parity ÷ the most drives one node has in a set), logged at every start, and a cluster
that tolerates none says so loudly.

- **A node answers for its own drives only.** At start it reconciles the drives attached
  to it with its slots (ADR-0004's rules, per node). Another node's drives are that
  node's business; a node that is down does not keep the others from starting.
- **Unreachable is not damaged.** A drive that cannot be reached yields
  `ErrDriveOffline`. Reads use the other shards; writes go on while a write quorum of
  shards is durable, and are refused otherwise; healing leaves such shards out — it
  neither uses, rebuilds nor counts them — and never reports an object as lost because
  nodes are down. Requests that fail only for lack of reachable nodes answer 503.
- **Scrubbing** is done by one node at a time, the leader of the metadata group.
  **Garbage collection** is done by every storage node on its own drives. Before a shard
  is removed the collector asks, in this order: is the data referenced (no); has any
  node a write in progress on it (`NodeService.InFlight`, every recorded node — one that
  cannot be asked counts as yes); is it referenced now (no). The second look at the
  index closes the gap in which a write commits and lets go of its pin.

### Events

Events are derived from applied entries inside the state machine, after the entry's
transaction has committed, and published through `ports.EventPublisher`. Their IDs come
from the entry's index, so every node derives the same event with the same ID. The
contract is `api/asyncapi/events.yaml`. The default publisher is in-process, bounded and
never blocks the state machine: delivery is at most once per node, and an event can be
missed (queue full; node stopped between commit and delivery; node caught up from a
snapshot). Nothing that must not be missed depends on it. Durable delivery to the
outside (milestone 8) will read the applied log through its own adapter.

### Formats

Under entry schema version 1, of which no release has shipped: new commands `node.set`
and `node.remove`; `topology.set` slots gain `node_id`. New state bucket `nodes`.
`node.json` may now hold a given node ID instead of a generated one.

**Migration.** The previous commit (milestone 2, unreleased) recorded topologies whose
slots name no node. Such a topology belonged to a node on its own; at its first start
with this code the node rewrites it with its own ID in every slot (`topology.set`, one
version up) and goes on. Tested in `internal/app` and by starting the previous commit's
binary and then this one on the same directories.

## Consequences

- **"Stateless gateway" is read as: holds nothing that cannot be rebuilt.** A gateway
  has a replica of the metadata in its metadata directory, which it would get again from
  the leader if it lost it. It needs that directory to be writable, and reasonably fast.
  The alternative — gateways that ask a voter for every metadata read — keeps them
  diskless and puts every listing and lookup on three or five nodes. This is the
  decision most worth revisiting; moving the object index to sharded groups
  (`ports.ObjectIndex`) would revisit it anyway.
- **The cluster's size is fixed when it forms.** Storage nodes and drives cannot be added
  afterwards (new erasure sets); gateways and voters can join the metadata group through
  node RPC. A storage node that starts with drives the topology does not know is refused.
- **A node needs all of its own drives to start**, as before; and a new cluster needs all
  of its nodes up once, to form.
- **A voter that lost its metadata directory must not simply be started again.** With an
  empty log it could vote a second time in a term it already voted in. It has to be
  removed from the group first; nothing enforces that yet.
- **A dead node that stays recorded blocks garbage collection** on the others (it cannot
  be asked whether it is writing). It has to be removed. Unknown-outcome pins are lost
  when a gateway restarts; what then protects the shards is the grace period (at least
  1 h) against the deadline of a forwarded proposal (30 s).
- **Keys.** Every node must open the same KMS key. Until the Vault adapter (PLAN 4.4)
  exists, the nodes of a cluster share one sealed key file, which the node says at start
  is a development arrangement.
- **Probes are not served until a node has joined its cluster**: the listeners open after
  the node is assembled. The Helm chart (PLAN 7.1) needs them earlier.
- **Cost of a read** on a node that does not lead: one RPC, one round of term
  confirmations (shared by reads that arrive together) and up to 10 ms. Reading a shard
  from another node opens it once per request for a range; neither has been optimised
  or benchmarked.
- **Never stretch the group across a WAN.** Non-voters included: they are in the read
  path of their own requests. Sites are linked by replication (milestone 8).
- Upgrades: a release that adds an entry format raises `MetadataVersion`; the rolling
  upgrade test in `raftmeta` shows how the format stays unused until every member can
  apply it. Rollback is possible until the last node has been upgraded.

# 0004. Erasure coding, drive topology and self-healing

Date: 2026-10-07
Status: accepted

## Context

`CLAUDE.md` §2 requires Reed-Solomon erasure coding over erasure sets of 4–16 drives,
default parity by set size, shards of one object never sharing a drive, a write quorum
before a write is committed, and self-healing. ADR-0001 fixed the shard file format and
ADR-0003 the object-version entry so that this milestone would change values, not
formats. Three things were still open: how the stream is cut into shards, how a drive is
identified across restarts and replacement, and how unreferenced shards are found.

## Decision

### Stripes and shards

The encrypted stream of an object version (ADR-0002) is cut into stripes of 1 MiB
(`StripeSize`, recorded per version). Each stripe is split into `Data` chunks of equal
length, the last padded with zeros, and extended with `Parity` Reed-Solomon chunks
(`klauspost/reedsolomon`, behind `ports.ErasureCodec`). Chunk `i` of every stripe is
appended to shard `i`. The stripe size is independent of the 128 KiB checksum block size
of the shard file.

| Drives in the set | Data | Parity | Write quorum | Read quorum |
|---|---|---|---|---|
| 1–3 | 1 | 0 | 1 | 1 |
| 4 | 2 | 2 | 3 | 2 |
| 5 | 3 | 2 | 3 | 3 |
| 6 | 3 | 3 | 4 | 3 |
| 7 | 4 | 3 | 4 | 4 |
| 8–16 | n − 4 | 4 | n − 4 (5 for n = 8) | n − 4 |

Write quorum is `Data`, plus one when `Data = Parity`. With fewer than four drives an
object is one shard on one drive: encrypted and checksummed, without parity, and the node
says so at start.

More than 16 drives are divided into the fewest sets of at most 16; a count that does not
divide evenly is refused. The set, and the drive of each shard within it, are derived
from the object's random data ID, so load spreads and no two shards of an object share a
drive.

### Write and read

- **Write.** A shard writer is opened on every drive of the placement. A drive that fails
  at any point is dropped; the write goes on while a write quorum remains and fails,
  removing what it wrote, when it does not. After the digests are verified the remaining
  shards are committed (flushed and renamed), and only if a write quorum is durable is
  the version proposed to Raft. The layout names all `Data + Parity` locations, so a
  shard that could not be written is rebuilt later where it belongs; such a version is
  queued for healing at once.
- **Read.** The data shards are read; when one is missing, has the wrong size or fails
  its block checksum, the stripe is rebuilt from parity. Every chunk is checksum-verified
  by the drive and every segment authenticated by the stream cipher, so a rebuilt stripe
  is as trustworthy as an intact one. Fewer than `Data` usable shards is an error; no
  bytes are returned. A degraded read queues the object for healing.

### Topology

Which drive is in which slot of which erasure set is replicated metadata
(`topology.set`, compare-and-set on a version), written once at the first start from the
attached drives. Object layouts name drive IDs, never directories or positions, so the
order of `--data` flags is irrelevant.

A slot outlives its drive. When exactly one drive of the topology is absent and exactly
one attached drive is unknown **and was empty when opened**, that drive takes over the
slot; the old ID is kept in the slot's `retired` list, and layouts that name it resolve
to the new drive. Anything else — a missing drive, an unknown drive that holds data, two
changes at once, a retired drive coming back — stops the node with a message.

### Healing, scrubbing, collecting

- **Heal** verifies every block of every shard of a version and rewrites the shards that
  are missing, truncated or corrupt from the others (delete, then create: shards are
  immutable). Reed-Solomon is deterministic, so a rebuilt shard is byte-identical. If the
  version was replaced or deleted meanwhile, what was rebuilt is removed again.
- **Scrub** heals every object in turn (`--scrub-interval`, default 24 h;
  `--scrub-rate-mib`, default 64 MiB/s). It is what finds bitrot in data nobody reads and
  what refills a replaced drive.
- **Collect** removes shards that no object version refers to and that are older than
  `--gc-grace` (default 24 h, at least 1 min). The state machine keeps a derived index
  from data ID to reference count (`data` bucket), so the collector asks one question per
  shard instead of scanning every object. The index is rebuilt when a state lacks it.

### Formats

- Shard files and the drive file: unchanged (ADR-0001, version 1).
- `object.put`: unchanged; `data_shards`, `parity_shards`, `stripe_size` and `shards` now
  take values other than 1/0.
- New under entry schema version 1: `topology.set`. New state buckets: `cluster`, `data`.
  No release has shipped schema version 1, so no older binary can meet these.

## Consequences

- An object survives the loss or corruption of any `Parity` of its shards, and stays
  writable while a write quorum of its drives works.
- **All drives must open for the node to start.** A dead drive has to be replaced by an
  empty one (one at a time) before a restart. Starting degraded is not implemented.
- Objects written by milestone 1 (one shard, no parity) remain readable; they are not
  re-encoded. A deployment that had four or more drives under milestone 1 keeps those
  objects without parity until they are rewritten.
- Changing the number of drives of an existing deployment is not supported; the
  topology is fixed at first start apart from one-for-one replacement.
- A rebuilt shard needs `Data` full shards to be read: healing a large drive is
  proportional to the data on the whole set.
- The grace period is the only thing between an in-flight upload's shards and the
  collector. It must stay far above the longest time an upload can take between making
  its shards durable and committing.
- Writes encode and write shards sequentially. Parallel shard I/O is a later
  optimisation that does not touch any format.

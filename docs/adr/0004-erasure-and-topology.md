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

### Topology and drive identity

Which drive is in which slot of which erasure set is replicated metadata
(`topology.set`, compare-and-set on a version). Object layouts name drive IDs, never
directories or positions, so the order of `--data` flags is irrelevant.

A deployment has a random **deployment ID**, created with its topology. Every drive is
*claimed* for it: the ID is written, durably, into the `cluster_id` field that ADR-0001
reserved in `cactusfs-drive.json`. Drives and metadata that do not belong together are
therefore never combined:

- **A new deployment** is created only from drives that are all empty and unclaimed.
  Drives that hold data while the metadata directory knows no deployment mean the wrong
  metadata is in use (a missing mount, a wrong `--meta-dir`); the node refuses to start.
  Adopting them would make every shard look unreferenced, and the collector would then
  delete it.
- **An existing deployment** refuses a drive claimed for another deployment, a drive it
  retired earlier, and an unknown drive that holds data.
- **Replacement.** When exactly one drive of the topology is absent and exactly one
  attached drive is unknown and empty, the empty drive may take over the slot — only
  where the set has parity to rebuild from, and only when the operator confirms it for
  that start with `--allow-drive-replacement`. An empty directory is also what a missing
  mount looks like, and a replacement is permanent: the old ID moves to the slot's
  `retired` list, layouts that name it resolve to the new drive, and the old drive is
  refused from then on. Without parity (fewer than four drives) a replacement is never
  made: nothing could be rebuilt.
- **Emptiness is a property of the drive** (no shard file on it), not of who formatted
  it, and the claim is written after the topology is committed. A start interrupted at
  any point — after formatting, after the commit, before the claim — is simply repeated.

### Healing, scrubbing, collecting

- **Heal** verifies every block of every shard of a version and rewrites the ones that
  are missing, truncated or corrupt from the others. It is built so that it can never
  make an object worse:
  - only a shard that is *definitely* damaged (not found, wrong size, checksum mismatch)
    is rebuilt; any other error — too many open files, an I/O error — stops the heal of
    that object with nothing changed;
  - a rebuilt shard is written in full beside the damaged one and replaces it with one
    rename (`ShardStore.Rewrite`); a rebuild that fails leaves every shard as it was;
  - afterwards it checks that the version is still the object's current one. Only when a
    lookup *succeeds* and shows another version, or none, are the shards it just rebuilt
    removed — those and no others. A lookup that fails, as during shutdown, removes
    nothing;
  - one heal per object at a time.

  Reed-Solomon is deterministic, so a rebuilt shard is byte-identical to the lost one.
- **Scrub** heals every object in turn (`--scrub-interval`, default 24 h), throttled as
  it reads (`--scrub-rate-mib`, default 64 MiB/s). It is what finds bitrot in data nobody
  reads and what refills a replaced drive.
- **Collect** removes shards that no object version refers to. The state machine keeps a
  derived index from data ID to reference count (`data` bucket), rebuilt when a state
  lacks it, so the collector asks one question per shard. A new shard is unreferenced
  until its version commits; three things keep the collector away from it:
  - a write pins its data ID in memory from the moment its shards are created until its
    version is committed or definitely refused; a commit whose outcome is unknown
    (ADR-0003) stays pinned for the life of the process;
  - nothing is collected, and no reference question is answered, while the metadata is
    not ready — a state machine that has stopped has entries it has not applied;
  - only shards older than `--gc-grace` (default 24 h, at least 1 h) are removed.

### Formats

- Shard files and the drive file: unchanged (ADR-0001, version 1).
- `object.put`: unchanged; `data_shards`, `parity_shards`, `stripe_size` and `shards` now
  take values other than 1/0.
- The drive file: unchanged in form; `cluster_id` now holds the deployment ID.
- New under entry schema version 1: `topology.set` (with `deployment_id`). New state
  buckets: `cluster`, `data`. No release has shipped schema version 1, so no older binary
  can meet these.
- `ports.ShardStore` gains `List` and `Rewrite`.

## Consequences

- An object survives the loss or corruption of any `Parity` of its shards, and stays
  writable while a write quorum of its drives works.
- **All drives must open for the node to start.** A dead drive has to be replaced by an
  empty one, one at a time and confirmed, before a restart. Starting degraded is not
  implemented.
- **A replacement cannot be undone.** If the wrong drive was confirmed as gone, its
  contents are rebuilt on the new one from parity, and the old drive is not accepted
  back.
- **A shard is given up for the rest of a read or heal after one bad block.** An object
  whose bad blocks sit in different stripes of more than *parity* shards could in
  principle be recovered stripe by stripe; today it is reported as unrecoverable.
  Nothing is deleted in that case.
- A layout of one shard without parity stays readable next to erasure-coded ones; the
  reader goes by what each version records. Drives that hold data but no deployment
  claim, as a pre-release milestone 1 build left them, are not adopted.
- Changing the number of drives of an existing deployment is not supported; the
  topology is fixed at first start apart from one-for-one replacement.
- A rebuilt shard needs `Data` full shards to be read: healing a large drive is
  proportional to the data on the whole set.
- The pins that protect a write in progress live in memory. After a crash they are not
  needed: an upload that was not acknowledged is dead, and entries that reached the log
  are applied before the node serves or collects anything.
- Writes encode and write shards sequentially. Parallel shard I/O is a later
  optimisation that does not touch any format.

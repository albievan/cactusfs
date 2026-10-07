# 0001. On-disk format of drives and shards

Date: 2026-10-07
Status: accepted

## Context

A storage node keeps shards of encrypted object data on local drives. `CLAUDE.md`
requires that object keys and bucket names never become filesystem paths, that every
shard carries a SHA-256 checksum verified on every read, that bytes failing verification
are never returned, that writes are durable before they are acknowledged, and that
on-disk formats carry a version from the first release. The same code must run on
Linux, macOS and Windows. Milestone 2 adds erasure coding, healing and scrubbing and
must not need a format change.

## Decision

Implemented in `internal/adapters/disk`. All integers are big endian.

### Drive

A drive is a directory, opened as an `os.Root` so that no operation can leave it.

```
<drive>/
  cactusfs-drive.json     identity of the drive
  lock                    exclusive lock held by the running process
  tmp/                    writes in progress; emptied on every open
  shards/<aa>/<bb>/<data id>.<shard index>
```

`cactusfs-drive.json` (format version 1):

```json
{ "magic": "cactusfs-drive", "format_version": 1, "drive_id": "<32 hex>",
  "created_at": "…", "cluster_id": null, "node_id": null, "set": null }
```

The drive ID is random and is what object metadata refers to; the directory can be
moved or remounted. `cluster_id`, `node_id` and `set` exist from version 1 so that
joining a cluster fills them in without a format change. A directory that holds shards
but no drive file is refused rather than re-formatted.

`<data id>` is the 32-character lowercase hexadecimal data ID generated for each write;
`<aa>` and `<bb>` are its first two bytes. `<shard index>` is 0–255. Nothing else
appears in a path.

### Shard file, version 1

```
header (64 bytes)
   0  magic            8 bytes  "CFSSHARD"
   8  format version   uint16   1
  10  header length    uint16   64
  12  block size       uint32   data bytes per block (131072 today, at most 16 MiB)
  16  data ID          16 bytes
  32  shard index      uint32
  36  reserved         12 bytes zero
  48  header checksum  16 bytes first half of SHA-256(header[0:48])
blocks, back to back
      checksum         32 bytes SHA-256(data ID || shard index (uint32) || block number (uint64) || data)
      data             block size bytes; fewer in the last block only
```

An empty shard has a header and no blocks. The number of data bytes follows from the
file size.

- **Verified on every read.** A reader checks a block's checksum before returning any
  byte of it. A mismatch is `ErrIntegrity`.
- **Position-bound checksums.** The checksum covers the shard identity and the block
  number, so a block moved within a file, or a whole file put in another shard's place,
  fails verification and the damaged shard can be named.
- **Random access.** Block `n` starts at `64 + n × (32 + block size)`; a range read
  verifies only the blocks it touches. The checksum block size is independent of the
  erasure stripe size.
- **Truncation.** A file cut inside a block or a checksum is detected here. A file cut
  exactly between blocks is a valid shorter shard at this layer; the expected shard size
  is recorded in the object version's layout, and the stream authentication of ADR-0002
  detects the missing data.

### Write protocol

1. Create `tmp/<data id>.<index>.<sequence>` with `O_EXCL`; write header and blocks.
2. `fsync` the file, close it.
3. Create missing shard directories, `fsync`ing the parent of each new one.
4. Refuse if the destination exists — shards are immutable — then `rename` into place.
5. `fsync` the shard's directory.

Only after step 5 does `Commit` return, and only then may the object version be proposed
to Raft. A crash before step 4 leaves a file in `tmp/`, removed on the next open. A crash
after it leaves a complete, verified shard that no metadata refers to; the collector of
PLAN 2.7 removes those.

### Operating systems

- **Linux, macOS:** `flock(LOCK_EX|LOCK_NB)` on `lock`; directories are `fsync`ed.
- **Windows:** `LockFileEx` on `lock`. A directory handle cannot be flushed; file
  contents are flushed with `FlushFileBuffers` before the rename and NTFS journals the
  rename itself. Directory sync is therefore a no-op there.

## Consequences

- A reader refuses an intact header of another version with `ErrUnsupportedFormat`
  rather than guessing. Version N+1 must keep reading version N for rolling upgrades.
- Any change to the layout above is a new format version with a superseding ADR, a
  migration and an upgrade test.
- Overhead is 64 bytes per shard plus 32 bytes per 128 KiB (0.02 %).
- Per-block checksums mean a scrubber (PLAN 2.5) can verify a shard without any key.
- The directory fan-out (65 536 leaf directories) keeps directories small up to
  hundreds of millions of shards per drive.

# ADR-0023: Entry schema 5 - bucket usage, the delivery outbox, targets, outbound connections

- **Status:** proposed (2026-10-12). It becomes accepted when the code of PLAN 8.0 has
  landed and the gates have seen it (PLAN 8.3.6)
- **Date:** 2026-10-12
- **Revised:** 2026-10-12, to the owner-side rulings of that day (PLAN, Decisions: "the
  holes the writing of ADR-0023 to 0026 found") and to the domain and the ports of PLAN
  8.0.4 and 8.0.5, before the codec of PLAN 8.0.6 is written from it; and again that day,
  to the findings of the review of 8.0.2 to 8.0.5: the bound of a replica's sequence, the
  canonical form written by hand, certificates judged by their framing in apply, the path
  of an endpoint shown to nobody, the addresses never connected to, and a clamp that
  begins a recount
- **Builds on:** ADR-0003 (metadata), ADR-0005 (cluster, levels), ADR-0008 (the leader's
  time, versions and their sequence), ADR-0016 (entry schema 3, the level rule as it
  stands), ADR-0019 (entry schema 4, the schema byte as a switch)
- **With:** ADR-0024 (quotas), ADR-0025 (bucket notifications and audit export),
  ADR-0026 (replication between sites), which share the schema and are built on what
  this one puts into the state

## Context

Milestone 8 is three features: quotas, bucket notifications, and replication between
sites. Four facts of the code decide how they can be built.

- **Nothing is counted per bucket.** The usage of a bucket is a walk of its current
  versions, bounded at 100 000 keys and kept for 30 seconds. A quota cannot be decided
  by a walk, and a walk does not see the versions that are no longer current - which
  are what a store that keeps versions against ransomware fills up with.
- **Events are lost at a restart.** The in-process bus delivers at most once, drops when
  its queue of 4 096 is full, and replays nothing. A notification that a receiver may
  rely on, and a version that must reach another site, need something that survives
  the node.
- **The Raft log cannot be that something.** It is truncated behind snapshots, and a
  snapshot is the bbolt file: a member that is sent one has no log to read from. What
  is still to be delivered has to be in the replicated state itself.
- **The node makes no outbound call today.** Its only connections out are to its own
  peers, the key service and the collector the operator configured. A webhook, a NATS
  server and another site are addresses that somebody types into an API: every one is
  a request the node makes on somebody's word.

All three features add commands and stored records. A member that does not know a
command, or a kind of bucket configuration, must never meet one (ADR-0016). So, as for
goals B to D, they land as **one** rise of the entry schema - not three: a build that
knew one feature's commands and announced schema 5 would stop at another's.

## Decision

### Entry schema 5

`MetadataVersion` is 5. Seven commands are new; six old commands can carry something
new; five buckets of bbolt and two counters are new; three stored records gain fields.

**New commands.** Each is of schema 5 by what it is (`versionOf`) and is never written
below 5.

| Command | Entry field | Payload | A member at level 4 |
|---|---|---|---|
| `bucket.quota.put` | `set_bucket_quota` | `{bucket, bytes}` (`bytes` 0 removes) and `audit` | never meets it: below level 5 the proposer answers "unavailable" and logs nothing. If it met one it would stop, as at any command it does not know |
| `bucket.usage.walk` | `walk_bucket_usage` | `{bucket, limit, restart}` | the same; the leader proposes none below level 5 |
| `target.put` | `put_target` | the target record (below) and `audit` | the same |
| `target.delete` | `delete_target` | `{name}` and `audit` | the same |
| `outbox.ack` | `ack_outbox` | `{target, upto, audit_upto, results: [result]}`; a result is `{seq, status}` - it names a row - or `{b, k, vid, status}` - it names a version; never both. `status` is `COMPLETED` or `FAILED`. At most 1 000 results (`delivery.MaxAckResults`) | the same; no worker delivers below level 5 |
| `bucket.replica.put` | `set_bucket_replica` | `{bucket, source}` (`source` empty turns it off; otherwise 1 to 64 letters, digits and hyphens, `object.ValidateReplicaSource`) and `audit` | the same |
| `replication.resync.put` | `put_resync` | `{bucket, target, state, key, version, scanned, sent}`: `bucket.Resync`. `state` is `running` or `done`; `key` and `version` are the last version dealt with (`version` as `object.VersionRef` writes one: `null` or 32 hexadecimal characters), both empty before the first; `sent` is never more than `scanned` | the same |

`decodeEntry` refuses each of the seven in an entry that claims a schema below 5.

**Old commands with something new.**

| Command | New in the log | Schema of the entry | Below level 5 |
|---|---|---|---|
| `bucket.config.put` | the kinds `notification` and `replication`, each a canonical JSON document (ADR-0025, ADR-0026) | 5 when the kind is one of the two | refused as "unavailable", as `tagrules` was below 3; a member of schema 4 that met one would refuse it as a kind there is not |
| `bucket.create` | nothing; it is **written as schema 5** once the level is 5 (`writtenAt`), with the fields it always had | 5 at level 5 | written as before (schema 1 or 4); no usage record is made |
| `object.put` | `replica: {from, seq, null}` | 5 when it carries `replica` | `replica` is refused as "unavailable"; a put without it is written as before |
| `upload.create` | `replica_from` | 5 when it carries it | refused as "unavailable" |
| `upload.complete` | `replica: {from, seq, null}` | 5 when it carries `replica` | refused as "unavailable" |
| `object.version.delete` | `replica: {from, seq, null}`; `by` (who deleted, in the words the audit trail uses for actors) | 5 when it carries either | `replica` is refused as "unavailable"; `by` is **left out** (`heldTo`) and the delete is written as before |

`replica` is `object.Replica`: `from` is the deployment ID of the source, as
`object.ValidateReplicaSource` takes one; `seq` is the sequence the version has there,
**from 1 to 2^62** (`object.MaxReplicaSeq`; `Replica.Validate` refuses 0 and anything
above, in apply, on every member); `null` says that the version is the null version of its key at the source and
is stored as the null version here, at that sequence (ADR-0026).

What is of schema 5 in an entry that claims an older schema is treated exactly as the
binary of that schema treats the same bytes: `fsm.Apply` strips `replica`,
`replica_from` and `by` from an entry whose byte is below 5, as it strips `created_by`
below 4, and a `bucket.create` below 5 makes the bucket record the release of schema 4
makes, byte for byte, and no usage record.

**New in the state.** Every record is JSON with `"v":1`, its fields in a fixed order,
and its bytes pinned by a golden file: a change that makes two releases store other bytes
needs a rise of the entry schema. The usage record and the two canonical documents are
encoded by the domain and pinned in `internal/domain/bucket/testdata/`
(`usage-record.golden.json`, `usage-record-counting.golden.json`,
`usage-record-recount.golden.json`, `usage-record-text.golden.json`,
`usage-record-bounds.golden.json`, `notification.golden.json`,
`notification-text.golden.json`, `replication.golden.json`,
`replication-text.golden.json`); the records the store encodes are pinned in
`internal/adapters/raftmeta/testdata/`. Each bucket of bbolt is created when first
written.

**Canonical documents.** The usage record, the notification rules and the replication
configuration are read by a parser that takes the bytes its writer writes and no others,
and members of one cluster are built with different releases of Go. So their bytes are
decided by hand, in one place (`internal/domain/bucket/canonical.go`), and by nothing
`encoding/json` may do differently one day; neither the writer nor the reader calls it.
No white space; the fields of an object in one fixed order, each written or left out by
its own rule. **A string** is its UTF-8 bytes between quotation marks, with exactly three
things escaped: `"` as `\"`, `\` as `\\`, and each of U+0000 to U+001F as `\u00XX` with
lower-case hexadecimal digits - never a short form (`\n`, `\t`, `\b`, `\f`, `\r`), never
`\/`. Everything else is written raw: `<`, `>` and `&`, U+007F, U+2028 and U+2029, and
characters beyond the basic plane (as their four bytes, never as a surrogate pair). Text
that is not valid UTF-8 cannot be written. **A number** is a whole number of decimal
digits: no sign - nothing in these documents can be negative, so `-` is legal nowhere - no
leading zero, no point, no exponent. `true` and `false` in those words. The reader
accepts exactly this, and then writes what it read and compares the bytes, so that
`Parse(Document(x)) = x` and `Document(Parse(b)) = b` for every `b` it takes (fuzzed in
both directions; `*-text.golden.json` pin a string of every class in a cursor, an ID, a
prefix and a suffix, and values of 1 024 bytes). The documents remain JSON that any
reader reads.

| Bucket | Key | Value |
|---|---|---|
| `bucketusage` | bucket name | `{"v":1,"exact","objects","current_bytes","versions","stored_bytes","upload_bytes","replication_pending","replication_failed","high","count":{"phase","cursor","objects","current_bytes","versions","stored_bytes","upload_bytes","replication_pending","replication_failed"}}`: what `bucket.Usage.Record` writes. Every field is always written but `count`, which is there only while a count or a recount is under way; `phase` is 1, 2 or 3 (below) |
| `targets` | target name | `{"v":1,"name","kind","endpoint","region","subject","ca_pem","buckets":[pattern],"audit","auth":{"mode","id"},"secret":sealed,"sign_key":sealed,"refs","created_at","updated_at"}`; `kind` is `webhook`, `nats` or `s3`; `auth.mode` is `none`, `bearer`, `basic`, `token` or `sigv4` |
| `outbox` | 10 bytes: the index of the entry (8, big-endian), then the number of the row within the entry (2) | `{"v":1,"e","b","k","vid","sz","et","at","by","to":[consumer]}`; `e` is `put`, `complete`, `marker`, `delete`, `tags`, `retention` or `legalhold`; a consumer is `n:<target>` (notification) or `r:<target>` (replication) |
| `outboxcursor` | target name | `{"v":1,"from","acked","audit_acked","lost","resync_needed"}` |
| `resync` | bucket name | the payload of `replication.resync.put` |
| `meta` | `outboxcount`, `outboxdropped` | 8 bytes each, big-endian: the `rows` and the `dropped` of the outbox's state |

| Existing record | New field | Written by |
|---|---|---|
| bucket record | `quota_bytes` | `bucket.quota.put` |
| bucket record | `replica_from` | `bucket.replica.put` |
| bucket record | `config["notification"]`, `config["replication"]` | `bucket.config.put` |
| version record | `replication`: `PENDING`, `COMPLETED`, `FAILED` or `REPLICA` | a put, a completion or a marker (ADR-0026); `outbox.ack` |
| upload record | `replica_from` | `upload.create` that carries it |

All of them are left out when empty, so a record that carries none of them is the
record of schema 4. `replica` itself is not stored: what is kept of it is the version's
`Seq` and its `replication`, which is `REPLICA`.

### The switch - and why it is not the schema byte

ADR-0019 made the schema byte the switch: a member keeps the record of a bucket's
latest use for an entry of schema 4 and not for one below. That is right for a record
that may miss a row. It is wrong for a counter.

**Derived state of schema 5 - the usage record and the outbox - is kept by every entry,
of whatever schema byte, while its record or its configuration exists.** And those can
only be made by an entry of schema 5: a usage record by a `bucket.create` of schema 5
or the first `bucket.usage.walk`; a row of the outbox only for a bucket that has a
`notification` or `replication` document.

- A proposer that learns the level a moment late writes a put as schema 4 after the
  cluster is at level 5. Under ADR-0019's rule that put would not be counted, and the
  bucket's usage would be wrong for good; for a quota that is not tolerable.
- It is safe for the reason the byte was: all members decide alike from the log alone.
  No member of schema 4 can be past the entry that made the record - it stops there -
  so every member that applies a later put, of whatever byte, has the record and
  updates it.
- The record of latest use (ADR-0019) is unchanged and still goes by the byte.

### Level rule and state schema

In ADR-0019's form.

- Nothing of schema 5 is proposed until every member has registered that it knows
  schema 5.
- **Refused below level 5, as "unavailable"** (503 in both APIs): setting or removing a
  quota, a recount, defining or deleting a target, a notification or replication
  document, replica mode, a replica request, a resync. None of these existed before.
- **Left out below level 5:** `by` on a delete, and with it nothing else.
- **Nothing is refused that worked before.** Puts, deletes, multipart uploads, bucket
  creation and every configuration of schema 4 work at level 4 exactly as they did.
- A proposer that writes an entry as schema 5 a moment early is refused by the leader,
  which logged nothing, and writes it again below (`Store.propose`) - for the entries
  that have a lower form; for the others it answers "unavailable".
- A member that meets an entry of a schema it does not know stops, as before.
- The state becomes schema 5 with the first schema-5 entry in the log - whether it took
  effect, was refused, or was applied as nothing - for the reason ADR-0016 gives. The
  release before then refuses the state, and a snapshot of it.
- Once the state is of schema 5, a member that has not registered no longer holds the
  cluster below 5 (`Store.entryVersion`).

### Bucket usage

**What is counted.** Every stored version, current or not, by its `Size` - the bytes of
the object as it was sent, not what it takes on the drives. A delete marker is a
version of no bytes. The parts of multipart uploads that are neither completed nor
aborted are counted apart, as `upload_bytes`. `objects` and `current_bytes` are the
current versions that are not delete markers: the two figures the API always gave.
`replication_pending` and `replication_failed` are the versions of the bucket whose
`replication` is `PENDING` and `FAILED` (ADR-0026): they follow a commit that makes a
version `PENDING`, every result of an `outbox.ack` that changes a status, and the removal
of such a version, so that the replication status of a bucket is exact and not what one
leader remembers. They count versions, not bytes, and nothing of them counts against a
quota.

**How it is kept.** Each function of the state machine that changes a key - a put, a
completion, a delete in each of its three branches, the delete of schema 1, a part, an
abort, the deletion of a bucket - calls one helper with the change it made, and so does `outbox.ack`
for a status it changed. The change is a `bucket.UsageDelta`: the key whose versions
changed, the upload whose parts changed (the completion of an upload is both), the
sequence of a version it committed, and what the key has after less what it had before.
`objects` and `current_bytes` come from reading `objects[key]` before and after. The
record is kept through `bucket.Usage` alone - `Add`, `Counting`, `Step`, `Finish` - and
nothing is computed by a member from anything but the entry and the state.

**A record is exact, or being counted, or both** (`Usage.Validate`); the zero record is
no record. It is read with `bucket.ParseUsage`, which takes the bytes `Record` writes and
no others: a field too many, a field twice, another order or white space is refused, and
a `"v"` that is not 1 is `ErrUnsupportedFormat`.

**No count goes below nothing, and none wraps.** A change that would take a count below
0 leaves it at 0; one that would take it past `math.MaxInt64` leaves it there. Every
member clamps alike, so the records stay identical. `used` (ADR-0024) is clamped the
same way. **A clamp begins a recount.** No change of a real bucket is held at a bound: a
count that is, was wrong before and is wrong now. So `Usage.Add` returns, with the
record, whether it clamped - and the record it returns then has a count begun, as
`Counting` begins one (a count that was under way begins again; a record that was exact
stays exact and goes on being kept meanwhile). That is decided in apply, from the record
and the entry alone, so every member does it alike; the leader's counter proposes walk
entries for every record that has a count under way, the walk puts the totals right, and
`Finish` reports the difference as drift. The flag itself is not stored: the store counts
it in `cactusfs_bucket_usage_clamped_total` (PLAN 8.0.7).

**One `Add` is one key and one upload.** While a bucket is being counted, whether a
change counts is decided by where its key and its upload are; an entry that changes
several keys calls `Add` once for each. The changes of one key may be summed into one
delta - the property tests hold that grouping, with and without a walk under way - and
those of two keys may not.

**`high` and the sequence of a version.** `high` is the greatest sequence (`Seq`,
ADR-0008) committed in the bucket. A version made here is given
`Seq = max(index of the entry, high + 1)` (`bucket.Usage.NextSeq`, which does not wrap:
at the last number a `uint64` holds it stays there). For a bucket that never took a
version from another deployment that is the index of the entry, as it always was. A
replica's sequence is at most 2^62 (`object.MaxReplicaSeq`), so there is always a
sequence above every replica: without the bound, one replica at 2^64-1 would wrap
`high + 1` to 0 and nothing written here could ever outrank it. A bucket that was
a replica holds versions at the sequences of its source, which are unrelated to the
indexes here; `high` keeps a version written after promotion the newest of its key
(ADR-0026).

**A new bucket** is made exact and zero by its `bucket.create` of schema 5.

**A bucket that exists** has no record until the leader proposes the first
`bucket.usage.walk` for it, which makes one in counting state: `exact` false, **the
totals zero**, and a `count` in phase 1 with an empty cursor. While a bucket is counted
for the first time its totals stay zero - only `high` and `count` move - and nothing may
be read from them. The count is online and inside apply:

- a walk entry visits **whole object keys** - the current version and every other
  version of the key, read from `objects/<bucket>` and `versions/<bucket>` merged in
  key order - until it has visited `limit` records, and ends at a key boundary
  (phase 1, `UsagePhaseKeys`). `limit` is from 1 to 100 000 (`ports.MaxUsageWalk`); the
  leader proposes entries of `limit` 10 000, one every 100 ms;
- when the keys are done, the walk goes through `parts/<bucket>` upload by upload, in
  the order of the upload IDs as text (phase 2, `UsagePhaseUploads`). A phase that ends
  leaves the cursor empty: before everything of the next;
- **phase 3** (`UsagePhaseDone`) is a walk that has been through everything and waits for
  `Finish`. It has no cursor. `Finish` is refused in any other phase, and `Step` in this
  one; the entry that ends the last phase finishes the walk, as the model of the port
  does, and an entry that finds a record in phase 3 has only to finish it;
- a step moves the cursor forward or not at all, counts versions in phase 1 and nothing
  but `upload_bytes` in phase 2, and raises `high` to the greatest sequence it visited
  (`Usage.Step`; a step the walk cannot have made is `ErrInvalidArgument`);
- while a bucket is being counted, a change is added to `count` if it is one the walk has
  been past, and is left to the walk otherwise: in phase 1 a change of a key at or
  before the cursor, compared byte for byte, and no change of an upload; in phase 2
  every change of a key, and a change of an upload at or before the cursor; in phase 3
  everything. `high` follows the sequence of every change, counted or not;
- at the end (`Finish`) the record is `exact` with what the walk counted, `count` is
  gone, and from then on every change keeps the record.

Counting is deterministic and safe across a snapshot: the cursor is in the record, and
a new leader goes on from it. **A quota and replica mode can be set only on a bucket
whose record is exact** (`ErrConflict`, 409, until then) - and replica mode only where
that record says `versions` is 0 (ADR-0026).

**A recount** (`restart: true`) runs the same walk into `count` while the live totals
go on being kept, and puts its result in their place at the end. A walk that was under
way is thrown away and begun again. A result that differs from the totals it replaces is
drift - a fault of this code - and is counted in `cactusfs_bucket_usage_drift_total`
(`Finish` says so; a first count cannot drift). Without `restart`, a walk under way goes
on, and an entry for a record that is exact and not being counted changes nothing.

### The outbox

**A row is appended in apply** when an entry changes an object of a bucket whose
`notification` or `replication` document has a rule that matches: the kind of change,
and a prefix and suffix of the key compared byte for byte. The row lists the consumers
it is for. Matching in apply makes the two canonical documents and their match rule
part of the replicated format, like the rules of `bucket.Activity`: they are pinned by
golden files and property tests, and a change of them is a rise of the schema.

A row is validated as `delivery.Item` is: one row for a change, for all the consumers it
has, and a target a consumer of it once at most. A target is of one kind, so a row is for
it as `n:` or as `r:`, never both.

**One log, one cursor a target.** The node that leads delivers: for each target it
reads the rows after the target's cursor, in order, hands each to what delivers that
kind of consumer, and acknowledges through Raft with `outbox.ack` every 100 rows or
every second. Delivery is therefore **at least once**; what is sent twice is bounded by
one batch for each change of leader.

**Reading** (`ports.Outbox.ReadOutbox(ctx, target, after, limit)`, a read and no entry)
returns the rows after `after` that are for the target, in ascending order, at most
`limit` of them (1 to 1 000, `delivery.MaxOutboxRead`) - **and `upto`: the place of the
last row it looked at**, which is that of the last item or beyond it. Rows for other
targets are passed over, and a target that acknowledges `upto` has them behind it;
without it a target with no rows of its own would hold every row in the outbox for
ever. With nothing to look at `upto` is `after`; a read that returns no items and an
`upto` beyond `after` is repeated from `upto`. Rows at or before the cursor's `from` are
never returned.

**`outbox.ack`**, in this order:

- it is refused whole, with `ErrInvalidArgument` and nothing changed, when it carries
  more than 1 000 results, a result that `delivery.Result.Validate` refuses (a row and a
  version both, neither, or a status that is not `COMPLETED` or `FAILED`), or **any
  result for a target that is not of kind `s3`**: results are of replication. A target
  that is not there is `ErrEntityNotFound`;
- **`upto` is clamped**: an `upto` beyond the last row there is counts as that row - and
  as the cursor's own place when the outbox is empty - so that no acknowledgement passes
  what was not written yet;
- the cursor moves forward only (`Cursor.Advanced`): an `upto`, or an `audit_upto`,
  behind the cursor changes nothing and is no error - it is the acknowledgement of a
  node that led before. **`audit_upto` moves `audit_acked` of a target with `audit:
  true` and of no other** (ADR-0025);
- each result sets the `replication` of the version it names (ADR-0026). A result by
  `seq` names the version of that row, **when the row is still there and was for this
  target as `r:`**; a result by `{b, k, vid, status}` names the version itself - that is
  how a version a resync sent, or one whose row was dropped at the bound, gets its
  status. Either way the status is written only on a version that still exists and is
  not `REPLICA`; a result that names what is gone changes nothing and is no error. A
  status that changes moves `replication_pending` and `replication_failed` of the
  bucket's usage record;
- it then trims rows at or below the lowest cursor of all targets, at most 1 000 an
  entry (`delivery.MaxOutboxTrim`).

A bucket that is deleted leaves its rows to their targets.

**The state of the outbox** as `ports.Outbox.OutboxCursors` returns it is `{cursors,
rows, dropped}`: the cursor of every target in name order, how many rows the outbox
holds (`outboxcount`, at most `MaxOutboxRows`), and how many were pushed out at the
bound since the deployment began (`outboxdropped`).

**The bound.** `MaxOutboxRows = 1 000 000`, a constant of schema 5 in
`internal/domain/delivery`, as `MaxOutboxTrim`, `MaxAckResults` and `MaxOutboxRead` (1 000
each) are. At the bound the oldest row is dropped, `outboxdropped` rises, and each
consumer of that row that had not passed it gets `lost` raised by one - and
`resync_needed` for a replication consumer (`Cursor.Dropped`). `resync_needed` is
cleared by a `replication.resync.put` whose `state` is `done`, for the target it names.
**A target that cannot be reached never refuses a write and never grows the
state without bound; past the bound it loses its oldest rows, and that is counted, said
in its status, in a metric and in the audit trail.** At the bound the outbox is some
hundreds of megabytes of the bbolt file, and of every snapshot.

A cursor is made with its target and begins at the entry that made it (`from`): a
target defined under a name that was used before is sent nothing of before. It stays
with a target that is replaced, and goes with one that is deleted.

### Targets

A target is a place outside the deployment that it sends to: a webhook, a NATS server
with JetStream, or another CactusFS deployment (`s3`).

- **Defined by who administers the cluster, and by nobody else**: `admin:PutTarget`,
  `admin:DeleteTarget`, `admin:GetTarget`, `admin:ListTargets`, `admin:GetTargetStatus`,
  on `arn:cactusfs:admin:::target/<name>` (the list: `arn:cactusfs:admin:::targets`).
  A bucket's document names a target, **never an address**. This is the position on
  request forgery: whoever may set a bucket's notification rules cannot make the
  cluster connect to a place of their choosing.
- **Each target lists the buckets that may use it** (`buckets`: names, or patterns with
  `*`). The list must be said; an empty list admits none.
- **Apply refuses** a `notification` or `replication` document that names a target that
  does not exist, is of a kind that does not fit, or does not admit the bucket. `refs`
  counts the documents that name a target and follows put, replace, removal and the
  deletion of a bucket; a target with `refs > 0` cannot be deleted (`ErrEntityInUse`).
- **A target that documents name cannot change its kind** (`ErrEntityInUse` while
  `refs > 0`): what was matched for a webhook is not to be handed to another
  deployment. Everything else of a target is replaced whole by `target.put`, but for
  three things that are the store's: `refs`, which it counts; `created_at`, which a
  target that exists keeps; and the cursor.
- **Apply judges bytes; the use case judges what they mean.** `delivery.Target.Validate`
  runs in apply on members built with different releases of Go, so it decides by rules
  of bytes that are written out in the domain and calls no clock, no resolver and no
  parser of the standard library whose answer a release may change - not `net/url`, not
  `encoding/pem`, not `encoding/base64`, not `crypto/x509`: the name; the kind; an
  endpoint of 1 to 2 048 bytes of printable ASCII that begins with the scheme of the
  kind and holds none of `@`, `?`, `#` and `\`; at most 1 000 `buckets`, each 1 to 63 of
  the characters of a bucket name and `*`, none twice; `ca_pem` of at most 64 KiB that
  is **framed** as certificates and as nothing else - lines ending in LF or CRLF; only
  empty lines outside a block; a block is the line `-----BEGIN CERTIFICATE-----`, one or
  more lines of 1 to 64 characters of the base64 alphabet, `=` once or twice at the end
  of the last of them only, a multiple of four characters in all, then
  `-----END CERTIFICATE-----`; no header, no other label, no text between blocks; and
  what fits the kind - a `webhook`
  has `auth.mode` `none` or `bearer` and neither `region` nor `subject`; `nats` has
  `none`, `token` or `basic`, a `subject` without wildcards and no `region`; `s3` has
  `sigv4`, a `region`, no `subject`, and is never sent the audit trail. A mode other
  than `none` needs a secret and `none` has none; `auth.id` is there for `basic` and
  `sigv4` and only there; only a `webhook` has a `sign_key`. Whether the endpoint can be
  connected to and whether the certificates can be read is asked by the use case before
  the target is proposed (`delivery.CheckEndpoint`; `delivery.CheckCA`, which is where
  `encoding/pem` and `crypto/x509` are called). Whatever `CheckCA` takes, `Validate`
  takes; the other way round does not hold, and need not: bytes that are framed as a
  certificate and are none are kept by every member alike - no use case proposes them -
  and fail when the target is connected to.
- **The path of an endpoint is shown to nobody.** The URL of a webhook often carries a
  token in its path (incoming-webhook URLs do), and `admin:GetTarget` and
  `admin:ListTargets` are held by the built-in `auditor`. So no answer of the API gives
  the path back, to any caller: `Target.endpoint` is the origin - the scheme, the host,
  and the port when the endpoint names one - with `endpoint_path_set`. `TargetPut.endpoint`
  is the whole URL and is write-only in effect; a `PUT` of a target that exists without
  `endpoint` keeps the stored one, as one without `auth.secret` keeps the secret, and to
  change any part of the endpoint the whole URL is sent again. In the domain what is
  shown of a target is `delivery.Target.Shown`, a type of its own (`ShownTarget`) that
  has no field for a path or a secret, made by `Target.Origin`, which cuts by bytes at
  the first slash after `://`; every printed form of a `Target` and of an `Endpoint` -
  `fmt`, `slog`, JSON - leaves the path out. The path is stored in the clear in the
  target record (metadata is not encrypted at rest, as before) - see "Open".
- **Secrets are sealed through the KMS exactly as the secrets of API keys are**
  (`iam.Seal`, ADR-0006, ADR-0010): a data key of their own, wrapped by the KMS key,
  with the additional data `target/<name>/secret` and `target/<name>/sign_key`, so that
  a sealed secret moved to another target or another field does not open. They are
  write-only in the API, are opened only by the node that leads at the moment it
  connects, and are in no log line, audit record, event, span or status: every printed
  form of a target - any verb of `fmt`, `slog`, and JSON - is its name and its kind. **The
  crypto design does not change.**
- A target's name is 1 to 63 lower-case letters, digits and hyphens, and no hyphen
  first (`delivery.ValidateTargetName`): it is part of the ARNs of the S3 API and of the
  consumers in the outbox.
- A change of a target and its deletion carry their audit record in the entry
  (`audited`, as sub-buckets do) and publish `cactusfs.cluster.target-changed.v1` and
  `cactusfs.cluster.target-deleted.v1`, which name the target and nothing of it.

### Outbound connections

One policy for every connection a target causes. The rules that judge an endpoint and
an address are in the domain (`internal/domain/delivery`); the dialer is an adapter
(`internal/adapters/egress`).

- **An endpoint** (`delivery.CheckEndpoint`) is `https://` for the kinds `webhook` and
  `s3` and `tls://` for `nats`; the port it names, or 443, or 4222 for `nats`. Refused:
  more than 2 048 bytes; a space, a backslash, or anything that is not printable ASCII;
  another
  scheme; userinfo, and an `@` anywhere; a query or a `?`; a fragment or a `#`; a zone or
  anything escaped in the host; a port that is not from 1 to 65535; a host that is
  neither an IP address nor a DNS name (labels of letters, digits, hyphens and
  underscores, 1 to 63 bytes each, 253 in all) - **and a name whose last label is a
  number**, which some resolvers read as an IPv4 address; a literal address that nothing
  could admit (below); a path for `nats`, and any path but `/` for `s3`, which is
  addressed path-style from its root. The refusal never repeats the endpoint or any part
  of it: what was typed may hold a credential.
- TLS 1.2 or newer, verified against the target's `ca_pem` or the system's roots.
  **There is no way to skip verification.**
- **No redirect is followed**: a 3xx is a failure of class `redirect`. No proxy is
  used, whatever the environment says.
- Timeouts: 5 s to connect, 5 s for the handshake, 30 s for the headers of a response.
  A response body is read to 64 KiB and thrown away.
- **The resolved address is checked in the dialer's `Control` hook at every connect**,
  which is what defeats a name that resolves to one address when it is checked and to
  another when it is used.
- **An address** (`delivery.AllowedAddress`). Never, whatever `--egress-allow` holds:
  an address that is none; the unspecified address and the rest of `0.0.0.0/8`;
  multicast; the IPv4 broadcast address; and the addresses where the metadata service
  of a cloud answers, kept as one table in the domain: 169.254.169.254 (AWS, Azure,
  Google Cloud and others), `fd00:ec2::254` (AWS over IPv6), `fd20:ce::254` (Google Cloud
  over IPv6) and 100.100.100.200 (Alibaba Cloud). Only where `--egress-allow` names it:
  loopback and link-local.
- **That table is best effort.** It holds the addresses known today; a cloud that
  answers elsewhere is not in it, and the operator's own network policy is what stands
  behind it (the chart's default-deny `NetworkPolicy`, PLAN 8.2.9). And
  **`--egress-allow` of a link-local range opens what is in it**: to allow
  `169.254.0.0/16` is to allow every service that answers there but the addresses of the
  table - a task-credential endpoint among them. Allow the one address a receiver has.
- **Private ranges are allowed**: on premises, that is where targets are.
- An IPv4 address written as IPv6 - mapped, or under the well-known NAT64 prefix
  `64:ff9b::/96` - is judged as the IPv4 address it reaches, so a range of IPv4
  addresses is admitted by an IPv4 prefix and by no other. A zone is not looked at.
- **An IPv6 address that stands for an IPv4 one by a rule a gateway decides is refused
  outright unless a prefix of `--egress-allow` names it**: IPv4-compatible addresses
  (`::/96`, other than `::1`, which is loopback, and `::`), the local-use NAT64 prefix
  `64:ff9b:1::/48`, and 6to4 (`2002::/16`). What such an address reaches cannot be
  judged from the node. Where the range fixes which IPv4 address is meant (`::/96`,
  `2002::/16`), one that is never connected to is not connected to this way either,
  whatever was allowed.
- `--egress-allow` (CIDRs; none by default) admits loopback, link-local and those
  ranges again - **never the metadata addresses.**
- Settings: `egress-allow`, `delivery-timeout` (30 s), `replication-workers` (4; 1 to
  32), `replication-rate-mib` (0: not limited).

Telemetry carries no name somebody chose: no metric has a label with a bucket or a
target. Delivery is counted by kind and result; what is known of one target is read
from `GET /api/v1/targets/{name}/status`, which any node answers by asking the leader
over `NodeService.DeliveryStatus`.

### Migration

**None for the state**: a schema-4 state opens as it is and is not modified by being
opened or read. **The usage of buckets that exist is counted** by the walk above,
started by the leader without being asked once the level is 5.

Proved by the fixture `test/integration/testdata/schema-4`, written by the binary of
commit 84334a7 with a manifest of every bucket's figures as its generator wrote them;
by the test that this tree opens it and reads everything the manifest lists, the state
says 4 after that, 5 after the first schema-5 entry, and walk entries of `limit` 50
then bring every bucket to exactly the manifest's figures (PLAN 8.0.10); by
`TestUpgradeFromSchema3`, unchanged; and by a rolling upgrade from the image of 84334a7
with the binaries themselves (`TestRollingUpgradeFromSchema4ToSchema5`, PLAN 8.0.11).

### Rollback

Possible until the first schema-5 entry is applied; from then on the release before
refuses the state.

**In practice that is within seconds of the last member registering.** At level 5 the
leader proposes a `bucket.usage.walk` for the first bucket that has no exact record,
and that entry is of schema 5; so is the first bucket created and the first delete
(which carries `by`). A cluster that holds one bucket has lost its way back before
anybody has used anything of milestone 8. This is sooner than schema 4, where the way
back ended with the first upload, and it is by design: usage that is counted only once
somebody asks would make a quota wait for an operator.

- To keep a way back, hold one member at the previous release: the cluster stays at
  level 4, loses nothing it had, counts nothing, and answers "unavailable" to
  everything of milestone 8.
- The rise is automatic at the last registration. There is no step that finalises it
  and no switch that holds it.
- A node that runs alone is at level 5 from its first start with this release, and has
  no way back from then on if it holds a bucket.
- A cluster that was fully upgraded and is then rolled back does not lose anything, but
  no node starts until it is rolled forward again: an outage, not a loss.
- A member of the release before that is joined to a cluster whose state is 5 stops at
  the first entry of schema 5 or at the snapshot it is sent. It fails safe.

## Consequences

- **Cost on the write path.** One usage record read and written in the transaction of
  every put, completion, part, abort and delete; and one row appended for a bucket
  with a rule that matches. Not measured yet: PLAN 8.0.7 adds a benchmark against
  84334a7.
- **A key with very many versions makes one long step of the walk**: a walk entry ends
  at a key boundary, so a key of a million versions is one apply of about four seconds,
  with every other change waiting. Accepted; it happens once a bucket, and at a
  recount.
- The outbox is in the snapshot: up to some hundreds of megabytes at the bound.
- Rule matching, the two canonical documents and the rules of `bucket.Usage` are part
  of the replicated format.
- Metadata is not encrypted at rest (as before): the outbox holds object keys and the
  names of actors; a target record holds its endpoint in the clear and its secrets
  sealed.
- One slow target delays only itself. A target that many buckets use is one queue:
  what stalls it stalls all of them.
- The whole state machine of three features lands before any of them can be used. If
  the design of one moves after level 5 is rolled out, it costs a schema 6.

## Alternatives

- **A scan when a write arrives** - bounded at 100 000 keys, slow, and wrong beyond
  the bound.
- **A walk of every bucket in one apply transaction** - stalls every member at once
  for as long as the largest bucket takes.
- **A total computed by the proposer** - one member's scan trusted by all, under a
  long read transaction; nothing derived from the log.
- **The schema byte as the switch for usage** - see above: drift for good.
- **The Raft log as the cursor of delivery** - truncated behind snapshots.
- **An outbox a bucket** - a cursor for each pair of bucket and target, no order across
  buckets for one target, and the trim is a walk of all of them.
- **A cursor kept by the node, not replicated** - a new leader would send everything
  again, or nothing.
- **Bucket owners naming URLs** - request forgery by design; see Targets.

## Open, for the owner

Defaults are applied until answered.

1. **Is losing the way back to schema 4 within seconds of the upgrade acceptable, or
   should the count wait for an explicit switch?** Default: automatic, as schema 4
   was. The other answer is a command that an administrator sends to "finalise", with
   the leader proposing no walk before it; it is not planned.
2. **May bucket owners define targets?** Default: no - `cluster-admin` only, and each
   target must list the buckets that may use it. A sidecar on loopback needs
   `--egress-allow`.
3. **May anybody be shown the path of a target's endpoint?** Default: **no - the path is
   never shown**, to no caller and in no answer, log, status or event (the stricter
   reading of CLAUDE.md section 3: a path may be a token, and `auditor` reads targets).
   The cost: an administrator cannot read back what was typed, and changes the host by
   sending the whole URL again. The other answers are an action of its own
   (`admin:GetTargetEndpoint`, held by `cluster-admin`) or showing the path to whoever
   holds `admin:PutTarget`; neither is planned. Not decided here either: whether the
   path should be **sealed** through the KMS like the secret, so that it is not in the
   clear in the metadata and its snapshots - that is a field of the target record, and
   has to be ruled before the codec of PLAN 8.0.8 is written.
4. **Should a count that was clamped stop being called exact?** Default: no - the record
   stays exact, a recount begins in the same entry, and the clamp is counted. A quota
   therefore goes on being judged by a figure that is known to be wrong until the
   recount ends (at 0: it admits more than it should; at the bound: nothing). Only a
   fault of this code clamps.

Ruled on 2026-10-12 and now in the text above: a result of `outbox.ack` may name a
version as well as a row; the usage record carries `replication_pending` and
`replication_failed`. The canonical document of replication, which the plan did not write
down, is `bucket.ReplicationConfig.Document` with its golden file and its fuzz test
(ADR-0026).

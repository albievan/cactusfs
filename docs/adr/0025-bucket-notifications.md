# ADR-0025: Bucket notifications and the export of the audit trail

- **Status:** proposed (2026-10-12). It becomes accepted when the code of PLAN 8B has
  landed and the gates have seen it (PLAN 8.2.9)
- **Date:** 2026-10-12
- **Revised:** 2026-10-12, to the owner-side rulings of that day (PLAN, Decisions: "the
  holes the writing of ADR-0023 to 0026 found", point 6) and to the domain of PLAN 8.0.4
  (`internal/domain/delivery`, `internal/domain/bucket/notification.go`)
- **Builds on:** ADR-0023 (entry schema 5: the outbox, targets, outbound connections),
  ADR-0006 (the audit trail), ADR-0011 (bucket configurations), ADR-0016 (who uploaded)

## Context

CLAUDE.md names bucket notifications and audit export, through NATS JetStream and
webhooks, as what `ports.EventPublisher` is for. Until now there was the in-process
bus only: at most once, lost at a restart, and never leaving the node.

A notification is a promise to somebody else's program - "tell me when an object
arrives". Such a program is written against S3's notifications; it expects their
shape, and it expects not to miss one. And a notification is a request the cluster
makes to an address: who may choose that address is a question of security before it
is one of features.

## Decision

### Targets are the administrator's

A notification goes to a **target** (ADR-0023): defined by whoever holds
`admin:PutTarget`, which of the built-in roles is `cluster-admin` alone. A bucket's
configuration names a target; it never holds an address. A target lists the buckets
that may use it. Whoever may configure a bucket (`s3:PutBucketNotification`, which
`bucket-admin` holds by `s3:*`) chooses among the targets the administrator opened to
that bucket, and nothing else.

### Where a target may be

The administrator's word is not enough either: an endpoint is judged when the target is
defined, and the address it resolves to at every connect. The rules are ADR-0023's and
are in the domain (`internal/domain/delivery/endpoint.go`); as they bear on the two
kinds of target a notification goes to:

**`delivery.CheckEndpoint` refuses**, with `ErrInvalidArgument` and without repeating the
endpoint or any part of it:

- an endpoint that is empty or longer than 2 048 bytes, or that holds a space, a
  backslash, or anything that is not printable ASCII;
- a scheme other than `https://` for a webhook and `tls://` for a NATS server: nothing
  is sent in the clear;
- userinfo, and an `@` anywhere - a credential belongs in the target's sealed secret,
  never in an address that is shown and logged; a query, or a `?`; a fragment, or a `#`;
- a zone, or anything escaped, in the host; a port that is not from 1 to 65535 (none
  means 443, and 4222 for NATS);
- a host that is neither an IP address nor a DNS name - labels of letters, digits,
  hyphens and underscores, 1 to 63 bytes each, 253 in all - and **a name whose last label
  is a number**, which some resolvers read as an IPv4 address;
- a literal address that no setting could admit (the "never" of the list below);
- a path for a NATS server. A webhook may have one.

**`delivery.AllowedAddress` refuses**, of the address a name resolved to, at every
connect:

- never admitted, whatever `--egress-allow` holds: an address that is none; the
  unspecified address and the rest of `0.0.0.0/8`; multicast; the IPv4 broadcast
  address; and the addresses of the metadata services of clouds - 169.254.169.254,
  `fd00:ec2::254`, `fd20:ce::254` and 100.100.100.200, a list that is best effort
  (ADR-0023);
- admitted only where `--egress-allow` names it: loopback and link-local - the
  operator's setting, for a receiver on the node itself;
- an IPv4 address written as IPv6, mapped or under the NAT64 prefix `64:ff9b::/96`, is
  judged as the IPv4 address it reaches, and a range of IPv4 addresses is admitted by an
  IPv4 prefix and by no other; a zone is not looked at;
- an address of `::/96`, `64:ff9b:1::/48` or `2002::/16` - an IPv4 address behind a
  gateway's rule - is refused unless a prefix of `--egress-allow` names it.

A webhook's path is kept and used, and shown to nobody: an answer of the API gives the
origin of a target's endpoint and whether it has a path (ADR-0023).

Everything else is allowed, private ranges among it: on premises that is where receivers
are. A refused address is the failure `egress_refused`.

### Configuration

S3's: `PutBucketNotificationConfiguration` and `GetBucketNotificationConfiguration`
(`PUT` and `GET /{bucket}?notification`), authorized as `s3:PutBucketNotification` and
`s3:GetBucketNotification`. The management API has the same document as JSON under
`/buckets/{bucket}/notifications`, with the same two actions.

**Accepted XML**, decoded strictly - an element that is not listed here is refused,
never passed over:

```xml
<NotificationConfiguration>
  <QueueConfiguration>
    <Id>name-of-the-rule</Id>
    <Queue>arn:cactusfs:notify:::TARGET</Queue>
    <Event>s3:ObjectCreated:*</Event>
    <Filter><S3Key>
      <FilterRule><Name>prefix</Name><Value>logs/</Value></FilterRule>
      <FilterRule><Name>suffix</Name><Value>.gz</Value></FilterRule>
    </S3Key></Filter>
  </QueueConfiguration>
  <TopicConfiguration>
    <Id>...</Id><Topic>arn:cactusfs:notify:::TARGET</Topic><Event>...</Event><Filter>...</Filter>
  </TopicConfiguration>
</NotificationConfiguration>
```

- `QueueConfiguration` and `TopicConfiguration` mean the same here; which one a rule
  was put as is kept (`as`) so that a GET returns what was put, in order.
- The ARN is `arn:cactusfs:notify:::<target>`. The target must exist, be of kind
  `webhook` or `nats`, and admit the bucket.
- **Events accepted:** `s3:ObjectCreated:*`, `s3:ObjectCreated:Put`,
  `s3:ObjectCreated:CompleteMultipartUpload`, `s3:ObjectRemoved:*`,
  `s3:ObjectRemoved:Delete`, `s3:ObjectRemoved:DeleteMarkerCreated`.
- An `Id` that is left out is made by the server, as S3 does.
- An empty `<NotificationConfiguration/>` removes the configuration, as in S3. There is
  no DELETE in the S3 API; the management API has one.

**Refused:**

| What | Answer |
|---|---|
| `CloudFunctionConfiguration`, `EventBridgeConfiguration` | `501 NotImplemented` |
| an event S3 has and CactusFS does not tell - `s3:ObjectCreated:Copy` and `:Post`, `s3:ObjectTagging:*`, `s3:ObjectRestore:*`, `s3:Replication:*`, `s3:LifecycleExpiration:*` and the others | `501 NotImplemented` |
| an event name S3 does not have | `400 InvalidArgument` |
| a filter rule whose name is neither `prefix` nor `suffix`, or two of one name | `400 InvalidArgument` |
| an ARN of another form; a target that is not there, is of kind `s3`, or does not admit the bucket | `400 InvalidArgument` |
| anything else that is not the document above | `400 MalformedXML` |
| any of it below level 5 | `503 ServiceUnavailable` |

A copy arrives in the metadata as a put and is told as `ObjectCreated:Put`; that is
why `s3:ObjectCreated:Copy` cannot be asked for by itself.

**The canonical document**, which is what is replicated and matched in apply:

    {"v":1,"targets":[name],"rules":[{"id","as":"queue|topic","target","on":[class],"prefix","suffix"}]}

with `on` among `put`, `complete`, `marker`, `delete`, in that order, each once and at
least one; `targets` the names the rules use, sorted, once each; `prefix` and `suffix`
left out when empty. A document has 1 to 100 rules (`bucket.MaxNotificationRules`) and
at most 64 KiB; an `id` is 1 to 255 bytes and unique in the bucket, a prefix and a
suffix at most 1 024 bytes, all valid UTF-8. Its bytes are pinned by the golden file
`internal/domain/bucket/testdata/notification.golden.json`. Encoding it twice is encoding
it once, and `bucket.ParseNotificationConfig` takes the bytes `Document` writes and no
others: a field too many or twice, kinds of change in another order, white space, or a
list of targets that is not the rules' is refused, so that a document means to every
member what it reads as.

`NotificationConfig.Match(event, key)` gives one consumer `n:<target>` for every target
that a rule for that kind of change names, whose prefix the key begins with and whose
suffix it ends with, byte for byte - sorted by target, each once.

### What is told

A row of the outbox is appended in apply (ADR-0023) for each change a rule matches:

| Change | Class | `eventName` |
|---|---|---|
| a version committed by a put or a copy | `put` | `ObjectCreated:Put` |
| a version committed by the completion of a multipart upload | `complete` | `ObjectCreated:CompleteMultipartUpload` |
| a delete that leaves a delete marker | `marker` | `ObjectRemoved:DeleteMarkerCreated` |
| a version removed for good, by a caller or by a lifecycle rule | `delete` | `ObjectRemoved:Delete` |

Not told: a part, the creation or the abort of an upload, a change of tags, retention
or legal hold, a write that was refused, a read. A version that arrives in a replica
bucket (ADR-0026) is told as the put or the marker it is.

### The record

S3's event notification, **one record a message**
(`cactusfs.notification.object.v1` in `api/asyncapi/events.yaml`):

```json
{"Records":[{
  "eventVersion":"2.1","eventSource":"cactusfs:s3",
  "eventTime":"2026-10-12T09:30:00.123456789Z","eventName":"ObjectCreated:Put",
  "userIdentity":{"principalId":"user/alice"},
  "s3":{"bucket":{"name":"photos"},
        "object":{"key":"2026/a+b.jpg","size":1024,"eTag":"9b2c...","versionId":"0f3a...","sequencer":"00000000000004d20000"}}
}]}
```

| Field | What it is |
|---|---|
| `eventVersion` | `2.1`, the version of S3's record this follows |
| `eventSource` | `cactusfs:s3` - not `aws:s3`: a receiver must be able to tell |
| `eventTime` | the time of the entry, which is the leader's (ADR-0008) |
| `eventName` | as above, without the `s3:` the configuration writes, as in S3 |
| `userIdentity.principalId` | who made the change, as the audit trail names actors: `created_by` of the version, `by` of a delete. **Absent when it was not recorded** |
| `s3.bucket.name` | the bucket |
| `s3.object.key` | the key, URL-encoded as S3 encodes it in a notification |
| `s3.object.size`, `eTag` | of the version; absent for a delete marker |
| `s3.object.versionId` | the version or the marker; absent in a bucket that keeps no versions |
| `s3.object.sequencer` | the key of the outbox row in hexadecimal, 20 characters: of two records the greater was made later |

**What is absent, on purpose.** `requestParameters.sourceIPAddress`: the metadata holds
no address of a client, and behind a balancer it would be the balancer's (threat
model, question 29). `responseElements`, `awsRegion`, `s3.configurationId`,
`s3.bucket.arn` and `ownerIdentity`: nothing here would make them true. **Object
contents, never.** User metadata and tags, never.

### Delivery

Each adapter is a `ports.EventPublisher`, one instance a target, made by the node that
leads when it first has something for the target.

**At least once.** A row is acknowledged through Raft only after the target has taken
it (ADR-0023). After a failure, and after a change of leader, up to one batch of 100
is sent again. **Consumers deduplicate by the ID of the message**, which is the same
every time it is sent: for a notification it is the `sequencer` of the record, the place
of the row in 20 hexadecimal characters (`delivery.Item.ID`).

**Order.** A target receives its messages in ascending `sequencer`, which is the order
the changes were made in the deployment - so, in particular, the changes of one key in
their order - except where one is sent again. Nothing is delivered in parallel to one
target.

**A webhook** is sent a `POST` to the target's endpoint with the record as its body.

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `User-Agent` | `cactusfs/<version>` |
| `X-Cactusfs-Event-Id` | the ID of the message |
| `X-Cactusfs-Event-Type` | `cactusfs.notification.object.v1` or `cactusfs.audit.exported.v1` |
| `X-Cactusfs-Timestamp` | when this attempt was made, in seconds since the Unix epoch |
| `X-Cactusfs-Signature` | `v1=` and the lower-case hexadecimal of HMAC-SHA256, under the target's signing key, of: the timestamp, a full stop, the body |
| `Authorization` | `Bearer <secret>`, when the target has one |

**How a receiver verifies.** Take the body as bytes, before any parsing. Compute
HMAC-SHA256 over `<X-Cactusfs-Timestamp>.<body>` with the signing key; compare with the
header in constant time; refuse on a mismatch. Refuse a timestamp more than five
minutes from the receiver's clock. Remember the IDs taken, for as long as a message may
be sent again, and answer 2xx to one that was taken already. The signing key is made
by the server when the target is defined, shown once, and replaced by asking for a new
one; the timestamp is of the attempt, so that a message that waited an hour is not
refused for its age.

**NATS JetStream.** `Publish` to the target's one `subject`, with the record as the
payload, the header `Nats-Msg-Id` set to the ID of the message - JetStream
deduplicates by it within its window - and `Cactusfs-Event-Type`; then wait for the
acknowledgement of the stream. The subject is the same for every bucket and every
kind of change: it is the administrator's to choose, and **the stream that takes it is
the operator's** - CactusFS neither creates nor changes a stream, and a subject that no
stream takes is the failure `no_stream`. TLS is required; the server is authenticated
by its certificate and the client by a token, or a user and a password. The connection
is made through the dialer of ADR-0023, so every address the client is given - by the
endpoint or by the server it reached - passes the same check.

**Failures.** What a failure is called, and what follows:

| Class | When | What follows |
|---|---|---|
| none: delivered | a 2xx from a webhook; a JetStream acknowledgement | the row is passed |
| `rejected` | a 4xx from a webhook other than 401, 403, 408 and 429 | **the row is passed and counted**: a receiver that says no to one message must not hold all the others |
| `auth` | **a 401 or a 403 from a webhook**; a NATS server that does not take the token | tried again; nothing is passed; **the target is `stalled`** - somebody has to act |
| `server` | a 5xx, a 408, a 429 from a webhook, and an answer that is no status at all | tried again; nothing is passed |
| `redirect` | any 3xx: it is never followed | tried again; nothing is passed |
| `timeout` | the target did not answer in time | tried again; nothing is passed |
| `connect` | the target could not be connected to | tried again; nothing is passed |
| `tls` | the handshake failed, or the certificate does not chain to what the target says it must | tried again; nothing is passed |
| `egress_refused` | an address the deployment does not connect to (`AllowedAddress`) | tried again; nothing is passed |
| `no_stream` | a NATS subject that no stream takes | tried again; nothing is passed |
| `internal` | a failure on this side of the connection - the credential could not be opened - and any error that carries no class | tried again; nothing is passed |

These are the classes of `internal/domain/delivery/class.go` that a notification can
meet (`delivery.WebhookClass(status)` is the table's rows for a webhook). The list is one
for all targets; replication adds `sse_c`, which passes the row as `rejected` does, and
`no_such_bucket`, `versioning`, `not_replica` and `quota` (ADR-0026). **Only `rejected`
and `sse_c` pass a row** (`Class.Passes`); every other failure holds the target where it
is and is tried again for as long as it takes.

**A 401 and a 403 are of the credential, not of the message.** As `rejected`, a bearer
secret that was mistyped or has expired would pass every message undelivered, with a
counter as the only trace. As `auth` it loses nothing: the target stalls, its status
says `stalled` and `auth`, `CactusFSDeliveryStalled` fires when its oldest row has waited
15 minutes (PLAN 8.2.7), and delivery goes on from the same row once the secret is put
right.

**The pause** between two attempts at a target that fails (`delivery.Backoff`) doubles
from 1 s (`BackoffMin`) after the first failure to 5 min (`BackoffMax`), which it
reaches at the tenth failure in a row; jitter then takes up to a quarter off it, so that
targets that failed together are not tried together. It is never longer than 5 min and
never shorter than 750 ms.

The error's own words stay in the log of the node that leads, and never in a status,
an event or the audit trail; neither does the endpoint. A failure is a `delivery.Error`,
whose text is its class and nothing else; and a class that another node tells is shown
only if it is a word - at most 32 lower-case letters and underscores - and as `internal`
otherwise (`delivery.ShownClass`), so that nothing that arrives where a class belongs can
be the words of an error, or an address.

**Overflow.** A target that takes nothing keeps its rows until the outbox is at its
bound; then its oldest are dropped and counted (`lost`, ADR-0023), an audit record
`system/outbox` says so at most once a minute, `cactusfs_outbox_dropped_total` rises
and `CactusFSOutboxDropping` fires. Writes never fail and never wait for a target.

**Start.** Connections are made by the worker, when it first has something to send. A
target that cannot be reached holds up neither the start of a node, nor its readiness,
nor a write.

### The audit trail through a target

A target with `audit: true` is sent every record of the audit trail, from the record
after `audit_acked` on, as `cactusfs.audit.exported.v1`: the whole record, as the
node's own exporter writes it to its output, with `previous_hash` and `hash`. In
sequence, at least once, with the ID `audit-<sequence>`. The trail is a durable log
already; all this needs is the cursor. A receiver that holds every sequence can verify
the chain. The exporter to the node's output is unchanged.

### What leaves the cluster

To a place the administrator named, and to no other: object keys, sizes, entity tags,
version IDs, the names of actors, times; and with `audit: true` the audit trail, which
holds the addresses of clients. That is said in the description of a target, in the
README, and in the threat model. What never leaves: object contents, user metadata,
tags, a secret, a key, a token.

## Consequences

- A receiver written for S3's notifications reads these, with three differences it
  must know: `eventSource`, no address of the client, and a copy told as a put.
- A new dependency of the binary: `github.com/nats-io/nats.go` (Apache-2.0), pure Go.
  Standalone needs no NATS server: nothing connects unless a target was defined.
- One queue a target: a webhook that takes a second a message delivers a message a
  second, whatever the rate of changes. Posts are not batched.
- **A receiver must not answer 401 or 403 to say no to one message**: those stall its
  target until somebody acts. A message it will never take is refused with another 4xx
  (400, 422).
- A target that many buckets use learns the keys of all of them; the list of buckets a
  target admits is what keeps tenants apart.
- The names of actors go to the receiver. A deployment that must not tell them has no
  switch for it yet.
- The chart's default-deny NetworkPolicy needs an egress rule for each target
  (`networkPolicy.egressTo`).

## Alternatives

- **Targets defined by whoever configures the bucket** - an address of their choosing,
  called by the cluster.
- **The envelope of the in-process events as the body** - no receiver written for S3
  would read it.
- **Several records a message** - a receiver that fails on the third of five has taken
  two; one record a message makes "taken" mean one thing.
- **At most once, from the bus** - a restart loses what a program was promised.
- **Retrying a rejected message for ever** - one message a receiver will never take
  stops every one behind it.
- **401 and 403 as `rejected`** - a webhook whose secret is wrong would be passed every
  message and delivered none.
- **The address of the client in the record** - see above.

## Open, for the owner

Defaults are applied until answered.

1. **May `nats.go` become a dependency of the binary?** Default: yes.
2. **May bucket owners define targets?** Default: no (ADR-0023).
3. **Is the export of the audit trail part of this milestone?** Default: yes, as PLAN
   8.2.8; it is the first thing to be cut if time is short, and the field of the cursor
   stays so that no later rise of the schema is needed.
4. **Should the address of the client be in a notification?** Default: no.
5. **Decided here beyond the plan, for want of anything written:** the two headers
   `X-Cactusfs-Event-Id` and `X-Cactusfs-Event-Type` (a receiver has to deduplicate by
   something, and one endpoint may take both messages); that neither is covered by the
   signature, which is as the plan fixed it (`timestamp "." body`) - the body carries
   the `sequencer` or the `sequence`, which are; the timestamp being of the attempt;
   the five minutes; `audit-<sequence>` as the ID of an exported record; one NATS
   subject for both messages. If the ID should be signed, the signature becomes
   `timestamp "." id "." body`, and that is cheap now and a new `v2=` later.
6. **The names of actors leave the cluster in `userIdentity`.** Default: they do, as S3
   does. A switch on the target to leave them out would be one field of the target
   record - which is of schema 5, so it wants deciding before PLAN 8.0.8.

Ruled on 2026-10-12 and now in the text above: a webhook's 401 and 403 are the class
`auth`, tried again with the target stalled, and not `rejected`.

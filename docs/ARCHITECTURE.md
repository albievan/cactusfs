# CactusFS architecture

CactusFS is an S3-compatible, erasure-coded, multi-node object store. It ships as one Go
binary that runs standalone on Windows, Linux and macOS or as a cluster, and it puts data
security first. This document describes the design. `CLAUDE.md` holds the rules,
`PLAN.md` holds the work and its status, and `docs/adr/` holds the decisions that are
expensive to reverse.

Sections marked **(implemented)** describe code that exists. What does not exist yet is
said where it belongs, with the milestone or goal of `PLAN.md` that holds it: delivery of
events to NATS and webhooks, replication between sites and quotas (milestone 8, of
which entry schema 5, the domain, the ports, the replicated store and the contracts
exist, and no use case, worker or adapter); sub-buckets (ADR-0017) and tag rules with a
catalogue of tags (ADR-0018), of which only the domain rules, the ports and the
replicated store exist. The release is 0.7.0 (`internal/buildinfo/VERSION`); none has
been published.

## 1. Shape of the system

```
                 S3 clients                     operators, console
                     │ SigV4 / TLS                  │ API key or session / TLS
              ┌──────▼──────┐                ┌──────▼───────┐
              │   s3api     │                │ adminapi     │  health   console
              └──────┬──────┘                └──────┬───────┘     │        │
   inbound adapters  └──────────────┬───────────────┴─────────────┴────────┘
                                    ▼
                         ┌────────────────────┐
                         │  app (use cases)   │──► ports.Authorizer on every request
                         └─────────┬──────────┘
                                   │ depends on
                         ┌─────────▼──────────┐
                         │ domain (pure model)│
                         └────────────────────┘
                                   ▲ ports (interfaces)
   outbound adapters ┌─────────────┼──────────────┬───────────────┬──────────────┐
                  disk         raftmeta          kms            events        noderpc
              (shards on     (Raft + bbolt)  (Vault Transit  (in-process)   (gRPC, mTLS)
               local drives)                  or sealed file)
```

The events adapter is the in-process bus alone; NATS JetStream and webhook adapters are
milestone 8 and do not exist.

One Go module, `github.com/albievan/cactusfs`. No external database, no proxy in front of
the gateway, no cgo.

## 2. Hexagonal layout and the dependency rule **(implemented)**

| Package | Holds | May import |
|---|---|---|
| `internal/domain/...` | entities, value objects, typed sentinel errors | the standard library only; never `net/http`, `os`, a storage engine or an SDK |
| `internal/ports` | interfaces to the outside world | `domain` |
| `internal/app` | use cases | `domain`, `ports` |
| `internal/adapters/...` | implementations of ports, and inbound transports | `app`, `ports`, `domain`, third-party libraries |
| `cmd/cactusfs` | composition root | everything; the **only** place adapters are constructed and wired |

Imports only point inward. `depguard` in `.golangci.yml` enforces it, so a violation fails
`make lint`: `domain` may import only the standard library (and not `net/http`, `os` or
`database/sql`); `app` only the standard library, `domain` and `ports`; `ports` and
adapters may not import outward.

Conventions that follow from it:

- Constructor injection by hand. No `init()` (linted), no package-level state and no
  `panic` outside `main` (both by review).
- `context.Context` is the first parameter of anything that does I/O and is always
  propagated.
- Errors are wrapped with context (`fmt.Errorf("object.Put: %w", err)`). Domain errors
  are sentinels in `internal/domain/errors.go`; only inbound adapters translate them to
  S3 XML error codes or `application/problem+json`.
- No business or authorization logic in an adapter, a handler or the console.

### Packages that exist today

| Package | Purpose |
|---|---|
| `internal/domain` | typed sentinel errors, and their names on the wire between nodes |
| `internal/domain/bucket` | bucket naming rules, bucket configuration (CORS, tags), lifecycle rules, the record of a bucket's latest use (ADR-0019); sub-buckets and tag rules (ADR-0017, 0018), the usage record, notification and replication documents (ADR-0023 to 0026): rules only, used by no use case yet |
| `internal/domain/object` | IDs, keys, version metadata, ranges, listing rules, conditions, tags, Object Lock, multipart, SSE, the record of who read an object (ADR-0016), the streaming AEAD (ADR-0002) |
| `internal/domain/iam` | principals (a locked console session among them, ADR-0027), actions, the policy language and the decision, bucket policies, users, groups, roles, API keys, built-in roles, sealed secrets |
| `internal/domain/delivery` | targets of delivery and their endpoints, the items of the outbox, classes of failure (ADR-0023, 0025: used by the metadata store, by no use case yet) |
| `internal/domain/audit` | audit records and their hash chain |
| `internal/domain/cluster` | readiness; nodes, roles and labels; drive topology: planning across nodes, slots, reconciliation, replacement; a node's settings and their redaction (ADR-0022) |
| `internal/domain/erasure` | erasure parameters, quorum, set layout, placement, stripe geometry |
| `internal/domain/search` | the language of the search field: text, wildcard, `rx:` regular expression |
| `internal/domain/tagging` | the catalogue of tag definitions (ADR-0018: rules only, used by no use case yet) |
| `internal/ports` | `KMS`, `ShardStore` (`ShardWriter`, `ShardReader`), `ErasureCodec`, `BucketStore`, `BucketPolicies`, `ObjectIndex`, `UploadStore`, `ObjectAccessStore`, `BucketActivityStore`, `RootCredentialStore`, `TopologyStore`, `NodeRegistry`, `ClusterView`, `DriveSource`, `NodeSettingsReader`, `LocalSettings` (a function), `EventPublisher`, `IAMStore`, `AuditLog`, `SessionStore`, `IdempotencyStore`, `PasswordHasher`, `IdentityProvider`, `Authorizer`, `Clock`, `ReadinessCheck`; `SubBucketStore` and `TagDefinitionStore`, and of milestone 8 `BucketUsageStore`, `TargetStore`, `Outbox` and `ReplicationStore`, implemented by the metadata store and called by no use case yet; `DeliveryStatusReader` and `SiteClient`, of milestone 8, implemented by nothing but the fakes of `porttest` yet |
| `internal/ports/porttest` | in-memory fakes and the contract tests every implementation runs |
| `internal/app` | bucket, object, multipart, lifecycle, search, settings and identity use cases; sessions and idempotency; who read what; striped storage, healing, scrubbing, garbage collection, topology; the authorizer; the audit trail; health |
| `internal/buildinfo` | the version, build and commit of the binary (section 8) |
| `internal/adapters/s3api` | in: S3 API (SigV4, XML) |
| `internal/adapters/adminapi` | in: the management API (`api/`: the server generated from the contract) and Swagger UI (`swaggerui/`) (ADR-0007) |
| `internal/adapters/console` | in: serves the embedded console, or the page that says it was not built in |
| `internal/adapters/httpsession` | in: session cookie and CSRF header |
| `internal/adapters/health` | in: `GET /healthz`, `GET /readyz`; the shutdown gate |
| `internal/adapters/telemetry` | metrics, traces, the `/metrics` handler, decorators for every outbound port, interceptors for node RPC, the gauges of the node's state |
| `internal/adapters/disk` | out: shards on a local drive (ADR-0001); the file system behind a drive, per OS |
| `internal/adapters/erasurecodec` | out: Reed-Solomon (ADR-0004) |
| `internal/adapters/fsutil` | durable file creation and directory sync per OS |
| `internal/adapters/raftmeta` | out: Raft + bbolt metadata, one voter or a cluster (ADR-0003, 0005) |
| `internal/adapters/noderpc` | both: node-to-node gRPC (`nodev1/`: generated from `api/proto`); drives of other nodes as `ShardStore` (ADR-0005) |
| `internal/adapters/nodetls` | mutual TLS 1.3 between nodes; certificates read again from disk (ADR-0020); development certificates |
| `internal/adapters/events/inproc` | out: in-process event bus |
| `internal/adapters/kms/local` | out: sealed local key file |
| `internal/adapters/kms/vault` | out: Vault Transit (ADR-0006) |
| `internal/adapters/passwords` | out: Argon2id password hashes |
| `internal/adapters/audit` | out: audit records as JSON lines |
| `api/openapi`, `api/asyncapi`, `api/proto` | the contracts: the management API (and the Go package that embeds it for serving), the events, node RPC |
| `web` | the console (React + TypeScript + Vite), and the Go package that embeds its build with the tag `console` |
| `cmd/cactusfs` | composition root: the settings, node wiring, listeners, shutdown |
| `test/integration`, `test/clustertest` | black-box tests of the built binary and of a four-node cluster of containers; fixtures of older schemas for the upgrade tests |
| `test/s3compat`, `test/chaos`, `test/k8s` | the S3 compatibility suite and its baseline; fault injection; the chart on k3s |

## 3. The binary **(implemented: `version`, `server` standalone and clustered, `config`, `dev-init`)**

```
cactusfs version
cactusfs server [flags]
cactusfs config [--format env|markdown]
cactusfs dev-init --dir <directory> --nodes <id>[,<id>...]
```

`dev-init` writes what the nodes of a development cluster must share: a throw-away node
CA with a certificate per node, a second CA with a certificate for the S3 listener, and
one sealed KMS key file. It never overwrites; a complete directory is left alone.

Configuration is 12-factor: every setting is a `CACTUSFS_*` environment variable with a
matching flag, and the flag wins. The settings are declared once, in `serverSettings()`
of `cmd/cactusfs`: each with its name, variable, default, meaning, group, kind (`plain`,
`path`, `url`, `secret`, `secret-file`) and scope (`node`, or `cluster` for what every
node must have the same). `cactusfs server` reads its command line by that list -
default, then variable, then flag, and where each value came from is kept - and
`cactusfs config --format env|markdown` prints it: `deploy/cactusfs.env.example` and the
table below are that output (`make settings-docs`, part of `make gen`), held equal by a
test. The binary reads no file of variables: what starts it hands it its environment.

<!-- settings:begin -->

**Listeners: where clients reach the node**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--s3-addr` | `CACTUSFS_S3_ADDR` | `:9000` | listen address of the S3 API |
| `--admin-addr` | `CACTUSFS_ADMIN_ADDR` | `:9001` | listen address of the admin listener: probes, `/metrics`, the management API, Swagger UI and the console |
| `--s3-domain` | `CACTUSFS_S3_DOMAIN` | — | domain for virtual-hosted-style requests, \<bucket>.\<domain>. The same on every node |
| `--region` | `CACTUSFS_REGION` | `us-east-1` | region name reported to S3 clients; default us-east-1. The same on every node |
| `--drain-delay` | `CACTUSFS_DRAIN_DELAY` | `0s` | on shutdown, keep serving this long with /readyz answering 503 before closing the listeners |
| `--shutdown-timeout` | `CACTUSFS_SHUTDOWN_TIMEOUT` | `30s` | time allowed for in-flight requests to finish once the listeners have closed |

**Storage: drives and the metadata directory**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--data` | `CACTUSFS_DATA` | required on a node that stores | drive directory; repeat the flag or separate directories with commas |
| `--drive-labels` | `CACTUSFS_DRIVE_LABELS` | — | what the deployment calls the volume behind each drive directory - a compose volume, a claim - as labels separated by commas, one for each --data entry in the same order, or none; shown with the node's settings and used for nothing else |
| `--meta-dir` | `CACTUSFS_META_DIR` | `<first data directory>/meta` | metadata directory; default \<first data directory>/meta |
| `--allow-drive-replacement` | `CACTUSFS_ALLOW_DRIVE_REPLACEMENT` | `false` | confirm that one empty drive replaces one missing drive at this start; permanent: the missing drive is refused afterwards |

**Cluster: the node among its peers**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--node-id` | `CACTUSFS_NODE_ID` | generated on a node on its own | name of this node: lowercase letters, digits and hyphens; required in a cluster, where it must be the name in the node certificate |
| `--roles` | `CACTUSFS_ROLES` | all | roles of this node: gateway, meta, storage, console; default all. For members of a cluster only: a node on its own has every role |
| `--peers` | `CACTUSFS_PEERS` | — | every node of the cluster, this one included, as id=host:port of its node RPC, separated by commas; makes the node a member of a cluster. The same on every node |
| `--rpc-addr` | `CACTUSFS_RPC_ADDR` | `:9002` | listen address of node RPC |
| `--raft-addr` | `CACTUSFS_RAFT_ADDR` | `:9003` | listen address of the Raft transport |
| `--advertise-rpc` | `CACTUSFS_ADVERTISE_RPC` | the node's entry in `--peers` | host:port at which other nodes reach this node's RPC; default its entry in --peers |
| `--advertise-raft` | `CACTUSFS_ADVERTISE_RAFT` | the host of `--advertise-rpc`, the port of `--raft-addr` | host:port at which other nodes reach this node's Raft transport; default the host of --advertise-rpc with the port of --raft-addr |
| `--topology-region` | `CACTUSFS_TOPOLOGY_REGION` | — | region this node is in, for placement |
| `--topology-zone` | `CACTUSFS_TOPOLOGY_ZONE` | — | zone this node is in, for placement |
| `--topology-rack` | `CACTUSFS_TOPOLOGY_RACK` | — | rack this node is in, for placement |
| `--topology-host` | `CACTUSFS_TOPOLOGY_HOST` | — | host this node runs on, for placement |

**TLS: certificates of the listeners and of the node**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--tls-cert` | `CACTUSFS_TLS_CERT` | required, unless `--dev-plaintext` | path of the PEM certificate chain of the S3 and admin listeners |
| `--tls-key` | `CACTUSFS_TLS_KEY` | required, unless `--dev-plaintext` | path of the PEM private key of that certificate. The file holds a secret: its path is not shown with the node's settings |
| `--tls-reload-interval` | `CACTUSFS_TLS_RELOAD_INTERVAL` | `1m` | how often the certificate and key files, the node's too, are read again (at least `5s`; `0` never reads them again). A new pair that is valid is used for new connections without a restart; one that is not is refused and logged, and the old one stays (`cactusfs_tls_reload_pending` in `/metrics` is 1 while it does). The node CA is read at start only: a new CA needs a restart. Rotation is not revocation: a replaced certificate and its key remain accepted by peers until that certificate expires; for a compromised key, rotate the CA or remove the node. The files must be regular files (not pipes) |
| `--dev-plaintext` | `CACTUSFS_DEV_PLAINTEXT` | `false` | DEVELOPMENT ONLY: serve HTTP without TLS, on a loopback IP address only |
| `--node-tls-cert` | `CACTUSFS_NODE_TLS_CERT` | required in a cluster | path of the node certificate for mutual TLS between nodes |
| `--node-tls-key` | `CACTUSFS_NODE_TLS_KEY` | required in a cluster | path of the node certificate's private key. The file holds a secret: its path is not shown with the node's settings |
| `--node-tls-ca` | `CACTUSFS_NODE_TLS_CA` | required in a cluster | path of the CA certificate that issues the cluster's node certificates, and nothing else |

**KMS: the key that wraps data keys**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--kms` | `CACTUSFS_KMS` | required | key management service: `vault` for HashiCorp Vault Transit, `local` for a sealed local key file. There is no default and no unencrypted mode. The same on every node |
| `--kms-key-file` | `CACTUSFS_KMS_KEY_FILE` | `<metadata directory>/kms-local.json`; required in a cluster | path of the local KMS key file; default \<metadata directory>/kms-local.json. The file holds a secret: its path is not shown with the node's settings |
| — (no flag: a secret) | `CACTUSFS_KMS_PASSPHRASE` | required with the local KMS, or `--kms-passphrase-file` | passphrase that opens the local KMS key file. A secret: from the environment only, never shown |
| `--kms-passphrase-file` | `CACTUSFS_KMS_PASSPHRASE_FILE` | — | path of a file holding the local KMS passphrase, instead of $CACTUSFS_KMS_PASSPHRASE. The file holds a secret: its path is not shown with the node's settings |
| `--vault-addr` | `CACTUSFS_VAULT_ADDR` | required with `--kms vault` | URL of Vault, https://host:port. The same on every node |
| `--vault-transit-mount` | `CACTUSFS_VAULT_TRANSIT_MOUNT` | `transit` | path of Vault's Transit engine; default transit. The same on every node |
| `--vault-key` | `CACTUSFS_VAULT_KEY` | required with `--kms vault` | name of the Transit key, of an AEAD type such as aes256-gcm96. The same on every node |
| `--vault-token-file` | `CACTUSFS_VAULT_TOKEN_FILE` | required with `--kms vault` | path of a file holding the Vault token; read at every use, so it can be renewed in place. The file holds a secret: its path is not shown with the node's settings |
| `--vault-ca-cert` | `CACTUSFS_VAULT_CA_CERT` | the system's CAs | path of the CA certificate that issued Vault's certificate; default the system's CAs |
| `--vault-server-name` | `CACTUSFS_VAULT_SERVER_NAME` | — | name expected in Vault's certificate, when Vault is reached under another |
| `--vault-namespace` | `CACTUSFS_VAULT_NAMESPACE` | — | Vault Enterprise namespace. The same on every node |

**Bootstrap: the root credentials of a new deployment**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| — (no flag: a secret) | `CACTUSFS_ROOT_ACCESS_KEY` | generated, and shown once, by a new deployment | access key ID of the root credentials a new deployment begins with; set it together with $CACTUSFS_ROOT_SECRET_KEY. A secret: from the environment only, never shown |
| — (no flag: a secret) | `CACTUSFS_ROOT_SECRET_KEY` | generated, and shown once, by a new deployment | secret key of the root credentials a new deployment begins with; set it together with $CACTUSFS_ROOT_ACCESS_KEY. A secret: from the environment only, never shown |

**Console sessions: how long one lasts, and what becomes of it while it uploads**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--session-lifetime` | `CACTUSFS_SESSION_LIFETIME` | `30m` | how long a console session lasts, from sign-in or from its last unlock, before it reaches its timeout (`10s` to `12h`). Nothing extends it: at the timeout the user signs in again, or - while objects are being uploaded with the session - it is locked until the user's password unlocks it. It must be the same on every node: any gateway may be asked what a session is. The same on every node |
| `--session-upload-grace` | `CACTUSFS_SESSION_UPLOAD_GRACE` | `1m30s` | how long after its last upload a console session that is locked ends (`5s` to `10m`). While a session uploads, each node it uploads through writes one entry to the metadata log in a third of this time. It must be the same on every node. The same on every node |
| `--session-lock-max` | `CACTUSFS_SESSION_LOCK_MAX` | `12h` | the longest a console session that reached its timeout while it uploads stays locked - able to store objects and nothing else - counted from the timeout, whatever is still being uploaded (at most `24h`); `0`: a session never locks, and ends at its timeout. It must be the same on every node: a gateway with a smaller value takes for ended what another takes for locked, and the node that leads the metadata group removes session records by its own. The same on every node |

**Previews: how much of an object the console is sent to show**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--preview-max-size` | `CACTUSFS_PREVIEW_MAX_SIZE` | `32` | the largest object the console is sent to preview, in MiB (`1` to `256`); a larger one is refused (413) before any of it is read, and can be downloaded. Of text, CSV and JSON the first MiB is sent whatever their size. Every preview is a read of up to this much from the drives; a caller has at most 2 running at once and a node 16. A console may ask any gateway. The same on every node |

**Delivery: outbound connections to notification targets and other sites**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--egress-allow` | `CACTUSFS_EGRESS_ALLOW` | none | ranges (CIDRs, separated by commas, such as `127.0.0.1/32`) where a notification target or another site may be at an address that is otherwise never connected to: loopback, link-local, and the IPv6 ranges that stand for IPv4 addresses by a gateway's rule (`::/96`, `64:ff9b:1::/48`, `2002::/16`, `::ffff:0:0:0/96`). Private ranges need no entry. A range of every address (`0.0.0.0/0`, `::/0`) and one that could admit nothing (`0.0.0.0/8`, multicast) are refused. The addresses of the metadata services of clouds (169.254.169.254, `fd00:ec2::254`, `fd20:ce::254`, 100.100.100.200) are never connected to, whatever this holds; a range that is allowed is open with everything else in it, so allow the one address a receiver has. The address a name resolves to is judged at every connect. It has no effect yet: it is read by bucket notifications and replication, which come with PLAN 8B and 8C. The same on every node |
| `--delivery-timeout` | `CACTUSFS_DELIVERY_TIMEOUT` | `30s` | how long a notification target or another site has to answer one request, once it is sent, and to make progress reading or writing (`1s` to `5m`); one that does not is tried again later. Connecting and the TLS handshake have 5 seconds each besides. It has no effect yet: it is read by bucket notifications and replication, which come with PLAN 8B and 8C. The same on every node |
| `--replication-workers` | `CACTUSFS_REPLICATION_WORKERS` | `4` | how many versions are replicated to other sites at once (`1` to `32`). It has no effect yet: it is read by bucket notifications and replication, which come with PLAN 8B and 8C. The same on every node |
| `--replication-rate-mib` | `CACTUSFS_REPLICATION_RATE_MIB` | `0` | limit on the bytes replication sends to other sites, in MiB per second averaged over 10 seconds (at most `1048576`); `0` is no limit. It has no effect yet: it is read by bucket notifications and replication, which come with PLAN 8B and 8C. The same on every node |

**Maintenance: background work**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--scrub-interval` | `CACTUSFS_SCRUB_INTERVAL` | `24h` | pause between two passes of the scrubber, which verifies and heals every object; 0 disables it. The same on every node |
| `--scrub-rate-mib` | `CACTUSFS_SCRUB_RATE_MIB` | `64` | limit on how fast the scrubber reads, in MiB per second; 0 is no limit. The same on every node |
| `--lifecycle-interval` | `CACTUSFS_LIFECYCLE_INTERVAL` | `1h` | pause between two passes of the worker that carries out the lifecycle rules of buckets; 0 disables it. The same on every node |
| `--gc-interval` | `CACTUSFS_GC_INTERVAL` | `1h` | pause between two passes of the garbage collector for unreferenced shards; 0 disables it |
| `--gc-grace` | `CACTUSFS_GC_GRACE` | `24h` | minimum age of an unreferenced shard before it is removed (at least `1h`). The same on every node |
| `--access-flush-interval` | `CACTUSFS_ACCESS_FLUSH_INTERVAL` | `30s` | how often the downloads this node counted are recorded with the objects, at least 5s; a node that crashes loses up to this much of the count |

**Observability: log, audit export and traces**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--log-level` | `CACTUSFS_LOG_LEVEL` | `info` | minimum log level: debug, info, warn or error |
| `--audit-export` | `CACTUSFS_AUDIT_EXPORT` | `false` | write every audit record to standard output as a JSON line with "stream":"audit" |
| `--otlp-endpoint` | `CACTUSFS_OTLP_ENDPOINT` | — | URL of an OTLP/HTTP trace collector, `https` unless the host is a loopback IP address; traces are not exported when empty. A URL with userinfo, a query or a fragment is refused: a credential does not belong in it, and a credential for the collector is not a setting of CactusFS |

<!-- settings:end -->

Secrets have no flag, because a command line is visible to every user of the machine:
`CACTUSFS_KMS_PASSPHRASE` (or the passphrase file), and optionally
`CACTUSFS_ROOT_ACCESS_KEY` with `CACTUSFS_ROOT_SECRET_KEY`.

**The settings of a running deployment** are read live, never stored: nothing of them is
replicated and no schema knows them. `GET /api/v1/settings` (`admin:GetSettings`; the
console's Settings screen) is answered by `app.SettingsService` on the node that was
asked: its own settings come from `ports.LocalSettings`, those of every other recorded
node are asked of that node over node RPC (`NodeService.Settings`, through
`ports.NodeSettingsReader`), all at once and none waited for longer than 4 s (a second more than a connection is waited for, so that a node that cannot be reached is not called slow). A node
that is down, too slow or of a release without the call is in the answer as
`unreachable`, `timeout` or `unsupported`, and costs nobody else's time. Settings of
scope `cluster` in which the nodes that answered differ are listed as mismatches, with
the name of the KMS key and the version beside them.

What a node says of itself is put together in the composition root
(`cmd/cactusfs/localsettings.go`): every setting of the list above with its value and
where it came from (`default`, `environment`, `flag`, or `derived` for what the node
worked out, such as the metadata directory inside the first drive); its roles, labels,
build and start time; the name of its KMS key; each drive it opened with its directory,
the label it was given (`--drive-labels`: the compose volume, the claim - a process
cannot know that name, so it is shown as it was told), the file system behind it (Linux)
and its room; the metadata volume; and the certificates in use at that moment, by their
names and dates. **Redaction happens there, in the node that owns the values**
(`cluster.NewSetting`, by the declared kind): a secret and the path of a file that holds
one never leave the process - not to a peer, not to the gateway, not to the log - and
each wire redacts once more what it carries. The volumes are asked with one deadline of
two seconds, one question out to each at a time: whoever asks while a question is out
waits for that answer against a deadline of their own, so two callers at the same
moment are told the same, and a mount that hangs costs each caller its two seconds and
the node one goroutine - it is shown as not reached, without its room, to those whose
time ran out, and is not asked a second time.

Behaviour:

- **TLS or nothing.** Without a certificate and key the server refuses to start. With
  `--dev-plaintext` it serves HTTP, but only on `127.0.0.1` or `::1`; a wildcard, a
  routable address or a host name is rejected. TLS minimum is 1.2, restricted to ECDHE
  suites with AES-GCM or ChaCha20-Poly1305; 1.3 is negotiated when the client offers it.
  Certificates are replaced without a restart (ADR-0020): every `--tls-reload-interval`
  the certificate and key files - of the listeners and of the node - are read again, and
  a pair whose content changed is put to use for new handshakes if it parses, matches,
  is valid at the time and, for the node, names this node and verifies under the node
  CA; anything else is refused and logged and the pair in use stays. Open connections
  are not touched, so Raft keeps its leader. The node CA is read at start only: trust
  roots change with a restart. The files are regular files of at most 1 MiB; the node's
  pair is read within its directory, the listeners' pair through links wherever they
  lead. A read that hangs holds up neither handshakes nor shutdown. The listeners'
  pair is held to the same checks at start, where a failure is logged at `ERROR` and
  the node starts all the same. A refusal that stands is logged again every hour, and
  is `cactusfs_tls_reload_pending` = 1 for as long as it stands.
- **Probes.** `GET /healthz` answers 200 while the process can serve HTTP. `GET /readyz`
  answers 200 when every `ports.ReadinessCheck` passes and 503 otherwise. The probes are
  unauthenticated, so a response is only `{"status":"ok"}` or `{"status":"unavailable"}`;
  the failing check and its error go to the log.
- **Start-up.** The listeners open first, before the node has opened its stores or
  joined its cluster - a member may wait a long time for the others. Until the node
  has started, `/healthz` answers 200 and `/readyz` 503, and everything else is
  refused without being read or authenticated: 503 `ServiceUnavailable` in XML on the
  S3 listener, 503 problem+json on the admin listener (management API, Swagger UI,
  console and `/metrics` alike), each with `Retry-After`. The log says `listener open;
  the node is starting` then, and `listening` only once the node itself is served. A
  node told to stop before it has started stops at once, with no drain delay.
- **Shutdown.** On SIGTERM or interrupt the readiness gate closes, so `/readyz` answers
  503. The node keeps serving for the drain delay, then closes its listener, gives
  in-flight requests the shutdown timeout to finish, and exits 0. Requests that outlive
  the timeout are cut off and the exit code is 1. With the default drain delay of zero
  the listener closes immediately, so the 503 is only observable when a delay is set.
- **Root credentials.** There is no built-in API key (for the one built-in console
  user see ADR-0014). A new deployment stores the root
  credentials supplied in the environment, or generates a pair and prints it once on
  standard error — never in the log. The secret is stored sealed through the KMS. On
  later starts, supplied credentials that differ from the stored ones stop the node.
- **KMS.** The local key file is created on the first start of a new deployment. On an
  existing deployment a missing key file, a wrong passphrase or another deployment's key
  stops the node: nothing is ever stored or served without the right key.
- **Metrics.** `GET /metrics` on the admin listener requires
  `Authorization: Bearer <access key ID>:<secret>` and passes the authorizer
  (`admin:GetMetrics`). Metrics carry no bucket, key or access-key labels.
- **Observability pack** (`deploy/observability/`). Prometheus alerting and recording
  rules with their `promtool` unit tests (`rules/`), and three Grafana dashboards
  (`dashboards/`: overview, the two APIs, storage and background work) that use only
  panels of Grafana itself and fetch nothing. The Helm chart can hand both to the
  Prometheus Operator - a PodMonitor that scrapes `/metrics` of every pod over TLS with a
  credential from a Secret, and a PrometheusRule with the same rules - and renders
  neither unless told to. The rules expect the scrape job to be named `cactusfs`.
  The alerts cover the APIs, the KMS and the shard stores, healing, the metadata group
  (no leader, elections, a node behind, an unfinished upgrade of the schema), drives
  (offline, nearly full), node certificates, a certificate on disk that was refused,
  refused handshakes, calls between nodes,
  readiness and the audit trail.
  `make observability-check` runs `promtool`, checks the dashboards and the chart's two
  templates, and runs a test of `internal/adapters/telemetry` that fails when a rule, a
  rule test or a dashboard names a metric the registry does not have. The chart's
  PodMonitor scrapes every node, whatever its roles: the gauges of the metadata group
  and of the drives are those of nodes that need not be gateways.
- **Traces.** Spans are recorded only when `--otlp-endpoint` is set; without it they
  are made against a provider that drops them and nothing leaves the process. An S3 or
  management request is a server span; every call across an outbound port is a child
  of it (`<port>.<Method>`); every call between nodes is a span on each side
  (`cactusfs.noderpc/<Method>`), and the caller's trace travels in the gRPC metadata
  of the call as a W3C `traceparent`, with the `tracestate` beside it when there is
  one, so that a PUT through a gateway and the storage nodes it writes to is one trace.
  No baggage is read or sent, and the protobuf schema carries nothing of it. A node
  that does not trace neither reads nor passes on either of the two. No span has an
  attribute, and no metric a label, that holds a bucket, a key, a user, an access key,
  a node's name, the directory of a drive or the text of an error.
  The endpoint holds no credential: a URL with userinfo, a query or a fragment is
  refused at start (the exporter would send neither - it takes the host and the path of
  the URL and nothing else), and every refusal names the endpoint as
  `cluster.RedactURL` leaves it. CactusFS has no setting that gives the collector a
  credential. What the exporter cannot send it reports to OpenTelemetry's error
  handler, of which a process has one: `telemetry.New` sets it when it creates an
  exporter (`telemetry.ExportErrors`), because the library otherwise prints each error
  to standard error with the URL it was sent to. The node logs `traces could not be
  exported` at WARN with the class of the error (`timeout`, `canceled`, `certificate`,
  `dns`, `connection`, `request`, `other`), the name of its Go type and how many were
  left out - never its text - and at most once a minute.
  Not a setting, and so neither listed nor shown with the settings: the OpenTelemetry
  exporter reads its own `OTEL_EXPORTER_OTLP_*` variables from the node's environment
  (headers, a client certificate, a timeout among them); the endpoint and whether TLS
  is used are always those of `--otlp-endpoint`. Nothing in CactusFS sets, tests or
  redacts them, and the library reports one it cannot read through its own log, on
  standard error, with the value.
- **Ports.** Every outbound port a use case is given is a decorator of
  `internal/adapters/telemetry` (`InstrumentX`): one span and one count per call, the
  `method` label the name of the Go method, the `result` `error` whenever the port
  returns an error - also when the error is the port's way of saying that a bucket, an
  object, a user or a session does not exist, or that a password was not accepted. A
  test of `cmd/cactusfs` walks a started node by reflection and fails on any port that
  is held without its decorator; the exceptions (the clock, the authorizer, the erasure
  codec, the composition of drives, shard readers, readiness checks) are listed in
  that test with their reasons.
- **State.** The gauges of the metadata group, the drives, the certificates and
  readiness are read when the metrics are scraped, from accessors that ask no other
  node: what Raft keeps in memory, short reads of the node's own state, one `fstatfs` a
  drive through the handle the drive was opened with. Every source is asked through a
  probe: a scrape waits two seconds for all of them together and goes on without those
  that have not answered - their series are absent from that scrape, not zero, except
  `cactusfs_drive_online`, which is 0 for every drive the last answer named - and a
  source that hangs (the metadata state while a snapshot is restored, a drive) is asked
  once, not once a scrape. A gauge whose source does not exist (a node that is starting,
  a node without a node certificate) is absent as well. Two counters are read the same
  way from counts that are kept where the thing happens, and never go back: the
  refusals of certificate files (`cactusfs_tls_reload_refused_total`, every reason at 0
  from the first scrape of a certificate) and the handshakes `nodetls` does itself, on
  the Raft port and on the connections Raft dials, which a scrape adds to
  `cactusfs_node_handshakes_refused_total` beside those of node RPC, which gRPC does and
  reports one by one. A handshake reaches that counter by one of the two ways only. Its
  reasons are the same four whoever did the handshake: no certificate, another CA or
  another node's name is `certificate`; a node the Raft port does not admit is
  `not_admitted`.
- **Logs** are JSON on stdout (`log/slog`). Secrets, keys, tokens and object contents are
  never logged.

The metrics of the binary. A test of `internal/adapters/telemetry` holds this table and
the registry to each other, in both directions and with types and labels; the process
metrics (`cactusfs_process_*`) and those of the Go runtime (`go_*`) come from the
Prometheus client and are not listed.

<!-- metrics:begin -->
| Metric | Type | Labels | What it says |
|---|---|---|---|
| `cactusfs_build_info` | gauge | `version`, `goversion` | always 1; which build this is |
| `cactusfs_ready` | gauge | - | 1 when `/readyz` answers 200, else 0 |
| `cactusfs_s3_requests_total` | counter | `operation`, `code` | S3 requests by operation and HTTP status |
| `cactusfs_s3_request_duration_seconds` | histogram | `operation` | duration of S3 requests |
| `cactusfs_s3_request_bytes_total` | counter | `operation`, `direction` | bytes received from (`in`) and sent to (`out`) S3 clients |
| `cactusfs_admin_requests_total` | counter | `operation`, `code` | management API requests by route and HTTP status |
| `cactusfs_admin_request_duration_seconds` | histogram | `operation` | duration of management API requests |
| `cactusfs_port_calls_total` | counter | `port`, `method`, `result` | calls across an outbound port; `result` is `ok` or `error` |
| `cactusfs_port_call_duration_seconds` | histogram | `port`, `method` | duration of calls across an outbound port |
| `cactusfs_noderpc_requests_total` | counter | `side`, `method`, `code` | calls between nodes: `side` is `client` (made) or `server` (answered), `method` one of node RPC's or `other`, `code` the gRPC status |
| `cactusfs_noderpc_request_duration_seconds` | histogram | `side`, `method` | duration of calls between nodes; of a stream, until it ends |
| `cactusfs_node_handshakes_refused_total` | counter | `reason` | handshakes with other nodes this node refused, on node RPC and on Raft, each counted once: `certificate`, `not_admitted`, `timeout`, `other` |
| `cactusfs_node_certificate_not_after_seconds` | gauge | - | when the node certificate in use ends, in seconds since 1970; after a replacement, the new one's end; clusters only |
| `cactusfs_tls_certificate_not_after_seconds` | gauge | `use` | when a certificate in use ends, in seconds since 1970: `use` is `node` (shown to the other nodes) or `s3` (shown to clients of the S3 API, the management API and the console) |
| `cactusfs_tls_reload_pending` | gauge | `use` | 1 while the certificate files hold something that was refused and the certificate from before is still shown, else 0 |
| `cactusfs_tls_reload_refused_total` | counter | `use`, `reason` | contents of the certificate files that were refused, each counted once: `unreadable`, `pair`, `not_yet_valid`, `expired`, `identity`, `untrusted`, `usage` |
| `cactusfs_raft_is_leader` | gauge | - | 1 on the node that leads the metadata group |
| `cactusfs_raft_term` | gauge | - | the Raft term; grows with every election |
| `cactusfs_raft_applied_index` | gauge | - | last metadata entry the node's state reflects |
| `cactusfs_raft_commit_index` | gauge | - | last metadata entry the node knows to be committed |
| `cactusfs_raft_peers` | gauge | - | voters of the metadata group, as the node knows it |
| `cactusfs_metadata_schema_level` | gauge | - | newest entry schema the cluster writes now |
| `cactusfs_metadata_schema_supported` | gauge | - | newest entry schema this binary applies |
| `cactusfs_metadata_state_schema` | gauge | - | schema of the node's metadata state |
| `cactusfs_drive_online` | gauge | `drive` | 1 when a drive of this node can be reached; `drive` is its generated ID |
| `cactusfs_drive_free_bytes` | gauge | `drive` | bytes the node may still write to the drive's file system |
| `cactusfs_drive_total_bytes` | gauge | `drive` | size of the drive's file system |
| `cactusfs_heal_objects_total` | counter | `result` | objects checked by healing: `healthy`, `healed`, `unrecoverable` |
| `cactusfs_heal_shards_rebuilt_total` | counter | - | shards rebuilt from the rest of their object |
| `cactusfs_scrub_passes_total` | counter | - | finished passes of the scrubber |
| `cactusfs_scrub_objects_total` | counter | - | objects the scrubber verified |
| `cactusfs_scrub_bytes_total` | counter | - | stored bytes the scrubber verified |
| `cactusfs_gc_passes_total` | counter | - | finished passes of the garbage collector |
| `cactusfs_gc_shards_removed_total` | counter | - | unreferenced shards removed |
| `cactusfs_lifecycle_actions_total` | counter | `action` | what lifecycle rules did, by kind |
| `cactusfs_object_access_dropped_total` | counter | - | reads not counted towards who read what |
| `cactusfs_events_total` | counter | `type` | events derived from applied metadata entries |
<!-- metrics:end -->

The values of `port`: `kms`, `shards`, `metadata`, `local_config`, `object_access`,
`bucket_activity`, `iam`, `sessions`, `idempotency`, `audit`, `events`,
`identity_provider`, `passwords`, `cluster_view`, `node_settings`, `targets`.

Default ports, all configurable: S3 `9000`; admin API, Swagger UI and console `9001`;
node RPC `9002`; Raft `9003`.

### Roles and run modes **(implemented, ADR-0005)**

One binary, four roles selected with `--roles` (default: all): `gateway` (S3 API,
authentication, erasure encode/decode; holds nothing that cannot be rebuilt), `meta`
(Raft voter), `storage` (drives; collects garbage on them) and `console` (the UI, on
the admin port). Every node answers probes and serves metrics; only gateways open the S3 listener.

- **Standalone** — `cactusfs server --data <dir>...` runs every role in one process with
  a single-node Raft group. With four or more drives it erasure-codes across them; with
  fewer it still encrypts and checksums and logs that it has no parity.
- **Cluster** — the same binary on four or more storage nodes with three or five `meta`
  voters, started with `--peers` and node certificates. The nodes form the cluster
  themselves when all of them are up for the first time: the voters bootstrap the
  metadata group, the leader records the drive topology across all nodes. After that a
  node needs only a majority of voters to start and serve.

Standalone is a cluster of one, not a second implementation: the same store, the same
read and write paths, with a transport that has nobody to talk to.

What a cluster does when things fail:

| Situation | Behaviour |
|---|---|
| Nodes down, up to (parity ÷ drives per node per set) | reads and writes continue; what the nodes missed is rebuilt on them by the scrubber |
| More nodes down, read quorum of shards reachable | reads continue; writes answer 503 |
| Fewer shards reachable than needed | reads answer 503; nothing is reported lost, nothing is rewritten |
| Metadata leader lost | the voters elect another; requests wait up to 10 s, then 503 |
| A node cut off from the majority | it answers nothing — no read from what it knew, no write |
| No majority of voters | no reads, no writes, until it returns |

## 4. Storage engine **(implemented, ADR-0001, 0002, 0004, 0005)**

- **Erasure coding.** The encrypted stream of an object is cut into 1 MiB stripes; each
  stripe is split into data chunks and extended with Reed-Solomon parity chunks, and
  chunk *i* of every stripe goes to shard *i*. Erasure sets have 4–16 drives. Default
  parity is 2 for 4–5 drives, 3 for 6–7 and 4 for 8 or more. Shards of one object never
  share a drive. More than 16 drives form several equal sets. With fewer than four
  drives an object is one shard on one drive, encrypted and checksummed but **without
  parity**, and the node says so at start. In a cluster the shards of a set are spread
  across nodes, then racks and zones where they are labelled (implemented, ADR-0005).
  Parity per storage class is not implemented, and no step of `PLAN.md` holds it yet.
- **Quorum.** Write quorum is the number of data shards, plus one when data equals
  parity. Read quorum is the number of data shards.
- **Write path.** Encrypt → erasure-code → write shards under a new immutable ID and
  fsync on a write quorum → commit the object version through Raft → acknowledge. The
  Raft commit is the only visibility switch. A commit whose outcome is unknown keeps its
  shards; shards that end up unreferenced are garbage-collected after a grace period.
- **Consistency.** Reads and listings are strongly consistent (read-after-write).
- **Integrity.** Every shard block carries a SHA-256 checksum, bound to its position,
  that is verified on every read, and every segment is authenticated by AES-GCM. A read
  that meets a missing or damaged shard rebuilds the stripe from parity and queues the
  object for healing. Bytes that fail verification are never returned.
- **Self-healing.** Healing rewrites missing, truncated and corrupt shards from the
  others, and is built never to make an object worse: only definite damage is rebuilt,
  and a rebuilt shard replaces the old one in one step. A scrubber verifies every block
  of every object in the background and heals what it finds; it is also what refills a
  replaced drive. A collector removes shards no object refers to, never those of a write
  in progress and never while the metadata is not ready.
- **Topology.** Which drive is in which slot of which erasure set is recorded in Raft
  metadata at the first start and addressed by drive ID, so the order of `--data` does
  not matter. Every drive is claimed for the deployment's ID, so drives and metadata
  that do not belong together are refused: a node started against empty metadata with
  drives full of data does not start. One empty drive may take the place of one missing
  drive when the set has parity and the operator confirms it
  (`--allow-drive-replacement`); anything else stops the node. All drives must open for
  a node to start.
- **Keys are not paths.** Object keys and bucket names never become filesystem paths.
  Data lives under generated IDs, so case-insensitive filesystems, Windows reserved
  names, path length limits and `../` cannot affect it.
- **Portability.** OS differences (directory fsync, file locking, service control) live
  in `adapters/disk`, `adapters/fsutil` and `cmd/cactusfs` behind build tags.

## 5. Metadata **(implemented, ADR-0003, 0005)**

Bucket configuration, the root credential, the record of nodes, the drive topology and
the object index are replicated with Raft and stored in bbolt. (So are users, groups, roles, policies,
API keys, console sessions and the audit trail: ADR-0006.) Every node holds a replica; nodes with the
`meta` role vote. Changes go through the leader; reads are linearizable on every node,
by read index. The object index sits behind `ports.ObjectIndex` so it can move to
sharded Raft groups later; that move needs an ADR. On-disk formats and Raft entries
carry a version; no node writes an entry format newer than the oldest member of the
cluster understands, so version N runs alongside N+1 during a rolling upgrade.

The entry schema is at 5:

| Schema | Adds | ADR |
|---|---|---|
| 1 | buckets, the object index, the root credential, nodes and topology, identities, sessions, the audit trail | 0003, 0005, 0006 |
| 2 | the leader's time on every entry; object versions, delete markers, Object Lock, bucket configuration, multipart uploads | 0008 (with 0009, 0011, 0012) |
| 3 | who committed a version and the counts of reads; the commands of sub-buckets, tag rules and tag definitions, which the state machine applies and no use case sends yet | 0016 (with 0017, 0018) |
| 4 | who created a bucket; the record of a bucket's latest uploads and downloads | 0019 |
| 5 | what every bucket holds, counted by every member, and its quota; targets, the outbox of what is to be delivered and its cursors; replica buckets and the record of a resync - which the state machine applies and the store proposes, and no use case sends yet | 0023 (with 0024, 0025, 0026) |

Upgrades from schema 2, 3 and 4 are tested against fixtures written by the binaries of
those schemas (`test/integration/testdata`), and rolling upgrades from 3 to 4 and from
4 to 5 with the binaries themselves (`make test-rolling`, which takes the release to
start from and the test of that upgrade: `ROLLING_FROM_COMMIT`, `ROLLING_TEST`). After
the last member is upgraded, the first entry of the new schema ends the way back to the
release before (ADR-0016, ADR-0019, ADR-0023).

Schema 5 (ADR-0023) adds seven commands - `bucket.quota.put`, `bucket.usage.walk`,
`target.put`, `target.delete`, `outbox.ack`, `bucket.replica.put`,
`replication.resync.put` - two kinds of bucket configuration (`notification`,
`replication`), and `replica`, `replica_from` and `by` on a put, a completion, an upload
and a delete. In the state, each made when it is first written:

| Bucket of bbolt | Key | Holds |
|---|---|---|
| `bucketusage` | bucket name | the usage record (`bucket.Usage`): objects, current bytes, versions, stored bytes, the bytes of parts of uploads in progress, the versions pending and failed in replication, the greatest sequence, and where a count stands |
| `targets` | target name | a target: its kind, the origin of its endpoint, and - sealed through the KMS - its path, its secret and its signing key |
| `targetrefs/<target>` | bucket name | which kinds of document of that bucket name the target: `n`, `r` or `nr` |
| `outbox` | index of the entry and number of the row | a row: what changed, of which version, for which consumers |
| `outboxcursor` | target name | how far delivery to the target has come, what it lost, whether it needs a resync |
| `resync` | bucket name | how far the resync of a bucket has come |
| `meta` | `outboxcount`, `outboxdropped` | the rows the outbox holds, and those pushed out at its bound |

and on the records that were there: `quota_bytes` and `replica_from` of a bucket,
`replication` of a version, `replica_from` of an upload - all left out when empty, so a
record without them is the record of schema 4.

A usage record is made by an entry of schema 5 and by no other - exact and empty by the
`bucket.create` of a new bucket, which is written as schema 5 once every member knows
it, or in counting state by the first `bucket.usage.walk` for a bucket that was there -
and from then on **every** entry that changes a key or a part keeps it, whatever its
schema byte (unlike the record of a bucket's latest use, which goes by the byte): a
counter that missed a put written a moment late would be wrong for good. The walk counts
a bucket online, a bounded number of records an entry, with its cursor in the record. A
put or a part that would take a bucket past its quota is refused where it is applied.
The store implements `ports.BucketUsageStore`, `ports.TargetStore`, `ports.Outbox` and
`ports.ReplicationStore`; below level 5 every change of theirs is "unavailable", with
nothing logged, and there is nothing to read.

**What makes a state schema 5 today is one thing: a bucket that is created** at level 5.
Nothing else in this build proposes an entry of schema 5 - the worker that counts
buckets without being asked, the APIs of quotas, targets, notifications and replication
are of later steps of milestone 8 and answer `501` - so a deployment upgraded to this
build keeps its way back to the release of schema 4 until its first new bucket, and the
buckets it had are not counted yet. That way back is walked with **every node stopped
first** and then all started on the release of schema 4, never a node at a time: a node
taken back has a record that says 5 until it registers again, the leader goes on
writing schema 5 meanwhile, and one bucket created in that time stops the node taken
back at that entry - fail-safe, its state untouched, and cured by starting it on this
release again - and ends the way back for the cluster (ADR-0023, "Rollback";
`TestARollingRollbackFromLevel5EndsAtTheFirstBucket`). A bucket created through a node
that had not yet seen level 5 is written below schema 5 and has no usage record until
the counter of PLAN 8.3.1 walks it. The bytes of schema 5 are frozen: the golden files
`schema5-entries.golden.txt` and `schema5-records.golden.txt`
(`internal/adapters/raftmeta/testdata`) may not change without a schema 6.

Schema 4 adds who created a
bucket (`created_by` on `bucket.create`) and no command. It is also the schema a put, a
completion and a batch of reads are written in once every member knows it: applying
such an entry, every member keeps `bucketactivity/<bucket>` - the latest three uploads
and three downloads of the bucket, with who and when (`bucket.Activity`,
`ports.BucketActivityStore`) - so that the record follows from the log alone. Below
level 4 a bucket is created without its creator and no record is kept; nothing is
refused. A state says schema 4 from its first such entry - in practice the first
upload after the last member was upgraded - and the release before then refuses it.

Both ways of making a bucket - the management API and S3 `CreateBucket` - go through
`BucketService.Create` or `CreateWithObjectLock`, which record the actor as the audit
trail names one. The creator and the record are read out in one place,
`BucketService.ListDetailed` (`GET /api/v1/buckets?details=true`, the console's grid of
buckets). `s3:ListAllMyBuckets` gates the list, through `Authorize`, as ever. What a
bucket of the list carries is decided with `Authorizer.Allowed`, which writes no
denial, by the request the use case for one bucket is gated by: `admin:GetObjectAccess`
on the bucket (`accessRequest`) for the creator and for whether any activity is shown,
`s3:ListBucket` (`listRequest`) for the object keys in the activity, `s3:GetBucketPolicy`
(`bucketRequest`) for the summary of the policy - `iam.BucketPolicy.Summary`, the
number of statements and whether one allows something to every caller. A row of the
activity is of an object, so each row that was read is decided once more, for its
object: `accessRequest` with the row's key, the request `ObjectService.Access` gates
by and `ListWithAccess` filters by, asked before the key is blanked. A row that is not
allowed is left out whole - who, when and key - so a deny by key holds in the list as
it holds for `/object/access`; the action on the bucket stays required besides (the
stricter reading: held on objects only, it shows nothing here). These are at most six
decisions a shown bucket, in memory: no bucket policy binds an `admin:` action. A
part is left out, bucket by bucket (`access_included`, `policy_included`), never
refused. The cost does not grow by a question to the leader for each bucket: after the
one consistent list the policies of all buckets are walked once in this node's state,
page by page, as a search walks them - the authorizer decides from the pages, and the
summary of a policy is of the policy the authorizer parsed, or remembers under the
SHA-256 of the document that walk has just read (`Authorizer.walkedSummary`): no
document is parsed a second time for its summary, and for root, whom no bucket policy
binds, the same walk is made for the summaries - and the records are read in
one call, `BucketActivities`, at most 10 000 buckets each. A bucket whose stored policy
is damaged is shown without keys and without a summary; when the policies or the
records cannot be read, the list fails. How much a bucket holds stays a question of
its own (`GET /api/v1/buckets/{bucket}/usage`), which the console asks row by row,
four at a time.

## 6. APIs **(implemented: the S3 API with the depth of milestone 6, the management API, node RPC and events; NATS and webhook delivery is milestone 8)**

- **S3 data API.** Defined by AWS's S3 API reference: path-style and virtual-hosted-style
  addressing; SigV4 header, presigned and streaming signatures; XML errors. Its contract
  is the compatibility suite in `test/s3compat`, guarded by a recorded baseline.
  **Implemented:** ListBuckets, CreateBucket, HeadBucket, DeleteBucket,
  GetBucketLocation, PutObject, GetObject (single range), HeadObject, DeleteObject,
  DeleteObjects, PutBucketVersioning, GetBucketVersioning, ListObjectVersions, and
  `versionId` on GetObject, HeadObject and DeleteObject (ADR-0008), Object Lock
  (buckets created with it, default retention, retention and legal hold of versions,
  governance bypass; ADR-0009), multipart uploads (CreateMultipartUpload, UploadPart,
  CompleteMultipartUpload, AbortMultipartUpload, ListParts, ListMultipartUploads;
  ADR-0012, with a key per part and a sealed part list, ADR-0010), SSE-S3, SSE-KMS with
  an encryption context and SSE-C (ADR-0010), bucket policies with anonymous read access,
  CORS rules with unauthenticated preflight, tags of buckets and objects (ADR-0011),
  lifecycle rules carried out by the node that leads (ADR-0013), CopyObject and
  UploadPartCopy (the source is read and the destination written as any reader and
  writer would, under keys of its own), ListObjects version 1, and the conditional
  headers on GET, HEAD and the source of a copy,
  ListObjectsV2; SigV4 in the Authorization header and in the query string (presigned
  URLs, valid for at most seven days); signed aws-chunked uploads; literal, `UNSIGNED-PAYLOAD` and
  `STREAMING-UNSIGNED-PAYLOAD-TRAILER` payloads with CRC32, CRC32C, CRC64NVME and SHA-256
  checksums and `Content-MD5`. Everything else answers `501 NotImplemented` after
  authentication. The suite runs the AWS SDK for Go with its default options, the AWS
  CLI, and a selected list of the S3 tests of Ceph, over TLS against the built binary
  on four drives. The two tools are in the builder image, pinned by checksum.
- **Admin API (implemented, ADR-0007).** REST/JSON under `/api/v1` on the admin listener
  of gateway nodes, designed first in `api/openapi/admin.yaml` (OpenAPI 3.0.3), with the
  strict server generated by `oapi-codegen`. An API key as a bearer credential, or a
  console session with a CSRF token. Errors are `application/problem+json`; every
  mutation accepts `Idempotency-Key`, whose records are replicated and whose stored
  answers are sealed through the KMS. Paths, under `/api/v1`: `/session` and
  `/session/unlock`; `/buckets`,
  `/buckets/{bucket}` and `/buckets/{bucket}/usage` (with, when asked, who created each
  bucket, what its policy is and who used it last); `/buckets/{bucket}/objects` (list),
  `/buckets/{bucket}/object` (upload, download, delete), `/buckets/{bucket}/object/metadata`
  and `/buckets/{bucket}/object/access` (who uploaded and who downloaded); `/search`;
  `/users`, `/users/{name}`, `/users/{name}/keys` and `/keys/{access_key_id}`; `/groups`,
  `/roles` and `/policies`, each with `/{name}`; `/cluster` and `/cluster/maintenance`;
  `/settings`; `/audit`. The paths of milestone 8 are in the contract and have no use
  case yet, so each answers 501 `NotImplemented` to a caller that has authenticated:
  `/buckets/{bucket}/quota`, `/buckets/{bucket}/usage/recount`,
  `/buckets/{bucket}/notifications`, `/buckets/{bucket}/replication` (with `/status`
  and `/resync`), `/buckets/{bucket}/replica`, `/targets`, `/targets/{name}` and
  `/targets/{name}/status`. There is no path yet for a bucket's policy, for sub-buckets or
  for tags: bucket policies, CORS, tags and lifecycle rules are set through the S3 API.
  The contract is served at `/api/v1/openapi.json` and
  `.yaml`, and Swagger UI at `/docs` from embedded assets under a Content-Security-Policy
  that allows nothing from another host.
- **Search (implemented; sub-buckets to come).** `GET /api/v1/search?q=` and the field
  at the top of the console's navigation find buckets by name and objects by key. The
  query language is `internal/domain/search`: plain text is a case-insensitive
  substring, `*` and `?` make a wildcard over the whole name, `rx:` begins an RE2
  regular expression taken as written; every form compiles to one RE2 program, so
  matching is linear in the name. `app.SearchService` walks the buckets in name order
  and, in each, the index of current versions in key order. **Scope:** a bucket's name
  is a result only for a caller allowed `s3:ListAllMyBuckets`, and a bucket's keys are
  searched only for a caller allowed `s3:ListBucket` on it, the bucket's policy
  included - asked of the one authorizer through `ports.Authorizer.Allowed`, which is
  the decision of `Authorize` without its audit record and exists for filtering result
  sets only, never as the gate of an operation. A caller who may list neither the
  buckets nor any bucket, or who names a bucket they may not list, is refused through
  `Authorize`, once, audited. A search is a read and leaves no audit record; the query
  is never logged. **Bounds of one request:** 50 000 keys examined, 200 buckets, two
  seconds on the node's clock, `limit` results (100, at most 500), and the index is
  walked 5 000 keys at a time so that the metadata store's read lock - which an entry
  waiting to be applied waits for - is let go between walks; matching happens after
  each walk, outside the lock. One search at a time for a principal and eight for a
  node (with two, two principals searching without pause shut everybody else out);
  beyond that 429 with `Retry-After: 1`. **Deciding the buckets:** every page first
  decides every bucket there is, before it looks at where its token points
  (`searchWalk.visible`): one linearizable listing of the buckets, then - for a caller
  a bucket policy binds; root causes none - a walk of all bucket policies in this
  node's own state through `ports.BucketPolicies`
  (`BucketPolicies(ctx, from, visit) (next, err)`, implemented by
  `raftmeta.LocalConfig`), and one decision of the authorizer for each bucket. The port
  pages: one call is one read transaction over at most 256 buckets or 1 MiB of stored
  records, the read lock of the state is let go before the visitor is called and
  between pages (1.5 ms a page of 17 KiB policies, 0.9 ms a page of 256 small ones,
  Apple M3), and a document is lent to the visitor for the call only. Every page is
  the node's state at least as new as the listing before it; a policy changed during
  the walk is seen as it was or as it is - what two requests one after the other get.
  The search tells the request scope where the policies are (`primeBucketPolicies`,
  inside `internal/app`); the scope keeps one page of the walk and the search one bit a
  bucket, never the policies of all buckets. Who is bound by a policy, how it is read
  and what follows stay in `Authorizer.decide`. **Parsed policies are remembered** in
  the `Authorizer` value the composition root builds, from one search to the next: by
  bucket name, under the SHA-256 of the document, asked only with the hash of the bytes
  just read - so nothing is ever decided from what a policy used to say, a page costs a
  hash for each policy and a parse only for those that changed, and a document that
  does not parse is never remembered and hides its bucket every time. The memory is
  bounded at 131 072 policies and 128 MiB of documents (about 200 MiB of heap); what
  finds no room is parsed every time, and what a whole walk did not come by - a deleted
  bucket, a removed policy - is dropped when that walk ends. Only a search uses it: any
  other request reads the policy of its bucket in one linearizable read and parses
  what it read. So a page costs one question to the leader and work that is linear in
  the number of buckets and the same for every caller and every token: measured
  (`BenchmarkSearchDeciding`, Apple M3), deciding 5 000 buckets with 16 KiB policies
  takes 40 ms and 2.5 MB warm against 2.6 s and 2.3 GB cold, and 20 000 with 1 KiB
  policies 20-50 ms warm against 0.8-1 s cold, to which reading the pages adds 0.17 s
  and 0.07 s. That work counts towards the two seconds, looked at before every bucket;
  when it takes them all, the request fails with 503 (`domain.ErrUnavailable` together
  with `domain.ErrTooManyBuckets`, and a `detail` that says what follows) rather than
  stop where a token would have to name a bucket that is not the caller's. After a
  restart that passes: what was parsed before the request gave up is remembered, and
  the next gets further. Past about 10 000 buckets with 16 KiB policies, or 150 000
  with 1 KiB policies, it is a limit of the deployment's size for everybody but root -
  a search of one named bucket stays possible - and a quota on buckets (milestone 8)
  is what keeps a deployment from being put there. A failure to read a page, damage to
  the store included, fails the search (503); one damaged policy leaves out its bucket
  alone. The walk is then
  given only buckets the caller may see, so `examined`, `complete`, the number of
  pages and every token speak of those alone. A bound that stops a request answers
  `complete: false` with `next_continuation`: base64url of
  `{v, q: SHA-256 of the query, b: bucket, k: last key examined, n: bucket name done}`.
  The token is validated - `b` must be a valid bucket name - and not signed: it is
  looked at only after every bucket was decided, among those the caller may see, so a
  made-up token can only skip ahead in what that caller would have been shown.
  Sub-bucket scope plugs in at one marked place, `searchWalk.mayList`.
- **Who uploaded and who downloaded (implemented, ADR-0016).** Every version records who
  committed it (`created_by` on a put, a copy's destination and the completion of a
  multipart upload), in the words of the audit trail. Downloads are counted by the node
  that serves them, in memory, when the body it handed out is closed: a GET answered 200
  or 206 through S3 - signed, presigned or anonymous - and a download through the
  management API; each ranged request by itself. The source of a CopyObject or
  UploadPartCopy is counted apart, as a copy, once its destination is committed. HEAD,
  304, 412, a read that could not be opened and everything the store reads for itself
  (healing, scrubbing, collection, lifecycle) are not counted: they do not pass through
  the one use case that counts (`ObjectService.Get`). `app.AccessRecorder` hands the
  counts to the metadata store through `ports.ObjectAccessStore` every
  `--access-flush-interval` and once more on a graceful stop, at most 256 aggregates an
  entry, each batch named by the node, a number it chose at its start and a sequence
  number; a batch that failed or whose outcome is unknown is sent again under the same
  number and is applied once. Nothing on the path of a GET proposes, waits or can fail
  for it. At most 10 000 aggregates wait on a node; beyond that reads are not counted
  and `cactusfs_object_access_dropped_total` says how many. Until every member knows
  entry schema 3 the uploader is not recorded and the counts wait in memory.
  `GET /api/v1/buckets/{bucket}/object/access?key=` serves the record to holders of
  `admin:GetObjectAccess` (`cluster-admin`, `auditor`); `s3:GetObject` does not imply
  it. The listing (`GET /api/v1/buckets/{bucket}/objects?access=true`,
  `ObjectService.ListWithAccess`) can carry, for each object of the page, the uploader,
  the time of upload, the two totals and the time of the last read - what the record
  says, without the readers. `s3:ListBucket` gates the listing, through `Authorize`, as
  ever. `admin:GetObjectAccess` only decides what the page carries, so it is asked with
  `Authorizer.Allowed`, which writes no denial: for the bucket, and for every object of
  the page with the very request the single read is gated by (`accessRequest`), so that
  a policy that gives or denies the action by key holds in the listing too. The answer
  says `access_included` when the caller holds the action on the bucket or on an object
  of the page, and only the objects the caller holds it on carry `access`; otherwise it
  is the plain listing, never a refusal. The records of a page are read in one call,
  `ports.ObjectAccessStore.ObjectAccesses`: one read transaction of point reads, at
  most 1 000 versions, no walk. The console's object browser asks for it and shows the
  five columns only when the answer includes them.
  The figures are counts kept as well as can be, **not an audit trail**: a crash
  loses up to an interval of them.
- **Console (implemented, ADR-0007).** `web/`, embedded with `-tags console` and served
  at `/` by nodes with the console role. It calls only the admin API, through the client
  generated from the contract. Uploads belong to the console, not to a screen: they go
  on while any screen is shown, and a bar at the top of every screen shows them and the
  notices of what the console did (kept for the visit, not stored). An object's details
  open in its own row of the object browser.
- **Console sessions (implemented, ADR-0006, 0007, 0027).** A session is a record in
  the replicated `sessions` store under the SHA-256 of its token, with its user, the
  hash of its CSRF token, `created_at` and `expires_at`; the cookie is
  `__Host-cactusfs-session` (`HttpOnly`, `Secure`, `SameSite=Strict`). It lasts
  `--session-lifetime` from sign-in or from its last unlock, and nothing extends it.
  Whether it is **active**, **locked** or **ended** is derived on every gateway from the
  record, the clock and two more kinds of record in the same store, written with the
  commands that exist (`session.put`, `session.delete`) - no entry, command or schema
  changed for it:
  - **the upload lease**, `upload-lease:<SHA-256 of the user and the sign-in time>`,
    written by the gateway that serves a `PUT /buckets/{bucket}/object` of the session:
    `SessionService.Run`, the keeper, writes it every third of `--session-upload-grace`
    while an upload is in flight, and once more when the last has ended, and
    `UploadBegun` wakes it when a lease is about to lapse. A session past its timeout
    with a live lease, and less than `--session-lock-max` past it, is locked. The key is
    the hash of no token, so a lease signs nobody in;
  - **the mark of a changed password**, `password-change:<SHA-256 of the user>`,
    written after the password by `IAMService.UpdateUser` and kept 48 hours: a session
    of the user begun no later than the mark plus `PasswordChangeMargin` (5 s) has
    ended, active or locked. A session is dated when its password is about to be
    checked, so a sign-in with the old password that straddles the change is ended too.

  A locked session is a principal with `LockedSession` set, and `iam.Decide` refuses it
  every action but `s3:PutObject` with `domain.ErrSessionLocked`, which the management
  API answers **423** `SessionLocked` (a wrong unlock password is 403, never 401, so that
  an open console does not take either for the end of the session and abort its
  uploads). It may also read its state (`GET /session`, which says `locked`,
  `expires_at` and `lock_ends_at`), unlock and sign out. It is given no stored answer of
  an `Idempotency-Key` but that of an upload. `POST /session/unlock` checks the
  password of the session's own user through `ports.IdentityProvider`, counted by the
  limiter of sign-in; the fifth wrong password ends the session; success writes a new
  record with a new session token and CSRF token, the same `created_at` and a new
  `expires_at`, and deletes the old. The node that began a session deletes its record a
  tick after it has ended; the leader's sweep, every ten minutes, removes what is more
  than `--session-lock-max` past its timeout. The three settings must be the same on
  every node. A node of the release before reads neither lease nor mark: there a lock
  is a sign-out, and a session outlives a change of password to its timeout.
- **Events.** Every state change other components care about is an event named
  `cactusfs.<aggregate>.<event>.v1`, defined in `api/asyncapi/events.yaml` and derived
  from **applied Raft entries**, never published from a handler. Delivery goes through
  `ports.EventPublisher`. **Implemented:** the contract, with twenty-two channels:
  twenty events published inside the process (buckets, objects, versioning, Object
  Lock, bucket configuration, tags, the root credential, topology, nodes, the audit
  trail - and, of entry schema 5, the quota and the replica mode of a bucket and a
  target defined or deleted, which the state machine tells of and no use case causes
  yet), and two messages that are for targets outside the deployment only,
  `cactusfs.notification.object.v1` and `cactusfs.audit.exported.v1`, which nothing
  sends yet; and the in-process publisher (at most once per node; counted in
  `cactusfs_events_total`). NATS JetStream and webhooks are milestone 8 and do not
  exist. Consumers dedupe by event ID.
- **Node RPC.** gRPC over mTLS 1.3, defined in `api/proto/cactusfs/node/v1` and checked
  with `buf lint` and `buf breaking`. **Implemented**, three services: `NodeService`
  (`Info`, `InFlight`, `Settings`), `MetaService` for what only the metadata leader
  can do (`Propose`, `ReadIndex`, `Term`, `Join`, `Leave`), and `StorageService` for
  shards (`WriteShard`, `StatShard`, `ReadShard`, `DeleteShard`, `ListShards`).
- **Sub-buckets and tag rules (not implemented).** ADR-0017 and ADR-0018 are accepted,
  and entry schema 3 carries their commands: the domain rules
  (`internal/domain/bucket`, `internal/domain/tagging`), the ports (`SubBucketStore`,
  `TagDefinitionStore`) and the metadata store's side of them exist and are tested. No
  use case, no path of the management API and no screen of the console uses them, and
  the authorizer does not read a sub-bucket's policy (`PLAN.md`, Goals C and D).

## 7. Security **(see the threat model for what is implemented)**

The full analysis is in `docs/threat-model.md`. In brief:

- Encryption at rest is always on **(implemented)**: AES-256-GCM streaming AEAD with a
  per-object data key wrapped by a KMS key. KMS adapters: a sealed local key file for
  standalone **(implemented)**, Vault Transit for clusters **(implemented, ADR-0006)**.
  There is no unencrypted fallback.
- Encryption in transit: TLS 1.2+ for clients **(implemented for the S3 and the admin
  listener, which share one configuration)**, mTLS 1.3 between nodes including the Raft
  transport **(implemented; certificates are replaced without a restart, ADR-0020)**.
- Every inbound request is authenticated and passes through the single
  `ports.Authorizer` **(implemented, ADR-0006)**: users, groups, roles and IAM-style
  policies; default deny; explicit deny wins; a key never exceeds its owner. One
  exception to default deny, for one action (ADR-0028): the user who committed an object
  version may preview it (`admin:PreviewObject`) when no policy speaks of it - a preview
  also needs `s3:GetObject`, a Deny still refuses and a key's inline policy must allow
  it; decided in `iam.Decide` like everything else. The key,
  its owner and every policy that applies are resolved in one consistent read when a
  request is authenticated. The probe endpoints are the one exception and disclose
  nothing.
- No default API key. A new deployment has one default console user, `admin` with the
  password `admin`, made once (ADR-0014): change it first. API key secrets are envelope-encrypted, shown once, compared
  in constant time **(implemented)**. Keys expire, can be narrowed by an inline policy
  and are revoked for every node at once.
- Hash-chained, append-only audit trail **(implemented)**: changes to identities are
  recorded in the same Raft entry as the change; refusals in the background.
  `--audit-export` writes the records to the log stream as JSON lines.
- Console accounts with Argon2id and replicated, short-lived sessions
  **(implemented, with the console that signs in through them)**. A session that times
  out while it uploads is locked - it may store objects and nothing else - until the
  user's password unlocks it; a changed password ends every session of the user
  **(implemented, ADR-0027; section 6)**.
  The console's "Delete all" asks for the password through the same unlock before it
  begins; each deletion is still judged by the authorizer, so that is a confirmation,
  not a permission (threat model, open question 32).
- Versioning and Object Lock for ransomware resistance **(implemented, ADR-0008, 0009)**.
- Air-gap safe: no runtime call to the internet, no telemetry, no CDN asset.

## 8. Build and delivery **(implemented)**

A fresh clone builds two ways, and `make verify-clone` proves both from a clean export:

- **Go only** — `go build ./cmd/cactusfs` with nothing installed but the Go version named
  in `go.mod`. Generated code is committed. Go build tools are pinned with `tool`
  directives in `go.mod` and run with `go tool`.
- **Container only** — `docker build .` or `podman build .`. The `Dockerfile` builds the
  console (`web` stage), then the binary with `-tags console` (`build` stage), then the
  distroless image. Base images are public and pinned by digest.

The same `Dockerfile` has a `builder` stage: the tool image every `make` target runs in
(`ENGINE=docker|podman`), with the engine socket passed through for targets that build
images. Developers and CI therefore use identical tool versions.

| Tool | Pinned in |
|---|---|
| Go, Node, distroless base | `Dockerfile` `ARG`s, by digest |
| gofumpt, goimports, govulncheck, go-licenses | `go.mod` `tool` directives |
| oapi-codegen, protoc-gen-go, protoc-gen-go-grpc | `go.mod` `tool` directives |
| golangci-lint, Helm, Docker CLI, buf, oasdiff, the registry of the signing gate | `Dockerfile` `ARG`s, by digest |
| kubeconform, yq, promtool, cosign, syft, the AWS CLI, ceph/s3-tests | `Dockerfile` `ARG`s, by version and SHA-256 |
| pnpm | `Dockerfile` `PNPM_VERSION` and `web/package.json` `packageManager` (by version, from the npm registry) |
| console packages | `web/pnpm-lock.yaml` |

The product image is distroless static, runs as UID 65532, has no shell, and is meant to
run with a read-only root filesystem and all capabilities dropped.

**Supply chain (ADR-0021; the gate and the release workflow are implemented, no release is published yet).**
`make sbom` writes SPDX 2.3 JSON to `dist/sbom/` with `syft`: one document for the image
of each architecture, read from the Go module list embedded in the binary, and one for
the console's packages, read from `web/pnpm-lock.yaml`. An image's SBOM declares the
licence of CactusFS itself as `LicenseRef-CactusFS-Blessing`, with the text of
`LICENSE.md` in the document: no identifier of the SPDX list is that text. `make sign-verify` starts a
throw-away registry, pushes both images, and with `cosign` and a key pair made for the
run signs each **by digest**, attaches its SBOM as a signed attestation and verifies
both - with the public key alone, no certificate authority, no transparency log, no
network. It then proves what must be refused: an image never signed, a signed tag moved
to another image, another public key, and one image's signature or attestation put on
another. `scripts/release.sh --dry-run`, which the gate runs, rehearses a release
against such a registry (manifest list, signatures, attestations, `dist/release/` with
`SHA256SUMS`). The gate's key lives for one run: it proves the procedure, not where an
image came from.

A **release** is the same script with `--publish`, and is made only by
`.github/workflows/release.yml` on a pushed tag `v<version>`: the script refuses
anywhere else, and unless the tag is `v` + `internal/buildinfo/VERSION` on a clean
commit. After `make check`, `make test-chaos` and `make test-rolling` in jobs that can
neither push nor sign, it publishes to `ghcr.io/albievan/cactusfs` the image of each platform
(`<version>-<build>-<arch>`) and one manifest list (`<version>-<build>`, and
`<version>` once it is signed and verified - and only where the registry has no such
tag, or has it for this very list: a version is released once; never `latest`), the chart to
`oci://ghcr.io/albievan/charts`, and to the GitHub Release the six binaries with
`SHA256SUMS`, the SBOMs and the digests. Signing is **keyless** (Sigstore): the list,
each platform manifest and the chart by digest, each SBOM as an attestation,
`SHA256SUMS` as a file - under a certificate of Fulcio for the identity
`https://github.com/albievan/cactusfs/.github/workflows/release.yml@refs/tags/v<version>`,
recorded in Rekor - and each is verified against that identity before the next step.
The two modes share the functions that push, list, sign, attest and verify
(`SC_MODE` in `scripts/lib-supplychain.sh`); they differ in the cosign flags, in the
registry (HTTPS and a token instead of the run's own) and in that a release does not
set the proxy that keeps the gate off the network. cosign, syft and the registry are tools of the
builder image (pinned by checksum and by digest in the `Dockerfile`); none is in the
product, and the product verifies nothing at run time and calls nothing.

**Build identity.** `internal/buildinfo` says which build runs: the release, embedded
from `internal/buildinfo/VERSION` (the one place it is set), and the build number (the
count of commits) and commit, which `make` and the `Dockerfile` pass with `-ldflags -X`;
a plain `go build` is build `0`, a local build, with the commit the Go toolchain
recorded. The package has no state and `internal/domain` does not import it; the
composition root reads it once and hands it to telemetry, the start-up log and the
management API, which names it only in answers to authenticated callers
(`ServerVersion` in the session answers and in `GET /cluster`, for the answering node).
Images are tagged and labelled `<version>-<build>`; the chart's `appVersion` is the
release and pods are labelled with the image tag deployed. A node's build is not
replicated: knowing every node's build from one answer needs a field in the node
record, a schema change of its own - entry schema 4 (ADR-0019) does not carry it.

The console (`web/`, React + TypeScript + Vite) is built by the `Dockerfile` and by
`make build`, embedded in the binary with the build tag `console` and served on the
admin port by nodes with the `console` role (`internal/adapters/console`). Without the
tag - plain `go build ./cmd/cactusfs` - the binary serves a page that says the console
was not built into it. Logos come from the brand assets:
masters in `docs/brand/`, the console's copies in `web/src/assets/brand/`, the tab icon
in `web/public/favicon.svg`. They are used as they are and never redrawn.

## 9. Deployment targets **(implemented: binary, compose and the Helm chart, ADR-0015)**

| Target | Artifact |
|---|---|
| Single binary | `dist/cactusfs-<os>-<arch>` — implemented, six targets |
| Docker Desktop, Podman | `deploy/compose/compose.yaml` — implemented: 4-node development cluster over mTLS, rootless-safe; `make up` / `make down` |
| k3s, k8s | `deploy/helm/cactusfs` — implemented: a StatefulSet per node, a claim per drive, PDB, default-deny NetworkPolicy, `restricted` Pod Security Standard; rendered, validated and asserted by `make helm-lint`, installed on k3s by `make test-k8s` |

Core code uses no cloud-provider API. Hybrid means separate clusters per site linked by
asynchronous bucket replication; one Raft quorum is never stretched across a WAN.

## 10. Quality gates **(implemented)**

`make check` runs, in this order: `fmt-check`, `lint`, `vet`, `cover`, `test-int`,
`s3-compat`, `vuln`, `contracts`, `licences`, `web-check`, `build-all`, `image`, `sbom`,
`sign-verify`, `helm-lint`, `observability-check`, `verify-clone`. These are the gates of
`CLAUDE.md` §5 and three more that milestone 7 added: `make sbom`, `make sign-verify`
and `make observability-check`. Every gate has its
subject today and fails without it - a chart, a contract, a baseline, a dashboard, a
value set, a test it names: `PLAN.md` ("Gates and what each fails without") says what
each one fails on.

- `make test-chaos` and `make test-rolling` are outside `make check` and CI jobs of
  their own. The second builds the release before this entry schema from the history
  of the repository, which a clean export has not: commit 84334a7 for the upgrade from
  schema 4 to 5, which is what it runs when told nothing, and 0b7d825 for that from 3
  to 4 (`make test-rolling ROLLING_FROM_COMMIT=0b7d825
  ROLLING_TEST=TestRollingUpgradeFromSchema3ToSchema4`); CI runs both. `ROLLING_TEST`
  is matched as the beginning of a name, so the second runs the case of a locked
  console session among nodes of the release before as well
  (`TestRollingUpgradeFromSchema3ToSchema4SessionLock`, ADR-0027).

- `make test-k8s` (`test/k8s`, build tag `k8s`) is outside `make check` and a CI job of
  its own, because k3s needs a privileged container: the chart installed on k3s with the
  image built here - pods admitted as `restricted` and ready, an S3 round trip, an object
  read with a node away and after a pod was deleted, ports 9001-9003 closed to a pod that
  is not of the release.

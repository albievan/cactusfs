# CactusFS architecture

CactusFS is an S3-compatible, erasure-coded, multi-node object store. It ships as one Go
binary that runs standalone on Windows, Linux and macOS or as a cluster, and it puts data
security first. This document describes the design. `CLAUDE.md` holds the rules,
`PLAN.md` holds the work and its status, and `docs/adr/` holds the decisions that are
expensive to reverse.

Sections marked **(implemented)** describe code that exists. What does not exist yet is
said where it belongs, with the milestone or goal of `PLAN.md` that holds it.
Replication between sites (ADR-0026) is implemented end to end - the destination's
replica mode, replica requests and the questions a source asks, the client of another
site, the replication configuration, the replicator with the later changes of a version,
the resync, the status of a bucket's replication with its metrics and alerts, and the
console (PLAN 8C; "Replication between sites" and "Delivery to targets" in section 5,
"Replica requests" and "Replication configuration" in section 6); no published release
has it yet. Bucket notifications (ADR-0025) are
implemented end to end - the webhook and NATS publishers, the notifier that hands them
the outbox, the export of the audit trail through them, S3's notification configuration,
the management API and the console's Targets screen and a bucket's rules (PLAN 8B;
"Delivery to targets", section 5); no published release has them yet. Quotas on buckets
(ADR-0024) are implemented end to end - the usage of every bucket counted, the quota
enforced where entries are applied and checked before a write, the management API, the
metrics and alerts, and the console (PLAN 8A; "Usage and quotas", section 5); no
published release has them yet. Sub-buckets (ADR-0017) and tag rules with a catalogue
of tags (ADR-0018) are implemented end to end - domain, store, use cases, management
API and console. The release is 0.7.0 (`internal/buildinfo/VERSION`, tagged
`v0.7.0` on 2026-11-13): its image (`ghcr.io/albievan/cactusfs:0.7.0`) and Helm chart
(`ghcr.io/albievan/charts/cactusfs:0.7.0`) were published and signed keyless by the
release workflow, which then failed to create the GitHub Release (cosign's bundle was not
readable to the runner's user; the scripts are fixed), so it has no GitHub Release, no
published binaries and no `SHA256SUMS`. That release predates the enforcement of tag
rules (PLAN D.2b): no published release enforces them yet.

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

The events adapter is the in-process bus for the events inside the process; the NATS
JetStream and webhook adapters (`internal/adapters/events/{nats,webhook}`) are the
publishers of targets, through which notifications and the audit trail leave the
deployment (ADR-0025, "Events" in section 6).

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
| `internal/domain/bucket` | bucket naming rules, bucket configuration (CORS, tags), lifecycle rules, the record of a bucket's latest use (ADR-0019); sub-buckets and tag rules (ADR-0017, 0018), the usage record and the rule of a quota (`Usage.Admits`; ADR-0023, 0024); notification and replication documents and what they match (`Match`, `Replicates`; ADR-0025, 0026), applied by the state machine and kept by `app.NotificationService` and `app.ReplicationService`; the record of a resync |
| `internal/domain/object` | IDs, keys, version metadata, ranges, listing rules, conditions, tags, Object Lock, multipart, SSE, the record of who read an object (ADR-0016), the streaming AEAD (ADR-0002) |
| `internal/domain/iam` | principals (a locked console session among them, ADR-0027), actions, the policy language and the decision, bucket policies, users, groups, roles, API keys, built-in roles, sealed secrets |
| `internal/domain/delivery` | targets of delivery and their endpoints, the items of the outbox, classes of failure and the pause between attempts (ADR-0023, 0025, 0026: used by the metadata store, the use case of targets, the delivery worker, the notifier and the replicator) |
| `internal/domain/audit` | audit records, their hash chain and their exported line (`Record.Line`, `ParseLine`) |
| `internal/domain/cluster` | readiness; nodes, roles and labels; drive topology: planning across nodes, slots, reconciliation, replacement; a node's settings and their redaction (ADR-0022) |
| `internal/domain/erasure` | erasure parameters, quorum, set layout, placement, stripe geometry |
| `internal/domain/search` | the language of the search field: text, wildcard, `rx:` regular expression |
| `internal/domain/preview` | what kind of thing an object is shown as in a preview, from its stored type and the signature of its bytes; the size of an image from its header; the walk of a GIF's frames as its bytes stream (ADR-0028) |
| `internal/domain/preview/previewtest` | the hostile files of the preview's tests, made by code, for the domain's, the use case's and the API's suites (a depguard rule keeps it out of the binary) |
| `internal/domain/release` | versions (semantic versioning 2.0), releases and their files, the channels, what may be installed (`Allowed`, `NextStep`), the signed lists `SHA256SUMS` and `digests.txt` read strictly, `Verified` - the one thing an upgrader takes - and the progress of an upgrade (ADR-0029) |
| `internal/domain/tagging` | the catalogue of tag definitions (ADR-0018), kept by `app.TagDefinitionService` |
| `internal/ports` | `KMS`, `ShardStore` (`ShardWriter`, `ShardReader`), `ErasureCodec`, `BucketStore`, `BucketPolicies`, `ObjectIndex`, `UploadStore`, `ObjectAccessStore`, `BucketActivityStore`, `RootCredentialStore`, `TopologyStore`, `NodeRegistry`, `ClusterView`, `DriveSource`, `NodeSettingsReader`, `LocalSettings` (a function), `EventPublisher`, `IAMStore`, `AuditLog`, `SessionStore`, `IdempotencyStore`, `PasswordHasher`, `IdentityProvider`, `Authorizer`, `Clock`, `ReadinessCheck`, `ReleaseSource`, `ReleaseVerifier`, `Upgrader` (ADR-0029); `SubBucketStore` and `TagDefinitionStore` (read by the object use cases and kept by `app.TagDefinitionService`), and of milestone 8 `BucketUsageStore` (kept by `app.UsageCounter`, read by `app.QuotaService` and the object use cases), `TargetStore` (of `app.TargetService`), `Outbox` (read by `app.DeliveryWorker`) and `ReplicationStore` (of `app.ReplicationService` and `app.ResyncWorker`), implemented by the metadata store; `DeliveryStatusReader` (implemented by `app.DeliveryWorker` on the node that leads and, on the others, by asking it over node RPC - `leaderDelivery` in `cmd/cactusfs`; read by `app.TargetService` and `app.ReplicationService`); `SiteClient` (implemented by `internal/adapters/s3client`, called by `app.Replicator`) |
| `internal/ports/porttest` | in-memory fakes and the contract tests every implementation runs |
| `internal/app` | bucket, object (previews included), multipart, lifecycle, search, settings and identity use cases; quotas and the usage of buckets (`QuotaService`, `UsageCounter`, ADR-0024); targets (`TargetService`); delivery to targets (`DeliveryWorker`) and the export of the audit trail (`AuditExporter`); bucket notifications (`NotificationService`, `Notifier`, ADR-0025); replication between sites (`ReplicationService`, `Replicator`, `ResyncWorker`, ADR-0026); checking for releases and installing one (`UpdateService`, ADR-0029); sessions and idempotency; who read what; striped storage, healing, scrubbing, garbage collection, topology; the authorizer; the audit trail; health |
| `internal/buildinfo` | the version, build and commit of the binary, and whether it was built as the image's (section 8) |
| `internal/tools` | tools of `make` targets, built with the tag `tools`: `trustedroot` fetches Sigstore's trusted root through its TUF client (`make trusted-root`) |
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
| `internal/adapters/egress` | out: the one client of every outbound call a deployment is told to make - https only, every resolved address judged at connect, bounded bodies, redirects only where a policy allows (ADR-0023, as ADR-0029 amends it) |
| `internal/adapters/releases/github` | out: the update source - GitHub's releases JSON or a mirror of it; downloads only from the source's host (and `github.com` for GitHub's API) (ADR-0029) |
| `internal/adapters/releases/sigstore` | out: the verifier of the release workflow's Sigstore bundles, offline, against the embedded `trusted_root.json` (ADR-0029); `sigstoretest` signs with a Sigstore of its own for tests (a depguard rule keeps it out of the binary) |
| `internal/adapters/upgrade/binary`, `upgrade/kubernetes`, `upgrade/off` | out: the three upgrade modes - replace the binary of a node on its own; move the chart's StatefulSets one node at a time through the Kubernetes API; install nothing and show the commands (ADR-0029) |
| `internal/adapters/events/inproc` | out: in-process event bus |
| `internal/adapters/events/webhook`, `events/nats` | out: the publishers of a target - a signed POST through the egress client; a message to NATS JetStream through the egress dial function, acknowledged (ADR-0025) |
| `internal/adapters/sealed` | the opener of a target's sealed fields through the KMS, shared by the publishers (ADR-0023) |
| `internal/adapters/s3client` | out: the client of another deployment that replication sends to (`ports.SiteClient`) - path-style S3 through the egress client, every request signed with SigV4, the replica headers among the signed ones, objects and parts sent as signed chunks; failures classed as the publishers' are (ADR-0026) |
| `internal/adapters/kms/local` | out: sealed local key file |
| `internal/adapters/kms/vault` | out: Vault Transit (ADR-0006) |
| `internal/adapters/passwords` | out: Argon2id password hashes |
| `internal/adapters/audit` | out: audit records as JSON lines (`audit.Record.Line`) on the node's output |
| `api/openapi`, `api/asyncapi`, `api/proto` | the contracts: the management API (and the Go package that embeds it for serving), the events, node RPC |
| `web` | the console (React + TypeScript + Vite), and the Go package that embeds its build with the tag `console` |
| `cmd/cactusfs` | composition root: the settings, node wiring, listeners, shutdown |
| `test/integration`, `test/clustertest` | black-box tests of the built binary and of a four-node cluster of containers; fixtures of older schemas for the upgrade tests; `releasemirror`, a TLS mirror of releases signed with `sigstoretest`, for the test of upgrading from the console |
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
| `--tls-reload-interval` | `CACTUSFS_TLS_RELOAD_INTERVAL` | `1m` | how often the certificate and key files, the node's too, are read again (at least `5s`; `0` never reads them again). A new pair that is valid is used for new connections without a restart; one that is not is refused and logged, and the old one stays (`cactusfs_tls_reload_pending` in `/metrics` is 1 while it does). The node CA is read at start only: a new CA needs a restart. Rotation is not revocation: a replaced certificate and its key remain accepted by peers until that certificate expires; for a compromised key, replace the node CA (a node that leaves the cluster is revoked, ADR-0030, but only the node itself can ask to leave). The files must be regular files (not pipes) |
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
| `--root-access-key-file` | `CACTUSFS_ROOT_ACCESS_KEY_FILE` | — | path of a file holding the access key ID of the root credentials, instead of $CACTUSFS_ROOT_ACCESS_KEY. The file holds a secret: its path is not shown with the node's settings |
| `--root-secret-key-file` | `CACTUSFS_ROOT_SECRET_KEY_FILE` | — | path of a file holding the secret key of the root credentials, instead of $CACTUSFS_ROOT_SECRET_KEY. The file holds a secret: its path is not shown with the node's settings |

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
| `--egress-allow` | `CACTUSFS_EGRESS_ALLOW` | none | ranges (CIDRs, separated by commas, such as `127.0.0.1/32`) where the update source (`update-source`, and every address its downloads are redirected to), a notification target or another site may be at an address that is otherwise never connected to: loopback, link-local, and the IPv6 ranges that stand for IPv4 addresses by a gateway's rule (`::/96`, `64:ff9b:1::/48`, `2002::/16`, `::ffff:0:0:0/96`). Private ranges need no entry. A range of every address (`0.0.0.0/0`, `::/0`) and one that could admit nothing (`0.0.0.0/8`, multicast) are refused. The addresses of the metadata services of clouds (169.254.169.254, `fd00:ec2::254`, `fd20:ce::254`, 100.100.100.200) are never connected to, whatever this holds; a range that is allowed is open with everything else in it, so allow the one address a receiver has. The address a name resolves to is judged at every connect. It is read by the update source (ADR-0029), by notification targets, the export of the audit trail included (ADR-0025), and by replication to other sites (ADR-0026). The same on every node |
| `--delivery-timeout` | `CACTUSFS_DELIVERY_TIMEOUT` | `30s` | how long a notification target or another site has to answer one request, once it is sent, and to make progress reading or writing (`1s` to `5m`); one that does not is tried again later. Connecting and the TLS handshake have 5 seconds each besides. It is read by notification targets, the export of the audit trail included (ADR-0025), and by replication to other sites (ADR-0026), where it bounds each request of a version, not the whole version: a version may take as long as its size and replication-rate-mib need. The same on every node |
| `--replication-workers` | `CACTUSFS_REPLICATION_WORKERS` | `4` | how many versions are replicated to other sites at once (`1` to `32`), by the node that leads the metadata group; two versions of one object, or a version and a later change of it, are never sent at once (ADR-0026). The same on every node |
| `--replication-rate-mib` | `CACTUSFS_REPLICATION_RATE_MIB` | `0` | limit on the bytes replication sends to other sites, in MiB per second averaged over 10 seconds (at most `1048576`); `0` is no limit. It is held to over every 10 seconds, with no burst above it: what all the versions being sent read together, by the node that leads (ADR-0026). An attempt at a version is given a day, or the time its size takes at 64 KiB/s when that is longer - or at this rate shared by `replication-workers` to each of the 256 targets a deployment can have, when that is slower still; a transfer that stops moving is ended by `delivery-timeout`. A version that cannot be sent is tried again - one of parts from the parts that arrived, one sent in one request from its start - and holds its target meanwhile. The same on every node |

**Maintenance: background work**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--scrub-interval` | `CACTUSFS_SCRUB_INTERVAL` | `24h` | pause between two passes of the scrubber, which verifies and heals every object; 0 disables it. The same on every node |
| `--scrub-rate-mib` | `CACTUSFS_SCRUB_RATE_MIB` | `64` | limit on how fast the scrubber reads, in MiB per second; 0 is no limit. The same on every node |
| `--lifecycle-interval` | `CACTUSFS_LIFECYCLE_INTERVAL` | `1h` | pause between two passes of the worker that carries out the lifecycle rules of buckets; 0 disables it. The same on every node |
| `--gc-interval` | `CACTUSFS_GC_INTERVAL` | `1h` | pause between two passes of the garbage collector for unreferenced shards; 0 disables it |
| `--gc-grace` | `CACTUSFS_GC_GRACE` | `24h` | minimum age of an unreferenced shard before it is removed (at least `1h`). The same on every node |
| `--access-flush-interval` | `CACTUSFS_ACCESS_FLUSH_INTERVAL` | `30s` | how often the downloads this node counted are recorded with the objects, at least 5s; a node that crashes loses up to this much of the count |

**Updates: checking for releases, and installing one from the console**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--update-source` | `CACTUSFS_UPDATE_SOURCE` | none: nothing is asked | address of the releases JSON - `https://api.github.com/repos/albievan/cactusfs/releases`, or a mirror of it that answers as it does - which the node asks what releases exist. `https` only, with no user information, query or fragment. Nothing is asked of anyone while it is empty, the default. Who answers it decides what is offered, never what is installed: a release is installed only when its signed lists verify against the trusted root, for its version. The same on every node |
| `--update-check-interval` | `CACTUSFS_UPDATE_CHECK_INTERVAL` | `24h` | how often the node asks the update source by itself, give or take a tenth, the first time within 5 minutes of its start (at least `1m`; `0` only when asked to from the console). A node asks at most once a minute, however it is asked. The same on every node |
| `--update-channel` | `CACTUSFS_UPDATE_CHANNEL` | `stable` | which releases are offered: stable, or prerelease for pre-releases too. The same on every node |
| `--update-ca` | `CACTUSFS_UPDATE_CA` | the system's CAs | path of a PEM file of the CA certificates the update source's certificate is verified against; default the system's CAs |
| `--update-trusted-root` | `CACTUSFS_UPDATE_TRUSTED_ROOT` | the root embedded in the binary | path of a Sigstore `trusted_root.json` that the signatures of releases are verified against, instead of the one embedded in the binary: for a node that has been offline across a rotation of Sigstore's keys. A file that does not parse stops the node at start |
| `--upgrade-mode` | `CACTUSFS_UPGRADE_MODE` | `off` | how a release is installed from the console: `off` installs nothing and shows the commands to run instead; `binary` replaces the binary of a node on its own and restarts it (needs `--update-source`; refused by the binary of the container image); `kubernetes` moves the nodes of a chart to the release's image one at a time, this node last (set by the chart's `upgrade.fromConsole.enabled`, which mounts the token it needs; refused without it) |

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
  The node ports (node RPC and Raft) take at most 16 handshakes in flight from one
  source (an IPv4 address or an IPv6 /64) and 4096 in all; a caller that sends nothing
  within 2 s is closed, and a handshake has 10 s to finish.
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
| `cactusfs_quota_refusals_total` | counter | - | writes this node refused for a bucket's quota (ADR-0024), by the pre-check before any shard was written or at commit; no bucket is named |
| `cactusfs_buckets_quota` | gauge | `state` | buckets by how they stand against their quota: `set` (they have one), `over_90` (they hold 90 % of it or more), `full` (they hold all of it or more); each includes the next; read from the node's own copy of the usage records, so the same on every node that is current (aggregate with `max`); read again at most every 10 s; absent below metadata level 5 or when the node cannot read the buckets; alerts `CactusFSBucketQuotaFull` and, for its absence on every node, `CactusFSBucketQuotasUnknown` |
| `cactusfs_bucket_usage_counting` | gauge | - | buckets whose usage is being counted, for the first time or by a recount (ADR-0023); like `cactusfs_buckets_quota`, the same on every node |
| `cactusfs_bucket_usage_drift_total` | counter | - | recounts of a bucket's usage that found other totals than were kept, counted by the node that proposed the entry that ended them, so a sum over the nodes is the number of drifts (a fault of whoever kept the record; ADR-0024); a clamp of a count at its bound shows here when its recount ends, and has no metric of its own; alert `CactusFSBucketUsageDrift` |
| `cactusfs_delivery_attempts_total` | counter | `kind`, `result` | attempts at handing an item of the outbox to a target, made by this node while it led the metadata group (ADR-0023): `kind` is `webhook`, `nats` or `s3`, `result` is `ok` or the class of the failure (`rejected`, `timeout`, `connect`, `tls`, `server`, `redirect`, `egress_refused`, `auth`, `no_stream`, ...; a word this release does not know is `other`); no target is named; each attempt is counted once, on the node that made it, so a sum over the nodes is the number of attempts |
| `cactusfs_delivery_targets` | gauge | `kind`, `state` | targets by kind and state of delivery: `ok`, `retrying`, `stalled`; every kind and state is a series, 0 where none is; exposed by the node that leads the metadata group and absent on every other, so aggregate with `max` (during a change of leader two nodes may expose it for one scrape); the state of one target is `GET /api/v1/targets/{name}/status` |
| `cactusfs_delivery_oldest_pending_seconds` | gauge | `kind` | how long the oldest item of the outbox that waits for a target of the kind has waited, 0 when none waits; the leader's, absent elsewhere (aggregate with `max`) |
| `cactusfs_outbox_rows` | gauge | - | rows the outbox holds, as the leader read them last (at most every 5 s); absent on every other node (aggregate with `max`) |
| `cactusfs_outbox_dropped_total` | counter | - | rows pushed out of the outbox at its bound since the deployment began, a count the metadata keeps (`outboxdropped`) and the leader exposes: it does not begin again with a new leader, so aggregate with `max`, never `sum` |
| `cactusfs_egress_refusals_total` | counter | `class` | requests to a place outside the deployment that got no answer, and connections to one (a NATS server) that could not be made, by class: `egress_refused` (an address the policy refuses), `redirect`, `tls`, `timeout`, `connect`, `internal`; every class a series from the start; no target and no URL is named; counted by the node that sent the request (sum over the nodes) |
| `cactusfs_replication_bytes_total` | counter | - | bytes of versions and parts this node read into requests to other deployments (ADR-0026), whatever became of the request - the traffic `--replication-rate-mib` bounds; no bucket, key or target is named (sum over the nodes) |
| `cactusfs_replication_versions_total` | counter | `result` | versions this node, while it led, found at or sent to another deployment (`completed`) or gave up on (`failed`: SSE-C, or refused there for itself), from the outbox and from resyncs; both results a series from the start (sum over the nodes) |
| `cactusfs_replication_lag_entries` | gauge | - | rows of the outbox that wait for a target of kind `s3` (another deployment), all such targets together; the leader's, absent elsewhere (aggregate with `max`); one bucket's is `GET /api/v1/buckets/{bucket}/replication/status` |
| `cactusfs_replication_lag_seconds` | gauge | - | how long the oldest of those rows has waited, 0 when none waits; the leader's, absent elsewhere (aggregate with `max`); `CactusFSReplicationLag` fires above 15 min |
| `cactusfs_replication_resyncs` | gauge | `state` | resyncs the leader walks, by state: `walking`, `held` (by a failure, retrying or stalled), `waiting` (for a configuration, a target, an object the outbox sends, or the other buckets of the target); every state a series; the leader's, absent elsewhere (aggregate with `max`) |
| `cactusfs_replication_resync_events_total` | counter | `event` | what befell the resyncs this node walked while it led: `held` (a pass held at a version), `preempted` (a send the outbox ended), `restarted` (a walk begun again because its target lost rows); every event a series from the start (sum over the nodes) |
| `cactusfs_update_checks_total` | counter | `outcome` | askings of the update source, asked for or periodic: `ok`, or why it could not be read (`unreachable`, `rate-limited`, `refused`, `invalid-answer`, `timeout`) |
| `cactusfs_update_available` | gauge | - | 1 when the releases the update source last listed hold one that may be installed, else 0 |
| `cactusfs_upgrade_runs_total` | counter | `outcome` | upgrades started from this node: `refused` by the upgrader, or the state the run ended in (`done`, `failed`, `restart-required`) |
<!-- metrics:end -->

The values of `port`: `kms`, `shards`, `metadata`, `local_config`, `object_access`,
`bucket_activity`, `iam`, `sessions`, `idempotency`, `audit`, `events`,
`identity_provider`, `passwords`, `cluster_view`, `node_settings`, `targets`, `release_source`,
`release_verifier`, `upgrader`, `bucket_usage`, `outbox`, `delivery_status`, `replication`.

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

The entry schema is at 6 (`MetadataVersion`, `internal/adapters/raftmeta/entry.go`):

| Schema | Adds | ADR |
|---|---|---|
| 1 | buckets, the object index, the root credential, nodes and topology, identities, sessions, the audit trail | 0003, 0005, 0006 |
| 2 | the leader's time on every entry; object versions, delete markers, Object Lock, bucket configuration, multipart uploads | 0008 (with 0009, 0011, 0012) |
| 3 | who committed a version and the counts of reads; the commands of sub-buckets, tag rules, tags of an upload and tag definitions | 0016 (with 0017, 0018) |
| 4 | who created a bucket; the record of a bucket's latest uploads and downloads | 0019 |
| 5 | what every bucket holds, counted by every member, and its quota; targets, the outbox of what is to be delivered and its cursors; replica buckets (made and promoted through `app.ReplicationService`) and the record of a resync (started by `ReplicationService.Resync`, walked by `app.ResyncWorker`) | 0023 (with 0024, 0025, 0026) |
| 6 | the revocation of a node that leaves (`node.revoke`, the bucket `revokednodes`); nothing old changes | 0030 |

Upgrades from schema 2, 3 and 4 are tested against fixtures written by the binaries of
those schemas (`test/integration/testdata`), and rolling upgrades from 3 to 4 and from
4 to 5 with the binaries themselves (`make test-rolling`, which takes the release to
start from and the test of that upgrade: `ROLLING_FROM_COMMIT`, `ROLLING_TEST`). After
the last member is upgraded, the first entry of the new schema ends the way back to the
release before (ADR-0016, ADR-0019, ADR-0023). The rise from 5 to 6 is tested in
process (`TestRollingUpgradeToSchema6`, `TestRevokingANode`,
`TestAStateOfSchema6IsOpenedOnlyByABinaryOfSchema6` of `internal/adapters/raftmeta`):
only a node that leaves writes schema 6, so an upgraded cluster where nobody leaves stays
one the release of schema 5 opens (ADR-0030).

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

**A state becomes schema 5 within seconds of level 5** (ADR-0023, "Rollback", as it
holds from PLAN 8.3.3): the leader's usage counter proposes a `bucket.usage.walk` for
the first bucket that has no exact record, and that entry is of schema 5 - as are a
bucket created at level 5, a quota set, a recount and a target defined. A deployment of
one node is at level 5 from its first start with this build. So the way back to the
release of schema 4 ends, for a deployment that holds a bucket, almost as soon as the
last member has registered; to keep it, hold one member at the release before (the
cluster stays at level 4, counts nothing and answers 503 to everything of milestone 8).
Before that entry, the way back is walked with **every node stopped
first** and then all started on the release of schema 4, never a node at a time: a node
taken back has a record that says 5 until it registers again, the leader goes on
writing schema 5 meanwhile, and one bucket created in that time stops the node taken
back at that entry - fail-safe, its state untouched, and cured by starting it on this
release again - and ends the way back for the cluster (ADR-0023, "Rollback";
`TestARollingRollbackFromLevel5EndsAtTheFirstBucket`). A bucket created through a node
that had not yet seen level 5 is written below schema 5 and has no usage record until
the counter of PLAN 8.3.1 walks it. The bytes of schema 5 are frozen: the golden files
`schema5-entries.golden.txt` and `schema5-records.golden.txt`
(`internal/adapters/raftmeta/testdata`) may not change - schema 6 left them as they
were - and neither may `schema6.golden.txt`, which pins `node.revoke` and its record
(`TestTheGoldenFileOfSchema6IsFrozen`): a change of any of them needs a schema 7.

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

### Usage and quotas **(implemented, ADR-0023, ADR-0024; PLAN 8A)**

- **Usage is kept by the state machine.** Every member keeps each bucket's usage record
  as it applies entries (above); a bucket made at level 5 is exact from its creation.
  `app.UsageCounter`, run on every node and active on the leader only, counts the
  buckets that are not exact - those from before level 5, and any whose count was begun
  again - in `bucket.usage.walk` entries of 10 000 records, one every 100 ms, the buckets
  taking turns; it looks at every bucket at once on coming to lead and then every 10 s.
  `POST /api/v1/buckets/{bucket}/usage/recount` (`admin:RecountBucketUsage`) begins a
  recount through any node; it is audited by the use case in the background, since its
  entry carries no record. A recount that finds other totals than were kept is drift:
  logged with the bucket's name and counted once, by the node whose entry ended it.
- **`GET /api/v1/buckets/{bucket}/usage`** answers from this node's own copy of the
  record when it is exact (`exact: true`, with versions, stored bytes, the bytes of
  uploads in progress and the quota) - not cached, and on a follower possibly a moment
  behind the leader - and by the bounded walk of before otherwise (`exact: false`).
- **A quota** is a hard limit in bytes on one bucket, 1 to 2^62, on what it stores:
  every version, current or not, and the parts of uploads in progress
  (`stored_bytes + upload_bytes`). `app.QuotaService` reads, sets and removes it
  (`admin:GetBucketQuota`, `admin:PutBucketQuota`, `admin:DeleteBucketQuota`; held by
  `cluster-admin`, and the first by `auditor`; never by `bucket-admin`), carrying the
  audit record in the entry; a quota is refused with 409 until the bucket is exact, and
  everything of quotas answers 503 below level 5.
- **Where it is enforced.** In apply, by `bucket.Usage.Admits` on the net change of
  a put or a part - the authority, in the one order of the log, so two writers racing for
  the last bytes cannot both pass. Before a shard is written, `QuotaService.Admit` judges
  the declared length less what the write would replace, from this node's copy: advice
  that saves the shards of a write that cannot succeed. A write the store refuses at
  commit has its shards removed at once. Nothing that removes or completes is refused,
  so a full bucket can always be emptied. Both APIs answer `403 QuotaExceeded`; the
  inbound adapters read what is left of a refused body, within 8 MiB and 5 s, so that the
  client hears the 403 rather than a reset (`internal/adapters/httpdrain`).
- **What is told.** `cactusfs_quota_refusals_total` (each refusal once, on the node that
  refused it; not audited - ADR-0024, open point 4), `cactusfs_buckets_quota{state}` and
  `cactusfs_bucket_usage_counting` (read by every node from its own copy, at most every
  10 s: aggregate with `max`), `cactusfs_bucket_usage_drift_total` (sum it). The alerts
  `CactusFSBucketQuotaFull`, `CactusFSBucketQuotasUnknown` (the gauges absent on every
  node at level 5 with a leader) and `CactusFSBucketUsageDrift` are in
  `deploy/observability`. No metric names a bucket. A clamp of a count at its bound is
  counted by no metric: the recount it begins reports it as drift (ADR-0023).
- **The console** shows a bucket's quota in the grid of buckets - a bar of what counts
  against it, "not set", or "not counted yet" - and an editor in GB (10^9 bytes) for
  whoever may read it; the server decides every change.

### Replication between sites **(implemented, ADR-0026; PLAN 8C)**

- **Two deployments, one writer.** A site is a deployment of its own - cluster, CA, KMS,
  users - and two sites are linked by replication of buckets, never by one metadata
  group across a WAN. A bucket is written by its clients or by exactly one source: a
  replica bucket (`replica_from`, `bucket.replica.put`, `admin:PutBucketReplicaMode`)
  takes versions and delete markers only from requests that carry `replica` from that
  source, and the state refuses every other new version or delete marker in apply
  (`409 InvalidBucketState`), the root identity's too. It becomes a replica only while
  it holds no version. The destination's own users may still change the tags, retention
  and legal hold of a version there, and the source's next later change of that version
  sets them again as the source has them (a retention is never shortened there). No two
  writers make versions, so there are no conflicts to resolve.
- **Order by the source's sequence.** A replica version keeps its source's version ID,
  time and sequence, and is current exactly when no version of its key has a greater
  sequence, so arrivals in any order give the same state, byte for byte
  (`TestAnyOrderOfArrivalGivesTheSameVersions`); a copy sent twice is refused the second
  time, which the source takes as "there".
- **The source** appends a row for `r:<target>` as it commits a version a rule of the
  bucket's configuration matches (status `PENDING`), and the node that leads sends it
  through the delivery worker's replicator as an S3 client of the other site (below,
  "Delivery to targets"); results come back through `outbox.ack` (`COMPLETED`,
  `FAILED`), by row or, for a resync, by version. `pending` and `failed` of a bucket are
  counted in its usage record by every member.
- **What crosses**: new versions with their metadata, tags and locks, later changes of
  tags, retention and legal hold, and a caller's delete markers when a rule says so.
  **Never**: a version removed for good, a lifecycle rule's marker, an SSE-C version
  (`FAILED`), a replica (one hop - also after its bucket's promotion, ADR-0026 open 18),
  bucket configuration, policies, identities. No key crosses: the source decrypts inside
  TLS, the destination encrypts under its own keys.
- **Failing over is a promotion** (`PUT /buckets/{bucket}/replica` with an empty
  source): the bucket takes writes again and nothing more from its source. **Fail-back
  is by hand**, into a new, empty replica bucket at the old site, and carries only what
  the promoted bucket wrote itself (README, "What fail-back is not").
- Where each part is: the destination's requests and questions, and the configuration,
  in section 6 ("Replica requests", "Replication configuration"); the replicator, the
  resync and the status of a bucket's replication under "Delivery to targets" below; the
  console in section 6 ("Console"); the threats in the threat model ("Replication
  between sites").

### Delivery to targets **(implemented: the worker and its status, ADR-0023, PLAN 8.0.14; the notifier and the publishers of targets, ADR-0025, PLAN 8.2.5; the audit trail through a target, PLAN 8.2.8; the replicator, its resync and the status of replication, ADR-0026, PLAN 8.1.4 to 8.1.7)**

- **One node delivers.** `app.DeliveryWorker` runs on every node and works on the one that
  leads the metadata group (it asks every second); when the node stops leading it stops
  sending - not the next item, not the next attempt - and forgets what it kept. Every
  5 s it reads the targets and the cursors: a new target gets a loop, a deleted one loses
  it, a changed one is delivered to as it now is.
- **A loop a target.** It reads the outbox after the target's cursor, 100 items at a time
  (`app.DeliveryBatch`), hands each to the `app.Deliverer` registered for its kind of
  consumer (`n:` or `r:`), in order, and acknowledges through Raft (`outbox.ack`) after
  every 100 delivered items or once a delivered item has waited 1 s - so a new leader
  sends again at most one batch and one wave (amended 2026-11-16 at the review of PLAN
  8.1.4: a wave cut off by a stop or a lost lead has nothing of it acknowledged, and
  is of one batch). An acknowledgement that fails holds the target as a
  failed attempt does: it is tried again after `delivery.Backoff` and nothing more is
  delivered until it is taken. While the node does not lead a loop sends nothing and
  looks again every second, so a lead lost for a moment stops nothing for good; a loop
  that finds its target deleted ends, and a target made again under the name gets a new
  loop at the next refresh. Rows for other targets are passed over and acknowledged
  with the rest. A failure holds the target at the item, tried again after
  `delivery.Backoff` (1 s doubling to 5 min, up to a quarter off at random), with what
  was delivered before acknowledged first; a failure whose class passes (`rejected`,
  `sse_c`) gives the item up, counted. Results of replication (`COMPLETED`, `FAILED`)
  go with the acknowledgement, for a target of kind `s3` only. An item of a kind that
  nothing delivers fails as `internal` and holds its target. **Waves** (PLAN 8.1.4): a
  kind may be handed over several items at once (`DeliveryWorkerConfig.Parallel`;
  replication: `--replication-workers`). The next items of the outbox go together, at
  most that many and never two of one object - the object at the target, when the
  Deliverer names it (`app.ObjectNamer`; the replicator names the target, the bucket
  there and the key, so two buckets here never reach one object there at once) - so
  the items of one object go one after another in the order of the outbox; the next
  wave begins when every item of this one was taken or passed, so the cursor never
  passes an item that was not, and a failure holds what is behind its wave as a failure
  of one item does. Each item is paused by its own failures, and the target is shown by
  the item that has failed most. Notifications go one by one. **A target that changes**
  - its connection (`delivery.Target.SameConnection`: endpoint, certificates, region,
  credentials), or a bucket it admitted that it admits no longer (compared as sets: a
  list widened or reordered changes nothing) - ends what is in flight to it at the next
  refresh (each attempt runs under a context of the run's that the refresh cancels), and
  the item is sent again at once to the target as it now is.
- **Publishers.** The notifier (`app.Notifier`, the `Deliverer` of `n:`) publishes
  through the publisher of the target, which the composition root makes the first time
  there is something for it (`cmd/cactusfs/publishers.go`, an `app.PublisherSource`):
  a webhook (`internal/adapters/events/webhook`) or a NATS server
  (`internal/adapters/events/nats`), each with an egress client of its own - the
  target's certificates are its roots - observed by `telemetry.ObserveEgress` through
  `egress.Client.Observe` (an observer is told of each request and its outcome and cannot
  stand in for the transport), the dial function behind
  `telemetry.InstrumentEgressDial`, and the publisher behind
  `telemetry.InstrumentEventPublisher`. Making one connects to nothing and opens
  nothing: the sealed fields are opened through the node's KMS (`sealed.KMS`, shared by
  both adapters) at each attempt of a webhook and at each connection to a NATS server,
  and cleared once used. A target whose endpoint, subject, certificates, way of proving
  itself or sealed fields changed gets a new publisher and the old one is closed; once a
  minute those of targets that are gone (and made before the targets were read) are
  closed, and all of them on a node that does not lead; once the node closes none is made.
  A request on behalf of a target older (`CreatedAt`) than the one held under its name is
  refused as `internal`, so a deleted target's loop cannot close the publisher of the one
  made again. The same source (`publishers.Site`, an `app.SiteSource`) makes the client
  of another deployment for a target of kind `s3` - `internal/adapters/s3client`, with
  the egress client of the target, the same sealed opener, the ID of this deployment
  and the build's version - behind `telemetry.InstrumentSiteClient` (port `site`), and
  keeps, replaces and closes it as a publisher.
- **The replicator** (`app.Replicator`, the `Deliverer` of `r:`; ADR-0026, PLAN 8.1.4).
  For each row it reads the replication configuration of the row's bucket - sent only
  while it names the target, which must admit the bucket, to the bucket it names - and
  the version, through `ObjectService.ReplicationVersion`, `OpenReplicationSpan` and
  `ReplicationParts` as the replication service (`iam.ServiceReplication`, which may
  `s3:GetObjectVersion` and nothing else; not counted as a download). It sends nothing
  to a bucket until its probe says it keeps versions and is a replica of this
  deployment (asked again after a minute, after any failure there and once the target's
  connection changes), and nothing that carries a lock to a bucket without Object Lock
  (the probe says which; the target holds as `object_lock`), asks whether
  the version is there already (with its entity tag: `COMPLETED`, nothing sent; with
  another: `FAILED`), then sends it in one PUT, or a version of parts as an upload of
  the same parts, or a delete marker - only while the deciding rule copies markers,
  never one the lifecycle service made (its delete says `by: service/lifecycle`, as
  every delete now says who deleted). **A version of parts goes on** from where an
  attempt was cut off: the upload is remembered by target, generation of its
  connection, bucket there and version (in this process; at most 1 024 uploads and
  100 000 parts, the oldest idle ones beyond aborted as a best effort - one an attempt
  is sending through is never aborted, and the bounds are exceeded instead while the
  uploads in flight alone pass them), the next attempt asks for
  its parts (`ListParts`) and sends those that did not arrive with the entity tag they
  were given; the other uploads this deployment left of the key are aborted; an upload
  refused for good is aborted. A version that never finishes holds its target - one
  sent in one request starts again from its first byte at each attempt. `COMPLETED` once the other deployment
  took it; `FAILED` for a version of a customer's key (`sse_c`, nothing read) and one
  the other deployment refuses for itself (`rejected`, a 400 or 413); every other
  failure holds the target and reports nothing. **Later changes** (`tags`,
  `retention`, `legalhold`; PLAN 8.1.5) are carried as the version has them now, and
  only to the version this deployment sent - the version there is asked for first, and
  one not there or with other bytes is `FAILED` with nothing sent; one the other
  deployment refuses - Object Lock's `403 ObjectLocked` - is `FAILED` and the target
  goes on; one it takes reports no status; a retention that ends within 15 minutes
  (`object.RetentionMargin`, the rule of a copy) is passed. A row of a version that is gone, of a bucket the target no
  longer admits or whose configuration no longer names it, is passed with no status.
  The bytes of every version being sent are held together to `--replication-rate-mib`,
  with no burst above it. **One send to an object there at a time**, whoever sends, by
  the name `ObjectOf` gives the object (the target, the bucket there, the key): a row of
  the outbox waits for another row, and ends a resync's send to the object rather than
  wait for it - but waits, within its attempt's deadline, for a version whose send it has
  ended `app.ResyncPreemptions` (3) times, so a hot key cannot starve an old version; a
  resync waits for nobody (below).
- **The resync** (`app.ResyncWorker`; ADR-0026, "Resync, as built"; PLAN 8.1.6 and its
  review). `ReplicationService.Resync` (`admin:ResyncReplication`, audited) stores the
  start of the walk through Raft (`replication.resync.put`: running, no cursor); the
  worker, on the node that leads and nowhere else - a lead lost ends what is in flight
  at once - gives every running resync one pass a round: 500 versions from its cursor in
  the order of the version index (keys in order, the versions of a key from the newest),
  each sent through **the same replicator** (`Replicator.Resend`: its reads, classes,
  probe, locks, uploads that go on, rate and deadlines), which asks the other deployment
  first - a delete marker too - and sends only what is not there. A version is sent when
  a rule matches it; a marker only when the deciding rule copies markers and the state
  gave it a status when it was made; a replica never; **a `FAILED` version never** - a
  resync neither sends it nor lifts its status. After each pass the statuses, each read
  again just before (one that became `FAILED` meanwhile is left out), are acknowledged by
  version (`outbox.ack`, `{b, k, vid, status}`), then the cursor is stored - not
  proposed again when unchanged; a new leader goes on from it and what it walks again it
  finds there. A failure that holds the target holds the walk at the version (stored up
  to it, tried again after the pauses of delivery); one of the version passes it; an
  object the outbox is sending (`ErrResyncObjectBusy`) or a configuration that stops
  sending the bucket to the target (`ErrResyncConfiguration`) stops the pass at the
  version with no failure counted, and the walk comes back to it. A pass with fewer than
  500 versions ends the walk; at the end of the round, once for each target, the cursor is
  read, and **a walk whose target lost rows since it began begins again** - with no bound:
  a resync never ends while its target keeps losing rows. Otherwise it is `done`, unless the target
  needs a resync (`resync_needed`, which one `done` clears for every bucket sent there):
  then the worker starts the resync of every other bucket the target admits whose
  configuration names it (audited as the deployment's own), and stores `done` for them
  all only once every one has been through and no row was lost since the earliest began.
  A round reads the record of every bucket a replicating target admits every 10 s
  (`ResyncScanEvery`) and only the running ones between, none while no bucket has a
  replication configuration (`Target.Refs`), and what it costs grows with the buckets of
  a target, not their square; a round that moved nothing proposes nothing and is followed by a
  pause. How a walk stands beyond its record - the state and class of its own sending,
  or why it waits (`configuration`, `target`, `object`, `others`) - is the leader's
  memory (`ResyncWorker.Progress`); it is not part of a bucket's status, which is the
  same on every node (below) - but for a walk held by a failure, which shows as its
  target's state - and is shown by the metrics of replication instead:
  `cactusfs_replication_resyncs{state}` (`ResyncWorker.Summary`) and
  `cactusfs_replication_resync_events_total{event}` (`ResyncWorkerConfig.OnEvent`: held,
  preempted, restarted).
- **The status of a bucket's replication** (ADR-0026, "Status"; PLAN 8.1.7).
  `GET /api/v1/buckets/{bucket}/replication/status` (`ReplicationService.Status`,
  `admin:GetReplicationStatus` on the bucket, which `cluster-admin` and `auditor` hold
  and a `bucket-admin` does not) is answered by any node, each part from where it is
  true: the configuration, the record of the resync and the cursors of the targets
  (`lost`, `resync_needed`), each a consistent read of the metadata; then `pending` and
  `failed` from the bucket's usage record (`replication_pending`, `replication_failed`;
  0 while it is first counted), read from the node's own copy after those consistent
  reads, so never older than they are; then `target_state`, `last_error_class` and
  `oldest_pending_at` of the target, from the node that leads
  (`ports.DeliveryStatusReader`, the question `GET /targets/{name}/status` asks, within
  5 s). The resync is the stored record only (`state`, `scanned`, `sent`): the leader's
  memory of a walk would differ from one leader to the next and a follower cannot read
  it, and the contract says the resync is the same whichever node answers. A resync held
  by a failure shows as its target's state and class all the same: the delivery worker
  shows a target no better than its worst held resync (`DeliveryWorkerConfig.Resyncs`,
  `ResyncWorker.ResyncHeld`) until the resync's next attempt succeeds - in both status
  endpoints and in `cactusfs_delivery_targets`. 404 `NoSuchEntity` without a configuration; 503
  without a leader to ask, or one that did not answer in time or does not know the
  target yet; a leader of a release that cannot be asked makes the state `unsupported`.
- **The audit trail through a target** (ADR-0025; PLAN 8.2.8). A webhook or NATS
  target defined with `audit: true` is sent every record of the audit trail from the
  one after its cursor's `audit_acked` on - from the first record of the trail for a
  target defined so - by a second loop of the worker beside the target's loop of the
  outbox, through the same publisher (`app.AuditExporter`, wired in
  `cmd/cactusfs/node.go` with the node's decorated `ports.AuditLog`). Each record is the
  message `cactusfs.audit.exported.v1` with the ID `audit-<sequence>`, whose body is the
  record's line (`audit.Record.Line` in `internal/domain/audit`) - the bytes the
  exporter to the node's own output writes, `previous_hash` and `hash` included, so a
  receiver that holds every sequence verifies the chain (`audit.ParseLine`,
  `audit.Verify`). In sequence, at least once: the loop reads 100 records at a time,
  sends them in order and moves `audit_acked` through Raft (`outbox.ack` with no
  place in the outbox) after 100 or once one has waited 1 s, so a new leader sends again
  at most one batch - the export sends one record at a time, in no wave (as amended
  2026-11-16, at the review of PLAN 8.1.4, for the rows of the outbox); a receiver deduplicates by the ID. **No record is passed:** a
  failure of any class - a webhook's 4xx, which a notification passes, included - holds
  the export at the record, tried again after `delivery.Backoff`, so that no receiver
  holds a trail with a gap it was not told of; the target is shown retrying, then
  stalled. The export is part of the target's status: what it has read and not
  acknowledged is pending, dated when it was read (so sending an old trail from its
  start is no stall), and the worse state of the two loops is shown with its class.
  A change of a target takes effect at the next refresh (5 s): a target that stops
  being sent the trail stops then and keeps its cursor, and sent it again goes on from
  there; one deleted and defined again under its name is another, which the old loop
  recognises by its time of definition - also read from the store before each
  acknowledgement, so the new cursor is never moved by it. **Who may:** defining a
  target sent the trail, or changing one that is, needs `admin:GetAuditLog` on the trail
  (`arn:cactusfs:admin:::audit`) as well as `admin:PutTarget` (`app.TargetService.Put`),
  so a holder of `admin:PutTarget` who may not read the trail cannot have it sent to an
  address of theirs; deleting one needs `admin:DeleteTarget` alone. The exporter to the
  node's output (`--audit-export`) is a consumer of its own and unchanged.
- **Deadlines.** Every call to the metadata carries 30 s; every attempt at an item
  carries 10 s + 2 x `--delivery-timeout` (the connect and the handshake, the answer's
  headers, and as long again for the rest), and an attempt at a version to send to
  another deployment that and a day, or the time its size takes at 64 KiB/s when that
  is longer - or at `--replication-rate-mib` shared by every version that may be sent
  at once, when that is slower still (`app.ReplicationAttemptTimeout`, through
  `DeliveryWorkerConfig.AttemptTimeouts`): generous, because a version sent in one
  request cannot go on from where it was cut off. What ends a transfer that stops moving
  is the egress client, whose connections may make no progress for longer than
  `--delivery-timeout`; a lead that is lost ends the attempt at once.
- **What is told.** Of a failure, its class and nothing else: the log line names the
  target, its kind, the class, the failures in a row and the pause - never the words of
  the error, which may name an endpoint, whose path may be a token. How a target stands
  is kept in memory since the node came to lead: `ok`, `retrying`, or `stalled` (a class
  somebody has to act on - `auth`, `tls`, `redirect`, `egress_refused`, `no_stream`,
  `no_such_bucket`, `versioning`, `not_replica`, `quota`, `object_lock` - or 10 failures
  in a row), pending,
  the oldest pending, the last success, the last class, rejected, failed; `lost` and
  `resync_needed` come from the cursor. **Pending** is the items in hand (read, not
  acknowledged) and those found after them by a count that reads at most 10 x 1 000
  items, at most every 10 s, while the target is behind: a target further behind shows
  that bound.
- **Status from any node.** `GET /api/v1/targets/{name}/status` (`admin:GetTargetStatus`)
  is answered by the node that leads from its worker and by any other through
  `NodeService.DeliveryStatus` over mutual TLS (`ports.DeliveryStatusReader`, port label
  `delivery_status`). A leader of a release without the call (UNIMPLEMENTED) is shown as
  `unsupported` with figures of 0; no leader, one that stepped down or one that does not
  answer in 5 s is 503. A word where a state or a class belongs that cannot be one is
  not shown, on both sides of the wire.
- **Metrics.** `cactusfs_delivery_attempts_total{kind,result}` (sum over the nodes),
  `cactusfs_delivery_targets{kind,state}`, `cactusfs_delivery_oldest_pending_seconds{kind}`,
  `cactusfs_outbox_rows` and `cactusfs_outbox_dropped_total` (the leader's, absent
  elsewhere: aggregate with `max`), and at the egress boundary
  `cactusfs_egress_refusals_total{class}` (`telemetry.ObserveEgress` for requests and
  `telemetry.InstrumentEgressDial` for the connections of a NATS publisher, which the
  publishers of 8B and the client of another deployment of 8C are wired through). No metric names
  a target. Alerts `CactusFSDeliveryStalled` (the oldest row waiting for a webhook or a
  NATS target over 15 min), `CactusFSReplicationLag` (the same for another deployment,
  from `cactusfs_replication_lag_seconds`, or the old series under a leader of an older
  release; PLAN 8.1.7), `CactusFSReplicationResyncHeld` (a resync held for 15 min) and
  `CactusFSOutboxDropping` are in `deploy/observability`. Replication adds `cactusfs_replication_bytes_total` (counted
  by `telemetry.InstrumentSiteClient` as the client reads each body),
  `cactusfs_replication_versions_total{result}` (`ReplicatorConfig.OnVersion`), the
  gauges of lag and resyncs of the leader and the events of resyncs - none names a bucket,
  a key or a target.
- **Overflow** (ADR-0025; PLAN 8.2.7). A target that takes nothing never refuses or
  holds up a write: at the bound the outbox drops its oldest row, counted in
  `outboxdropped` and in the `lost` of each target that had not passed it. The worker
  compares the counts it reads every 5 s with those it last told of and - at most once a
  minute on a node (`app.OutboxReportEvery`), plus one when its lead ends - writes
  through the auditor an audit record: actor `system`, action `system:OutboxDropped`,
  resource `system/outbox`, outcome `failed`, with what was dropped since (`dropped`),
  the count (`dropped_total`), the rows held, `since`, `targets` (`name=n`, those that
  lost most first, within 512 bytes) and `targets_more` (how many were left out); it
  logs the same; no endpoint. What is dropped within the minute is told, added up, when
  it has passed, or when the node stops leading or stops - then at once: the next
  leader begins its counting with its first read, and would not tell it. The node's
  auditor runs until its other background tasks have returned, so that last record is
  written before the node closes. What was dropped before a node came to lead was the
  former leader's to tell, and the count is in every record.

## 6. APIs **(implemented: the S3 API with the depth of milestone 6, bucket notifications and replication, the management API with the policy builder's catalogue, validation and explanations, node RPC and events with NATS and webhook delivery to targets)**

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
- **Replica requests (implemented at the destination, ADR-0026, PLAN 8.1.1).** A bucket
  made a replica of another deployment (`PUT /api/v1/buckets/{bucket}/replica`,
  `admin:PutBucketReplicaMode`, audited in the entry) takes new versions and delete
  markers from that deployment and from nobody else - the state refuses everything else
  with `409 InvalidBucketState`, the root identity's writes too. A copy is an ordinary
  S3 request with `x-cactusfs-replica-{source,version-id,seq,mtime,etag}`: PutObject and
  CompleteMultipartUpload carry all five, CreateMultipartUpload the source alone,
  DeleteObject without `versionId` all but the entity tag; no other request but the
  questions and later changes below takes them (`400 ReplicaHeaderNotTaken`, a code of
  its own that the source holds on rather than fails a change for; never passed over). The adapter (`s3api/replica.go`) refuses,
  before the body is read, a replica header the signature does not cover and a payload
  it does not cover (its SHA-256, or signed chunks; never `UNSIGNED-PAYLOAD`, a presigned
  URL or a digest beside an unsigned payload) with `403 AccessDenied`, each audited as a
  failed authentication; a header missing, twice or unreadable is `400`. The use case
  (`app.ReplicaWrite`) asks the authorizer for `s3:ReplicateObject` (`s3:ReplicateDelete`
  for a marker) - granted by no wildcard - instead of the ordinary action, checks the
  bucket is the replica of that source before a shard is written, commits the version
  with its source's ID, time and sequence, refuses a content whose entity tag is not the
  one named (`400 BadDigest`) and an SSE-KMS or SSE-C request, and writes each copy it
  committed into the audit trail. A part of a replica upload, its abort and the listing
  of its parts need `s3:ReplicateObject`, and the ordinary actions do not do; a part of
  one needs a signed payload and is never an UploadPartCopy; an ordinary upload begun
  before its bucket became a replica takes no more parts (`409`); a second copy of a
  version is `409`. GET and HEAD answer `x-amz-replication-status` from the stored status.
  The questions the source asks before it sends (ADR-0026, "What the source asks"; PLAN
  8.1.1a) carry the signed source alone and are judged as `s3:ReplicateObject`, refused
  as a copy is unless the bucket is the replica of exactly that source
  (`ObjectService.ReplicaBucket`, `ReplicaVersion`, `ListReplicaUploads`,
  `AbortReplicaUpload`): HEAD of the bucket and `GET ?versioning` answer the versioning
  and the source in `x-cactusfs-replica-source`; HEAD of a version named by `versionId`
  its `ETag` and no other header of it (404 not there, 405 a delete marker; a 409 of
  these HEADs is only ever a bucket that is no replica, any other conflict 503); `GET
  ?uploads&prefix=<key>` only the uploads of that key the source began, the caller as
  their initiator and owner; `DELETE
  ?uploadId` only such an upload (any other is `404 NoSuchUpload` to the source). The
  later changes of a version (`?tagging`, `?retention`, `?legal-hold` with `versionId`)
  may carry the source too: then they are taken only in that source's replica, never
  with a bypass, and a refusal by Object Lock of any request that carried replica
  headers is `403 ObjectLocked` instead of `AccessDenied`, so that the source fails that
  change alone rather than stall the target.
- **Replication configuration (implemented at the source, ADR-0026, PLAN 8.1.3).**
  `PUT`, `GET` and `DELETE /{bucket}?replication` (`PutBucketReplication`,
  `GetBucketReplication`, `DeleteBucketReplication`; `s3:PutReplicationConfiguration`
  for the put and the delete, `s3:GetReplicationConfiguration` for the read) over
  `app.ReplicationService.SetConfiguration`, `Configuration` and `DeleteConfiguration`,
  which authorize, judge and audit; the adapter (`s3api/replication.go`) only reads the
  document, with the strict decoder of every configuration (`decodeShapedXML`). The
  `<Role>` is `arn:cactusfs:replication:::<target>` and names the target; every rule's
  `<Destination><Bucket>` is `arn:aws:s3:::<bucket>`, the same bucket for every rule
  (`501` otherwise); a rule has `ID` (one is given, `rule-<n>`, when left out),
  `Status`, `Priority`, a prefix as `<Prefix>` (the first form of the schema) or
  `<Filter><Prefix>` (the later form) - both is `400`, neither, or rules of both forms
  in one document, `400 MalformedXML` - and `DeleteMarkerReplication`, which a rule of
  the later form must give (`MalformedXML` otherwise) and one of the first form that
  leaves it out copies no markers; two enabled rules of one priority whose prefixes
  overlap are `400 InvalidArgument` (the use case's rule, not the applied format's). Filters on tags or
  of several conditions, `SourceSelectionCriteria`, `ExistingObjectReplication` and
  every element of the destination but its bucket answer `501 NotImplemented`; another
  role or destination form, a priority that is not decimal digits from 0 to 2^31 - 1,
  an ID that is there and empty, and a target that is not there, is not of kind `s3` or
  does not admit the bucket (one sentence for the three) answer `400 InvalidArgument`;
  a bucket whose versioning is not `Enabled` `400 InvalidRequest`
  (`domain.ErrVersioningRequired`); a replica bucket `409 InvalidBucketState`; GET of
  none `404 ReplicationConfigurationNotFoundError`; anything else that is not the
  document `400 MalformedXML`. The store judges the bucket and the target again when
  the change is committed, and the applied entry is told as
  `bucket.configuration-changed`. GET writes every rule in the later form of the
  schema, with its filter, priority and delete markers. What a rule matches is sent by
  the replicator (PLAN 8.1.4; "Delivery to targets" in section 5), **released together
  with this** - a configuration makes rows for `r:<target>` that only the replicator
  delivers, and without it the target's cursor would hold, the outbox would stop being
  trimmed for every target, and the matched versions would stay `PENDING`. Later
  changes of a version are carried by the same replicator (PLAN 8.1.5). **A bucket
  there takes the versions of one bucket here:** a configuration that names a target
  and a bucket there that another bucket's configuration names already is `400
  InvalidArgument`, in the one sentence of every refusal of a target (the use case's
  rule, `ReplicationService.refuseShared`; two put at the same moment may both pass it -
  PLAN, Decisions "8.1.4 review"; no index of the named pairs, which would be a schema
  change - "8.1.4 re-review").
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
  (with the replication status of the current version, `replication_status`, when a rule
  is about it; PLAN 8.1.10, ADR-0026 open 17),
  `/buckets/{bucket}/object/access` (who uploaded and who downloaded) and
  `/buckets/{bucket}/object/preview` (an object's bytes for the console to show, below); `/search`;
  `/users`, `/users/{name}`, `/users/{name}/keys` and `/keys/{access_key_id}`; `/groups`,
  `/roles` and `/policies`, each with `/{name}`; `/policy-catalogue`,
  `/policy-validation`, `/buckets/{bucket}/policy/validation`,
  `/buckets/{bucket}/subbuckets/{name}/policy/validation` and `/access/explain` (the
  policy builder, below); `/cluster` and `/cluster/maintenance`;
  `/settings`; `/audit`. The paths of milestone 8 are in the contract; those of quotas
  (`/buckets/{bucket}/quota`, `/buckets/{bucket}/usage/recount`; PLAN 8A, "Usage and
  quotas" in section 5), of targets (`/targets`, `/targets/{name}`; PLAN 8.0.13) and
  of a bucket's notification rules (`/buckets/{bucket}/notifications`,
  `app.NotificationService`; PLAN 8.2.2) are implemented and answer 503 below metadata
  level 5; so is a bucket's replica mode (`/buckets/{bucket}/replica`,
  `app.ReplicationService`; PLAN 8.1.1; 409 `InvalidBucketState` for a bucket that keeps
  no versions or is replicated itself, 409 `BucketNotEmpty` for one that holds a
  version); so is a bucket's replication configuration (`/buckets/{bucket}/replication`,
  the same use case; PLAN 8.1.3; 400 for a bucket whose versioning is not enabled or a
  rule that leaves out `id`, `enabled`, `priority` or `delete_markers`, 409
  `InvalidBucketState` for a replica, 404 `NoSuchEntity` for a bucket without one);
  `/targets/{name}/status` (PLAN 8.0.14) is answered by any node, which asks
  the node that leads ("Delivery to targets", section 5); `POST
  /buckets/{bucket}/replication/resync` (PLAN 8.1.6) stores the start of a resync through
  Raft on any node and answers 202 with the record (`ReplicationService.Resync`,
  `admin:ResyncReplication`, audited; 404 `NoSuchEntity` for a bucket without a
  configuration), and the leader walks it; and `GET /buckets/{bucket}/replication/status`
  (PLAN 8.1.7) is answered by any node (`ReplicationService.Status`,
  `admin:GetReplicationStatus`; "The status of a bucket's replication", section 5). An
  operation of the contract whose use case is not written answers 501 `NotImplemented`
  to a caller that has authenticated; since PLAN 8.1.7 there is none. A bucket's policy (`/buckets/{bucket}/policy`, the document
  S3's `PutBucketPolicy` sets, through the same `BucketService.SetPolicy`; one it cannot
  keep is 400 `InvalidPolicy`) and sub-buckets (`/buckets/{bucket}/subbuckets[/{name}]`,
  `app.SubBucketService`; 404 `NoSuchSubBucket`, 409 `TooManySubBuckets` and
  `SubBucketNotEmpty`) are served (PLAN C.6): the use cases authorize and audit, the
  handlers translate. A node without the use case of sub-buckets answers their paths
  `501`. The paths of tags (ADR-0018) are served (PLAN D.4): a bucket's tags
  (`/buckets/{bucket}/tags`), an object's (`/buckets/{bucket}/object/tags`, `key` and
  `version_id` in the query), a bucket's tag rules (`/buckets/{bucket}/tag-rules`;
  `s3:GetBucketTagRules`, `s3:PutBucketTagRules`) and the catalogue (`/tag-definitions`
  and `/tag-definitions/{key}`; `admin:ListTagDefinitions`,
  `admin:{Get,Put,Delete}TagDefinition`, `app.TagDefinitionService`, 501 on a node not
  given it), and an upload with tags (`X-CactusFS-Tagging`, read by
  `object.ParseTagging` as S3's `x-amz-tagging` is, and part of the upload's
  idempotency fingerprint). Tags cross as lists in the order given, a key given twice
  refused by the domain (400 `InvalidArgument`), and are answered in key order. A tag a
  bucket's rules refuse is 400 `TagRuleViolated`; a definition a rule names is 409
  `EntityInUse`, one defined already 409 `EntityAlreadyExists`, one not defined 404
  `NoSuchEntity`. A bucket made through the API (`BucketService.CreateLowercasing`) is
  given the tag rule `lowercase`: two entries. When the second fails the bucket is
  deleted again if it is still only what was made (its record, no configuration, no
  sub-bucket, no upload: read through `WithUndo`), and the error is answered; when it
  cannot be, it is left without the rule, answered 201, audited `tag_rules=not_set` and
  logged at ERROR; one deleted by another request in the gap is 409 `Conflict`
  (ADR-0018). The 201 is the bucket as written, with no second read or authorization. The catalogue's create, put and delete are asked of the authorizer before an
  Idempotency-Key is reserved (`askedFirstOf`, by the path value or body field `key`).
  CORS and lifecycle rules are still set through the S3 API.
  The contract is served at `/api/v1/openapi.json` and
  `.yaml`, and Swagger UI at `/docs` from embedded assets under a Content-Security-Policy
  that allows nothing from another host.
- **Search (implemented).** `GET /api/v1/search?q=` and the field
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
  a search of one named bucket stays possible - and nothing keeps a deployment from
  growing past it: a quota (ADR-0024) limits the bytes of one bucket, not the number of
  buckets or the size of their policies. A failure to read a page, damage to
  the store included, fails the search (503); one damaged policy leaves out its bucket
  alone. The walk is then
  given only buckets the caller may see, so `examined`, `complete`, the number of
  pages and every token speak of those alone. A bound that stops a request answers
  `complete: false` with `next_continuation`: base64url of
  `{v, q: SHA-256 of the query, b: bucket, k: last key examined, n: bucket name done}`.
  The token is validated - `b` must be a valid bucket name - and not signed: it is
  looked at only after every bucket was decided, among those the caller may see, so a
  made-up token can only skip ahead in what that caller would have been shown.
  A bucket the caller may not list is narrowed to the sub-buckets they may list
  (`Allowed`, no audit record), read from the node's own copy of the state; only when
  they may see nothing of a bucket they named is the audited refusal made.
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
- **Previews of objects (implemented, ADR-0028).** The console shows an image (PNG,
  JPEG, GIF, WebP), a PDF, a DOCX, an XLSX, or text, CSV and JSON, without the object
  ever being rendered by the browser on the console's origin, and without anything of
  it being parsed or rendered on the server beyond what decides its kind and size. The
  flow:

  ```
  console (ObjectsScreen: "Preview" in an object's opened row)
    | GET /api/v1/buckets/{b}/object/preview?key=   (generated client, read as a stream)
    v
  adminapi: authenticate -> ObjectService.Preview -> 200 octet-stream attachment
            (each write taken within 2 min)           or problem+json
    v
  app.ObjectService.Preview                           internal/app/preview.go
    1 Authorize s3:GetObject (s3:GetObjectVersion), resolve the version
    2 Authorize admin:PreviewObject, ObjectCreator = version.CreatedBy
    3 SSE-C -> 409; preview.Expected(stored type) -> 415; size > cap -> 413
                                              (no shard read yet)
    4 take a place: 2 per caller, 16 per node, else 429
    5 open exactly that version (as a download: decrypt, verify, decode)
    6 first 4 KiB -> preview.Classify(stored type, bytes) -> kind, or 415
      image: up to 256 KiB -> preview.ImageBounds -> 413 / 415
      GIF: every byte through preview.GIFFrames (fails the body at a bad frame)
      text kinds: the first 1 MiB, Truncated
    7 Close of the body: place given back, counted as a download (ADR-0016)
    v
  console PreviewDialog: renderer by X-CactusFS-Preview-Kind only; <= 256 MiB, one Blob
    image      -> Blob re-typed image/png|jpeg|gif|webp -> blob: address in an <img>
    text, json -> text nodes;  csv -> our RFC 4180 reader -> a table (1 000 x 100)
    pdf        -> pdf.js 6.4 in a module worker of the console's origin -> a canvas
    docx, xlsx -> ooxml.worker (our zip reader + fflate Inflate, our XML reader,
                  bounded, ended after 10 s) -> plain model -> ooxml-check -> React
  ```

  **Who may** is decided by the one `ports.Authorizer`: `s3:GetObject` (or
  `s3:GetObjectVersion`) on the object - a preview is never more than a download - and
  either `admin:PreviewObject` on it or ownership of the version shown.
  `iam.Request.ObjectCreator`, set by the use case from the version it resolved and
  never from the request, lets `iam.Decide` count the policies' silence as an Allow
  when it is `user/<the caller>`, for that one action. An explicit Deny refuses the
  owner; an API key's inline policy must allow both actions; a locked session previews
  nothing; root may; an anonymous caller never reaches the management API. Built-in
  roles: `bucket-admin`, `read-write` and `read-only` hold the action, `cluster-admin`
  through `admin:*`, `auditor` not. A refusal of either action is audited like every
  denial; an allowed preview is not, as a download is not; a 409, 413 or 415 is no
  denial. **What kind** is decided from the stored type AND the bytes' signature, never
  from the key: SVG, HTML, XML, script and every type not listed are never previewable,
  and `application/octet-stream` or no type lets the bytes decide for images and PDF
  only. **The answer** is always `application/octet-stream` with
  `Content-Disposition: attachment`, `nosniff`,
  `Content-Security-Policy: sandbox; default-src 'none'`,
  `Cross-Origin-Resource-Policy: same-origin` and `no-store`, and
  `X-CactusFS-Preview-Kind` and `X-CactusFS-Preview-Truncated`; no `Range`; `HEAD` is
  405. A transfer cut off after it began (a GIF frame past its screen, a stalled client)
  is ended short of its `Content-Length` and logged at Warn; the browser's fetch fails
  the broken body, and the dialog shows the preview as cut off. **The console** draws what
  it is given itself - React elements, text nodes, an `<img>` of a blob address, a
  canvas - never markup, a frame or a navigation; its CSP gains only `img-src blob:` and
  an explicit `worker-src 'self'`, and an eslint rule refuses the markup sinks in its
  code. It previews the current version; the API also takes `version_id`. The cap is
  the setting `--preview-max-size` (section 3). No entry, state, on-disk format or
  crypto design changed: `ObjectCreator` lives in memory and `CreatedBy` exists since
  entry schema 3, so a version from before has no owner and is previewed by assignment
  only. A node of the release before answers the path 404 `NotFound`. The threats and
  their tests are in the threat model ("Previews of objects").
- **The policy builder: the catalogue, validation and "Who may do this?" (implemented,
  ADR-0032; PLAN 9.1 to 9.10).** The console builds policies by choosing, and asks the
  node every question a builder needs answered, so that nothing about the policy
  language is known or reckoned in React. No entry, command, stored record, on-disk
  format or crypto design changed for it.
  - **The catalogue is the one source** of what a policy can say:
    `internal/domain/iam/catalogue.go` - `Catalogue()` (every `s3:` and `admin:` action a
    policy can grant, and nothing else: no `cluster:` action and no `auth:` or `system:`
    name of the audit trail; each with its group, a sentence, the resource templates it
    is asked on and five flags), `ConditionKeys()`, `ResourceKinds()`, `PolicyLimits()`
    and `BucketPolicyPrincipals()`. The flags `by_name_only`, `in_bucket_policy`,
    `anonymous` and `in_sub_bucket_policy` are the predicates `Decide` and the parsers use,
    computed and not restated. Templates have four placeholders (`iam.FitsTemplate`):
    `{bucket}` and `{name}` hold one or more characters without `/`, `*` or `?` - a
    name is one bucket or entity, never a pattern; `{tag_key}` the same with `/`
    allowed, as a tag key may hold one; `{key}` any object key as it is. Tests tie it to
    the code both ways: `TestCatalogueHoldsEveryAction` (go/parser: every action constant
    is in it, nothing else is), `TestCatalogueAgreesWithDecide`,
    `TestConditionKeysAgreeWithTheParser`, `TestUseCasesAskWhatTheCatalogueSays` (every
    question every use case asks the authorizer fits a template of its action, and every
    action is asked - `admin:GetMetrics` by the telemetry adapter, held by its own test)
    and `TestContractActionsAreInTheCatalogue` (every
    `x-cactusfs-action` of the contract). It is a property of the binary: two nodes of
    different releases may answer two catalogues, and each validates by its own.
    `PolicyToolsService.Catalogue` serves it (`admin:GetPolicyCatalogue` on
    `policy-catalogue`; `bucket-admin` names it, `auditor` and `cluster-admin` hold it by
    wildcard).
  - **Validation authorizes by the put's own action and stores nothing.**
    `PolicyToolsService.ValidateIdentity`, `ValidateBucket` and `ValidateSubBucket`
    (`internal/app/policytools.go`) authorize exactly as the put that would store the
    document: `admin:PutPolicy` on `policy/<name>` (or `policy`); with `key_owner`,
    `admin:CreateAPIKey` on `user/<owner>`, after refusing - and auditing - a principal
    bound by its key's inline policy, as `CreateAPIKey` does; `s3:PutBucketPolicy` and
    `s3:PutSubBucket` on the bucket. They read nothing beyond what that authorization
    reads (the authorizer reads the bucket's policy), so they say nothing a put would
    not; a bucket or sub-bucket that is not there is no error. They run the parsers the
    puts run, which collect every fault as `iam.PolicyFault` - an RFC 6901 pointer into
    the document as sent, its statement, a reason - at most 100, in an error
    (`iam.PolicyError`) that wraps the sentinels of before and reads as the first fault,
    word for word (`FuzzPolicyFaults`: accepted exactly when no fault, for all three
    parsers). A valid document also gets the analysis (`iam.AnalysePolicy`,
    `AnalyseBucketPolicy`, `AnalyseSubBucketPolicy`): per statement, the catalogue's
    actions its patterns cover (a by-name-only action never by an Allow's wildcard),
    whether it is open to anyone, which actions a caller without credentials could be
    let in to and which it names for anyone that are never anonymous, before conditions,
    and the patterns that cover no action of this node's catalogue. The analysis
    classifies and decides nothing (`TestAnalysisAgreesWithDecide`). The four puts give
    the same faults with their 400, in `Problem.faults`, through `app.PolicyFaults` -
    never from an error that wraps `ErrIntegrity`. The three POSTs take
    `Idempotency-Key` and ignore it, need the CSRF token, and refuse a repeated policy
    field (`refuseRepeatedPolicy`).
  - **The explanation path.** The one evaluation is the unexported `iam.evaluate`;
    `Decide`, `Evaluate` and `Explain(req)` are its three views
    (`FuzzExplainAgreesWithDecide`; `TestDecideAllocations` holds `Decide`'s allocations
    to those before). It is reached through
    `ports.AccessExplainer` (`internal/ports/system.go`), **which is never a gate**: its
    one production implementation is the unexported `explainer` made by
    `app.NewExplainer(*Authorizer)` (`internal/app/explainer.go`; `porttest.Explainer`
    serves use-case tests), which calls the
    authorizer's own `decide` with an explanation to fill and logs and audits nothing.
    `app.Authorizer` has no method `Explain`, so neither it nor any `ports.Authorizer` can
    be asserted to the port. `cmd/cactusfs/node.go` makes one, in one place, for
    `ExplainService` - the only holder. `TestOnlyTheExplainUseCaseHoldsAnExplainer`
    (go/types over the source of `internal/app`, `internal/adapters` and `cmd/cactusfs`)
    refuses any other reference to the port, to `app.NewExplainer`, to `decide` or to a
    call of `Explain`; `TestTheExplainerRuleSeesAHolder` plants the escapes it must catch.
    `ExplainService.Explain` (`internal/app/explain.go`): checks the request's shape;
    resolves an access key first, so that it authorizes `admin:ExplainAccess` on
    `user/<owner>` (`user` for a key not there, as `DeleteAPIKey` does), `user/<name>` or
    `anonymous`; reads the action and resource against the catalogue (a catalogue name
    without a wildcard; a resource of one of its templates; a 400 here is recorded);
    then **the probing
    defence** - an answer about an `s3:` action on a bucket also needs
    `s3:GetBucketPolicy` on it, and one whose key or listing prefix could lie in a
    sub-bucket `s3:GetSubBucketPolicy`, asked with `Allowed` whether or not such a
    sub-bucket is registered; a "no", or a policy that cannot be read (`ErrIntegrity`),
    is a 403 audited as a refusal of `admin:ExplainAccess`; resolves the subject (404
    `NoSuchEntity`; a disabled user or an expired key "would not authenticate", no
    decision); asks the explainer at the node's time; **filters the citations** - a cited
    statement keeps its policy name, indexes and Sid only where the caller is `Allowed`
    `admin:GetPolicy`, `s3:GetBucketPolicy`, `s3:GetSubBucketPolicy` or
    `admin:ListAPIKeys` on the key's owner, otherwise it is "a policy you may not read"
    with its source and effect; and **records the answer synchronously** - one record
    (subject, action, resource, the answer; never a document) through
    `ports.AuditLog.AppendAudit`, not the background queue of `Auditor.Succeeded`, on a
    context detached from the request's (`context.WithoutCancel`, bounded by
    `auditWriteTimeout`): a question the node has answered is recorded even if its
    caller has gone (`TestAnExplanationIsRecordedWhenItsCallerGoes`); one that cannot be
    decided (store unavailable, caller gone before the decision) gets neither an answer
    nor a record. The 400 of the request's reading and the 404 of the subject are
    recorded the same way; a request refused as malformed before the gate (no subject or
    two, a name or key ID that cannot be one, the contract's binding) is a 400 that is
    not. No record, no answer: 503, or 500 for `ErrOutcomeUnknown`. Every refusal carries one fixed text; nothing a store or parser
    said is returned, recorded or logged (a damaged policy is logged with the names of
    its bucket and sub-bucket only). Not considered: who made a version, so an answer
    about `admin:PreviewObject` says `object_ownership`.
  - **The console reads the catalogue lazily.** `usePolicyTools`
    (`web/src/ui/policyHooks.ts`) asks for it only on the first edit of an editable
    policy or when "Open the policy builder" is pressed - on the "Who may do this?"
    screen, on the first change of its "Does what" fields or when "Load the list of
    actions" is pressed - never as a screen is shown or a policy viewed or focused - so
    a caller without
    `admin:GetPolicyCatalogue` causes no audited refusal by looking; once per screen,
    shared on a bucket's page (`PolicyToolsContext`), waited for at most 10 s
    (`CATALOGUE_WAIT_MS`). 404 (an older node), 403 or a failure leave the JSON editor
    with a notice. The reader and writer (`web/src/policy/{read,write}.ts`) keep
    `canonical(write(read(t))) = canonical(t)`, held against a corpus the Go parsers
    write (`web/policycorpus_test.go`, `TestBuilderCorpusIsCurrent`); what the reader
    cannot take is kept as it is and the builder disabled with the reason. The JSON is
    sent as shown and an unchanged document is never sent. Validation runs as typed
    (debounced) only once a document differs from the stored one, and every save checks
    the exact bytes it sends; the bucket and sub-bucket editors check at save even when
    the builder is not there (`BucketPolicyEditor.tsx`), and before a bucket's policy that
    the analysis marks open to anyone - or that a node of an older release could not
    check - is sent, they ask. "Who may do this?" (`web/src/screens/AccessScreen.tsx`,
    `#/access`) asks `GET /access/explain` only on Explain, never aborts a request, and
    shows the answer as given.
- **Console (implemented, ADR-0007).** `web/`, embedded with `-tags console` and served
  at `/` by nodes with the console role. It calls only the admin API, through the client
  generated from the contract. Uploads belong to the console, not to a screen: they go
  on while any screen is shown, and a bar at the top of every screen shows them and the
  notices of what the console did (kept for the visit, not stored). An object's details
  open in its own row of the object browser. A bucket's details hold its policy, its
  sub-buckets, its tags, its tag rules and its notification rules, each folded and
  asked for only once opened. Every policy - on the Policies screen, a role's page, the
  form of a new API key, a bucket's and a sub-bucket's - is edited with the policy
  builder beside its JSON (`web/src/ui/PolicyBuilder.tsx`), and the **Who may do this?**
  screen asks the node for an explanation ("The policy builder", above).
  Tags (ADR-0018, PLAN D.6; `web/src/screens/{TagSections,ObjectTags,UploadTags,TagsScreen}.tsx`,
  `web/src/ui/{tags.ts,TagRows.tsx}`) are edited as rows of a key and a value and sent
  as the list typed, in its order - a key given twice and an empty key included, for
  the API to refuse; what a save is answered with (key order, lower case where the
  rules say so) is what is shown after it. An object's tags are those of its current
  version, in its opened row. The upload form has a tags field sent as
  `X-CactusFS-Tagging` (each key and value percent-encoded); once files are chosen it
  reads the bucket's tag rules, says what they require and begins with the keys
  required of the key typed - or says, where the rules cannot be read (403 or a
  failure), that the server will tell; what the server refuses is said beside the
  file, and a locked session's 423 is said as such and told to the lock. The
  **Tags** screen is the catalogue: listed, defined, changed and removed after a
  question; a caller the API does not show it (403) is told so in a line. A 403 on
  reading a bucket's or an object's tags or a bucket's rules offers no editor (they
  are replaced whole, and what cannot be seen is not replaced from here). The keys and
  values of the catalogue and of the rules are offered while a tag is typed, never
  required. The **Targets** screen (PLAN 8.2.9; `web/src/screens/TargetsScreen.tsx`)
  lists every target with its kind, the origin of its endpoint (the API never gives the
  path), what it is sent, and - asked of `/targets/{name}/status` for each - its state,
  what waits (with what was lost or rejected) and the class of its last failure; it
  defines, changes and deletes targets. Secret fields are write-only: a password field
  that an answer never fills, sent only when typed (a change without it keeps the
  secret), cleared once sent; the endpoint likewise. A webhook's signing key, in the
  answer that made it, is shown once with how a receiver verifies with it, and held in
  the screen's state until the reader says it was copied. A bucket's details hold its
  notification rules too (`web/src/screens/BucketNotifications.tsx`): listed in order,
  added and removed by sending the whole list (`PUT`), all removed by `DELETE`. Which
  target a bucket may name, and who may do any of it, is the API's to say: a 403 to a
  list is a line, to a change the API's words. A bucket's details hold its replication
  too (PLAN 8.1.10; `web/src/screens/BucketReplication.tsx`): the target, the bucket
  there and the rules as the API keeps them, and the status - the target's state with
  the console's words for its class, pending (or "not counted yet" while the bucket's
  usage is not exact), failed, lost, the stored record of the resync and
  `resync_needed` - with **Resync** behind a question; and its replica mode with
  **Promote**, enabled only once the bucket's name is typed exactly, in a dialog that
  says what promotion does. The configuration and making a bucket a replica are shown,
  not edited. The object browser shows the replication status of an object's current
  version, from `replication_status` of `/object/metadata`; the Targets screen names
  the classes of another deployment.
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
  `ports.EventPublisher`. **Implemented:** the contract, with twenty-two channels -
  twenty events published inside the process, and two messages that leave it for
  targets only. Inside the process (the in-process publisher, at most once per node,
  counted in `cactusfs_events_total`; consumers dedupe by event ID):

  | Since | Events |
  |---|---|
  | milestones 1 to 7 (sixteen) | `bucket.created`, `bucket.deleted`, `object.created`, `object.deleted`, `bucket.versioning-changed`, `object.delete-marker-created`, `object.retention-changed`, `object.legal-hold-changed`, `bucket.object-lock-changed`, `bucket.configuration-changed` (a policy, CORS, lifecycle rules, tags, tag rules, notification rules or a replication configuration - the payload names the bucket, not the document), `object.tags-changed`, `iam.root-credential-set`, `cluster.topology-changed`, `cluster.node-registered`, `cluster.node-removed`, `audit.recorded` |
  | entry schema 5 (four) | `bucket.quota-changed` (through `app.QuotaService`), `bucket.replica-mode-changed` (through `app.ReplicationService`), `cluster.target-changed` and `cluster.target-deleted` (through `app.TargetService`) - each told by the state machine as it applies the entry |
  | to targets only (two) | `cactusfs.notification.object.v1`: S3's event record of a put, a completed upload, a delete marker or a version removed for good, one record a message, sent by the notifier through the webhook and NATS publishers of the targets a bucket's rules name (PLAN 8.2.1 to 8.2.5); `cactusfs.audit.exported.v1`: the records of the audit trail, in sequence, sent by the worker through the same publishers to a target defined with `audit: true` (PLAN 8.2.8) |

  The two messages for targets are not events of the bus: they are rows of the outbox
  (and records of the trail) that the state machine appends as it applies an entry,
  delivered at least once by the node that leads, deduplicated by the receiver by the ID
  of the message (`X-Cactusfs-Event-Id`, `Nats-Msg-Id`), and signed for a webhook with
  HMAC-SHA256 under the target's signing key (ADR-0025; "Delivery to targets", section 5).
- **Node RPC.** gRPC over mTLS 1.3, defined in `api/proto/cactusfs/node/v1` and checked
  with `buf lint` and `buf breaking`. **Implemented**, three services: `NodeService`
  (`Info`, `InFlight`, `Settings`), `MetaService` for what only the metadata leader
  can do (`Propose`, `ReadIndex`, `Term`, `Join`, `Leave`), and `StorageService` for
  shards (`WriteShard`, `StatShard`, `ReadShard`, `DeleteShard`, `ListShards`). A node
  that leaves (`Leave`) is revoked through Raft (entry schema 6, ADR-0030): no member
  serves it, lets it join again or keeps its Raft connections afterwards. Only the node
  itself can ask to leave; an operator's command to remove another node is not built.
- **Sub-buckets and tag rules (implemented end to end: domain, store, use cases,
  management API, console).** ADR-0017 and
  ADR-0018 are accepted, and entry schema 3 carries their commands: the domain rules
  (`internal/domain/bucket`, `internal/domain/tagging`), the ports (`SubBucketStore`,
  `TagDefinitionStore`) and the metadata store's side of them exist and are tested.
  The authorizer reads a sub-bucket's policy: the sub-bucket of a request is the first
  segment of its key, or of a listing's prefix (only a prefix beginning `<name>/`
  counts), and its policy is read with the bucket's in one consistent read a request
  scope (`ports.BucketAccessReader`, which the node's local copy does not implement).
  A sub-bucket policy is confined (`iam.ParseSubBucketPolicy`: its own keys and the
  bucket's listings, named actions only, no governance bypass, no Allow to `*`); a
  Deny in any of the three sources refuses - an anonymous caller too - and an Allow in
  any allows, within an API key's bound. One that does not parse refuses requests in
  that sub-bucket (`ErrIntegrity`, its text never logged). `app.SubBucketService` lists
  (names and ages, never a policy; filtered to what the caller may list), gets, puts and
  deletes them, audited; the management API serves it (PLAN C.6), and the console
  shows a bucket's policy and its sub-buckets in the bucket's details, folded until
  opened (`web/src/screens/SubBuckets.tsx`, PLAN C.8, C.8b): a policy is edited as
  JSON text - and since PLAN 9.9 with the policy builder beside it
  (`web/src/screens/BucketPolicyEditor.tsx`, "The policy builder" below) - and sent as
  typed (as on the Policies screen and in the API-key form), a
  stored one shown compact when indented it would pass the 20 KiB the API measures as
  sent, the server's problem shown next to the editor, a 403 on the list said as "you
  may not see", and a 404 `NotFound` worded as a node of an older release. A Save or a
  Remove in an open sub-bucket re-creates one deleted meanwhile (a PUT creates what is
  missing; the contract has no precondition yet): the console sees a time of creation
  in the answer other than the one it showed and says that the sub-bucket was created
  again since - as seen, not as caused by the change, since another admin or a failed
  read-back gives the same sign - but cannot prevent it. A caller refused the read (403)
  is shown no time, so a re-creation is not reported to them.
  Tag rules (PLAN D.2-D.3): `BucketService.{TagRules,SetTagRules,DeleteTagRules}`
  (`s3:GetBucketTagRules`, `s3:PutBucketTagRules`, audited) and
  `app.TagDefinitionService` (`admin:ListTagDefinitions` on `tag-definitions`,
  `admin:{Get,Put,Delete}TagDefinition` on `tag-definition/<key>`; changes audited in
  the entry that makes them; a create is a read then a put, so two creates of one key
  through different gateways may both succeed; a definition a rule names is
  `domain.ErrEntityInUse`). The rules of a bucket are read once a request, consistent,
  and every way an object comes to have tags - a put, a copy (the source's tags or its
  own), a change or removal of its tags, a multipart upload when it begins and again
  when it completes - keeps its tags as they say (in lower case where they lower-case;
  a bucket's own tags too) and refuses what they do not allow (`domain.ErrTagRule`,
  naming the rule, never a value; S3's 400 `InvalidTag`). A catalogue that cannot be
  read, rules of a later release, and a node not given the catalogue refuse a write as
  `domain.ErrUnavailable` (503), never as a broken rule. Tags reach the use cases as
  lists (`object.Tag`), and a key given twice is refused in the domain
  (`object.TagMap`). Nothing sets rules or the catalogue but these use cases, served by the
  management API (PLAN D.4) and edited in the console (PLAN D.6).

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
- The policy builder adds no second authorizer **(implemented, ADR-0032; section 6, "The
  policy builder")**. The actions, condition keys and resource templates it offers are
  the domain's catalogue, tied to the action constants and to `Decide` by tests; the
  console reckons no decision, no wildcard's coverage and no "open to anyone" - the node
  answers each. Validation is authorized by the action of the put it stands for, stores
  nothing and reads nothing that put's authorization does not, so it is no oracle beyond
  the put. "Who may do this?" reaches the authorizer's `decide` through
  `ports.AccessExplainer`, which is never a gate and which only `ExplainService` holds -
  enforced by type (`TestOnlyTheExplainUseCaseHoldsAnExplainer`), and the authorizer
  cannot be asserted to it. An explanation discloses what another identity may do, so
  `admin:ExplainAccess` is held by `cluster-admin` alone among the built-in roles; an
  answer about a bucket also needs the right to read the bucket's policy (and a
  sub-bucket's, when the request could fall in one), so it cannot probe a policy its
  caller may not read, and a damaged policy is answered as that refusal; a cited
  statement is named only where its caller may read its policy; and every answer is
  recorded synchronously in the audit trail, on a context detached from the request,
  before it is given - no record, no answer. A question the node has answered is
  recorded even if its caller has gone; one that cannot be decided (store unavailable,
  caller gone before the decision) gets neither an answer nor a record.
- No default API key. A new deployment has one default console user, `admin` with the
  password `admin`, made once (ADR-0014): change it first. API key secrets are envelope-encrypted, shown once, compared
  in constant time **(implemented)**. Keys expire, can be narrowed by an inline policy
  and are revoked for every node at once.
- Hash-chained, append-only audit trail **(implemented)**: changes to identities are
  recorded in the same Raft entry as the change; refusals in the background; every
  answer of "Who may do this?" synchronously, before it is given.
  `--audit-export` writes the records to the log stream as JSON lines; a target defined
  with `audit: true` is sent the same lines, every record in sequence, at least once
  (PLAN 8.2.8; section 5, "Delivery to targets").
- Console accounts with Argon2id and replicated, short-lived sessions
  **(implemented, with the console that signs in through them)**. A session that times
  out while it uploads is locked - it may store objects and nothing else - until the
  user's password unlocks it; a changed password ends every session of the user
  **(implemented, ADR-0027; section 6)**.
  The console's "Delete all" asks for the password through the same unlock before it
  begins; each deletion is still judged by the authorizer, so that is a confirmation,
  not a permission (threat model, open question 32).
- Versioning and Object Lock for ransomware resistance **(implemented, ADR-0008, 0009)**.
- Replication between sites **(implemented, ADR-0026)**: the source is an S3 client of
  the other site over verified TLS, through the egress client, with a key of that site
  that holds `s3:ReplicateObject` and `s3:ReplicateDelete` - granted by no wildcard and
  no bucket policy - and the actions of later changes, and can read, list and remove
  nothing; every replica header and every payload is signed; a replica bucket refuses
  every other new version or delete marker in apply (its own users may still change
  tags, retention and legal hold, which the source's next later change sets again); a version removed for good is never replicated, so a
  source that was taken over cannot purge its copy; no key or wrapped key crosses (the
  source decrypts inside TLS, the destination encrypts under its own keys).
- Air-gap safe: no runtime call to the internet, no telemetry, no CDN asset. The one
  call a deployment can be told to make for itself - asking an update source for
  releases - is off until an operator sets `update-source`.

### Checking for releases and upgrading **(implemented, ADR-0029; off by default)**

`app.UpdateService` asks `ports.ReleaseSource` (`releases/github`) for the releases -
on its interval, or when the console asks, at most once a minute - and keeps what it
found in memory; nothing about releases is replicated, and no event is published. It
offers what `release.Allowed` admits: a newer patch of the running minor or the newest
patch of the next one, on the channel set. All three actions - `admin:GetUpdates`,
`admin:CheckForUpdates`, `admin:UpgradeCluster` - are asked of the one resource
`arn:cactusfs:admin:::updates`. Installing needs a console session whose user types the
password again in the request (`SessionService.Confirm`); an API key never installs.

The trust chain, each link checked on the node, offline:

1. the release's signed list - `SHA256SUMS` for a binary, `digests.txt` for the image -
   and its Sigstore bundle are fetched (bounded: 64 KiB and 1 MiB), and the bundle is
   verified by `ports.ReleaseVerifier` (`releases/sigstore`, `sigstore-go`) against the
   trusted root embedded in the binary or `update-trusted-root`: certificate chain, a
   transparency-log entry, an observer timestamp and a signed certificate timestamp;
2. the certificate must name exactly the release workflow at the tag of the version
   being installed, `…/release.yml@refs/tags/v<target>`, issued by GitHub Actions;
3. the list is read strictly (`release.ParseSums`, `ParseDigests`);
4. a binary is streamed through SHA-256 and must be the one the list names; an image is
   installed by the digest of its manifest list, which the runtime checks itself.

Only then is a `release.Verified` made, and `ports.Upgrader` takes nothing else. The
modes: `off` (the default) shows the commands of the deployment's own way; `binary`
fetches the binary again beside the executable, checks its SHA-256 and that `<new>
version` names the target, keeps the old as `cactusfs.previous` and renames the new
one into place, then restarts into it (Unix) or says `restart-required` (Windows) - a
node that stops before the swap ends the run with the old binary, a swap once begun
completes; `kubernetes` patches the chart's StatefulSets one node at a time, this node
last, by eviction under the PodDisruptionBudget, held by a lock annotation written with
`resourceVersion`, and puts back the first node that does not come up. A cluster of
binaries on several machines, and compose, are `off`: commands only.

The decorators of `internal/adapters/telemetry` trace every call of the three ports and
count `cactusfs_update_checks_total{outcome}`, `cactusfs_update_available` and
`cactusfs_upgrade_runs_total{outcome}` (section 3, metrics). The threats are in the
threat model, "Updates and upgrades".

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
`LICENSE.md` in the document: no identifier of the SPDX list is that text. The console's
document says, in its comment, where the notices of its packages are: `web/dist/notices.txt`
(PLAN X.5), written by the console's build (`web/notices.mjs`, a plugin of
`web/vite.config.ts`) from the installed production tree of `web/` - every package's name,
version, licence and the text of its `LICENSE*`/`NOTICE*`/`COPYING*` files, sorted, no
date, then pdf.js's `LICENSE_JBIG2`, `LICENSE_PDFJS_JBIG2`, `LICENSE_OPENJPEG` and
`LICENSE_PDFJS_OPENJPEG` for the two decoders shipped beside the bundle. A licence off the
list, a package without a licence or notice file, a decoder whose notices are not named, a
module bundled - for the page or a worker - from a package that is not a production one, or
Vite's module preload polyfill (turned off: `build.modulePreload.polyfill` false) fails the
build (and removes `web/dist`). The file is embedded with the console and served
at `/notices.txt` (`text/plain`, `no-cache`); `make licences` builds the console, fails if
the file is not byte for byte what the build of the installed tree gives (it is generated
deterministically, so this holds the build's plugin and the gate to one output), and runs a test of the
binary's embedded copy (`web/embed_console_test.go`, `-tags console`). `make sign-verify` starts a
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
| k3s, k8s | `deploy/helm/cactusfs` — implemented: a StatefulSet per node, a claim per drive, PDB, default-deny NetworkPolicy (each address the nodes must reach - Vault, a collector, a notification target - an egress rule of `networkPolicy.egressTo`; a rule without peers or ports is refused), `restricted` Pod Security Standard; rendered, validated and asserted by `make helm-lint`, installed on k3s by `make test-k8s` |

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
  is not of the release; and an upgrade from the console (`TestUpgradeOnK3s`, ADR-0029):
  `upgrade-mode` off answering 501 on four nodes, the opt-in turned on, the nodes moved
  to a second image of this tree one at a time and the node that runs it last, an image
  that cannot be pulled put back.

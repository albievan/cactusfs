# CactusFS architecture

CactusFS is an S3-compatible, erasure-coded, multi-node object store. It ships as one Go
binary that runs standalone on Windows, Linux and macOS or as a cluster, and it puts data
security first. This document describes the design. `CLAUDE.md` holds the rules,
`PLAN.md` holds the work and its status, and `docs/adr/` holds the decisions that are
expensive to reverse.

Sections marked **(target)** describe the design the plan builds toward; the milestone
that implements each is named. Sections marked **(implemented)** describe code that exists.

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
              (shards on     (Raft + bbolt)  (Vault Transit  (in-process,   (gRPC, mTLS)
               local drives)                  or sealed file)  NATS, webhook)
```

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
| `internal/domain/bucket` | bucket naming rules |
| `internal/domain/object` | IDs, keys, version metadata, ranges, listing rules, the streaming AEAD (ADR-0002) |
| `internal/domain/iam` | principals, actions, the policy language and the decision, users, groups, roles, API keys, built-in roles, sealed secrets |
| `internal/domain/audit` | audit records and their hash chain |
| `internal/domain/cluster` | readiness; nodes, roles and labels; drive topology: planning across nodes, slots, reconciliation, replacement |
| `internal/domain/erasure` | erasure parameters, quorum, set layout, placement, stripe geometry |
| `internal/ports` | `KMS`, `ShardStore`, `ErasureCodec`, `BucketStore`, `ObjectIndex`, `RootCredentialStore`, `TopologyStore`, `NodeRegistry`, `ClusterView`, `DriveSource`, `EventPublisher`, `IAMStore`, `AuditLog`, `SessionStore`, `PasswordHasher`, `IdentityProvider`, `Authorizer`, `Clock`, `ReadinessCheck` |
| `internal/ports/porttest` | in-memory fakes and the contract tests every implementation runs |
| `internal/app` | bucket, object and identity use cases; striped storage, healing, scrubbing, garbage collection, topology; the authorizer; health |
| `internal/adapters/s3api` | in: S3 API (SigV4, XML) |
| `internal/adapters/health` | in: `GET /healthz`, `GET /readyz`; the shutdown gate |
| `internal/adapters/telemetry` | metrics, traces, the `/metrics` handler, decorators for every outbound port |
| `internal/adapters/disk` | out: shards on a local drive (ADR-0001) |
| `internal/adapters/erasurecodec` | out: Reed-Solomon (ADR-0004) |
| `internal/adapters/fsutil` | durable file creation and directory sync per OS |
| `internal/adapters/raftmeta` | out: Raft + bbolt metadata, one voter or a cluster (ADR-0003, 0005) |
| `internal/adapters/noderpc` | both: node-to-node gRPC; drives of other nodes as `ShardStore` (ADR-0005) |
| `internal/adapters/nodetls` | mutual TLS 1.3 between nodes; development certificates |
| `internal/adapters/events/inproc` | out: in-process event bus |
| `internal/adapters/kms/local` | out: sealed local key file |
| `internal/adapters/kms/vault` | out: Vault Transit (ADR-0006) |
| `internal/adapters/passwords` | out: Argon2id password hashes |
| `internal/adapters/httpsession` | in: session cookie and CSRF header |
| `internal/adapters/audit` | out: audit records as JSON lines |
| `cmd/cactusfs` | composition root: configuration, node wiring, listeners, shutdown |

## 3. The binary **(implemented: `version`, `server` standalone and clustered, `dev-init`)**

```
cactusfs version
cactusfs server [flags]
cactusfs dev-init --dir <directory> --nodes <id>[,<id>...]
```

`dev-init` writes what the nodes of a development cluster must share: a throw-away node
CA with a certificate per node, a second CA with a certificate for the S3 listener, and
one sealed KMS key file. It never overwrites; a complete directory is left alone.

Configuration is 12-factor: every setting is a `CACTUSFS_*` environment variable with a
matching flag, and the flag wins.

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--data` | `CACTUSFS_DATA` | — (required) | drive directory; repeat or comma-separate |
| `--meta-dir` | `CACTUSFS_META_DIR` | `<first drive>/meta` | metadata directory |
| `--kms` | `CACTUSFS_KMS` | — (required) | `vault`: HashiCorp Vault Transit; `local`: sealed local key file. No default and no unencrypted mode |
| `--vault-addr`, `--vault-key`, `--vault-token-file` | `CACTUSFS_VAULT_ADDR`, `…_KEY`, `…_TOKEN_FILE` | — | Vault's URL (https), the Transit key (an AEAD type), the file holding the token |
| `--vault-transit-mount`, `--vault-ca-cert`, `--vault-server-name`, `--vault-namespace` | `CACTUSFS_VAULT_…` | `transit`, system CAs, —, — | where Transit is mounted and how Vault's certificate is checked |
| `--audit-export` | `CACTUSFS_AUDIT_EXPORT` | `false` | write every audit record to standard output as a JSON line |
| `--kms-key-file` | `CACTUSFS_KMS_KEY_FILE` | `<meta dir>/kms-local.json` | local KMS key file |
| `--kms-passphrase-file` | `CACTUSFS_KMS_PASSPHRASE_FILE` | — | file holding the KMS passphrase |
| `--s3-addr` | `CACTUSFS_S3_ADDR` | `:9000` | S3 API listener |
| `--s3-domain` | `CACTUSFS_S3_DOMAIN` | — | enables virtual-hosted style, `<bucket>.<domain>` |
| `--region` | `CACTUSFS_REGION` | `us-east-1` | region reported to clients |
| `--admin-addr` | `CACTUSFS_ADMIN_ADDR` | `:9001` | admin listener: probes and `/metrics` |
| `--tls-cert` | `CACTUSFS_TLS_CERT` | — | PEM certificate chain |
| `--tls-key` | `CACTUSFS_TLS_KEY` | — | PEM private key |
| `--dev-plaintext` | `CACTUSFS_DEV_PLAINTEXT` | `false` | development only: HTTP without TLS, loopback IP only |
| `--scrub-interval` | `CACTUSFS_SCRUB_INTERVAL` | `24h` | pause between scrub passes; `0` disables scrubbing |
| `--scrub-rate-mib` | `CACTUSFS_SCRUB_RATE_MIB` | `64` | scrub read limit in MiB/s; `0` is no limit |
| `--gc-interval` | `CACTUSFS_GC_INTERVAL` | `1h` | pause between garbage collection passes; `0` disables it |
| `--gc-grace` | `CACTUSFS_GC_GRACE` | `24h` | minimum age of an unreferenced shard before it is removed (at least `1h`) |
| `--allow-drive-replacement` | `CACTUSFS_ALLOW_DRIVE_REPLACEMENT` | `false` | confirm, for this start, that one empty drive replaces one missing drive; permanent |
| `--node-id` | `CACTUSFS_NODE_ID` | generated when alone | name of the node; in a cluster required, and the name in its certificate |
| `--roles` | `CACTUSFS_ROLES` | all | `gateway`, `meta`, `storage`, `console` (cluster only) |
| `--peers` | `CACTUSFS_PEERS` | — | every node of the cluster, this one included, as `id=host:port` of its node RPC; makes the node a cluster member |
| `--rpc-addr`, `--raft-addr` | `CACTUSFS_RPC_ADDR`, `CACTUSFS_RAFT_ADDR` | `:9002`, `:9003` | listeners for node RPC and Raft |
| `--advertise-rpc`, `--advertise-raft` | `CACTUSFS_ADVERTISE_RPC`, `…_RAFT` | from `--peers` | addresses at which other nodes reach them |
| `--node-tls-cert`, `--node-tls-key`, `--node-tls-ca` | `CACTUSFS_NODE_TLS_CERT`, `…_KEY`, `…_CA` | — | node certificate, key and node CA; required in a cluster |
| `--topology-region`, `-zone`, `-rack`, `-host` | `CACTUSFS_TOPOLOGY_REGION`, … | — | where the node is, for placement |
| `--otlp-endpoint` | `CACTUSFS_OTLP_ENDPOINT` | — | OTLP/HTTP trace collector; nothing is exported when empty |
| `--log-level` | `CACTUSFS_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `--drain-delay` | `CACTUSFS_DRAIN_DELAY` | `0s` | on shutdown, time to keep serving with `/readyz` at 503 before the listeners close |
| `--shutdown-timeout` | `CACTUSFS_SHUTDOWN_TIMEOUT` | `30s` | time for in-flight requests to finish once the listeners have closed |

Secrets have no flag, because a command line is visible to every user of the machine:
`CACTUSFS_KMS_PASSPHRASE` (or the passphrase file), and optionally
`CACTUSFS_ROOT_ACCESS_KEY` with `CACTUSFS_ROOT_SECRET_KEY`.

Behaviour:

- **TLS or nothing.** Without a certificate and key the server refuses to start. With
  `--dev-plaintext` it serves HTTP, but only on `127.0.0.1` or `::1`; a wildcard, a
  routable address or a host name is rejected. TLS minimum is 1.2, restricted to ECDHE
  suites with AES-GCM or ChaCha20-Poly1305; 1.3 is negotiated when the client offers it.
- **Probes.** `GET /healthz` answers 200 while the process can serve HTTP. `GET /readyz`
  answers 200 when every `ports.ReadinessCheck` passes and 503 otherwise. The probes are
  unauthenticated, so a response is only `{"status":"ok"}` or `{"status":"unavailable"}`;
  the failing check and its error go to the log.
- **Shutdown.** On SIGTERM or interrupt the readiness gate closes, so `/readyz` answers
  503. The node keeps serving for the drain delay, then closes its listener, gives
  in-flight requests the shutdown timeout to finish, and exits 0. Requests that outlive
  the timeout are cut off and the exit code is 1. With the default drain delay of zero
  the listener closes immediately, so the 503 is only observable when a delay is set.
- **Root credentials.** There is no built-in login. A new deployment stores the root
  credentials supplied in the environment, or generates a pair and prints it once on
  standard error — never in the log. The secret is stored sealed through the KMS. On
  later starts, supplied credentials that differ from the stored ones stop the node.
- **KMS.** The local key file is created on the first start of a new deployment. On an
  existing deployment a missing key file, a wrong passphrase or another deployment's key
  stops the node: nothing is ever stored or served without the right key.
- **Metrics.** `GET /metrics` on the admin listener requires
  `Authorization: Bearer <access key ID>:<secret>` and passes the authorizer
  (`admin:GetMetrics`). Metrics carry no bucket, key or access-key labels.
- **Logs** are JSON on stdout (`log/slog`). Secrets, keys, tokens and object contents are
  never logged.

Default ports, all configurable: S3 `9000`; admin API, Swagger UI and console `9001`;
node RPC `9002`; Raft `9003`.

### Roles and run modes **(implemented, ADR-0005)**

One binary, four roles selected with `--roles` (default: all): `gateway` (S3 API,
authentication, erasure encode/decode; holds nothing that cannot be rebuilt), `meta`
(Raft voter), `storage` (drives; collects garbage on them) and `console` (UI, milestone
5). Every node answers probes and serves metrics; only gateways open the S3 listener.

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
  parity**, and the node says so at start. Spreading across nodes, racks and zones, and
  parity per storage class, are milestone 3.
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
the object index are replicated with Raft and stored in bbolt. (Users, groups, policies
and API keys join them in milestone 4.) Every node holds a replica; nodes with the
`meta` role vote. Changes go through the leader; reads are linearizable on every node,
by read index. The object index sits behind `ports.ObjectIndex` so it can move to
sharded Raft groups later; that move needs an ADR. On-disk formats and Raft entries
carry a version; no node writes an entry format newer than the oldest member of the
cluster understands, so version N runs alongside N+1 during a rolling upgrade.

## 6. APIs **(S3 subset, management API, node RPC and events implemented; S3 depth is milestone 6)**

- **S3 data API.** Defined by AWS's S3 API reference: path-style and virtual-hosted-style
  addressing; SigV4 header, presigned and streaming signatures; XML errors. Its contract
  is the compatibility suite in `test/s3compat`, guarded by a recorded baseline.
  **Implemented:** ListBuckets, CreateBucket, HeadBucket, DeleteBucket,
  GetBucketLocation, PutObject, GetObject (single range), HeadObject, DeleteObject,
  ListObjectsV2; SigV4 in the Authorization header and in the query string (presigned
  URLs, valid for at most seven days); signed aws-chunked uploads; literal, `UNSIGNED-PAYLOAD` and
  `STREAMING-UNSIGNED-PAYLOAD-TRAILER` payloads with CRC32, CRC32C, CRC64NVME and SHA-256
  checksums and `Content-MD5`. Everything else answers `501 NotImplemented` after
  authentication. The suite runs the AWS SDK for Go with its default options over TLS
  against the built binary.
- **Admin API (implemented, ADR-0007).** REST/JSON under `/api/v1` on the admin listener
  of gateway nodes, designed first in `api/openapi/admin.yaml` (OpenAPI 3.0.3), with the
  strict server generated by `oapi-codegen`. An API key as a bearer credential, or a
  console session with a CSRF token. Errors are `application/problem+json`; every
  mutation accepts `Idempotency-Key`, whose records are replicated and whose stored
  answers are sealed through the KMS. Operations: session, buckets, objects (list,
  upload, download, delete), users, groups, roles, policies, API keys, cluster and
  maintenance status, audit log. The contract is served at `/api/v1/openapi.json` and
  `.yaml`, and Swagger UI at `/docs` from embedded assets under a Content-Security-Policy
  that allows nothing from another host.
- **Console (implemented, ADR-0007).** `web/`, embedded with `-tags console` and served
  at `/` by nodes with the console role. It calls only the admin API, through the client
  generated from the contract.
- **Events.** Every state change other components care about is an event named
  `cactusfs.<aggregate>.<event>.v1`, defined in `api/asyncapi/events.yaml` and derived
  from **applied Raft entries**, never published from a handler. Delivery goes through
  `ports.EventPublisher`. **Implemented:** the contract, eight events, and the
  in-process publisher (at most once per node; counted in `cactusfs_events_total`).
  NATS JetStream and webhooks are milestone 8. Consumers dedupe by event ID.
- **Node RPC.** gRPC over mTLS 1.3, defined in `api/proto/cactusfs/node/v1` and checked
  with `buf lint` and `buf breaking`. **Implemented:** node description and in-flight
  questions, the metadata leader's services, and shard storage.

## 7. Security **(see the threat model for what is implemented)**

The full analysis is in `docs/threat-model.md`. In brief:

- Encryption at rest is always on **(implemented)**: AES-256-GCM streaming AEAD with a
  per-object data key wrapped by a KMS key. KMS adapters: a sealed local key file for
  standalone **(implemented)**, Vault Transit for clusters **(implemented, ADR-0006)**.
  There is no unencrypted fallback.
- Encryption in transit: TLS 1.2+ for clients **(implemented for the admin listener)**,
  mTLS 1.3 between nodes including the Raft transport.
- Every inbound request is authenticated and passes through the single
  `ports.Authorizer` **(implemented, ADR-0006)**: users, groups, roles and IAM-style
  policies; default deny; explicit deny wins; a key never exceeds its owner. The key,
  its owner and every policy that applies are resolved in one consistent read when a
  request is authenticated. The probe endpoints are the one exception and disclose
  nothing.
- No default credentials. API key secrets are envelope-encrypted, shown once, compared
  in constant time **(implemented)**. Keys expire, can be narrowed by an inline policy
  and are revoked for every node at once.
- Hash-chained, append-only audit trail **(implemented)**: changes to identities are
  recorded in the same Raft entry as the change; refusals in the background.
  `--audit-export` writes the records to the log stream as JSON lines.
- Console accounts with Argon2id and replicated, short-lived sessions **(use cases
  implemented; the console is milestone 5)**.
- Versioning and Object Lock for ransomware resistance (milestone 6).
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
| golangci-lint, Helm, Docker CLI | `Dockerfile` `ARG`s, by digest |
| pnpm | `Dockerfile` `PNPM_VERSION` and `web/package.json` `packageManager` (by version, from the npm registry) |
| console packages | `web/pnpm-lock.yaml` |

The product image is distroless static, runs as UID 65532, has no shell, and is meant to
run with a read-only root filesystem and all capabilities dropped.

The console (`web/`, React + TypeScript + Vite) is a placeholder until milestone 5. It is
built by the `Dockerfile` and `make build` but is not embedded or served yet (PLAN 5.4),
so the `console` build tag has no effect today. Logos come from the brand assets:
masters in `docs/brand/`, the console's copies in `web/src/assets/brand/`, the tab icon
in `web/public/favicon.svg`. They are used as they are and never redrawn.

## 9. Deployment targets **(binary and compose implemented; Helm is milestone 7)**

| Target | Artifact |
|---|---|
| Single binary | `dist/cactusfs-<os>-<arch>` — implemented, six targets |
| Docker Desktop, Podman | `deploy/compose/compose.yaml` — implemented: 4-node development cluster over mTLS, rootless-safe; `make up` / `make down` |
| k3s, k8s | `deploy/helm/cactusfs` — StatefulSet, PVC per drive, PDB, default-deny NetworkPolicy, `restricted` Pod Security Standard |

Core code uses no cloud-provider API. Hybrid means separate clusters per site linked by
asynchronous bucket replication; one Raft quorum is never stretched across a WAN.

## 10. Quality gates **(implemented)**

`make check` runs every gate in `CLAUDE.md` §5. `PLAN.md` ("Gates that have nothing to
check yet") lists the gates whose subject a later milestone introduces and how each
behaves until then.

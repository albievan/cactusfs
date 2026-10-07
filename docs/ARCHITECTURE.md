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
| `internal/domain` | typed sentinel errors |
| `internal/domain/bucket` | bucket naming rules |
| `internal/domain/object` | IDs, keys, version metadata, ranges, listing rules, the streaming AEAD (ADR-0002) |
| `internal/domain/iam` | principals, actions, the default-deny decision, credential rules, sealed secrets |
| `internal/domain/cluster` | readiness |
| `internal/ports` | `KMS`, `ShardStore`, `BucketStore`, `ObjectIndex`, `RootCredentialStore`, `Authorizer`, `Clock`, `ReadinessCheck` |
| `internal/ports/porttest` | in-memory fakes and the contract tests every implementation runs |
| `internal/app` | bucket, object and identity use cases; the authorizer; health |
| `internal/adapters/s3api` | in: S3 API (SigV4, XML) |
| `internal/adapters/health` | in: `GET /healthz`, `GET /readyz`; the shutdown gate |
| `internal/adapters/telemetry` | metrics, traces, the `/metrics` handler, decorators for every outbound port |
| `internal/adapters/disk` | out: shards on a local drive (ADR-0001) |
| `internal/adapters/raftmeta` | out: Raft + bbolt metadata (ADR-0003) |
| `internal/adapters/kms/local` | out: sealed local key file |
| `cmd/cactusfs` | composition root: configuration, node wiring, listeners, shutdown |

## 3. The binary **(implemented: `version`, `server` standalone; roles are target, milestone 3)**

```
cactusfs version
cactusfs server [flags]
```

Configuration is 12-factor: every setting is a `CACTUSFS_*` environment variable with a
matching flag, and the flag wins.

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--data` | `CACTUSFS_DATA` | — (required) | drive directory; repeat or comma-separate |
| `--meta-dir` | `CACTUSFS_META_DIR` | `<first drive>/meta` | metadata directory |
| `--kms` | `CACTUSFS_KMS` | — (required) | `local`: sealed local key file. No default and no unencrypted mode |
| `--kms-key-file` | `CACTUSFS_KMS_KEY_FILE` | `<meta dir>/kms-local.json` | local KMS key file |
| `--kms-passphrase-file` | `CACTUSFS_KMS_PASSPHRASE_FILE` | — | file holding the KMS passphrase |
| `--s3-addr` | `CACTUSFS_S3_ADDR` | `:9000` | S3 API listener |
| `--s3-domain` | `CACTUSFS_S3_DOMAIN` | — | enables virtual-hosted style, `<bucket>.<domain>` |
| `--region` | `CACTUSFS_REGION` | `us-east-1` | region reported to clients |
| `--admin-addr` | `CACTUSFS_ADMIN_ADDR` | `:9001` | admin listener: probes and `/metrics` |
| `--tls-cert` | `CACTUSFS_TLS_CERT` | — | PEM certificate chain |
| `--tls-key` | `CACTUSFS_TLS_KEY` | — | PEM private key |
| `--dev-plaintext` | `CACTUSFS_DEV_PLAINTEXT` | `false` | development only: HTTP without TLS, loopback IP only |
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

### Roles and run modes **(target, milestone 3)**

One binary, four roles selected with `--roles` (default: all): `gateway` (S3 and admin
API, authentication, erasure encode/decode, stateless), `meta` (Raft voter), `storage`
(drives, scrub, heal) and `console` (UI).

- **Standalone** — `cactusfs server --data <dir>...` runs every role in one process with
  a single-node Raft group. With four or more drives it erasure-codes across them; with
  fewer it still encrypts and checksums and logs that it has no parity.
- **Cluster** — the same binary on four or more storage nodes with three or five `meta`
  voters.

Standalone is a cluster of one, not a second implementation.

## 4. Storage engine **(milestone 1 implemented; erasure coding, heal and scrub are milestone 2; cluster is milestone 3)**

**Today:** every object is encrypted (ADR-0002) and stored as one checksummed shard on one
drive (ADR-0001), with the layout recorded in erasure-coded form (`1` data shard, `0`
parity) so that milestone 2 changes values, not formats. With several `--data`
directories objects are spread across them; there is no parity yet, and the node says so
at start. Shards of failed, replaced and deleted objects are removed best-effort; a crash
can leave unreferenced shards until the collector of PLAN 2.7.

- **Erasure coding (milestone 2).** Objects are split into Reed-Solomon data and parity shards over
  erasure sets of 4–16 drives. Default parity is 2 for 4–5 drives, 3 for 6–7 and 4 for 8
  or more, configurable per storage class. Shards of one object never share a drive and
  are spread across nodes, then racks and zones when nodes are labelled.
- **Quorum (milestones 2–3).** Write quorum is the number of data shards, plus one when data equals
  parity. Read quorum is the number of data shards.
- **Write path.** Encrypt → erasure-code → write shards under a new immutable ID and
  fsync on a write quorum → commit the object version through Raft → acknowledge. The
  Raft commit is the only visibility switch. Shards without a committed version are
  garbage-collected.
- **Consistency.** Reads and listings are strongly consistent (read-after-write).
- **Integrity.** Every shard block carries a SHA-256 checksum, bound to its position,
  that is verified on every read, and every segment is authenticated by AES-GCM. A
  background scrubber that finds bitrot and heals from parity is milestone 2. Bytes that fail verification
  are never returned.
- **Keys are not paths.** Object keys and bucket names never become filesystem paths.
  Data lives under generated IDs, so case-insensitive filesystems, Windows reserved
  names, path length limits and `../` cannot affect it.
- **Portability.** OS differences (directory fsync, file locking, service control) live
  in `adapters/disk` and `cmd/cactusfs` behind build tags.

## 5. Metadata **(implemented for a single voter, ADR-0003; multi-node is milestone 3)**

IAM, API keys, policies, bucket configuration, cluster topology and the object index are
replicated with Raft and stored in bbolt. The object index sits behind
`ports.ObjectIndex` so it can move to sharded Raft groups later; that move needs an ADR.
On-disk formats and Raft entries carry a version from the first release, and version N
must run alongside N+1 for rolling upgrades.

## 6. APIs **(S3 subset implemented; the rest is target, milestones 3, 5 and 6)**

- **S3 data API.** Defined by AWS's S3 API reference: path-style and virtual-hosted-style
  addressing; SigV4 header, presigned and streaming signatures; XML errors. Its contract
  is the compatibility suite in `test/s3compat`, guarded by a recorded baseline.
  **Implemented:** ListBuckets, CreateBucket, HeadBucket, DeleteBucket,
  GetBucketLocation, PutObject, GetObject (single range), HeadObject, DeleteObject,
  ListObjectsV2; SigV4 header authentication; literal, `UNSIGNED-PAYLOAD` and
  `STREAMING-UNSIGNED-PAYLOAD-TRAILER` payloads with CRC32, CRC32C, CRC64NVME and SHA-256
  checksums and `Content-MD5`. Everything else answers `501 NotImplemented` after
  authentication. The suite runs the AWS SDK for Go with its default options over TLS
  against the built binary.
- **Admin API.** REST/JSON under `/api/v1`, designed first in `api/openapi/admin.yaml`,
  with handlers generated by `oapi-codegen`. Errors are `application/problem+json`;
  every mutation accepts `Idempotency-Key`. The spec is served at
  `/api/v1/openapi.json` and `.yaml`, and Swagger UI at `/docs` from embedded assets.
- **Events.** Every state change other components care about is an event named
  `cactusfs.<aggregate>.<event>.v1`, defined in `api/asyncapi/events.yaml` and derived
  from **applied Raft entries**, never published from a handler. Delivery goes through
  `ports.EventPublisher`: in-process by default, NATS JetStream and webhooks optionally.
  Consumers dedupe by event ID.
- **Node RPC.** gRPC over mTLS 1.3, defined in `api/proto/`.

## 7. Security **(see the threat model for what is implemented)**

The full analysis is in `docs/threat-model.md`. In brief:

- Encryption at rest is always on **(implemented)**: AES-256-GCM streaming AEAD with a
  per-object data key wrapped by a KMS key. KMS adapters: a sealed local key file for
  standalone **(implemented)**, Vault Transit for clusters (milestone 4). There is no
  unencrypted fallback.
- Encryption in transit: TLS 1.2+ for clients **(implemented for the admin listener)**,
  mTLS 1.3 between nodes including the Raft transport.
- Every inbound request is authenticated and passes through the single
  `ports.Authorizer` **(implemented: root only, default deny; policies are milestone 4)**.
  The probe endpoints are the one exception and disclose nothing.
- No default credentials. API key secrets are envelope-encrypted, shown once, compared
  in constant time.
- Hash-chained, append-only audit trail. Versioning and Object Lock for ransomware
  resistance.
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

## 9. Deployment targets **(target, milestones 3 and 7)**

| Target | Artifact |
|---|---|
| Single binary | `dist/cactusfs-<os>-<arch>` — implemented, six targets |
| Docker Desktop, Podman | `deploy/compose/compose.yaml` — 4-node cluster, rootless-safe |
| k3s, k8s | `deploy/helm/cactusfs` — StatefulSet, PVC per drive, PDB, default-deny NetworkPolicy, `restricted` Pod Security Standard |

Core code uses no cloud-provider API. Hybrid means separate clusters per site linked by
asynchronous bucket replication; one Raft quorum is never stretched across a WAN.

## 10. Quality gates **(implemented)**

`make check` runs every gate in `CLAUDE.md` §5. `PLAN.md` ("Gates that have nothing to
check yet") lists the gates whose subject a later milestone introduces and how each
behaves until then.

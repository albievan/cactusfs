# CactusFS — PLAN

Source of truth for **what** is being built and **where we are**. Rules live in
`CLAUDE.md`; the design lives in `docs/ARCHITECTURE.md`.

Status values: `todo | in_progress | done | blocked`.
A step is `done` only when its acceptance criterion holds **and** `make check` is green.

## Status at a glance

| Milestone | Title | Status |
|---|---|---|
| 0 | Scaffold | done |
| 1 | Standalone store | done |
| 2 | Erasure coding and integrity on local drives | done |
| 3 | Cluster | done |
| 4 | IAM | done |
| 5 | Admin API, Swagger UI, console | done |
| 6 | S3 depth | in_progress |
| 7 | Deployment hardening | todo |
| 8 | Hybrid | todo |

---

## Milestone 0 — Scaffold

Done when `make verify-clone` and `make check` are green.

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 0.1 | done | Plan, architecture, threat model, agent definitions | `PLAN.md` covers milestones 0–8 with IDs, criteria and files; docs written fresh from `CLAUDE.md` | `PLAN.md`, `docs/ARCHITECTURE.md`, `docs/threat-model.md`, `docs/adr/README.md`, `.claude/agents/{planner,coder,reviewer}.md` |
| 0.2 | done | Go module and hexagonal skeleton with the health use case | `go build ./...`, `go vet ./...`, `go test ./...` pass with Go alone; `internal/domain` imports nothing outside the standard library; domain and app coverage ≥ 80 % | `go.mod`, `go.sum`, `internal/domain/errors.go`, `internal/domain/cluster/health.go`, `internal/ports/health.go`, `internal/app/health.go`, tests |
| 0.3 | done | `cactusfs` binary: `version` and `server` | `cactusfs version` prints version, Go version and OS/arch; `cactusfs server` serves only `GET /healthz` and `GET /readyz`, over TLS 1.2+ or (with `--dev-plaintext`) on loopback only; config from `CACTUSFS_*` env with matching flags; JSON logs on stdout; on SIGTERM `/readyz` answers 503 for `--drain-delay`, then the listener closes and in-flight requests finish within `--shutdown-timeout` | `cmd/cactusfs/{main,config,server,version}.go`, `internal/adapters/health/{handler,gate}.go`, tests, `test/integration/server_test.go` |
| 0.4 | done | Multi-stage `Dockerfile` | `docker build .` yields a distroless, non-root image < 60 MB for linux/amd64 and linux/arm64; every base image pinned by digest; `--target builder` yields the tool image | `Dockerfile`, `.dockerignore` |
| 0.5 | done | `Makefile` and gate scripts | every target in `CLAUDE.md` §6 exists; targets run inside the builder image with `ENGINE=docker\|podman`; plain `go build/test/vet` work without `make` | `Makefile`, `scripts/*.sh` (one per gate, plus `no-bypass.sh`), `scripts/web-licences.mjs`, `.golangci.yml`, `.gitignore`, `.gitattributes`, `test/s3compat/{doc.go,baseline.txt}` |
| 0.6 | done | Console scaffold | `pnpm -C web lint && typecheck && test && build` clean; no CDN or other remote asset; favicon linked and logo taken from the brand assets, unmodified | `web/**` (brand files in `docs/brand/`, `web/public/`, `web/src/assets/brand/` were supplied by Albie) |
| 0.7 | done | CI matrix | workflow runs `make check` on Linux and unit tests on Linux, macOS and Windows; actions pinned by commit SHA | `.github/workflows/ci.yml` |
| 0.8 | done | README | build-from-source steps for Go-only and container-only builds, run instructions | `README.md` |
| 0.9 | done | Verification and review | `make verify-clone` and `make check` green (darwin/arm64 host, Docker 29.8.1, linux/arm64 builder); reviewer: 3 blocker, 3 major fixed; 12 minor fixed or raised as open questions | — |

### Gates that have nothing to check yet

`make check` runs every §5 gate. Four of them guard artefacts that later milestones
introduce. They are wired now and behave as follows, so none can pass silently once its
subject exists:

| Gate | Behaviour in milestone 0 | Becomes real in |
|---|---|---|
| `make contracts` | regenerates and proves the tree is unchanged; **fails** if `api/openapi`, `api/asyncapi` or `api/proto` contains a contract whose checker is not wired | 3.2 (proto), 3.6 (AsyncAPI), 5.1 (OpenAPI) |
| `make s3-compat` | runs `test/s3compat` and compares against `baseline.txt` (0 tests recorded); **fails** if `baseline.txt` is missing, if the suite does not build, or if the suite has tests but no recorded baseline | 1.10 |
| `make helm-lint` | lints and renders `deploy/helm/cactusfs` when the chart exists; **fails** if `deploy/helm` holds any file without `cactusfs/Chart.yaml` | 7.1 |
| `make test-int` | black-box tests of the real binary, and of a four-node cluster of product-image containers (testcontainers-go) | 4.x (Vault), 8.2 (NATS) |

`make up`, `make down` and `make test-chaos` exist and exit non-zero with a pointer to the
step that implements them (`up`/`down`: 3.8; `test-chaos`: 2.8 and 3.9). They are not part of
`make check`.

---

## Milestone 1 — Standalone store

Done when a single `cactusfs server --data <dir>` passes the milestone's S3 compatibility
subset with encryption on.

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 1.1 | done | Domain model: bucket, object, version, IDs, typed errors | naming rules per the S3 reference; property tests; no external imports | `internal/domain/{bucket,object}/`, `internal/domain/errors.go` |
| 1.2 | done | Ports: `ObjectIndex`, `BucketStore`, `RootCredentialStore`, `ShardStore`, `KMS`, `Clock`, `Authorizer` | interfaces documented; fakes and contract tests in `internal/ports/porttest`, run against fakes and real adapters alike | `internal/ports/`, `internal/domain/iam/` |
| 1.3 | done | Disk adapter | shards stored under generated IDs only, all I/O through `os.Root`; exclusive drive lock; write + file fsync + directory fsync per OS; versioned header with checksum; position-bound SHA-256 per block verified on every read; ADR for the format | `internal/adapters/disk/`, `docs/adr/0001-on-disk-format.md` |
| 1.4 | done | Streaming AEAD and envelope encryption | AES-256-GCM segments, unique nonce, segment index in AAD; truncation/reorder detected; fuzz + property tests; ADR | `internal/domain/object/crypto*`, `internal/domain/iam/secret.go`, `docs/adr/0002-encryption.md` |
| 1.5 | done | Sealed local key file KMS (explicit opt-in) | no unencrypted fallback; key file sealed with a passphrase-derived key (Argon2id) | `internal/adapters/kms/local/` |
| 1.6 | done | Single-node Raft metadata (bbolt) | bucket and object index committed through Raft; versioned, typed entries; each entry applied exactly once across restarts and snapshots; ADR | `internal/adapters/raftmeta/`, `docs/adr/0003-metadata.md` |
| 1.7 | done | Use cases: bucket create/delete/list, object PUT/GET/HEAD/DELETE, ListObjectsV2 | write path order per §2; table-driven tests; coverage ≥ 80 % | `internal/app/` |
| 1.8 | done | S3 API adapter with SigV4 (header auth) and XML errors | path-style and virtual-hosted-style; literal, `UNSIGNED-PAYLOAD` and `STREAMING-UNSIGNED-PAYLOAD-TRAILER` (aws-chunked) payloads with checksum verification; fuzz tests for SigV4, XML and chunk decoding; constant-time comparison | `internal/adapters/s3api/` |
| 1.9 | done | Root bootstrap, `--data`, `--meta-dir`, KMS flags and wiring | first start requires or generates root credentials, shown once; no default credentials | `cmd/cactusfs/{config,node,server}.go`, `internal/app/identity.go` |
| 1.10 | done | S3 compatibility suite, first baseline | aws-sdk-go-v2 with default options over TLS against the built binary; one top-level test per behaviour, recorded in `baseline.txt` | `test/s3compat/` |
| 1.11 | done | Telemetry adapter | authenticated `/metrics` (`cactusfs_*`, own registry, no tenant labels) and OpenTelemetry traces as decorators on every port; no exporter unless an OTLP endpoint is configured | `internal/adapters/telemetry/` |

## Milestone 2 — Erasure coding and integrity on local drives

Done when a standalone node with four or more drives survives the loss or corruption of
up to *parity* drives without a failed read, repairs itself, and `make test-chaos` is
green.

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 2.1 | done | Erasure domain: parameters, quorum, stripe geometry, set layout | parity 2/3/4 by set size, 1+0 below four drives; write quorum = data (+1 when data = parity); sets of 4–16 drives, equal size; property tests on the geometry | `internal/domain/erasure/` |
| 2.2 | done | Reed-Solomon codec behind `ports.ErasureCodec` | `klauspost/reedsolomon`; property test: encode → lose ≤ parity shards → decode equals original; fuzz; contract test | `internal/ports/erasure.go`, `internal/adapters/erasurecodec/` |
| 2.3 | done | Topology in metadata and striped storage | drive slots recorded through Raft and addressed by drive ID, so reordering `--data` is harmless; shards of one object never share a drive; write succeeds on a write quorum and fails below it; reads reconstruct from any *data* shards; ADR | `internal/adapters/raftmeta/`, `internal/app/storage.go`, `internal/app/topology.go`, `docs/adr/0004-erasure-and-topology.md` |
| 2.4 | done | Heal | a degraded read queues the version; healing rebuilds missing, truncated and corrupt shards from the others and verifies the object is still current; a replaced (empty) drive is adopted into its slot and refilled | `internal/app/heal.go` |
| 2.5 | done | Background scrubber | walks every object, verifies every shard block, heals what it finds; rate-limited; `--scrub-interval`; metrics | `internal/app/scrub.go`, `cmd/cactusfs/` |
| 2.6 | done | Fewer than four drives | 1+0 on one drive each, encrypted and checksummed; logs that there is no parity | `internal/domain/erasure/`, `cmd/cactusfs/` |
| 2.7 | done | Garbage collection of unreferenced shards | `ShardStore.List`; a data-ID index in the state machine; shards older than `--gc-grace` that no version refers to are removed | `internal/ports/shards.go`, `internal/adapters/disk/`, `internal/adapters/raftmeta/`, `internal/app/gc.go` |
| 2.8 | done | Chaos suite | against the built binary with six drives: corrupt shards, delete shards, wipe up to *parity* drives → every read is correct and the drives are repaired; one more than parity → reads fail, never wrong data; `make test-chaos` | `test/chaos/`, `Makefile` |

## Milestone 3 — Cluster

Design (ADR-0005): every node runs Raft — `meta` nodes as voters, the others as
non-voters — so every node holds the metadata locally. Changes are forwarded to the
leader over node RPC; reads wait for the leader's read index. Shards are reached through
`ports.ShardStore` whether the drive is local or on another node. A cluster is formed
once, from the complete list of its nodes (`--peers`); its erasure sets are planned
across nodes, racks and zones at that moment.

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 3.1 | done | Cluster domain: nodes, roles, topology labels | `--roles` selects gateway/meta/storage/console; `region/zone/rack/host` labels; topology planned across nodes, then racks/zones; a node answers for its own drives only | `internal/domain/cluster/`, `cmd/cactusfs/` |
| 3.2 | done | Node RPC over mTLS 1.3 | gRPC contract in `api/proto/`; generated code committed; `buf lint` and `buf breaking` in `make contracts`; a remote drive passes the same `ShardStore` contract as a local one; every call passes the authorizer as a node principal | `api/proto/`, `internal/adapters/noderpc/`, `internal/adapters/nodetls/`, `buf.yaml`, `buf.gen.yaml`, `scripts/{gen,contracts}.sh`, `Dockerfile` |
| 3.3 | done | Multi-node Raft (3 or 5 voters) over mTLS | formation from identical peer lists, join, leave, leader change, linearizable reads on every node; a node never logs an entry newer than the oldest node understands (rolling upgrade N ↔ N+1 test) | `internal/adapters/raftmeta/`, `docs/adr/0005-cluster.md` |
| 3.4 | done | Distributed erasure sets | shards of one object on different nodes where the cluster allows; the tolerated number of node failures is computed and logged | `internal/domain/cluster/topology.go`, `internal/app/topology.go`, `internal/ports/drives.go` |
| 3.5 | done | Quorum write and read paths | no acknowledgement before write quorum + Raft commit; reads and writes survive nodes that are down up to parity; an unreachable drive is neither damage nor a reason to delete; read-after-write on every node | `internal/app/`, `internal/domain/errors.go` |
| 3.6 | done | Events from applied Raft entries | contract first in `api/asyncapi/events.yaml`, AsyncAPI validation in `make contracts`; in-process bus; consumers dedupe by event ID | `api/asyncapi/events.yaml`, `scripts/contracts.sh`, `internal/ports/events.go`, `internal/adapters/events/inproc/` |
| 3.7 | done | Integration tests on a real cluster | testcontainers-go: four containers of the product image form a cluster over mTLS; S3 through every node | `test/integration/` |
| 3.8 | done | Compose deployment | 4-node cluster; unchanged on `docker compose` and rootless `podman compose`; `make up/down`; `cactusfs dev-init` writes the throw-away CA, certificates and shared key file | `deploy/compose/compose.yaml`, `cmd/cactusfs/devinit.go`, `Makefile` |
| 3.9 | done | Chaos: node kill, partition, heal verification | `make test-chaos` passes: cluster scenarios next to the single-node ones | `test/chaos/` |

Out of scope for milestone 3, recorded so nobody assumes otherwise: adding storage nodes
or drives to a formed cluster (new erasure sets), starting a node with one of its drives
offline, the Vault KMS adapter (4.4 — a cluster shares a sealed key file until then, and
says so at start), an operator surface for join/leave beyond the node RPC (5.x).

## Milestone 4 — IAM

Design (ADR-0006): users belong to groups; users and groups hold roles; a role is a
named set of policies; a policy is an IAM-style JSON document. An API key belongs to a
user and may carry an inline policy that can only narrow what its owner may do. The
identity of a request - key, owner, every policy that applies - is resolved in one
consistent read at authentication and travels in the principal; the one authorizer
decides on it. Everything is replicated through Raft; every IAM change carries its
audit record in the same entry.

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 4.1 | done | IAM domain: users, groups, roles, policies | IAM-style JSON; default deny; explicit deny wins; conditions; fuzz + property tests for policy parsing and evaluation | `internal/domain/iam/` |
| 4.2 | done | Policy-backed `ports.Authorizer` | one decision point; no authorization logic in handlers; a key never exceeds its owner; denials audited | `internal/ports/iam.go`, `internal/app/authz.go`, `internal/adapters/raftmeta/iam.go` |
| 4.3 | done | API keys | secrets envelope-encrypted through the KMS, shown once; expiry, inline policy, last-used, immediate revocation; S3 requests authenticated with them | `internal/app/iam.go`, `internal/app/identity.go`, `internal/adapters/s3api/` |
| 4.4 | done | Vault Transit KMS adapter | `--kms vault`; integration test against Vault via testcontainers | `internal/adapters/kms/vault/`, `cmd/cactusfs/` |
| 4.5 | done | Built-in roles | `cluster-admin`, `bucket-admin`, `read-write`, `read-only`, `auditor`; cannot be changed or removed | `internal/domain/iam/builtin.go` |
| 4.6 | done | Audit log | append-only, hash-chained in the replicated state; IAM mutations (atomically with the change), authentication failures, denials; audit events in the AsyncAPI contract; export as JSON lines | `internal/domain/audit/`, `internal/app/audit.go`, `internal/adapters/audit/`, `api/asyncapi/events.yaml` |
| 4.7 | done | Console users | Argon2id local accounts behind `ports.IdentityProvider`; replicated sessions, short-lived; `HttpOnly`/`Secure`/`SameSite=Strict` cookie and CSRF token helpers | `internal/app/sessions.go`, `internal/adapters/passwords/`, `internal/adapters/httpsession/` |

Out of scope for milestone 4: the HTTP surface for managing IAM (admin API, milestone
5 - until then IAM is reachable through the use cases only), OIDC, SSE-KMS/SSE-C,
bucket policies (6.x).

## Milestone 5 — Admin API, Swagger UI, console

Design: ADR-0007. The contract comes first: nothing below 5.1 is written before
`api/openapi/admin.yaml` says what it is.

| Step | Status | What | Acceptance criterion | Files |
|---|---|---|---|---|
| 5.1 | done | OpenAPI contract and tooling | `api/openapi/admin.yaml` (OpenAPI 3.0.3, the highest `oapi-codegen` fully supports); `oapi-codegen` pinned as a `tool` directive; `oasdiff` pinned by digest in the builder image; both wired into `make gen` and `make contracts`; generated code committed | `api/openapi/admin.yaml`, `go.mod`, `Dockerfile`, `scripts/{gen,contracts}.sh` |
| 5.2 | done | Use cases the API needs | cluster status behind the authorizer (`admin:GetClusterStatus`); `Idempotency-Key` records replicated through Raft, responses stored sealed through the KMS; sign-in attempts limited per address | `internal/app/{clusterstatus,idempotency,sessions}.go`, `internal/ports/idempotency.go`, `internal/adapters/raftmeta/idempotency.go` |
| 5.3 | done | Admin API adapter | generated strict handlers under `/api/v1`; API key as bearer credential or console session with CSRF; every error is `application/problem+json`; `Idempotency-Key` on every mutation; spec served at `/api/v1/openapi.json` and `.yaml`; no authorization decision in the adapter | `internal/adapters/adminapi/` |
| 5.4 | done | Swagger UI at `/docs` | assets embedded with `go:embed`, nothing fetched from elsewhere (CSP enforces it); working Authorize button for API keys | `internal/adapters/adminapi/swaggerui/` |
| 5.5 | done | Embed the console | `-tags console` embeds `web/dist`; without the tag a stub page says the console was not built; served by nodes with the `console` role | `web/embed_*.go`, `internal/adapters/console/` |
| 5.6 | done | Generated web client | `openapi-typescript` + `openapi-fetch`, generated by `make gen`, committed; no hand-written API calls | `web/src/api/` |
| 5.7 | done | Console screens | sign-in, buckets, object browser, users, groups, roles and policies, API keys, cluster and drive status, audit log; responsive; brand assets used as they are; no business or authorization logic | `web/src/` |
| 5.8 | done | Wiring, tests, docs | admin API on the gateway role's admin listener; integration test over a real cluster; ADR-0007, ARCHITECTURE, README, threat model | `cmd/cactusfs/`, `test/integration/`, `docs/` |

## Milestone 6 — S3 depth

Built in this order; each step lands green on its own. Designs: ADR-0008 (versions in
the metadata state, entry schema v2), ADR-0009 (Object Lock and the clock that decides),
ADR-0010 (key hierarchy for multipart, SSE-KMS, SSE-C), ADR-0011 (resource policies,
anonymous access, CORS preflight), ADR-0012 (multipart uploads). `make test-chaos` before
every commit that touches the write path or the state machine.

| Step | Status | What | Acceptance criterion | Files |
|---|---|---|---|---|
| 6.3 | done | Presigned URLs and signed streaming SigV4 | AWS documentation vectors verify; SDK presigner in the compat suite; fuzz tests | `internal/adapters/s3api/{sigv4,chunked,payload,handler}.go` |
| 6.0 | in_progress | Foundations (entry schema 2 and the leader's time: done; extent refactor, routing table, strict XML: todo) | entry schema v2 gate (`MetadataVersion` 2, state schema 2 written by the first v2 entry); leader-stamped time `entry.At` with a high-water mark in the state; `applyEntry` receives the Raft index; `object.Extent` refactor of shard storage; S3 routing table (`s3api/routes.go`) with per-route query and header allow-lists; strict bounded XML decoder | `internal/adapters/raftmeta/{entry,fsm,store}.go`, `internal/app/storage.go`, `internal/domain/object/`, `internal/adapters/s3api/` |
| 6.2 | in_progress | Versioning | `objects/<bucket>/<key>` unchanged (current, non-marker version); new nested `versions/<bucket>` keyed `enc(key) ‖ ^seq ‖ vid` for noncurrent versions and delete markers; `ensureDataIndex` walks versions; bucket versioning state; `versionId` on GET/HEAD/DELETE; ListObjectVersions; DeleteObjects; heal and scrub walk versions; upgrade test from an M5 state (unit and with the previous binary); property test of the state machine against a model | `internal/domain/{object,bucket}/`, `internal/ports/metadata.go`, `internal/adapters/raftmeta/`, `internal/app/{objects,heal,scrub,buckets}.go`, `internal/adapters/s3api/` |
| 6.4 | todo | Object Lock (governance, compliance, legal hold) | enabled only at bucket creation (turns versioning on); default retention; checks in the domain, called by the use case and, authoritatively, by the state machine on the leader-stamped time; a compliance-locked version cannot be deleted or shortened by anyone, root included; `s3:BypassGovernanceRetention` is never granted by a wildcard; property test over arbitrary command sequences | `internal/domain/object/lock.go`, `internal/domain/bucket/lock.go`, `internal/adapters/raftmeta/`, `internal/app/`, `internal/adapters/s3api/` |
| 6.1 | todo | Multipart upload | each part its own encrypted, erasure-coded extent; one KMS data key per upload, HKDF-derived key per part; `uploads` and `parts` buckets; Complete validates and commits atomically at apply; range reads across parts; `md5-of-md5s-N` ETag; unlisted and aborted parts collected; UploadPartCopy deferred (501) | `internal/app/multipart.go`, `internal/domain/object/`, `internal/adapters/raftmeta/`, `internal/adapters/s3api/` |
| 6.5 | todo | SSE-KMS and SSE-C | alongside SSE-S3; SSE-C key combined with a KMS data key by HKDF and never stored (key MD5 and a check value are); refused without TLS, unsigned, or in a presigned query; SSE-KMS with the one configured key binds an encryption context | `internal/app/objects.go`, `internal/domain/object/`, `internal/adapters/s3api/` |
| 6.7 | todo | Bucket policy, CORS, tagging | resource policy with Principal as a type of its own; anonymous access only when a bucket policy grants it, and only for read actions; explicit deny wins across identity and bucket policies; a key's inline policy still bounds it; root not bound; CORS preflight without authentication and without an existence oracle; object and bucket tags | `internal/domain/iam/`, `internal/domain/bucket/`, `internal/app/`, `internal/adapters/s3api/` |
| 6.6 | todo | Lifecycle | expiration by days or date, noncurrent version expiration, abort of incomplete uploads, expired delete markers; worker on the metadata leader; deletes go through the normal use cases as a service principal and never bypass Object Lock; transitions and tag or size filters refused | `internal/domain/bucket/lifecycle.go`, `internal/app/lifecycle.go` |
| 6.9 | todo | CopyObject, UploadPartCopy, ListObjects v1, conditional headers | added: tools in 6.8 need them | `internal/app/`, `internal/adapters/s3api/` |
| 6.8 | todo | aws-cli and ceph/s3-tests in the compat suite | server started with four drives; tools run as processes in a `compat` image stage pinned by digest and commit; one Go test per aws-cli behaviour in `baseline.txt`; a selected, explicit list of s3-tests with its own baseline | `test/s3compat/`, `Dockerfile`, `scripts/s3compat.sh` |

## Milestone 7 — Deployment hardening

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 7.1 | todo | Helm chart; listeners (probes) open before the node has joined its cluster | StatefulSet + headless Service, PVC per drive, PDB, topology spread, probes, requests/limits, `restricted` PSS; manifest validation in `make helm-lint` | `deploy/helm/cactusfs/`, `scripts/helm-lint.sh` |
| 7.2 | todo | Default-deny NetworkPolicy | only S3, admin, node RPC and Raft ports allowed | `deploy/helm/cactusfs/templates/` |
| 7.3 | todo | Signed multi-arch images and SBOM | manifest list pushed, signature and SBOM attached | `Makefile`, `.github/workflows/release.yml` |
| 7.4 | todo | Observability pack | dashboards and alert rules for `cactusfs_*` | `deploy/` |
| 7.5 | todo | Certificate rotation without restart | reload on change | `cmd/cactusfs/` |
| 7.6 | todo | Drain delay default in the chart | `--drain-delay` (implemented in 0.3) set from the chart to cover the probe period; `preStop` not needed | `deploy/helm/cactusfs/` |

## Milestone 8 — Hybrid

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 8.1 | todo | Asynchronous bucket replication between sites | never one Raft quorum across a WAN; resumable; conflict rule in an ADR | `internal/app/replication.go`, `docs/adr/` |
| 8.2 | todo | Bucket notifications | NATS JetStream and webhook adapters; integration test via testcontainers | `internal/adapters/events/{nats,webhook}/` |
| 8.3 | todo | Quotas | per-bucket hard quota enforced on the write path | `internal/app/quota.go` |

---

## Decisions

| Date | Decision | Reason |
|---|---|---|
| 2026-10-07 | Go 1.27.1 pinned in `go.mod`; builder image runs with `GOTOOLCHAIN=local`. | Latest stable on this date; the builder must not silently fetch another toolchain. |
| 2026-10-07 | One `Dockerfile` holds the product stages (`web`, `build`, final) **and** the `builder` tool stage. | One place pins the Go and Node images for both the product and the tools `make` runs with. |
| 2026-10-07 | Go tools `gofumpt`, `goimports`, `govulncheck`, `go-licenses` are `tool` directives in `go.mod`. `golangci-lint`, `helm`, the Docker CLI, Node and pnpm are pinned by digest/version in the `builder` stage. | §2 asks for `tool` directives. golangci-lint's dependency graph is too large to share one `go.mod` with the product, and the others are not Go tools. None of them is needed for `go build`. |
| 2026-10-07 | The binary has no third-party Go dependency in milestone 0 (`flag`, `net/http`, `log/slog`). | §5 "prefer the standard library". |
| 2026-10-07 | `/healthz` and `/readyz` live in a new inbound adapter `internal/adapters/health`, mounted on the admin listener (`:9001`). | They are operational endpoints, not part of the versioned, generated admin API. |
| 2026-10-07 | Probe endpoints are unauthenticated and return only `{"status":"ok"\|"unavailable"}`; failure detail goes to the log. | Orchestrator probes cannot hold credentials. Stricter alternative (authenticated probes) is an open question for Albie. |
| 2026-10-07 | `server` refuses to start without `--tls-cert/--tls-key` unless `--dev-plaintext` is set; plaintext accepts only a loopback **IP literal** (no host names). | §3 encryption in transit, stricter interpretation. |
| 2026-10-07 | TLS minimum is 1.2 with Go's default suites; 1.3 is negotiated when the client supports it. | §3. |
| 2026-10-07 | `make verify-clone` exports the working tree through a temporary index and object directory (`git add -A` → `git write-tree` → `git archive`), not `HEAD`. | The gate must be able to pass *before* the commit it guards, and must not write into `.git`. |
| 2026-10-07 | `make image` builds one local image per architecture (`cactusfs:<v>-amd64`, `-arm64`) and checks each is < 60 MB. A pushed manifest list is step 7.3. | A multi-arch manifest cannot be loaded into every local image store; per-arch builds work unchanged on Docker and Podman. |
| 2026-10-07 | Console scaffold exists in milestone 0 but is not embedded or served until 5.4. | The `Dockerfile` needs the Node stage and the frontend gate needs a subject; the goal limits `server` to the two probe endpoints. |
| 2026-10-07 | TypeScript 6.0.x rather than 7.x. | `typescript-eslint` 8 supports `< 6.1`. |
| 2026-10-07 | No `LICENSE` file. | Project licence is still open (§11). |
| 2026-10-07 | `--drain-delay` (default `0s`) was pulled forward from 7.6 into 0.3. | Review finding: without a delay the listener closes at once and the `/readyz` 503 can never be observed, so the 0.3 criterion was untestable. Default stays `0s` so local runs stop immediately. |
| 2026-10-07 | TLS 1.2 is limited to ECDHE suites with AES-GCM or ChaCha20-Poly1305. | Go's default list also accepts CBC-SHA1 suites; §3 stricter interpretation. |
| 2026-10-07 | `.golangci.yml` excludes gosec G204 ("subprocess launched with a variable") under `test/` only. No other exclusion and no `//nolint` exists. | The black-box tests exist to execute the binary they have just built; the finding cannot be fixed without deleting the test. Listed as an open question for Albie because it is a silenced finding. |
| 2026-10-07 | `make lint` runs `scripts/no-bypass.sh`, which rejects `//nolint`, `t.Skip*` and `testing.Short` in Go sources. | §10 as a gate rather than a convention. |
| 2026-10-07 | The licence gate checks the Go dependencies of all six release platforms and walks the console's production and optional dependencies. | A build-tagged dependency for Windows or macOS would otherwise ship unchecked. |
| 2026-10-07 | The console placeholder shows `cactusfs-logo.svg` and links `favicon.svg`; brand files are committed as supplied. | "Brand assets" rule added to `CLAUDE.md` §2 during this milestone. |
| 2026-10-07 | M1 dependencies: `hashicorp/raft` v1.8.0 and `raft-boltdb/v2` v2.4.2 (MPL-2.0), `go.etcd.io/bbolt` v1.5.0 (MIT), `golang.org/x/crypto` (BSD-3, Argon2id), `golang.org/x/sys` (BSD-3, Windows file lock), `prometheus/client_golang` v1.24.1 and OpenTelemetry v1.47.0 (Apache-2.0); test-only `aws-sdk-go-v2` (Apache-2.0). | Raft, bbolt, Prometheus and OTel are named by `CLAUDE.md`; Argon2id and the Windows lock have no standard-library equivalent. Planner verified licences and six-platform `CGO_ENABLED=0` builds. |
| 2026-10-07 | `ports.EventPublisher` is not introduced in M1; it arrives in 3.6 with its AsyncAPI contract. `IDGenerator` is replaced by an injected `io.Reader` entropy source. | Contract-first rule for events; fewer ports with one implementation. |
| 2026-10-07 | Stored-object metadata is erasure-shaped from the start: `Layout{DataShards:1, ParityShards:0, StripeSize, StoredSize, Shards[{DriveID, Size}]}`. Shard files carry a checksummed header and a SHA-256 per block bound to data ID, shard index and block number. | So that milestone 2 (erasure coding, heal, scrub) changes values, not the on-disk format or the Raft entry schema. |
| 2026-10-07 | Stream AAD is `index ‖ final ‖ caller AAD` (fixed-width fields first); caller AAD and the KMS context are the data ID. | Unambiguous layout; ciphertext and wrapped key cannot be moved to another object. |
| 2026-10-07 | Raft runs with `NoSnapshotRestoreOnStart`; the FSM keeps its state in its own bbolt file and stores the last applied index in the same transaction as each change. | The state DB is already durable: restoring an older snapshot over it on every start would be wrong, and replay must be idempotent. |
| 2026-10-07 | `STREAMING-UNSIGNED-PAYLOAD-TRAILER` (aws-chunked with trailing checksum) is implemented in 1.8, not 6.3. Signed streaming and presigned URLs stay in 6.3. | It is what current AWS SDKs send by default over TLS, and the server is TLS-only. |
| 2026-10-07 | ETag is the MD5 of the object, computed in one file of the S3 adapter (`internal/adapters/s3api/md5.go`); `.golangci.yml` excludes gosec G401/G501 for that file only. | S3 clients compare ETag and `Content-MD5` with MD5; it is a protocol checksum here, not a security primitive (integrity is AES-GCM plus SHA-256). A silenced finding: open question for Albie. |
| 2026-10-07 | `/metrics` requires `Authorization: Bearer <access-key-id>:<secret>` and passes `ports.Authorizer` (`admin:GetMetrics`). | §3 stricter reading: every inbound request is authenticated. Prometheus can send a bearer credential. The token format is provisional until the admin API contract (5.1). |
| 2026-10-07 | Generated root credentials are printed once to stderr, never to the JSON log and never to a file. | "Shown once"; a file would be a secret stored unencrypted. Container log drivers capture stderr, so production should supply the credentials: open question for Albie. |
| 2026-10-07 | Authentication failures and denials are logged (without secrets) until the hash-chained audit log exists (4.6). | Audit trail is milestone 4. |
| 2026-10-07 | In M1, shards of failed, replaced and deleted objects are deleted best-effort after the fact; a crash can leave orphans until the collector of 2.7. | Orphans waste space but are never visible: the Raft commit is the only visibility switch. |
| 2026-10-07 | govet's optional `shadow` analyzer is disabled in `.golangci.yml` (all other optional analyzers stay on, except `fieldalignment`). | It is not part of `go vet` and rejects the idiomatic `if v, err := f(); err != nil` scoped error. It flagged exactly that idiom twice in test code the first time `enable-all` met real code. |
| 2026-10-07 | Secrets (KMS passphrase, root credentials) are read from the environment or a file only; they have no command-line flag. | A command line is visible to every user of the machine. This narrows "matching flags" in §2 for secrets. |
| 2026-10-07 | A missing local KMS key file is created only when the metadata directory is unused; on an existing deployment it is an error. | Creating a new key at a mistyped path would leave every object unreadable while the node looked healthy. |
| 2026-10-07 | The S3 listener has header and idle timeouts but no whole-request or whole-response timeout; the admin listener bounds both. | An object transfer may legitimately take hours. Slow-body abuse of the S3 listener is an open risk until per-read deadlines are added (threat model). |
| 2026-10-07 | S3 operations not implemented yet answer `501 NotImplemented` after authentication (ACLs, versioning, multipart, tagging, copy, multi-delete, ListObjects v1, presigned and signed-streaming requests); a recognised sub-resource is never treated as the plain operation on the same path. | A `DELETE /key?uploadId=…` served as DeleteObject would destroy data. |
| 2026-10-07 | SHA-1 trailing or header checksums are refused (`InvalidRequest`); CRC32, CRC32C, CRC64NVME and SHA-256 are verified. | gosec rejects SHA-1, and adding a second weak-hash exclusion is not justified while SDK defaults are CRC-based. |
| 2026-10-07 | SigV4 test vectors from the AWS documentation live in `internal/adapters/s3api/testdata/sigv4-examples.json`. | They contain AWS's public example key pair, which secret scanners flag when written in source. |
| 2026-10-07 | The compatibility suite builds and starts the real binary over TLS with a generated CA, and configures the SDK's default checksum behaviour explicitly instead of loading AWS configuration. | `cmd/cactusfs` is the only wiring point; the suite must not read the developer's AWS files; and a hand-built `aws.Config` otherwise silently turns the SDK's default checksums off (found when a test that should have exercised aws-chunked did not). |
| 2026-10-07 | `--otlp-endpoint` must be `https`, or `http` to a loopback IP. | Encryption in transit also applies to what the node sends out. |
| 2026-10-07 | Bucket names, object keys and object metadata are not encrypted in the metadata store. | §3 requires object encryption; encrypting the index is a product decision with a listing-performance cost. Open question for Albie. |
| 2026-10-07 | Review of milestone 1 (2 blocker, 5 major, 6 minor) — all fixed, each with a regression test that fails when the fix is removed. | Recorded so the reasoning survives: see the next rows. |
| 2026-10-07 | The stream encryptor ends the plaintext only on `io.EOF`; `io.ErrUnexpectedEOF` from the source is an error (`IncompleteBody`). | Blocker: a chunked or length-declared upload cut short by the client was sealed as final and committed, replacing the existing object. |
| 2026-10-07 | `domain.ErrOutcomeUnknown`: an error from the metadata store is a definite "not committed" only for state-machine refusals and errors Raft returns before logging. Otherwise the entry may still be applied, and nothing it refers to is cleaned up. The state machine is fail-stop: once an entry cannot be applied, no further entry is logged. | Blocker: `Put` deleted the new shard on any commit error, although the entry could be applied at the next start — leaving the index pointing at no data and, for an overwrite, the previous version unreferenced. |
| 2026-10-07 | S3 routing is an allow-list: a query parameter or `x-amz-*` header the routed operation does not know answers `501`; a query string that does not parse answers `400`; conditional headers (`If-Match`, `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since`) answer `501` until implemented (6.x). | Major: the deny-list missed new sub-resources, `;` in a query dropped the pair, and `If-None-Match: *` or object-lock headers were silently ignored. |
| 2026-10-07 | `Get` looks again when the shard of the version it read is gone and the object has since changed. | Major: a read racing an overwrite or delete failed with 500. |
| 2026-10-07 | At start the metadata store restores the newest snapshot when the state database is older than it. | Major: with `NoSnapshotRestoreOnStart`, a lost or stale `state.db` came up healthy with only the post-snapshot log applied. |
| 2026-10-07 | `serve` returns only when every request handler has returned; the metadata store refuses use after `Close` and `Close` waits for operations in flight. If handlers do not return within 60 s the process exits without closing the stores. | Major: after a shutdown timeout the stores were closed under running handlers. |
| 2026-10-07 | The KMS key file and `node.json` are created with their directory entry flushed (`internal/adapters/fsutil`). | Major: a power loss after first start could lose the key file while acknowledged objects existed. |
| 2026-10-07 | `Content-MD5` must be covered by the signature when present; the CreateBucket body is verified against its declared hash; a shard whose rename could not be made durable is removed again; the local KMS key ID format is validated. | Minor review findings. |
| 2026-10-07 | M2 dependency: `github.com/klauspost/reedsolomon` v1.14.2 (MIT; brings `klauspost/cpuid/v2`, MIT). Pure Go, builds for all six targets. | Named by `CLAUDE.md` §2. |
| 2026-10-07 | Erasure sets must be of equal size: the number of drives is divided into the fewest sets of at most 16; a count that does not divide evenly (17, 19, …) is refused at start. | Unequal sets would have different durability; refusing is explicit. |
| 2026-10-07 | Drives are bound to slots (set, position) in Raft metadata by drive ID at first start. An object's layout names drive IDs. All drives must open at start. (Automatic adoption of an empty drive, decided here first, was withdrawn after review: see below.) | Addressing by ID makes `--data` order irrelevant. Starting with a drive offline is deferred (risk: a dead drive needs replacing before restart). |
| 2026-10-07 | Review of milestone 2 (3 blocker, 5 major, 4 minor) — all fixed, each with a regression test that fails when the fix is removed; three of the blockers are also reproduced as chaos scenarios. ADR-0004 was revised in place before the milestone closed. | Recorded so the reasoning survives: see the next rows. |
| 2026-10-07 | After healing, shards are removed only when a lookup succeeds and shows the version replaced or gone, and only the shards that heal rebuilt. A failed lookup removes nothing. | Blocker: a lookup cancelled by shutdown was read as "replaced" and every shard of a healthy object was deleted. |
| 2026-10-07 | A deployment has a random ID; every drive is claimed for it in `cactusfs-drive.json` (`cluster_id`, reserved by ADR-0001). A new deployment needs drives that are all empty and unclaimed. | Blocker: with an empty metadata directory, drives full of data were adopted as a new deployment and the collector deleted every shard. |
| 2026-10-07 | Replacing a drive needs parity and the operator's confirmation for that start (`--allow-drive-replacement`); without parity it is never done. "Empty" is read from the drive (no shard file), not from having formatted it. | Blocker: an unmounted mount point looked like a new drive, took the slot and retired the real drive for good, even on a one-drive node. Major: a start interrupted after formatting left a deployment that could not start. |
| 2026-10-07 | Healing rebuilds a shard only on definite damage (`ErrShardNotFound`, `ErrIntegrity`) and through `ShardStore.Rewrite`, which replaces the old content in one rename. One heal per object at a time. | Major: any open or read error was treated as damage and the shard deleted before a replacement existed. |
| 2026-10-07 | Writes pin their data ID until the commit outcome is definite (unknown outcomes for the life of the process); the collector skips pinned data and does nothing while the metadata is not ready; `IsDataReferenced` fails on a stopped state machine. Minimum `--gc-grace` is 1 h. | Major: the grace period alone stood between a slow upload, or an unapplied log entry, and the collector. |
| 2026-10-07 | The scrubber is throttled as it reads and counts only verified bytes. A shard is still given up for the rest of a read after one bad block (ADR-0004, consequences). | Minor review findings; the second is a recorded limitation, not fixed. |
| 2026-10-07 | Milestone 3. Every node runs Raft: `meta` nodes vote, the others are non-voters with a local replica. Changes are forwarded to the leader; reads are linearizable on every node by read index. ADR-0005. | Local reads and listings on every node, one code path for a node alone and a cluster. Reads CLAUDE.md's "stateless gateway" as "holds nothing that cannot be rebuilt" — **open question for Albie**; the alternative is gateways that ask a voter for every metadata read. |
| 2026-10-07 | The leader proves leadership by asking the voters for their Raft term (majority, itself included), not with `raft.VerifyLeader`; reads wait on the state machine's own applied command index, not `raft.AppliedIndex`. | Found by the multi-node tests: `VerifyLeader` counts answers of non-voters, so a leader cut off from the other voters but not from a storage node would pass and serve stale reads; `AppliedIndex` advances before the state machine has applied. |
| 2026-10-07 | A node's identity is its certificate: the node ID is the first DNS name, clients verify it as `ServerName` whatever the address, Raft members are recorded as `<id>@<host:port>`. The node CA signs node certificates only. No plaintext node RPC or Raft, in development either. | Addresses and DNS are not trusted to say who is behind them; stricter reading of §3 mTLS. **Open question:** certificates are read at start (rotation = restart) and there is no revocation. |
| 2026-10-07 | Every node RPC response carries an `Error`; a failed RPC means "not reached or no answer". Domain sentinels cross nodes as stable names (`domain.Codes`). | The write path must tell "definitely not committed" from "outcome unknown"; only an answer is definite. |
| 2026-10-07 | A cluster forms once, from `--peers` (the complete node list): voters bootstrap with the identical member list when every node has answered and none knows a deployment; the leader then surveys all drives and records the topology. 3 or 5 voters and at least 4 storage nodes are enforced. Storage cannot be added to a formed cluster yet. | Identical bootstrap needs every node's self-description; a deployment ID anywhere means "join, never form". Expansion needs new erasure sets and rebalancing — its own milestone. |
| 2026-10-07 | Topology slots carry their node; a node reconciles only its own drives at start; an unreachable drive is `ErrDriveOffline` — not damage, not a reason to rewrite or delete; requests that fail only for lack of reachable nodes answer 503. | A node that is down must not keep the others from starting, and must never be mistaken for lost data. |
| 2026-10-07 | Scrubbing runs on the metadata leader only; garbage collection on every storage node for its own drives, asking every recorded node for writes in progress (unreachable = yes), and looking at the index again afterwards. | One scrubber for the cluster; the second look at the index closes the gap in which a write commits and unpins between the two questions (the order in milestone 2 had that gap behind the grace period). |
| 2026-10-07 | `MetadataVersion` per node; nobody writes an entry format newer than the oldest member's; the leader re-checks. | CLAUDE.md §2: version N must run alongside N+1. Tested with a stand-in for release N+1. |
| 2026-10-07 | Events: derived in the state machine after commit, IDs from the entry index; in-process publisher is bounded, never blocks, at most once per node. AsyncAPI validated with `@asyncapi/specs` + `ajv` (dev dependencies of `web/`), not `@asyncapi/parser`. | The state machine must never wait for a consumer. The parser pulls in ~160 packages and an install-time analytics package; the schema, `ajv` and a `$ref` check do the job in 9. |
| 2026-10-07 | New dependencies: `google.golang.org/grpc` and `protobuf` (already linked through the OTLP exporter; now direct), `protoc-gen-go` and `protoc-gen-go-grpc` as `tool` directives, `buf` pinned by digest in the builder, `testcontainers-go` (tests only; not linked into the binary). | Node RPC contract and its breaking-change gate; real multi-node integration and chaos tests. |
| 2026-10-07 | Until the Vault adapter (4.4), the nodes of a cluster share one sealed KMS key file; a cluster member never creates a key file. `cactusfs dev-init` writes it with throw-away certificates for development, and never overwrites. | Every node must unwrap every object's key. Creating a key per node would make objects unreadable through other nodes. |
| 2026-10-07 | The image ships `/var/lib/cactusfs/{data1..4,meta,config}` owned by UID 65532. | A named volume takes the ownership of the directory it is first mounted on; without them the non-root process cannot write to its volumes. |
| 2026-10-07 | In a cluster `CommitTimeout` is 10 ms; listeners (probes included) still open only after a node has joined its cluster. | Followers learn of commits with the leader's next message, and every read on a non-leader waits for that. Early probes are needed for Helm and moved to 7.1. |
| 2026-10-07 | Review of milestone 3 (2 blocker, 3 major, 9 minor, 2 nit). Fixed: a topology recorded before slots named their node is migrated at first start (upgrade test, and proven with the previous commit's binary); PLAN statuses; the Raft port admits voters only and completes its handshake with a deadline; only a node may propose its own record; the leader decides who votes and the voters are fixed at formation; tests that pin leadership confirmation by voters and a new leader's catch-up (each kills the mutant the reviewer found alive); node certificates carry exactly one name; the state machine validates a node record only by rules every release shares; generated root credentials are shown even when their commit was uncertain; `noderpc` no longer imports `nodetls`; compose and README wording. | Recorded so the reasoning survives. |
| 2026-10-07 | Deferred from that review, with reasons: any node certificate may still propose bucket, object and topology changes (narrowing by role belongs to IAM, milestone 4; stated in ADR-0005 and the threat model); the dev compose cluster shares one config volume, so nodes can read each other's keys (stated in the file); three tests assert "nothing happens" with short fixed sleeps; a certificate with two names is refused by code that no test exercises with such a certificate. | Honest list of what was not done. |
| 2026-10-08 | Milestone 4. A request's identity - key, owner, every policy that applies - is resolved in one linearizable read at authentication and travels in the principal; `iam.Decide` is the only place a decision is made. ADR-0006. | One read-index round per request; revocation, disabling and role changes take effect with the next request on every node. |
| 2026-10-08 | Policy parsing is strict (unknown, repeated or differently-cased fields, unknown operators and keys, empty conditions are errors). Conditions on an unknown source address are decided against the request in both Allow and Deny. | A policy must mean what it reads; the fuzzers and the review each found a way it did not (a literal `*` in a key; duplicate fields; a Deny that failed open). |
| 2026-10-08 | Built-in roles and policies are part of the binary, not of the state; the proposer vouches for built-in names in the entry so that the state machine decides nothing a release could decide differently. | Same meaning everywhere, extendable by upgrade, no divergence between replicas of two releases. |
| 2026-10-08 | IAM, session and audit commands are added to entry schema version 1, not a version 2. | No release has shipped version 1. A released schema would need a new version and the rolling-upgrade rule of ADR-0005. |
| 2026-10-08 | Changes to identities carry their audit record in the same Raft entry. Refusals and other administrative changes are appended in the background; refusals are coalesced by what the caller cannot vary and capped per window; under flood a record per refusal can be dropped (counted, logged), a change never. | Atomic where it matters most; an unauthenticated client must not be able to grow the replicated log one request at a time. **Open question for Albie:** CLAUDE.md §3 says every admin mutation, authentication failure and denial is an audit event - read strictly that allows no dropping at all, which would let a flood stop the store. |
| 2026-10-08 | Vault Transit through `net/http`, no Vault SDK; data keys are made on the node and wrapped with `encrypt` + associated data; a key type that cannot bind associated data is refused at start; token from a file, re-read at every use; no redirects, no proxy. | Smallest dependency and attack surface; the SDK is MPL-2.0 and large. Token renewal and other auth methods are left to the platform. |
| 2026-10-08 | Console passwords: Argon2id `m=65536,t=3,p=4` behind `ports.PasswordHasher` (the domain may not import `x/crypto`), at most four hashes at once. Sessions: 30 minutes, fixed, replicated, stored as hashes; `__Host-` cookie; CSRF token per session. | RFC 9106's second recommended set; the cap bounds memory when sign-in becomes reachable in milestone 5. No lockout or rate limit yet. |
| 2026-10-08 | The root credential is read once per process and stays the way back in: it has no policy and cannot be locked out. | It is written once and never changes; a policy mistake must not be unrecoverable. |
| 2026-10-08 | Review of milestone 4 (3 blocker, 6 major, 9 minor). Fixed: denials with long names were not audited; repeat counts lost at a window boundary; PLAN statuses; flooding the trail by varying the claimed key or the object; Deny conditions that failed open (unknown address, zoned IPv6, IPv4-mapped ranges); duplicate, differently-cased and empty fields accepted by the parser; audit records that did not say what was granted; bucket, bootstrap, topology and registration changes not audited; tests for owner-scoped key permissions, the Vault token's destination, the node's clock, literal wildcards. Also: last use only after the signature verified; narrowed keys cannot make keys; refusals do not name a key's owner; Vault's refusal (not an error on the way) proves the binding; Argon2id concurrency cap. | Recorded so the reasoning survives. |
| 2026-10-08 | Deferred from that review: `UpdateUser` without compare-and-set, and a password change not ending sessions; the built-in-immutable rule lives in the store adapters; no test that a failed `/metrics` authentication is audited; sign-out of a session that is already gone writes a record; the map of touched keys is never pruned. | Honest list of what was not done; the first is stated in ADR-0006. |
| 2026-10-08 | Raft replicates one request at a time per follower (`MaxRPCsInFlight: 1`, no pipelining), and a node that stops closes the Raft connections it accepted. | Found by a test that hung for 30 minutes: with pipelining, a follower that takes what it is sent and never answers blocks the leader's replication goroutine for good, and Raft's shutdown waits for that goroutine. Costs replication throughput, which metadata does not need; not benchmarked. |
| 2026-10-08 | Raft learns a node's advertised address from the stream layer's `Addr()`, not from an address provider that answered "use the configured address" for every other node. | Without pipelining that provider made Raft log a warning on every replication request, which drowned the node log (and hid the cause of a chaos failure). |
| 2026-10-08 | Milestone 5, ADR-0007. OpenAPI 3.0.3 (highest `oapi-codegen` 2.8.0 fully supports); strict generated server; which operations are public is read from the contract. `oasdiff` 1.33.0 comes into the builder image pinned by digest, not as a Go tool. | A Go `tool` directive for oasdiff would add its whole dependency tree to `go.mod`; the image is the same pinning with none of it. |
| 2026-10-08 | New dependencies: `github.com/oapi-codegen/oapi-codegen/v2` (tool, Apache-2.0), `github.com/oapi-codegen/runtime` (Apache-2.0, parameter binding of the generated server); in `web/`: `openapi-fetch` (MIT, runtime) and `openapi-typescript` (MIT, generator). Swagger UI 5.33.1 (Apache-2.0) is three committed files, not a dependency. | Required by CLAUDE.md §2 (generated handlers and client, embedded Swagger UI). |
| 2026-10-08 | `Idempotency-Key`: reserve, carry out, store the answer - two Raft entries around the change, answers sealed through the KMS, scoped to the caller, kept 24 h. Not atomic with the change. Four new commands in entry schema version 1. | The answer to creating a key holds a secret, so it may not be stored in the clear. Atomicity would put a record into every mutating entry; deferred, and it can be added compatibly. |
| 2026-10-08 | Objects stored through the management API get a SHA-256-derived entity tag marked `-1`; downloads are always attachments of an opaque type. | MD5 stays confined to the S3 adapter; an uploaded HTML file must never run on the console's origin. |
| 2026-10-08 | Sign-in attempts are limited per address and node, in memory (10 per 5 minutes). No per-user lockout. | A lockout by user name is a way to lock a user out on purpose. **Open question for Albie.** |
| 2026-10-08 | Sign-in, the contract, `/docs` and the console's files need no credential. | They cannot: nobody has one yet. **Open question for Albie** against "every inbound request is authenticated". |
| 2026-10-08 | Maintenance status is per answering node; the console is one bundle with a hash router and no dependencies beyond the generated client. | A cluster-wide view needs nodes to ask each other; left for later. |
| 2026-10-08 | Review of milestone 5 (0 blocker, 3 major, 9 minor). Fixed: uploads and downloads were recognised by a path ending in `/object`, so an entity of that name escaped the body limit, the media-type check and the request fingerprint - now by the matched operation; idempotency reservations were deleted by ID alone, so a slow request could undo the repeat that took over - the delete now names the reservation's time, and a reservation goes stale after 15 minutes, not 2; failed sign-ins were counted after the password check, so parallel guesses were not limited - now before, and the queue for password hashing is bounded. Also: HEAD of an object is 405; a test pins the exact set of public operations; tests for a user named `object`, bearer and cookie together, session and key scopes, CSRF token not in logs. | Recorded so the reasoning survives. |
| 2026-10-08 | Deferred from that review: sign-in limit keyed on the peer address only (shared behind a proxy; no trusted-proxy setting, no per-user count); parameters bound before authentication (400 instead of 401, unaudited); no cap on idempotency records per caller, and the console sends a fresh key on every change; upload fingerprint without content; no progress timeout on transfers; a new sign-in does not end the previous session; cookies shared between the S3 and admin ports. All in ADR-0007 under "Known limits". | Honest list of what was not done. |
| 2026-10-08 | Step 6.3. Presigned URLs (SigV4 in the query string, 1 second to 7 days) and signed aws-chunked uploads (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, with and without signed trailers). A request with both a presigned query and an Authorization header is refused. Signed `x-amz-*` query parameters of a presigned URL are treated as the headers they stand for. A signed `content-length` header is taken from the request's length. Signature Version 2 and SigV4A stay unsupported. | Verified against the worked examples of the AWS documentation (presigned GET and the three chunk signatures) and with the AWS SDK's presigner in the compat suite. A presigned URL acts as its signer does at the time of use: revoking the key ends its URLs. |
| 2026-10-08 | Milestone 6 goes to Raft entry schema version 2 and state schema 2; this supersedes "extend version 1" for commands whose meaning or payload changes. | M4 and M5 only added command types, which an older binary stops on. M6 changes what `object.put`, `object.delete`, `bucket.create` and `bucket.delete` mean, and entries are decoded leniently: an older binary would drop a retention field and diverge silently. The version gate and the rolling-upgrade test already exist (ADR-0005). |
| 2026-10-08 | Versions are added beside the existing object index, not by re-keying it: no rewrite of stored data, but an ADR and an upgrade test, because existing records are reinterpreted (as null versions) and the rebuild of the shard reference index must walk the new bucket. | A rebuild that forgot noncurrent versions would let the collector delete their shards. |
| 2026-10-08 | Object Lock is decided in the state machine on a time the metadata leader stamps into the entry, kept monotonic by a high-water mark in the state. | App-level checks race across gateways, and replicas must agree. **Open question for Albie:** a voter with a fast clock that becomes leader ends retention early; the stricter option (minimum clock of a voter majority for time-relaxed deletes) is not built yet. |
| 2026-10-08 | Stricter readings chosen for milestone 6: `s3:BypassGovernanceRetention` is never matched by a wildcard; a bucket policy can grant anonymous access to read actions only; an SSE-C key is combined with a KMS data key, never used alone; security-relevant XML is parsed strictly; root is not bound by bucket policies. | CLAUDE.md §3: when ambiguous, the stricter interpretation, flagged. **Open questions for Albie**, each. |
| 2026-10-08 | Step 6.9 added (CopyObject, UploadPartCopy, ListObjects v1, conditional headers); DeleteObjects joins 6.2. | aws-cli and s3-tests in 6.8 use them. |
| 2026-10-08 | Step 6.2. Versions as designed in ADR-0008. The routing table and the object-extent refactor of 6.0 were not needed for it and move to the steps that need them (multipart). `ports.ObjectIndex.PutVersion` and `DeleteObject` return what was stored, removed and marked; `ObjectService.Delete` takes an input and returns an output. A GET of the current version whose newest version is a delete marker answers 404 without the `x-amz-delete-marker` header. | Smallest change that is right for SDK and CLI paths; the missing header is cosmetic and recorded here. |


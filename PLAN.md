# CactusFS — PLAN

Source of truth for **what** is being built and **where we are**. Rules live in
`CLAUDE.md`; the design lives in `docs/ARCHITECTURE.md`.

Status values: `todo | in_progress | done | blocked`.
A step is `done` only when its acceptance criterion holds **and** `make check` is green.

## Status at a glance

| Milestone | Title | Status |
|---|---|---|
| 0 | Scaffold | done |
| 1 | Standalone store | todo |
| 2 | Erasure coding and integrity on local drives | todo |
| 3 | Cluster | todo |
| 4 | IAM | todo |
| 5 | Admin API, Swagger UI, console | todo |
| 6 | S3 depth | todo |
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
| `make test-int` | black-box test of the real binary; no testcontainers until there is something to contain | 3.7 (cluster), 4.x (Vault), 8.2 (NATS) |

`make up`, `make down` and `make test-chaos` exist and exit non-zero with a pointer to the
step that implements them (`up`/`down`: 3.8; `test-chaos`: 2.8 and 3.9). They are not part of
`make check`.

---

## Milestone 1 — Standalone store

Done when a single `cactusfs server --data <dir>` passes the milestone's S3 compatibility
subset with encryption on.

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 1.1 | todo | Domain model: bucket, object, version, IDs, typed errors | naming rules per the S3 reference; property tests; no external imports | `internal/domain/{bucket,object}/`, `internal/domain/errors.go` |
| 1.2 | todo | Ports: `ObjectIndex`, `BucketStore`, `ShardStore`, `KMS`, `Clock`, `IDGenerator`, `EventPublisher` | interfaces documented; fakes in `internal/ports/porttest` | `internal/ports/` |
| 1.3 | todo | Disk adapter | shards stored under generated IDs only; write + file fsync + directory fsync per OS; versioned on-disk header; ADR for the format | `internal/adapters/disk/`, `docs/adr/0001-on-disk-format.md` |
| 1.4 | todo | Streaming AEAD and envelope encryption | AES-256-GCM segments, unique nonce, segment index in AAD; truncation/reorder detected; fuzz + property tests; ADR | `internal/domain/object/crypto*`, `internal/adapters/kms/local/`, `docs/adr/0002-encryption.md` |
| 1.5 | todo | Sealed local key file KMS (explicit opt-in) | no unencrypted fallback; key file sealed with a passphrase-derived key (Argon2id) | `internal/adapters/kms/local/` |
| 1.6 | todo | Single-node Raft metadata (bbolt) | bucket and object index committed through Raft; versioned entries; restart-safe; ADR | `internal/adapters/raftmeta/`, `docs/adr/0003-metadata.md` |
| 1.7 | todo | Use cases: bucket create/delete/list, object PUT/GET/HEAD/DELETE, ListObjectsV2 | write path order per §2; table-driven tests; coverage ≥ 80 % | `internal/app/` |
| 1.8 | todo | S3 API adapter with SigV4 (header auth) and XML errors | path-style and virtual-hosted-style; fuzz tests for SigV4 and XML; constant-time comparison | `internal/adapters/s3api/` |
| 1.9 | todo | Root bootstrap and `--data` flag | first start requires or generates root credentials, shown once; no default credentials | `cmd/cactusfs/`, `internal/app/bootstrap.go` |
| 1.10 | todo | S3 compatibility suite, first baseline | aws-sdk-go-v2 tests for the milestone's operations recorded in `baseline.txt` | `test/s3compat/` |
| 1.11 | todo | Telemetry adapter | `/metrics` (`cactusfs_*`) and OpenTelemetry traces on every adapter boundary | `internal/adapters/telemetry/` |

## Milestone 2 — Erasure coding and integrity on local drives

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 2.1 | todo | Erasure domain: sets, parity defaults, quorum rules | 4–16 drives; parity 2/3/4 by set size; write quorum = data (+1 when data = parity) | `internal/domain/erasure/` |
| 2.2 | todo | Reed-Solomon codec (`klauspost/reedsolomon`) behind a port | property test: encode → lose ≤ parity shards → decode equals original; fuzz | `internal/adapters/erasurecodec/`, `internal/ports/` |
| 2.3 | todo | Shard placement and SHA-256 checksums | shards of one object never share a drive; checksum verified on every read; failed bytes never returned | `internal/app/`, `internal/adapters/disk/` |
| 2.4 | todo | Heal on read | degraded read reconstructs and rewrites missing/corrupt shards | `internal/app/heal.go` |
| 2.5 | todo | Background scrubber | detects bitrot, heals from parity, rate-limited, metrics | `internal/app/scrub.go` |
| 2.6 | todo | Fewer than 4 drives | still encrypts and checksums; logs that there is no parity | `internal/app/`, `cmd/cactusfs/` |
| 2.7 | todo | Garbage collection of uncommitted shards | shards without a committed version are removed after a grace period | `internal/app/gc.go` |
| 2.8 | todo | Chaos: shard corruption and drive loss | `make test-chaos` passes | `test/chaos/` |

## Milestone 3 — Cluster

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 3.1 | todo | Cluster domain: nodes, roles, topology labels | `--roles` selects gateway/meta/storage/console; `region/zone/rack/host` labels | `internal/domain/cluster/`, `cmd/cactusfs/` |
| 3.2 | todo | Node RPC over mTLS 1.3 | gRPC contract in `api/proto/`; generated code committed; `buf breaking` wired into `make contracts` | `api/proto/`, `internal/adapters/noderpc/`, `buf.yaml`, `scripts/contracts.sh` |
| 3.3 | todo | Multi-node Raft (3 or 5 voters) over mTLS | join, leave, leader change; rolling upgrade N ↔ N+1 test | `internal/adapters/raftmeta/` |
| 3.4 | todo | Distributed erasure sets | shards spread across nodes, then racks/zones | `internal/app/placement.go` |
| 3.5 | todo | Quorum write and read paths | no acknowledgement before write quorum + Raft commit; read-after-write consistency | `internal/app/` |
| 3.6 | todo | Events from applied Raft entries | contract first in `api/asyncapi/events.yaml`, AsyncAPI validation wired into `make contracts`; in-process bus; consumers dedupe by event ID | `api/asyncapi/events.yaml`, `scripts/contracts.sh`, `internal/adapters/events/inproc/` |
| 3.7 | todo | Integration tests on a real cluster | testcontainers-go multi-node cluster | `test/integration/` |
| 3.8 | todo | Compose deployment | 4-node cluster; unchanged on `docker compose` and rootless `podman compose`; `make up/down` | `deploy/compose/compose.yaml`, `Makefile` |
| 3.9 | todo | Chaos: node kill, partition, heal verification | `make test-chaos` passes | `test/chaos/` |

## Milestone 4 — IAM

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 4.1 | todo | IAM domain: users, groups, roles, policies | IAM-style JSON; default deny; explicit deny wins; fuzz + property tests for policy parsing | `internal/domain/iam/` |
| 4.2 | todo | `ports.Authorizer` and enforcement on every inbound request | no authorization logic in handlers; key never exceeds its owner | `internal/ports/`, `internal/app/authz.go` |
| 4.3 | todo | API keys | secrets envelope-encrypted through the KMS, shown once; expiry, inline policy, last-used, revocation | `internal/app/apikeys.go` |
| 4.4 | todo | Vault Transit KMS adapter | integration test against Vault via testcontainers | `internal/adapters/kms/vault/` |
| 4.5 | todo | Built-in roles | `cluster-admin`, `bucket-admin`, `read-write`, `read-only`, `auditor` | `internal/domain/iam/` |
| 4.6 | todo | Audit log | append-only, hash-chained; admin mutations, auth failures, denials; audit events added to the AsyncAPI contract | `internal/domain/audit/`, `internal/adapters/audit/`, `api/asyncapi/events.yaml` |
| 4.7 | todo | Console users | Argon2id local accounts; `ports.IdentityProvider`; secure session cookies + CSRF | `internal/app/sessions.go` |

## Milestone 5 — Admin API, Swagger UI, console

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 5.1 | todo | OpenAPI contract and tooling | `api/openapi/admin.yaml` first; `oapi-codegen` and `oasdiff` pinned as `tool` directives; wired into `make gen` and `make contracts` | `api/openapi/admin.yaml`, `go.mod`, `scripts/contracts.sh` |
| 5.2 | todo | Admin API adapter | generated handlers under `/api/v1`; `application/problem+json`; `Idempotency-Key` on mutations; spec served as JSON and YAML | `internal/adapters/adminapi/` |
| 5.3 | todo | Swagger UI at `/docs` | assets embedded with `go:embed`, no CDN; working Authorize button | `internal/adapters/adminapi/swaggerui/` |
| 5.4 | todo | Embed the console | `-tags console` embeds `web/dist`; without the tag a stub page says the console was not built | `web/embed_*.go`, `internal/adapters/console/` |
| 5.5 | todo | Generated web client | `openapi-typescript` + `openapi-fetch`; no hand-written API calls | `web/src/api/` |
| 5.6 | todo | Console screens | buckets, object browser, users, groups, roles and policies, API keys, cluster/drive/heal status, audit log | `web/src/` |

## Milestone 6 — S3 depth

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 6.1 | todo | Multipart upload | compat tests recorded | `internal/app/multipart.go`, `internal/adapters/s3api/` |
| 6.2 | todo | Versioning | compat tests recorded | `internal/domain/object/`, `internal/app/` |
| 6.3 | todo | Presigned URLs and streaming SigV4 | fuzz tests extended | `internal/adapters/s3api/` |
| 6.4 | todo | Object Lock (governance, compliance) | a compliance-locked version cannot be deleted by anyone | `internal/domain/object/lock.go` |
| 6.5 | todo | SSE-KMS and SSE-C | alongside SSE-S3 | `internal/app/` |
| 6.6 | todo | Lifecycle | expiration and non-current version rules | `internal/app/lifecycle.go` |
| 6.7 | todo | Bucket policy, CORS, tagging | anonymous access only when a bucket policy grants it | `internal/app/`, `internal/adapters/s3api/` |
| 6.8 | todo | aws-cli and ceph/s3-tests in the compat suite | baseline extended | `test/s3compat/` |

## Milestone 7 — Deployment hardening

| ID | Status | Step | Acceptance criterion | Files |
|---|---|---|---|---|
| 7.1 | todo | Helm chart | StatefulSet + headless Service, PVC per drive, PDB, topology spread, probes, requests/limits, `restricted` PSS; manifest validation in `make helm-lint` | `deploy/helm/cactusfs/`, `scripts/helm-lint.sh` |
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

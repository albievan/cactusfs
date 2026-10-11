# cactusfs.io — Engineering Rules for Claude Code

You are a senior engineer on CactusFS: an S3-compatible, erasure-coded, multi-node object
store written in Go (hexagonal, event-driven, container-first, Vault-backed) with a React
console. It targets the same functionality as MinIO and puts **data security first**.
Your job is to execute `PLAN.md` faithfully and produce code that passes the full quality
bar defined below. You work **autonomously**: when given a goal you plan, implement, test,
review and report — you do not stop to ask unless a decision is genuinely irreversible or
contradicts this file.

This file is loaded into every session. Keep it authoritative and short; details live in
`docs/ARCHITECTURE.md` and `PLAN.md`. If either does not exist yet, creating it is
milestone 0 (§8).

| Property       | Value                                                                  |
|----------------|------------------------------------------------------------------------|
| Module path    | `github.com/albievan/cactusfs`                                         |
| Go             | latest stable, pinned in `go.mod`; pure Go, `CGO_ENABLED=0`            |
| Artifact       | ONE binary `cactusfs` (linux, darwin, windows × amd64, arm64), ONE image |
| Data API       | S3-compatible (SigV4, XML)                                             |
| Management API | REST/JSON, OpenAPI design-first, Swagger UI                            |
| Durability     | Reed-Solomon erasure coding, checksums, self-healing                   |
| Metadata       | embedded Raft + bbolt — no external database                           |
| KMS / secrets  | HashiCorp Vault Transit (clusters); sealed local key file (standalone) |
| Events         | in-process bus; NATS JetStream and webhook adapters are optional       |
| Frontend       | React + TypeScript + Vite in `web/`, embedded with `-tags console`     |
| Build          | fresh clone builds with Go alone, or with a container engine alone     |
| Deploy         | compose (Docker Desktop, Podman) and Helm (k3s, k8s); on-prem, cloud, hybrid |

---

## 1. Operating mode (autonomous)

1. **Read first.** On every goal: read `PLAN.md`, `docs/ARCHITECTURE.md`, and the files
   you will touch. Never guess a package layout — `ls` it.
2. **Plan before code.** For any goal touching more than one package, write or update the
   relevant section of `PLAN.md` (§8) before editing source. Use the `planner` subagent
   for non-trivial goals.
3. **Execute in small verified steps.** Each step: change → `make check` → fix → next.
4. **Review before done.** Invoke the `reviewer` subagent on the diff. Address every
   BLOCKER and MAJOR finding. Re-run `make check`.
5. **Report.** Finish with the report format in §9: files, decisions, test evidence,
   open risks.
6. **Ask only when blocked** by: missing credentials; a destructive or irreversible
   action (force-push, deleting data, changing the on-disk format, Raft entry schema or
   crypto design after a release); or a conflict between the goal and this file.
   Otherwise decide, record the decision in `PLAN.md`, and continue.
7. **Never mark a step done with failing checks.** Leave it `in_progress`, explain why in
   the report, and propose the fix.

## 2. Architecture — non-negotiable

- **Hexagonal (ports & adapters).** One Go module:
  `internal/domain` (entities, value objects, domain errors — zero external imports),
  `internal/app` (use cases; depends on domain + port interfaces only),
  `internal/ports` (interfaces: stores, KMS, publishers, clocks, clients),
  `internal/adapters/...` (implementations),
  `cmd/cactusfs/main.go` (composition root — the ONLY place adapters are wired).
- **Dependency rule:** `domain ← app ← ports ← adapters ← cmd`. Imports only point
  inward. `internal/domain` never imports `net/http`, gRPC, bbolt, Raft, or any SDK.

```
cmd/cactusfs/                  single binary, role flags
internal/domain/{bucket,object,erasure,iam,cluster,audit}
internal/app/                  use cases
internal/ports/                interfaces
internal/adapters/
  s3api/      in   S3 HTTP API (SigV4, XML errors)
  adminapi/   in   management REST API (generated from OpenAPI) + Swagger UI
  console/    in   serves the embedded web/dist
  noderpc/    both node-to-node gRPC over mTLS (api/proto/)
  disk/       out  shard storage on local drives (build-tagged per OS)
  raftmeta/   out  Raft + bbolt metadata store
  kms/{vault,local}/   events/{inproc,nats,webhook}/   audit/   telemetry/
api/{openapi/admin.yaml,asyncapi/events.yaml,proto/}
web/   deploy/{compose,helm}/   docs/{ARCHITECTURE.md,adr/,threat-model.md}
test/{integration,s3compat,chaos}/
```

- **Standalone product.** CactusFS shares nothing with any other project. Never copy,
  import, vendor or reference code, scripts, configs, hostnames or paths from another
  repository, Albie's own included. No private modules, no `replace` directive to a
  local path, no `go.work`, no private registry or internal host. Every dependency is
  public and resolves through the default Go module proxy.
- **Builds from a fresh clone, two ways.**
  *Go only:* `git clone` then `go build ./cmd/cactusfs` succeeds with nothing installed
  but the Go version named in `go.mod`. To keep that true: generated code (OpenAPI,
  protobuf) is committed; build tools are pinned with `tool` directives in `go.mod` and
  run with `go tool`; the console is embedded only with `-tags console`, and without the
  tag the binary serves a stub page saying the console was not built.
  *Container only:* `docker build .` and `podman build .` succeed with nothing installed
  but a container engine. The multi-stage `Dockerfile` builds `web/` (Node stage), then
  the Go binary with `-tags console`, then the distroless image. Base images are public
  and pinned by digest.
  `make` targets run inside that pinned builder image (`ENGINE=docker|podman`, engine
  socket passed through for testcontainers), so every developer and CI use identical
  tool versions. Plain `go build`, `go test` and `go vet` must keep working without
  `make`.
- **One binary, four roles.** `gateway` (S3 + admin API, auth, erasure encode/decode,
  stateless), `meta` (Raft voter), `storage` (drives, scrub, heal), `console` (UI). Roles
  are chosen with `--roles`; the default is all of them. The gateway is our own Go code —
  no Kong, Envoy or nginx is required in front of it.
- **Two run modes, same code path.**
  *Standalone:* `cactusfs server --data <dir>...` runs every role in one process with a
  single-node Raft group on Windows, Linux and macOS. With ≥ 4 drives it erasure-codes
  across them; with fewer it still encrypts and checksums but logs that it has no parity.
  *Cluster:* the same binary on ≥ 4 storage nodes with 3 or 5 `meta` voters. Standalone
  is a cluster of one, not a separate implementation.
- **Portability.** No cgo, no OS-specific dependency in core. OS differences (directory
  fsync, file locking, signals/service control) live in `adapters/disk` and
  `cmd/cactusfs` behind build tags. **Object keys never become filesystem paths**: data
  is stored under generated IDs, so case-insensitive filesystems, Windows reserved names,
  path length limits and `../` traversal cannot affect it.
- **Storage engine.** Objects are split into Reed-Solomon data + parity shards
  (`klauspost/reedsolomon`) over erasure sets of 4–16 drives. Default parity: 2 for 4–5
  drives, 3 for 6–7, 4 for ≥ 8; configurable per storage class. Shards of one object
  never share a drive and are spread across nodes, then racks/zones when labelled.
  Write quorum = data shards (+1 when data = parity); read quorum = data shards.
- **Write path.** encrypt → erasure-code → write shards under a new immutable ID and
  fsync on a write quorum → commit the object version through Raft → acknowledge. The
  Raft commit is the only visibility switch; uncommitted shards are garbage-collected.
  Reads and listings are strongly consistent (read-after-write), as S3 clients expect.
- **Metadata.** IAM, API keys, policies, bucket config, cluster topology and the object
  index are replicated with `hashicorp/raft` and stored in bbolt. The object index sits
  behind `ports.ObjectIndex` so it can move to sharded Raft groups later; that change
  needs an ADR. On-disk formats and Raft entries carry a version from day one, and
  version N must run alongside N+1 for rolling upgrades.
- **API-first.**
  *S3 API:* defined by AWS's S3 API reference, path-style and virtual-hosted-style,
  SigV4 header, presigned and streaming signatures. Its contract is the compatibility
  suite in `test/s3compat` (aws-sdk-go-v2, aws-cli, ceph/s3-tests), not OpenAPI.
  *Admin API:* every endpoint is defined in `api/openapi/admin.yaml` BEFORE
  implementation (use the highest OpenAPI version `oapi-codegen` fully supports — check
  before choosing 3.1); handlers are generated with `oapi-codegen`; versioned under
  `/api/v1`; errors are `application/problem+json`. The spec is served at
  `/api/v1/openapi.json` and `.yaml`.
  *Swagger UI:* served at `/docs` from assets embedded with `go:embed` (no CDN), with a
  working **Authorize** button for API keys.
- **Event-driven.** Every state change other components care about is an event
  (`cactusfs.<aggregate>.<event>.v1`, defined in `api/asyncapi/events.yaml`) derived from
  **applied Raft entries** — never published directly from a handler. Delivery goes
  through `ports.EventPublisher`: in-process by default; NATS JetStream and webhooks for
  bucket notifications and audit export. Consumers are idempotent (dedupe by event ID).
- **Container-first.** 12-factor: config from `CACTUSFS_*` env vars with matching flags,
  JSON logs to stdout (`log/slog`), `/healthz` and `/readyz`, `/metrics`, graceful
  shutdown on SIGTERM. Multi-stage `Dockerfile` → distroless static, non-root, read-only
  root filesystem, all capabilities dropped, multi-arch (amd64 + arm64).
- **Vault-backed.** The KMS port has two adapters: Vault Transit (required for clusters
  in production) and a sealed local key file (standalone/dev, explicit opt-in). There is
  no unencrypted fallback. No secrets in code, images, env files or tests
  (the public default password of ADR-0014 is the one exception).
- **Frontend.** React + TypeScript + Vite in `web/`, responsive, talks ONLY to the admin
  API through the generated client (`openapi-typescript` + `openapi-fetch`). No business
  or authorization logic in the frontend. Screens: buckets, object browser, users,
  groups, roles and policies, API keys, cluster/drive/heal status, audit log.
- **Brand assets.** The logo files already exist — use them; never redraw or regenerate
  them. Masters live in `docs/brand/`. The console imports from `web/src/assets/brand/`
  (`cactusfs-logo.svg` on light backgrounds, `cactusfs-logo-reversed.svg` on dark,
  `cactusfs-logo-tagline*.svg` for the login screen, `cactusfs-mark.svg` where space is
  tight) and serves `web/public/favicon.svg` as the tab icon, linked from
  `web/index.html`. Do not use the tagline versions below 64 px high. Colours: Saguaro
  `#2E6B4E`, Night Green `#163B2C`, Sand `#F4ECDD`, Dune `#D9C39A`.

## 3. Data-security rules (storage product)

These are product constraints, not optional features. Every plan must respect them.

- **Encryption at rest, always on.** Every object is encrypted before erasure coding
  with AES-256-GCM as a streaming AEAD: fixed-size segments, a unique nonce per segment,
  and the segment index bound into the AAD so reordering and truncation are detected.
  Each object has its own data key, wrapped by a KMS key (envelope encryption). Support
  SSE-S3, SSE-KMS and SSE-C. Stdlib `crypto/*` and `golang.org/x/crypto` only — never
  write your own primitive.
- **Encryption in transit.** TLS 1.2+ for clients (1.3 preferred); mTLS 1.3 between
  nodes, Raft transport included. A plaintext listener exists only behind an explicit
  dev flag and binds to loopback.
- **Integrity.** Every shard carries a SHA-256 checksum verified on every read. A
  background scrubber finds bitrot and heals from parity. Never return bytes that failed
  verification.
- **No default credentials, with one exception.** First start requires or generates
  root credentials; there is no default API key. The exception (Albie, 2026-10-11,
  ADR-0014): a new deployment is given the console user `admin` with the password
  `admin` and the role `cluster-admin`, made once and never restored. Add no other
  built-in username or password.
- **API keys.** A key is `{access_key_id, secret}` owned by a user. The S3 API uses it
  with SigV4; the admin API accepts it as a bearer credential. SigV4 needs the secret
  server-side, so secrets are stored **envelope-encrypted through the KMS**, shown once
  at creation, never logged. Keys support expiry, a scoped-down inline policy, last-used
  tracking and immediate revocation. Compare secrets in constant time.
- **Console users.** Local accounts with Argon2id; OIDC sits behind
  `ports.IdentityProvider`. Sessions are short-lived, `HttpOnly`, `Secure`,
  `SameSite=Strict` cookies with CSRF protection.
- **RBAC.** Users, groups, roles and policies. Policies are IAM-style JSON (Effect,
  Action, Resource, Condition) with `s3:*` and `admin:*` actions. Built-in roles:
  `cluster-admin`, `bucket-admin`, `read-write`, `read-only`, `auditor`. Default deny;
  explicit deny wins; an API key never exceeds its owner. EVERY inbound request is
  authenticated and passes through the single `ports.Authorizer` — no authorization
  logic in handlers, no anonymous access unless a bucket policy grants it.
- **Audit trail.** Every admin mutation, authentication failure and authorization denial
  is an append-only, hash-chained audit event. Never delete audit records. Never log
  secrets, keys, tokens or object contents.
- **Ransomware resistance.** Versioning and Object Lock (governance and compliance).
  A compliance-locked version cannot be deleted by anyone, `cluster-admin` included.
- **Clean-room and licences.** MinIO is AGPL: never read, copy or port its source.
  Implement from the public S3 documentation. Dependencies must be MIT, BSD, ISC,
  Apache-2.0 or MPL-2.0 (`make licences`); no GPL, AGPL, BUSL or SSPL.
- **Air-gap safe.** No runtime call to the internet, no telemetry, no CDN assets.
- When a security rule is ambiguous, implement the stricter interpretation and flag it
  in the report as an open question for Albie.

## 4. Deployment targets — all must keep working

| Target | Artifact | Rules |
|---|---|---|
| Single binary | `dist/cactusfs-<os>-<arch>` | Runs standalone on Windows, Linux, macOS with no other software installed. |
| Docker Desktop, Podman | `deploy/compose/compose.yaml` | 4-node cluster. Works unchanged with `docker compose` and rootless `podman compose`: no privileged ports, no host network, no `privileged`. |
| k3s, k8s | `deploy/helm/cactusfs` | StatefulSet + headless Service, one PVC per drive, PodDisruptionBudget, topology spread, default-deny NetworkPolicy, `restricted` Pod Security Standard, requests/limits and probes. No dependency on a specific ingress, CSI driver or cloud. |

- **On-prem, cloud, hybrid.** Core code uses no cloud-provider API; anything
  provider-specific is an adapter. Nodes carry `region/zone/rack/host` labels for
  placement. Hybrid means separate clusters per site linked by asynchronous bucket
  replication — never stretch one Raft quorum across a WAN.
- Default ports (all configurable, never hard-coded): S3 `9000`; admin API, Swagger UI
  and console `9001`; node RPC `9002`; Raft `9003`.

## 5. Quality bar — `make check` must be green

| Gate | Command | Rule |
|---|---|---|
| Format | `gofumpt -l .` / `goimports` | zero diff |
| Lint | `golangci-lint run ./...` | zero findings (config in `.golangci.yml`, `gosec` on) |
| Vet | `go vet ./...` | clean |
| Unit tests | `go test -race -count=1 -cover ./...` | pass; **≥ 80 % coverage on `internal/domain` and `internal/app`** |
| Integration | `go test -race -tags=integration ./...` | pass — real multi-node cluster, Vault and NATS via **testcontainers-go** |
| S3 compatibility | `make s3-compat` | no regression against the recorded baseline |
| Vulnerabilities | `govulncheck ./...` | zero known vulns |
| Contracts | `make contracts` (oapi-codegen diff, `oasdiff`, AsyncAPI validate, `buf breaking`) | generated code in sync, no unflagged breaking changes |
| Licences | `make licences` | allow-list in §3 |
| Frontend | `pnpm -C web lint && pnpm -C web typecheck && pnpm -C web test` | clean |
| Build | `make build-all && make image` | all six OS/arch targets build with `CGO_ENABLED=0`; image < 60 MB |
| Deploy | `make helm-lint` | chart lints and renders valid manifests |
| Fresh clone | `make verify-clone` | a clean `git archive` export builds with `go build ./cmd/cactusfs` in a stock `golang` container and with `docker build .`; nothing outside the repo is read |

CI additionally runs unit tests on Linux, macOS and Windows. `make test-chaos` (node
kill, network partition, shard corruption, heal verification) must pass before any
milestone that touches the write path, quorum, Raft or healing is marked done.

Additional rules:
- Erasure, crypto, SigV4, XML and policy parsing have fuzz tests and property tests
  (encode → lose up to parity shards → decode equals the original).
- Errors are wrapped with context: `fmt.Errorf("object.Put: %w", err)`. Domain errors
  are typed sentinels in `internal/domain/errors.go`, mapped to S3 XML error codes or
  problem+json in the inbound adapters only.
- Every exported symbol has a doc comment. Every use case has a table-driven test.
- `context.Context` is the first parameter of anything that does I/O. Always propagate.
- No `init()`, no globals holding state, no `panic` outside `main`.
- Manual constructor injection. Prefer the standard library; a new dependency needs a
  reason recorded in `PLAN.md`.
- Observability: OpenTelemetry traces and Prometheus metrics (`cactusfs_*`) on every
  adapter boundary.
- Every mutating admin endpoint accepts `Idempotency-Key`.

## 6. Commands you will use

```
make gen          # oapi-codegen + protobuf + AsyncAPI + web API client
make check        # THE quality bar (everything in §5)
make test         # unit only, fast
make test-int     # integration via testcontainers
make s3-compat    # S3 compatibility suite against a local 4-node cluster
make test-chaos   # fault injection
go build ./cmd/cactusfs      # Go only, no console — must always work
docker build -t cactusfs .   # container engine only, full build (or: podman build)

make verify-clone # proves both commands above from a clean export
make build        # cactusfs for this OS → bin/   (builds web/ and embeds it)
make build-all    # cross-compile matrix → dist/
make image        # multi-arch container image
make run          # standalone single binary against ./.data
make up / down    # 4-node compose cluster (ENGINE=docker|podman)
make helm-lint    # helm lint + manifest validation
```

If a target is missing, add it to the `Makefile` — do not run ad-hoc commands that
future sessions won't know about.

## 7. Git discipline

- Work on the main branch. Always commit to `main`.
- Conventional commits: `feat(erasure): heal shards on read (PLAN 2.4)`.
- One logical change per commit. Commit only when `make check` is green.
- Never `git push --force`, never rewrite shared history.
- Do not open PRs or push unless the goal says so; report the branch name instead.

## 8. `PLAN.md` protocol

`PLAN.md` is the source of truth for **what** is being built and **where we are**.

- Every step has an ID (`3.2`), a status (`todo | in_progress | done | blocked`), an
  acceptance criterion, and the files it touches.
- Update status **as you go**. A crashed session must be resumable from `PLAN.md` alone.
- Record every non-trivial decision under `## Decisions` with date and reason.
  Architectural decisions also get an ADR in `docs/adr/`.
- If the goal you were given is not in `PLAN.md`, add it as a new milestone first.

Milestone order (seed `PLAN.md` with this if it is empty):

0. Scaffold: layout, `go.mod`, `Dockerfile`, `Makefile`, CI matrix, health endpoints,
   `README.md` with build-from-source steps, `docs/ARCHITECTURE.md`,
   `docs/threat-model.md`, `.claude/agents/{planner,coder,reviewer}.md`. Done when
   `make verify-clone` is green.
1. Standalone store: disk adapter, buckets, PUT/GET/HEAD/DELETE, ListObjectsV2, SigV4,
   root bootstrap, encryption on by default.
2. Erasure coding and integrity on local drives: checksums, scrub, heal.
3. Cluster: Raft metadata, node RPC over mTLS, distributed erasure sets, quorum, join.
4. IAM: users, groups, roles, policies, API keys, authorizer, audit log.
5. Admin API, Swagger UI, console.
6. S3 depth: multipart, versioning, presigned URLs, Object Lock, lifecycle, bucket
   policy, CORS, tagging.
7. Deployment hardening: Helm chart, NetworkPolicy, signed images, SBOM, observability.
8. Hybrid: site replication, bucket notifications, quotas.

## 9. Report format (end of every goal)

```
## Report: <goal>
**Status:** done | partial | blocked
**Branch:** <name>   **Commits:** <n>

### What was done
- <PLAN step id> — <one line> (files: a.go, b.go)

### Decisions made
- <decision> — <why> (recorded in PLAN.md / ADR-00X)

### Verification
- make check: PASS/FAIL (paste the summary lines; say which OS you ran on)
- coverage: domain 91 %, app 84 %
- s3-compat: <passed>/<total> (baseline <n>)
- reviewer findings: 0 blocker, 1 major (fixed), 2 minor (fixed / deferred: reason)

### Open questions / risks
- <anything needing Albie's decision>

### Next suggested step
- <PLAN step id>
```

## 10. Things you must never do

- Bypass a failing gate with `//nolint`, `t.Skip`, lowered thresholds, or `-short`.
- Acknowledge a write before it is durable on a write quorum and committed through Raft.
- Store or transmit object data, secrets or keys unencrypted, or weaken a TLS or
  cipher setting to make a client or test pass.
- Put business or authorization logic in adapters, handlers, or the React app.
- Derive a filesystem path from an object key or bucket name.
- Add a cgo dependency or anything that breaks the Windows, macOS or Linux build.
- Add an external service that standalone mode would need in order to start.
- Ship default credentials other than the console user of ADR-0014, or log secrets,
  tokens or object contents.
- Copy code from MinIO or any GPL/AGPL project.
- Copy, import or reference code, scripts, hosts or paths from any other project or
  private repository.
- Break the fresh-clone build: uncommitted generated code, a tool that must be installed
  on the host, a private dependency, or an unpinned base image.
- Change the on-disk format, Raft entry schema or crypto design without an ADR, a
  migration and an upgrade test.
- Delete or silence tests to make the build pass.
- Invent APIs — check `go doc` / the module source before using a library function.
- Leave `PLAN.md` stale.

## 11. Decisions on record (Albie, 2026-10-07)

- S3-compatible data API plus a separate OpenAPI management API with Swagger UI.
- Erasure coding for durability.
- Embedded Raft for metadata; no external database.
- One binary that runs standalone on Windows, Linux and macOS or as a cluster.
- Container-first; Docker Desktop, Podman, k3s and k8s; on-prem, cloud and hybrid.
- API keys for the API, a frontend for users, RBAC management.
- Standalone product: no code from any other project. Any Go developer can download the
  source and build it with Go alone or with a container engine alone.
- 2026-10-11: a default console user `admin` with the password `admin` on every new
  deployment (ADR-0014).
- 2026-10-11: the project's own licence is a dedication to the public domain, as
  SQLite's is (`LICENSE.md`). Dependencies keep to the list of §3.
- **Still open — ask before acting on it:** whether `github.com/albievan/cactusfs` is
  the final module path.

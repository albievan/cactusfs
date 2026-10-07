# CactusFS threat model

CactusFS stores other people's data, so its security properties are product constraints.
This document names what is protected, from whom, and which control answers each threat.
It is revised in the same change as any step that adds a trust boundary, an asset or a
control.

**Status column:** `implemented` means the control exists and is tested today;
`M<n>` is the milestone in `PLAN.md` that delivers it. A control that is not yet
implemented protects nothing: do not deploy CactusFS for real data before the milestones
it depends on are done.

## 1. Assets

| # | Asset | Why it matters |
|---|---|---|
| A1 | Object data | confidentiality, integrity, availability of customer data |
| A2 | Object and bucket metadata | names, sizes, tags and listings leak information; corruption loses data |
| A3 | Key material: KMS keys, wrapped data keys, the sealed local key file | compromise defeats encryption at rest |
| A4 | Credentials: root credentials, API key secrets, console passwords, session cookies | compromise grants access as the owner |
| A5 | Policies and role bindings | tampering grants access silently |
| A6 | Audit trail | the only evidence after an incident |
| A7 | Cluster membership and Raft log | a rogue voter or a forged entry rewrites A2 and A5 |
| A8 | The build and release pipeline | a poisoned dependency or image compromises every deployment |

## 2. Actors

| Actor | Capability assumed |
|---|---|
| Anonymous network attacker | can reach any listening port, observe and modify traffic on the path |
| Authenticated tenant | holds valid credentials with limited policy; tries to exceed them |
| Compromised API key | a leaked `{access_key_id, secret}` |
| Malicious or compromised administrator | holds `cluster-admin`; tries to destroy data or evidence (ransomware) |
| Storage-layer attacker | can read or alter raw drives, backups or decommissioned disks |
| Compromised node | controls one cluster member and its certificates |
| Supply-chain attacker | controls a dependency, a base image or a build tool |

Out of scope: an attacker with root on a quorum of nodes **and** access to the KMS;
physical side channels; denial of service by volumetric flooding (left to the network).

## 3. Trust boundaries

1. Client ↔ gateway (S3 API `9000`, admin API and console `9001`).
2. Node ↔ node (node RPC `9002`, Raft `9003`).
3. Process ↔ local drives.
4. Process ↔ KMS (Vault Transit, or the sealed local key file).
5. Process ↔ optional event sinks (NATS JetStream, webhooks).
6. Source repository ↔ built artefact.

## 4. Threats and controls

### Spoofing

| Threat | Control | Status |
|---|---|---|
| Use of a built-in or default account | No default credentials; first start requires or generates root credentials, shown once on standard error and stored sealed | **implemented** |
| Forged S3 requests | SigV4 verification (checked against AWS's documented examples); host and every `x-amz-*` header must be signed; constant-time comparison; 15-minute clock-skew window | **implemented** (header authentication; presigned URLs M6) |
| Stolen API key used indefinitely | Expiry, last-used tracking, immediate revocation, scoped-down inline policy | M4 |
| Password guessing against the console | Argon2id; short-lived `HttpOnly`, `Secure`, `SameSite=Strict` session cookies; CSRF protection | M4 |
| Rogue node joining the cluster | mTLS 1.3 between nodes, Raft transport included | M3 |

### Tampering

| Threat | Control | Status |
|---|---|---|
| Traffic modified in transit | TLS 1.2+ for clients (1.3 preferred), ECDHE with AEAD suites only; a plaintext listener exists only behind `--dev-plaintext` and binds to a loopback IP | **implemented** (S3 and admin listeners) |
| Upload corrupted or cut short in transit | Declared SHA-256, `Content-MD5` (which must be signed) and CRC32/CRC32C/CRC64NVME/SHA-256 checksums are verified before anything becomes durable; a body the transport reports as cut short is never stored | **implemented** |
| A write reported as failed takes effect later | A commit whose outcome is unknown keeps its data; the state machine is fail-stop; a stale state database is rebuilt from the newest snapshot | **implemented** |
| Bitrot or deliberate modification of shards on disk | Position-bound SHA-256 per block and AES-GCM per segment, both verified on every read; unverified bytes are never returned | **implemented** (detection); scrub and heal from parity M2 |
| Reordered, truncated or spliced ciphertext | Streaming AEAD with a unique nonce per segment and the segment index and final flag bound into the AAD (ADR-0002) | **implemented** |
| Path traversal, reserved names or case collisions via object keys | Keys and bucket names never become filesystem paths; data lives under generated IDs, and all drive I/O is confined by `os.Root` | **implemented** |
| Forged or replayed metadata changes | All metadata changes are Raft entries, versioned, applied in order and exactly once; an entry this binary cannot interpret stops the node rather than being skipped | **implemented** (single voter); mTLS between voters M3 |
| Policy tampering | Policies live in Raft; every mutation is authorized and audited | M4 |

### Repudiation

| Threat | Control | Status |
|---|---|---|
| An administrator denies an action or erases the evidence | Append-only, hash-chained audit events for every admin mutation, authentication failure and authorization denial; audit records are never deleted | M4. Until then authentication failures and denials are written to the log, which is not tamper-evident |

### Information disclosure

| Threat | Control | Status |
|---|---|---|
| Theft of drives, backups or decommissioned disks | Encryption at rest is always on: AES-256-GCM, a data key per object, wrapped by a KMS key; no unencrypted fallback | **implemented** |
| Theft of the local KMS key file | The master key is sealed with AES-256-GCM under an Argon2id-derived key; the passphrase is never stored | **implemented** (strength is the passphrase's; Vault Transit M4) |
| A node started with the wrong key | A missing key file on an existing deployment, a wrong passphrase or another deployment's key stops the node | **implemented** |
| Theft of the metadata store reveals API secrets | Secrets are envelope-encrypted through the KMS and shown once | **implemented** for the root key; user keys M4 |
| Bucket names, keys or object metadata read from the metadata store | Not encrypted: `state.db` and the Raft log hold names, sizes and user metadata in the clear | **not mitigated** — see open question 6 |
| Secrets in logs | Secrets, keys, tokens and object contents are never logged; secrets have no command-line flag | **implemented**, tested for the root secret and the KMS passphrase |
| Internal state disclosed through errors | Integrity, key and storage failures all answer `InternalError`; the cause is logged, not returned | **implemented** |
| Metrics disclose tenants | `/metrics` is authenticated and authorized (`admin:GetMetrics`); no label holds a bucket, key or access key | **implemented** |
| Existence of buckets or keys probed anonymously | Authentication precedes routing: an unsigned request is refused the same way whether or not its target exists | **implemented** |
| Probe endpoints reveal internals | `/healthz` and `/readyz` return only `ok` or `unavailable`; the reason is logged, not returned | **implemented** |
| Eavesdropping | See TLS above | **implemented** (admin listener) |
| Data leaves an air-gapped site | No runtime call to the internet, no telemetry, no CDN asset | **implemented** |

### Denial of service

| Threat | Control | Status |
|---|---|---|
| Slow or oversized requests exhaust a listener | Read-header and idle timeouts and a header size limit on both listeners; whole-request timeouts on the admin listener; object size and XML body limits | **implemented**; a slow *body* on the S3 listener is not bounded yet |
| Loss of drives or nodes | Reed-Solomon parity; quorum writes; self-healing | M2, M3 |
| Requests routed to a node that is going away | `/readyz` answers 503 for `--drain-delay` before the listener closes; in-flight requests then finish within `--shutdown-timeout` | **implemented**; the delay defaults to `0s` (off) and must be set per deployment (chart default: M7) |
| One tenant fills the cluster | Per-bucket quotas | M8 |

### Elevation of privilege

| Threat | Control | Status |
|---|---|---|
| A handler forgets an authorization check | Every use case calls the single `ports.Authorizer`; no authorization logic in handlers; default deny | **implemented** (root only); policies and explicit deny M4 |
| An unimplemented S3 feature is served as a different operation | Allow-list: any query parameter, `x-amz-*` header or conditional header the routed operation does not implement answers `501`; an unparseable query answers `400` | **implemented** |
| An API key exceeds its owner | A key's effective policy is the intersection with its owner's | M4 |
| Anonymous access | Only when a bucket policy explicitly grants it | M6 |
| Ransomware with stolen admin credentials | Versioning and Object Lock; a compliance-locked version cannot be deleted by anyone, `cluster-admin` included | M6 |
| Container escape or host damage | Distroless static image, non-root UID 65532, no shell; read-only root filesystem; all capabilities dropped; `restricted` Pod Security Standard; default-deny NetworkPolicy | image **implemented**; chart M7 |

### Supply chain

| Threat | Control | Status |
|---|---|---|
| A base image or tool changes under a tag | Every base image pinned by digest; Go tools pinned in `go.mod` and `go.sum`; console packages pinned by lockfile; CI actions pinned by commit SHA | **implemented**; pnpm itself is pinned by version only |
| A package runs code at install time | pnpm lifecycle scripts are disabled for the console | **implemented** |
| A vulnerable Go dependency | `govulncheck` gate | **implemented** |
| A vulnerable console package | Audit gate for `web/` | not planned yet — see open question 4 |
| A gate silently switched off | `make lint` rejects `//nolint`, skipped tests and `testing.Short`; gates whose subject does not exist yet fail as soon as it appears | **implemented** |
| A licence that taints the product | Allow-list gate: MIT, BSD, ISC, Apache-2.0, MPL-2.0 | **implemented** |
| Copied AGPL code | Clean-room rule: MinIO source is never read, copied or ported; the S3 API is implemented from AWS's public documentation | rule |
| A private or unreproducible build input | `make verify-clone` builds a clean export with Go alone and with a container engine alone | **implemented** |
| Tampered release image | Signed images and SBOM | M7 |
| Hand-rolled cryptography | Only the standard library `crypto/*` and `golang.org/x/crypto` | rule |

## 5. Open questions for Albie

1. **Unauthenticated probes.** `CLAUDE.md` §3 says every inbound request is
   authenticated. `/healthz` and `/readyz` are not, because kubelet and compose health
   checks cannot hold credentials; they disclose nothing instead. The stricter
   alternative is to require a credential and use exec probes. Decide before milestone 7.
2. **Probes over TLS.** The probes share the TLS admin listener. Kubernetes HTTPS probes
   do not verify the certificate, which is acceptable for a health check. An alternative
   is a separate loopback-only plaintext probe port.
3. **Minimum TLS version.** 1.2 is allowed for S3 client compatibility, with ECDHE and
   AEAD suites only. Requiring 1.3 on the admin listener is possible and stricter.
4. **Console dependency audit.** `govulncheck` covers Go only. Should `make check` gain a
   `pnpm audit` gate? It needs registry access at check time and can fail on advisories
   in build-only tools.
5. **Lint exclusions.** Two gosec findings are silenced by path in `.golangci.yml`, and
   nowhere else: G204 under `test/` (see below) and G401/G501 for
   `internal/adapters/s3api/md5.go`, the one file that computes the MD5 the S3 protocol
   prescribes for ETag and `Content-MD5`.
6. **Metadata at rest.** Object data is encrypted, but bucket names, object keys, sizes,
   content types and user metadata are stored in the clear in `state.db` and the Raft
   log. `CLAUDE.md` §3 says "every object is encrypted"; whether names and metadata must
   be too is a product decision (it costs listing performance or needs encrypted bbolt
   pages).
7. **Root credentials on standard error.** Generated credentials are printed once to
   stderr, which container log drivers capture. Production deployments should supply
   them; should generation be refused when stderr is not a terminal?
8. **Bearer format for `/metrics`.** `Bearer <access key ID>:<secret>` is provisional
   until the admin API contract (PLAN 5.1), and until scoped keys exist it is the root
   key that a scraper must hold.
9. **Slow uploads.** The S3 listener has no body deadline; a client can hold a
   connection open by sending slowly.
10. **SHA-1 checksums** (`x-amz-checksum-sha1`) are refused rather than verified,
    because the linter rejects SHA-1.
11. **gosec G204 in `test/`.** `.golangci.yml` excludes the "subprocess launched with a
   variable" finding for the black-box tests under `test/`, which exist to execute the
   binary they have just built. It is the only lint exclusion in the repository.
12. **Local KMS wrap bound.** The local KMS wraps every data key under one master key
    with a random 96-bit nonce, which is safe for about 2^32 wraps. There is no key
    rotation yet; Vault Transit (milestone 4) does not have this bound.
13. **Conditional requests** (`If-Match`, `If-None-Match`, `If-Modified-Since`,
    `If-Unmodified-Since`) answer `501` rather than being ignored. Caching clients and
    "create only if absent" uploads need them; they are not planned in a specific step
    yet.

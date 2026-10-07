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
| Use of a built-in or default account | No default credentials; first start requires or generates root credentials | M1 |
| Forged S3 requests | SigV4 verification; secrets compared in constant time; clock-skew window | M1 |
| Stolen API key used indefinitely | Expiry, last-used tracking, immediate revocation, scoped-down inline policy | M4 |
| Password guessing against the console | Argon2id; short-lived `HttpOnly`, `Secure`, `SameSite=Strict` session cookies; CSRF protection | M4 |
| Rogue node joining the cluster | mTLS 1.3 between nodes, Raft transport included | M3 |

### Tampering

| Threat | Control | Status |
|---|---|---|
| Traffic modified in transit | TLS 1.2+ for clients (1.3 preferred), ECDHE with AEAD suites only; a plaintext listener exists only behind `--dev-plaintext` and binds to a loopback IP | **implemented** (admin listener) |
| Bitrot or deliberate modification of shards on disk | SHA-256 per shard verified on every read; scrubber heals from parity; unverified bytes are never returned | M2 |
| Reordered, truncated or spliced ciphertext | Streaming AEAD with a unique nonce per segment and the segment index bound into the AAD | M1 |
| Path traversal, reserved names or case collisions via object keys | Keys and bucket names never become filesystem paths; data lives under generated IDs | M1 |
| Forged or replayed metadata changes | All metadata changes are Raft entries, versioned, applied in order | M1, M3 |
| Policy tampering | Policies live in Raft; every mutation is authorized and audited | M4 |

### Repudiation

| Threat | Control | Status |
|---|---|---|
| An administrator denies an action or erases the evidence | Append-only, hash-chained audit events for every admin mutation, authentication failure and authorization denial; audit records are never deleted | M4 |

### Information disclosure

| Threat | Control | Status |
|---|---|---|
| Theft of drives, backups or decommissioned disks | Encryption at rest is always on: AES-256-GCM, a data key per object, wrapped by a KMS key; no unencrypted fallback | M1 |
| Theft of the metadata store reveals API secrets | Secrets are envelope-encrypted through the KMS and shown once | M4 |
| Secrets in logs | Secrets, keys, tokens and object contents are never logged | **implemented** (rule; nothing secret exists yet) |
| Probe endpoints reveal internals | `/healthz` and `/readyz` return only `ok` or `unavailable`; the reason is logged, not returned | **implemented** |
| Eavesdropping | See TLS above | **implemented** (admin listener) |
| Data leaves an air-gapped site | No runtime call to the internet, no telemetry, no CDN asset | **implemented** |

### Denial of service

| Threat | Control | Status |
|---|---|---|
| Slow or oversized requests exhaust a listener | Read-header, read, write and idle timeouts; header size limit | **implemented** (admin listener) |
| Loss of drives or nodes | Reed-Solomon parity; quorum writes; self-healing | M2, M3 |
| Requests routed to a node that is going away | `/readyz` answers 503 for `--drain-delay` before the listener closes; in-flight requests then finish within `--shutdown-timeout` | **implemented**; the delay defaults to `0s` (off) and must be set per deployment (chart default: M7) |
| One tenant fills the cluster | Per-bucket quotas | M8 |

### Elevation of privilege

| Threat | Control | Status |
|---|---|---|
| A handler forgets an authorization check | Every inbound request passes through the single `ports.Authorizer`; no authorization logic in handlers; default deny; explicit deny wins | M4 |
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
5. **gosec G204 in `test/`.** `.golangci.yml` excludes the "subprocess launched with a
   variable" finding for the black-box tests under `test/`, which exist to execute the
   binary they have just built. It is the only lint exclusion in the repository.

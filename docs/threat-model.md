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
| Stolen API key used indefinitely | Expiry; last use recorded (to within five minutes); revocation that every node honours with the next request; an inline policy that narrows the key | **implemented**; there is no admin API to do it through until milestone 5 |
| Password guessing against the console | Argon2id (64 MiB, 3 passes); a refused sign-in looks and costs the same whether the user exists or not; sessions of 30 minutes stored as hashes; `__Host-` cookie that is `HttpOnly`, `Secure`, `SameSite=Strict`; CSRF token on every changing request | **implemented** in the use cases and helpers; **no lockout or rate limit yet**; the console that uses them is milestone 5 |
| Rogue node joining the cluster | Mutual TLS 1.3 for node RPC and Raft; only certificates of the cluster's node CA are accepted; a node's identity is the name in its certificate, never its address; every call passes the authorizer as that node; a node joins, leaves and records only itself; the Raft port admits voters only; no plaintext mode exists | **implemented**, with a limit: any node certificate may propose changes to buckets, objects and the topology, so a stolen one breaches the cluster's metadata, not just one node; the node CA must issue node certificates only — whatever it signs is a node. Certificates are read at start: rotation needs a restart, and there is no revocation list |
| A node is impersonated at its address (DNS or routing) | The caller checks that the certificate names the node it meant, whatever answered | **implemented** |
| A cut-off or deposed leader serves stale data | Reads on every node wait for a read index that the leader gives only after a majority of the *voters* confirm its term; a node without a majority answers nothing | **implemented**; tested with a network partition |
| An acknowledged write is lost when nodes fail | Acknowledged only after a write quorum of shards is durable on their nodes and the version is committed by a majority of voters | **implemented**; tested by killing nodes during writes |
| A node that is down is mistaken for lost data | An unreachable drive is neither read as damage nor a reason to rewrite or delete; the collector keeps anything a node it cannot ask might be writing | **implemented** |
| Old and new binaries diverge during an upgrade | No node writes a metadata entry format newer than the oldest member understands; a node that meets one it cannot apply stops and leaves the group | **implemented** |

### Tampering

| Threat | Control | Status |
|---|---|---|
| Traffic modified in transit | TLS 1.2+ for clients (1.3 preferred), ECDHE with AEAD suites only; a plaintext listener exists only behind `--dev-plaintext` and binds to a loopback IP | **implemented** (S3 and admin listeners) |
| Upload corrupted or cut short in transit | Declared SHA-256, `Content-MD5` (which must be signed) and CRC32/CRC32C/CRC64NVME/SHA-256 checksums are verified before anything becomes durable; a body the transport reports as cut short is never stored | **implemented** |
| A write reported as failed takes effect later | A commit whose outcome is unknown keeps its data; the state machine is fail-stop; a stale state database is rebuilt from the newest snapshot | **implemented** |
| Bitrot or deliberate modification of shards on disk | Position-bound SHA-256 per block and AES-GCM per segment, both verified on every read; unverified bytes are never returned; reads rebuild from parity; a scrubber verifies everything in the background and heals | **implemented** with four or more drives; below that, detection only |
| Reordered, truncated or spliced ciphertext | Streaming AEAD with a unique nonce per segment and the segment index and final flag bound into the AAD (ADR-0002) | **implemented** |
| An object made of parts is served with a part dropped, moved or swapped | Every part has a key of its own, derived from the upload's data key, its number and its data ID; the place of every part in the object - index, offset, count, total size - is sealed with an HMAC under a second derived key and verified before a byte of the part is returned (ADR-0010) | **implemented** |
| Path traversal, reserved names or case collisions via object keys | Keys and bucket names never become filesystem paths; data lives under generated IDs, and all drive I/O is confined by `os.Root` | **implemented** |
| Forged or replayed metadata changes | All metadata changes are Raft entries, versioned, applied in order and exactly once; an entry this binary cannot interpret stops the node rather than being skipped | **implemented** (single voter); mTLS between voters M3 |
| Policy tampering | Policies live in Raft; every change is authorized and written to the audit trail in the same entry as the change | **implemented** |
| A policy silently means less than it says | Parsing is strict: an unknown field, operator or condition key is an error, never ignored; evaluation is fuzzed against its laws (silence refuses, a matching Deny always wins, a key's policy only takes away) | **implemented** |
| The audit trail is edited | Records are numbered and hash-chained by the state machine; reading verifies the chain; the latest hash is published as an event and can be exported | **implemented**; a rewrite of the whole chain is only detected by somebody who kept a hash outside |
| The audit trail is flooded or outrun | Refusals are written in the background, repeats within ten seconds counted into one record, the queue bounded | **implemented**: what a caller can vary (the key claimed, the object named) does not make a new record; at most 64 kinds of refusal are written per ten seconds and node, the rest counted. Limit: under flood a record per refusal can be dropped, and is then only in the node's log; changes are never dropped |

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
| Theft of the metadata store reveals API secrets | Secrets are envelope-encrypted through the KMS and shown once; console passwords are Argon2id hashes; sessions are stored as hashes of their tokens | **implemented** |
| The KMS key is on the nodes | `--kms vault`: the key stays in Vault Transit; wrapped data keys are bound to their owner by associated data, and a key type that cannot bind is refused at start; HTTPS only; the token is read from a file and never logged | **implemented**; token authentication only, renewal is the platform's job |
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
| Loss of drives | Reed-Solomon parity (2–4 shards by set size); writes need a write quorum; healing refills a replaced drive | **implemented**; a node needs all of its own drives to start |
| Loss of a node | Erasure sets spread across nodes, racks and zones; 3 or 5 metadata voters; the number of node failures a cluster tolerates is computed and logged | **implemented**; storage cannot be added to a formed cluster yet |
| A drive from another deployment, or a stale drive, is attached | Drives are known by ID in replicated metadata and claimed for the deployment's ID; a foreign, retired or non-empty unknown drive is refused | **implemented** |
| The metadata directory is missing or the wrong one | A new deployment is created only from empty, unclaimed drives; drives with data and no matching metadata stop the node | **implemented** |
| A missing mount is mistaken for a replaced drive | Replacement needs parity and the operator's confirmation for that start; without parity it is never made | **implemented**; a confirmed replacement cannot be undone |
| Repair makes things worse | Healing rebuilds only definite damage, replaces a shard in one step, removes nothing when it cannot tell whether an object changed, and runs once per object at a time | **implemented** |
| Unreferenced data accumulates, or is removed while still needed | Garbage collection by data-ID reference count; writes in progress and commits of unknown outcome are pinned; nothing is collected while the metadata is not ready; a grace period of at least 1 hour (default 24) covers the rest | **implemented** |
| Multipart uploads are begun and never finished | Parts of an upload are referenced data, not garbage, until the upload is completed or aborted; uploads are listed and aborted with permissions of their own (`s3:ListBucketMultipartUploads`, `s3:AbortMultipartUpload`) and go with their bucket | **implemented**; nothing ends an abandoned upload on its own until lifecycle rules (PLAN 6.6), and there is no quota (M8) |
| Requests routed to a node that is going away | `/readyz` answers 503 for `--drain-delay` before the listener closes; in-flight requests then finish within `--shutdown-timeout` | **implemented**; the delay defaults to `0s` (off) and must be set per deployment (chart default: M7) |
| One tenant fills the cluster | Per-bucket quotas | M8 |

### Elevation of privilege

| Threat | Control | Status |
|---|---|---|
| A handler forgets an authorization check | Every use case calls the single `ports.Authorizer`, which decides on the policies resolved at authentication; no authorization logic in handlers; default deny; explicit deny wins | **implemented** |
| An unimplemented S3 feature is served as a different operation | Allow-list: any query parameter, `x-amz-*` header or conditional header the routed operation does not implement answers `501`; an unparseable query answers `400` | **implemented** |
| An API key exceeds its owner | A request with a key must be allowed by the owner's policies and, when the key has one, by its inline policy too | **implemented** |
| An administrator of users makes themselves an administrator of everything | Whoever may put users, groups, roles or policies, or make keys for an administrator, in effect holds every permission: by design, as in IAM. Of the built-in roles only `cluster-admin` can; a custom policy granting one of these grants the deployment. Every such change, and what it granted, is in the audit trail; a narrowed key cannot make keys | **accepted** |
| Anonymous access | Only when a bucket policy explicitly grants it | M6 |
| A node's clock decides what must not be deleted yet | The time of every metadata entry is stamped by the metadata leader and held monotonic by the state machine; gateways, proposers and followers have no say | **implemented** (ADR-0008), with a limit: a voter with a clock that runs ahead, once it leads, moves time forward. Voters need synchronised clocks |
| Another site makes a signed-in browser change something | Session cookie is `__Host-`, `HttpOnly`, `Secure`, `SameSite=Strict`; every change needs the session's CSRF token in a header; JSON bodies must be `application/json` | **implemented** |
| Script injected into the console or the documentation | Content-Security-Policy `default-src 'none'; script-src 'self'; style-src 'self'`: no inline script, nothing from another host; no CDN assets; downloads are always `attachment` and `application/octet-stream`, so an uploaded HTML file is never rendered on the console's origin | **implemented** |
| Password guessing at the console | Argon2id; at most four hashes at once; ten failed sign-ins per address in five minutes, per node; every failure audited; one answer for every reason of refusal | **implemented**, with limits: no lockout, the count is per node and in memory |
| A repeated request changes something twice, or leaks a stored answer | `Idempotency-Key` records are replicated and scoped to the caller; answers are sealed through the KMS; the same key with another request is refused | **implemented**; not atomic with the change (ADR-0007): after a node crash a repeat can run again |
| The management API reveals what it protects | A refusal and an internal error carry no detail; public are only sign-in, the contract, the documentation and the console's files | **implemented**; the public routes are an exception to "every request is authenticated" - see open questions |
| Ransomware with stolen admin credentials | Versioning: an overwrite or a delete destroys nothing while it is enabled, and removing a version is its own permission, which `read-write` does not have. Object Lock: a version under compliance retention or legal hold is removed by no entry the state machine accepts - not from root, not with any flag (ADR-0009); governance bypass is granted by no wildcard | **implemented**, with limits: the rule is enforced in the metadata, so whoever can write to the drives of enough nodes or to the metadata of a majority of voters is not stopped by it; a voter with a clock that runs ahead ends retentions early; a stolen node certificate can delete shards over node RPC |
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

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
| Use of a built-in or default account | No default API key: first start requires or generates root credentials, shown once on standard error and stored sealed | **implemented** |
| Sign-in to the console of a new deployment as `admin` / `admin` | **None until the operator acts.** A new deployment has that console user with `cluster-admin` (ADR-0014, Albie's decision). It is made once and never restored; the node warns in its log; `admin` cannot be set as a password again. The operator must delete the user or change the password before the admin port is reachable by others; a changed password does not end a session already open (30 minutes) nor remove keys, users or policies made through it - deleting the user ends its sessions and removes its keys, and the audit trail (`auth:SignIn`, `admin:*` by `admin`) shows whether anybody was first. The compose cluster publishes the admin port on loopback only | **accepted risk** |
| Forged S3 requests | SigV4 verification (checked against AWS's documented examples); host and every `x-amz-*` header must be signed; constant-time comparison; 15-minute clock-skew window | **implemented** (header authentication; presigned URLs M6) |
| Stolen API key used indefinitely | Expiry; last use recorded (to within five minutes); revocation that every node honours with the next request; an inline policy that narrows the key | **implemented**; there is no admin API to do it through until milestone 5 |
| Password guessing against the console | Argon2id (64 MiB, 3 passes); a refused sign-in looks and costs the same whether the user exists or not; sessions of 30 minutes stored as hashes; `__Host-` cookie that is `HttpOnly`, `Secure`, `SameSite=Strict`; CSRF token on every changing request | **implemented** in the use cases and helpers; **sign-in attempts are rate-limited per user and source**; the console that uses them is milestone 5 |
| Rogue node joining the cluster | Mutual TLS 1.3 for node RPC and Raft; only certificates of the cluster's node CA are accepted; a node's identity is the name in its certificate, never its address; every call passes the authorizer as that node; a node joins, leaves and records only itself; the Raft port admits voters only; no plaintext mode exists | **implemented**, with a limit: any node certificate may propose changes to buckets, objects and the topology, so a stolen one breaches the cluster's metadata, not just one node; the node CA must issue node certificates only — whatever it signs is a node. Certificates are read at start: rotation needs a restart, and there is no revocation list |
| A node is impersonated at its address (DNS or routing) | The caller checks that the certificate names the node it meant, whatever answered | **implemented** |
| A cut-off or deposed leader serves stale data | Reads on every node wait for a read index that the leader gives only after a majority of the *voters* confirm its term; a node without a majority answers nothing | **implemented**; tested with a network partition |
| An acknowledged write is lost when nodes fail | Acknowledged only after a write quorum of shards is durable on their nodes and the version is committed by a majority of voters | **implemented**; tested by killing nodes during writes |
| A node that is down is mistaken for lost data | An unreachable drive is neither read as damage nor a reason to rewrite or delete; the collector keeps anything a node it cannot ask might be writing | **implemented** |
| Old and new binaries diverge during an upgrade | No node writes a metadata entry format newer than the oldest member understands; a node that meets one it cannot apply stops and leaves the group. What every member derives while applying entries - the record of a bucket's latest uploads and downloads (ADR-0019) - is decided by the schema byte of the entry, which is in the log, never by what a member knows of the cluster when it applies it; a field an older binary would drop (`created_by` of a bucket) is stripped from an entry that claims the older schema, so both store the same bytes; a schema-3 state is not changed until the first entry of schema 4, after which the release before refuses it instead of misreading it | **implemented** |

### Tampering

| Threat | Control | Status |
|---|---|---|
| Traffic modified in transit | TLS 1.2+ for clients (1.3 preferred), ECDHE with AEAD suites only; a plaintext listener exists only behind `--dev-plaintext` and binds to a loopback IP | **implemented** (S3 and admin listeners) |
| Upload corrupted or cut short in transit | Declared SHA-256, `Content-MD5` (which must be signed) and CRC32/CRC32C/CRC64NVME/SHA-256 checksums are verified before anything becomes durable; a body the transport reports as cut short is never stored | **implemented** |
| A write reported as failed takes effect later | A commit whose outcome is unknown keeps its data; the state machine is fail-stop; a stale state database is rebuilt from the newest snapshot | **implemented** |
| Bitrot or deliberate modification of shards on disk | Position-bound SHA-256 per block and AES-GCM per segment, both verified on every read; unverified bytes are never returned; reads rebuild from parity; a scrubber verifies everything in the background and heals | **implemented** with four or more drives; below that, detection only |
| Reordered, truncated or spliced ciphertext | Streaming AEAD with a unique nonce per segment and the segment index and final flag bound into the AAD (ADR-0002) | **implemented** |
| An object made of parts is served with a part dropped, moved or swapped | Every part has a key of its own, derived from the upload's data key, its number and its data ID; the place of every part in the object - index, offset, count, total size - is sealed with an HMAC under a second derived key and verified before a byte of the part is returned (ADR-0010) | **implemented** |
| A customer key (SSE-C) is disclosed by the server | It is accepted over TLS only, in signed headers only, never in a query string; never stored and never logged, nor any digest of it; the decoded key and everything derived from it are cleared after use. Stored is a check value that takes the KMS key to test a guess against. Entity tags of SSE-C objects are not digests of their content | **implemented**; the key is in the gateway's memory for the length of a request, and the header as it arrived cannot be cleared from the HTTP server's buffers |
| Path traversal, reserved names or case collisions via object keys | Keys and bucket names never become filesystem paths; data lives under generated IDs, and all drive I/O is confined by `os.Root` | **implemented** |
| Forged or replayed metadata changes | All metadata changes are Raft entries, versioned, applied in order and exactly once; an entry this binary cannot interpret stops the node rather than being skipped | **implemented** (single voter); mTLS between voters M3 |
| Policy tampering | Policies live in Raft; every change is authorized and written to the audit trail in the same entry as the change | **implemented** |
| A policy silently means less than it says | Parsing is strict: an unknown field, operator or condition key is an error, never ignored; evaluation is fuzzed against its laws (silence refuses, a matching Deny always wins, a key's policy only takes away) | **implemented** |
| The audit trail is edited | Records are numbered and hash-chained by the state machine; reading verifies the chain; the latest hash is published as an event and can be exported | **implemented**; a rewrite of the whole chain is only detected by somebody who kept a hash outside |
| A node forges the record of who read an object (ADR-0016) | What a node counted is its own to report: node RPC passes a batch of reads through the authorizer and refuses one that names another node than the caller's certificate (`cluster:ReportReads`); the state machine applies a batch of a node the cluster does not know as nothing; a batch is applied once; each count is bounded and totals stop at their end instead of wrapping around; a read is recorded only for the version that was read, never for one that took its name | **accepted risk**: a node with a valid certificate - a compromised gateway - can still report whatever counts and readers it likes under its own name. The record is a count, not evidence, and is called that; it is not part of the audit trail, which such a node cannot forge this way |
| The audit trail is flooded or outrun | Refusals are written in the background, repeats within ten seconds counted into one record, the queue bounded | **implemented**: what a caller can vary (the key claimed, the object named) does not make a new record; at most 64 kinds of refusal are written per ten seconds and node, the rest counted. Limit: under flood a record per refusal can be dropped, and is then only in the node's log; changes are never dropped |

### Repudiation

| Threat | Control | Status |
|---|---|---|
| An administrator denies an action or erases the evidence | Append-only, hash-chained audit events for every admin mutation, authentication failure and authorization denial; audit records are never deleted | M4. Until then authentication failures and denials are written to the log, which is not tamper-evident |
| A search is used to look around unrecorded | By design a search leaves no audit record, and the buckets it passes over for a caller leave none: they are filtered with `Authorizer.Allowed`, the decision of `Authorize` without its record, or one search over N buckets would write N denials and bury the trail. `Allowed` is for filtering result sets only and gates nothing; what a caller asks for by name - a search of one bucket, or a search by somebody who may list nothing at all - goes through `Authorize` and is audited, once. A search shows only what a listing, itself unaudited, would show | **implemented**; accepted: there is no record of who searched for what |

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
| The version of the software told to anybody, to choose an attack by | The release, build and commit are named only in answers to authenticated callers (`server` in the session answers and `GET /cluster`), in `/metrics`, which is authenticated and authorized, and in the log. Nothing answered without a credential names them: not the sign-in page, a probe, a refusal, the contract, nor a response header | **implemented**, tested for the probes and for the refusals, the contract and the starting answers of the admin API; the documentation page, the console's files and the S3 API are given no build to name |
| Probe endpoints reveal internals | `/healthz` and `/readyz` return only `ok` or `unavailable`; the reason is logged, not returned | **implemented** |
| Eavesdropping | See TLS above | **implemented** (admin listener) |
| Data leaves an air-gapped site | No runtime call to the internet, no telemetry, no CDN asset | **implemented** |
| Who read an object is learned by somebody who should not know (ADR-0016) | The uploader, the counts and the readers are behind an action of their own, `admin:GetObjectAccess`, which reading the object does not give and only `cluster-admin` and `auditor` hold among the built-in roles; the single authorizer is asked before anything is said of the object, whether it exists included; the object's metadata and the S3 API say nothing of it; the object list says the uploader, the time of upload, the two counts and the time of the last read - never a reader - only when asked to (`access=true`) and only of the objects the caller holds `admin:GetObjectAccess` on, decided by the same authorizer with the same request as the single read, object by object, so that a deny by key holds in the list; a caller without it is given the plain list and no denial is audited, because the permission there filters a result and gates nothing - the list itself stays gated, and its refusal audited, by `s3:ListBucket`; no address of a reader is kept; no event carries a flush, no metric or span names a reader, and nothing the recorder logs names one ; the record of a bucket's latest uploads and downloads (`bucketactivity`, ADR-0019) names readers, uploaders and object keys as well and is kept in the same store: it is read out in one place, the list of buckets (next row) | **implemented**; **accepted risk**: metadata is not encrypted at rest, so the list of readers - and with it who created a bucket and its latest uploads and downloads - sits in the metadata store as object keys do |
| Who uses a bucket is learned through the list of buckets by somebody who should not know (ADR-0019) | `GET /buckets?details=true` says of each bucket only what the single authorizer allows this caller of that bucket, asked with the request the use case for one bucket is gated by: who created it and whether any of its latest uploads and downloads are shown, with `admin:GetObjectAccess` on the bucket, which `cluster-admin` and `auditor` hold among the built-in roles and listing or reading does not give; each upload and download - who and when - only where the same authorizer also allows `admin:GetObjectAccess` on the object the row names, the request `/object/access` is gated by, decided by the row's real key before any key is blanked, and a row that is not allowed is left out whole, so that an explicit deny by key holds in the list as it holds for the single read (holding the action on objects and not on the bucket shows nothing: the stricter reading); the keys of those objects only with `s3:ListBucket` on the bucket besides, the bucket's own policy included, so that the list names no object a listing would not; the summary of the policy - how many statements, and whether one allows something to every caller (conditions and denies not weighed: it is not a statement that the bucket is open to the unauthenticated), never what it says - with `s3:GetBucketPolicy`. The use case blanks what is not allowed before the handler sees it; the console shows what the API sent and decides nothing. A part that is left out is not refused and not audited, by design: the permissions filter a result and gate nothing, and a list of a thousand buckets must not write a thousand denials - the list itself stays gated, and its refusal audited, by `s3:ListAllMyBuckets`. A bucket whose stored policy is damaged is shown without keys and without a summary (fail closed). Without `details` the answer is what it was; the S3 API says nothing of any of it; no metric or span names who used a bucket | **implemented**, tested per role, per bucket with policies that differ between buckets, and per object with a deny by key; residual, accepted: the number of rows left out is not said, but fewer than three rows where a bucket has had more use tells a caller that something is kept from them; `access_included` and `policy_included` tell a caller which of those permissions they hold on a bucket, which they could learn by asking for the single read; the time a list takes grows with the number of buckets, as a search's does |
| Search discloses names of buckets or keys the caller may not list | Every bucket is decided by the one authorizer for every page, the bucket's policy included: its name is a result only with `s3:ListAllMyBuckets`, its keys are searched only with `s3:ListBucket` on it. The continuation token is validated (its bucket must be a valid bucket name), not signed, and grants nothing: each page first decides every bucket there is, from one walk of the bucket policies, and only then looks at where the token points, among the buckets the caller may see - so what a page does before its first key is the same for every token, and a token cannot be used to time whether a name is a bucket. The walk is given only buckets the caller may see: a stop, a token, `examined`, `complete` and the number of pages speak of those alone. Delete markers and older versions are not searched. The query is never logged | **implemented**, tested with made-up tokens (equal cost for each) and with hidden buckets (the same pages with and without them); residual, accepted: how long a page takes grows with the number of buckets in the deployment, whoever asks, so any caller who may search can estimate that number, and 503 tells them it is very large - not which names exist |

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
| A bucket is opened to the world by mistake or by a stolen key | Only a bucket policy lets a caller in without credentials, and only for a fixed list of read actions - never to write, delete or configure, whatever the policy says; setting a policy is its own permission and is audited with whether it opens anything to everyone; a policy that cannot be read refuses the request; no bucket policy can grant the governance bypass | **implemented**; a policy can still publish every object of a bucket to readers, which is what it is for |
| A browser is used against a bucket (cross-origin) | Requests are authenticated by signature, never by cookie, and `Access-Control-Allow-Credentials` is never sent; cross-origin answers are readable only for origins a rule of the bucket names; a refused preflight is the same for a missing bucket as for a missing rule | **implemented** |
| A lifecycle rule is used to destroy protected data | Rules are carried out through the same delete a client would call, as a service principal that may delete and abort and may never lift a retention; Object Lock is enforced by the state machine all the same; setting rules is its own permission, needs a signed body and is audited | **implemented**; a rule can still delete everything in a bucket that no lock keeps, which is what it is for, and a leader whose clock jumps far ahead expires early whatever no lock keeps |
| Requests routed to a node that is going away | `/readyz` answers 503 for `--drain-delay` before the listener closes; in-flight requests then finish within `--shutdown-timeout` | **implemented**; the delay defaults to `0s` (off) and must be set per deployment (chart default: M7) |
| Search is used to stall writes or exhaust a node | One request examines at most 50 000 keys and 200 buckets for at most two seconds and returns at most 500 results; the index is walked 5 000 keys at a time, so the metadata read lock a pending write waits for is held for about twenty milliseconds and let go between walks, and matching is done outside it; every query, `rx:` included, is one RE2 program of bounded size - no backtracking; one search at a time for a principal and eight for a node, then 429 with `Retry-After`. Deciding which buckets a caller may see costs one linearizable read a page (the listing of the buckets) and one walk of the bucket policies in the node's own state, not a question to the leader for each bucket. The walk is paged: a read transaction covers at most 256 buckets or 1 MiB of stored records and the read lock is let go before a policy is hashed or parsed and between pages - measured 1.5 ms a page where policies are 17 KiB (45 buckets), 0.9 ms a page of 256 buckets with 1.2 KiB policies (`BenchmarkLocalBucketPolicies`, Apple M3); before, the lock was held for the whole reading, which the review measured at 40 ms for every 1 000 buckets of the first kind. The authorizer remembers parsed policies by bucket and SHA-256 of the document just read (never a stale one, never a document that does not parse), in at most 128 MiB of documents (about 200 MiB of heap) and 131 072 entries, so a page hashes and does not parse: 5 000 buckets with 16 KiB policies are decided in 40 ms and 2.5 MB instead of 2.6 s and 2.3 GB (`BenchmarkSearchDeciding`), and a request keeps one page of policies and a bit a bucket. Deciding is inside the two seconds: when it takes them all the request fails with 503 and says why | **implemented**; residual, accepted: (1) just after a restart nothing is remembered, a page costs some ten times as much, and a deployment near the size above answers the first searches with 503 - each keeps what it parsed, so they pass; (2) **a limit, not a transient:** whoever may `s3:CreateBucket` and `s3:PutBucketPolicy` can make so many buckets with large policies - about 10 000 with 16 KiB each, or 150 000 with 1 KiB, on the machine measured - that deciding takes the two seconds even when everything that fits is remembered; search across buckets then answers 503 to everybody but the root identity (bound by no bucket policy, it reads none), each such request costing up to two CPU-seconds, at most eight at once a node; a search of one named bucket, and everything that is not a search, are not affected; a quota on buckets (milestone 8) is the remedy; (3) eight principals can hold all places of a node, which refuses search to others and nothing else |
| One tenant fills the cluster | Per-bucket quotas | M8 |

### Elevation of privilege

| Threat | Control | Status |
|---|---|---|
| A handler forgets an authorization check | Every use case calls the single `ports.Authorizer`, which decides on the policies resolved at authentication; no authorization logic in handlers; default deny; explicit deny wins | **implemented** |
| An unimplemented S3 feature is served as a different operation | Allow-list: any query parameter or `x-amz-*` header the routed operation does not implement, and a conditional header anywhere but on a plain read of an object, answers `501`; headers that belong to a copy answer `400` on a request that is not routed to a copy; an unparseable query answers `400` | **implemented** |
| A copy is used to read or write what the caller may not (confused deputy) | A copy is the use cases of reading and of writing in sequence, each authorized as itself against the caller's policies and the bucket's: `s3:GetObject` (or `GetObjectVersion`) on the source, `s3:PutObject` on the destination, the tag and lock permissions for what travels or is set; an SSE-C source takes its key, in signed headers, never in a query string; the destination inherits no lock and shares no data or key with the source; every `x-amz-*` header must be covered by the signature, so the holder of a presigned PUT cannot turn it into a copy | **implemented** |
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
| A test tool of the builder image changes under its name | The AWS CLI installer and the ceph/s3-tests archive are fetched by URL and accepted only by SHA-256; the Python packages of the tests are installed with `--require-hashes` from a lock file in the repository. They exist in the builder image only; the stage is not built for the product image | **implemented**; the tools themselves run with the privileges of the builder container during `make s3-compat`. The s3-tests archive is one GitHub generates for a commit: if its bytes change the builder build fails until the checksum is looked at again |
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
13. **Conditional writes** (`If-None-Match: *` and `If-Match` on a PUT, "create only
    if absent") answer `501` rather than being ignored: the condition would have to be
    part of the Raft entry, and that is not built. Conditional *reads* are implemented
    on GET and HEAD of an object and on the source of a copy; they are judged after
    authorization and, for an object under a customer key, after the key was shown.

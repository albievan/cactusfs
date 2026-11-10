# ADR-0029: Checking for the latest release, and upgrading from the console

- **Status:** accepted, pending Albie's review. The eleven open questions of PLAN Goal P
  were answered with their defaults by the session of 2026-11-09 ("Questions answered:
  defaults taken by the session on 2026-11-09, pending Albie's review"); each is listed
  under [Open, for the owner](#open-for-the-owner) with the default that was taken.
  **Built** (PLAN P.1 to P.11) and described here as built, after the review of
  2026-11-09 (PLAN P.12); the Kubernetes mode is tested against a fake API server, and
  `make test-k8s` on a real cluster is still to run.
- **Date:** 2026-11-09
- **Builds on:** ADR-0006 (identity and access, the audit trail), ADR-0007 (management
  API and console), ADR-0015 (the Helm chart: a StatefulSet for every node), ADR-0021
  (SBOMs and signatures: releases are signed keyless by the release workflow), ADR-0022
  (settings), ADR-0027 (the password of a session judged again)
- **Amends:** ADR-0023, "Outbound connections", for the update client only: it follows
  up to three redirects and reads more than 64 KiB ([below](#the-amendment-to-adr-0023))

## Context

Albie, 2026-11-09: "add functionallaty to check for the latest and allow to upgrade using
the webapp."

What the product had before this ADR:

- Every node knows its own build (`internal/buildinfo`: version, build number, commit)
  and shows it, and every other node's, to a caller allowed `admin:GetSettings`
  (ADR-0022). Nothing compares it to anything.
- Releases are made by `.github/workflows/release.yml` through `scripts/release.sh
  --publish` (ADR-0021): six binaries, `SHA256SUMS` signed keyless into
  `SHA256SUMS.sigstore.json`, a multi-arch image whose manifest list and platform images
  are signed by digest, the chart, and `digests.txt`. The signing identity is the
  workflow at the release's tag,
  `https://github.com/albievan/cactusfs/.github/workflows/release.yml@refs/tags/v<version>`,
  issued by `https://token.actions.githubusercontent.com`. `digests.txt` was not signed.
  No release has been published yet (`git tag -l` is empty on 2026-11-09).
- `CLAUDE.md` §3: "Air-gap safe. No runtime call to the internet, no telemetry, no CDN
  assets." The deployment calls out only where an operator named the place: the OTLP
  collector, Vault, targets of notifications and replication (ADR-0023).
- `CLAUDE.md` §2: version N must run alongside N+1 for rolling upgrades; only
  consecutive releases are tested together (`make test-rolling`).
- The chart runs every node as a StatefulSet of one pod with `updateStrategy: OnDelete`,
  a PodDisruptionBudget, and a default-deny NetworkPolicy (ADR-0015). Nothing in a pod
  can talk to the Kubernetes API: no ServiceAccount token is mounted.
- Outbound connections (ADR-0023): https only, the resolved address checked at every
  connect, no redirect followed, a response body read to 64 KiB and thrown away.

Installing code is the most powerful thing an administrator can do to a deployment:
whoever chooses what runs can read every object as it is decrypted. A feature that lets
the console do it must not become a way for anyone else to choose what runs - not the
network, not the server that lists releases, not a stolen session, not an older signed
release passed off as a newer one.

## Decision

### Off by default, and why

Nothing about releases is asked of any server until an operator sets **`update-source`**
(`CACTUSFS_UPDATE_SOURCE`). Nothing is installed until an operator also sets
**`upgrade-mode`** to something other than `off`. Both are off by default; neither has a
plaintext exception, a loopback included.

This reads `CLAUDE.md` §3 strictly: the deployment calls out only where the operator
named the place, as it does for OTLP, webhooks and other sites. A deployment that never
sets `update-source` makes no call it did not make before, and an air-gapped site can
point `update-source` at a mirror of its own (an https server that answers as the source
does) and carry releases across the gap by hand.

### The settings

Declared once in the settings registry (ADR-0022, PLAN P.9), each an environment
variable and a flag; a value that is refused stops the node at start.

| Setting | Default | What it is |
|---|---|---|
| `update-source` | none (off) | The address of the releases JSON: `https://api.github.com/repos/albievan/cactusfs/releases` or a mirror. https only, with a host and no user information, query or fragment: a value with any of them stops the node at start (`github.CheckSourceURL`, the rule the source itself applies; PLAN P.5, P.9), so that no credential is ever carried in it. Its path is shown with the node's settings, as the address of any other service is. |
| `update-check-interval` | `24h` | How often the node asks by itself, ± 10 % jitter, the first time within 5 minutes of its start; at least `1m`, or `0` to ask only when asked to from the console. A node asks at most once a minute however it is asked. |
| `update-channel` | `stable` | `stable` or `prerelease` (`release.ParseChannel`). |
| `update-ca` | none (the system's roots) | A PEM file of the roots the source's certificate is verified against. There is no way to skip verification. Only with `update-source`; a file without a certificate stops the node at start. |
| `update-trusted-root` | none (the embedded root) | A Sigstore `trusted_root.json` that replaces the embedded one. Only with `update-source`. |
| `upgrade-mode` | `off` | `off`, `binary` or `kubernetes` ([the modes](#the-three-modes)), for each node. `binary` is refused at start by the binary of the container image and without `update-source`; `kubernetes` is refused at start when the chart has not mounted the token of the Kubernetes API (`upgrade.fromConsole.enabled`). |

### The trust chain

What the source says - the list of releases, their dates, their files, their notes - is
not trusted: it decides what is offered, never what is installed. What is installed is
believed only through this chain, every link checked on the node, offline:

1. **The bundle.** `SHA256SUMS.sigstore.json` (binaries) or `digests.txt.sigstore.json`
   (the image) is verified by `ports.ReleaseVerifier` (`internal/adapters/releases/
   sigstore`, `sigstore-go`, PLAN P.4) against the trusted root: the Fulcio certificate
   chain, one transparency-log entry (`WithTransparencyLog(1)`), one observer timestamp
   (`WithObserverTimestamps(1)`), one signed certificate timestamp
   (`WithSignedCertificateTimestamps(1)`). No network: the bundle carries what is
   needed, and the trusted root is in the binary.
2. **The identity of the tag.** The certificate must name exactly the workflow at the tag
   of the version being installed, `…/release.yml@refs/tags/v<target>`, and the issuer
   `https://token.actions.githubusercontent.com`. Binding the identity to the target's
   own tag is what stops an older signed release being passed off as a newer one: the
   bundle of 0.7.0 does not verify as 0.7.1.
3. **The list.** The signed bytes are `SHA256SUMS` or `digests.txt`, read strictly by
   `release.ParseSums` and `release.ParseDigests` (`internal/domain/release`): one shape
   of line, no name twice, bounded in size and lines. `digests.txt` must give the image
   the tag of the version exactly (the line `release.sh` writes last, once the list is
   signed and verified).
4. **The artifact.** A binary's SHA-256, computed while it is written, must be the one
   `SHA256SUMS` lists under `release.BinaryAsset(GOOS, GOARCH)`; the image is installed
   by the digest of its manifest list, `<current repository>@sha256:…`, which the
   registry and the container runtime check for themselves.

The files are fetched only from where the operator pointed: an asset's address must be
https at the source's own scheme, host and port, or - when the source is GitHub's API,
`api.github.com` - at `github.com:443`, where GitHub's `browser_download_url` names every
file (`https://github.com/<owner>/<repo>/releases/download/<tag>/<name>`). Any other
address in the releases JSON is refused (class `refused`) and never asked. What that
host redirects to - GitHub's storage, on a host of its own - is followed under the
egress rules of [the amendment](#the-amendment-to-adr-0023).

Only then is a `release.Verified` made - by `release.NewVerifiedBinary(version, sums,
name, sha256)` or `release.NewVerifiedImage(version, digests)` - and `ports.Upgrader`
takes nothing else. The domain cannot see that the list was verified; the one caller
that makes a `Verified` (`app.UpdateService`, PLAN P.6) does so only from the bytes the
verifier accepted, in the same function.

`digests.txt` is signed from now on (`scripts/release.sh`, PLAN P.4). Releases made
before that cannot be installed in the Kubernetes mode.

### The trusted root, and how it is refreshed

Sigstore's public-good `trusted_root.json` is embedded in the binary
(`internal/adapters/releases/sigstore/trusted_root.json`). `make trusted-root` (manual,
network, not part of `make check`) fetches it again through the TUF client of
`sigstore-go` (`internal/tools/trustedroot`), and it is refreshed before every release,
so that a release carries a root that verifies the next ones. A deployment that has been
offline across a rotation of Sigstore's keys, or a site with its own Sigstore, sets
`update-trusted-root`. A root that does not parse stops the node at start.

### What may be installed

`release.Allowed(current, target, releases, channel)`:

- **a newer version of the running minor version** (same major and minor), or
- **the newest version of the next minor version** (same major) among the releases the
  channel offers.

Refused, each with its own reason (`release.Reason`, a `*release.Refusal` wrapping
`domain.ErrUpgradeNotAllowed`): `equal`, `older` (nothing is downgraded from the
console), `prerelease-on-stable`, `major`, `skipped-minor`, `not-newest-patch`,
`unknown-release` (of the next minor and not listed, so it cannot be the newest).
Pre-releases are offered and installable only on the channel `prerelease`; a release is a
pre-release when its version is one or the source marks it so.

The reason: only consecutive releases run alongside each other in the tests of a rolling
upgrade. From 0.7.0 the console offers 0.7.x and the newest 0.8.y; to 0.9 it goes through
0.8. `release.NextStep` is the newest release `Allowed` admits.

### The three modes

`ports.Upgrader`, one adapter per mode; `Availability` says whether it can run here,
and if not why and what an operator does instead (the console shows that as text to
copy).

**`off`** (the default). Nothing is installed. The console shows what was found and the
commands of the operator's own way: a binary to download and its checksum to check, or
`helm upgrade … --set image.digest=sha256:…`. Every deployment that is not one of the two
below - a cluster of binaries on VMs, compose - stays here.

**`binary`** (PLAN P.7). Needs: a binary that was built as one (`buildinfo.Packaging()`
is `binary`, not `image`), a deployment of one node, and a directory of the executable
the process can write. The binary of `release.BinaryAsset(GOOS, GOARCH)` is downloaded
beside the executable into `os.CreateTemp`, its SHA-256 computed as it is written, then
fsynced; `<new> version` must run and name the target; the old executable is kept as
`cactusfs.previous`; the new one is renamed into place. Unix: the node stops gracefully
and `syscall.Exec`s the new binary with the same arguments and environment. Windows: a
running executable can be renamed but not re-executed in place; the state is
`restart-required`, and whoever runs the service restarts it. The run outlives the
request that started it (at most 30 minutes) but not the node: when the node's lifetime
ends before the swap begins, the run ends there - `failed`, the old executable in
place, the temporary file removed; a swap once begun completes, so that the
executable's path always names a whole binary.

**`kubernetes`** (PLAN P.8). Needs: the chart's `upgrade.fromConsole.enabled` (false by
default), which gives the cactusfs container - and only it - a projected ServiceAccount
token, a Role that may get and patch the StatefulSets of this release by
`resourceNames`, read their pods and create evictions, and a NetworkPolicy egress to the
API server. Plain `net/http` to the in-cluster API; no client library. One node at a
time, **this node last**: patch the StatefulSet's image to `<current repository>@<the
verified list digest>` and `app.kubernetes.io/version`, evict the pod (the
PodDisruptionBudget respected), wait until the pod is Ready, the node reports the
target's build (the settings RPC) and `/readyz` answers. **The first node that does not
come up is put back** to the image it ran and the run stops (`failed`); nothing is
patched after it - unless the run has lost its lock meanwhile (below), which it reads
again, under the run's own time and not the node's, before it puts anything back: a run
whose lock another node took touches nothing more.

Helm drift: the console patches StatefulSets, which Helm then sees as changed. The
console shows the `helm upgrade --set image.digest=…` that brings the release's values
in line, and the chart's README says so: the next `helm upgrade` must name the installed
digest or a newer one, or, with `OnDelete`, it leaves the image of its own values in the
StatefulSets for the next pod that is deleted.

**What the token reaches.** The Role names the StatefulSets by `resourceNames`, but
`patch` of a StatefulSet changes its pod template, which is as good as creating any pod
in the namespace: one that mounts every Secret there - the Vault token, the root
credentials, the TLS keys, the node CA's key - runs as any ServiceAccount there, and runs
any image. The `restricted` Pod Security Standard limits privileges, not which Secrets
or ServiceAccounts a pod uses. Whoever holds the token holds the namespace and its
secrets; the chart's README says so and recommends a namespace of the chart's own. A
ValidatingAdmissionPolicy that lets this ServiceAccount's patches change only the
cactusfs container's image (to an `@sha256:` digest), the label
`app.kubernetes.io/version` and annotations `cactusfs.io/*` would close it; it is not
built ([open question 10](#open-for-the-owner)).

### Who may, and the password judged by the server

- `admin:GetUpdates` - read what the node knows (`GET /api/v1/updates`, `GET
  /api/v1/upgrade`). `cluster-admin`, and `auditor` through `admin:Get*`.
- `admin:CheckForUpdates` - ask the source now (`POST /api/v1/updates/check`). It reads,
  but makes the deployment call out: it does not begin with Get. `cluster-admin` only.
- `admin:UpgradeCluster` - install (`POST /api/v1/upgrade`). `cluster-admin` only.

All three are asked of the one resource `updates` (`arn:cactusfs:admin:::updates`), the
progress (`GET /upgrade`) included, so that a policy that denies that resource denies
all of them, and a grant of it alone allows them.

Installing is allowed **only to a console session whose user types the password again
in that very request** (`{version, password}`). The password is judged by the server
inside the request, by `SessionService.Confirm` - the path of `Unlock` (ADR-0027) without
rotating the session's token: the same Argon2id, decoy, per-address limiter and audit of
a failure. It is not recorded on the session: sessions are replicated, and a
"re-authenticated at" field would change the entry schema. **An API key can never
install**, whatever its policy says: a bearer credential has no password to type. The
console holds no rule of who may; it shows the button to a session that the API says is
allowed, and the API decides again.

One upgrade at a time (`domain.ErrUpgradeRunning`). Every check and every start is
audited: the actor, from, to, the digest installed and the outcome.

### The management API (PLAN P.2)

- `GET /api/v1/updates`: the running build, `source_configured`, `channel`,
  `checked_at`, `last_error` (a class only, never the source's words; the classes are
  fixed by the contract, P.2), `available[]` with `allowed` and
  `refusal`, `next_step`, `mode`, `unavailable_reason`, `instructions`.
- `POST /api/v1/updates/check`: 409 `UpdatesOff` without a source; at most one a minute
  a node (429).
- `POST /api/v1/upgrade` `{version, password}` → 202; problem+json 400, 401 (password),
  403, 409 (`UpgradeNotAllowed`, `UpgradeRunning`), 412 (`ReleaseNotVerified`, whose
  detail is a fixed sentence: what failed is in the node's log, not the answer), 423,
  429, 501 (`UpgradeUnavailable`). `Idempotency-Key` is accepted without effect, as on
  `/session/unlock`: a stored answer must not replay a password check.
- `GET /api/v1/upgrade`: the progress, `idle | verifying | running | restart-required |
  failed | done`, and each node with its version, build, state and reason.

New sentinels, none of them a denial: `ErrUpdatesOff` (409 `UpdatesOff`),
`ErrUpgradeNotAllowed` (409 `UpgradeNotAllowed`), `ErrUpgradeRunning` (409
`UpgradeRunning`), `ErrReleaseNotVerified` (412 `ReleaseNotVerified`),
`ErrUpgradeUnavailable` (501 `UpgradeUnavailable`, logged at info: it is how the
deployment was set up, not a fault). They have wire names (`updates_off`,
`upgrade_not_allowed`, `upgrade_running`, `release_not_verified`,
`upgrade_unavailable`); a node of the previous release reads them as an unclassified
failure, which nothing between nodes depends on. The S3 API never meets them.

### Nothing replicated; progress read from the cluster

What a node found is kept in its memory and asked again after a restart. **No Raft entry,
no state, no AsyncAPI event**: events come from applied entries only, and an upgrade
writes none. The audit trail is the record.

The progress of an upgrade is read from where it happens: in the Kubernetes mode from
the StatefulSets, the pods and each node's build (the settings RPC); in the binary mode
from the process itself. **The run is held by a lock annotation**, `cactusfs.io/upgrade`
on the StatefulSet of the first node by name, written with its `resourceVersion` so that
two gateways that start at once cannot both win; it names the run, the target, the image,
the holder, the actor and the time, and its holder writes a heartbeat into it every
quarter of a node's time. A Lease was not chosen: RBAC cannot limit the `create` of a
Lease by name. How a run ends, as built:

- **Failure**: the holder replaces the lock with `cactusfs.io/upgrade-result` (state
  `failed`, the node and why), under `resourceVersion`.
- **Success**: the holder moves itself last, so its own pod is evicted before it could
  write anything. Once every node runs the lock's image, is Ready and reports the
  target, the lock is replaced by the result `done`, under `resourceVersion`, by a node
  that no live run can contradict: the holder's own node once restarted - as it starts
  (`kubernetes.Upgrader.Settle`, for up to twice a node's time) or when it answers the
  progress - or any node once the lock is stale. Any other node reading the progress
  (`GET /upgrade`, which the auditor may call) says `done` and writes nothing: a holder
  that is alive may still be waiting for the cluster to be ready, and ends its run
  itself. A run still under way whose lock was replaced by its own result ends `done`.
- **Takeover**: a lock is taken over by a new start when its heartbeat is older than
  twice a node's time, when its holder is the starting node (whose process holds no
  run), when it does not parse, or when every node is already at its target. A run that
  finds its lock taken - at a heartbeat, or before it puts a node back - stops,
  `failed`, and touches nothing more; the new run resumes, skipping nodes already at
  the target and putting back from `cactusfs.io/upgrade-previous`.

### The amendment to ADR-0023

For the update client only (`egress.Policy` gains `MaxResponseBody` and `Redirects`; the
zero values keep the rules of ADR-0023 for every other client, PLAN P.5):

- **Up to 3 redirects**, each to `https` only, each hop's resolved address checked in the
  dialer as the first is. GitHub answers every asset download with a redirect to its
  storage. No cookie, no `Authorization` header, no token is ever sent, so nothing can
  leak across a redirect. The first request of a download goes only to the source's
  host (or `github.com` for GitHub's API, [above](#the-trust-chain)); a redirect may
  lead anywhere the egress rules admit, private ranges included, so a hostile source or
  mirror can still have the node send a GET without credentials to an https service it
  can reach - a residual risk in the threat model, bounded in Kubernetes by the
  NetworkPolicy.
- **Bodies up to 1 MiB of JSON** (GitHub's releases JSON exceeds 64 KiB) **and 256 MiB of
  a binary**; `Content-Length`, when given, is checked before a byte is read, and what is
  read is counted regardless.
- `User-Agent: cactusfs/<version>`. No proxy, whatever the environment says.

### What is left out

- **Clusters of binaries on VMs, and compose**: `off` only, with the commands to run.
  Coordinating restarts across machines the node does not control is not attempted.
- **Restarting on Windows**: `restart-required`; the service manager restarts.
- **HTTPS proxies**: none. A site behind one runs a mirror.
- **Downgrades** from the console: never. The previous binary is kept beside the new one
  (`binary`), and the previous image digest is in the audit record (`kubernetes`).
- **A ValidatingAdmissionPolicy** that limits the upgrade ServiceAccount's patches to
  the cactusfs container's image (an `@sha256:` digest), the version label and
  `cactusfs.io/*` annotations: not built, a residual risk ([what the token
  reaches](#the-three-modes), open question 10).
- **Release notes** are a link, shown and never fetched.

## Rolling upgrades and rollback

- **No change of the entry schema, of the state, of the on-disk format or of the crypto
  design.** The upgrade itself is a rolling upgrade from N to N+1, which the product
  supports already.
- A node of the previous release does not serve the endpoints: a path it does not know is
  answered 404 `NotFound`, and the console says that updates are not available on the node
  that answered.
- A policy that names one of the three actions is readable by the previous release - it
  accepts any `admin:<name>` - and grants nothing there.
- **Rollback** of this feature is the previous binary: nothing is left behind. Rollback
  of an upgrade it made is the operator's: the kept binary, or `helm upgrade` with the
  previous digest.

## Consequences

- An operator who turns checks on makes the deployment call one host they named once a
  day; the console tells which releases exist and which may be installed.
- Whoever holds `admin:UpgradeCluster` and the password of a console session chooses the
  code the deployment runs - but only code the release workflow signed for that version.
  A compromise of the release workflow, or of the GitHub account that may tag, is a
  compromise of every deployment that upgrades from the console (it is already one of
  every deployment that upgrades by hand).
- The Kubernetes mode gives a pod a token that can patch its own StatefulSets - and so,
  in effect, create any pod in the namespace, with its Secrets and ServiceAccounts.
  That token is an asset of its own (threat model, A9): scoped by `resourceNames` to
  which StatefulSets, not to what they may say, mounted into the cactusfs container
  only, never logged, and only when the chart says so. The namespace should be the
  chart's alone.
- The binary is larger by `sigstore-go` (measured in PLAN P.4; the image must stay under
  60 MB).
- The embedded trusted root ages. A deployment that skips many releases may hold a root
  that no longer verifies new bundles; `update-trusted-root` is the remedy, and the
  refusal says so.

## Alternatives considered

| Alternative | Why not |
|---|---|
| Checks on by default against GitHub | `CLAUDE.md` §3: no runtime call to the internet. |
| Trust the TLS connection to GitHub, or the checksums alone | Whoever controls the source, or the path, would choose what runs. |
| A key of our own baked into the binary | Keyless signing is on record (2026-10-12); a long-lived key is a secret to keep, and to lose. |
| A hand-written Sigstore verifier | `CLAUDE.md` §3: never write your own primitive; P.4 is a gate, and its failure blocks the goal. |
| Accept any newer signed release | Skipping a minor is a combination that was never tested together. |
| Record "re-authenticated at" on the session | A field of a replicated record: a new entry schema. |
| A Kubernetes operator, or a Job per upgrade | Another component to install and secure; the gateway already knows the order and the checks. |
| A Lease as the lock | RBAC cannot limit `create` of a Lease by name. |
| Replicate the check's result through Raft | Nothing else needs it; an entry schema change for a cache. |

## Tests

| Claim | Test |
|---|---|
| Versions, their order, every refusal, the next step | `TestParseVersion`, `TestCompareFollowsSemver`, `TestCompareIsATotalOrder`, `FuzzParseVersion`, `TestAvailable`, `TestAllowed`, `TestAllowedNeverAdmitsWhatItMustNot`, `TestNextStep` (`internal/domain/release`) |
| The signed lists, read strictly | `TestParseSums`, `TestParseDigests`, `FuzzParseSums`, `FuzzParseDigests` (`internal/domain/release`) |
| A `Verified` only from the list and the bytes it names | `TestVerified`, `TestVerifiedBinaryOfRealBytes` (`internal/domain/release`) |
| The built-in roles | `TestBuiltInRoles` (`internal/domain/iam`) |
| The sentinels have answers in both APIs | `TestEverySentinelIsInOneOfTheTwoLists` (`internal/domain`), `TestEverySentinelHasAnAnswer` (`adminapi`, `s3api`) |
| The ports' promises | `ReleaseSourceContract`, `ReleaseVerifierContract`, `UpgraderContract`, `UnavailableUpgraderContract` (`internal/ports/porttest`) |
| The verifier, every refusal against a VirtualSigstore | `TestVerify`, `FuzzVerifyBundle`, `TestReleaseSignsWhatTheVerifierReads`, `TestDefault` (`internal/adapters/releases/sigstore`) |
| The source, redirects, bounds, only the source's host | `TestFetchRedirects`, `TestFetchBounds`, `TestStall`, `TestRefusedBeforeAsking`, `TestFetchOnlyFromTheSourcesHost`, `TestTheAssetHostsOfGitHub` (`internal/adapters/releases/github`); `TestRedirectsFollowed`, `TestRedirectsRefused` (`egress`) |
| The use case, the password, the audit, the one resource | `TestUpdatesGet`, `TestUpdatesCheck`, `TestUpdatesStart`, `TestUpdatesActionsAreAskedOfTheUpdatesResource`, `TestUpdatesRun` (`internal/app`); `TestUpdatesThroughTheAPI` (`adminapi`) |
| The binary upgrader: crash at every step, the node stopping before and during the swap | `TestACrashLeavesAWholeBinary`, `TestTheNodeStoppingEndsARunUntilTheSwapBegins`, `TestTheBinaryIsReplaced` (`internal/adapters/upgrade/binary`) |
| The Kubernetes upgrader: order, lock, takeover, put back, success settled | `TestAnUpgradeMovesEveryNodeInOrderThisNodeLast`, `TestALockHeldByAnotherNode`, `TestANodeThatDoesNotComeUpIsPutBackAndTheRunStops`, `TestTimeRunningOutAfterTheLockWasTakenOverPutsNothingBack`, `TestARunSeenDoneByAnotherNodeEndsDone`, `TestSettleAtStart`, `TestProgress` (`internal/adapters/upgrade/kubernetes`, against a fake API server; `make test-k8s` still to run) |
| End to end: a standalone binary upgraded and restarted; tampered and mis-tagged releases refused | `TestUpdates` (`test/integration`, a TLS mirror signed with `sigstoretest`) |

## Open, for the owner

The defaults below were taken by the session of 2026-11-09, pending Albie's review.

1. Checks off until `CACTUSFS_UPDATE_SOURCE` is set. Default: off.
2. Installing as a second opt-in (`upgrade-mode`, Helm `upgrade.fromConsole.enabled`).
   Default: off.
3. Only a patch of the running minor or the next minor's newest patch. Default: yes.
4. The password judged by the server; API keys cannot install. Default: yes.
5. Helm drift: the console patches StatefulSets and shows the reconciling `helm upgrade
   --set image.digest=…`. Default: accept.
6. Windows: replaced, `restart-required`, no automatic restart. Default: yes.
7. Clusters of binaries on VMs: commands only. Default: yes.
8. The trusted root embedded, refreshed each release, `update-trusted-root` overrides.
   Default: accept.
9. No HTTPS proxy. Default: yes.
10. The Kubernetes token's reach: `patch` of the StatefulSets is, in effect, creating
    any pod in the namespace (every Secret, any ServiceAccount, any image), whatever
    `resourceNames` and the `restricted` Pod Security Standard say. A
    ValidatingAdmissionPolicy limiting this ServiceAccount's patches to the container
    image (`@sha256:`), the version label and `cactusfs.io/*` annotations would close it.
    Default: residual risk, documented, a namespace of the chart's own recommended; the
    policy later (threat model, open question 37).
11. Release notes a link only. Default: yes.

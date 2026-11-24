# CactusFS

An S3-compatible, erasure-coded, multi-node object store in a single Go binary, with a
web console. Data security comes first: encryption at rest that cannot be switched off,
TLS on every listener, checksums verified on every read, self-healing, role-based access
control and a hash-chained audit trail.

> **Status: pre-release, version 0.7.0.** Its image `ghcr.io/albievan/cactusfs:0.7.0` and
> chart `ghcr.io/albievan/charts/cactusfs:0.7.0` are published and signed; 0.7.0 has no
> GitHub Release with binaries (that last step failed), so build the binary from source.
> Milestones 0 to 7 of [`PLAN.md`](PLAN.md) are done: the standalone store, erasure
> coding, the cluster, identities and policies, the management API with its console, the
> S3 depth listed below, and deployment hardening (the Helm chart, certificate rotation,
> the observability pack, SBOMs, signatures and the release workflow). Milestone 8 is
> done too (PLAN 8.9, 2026-11-17): its metadata schema (entry schema 5) and its
> contracts, and its three features - quotas on buckets, bucket notifications and
> replication between sites ([Quotas](#quotas), [Notifications](#notifications),
> [Replication between sites](#replication-between-sites)) - in no published release
> yet. Milestone 9, the visual policy builder with "Who may do this?"
> ([The policy builder, and "Who may do this?"](#the-policy-builder-and-who-may-do-this)),
> is built up to PLAN 9.10, and is in no published release either: its test on four
> nodes and its review in a browser are still to come. Milestone 10, one deployment
> script for every container platform
> ([Deploying with deploy/deploy.sh](#deploying-with-deploydeploysh)), is built and is
> in no published release; it has been run on Docker Desktop, a remote k3s server
> (PLAN 10.13) and Rancher Desktop 1.25 (moby, containerd and its Kubernetes, PLAN 10.6),
> not yet on a Kubernetes other than k3s.
> [What is not there yet](#what-is-not-there-yet) says the rest plainly.
> CactusFS is not offered as ready for production, and no independent review of its
> security is recorded here: do not keep data in it that you cannot lose.

> **A new deployment has the console user `admin` with the password `admin`**, and that
> user holds every right ([ADR-0014](docs/adr/0014-default-console-user.md)). It is the
> same on every new deployment and known to everybody who reads this page. **Delete that
> user, or change its password, before anybody else can reach the admin port (9001).**
> See [First sign-in](#first-sign-in).

## Contents

- [What it does](#what-it-does)
- [What is not there yet](#what-is-not-there-yet)
- [Build from source](#build-from-source)
- [Quick start: one node](#quick-start-one-node)
- [First sign-in](#first-sign-in)
- [Addresses](#addresses)
- [Running a node](#running-a-node)
- [Settings](#settings)
- [Keys in Vault](#keys-in-vault)
- [A cluster](#a-cluster)
- [On Kubernetes](#on-kubernetes)
- [Deploying with deploy/deploy.sh](#deploying-with-deploydeploysh)
- [The S3 API](#the-s3-api)
- [Management API, documentation and console](#management-api-documentation-and-console)
- [Users, keys and the audit trail](#users-keys-and-the-audit-trail)
- [Operations](#operations)
- [Versions and builds](#versions-and-builds)
- [Checking for releases and upgrading](#checking-for-releases-and-upgrading)
- [Develop](#develop)
- [Verifying an image](#verifying-an-image)
- [Releases](#releases)
- [Layout](#layout)
- [Documentation](#documentation)
- [Security](#security)
- [Licence](#licence)

## What it does

- **One binary, two ways to run it.** `cactusfs server` on one machine (Linux, macOS,
  Windows; amd64, arm64; no cgo), or the same binary on four or more nodes as a cluster.
  A node on its own is a cluster of one, not a second implementation. No external
  database, and no proxy in front of it.
- **Durability.** Reed-Solomon erasure coding over sets of 4 to 16 drives (parity 2 for
  4-5 drives, 3 for 6-7, 4 for 8 or more); a SHA-256 checksum on every block, verified on
  every read; a scrubber that verifies everything in the background and heals from
  parity; garbage collection of shards nothing refers to. With fewer than four drives
  objects are encrypted and checksummed but have **no parity**, and the node says so.
- **Encryption at rest, always.** AES-256-GCM as a streaming AEAD, a data key for every
  object, wrapped by a KMS key: HashiCorp Vault Transit, or a sealed local key file.
  SSE-S3, SSE-KMS and SSE-C. There is no unencrypted mode.
- **Encryption in transit.** TLS 1.2 or later for clients, mutual TLS 1.3 between nodes,
  the Raft transport included. Certificates are replaced on disk without a restart.
- **Metadata** replicated with embedded Raft and kept in bbolt; reads and listings are
  strongly consistent. A write is acknowledged only when its shards are durable on a
  write quorum and its version is committed through Raft.
- **S3 API** with SigV4 (header, presigned, streaming): buckets and objects, multipart
  uploads, versioning, Object Lock, lifecycle rules, bucket policies, CORS, tagging,
  copies. [The S3 API](#the-s3-api) lists the operations.
- **Identities.** Users, groups, roles and IAM-style policies; API keys that expire, can
  be narrowed and are revoked at once; five built-in roles; a single authorizer with
  default deny; a hash-chained audit trail.
- **Sub-buckets** ([ADR-0017](docs/adr/0017-sub-buckets.md)): up to 10 named first-level
  prefixes of a bucket, each with a policy of its own that the single authorizer weighs
  with the bucket's and the caller's; S3 clients address their objects as keys of the
  bucket. Listed, created, given a policy and deleted (when empty) through the management
  API (`/api/v1/buckets/{bucket}/subbuckets`) and in the console, in a bucket's details.
- **Tag rules and a catalogue of tags** ([ADR-0018](docs/adr/0018-tag-rules-and-catalogue.md)):
  a bucket's rules keep its tags in lower case, take only tags of the deployment's
  catalogue, or require up to 10 tags (with the values allowed, for the whole bucket or
  one sub-bucket). They hold on every way an object comes to have tags - S3's
  `PutObject`, `CopyObject`, `PutObjectTagging`, `DeleteObjectTagging`, multipart uploads
  (tags on `CreateMultipartUpload`, checked again at completion) and the management API's
  upload - and a broken rule is refused (S3 `400 InvalidTag`, the management API
  `TagRuleViolated`), naming the rule, never a value. A bucket made through the
  management API or the console lower-cases its tags from the start; one made with S3's
  `CreateBucket` has no rules. Set through the management API and in the console;
  administrators keep the catalogue on the **Tags** screen. See
  [Tags and their rules](#tags-and-their-rules) for what a rule asks of uploaders.
- **Management API** (REST/JSON, designed first in OpenAPI) with Swagger UI, and a **web
  console** embedded in the binary: buckets as a grid with their creator, policy,
  latest use and how full each is against its quota; an object browser with upload,
  download, an object's details opened in its own row, and who uploaded and how often an object was downloaded; uploads that go
  on while you use other screens, with a bar at the top of every screen that shows them
  and what the console did; search; users, groups, roles, policies and API keys; the
  cluster; the settings of every node; the audit log. A bucket's page shows its details
  and can delete every object, after asking for your password. Its details hold, folded
  until you open them, the bucket's policy - shown as JSON, edited with the policy
  builder or as JSON, saved and removed - and
  its sub-buckets: listed, created with or without a policy, each one's policy edited or
  removed, and an empty one deleted (ADR-0017) - and its tags and tag rules: lower case,
  the catalogue only, and the tags every object must carry (ADR-0018) - and its
  notification rules: which changes, of which keys, are sent to which target (ADR-0025) -
  and its replication: where it is sent and how far that has come, a resync, and whether
  it is a replica, with a promotion that asks for its name to be typed (ADR-0026).
  An object's tags are edited in its details, and the upload form takes tags, says which
  the bucket requires and begins with them. The **Tags** screen keeps the catalogue of
  tags users are offered; the **Targets** screen the places notifications and the audit
  trail are sent to, with how delivery to each stands. Every kind of policy - a named
  policy, the inline policy of a new API key, a bucket's and a sub-bucket's - is edited
  in a **visual policy builder** beside the JSON it comes to, checked by the node as you
  type, and sent as the JSON shows it; **Who may do this?** asks the node what it would
  decide for a user, a key or anyone, and why
  ([The policy builder, and "Who may do this?"](#the-policy-builder-and-who-may-do-this)).
  An object you may read and either uploaded
  or may preview opens in a preview - images, text, CSV, JSON, PDF, DOCX and XLSX - drawn
  by the console itself, never served inline (ADR-0028). A session that times out
  while it uploads is locked, not ended, until your password unlocks it.
- **Bucket notifications** ([ADR-0025](docs/adr/0025-bucket-notifications.md)): S3's
  notification configuration (`PutBucketNotificationConfiguration`, queues and topics
  named `arn:cactusfs:notify:::<target>`) and the management API's rules send S3's
  event record of each put, completed upload, delete marker and removed version to a
  webhook - signed with HMAC-SHA256 - or to a NATS JetStream server, at least once and
  in order, from the outbox of the metadata; a target can be sent the hash-chained audit
  trail as well. Targets are defined by administrators, never by a bucket's owner, and
  each says which buckets may use it ([Notifications](#notifications)).
- **Replication between sites** ([ADR-0026](docs/adr/0026-replication-between-sites.md)):
  the versions of a bucket are copied, asynchronously and with their version IDs, times
  and order, to a bucket of another CactusFS deployment - its own cluster, CA, keys and
  users - that is a read-only replica until it is promoted. Configured with S3's
  `PutBucketReplication` or the management API, sent by the node that leads as an S3
  client of the other site with a key that site issued and that can do nothing but
  replicate; a resync sends what was there before; the status of each bucket, metrics
  and alerts say how far it has come ([below](#replication-between-sites)).
- **Deployment.** A distroless, non-root container image; a four-node compose cluster
  for development (Docker or rootless Podman); a Helm chart for Kubernetes and k3s with
  a default-deny NetworkPolicy under the `restricted` Pod Security Standard.
- **Observability.** JSON logs, Prometheus metrics (`cactusfs_*`) behind authentication,
  OpenTelemetry traces to a collector you name, and a pack of alerting rules and Grafana
  dashboards in [`deploy/observability`](deploy/observability/README.md).
- **Supply chain.** Base images pinned by digest, a licence allow-list, SPDX SBOMs, and
  images signed by digest - proved offline in the gate; a release workflow that signs
  keyless, which signed the image and chart of `v0.7.0`.
- **Updates from the console, off by default.** Told where releases are, a node checks
  for newer ones and the console shows them; told it may, it installs one - only a
  release the project's release workflow signed for that version, verified offline
  ([below](#checking-for-releases-and-upgrading)).
- **Air-gap safe.** The binary calls nothing on the internet, sends no telemetry and
  serves no asset from a CDN. Checking for releases is off until you name the place.

## What is not there yet

Said plainly, so that nobody finds out by trying:

- **The first release is incomplete.** `v0.7.0` (2026-11-13) published and signed its
  image, `ghcr.io/albievan/cactusfs:0.7.0`, and its chart,
  `oci://ghcr.io/albievan/charts/cactusfs` version `0.7.0`; the workflow then failed to
  create the GitHub Release (a permission error on the Sigstore bundle, fixed in the
  scripts since). So there is no GitHub Release: no published binaries, `SHA256SUMS` or
  `digests.txt`, and a node's check for releases, which reads GitHub's releases, finds
  none. The binaries ship with the next release. `v0.7.0` predates tag rules and their
  catalogue: no published release enforces them yet.
- **Replication is active-passive, one hop, and fails back by hand.** A bucket is
  written by its clients or by one source, never both; a bucket has one destination;
  a replica is never sent on; permanent deletes, bucket configuration, policies and
  identities are not replicated, nor are versions under a customer's key (SSE-C). There
  is no automatic fail-back, and a fail-back carries only what the promoted bucket wrote
  itself ([what fail-back is not](#what-fail-back-is-not)). Only prefixes filter: S3's
  filters on tags, Replication Time Control, replication metrics and the other elements
  listed under [The S3 API](#the-s3-api) answer `501`, and there is no Batch
  Replication. Replication is in no published release yet.
- **Quotas are on buckets, in bytes, and nothing else.** There is no quota by user, by
  tenant or by number of objects, no soft quota, and no limit on the number of buckets;
  a bucket has no quota until one is set, and the quotas together may promise more than
  the drives hold.
- **The policy builder writes only what the policy language has.** A bucket policy's
  principals are users and anyone - no groups or roles; `NotAction`, `NotResource` and
  `NotPrincipal` are refused, in the builder and in the JSON alike; there is no
  simulation at another time than the node's. The warning before a bucket is opened to
  anyone quotes each statement's Resource as written: the node's analysis says which
  actions a statement opens, not which keys. The builder and "Who may do this?" are in
  no published release yet, and have been tested in a simulated browser only.
- **Growing a cluster.** The voters and the storage of a cluster are fixed when it
  forms: storage nodes and drives cannot be added afterwards, and a node cannot be given
  a vote later. Gateways can join.
- **Single sign-on.** Console users are local accounts. The port for another identity
  provider exists (`ports.IdentityProvider`); no OIDC adapter does.
- **Parts of S3:** ACLs, conditional writes (`If-None-Match` on a PUT), the parts of
  replication named above, inventory, analytics, website hosting, storage classes with a parity of their own; of
  notifications, Lambda and EventBridge destinations and event names such as
  `s3:ObjectCreated:Copy`, `s3:ObjectRestore:*` or `s3:ObjectTagging:*` (a copy is told
  as a put; a change of tags, retention or a legal hold is not told). What is not
  implemented answers `501 NotImplemented`.
- **Metadata is not encrypted at rest.** Object data is; bucket names, object keys,
  sizes, user metadata, the tags of buckets and objects, tag rules, the catalogue's
  descriptions, sub-buckets' policies and what each bucket holds and its quota are in
  the metadata store in the clear
  ([threat model](docs/threat-model.md)).
- **No rotation of the local KMS key**, and no revocation list for node certificates.
- **A rolling upgrade is tested with the binaries themselves for two steps only**: from
  entry schema 3 to 4 and from 4 to 5 (`make test-rolling`). Older states are tested by
  opening fixtures that the older binaries wrote.
- **What `deploy/deploy.sh` does not do**
  ([Deploying with deploy/deploy.sh](#deploying-with-deploydeploysh)):
  - no PowerShell entry point: on Windows it runs in WSL 2 only;
  - no command to renew or reissue the development certificates: `cactusfs dev-init`
    keeps no CA key, so they expire after `TLS_VALIDITY` (a year by default) and their
    names are fixed when they are made - `--status` shows the expiry, and a deployment
    that outlives them needs your own PKI (`TLS_CERT_FILE`, `TLS_KEY_FILE`,
    `TLS_CA_FILE`);
  - no Helm release on Kubernetes: no `helm list`, `helm history` or `helm rollback`
    for what it made (going back is `--install` with the earlier image), and a
    namespace that holds a Helm release of the same name is refused;
  - a cluster on Rancher Desktop's containerd engine is refused (its Kubernetes,
    `RANCHER_KUBERNETES=yes`, takes both modes);
  - `VAULT_CA_FILE` is refused on the Docker targets, standalone and cluster: the
    config volume does not carry it yet, so Vault's certificate must be one the
    image trusts;
  - `K8S_STORAGE=hostpath` names the directories on the node and does not make them;
  - the image reaches a Kubernetes cluster of more than one node only through a
    registry (`K8S_REGISTRY`); a compose cluster on an engine of another machine is
    reached only through `CE_HOST` (another context or `DOCKER_HOST` engine is
    refused there, as compose bind-mounts files of `STATE_DIR`);
  - it installs Docker CE (`CE_INSTALL_DOCKER=yes`) on Debian, Ubuntu, RHEL, Rocky
    Linux, AlmaLinux and CentOS Stream only;
  - not yet run on a Kubernetes other than k3s (Rancher Desktop's is k3s): that path is
    proven against recording fakes ([Targets](#targets));
  - the image tags it built, loaded or imported stay until an upgrade on a Docker
    engine replaces them, or a purge ([Image tags](#image-tags)); on Kubernetes only
    a purge removes them; a tag the script did not make is never removed, and on k3s
    an image imported by `K3S_IMPORT`'s command is left;
  - S3 published beyond the loopback (a non-loopback `BIND`, a compose cluster, a
    NodePort or a LoadBalancer) and reached by an address or name fails TLS
    verification unless `TLS_HOSTS` listed it at the first install: the install warns
    when it is empty, and the development certificate cannot be given names later.
    The binaries of `make image-binaries` are in no published release: run
    `make image-binaries` first for the default `IMAGE_SOURCE=binaries`.

## Build from source

You need **one** of the following. Nothing else has to be installed.

### With Go alone

Requires the Go version named in `go.mod`; Go 1.21 or later downloads it by itself.

```sh
git clone https://github.com/albievan/cactusfs.git
cd cactusfs
go build ./cmd/cactusfs
./cactusfs version
```

This builds on Linux, macOS and Windows, for amd64 and arm64, without cgo. A binary
built this way serves the APIs and Swagger UI; in place of the console it serves a page
that says the console was not built in.

### With a container engine alone

Requires Docker or Podman.

```sh
git clone https://github.com/albievan/cactusfs.git
cd cactusfs
docker build -t cactusfs .        # or: podman build -t cactusfs .
docker run --rm cactusfs version
```

The image is distroless, runs as a non-root user (UID 65532) and has no shell. It
carries the web console, as does the binary of `make build`.

## Quick start: one node

A node needs a drive directory, a key management service and TLS. For a first look,
`--dev-plaintext` replaces TLS with plain HTTP, and for that reason only listens on a
loopback address (`127.0.0.1`): a node started this way cannot be reached from another
machine, and must never hold real data.

```sh
export CACTUSFS_KMS_PASSPHRASE='choose a long passphrase'     # at least 12 characters
./cactusfs server --dev-plaintext --data ./.data --kms local
```

`--kms local` keeps the key that wraps every data key in a sealed file,
`./.data/meta/kms-local.json`, which the passphrase opens. The passphrase is read from
the environment (or from a file, `--kms-passphrase-file`), never from the command line.
**Back up that file and the passphrase. Without both, nothing stored can be decrypted.**

On its first start the node prints, **once** and on standard error, the root credentials
it generated - an API key. Store them: they cannot be shown again. To choose them
yourself, set `CACTUSFS_ROOT_ACCESS_KEY` and `CACTUSFS_ROOT_SECRET_KEY` before the first
start (both, or neither) - or name files that hold them, `--root-access-key-file` and
`--root-secret-key-file` (`CACTUSFS_ROOT_ACCESS_KEY_FILE`, `CACTUSFS_ROOT_SECRET_KEY_FILE`),
which keeps them out of the environment of the process.

```sh
export AWS_ACCESS_KEY_ID=…  AWS_SECRET_ACCESS_KEY=…  AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://127.0.0.1:9000 s3 mb s3://photos
aws --endpoint-url http://127.0.0.1:9000 s3 cp cactus.jpg s3://photos/
aws --endpoint-url http://127.0.0.1:9000 s3 ls s3://photos/
```

The management API is at `http://127.0.0.1:9001/api/v1`, its documentation at
`http://127.0.0.1:9001/docs`, and the console - in a binary that has it - at
`http://127.0.0.1:9001/`.

## First sign-in

A new deployment has two kinds of credentials, and they are not the same thing:

- **Console: user `admin`, password `admin`**, with the role `cluster-admin`
  ([ADR-0014](docs/adr/0014-default-console-user.md)). It signs in to the console only;
  it has no API key.

  **Delete that user, or change its password, before anybody else can reach the admin
  port** (console → Users). The node says so on standard error and in its log at the
  first start. The user is made once: after you change or delete it, it never comes
  back, and `admin` cannot be set as a password again. Deleting is the surer of the two:
  it ends the user's sessions and removes its API keys. A changed password ends every
  session of the user too, on every node ([ADR-0027](docs/adr/0027-session-lock.md);
  during a rolling upgrade a node of the release before does not, and keeps a session
  to its timeout), but leaves in place whatever was made through them - API keys,
  users, policies. The audit trail shows whether anybody signed in first. The compose cluster publishes the admin port on `127.0.0.1` only for this
  reason, and the chart opens it to nobody until you say who.
- **API: the root key.** Generated and printed once at the first start, or the pair you
  set in `CACTUSFS_ROOT_ACCESS_KEY` and `CACTUSFS_ROOT_SECRET_KEY`, or in the files
  `CACTUSFS_ROOT_ACCESS_KEY_FILE` and `CACTUSFS_ROOT_SECRET_KEY_FILE` name (with the
  chart: the Secret named in `secrets.root`, keys `access-key` and `secret-key`, mounted
  as files; with compose: the files `scripts/compose-secrets.sh` writes). It works for S3,
  for the management API and in Swagger UI. **There is no default API key.**

## Addresses

Everything is served over TLS on two ports of every gateway node. `<host>` is the
machine, or the name your certificate carries.

| What | URL | Sign in with |
|---|---|---|
| S3 API | `https://<host>:9000` | an API key, signed with SigV4 (region `us-east-1` unless set) |
| Console | `https://<host>:9001/` | a console user: **`admin` / `admin`** on a new deployment |
| Swagger UI | `https://<host>:9001/docs` | **Authorize** with `<access key ID>:<secret key>` |
| Management API | `https://<host>:9001/api/v1` | `Authorization: Bearer <access key ID>:<secret key>`, or a console session |
| API contract | `https://<host>:9001/api/v1/openapi.yaml`, `/api/v1/openapi.json` | nothing |
| Liveness, readiness | `https://<host>:9001/healthz`, `/readyz` | nothing; they say `ok` or `unavailable` and no more |
| Metrics | `https://<host>:9001/metrics` | `Authorization: Bearer <access key ID>:<secret key>` of a key allowed `admin:GetMetrics` |

Nodes talk to each other on two more ports, over mutual TLS and to nobody else: node
RPC on `9002` and Raft on `9003`. All four ports are settings.

Where the two client ports are, by way of running it:

| Deployment | S3 | Console, Swagger UI, management API |
|---|---|---|
| The binary (`cactusfs server`) | `https://localhost:9000` | `https://localhost:9001` |
| The binary with `--dev-plaintext` | `http://127.0.0.1:9000` | `http://127.0.0.1:9001` |
| Compose cluster (`make up`) | `https://localhost:9000`, `:9010`, `:9020`, `:9030` (nodes 1 to 4) | `https://localhost:9001`, `:9011`, `:9021`, `:9031`, on this machine only |
| Helm chart, inside the cluster | `https://<release>.<namespace>.svc:9000` | `https://<release>-admin.<namespace>.svc:9001` |
| Helm chart, published as NodePorts | `https://<machine>:<service.nodePort>` (`service.type=NodePort`) | `https://<machine>:<adminService.nodePort>` (`adminService.type=NodePort`) |
| Made by `deploy/deploy.sh` | what `--install` and `--status` print ([Reaching the deployment](#reaching-the-deployment)) | the same; the admin port on `127.0.0.1`, or a ClusterIP, unless you open it |

The certificate authority to trust is your own; for the development setups it is
`s3-ca.pem`, which `cactusfs dev-init` writes.

## Running a node

Outside development a node serves TLS and refuses to start without a certificate:

```sh
./cactusfs server --data /mnt/drive1,/mnt/drive2,/mnt/drive3,/mnt/drive4 --kms local \
  --tls-cert server.crt --tls-key server.key
curl --cacert ca.crt https://localhost:9001/readyz      # {"status":"ok"}
```

- **Drives.** Give it four or more drives and objects are erasure coded: six drives
  store every object as three data and three parity shards and survive the loss of any
  three. With fewer than four drives there is no parity, and the node says so. All
  drives must be present for the node to start.
- **Replacing a drive.** To replace a drive that died, stop the node, put an empty
  drive in its place and start once with `--allow-drive-replacement`. The node will not
  do this on its own, because an empty directory is also what a missing mount looks
  like, and a replacement is permanent: the old drive is refused afterwards. The new
  drive is refilled in the background. One drive at a time, and only with four or more
  drives - without parity nothing can be rebuilt.
- **The metadata directory.** Keep `--meta-dir` (default `<first drive>/meta`) with the
  drives it describes. A node started with drives that hold data and a metadata
  directory that knows nothing of them refuses to start rather than treat the data as
  garbage.
- **The KMS key.** With `--kms local`, back up the key file
  (`<metadata directory>/kms-local.json` by default) and its passphrase. Without both,
  nothing stored can be decrypted. For keys that never touch the node's disks, see
  [Keys in Vault](#keys-in-vault).
- **Certificates** are read again every `--tls-reload-interval` (one minute): replace
  the files and new connections are served the new certificate, without a restart. See
  [Operations](#operations).

In a container, mount the drives and the certificate and run hardened:

```sh
docker run --rm --read-only --cap-drop ALL -p 9000:9000 -p 127.0.0.1:9001:9001 \
  -v cactus-data:/var/lib/cactusfs/data1 -v "$PWD/certs:/certs:ro" \
  -e CACTUSFS_DATA=/var/lib/cactusfs/data1 -e CACTUSFS_KMS=local -e CACTUSFS_KMS_PASSPHRASE \
  -e CACTUSFS_TLS_CERT=/certs/server.crt -e CACTUSFS_TLS_KEY=/certs/server.key \
  cactusfs
```

The admin port is published on this machine only, for the reason given under
[First sign-in](#first-sign-in). The image runs as UID 65532. Its directories under
`/var/lib/cactusfs` (`data1` to `data4`, `meta`, `config`) belong to that user, so a
named volume mounted on one of them is writable; a volume mounted anywhere else, or a
bind mount, must be made writable for UID 65532 by you.

## Settings

Every setting is a flag with a matching `CACTUSFS_*` variable (`--s3-addr` is
`CACTUSFS_S3_ADDR`); the flag wins over the variable, and the variable over the default. A
variable that is set to nothing counts as not set. Secrets are the exception: they are
read from the environment or from a file, never from the command line, which every user
of the machine can see.

The settings are declared once, in the binary. `./cactusfs server --help` lists them, and
so does the table below, which the binary prints (`cactusfs config --format markdown`;
`make settings-docs` writes it here, and a test fails when the two differ). A running
deployment shows the settings of every node - where each value came from, the drives and
what they are mounted on, the certificates served - under **Settings** in the console and
at `GET /api/v1/settings`, to a caller allowed `admin:GetSettings`; a secret is shown
there as "set" or "not set" and nothing else.

<!-- settings:begin -->

**Listeners: where clients reach the node**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--s3-addr` | `CACTUSFS_S3_ADDR` | `:9000` | listen address of the S3 API |
| `--admin-addr` | `CACTUSFS_ADMIN_ADDR` | `:9001` | listen address of the admin listener: probes, `/metrics`, the management API, Swagger UI and the console |
| `--s3-domain` | `CACTUSFS_S3_DOMAIN` | — | domain for virtual-hosted-style requests, \<bucket>.\<domain>. The same on every node |
| `--region` | `CACTUSFS_REGION` | `us-east-1` | region name reported to S3 clients; default us-east-1. The same on every node |
| `--drain-delay` | `CACTUSFS_DRAIN_DELAY` | `0s` | on shutdown, keep serving this long with /readyz answering 503 before closing the listeners |
| `--shutdown-timeout` | `CACTUSFS_SHUTDOWN_TIMEOUT` | `30s` | time allowed for in-flight requests to finish once the listeners have closed |

**Storage: drives and the metadata directory**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--data` | `CACTUSFS_DATA` | required on a node that stores | drive directory; repeat the flag or separate directories with commas |
| `--drive-labels` | `CACTUSFS_DRIVE_LABELS` | — | what the deployment calls the volume behind each drive directory - a compose volume, a claim - as labels separated by commas, one for each --data entry in the same order, or none; shown with the node's settings and used for nothing else |
| `--meta-dir` | `CACTUSFS_META_DIR` | `<first data directory>/meta` | metadata directory; default \<first data directory>/meta |
| `--allow-drive-replacement` | `CACTUSFS_ALLOW_DRIVE_REPLACEMENT` | `false` | confirm that one empty drive replaces one missing drive at this start; permanent: the missing drive is refused afterwards |

**Cluster: the node among its peers**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--node-id` | `CACTUSFS_NODE_ID` | generated on a node on its own | name of this node: lowercase letters, digits and hyphens; required in a cluster, where it must be the name in the node certificate |
| `--roles` | `CACTUSFS_ROLES` | all | roles of this node: gateway, meta, storage, console; default all. For members of a cluster only: a node on its own has every role |
| `--peers` | `CACTUSFS_PEERS` | — | every node of the cluster, this one included, as id=host:port of its node RPC, separated by commas; makes the node a member of a cluster. The same on every node |
| `--rpc-addr` | `CACTUSFS_RPC_ADDR` | `:9002` | listen address of node RPC |
| `--raft-addr` | `CACTUSFS_RAFT_ADDR` | `:9003` | listen address of the Raft transport |
| `--advertise-rpc` | `CACTUSFS_ADVERTISE_RPC` | the node's entry in `--peers` | host:port at which other nodes reach this node's RPC; default its entry in --peers |
| `--advertise-raft` | `CACTUSFS_ADVERTISE_RAFT` | the host of `--advertise-rpc`, the port of `--raft-addr` | host:port at which other nodes reach this node's Raft transport; default the host of --advertise-rpc with the port of --raft-addr |
| `--topology-region` | `CACTUSFS_TOPOLOGY_REGION` | — | region this node is in, for placement |
| `--topology-zone` | `CACTUSFS_TOPOLOGY_ZONE` | — | zone this node is in, for placement |
| `--topology-rack` | `CACTUSFS_TOPOLOGY_RACK` | — | rack this node is in, for placement |
| `--topology-host` | `CACTUSFS_TOPOLOGY_HOST` | — | host this node runs on, for placement |

**TLS: certificates of the listeners and of the node**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--tls-cert` | `CACTUSFS_TLS_CERT` | required, unless `--dev-plaintext` | path of the PEM certificate chain of the S3 and admin listeners |
| `--tls-key` | `CACTUSFS_TLS_KEY` | required, unless `--dev-plaintext` | path of the PEM private key of that certificate. The file holds a secret: its path is not shown with the node's settings |
| `--tls-reload-interval` | `CACTUSFS_TLS_RELOAD_INTERVAL` | `1m` | how often the certificate and key files, the node's too, are read again (at least `5s`; `0` never reads them again). A new pair that is valid is used for new connections without a restart; one that is not is refused and logged, and the old one stays (`cactusfs_tls_reload_pending` in `/metrics` is 1 while it does). The node CA is read at start only: a new CA needs a restart. Rotation is not revocation: a replaced certificate and its key remain accepted by peers until that certificate expires; for a compromised key, replace the node CA (a node that leaves the cluster is revoked, ADR-0030, but only the node itself can ask to leave). The files must be regular files (not pipes) |
| `--dev-plaintext` | `CACTUSFS_DEV_PLAINTEXT` | `false` | DEVELOPMENT ONLY: serve HTTP without TLS, on a loopback IP address only |
| `--node-tls-cert` | `CACTUSFS_NODE_TLS_CERT` | required in a cluster | path of the node certificate for mutual TLS between nodes |
| `--node-tls-key` | `CACTUSFS_NODE_TLS_KEY` | required in a cluster | path of the node certificate's private key. The file holds a secret: its path is not shown with the node's settings |
| `--node-tls-ca` | `CACTUSFS_NODE_TLS_CA` | required in a cluster | path of the CA certificate that issues the cluster's node certificates, and nothing else |

**KMS: the key that wraps data keys**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--kms` | `CACTUSFS_KMS` | required | key management service: `vault` for HashiCorp Vault Transit, `local` for a sealed local key file. There is no default and no unencrypted mode. The same on every node |
| `--kms-key-file` | `CACTUSFS_KMS_KEY_FILE` | `<metadata directory>/kms-local.json`; required in a cluster | path of the local KMS key file; default \<metadata directory>/kms-local.json. The file holds a secret: its path is not shown with the node's settings |
| — (no flag: a secret) | `CACTUSFS_KMS_PASSPHRASE` | required with the local KMS, or `--kms-passphrase-file` | passphrase that opens the local KMS key file. A secret: from the environment only, never shown |
| `--kms-passphrase-file` | `CACTUSFS_KMS_PASSPHRASE_FILE` | — | path of a file holding the local KMS passphrase, instead of $CACTUSFS_KMS_PASSPHRASE. The file holds a secret: its path is not shown with the node's settings |
| `--vault-addr` | `CACTUSFS_VAULT_ADDR` | required with `--kms vault` | URL of Vault, https://host:port. The same on every node |
| `--vault-transit-mount` | `CACTUSFS_VAULT_TRANSIT_MOUNT` | `transit` | path of Vault's Transit engine; default transit. The same on every node |
| `--vault-key` | `CACTUSFS_VAULT_KEY` | required with `--kms vault` | name of the Transit key, of an AEAD type such as aes256-gcm96. The same on every node |
| `--vault-token-file` | `CACTUSFS_VAULT_TOKEN_FILE` | required with `--kms vault` | path of a file holding the Vault token; read at every use, so it can be renewed in place. The file holds a secret: its path is not shown with the node's settings |
| `--vault-ca-cert` | `CACTUSFS_VAULT_CA_CERT` | the system's CAs | path of the CA certificate that issued Vault's certificate; default the system's CAs |
| `--vault-server-name` | `CACTUSFS_VAULT_SERVER_NAME` | — | name expected in Vault's certificate, when Vault is reached under another |
| `--vault-namespace` | `CACTUSFS_VAULT_NAMESPACE` | — | Vault Enterprise namespace. The same on every node |

**Bootstrap: the root credentials of a new deployment**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| — (no flag: a secret) | `CACTUSFS_ROOT_ACCESS_KEY` | generated, and shown once, by a new deployment | access key ID of the root credentials a new deployment begins with; set it together with $CACTUSFS_ROOT_SECRET_KEY. A secret: from the environment only, never shown |
| — (no flag: a secret) | `CACTUSFS_ROOT_SECRET_KEY` | generated, and shown once, by a new deployment | secret key of the root credentials a new deployment begins with; set it together with $CACTUSFS_ROOT_ACCESS_KEY. A secret: from the environment only, never shown |
| `--root-access-key-file` | `CACTUSFS_ROOT_ACCESS_KEY_FILE` | — | path of a file holding the access key ID of the root credentials, instead of $CACTUSFS_ROOT_ACCESS_KEY. The file holds a secret: its path is not shown with the node's settings |
| `--root-secret-key-file` | `CACTUSFS_ROOT_SECRET_KEY_FILE` | — | path of a file holding the secret key of the root credentials, instead of $CACTUSFS_ROOT_SECRET_KEY. The file holds a secret: its path is not shown with the node's settings |

**Console sessions: how long one lasts, and what becomes of it while it uploads**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--session-lifetime` | `CACTUSFS_SESSION_LIFETIME` | `30m` | how long a console session lasts, from sign-in or from its last unlock, before it reaches its timeout (`10s` to `12h`). Nothing extends it: at the timeout the user signs in again, or - while objects are being uploaded with the session - it is locked until the user's password unlocks it. It must be the same on every node: any gateway may be asked what a session is. The same on every node |
| `--session-upload-grace` | `CACTUSFS_SESSION_UPLOAD_GRACE` | `1m30s` | how long after its last upload a console session that is locked ends (`5s` to `10m`). While a session uploads, each node it uploads through writes one entry to the metadata log in a third of this time. It must be the same on every node. The same on every node |
| `--session-lock-max` | `CACTUSFS_SESSION_LOCK_MAX` | `12h` | the longest a console session that reached its timeout while it uploads stays locked - able to store objects and nothing else - counted from the timeout, whatever is still being uploaded (at most `24h`); `0`: a session never locks, and ends at its timeout. It must be the same on every node: a gateway with a smaller value takes for ended what another takes for locked, and the node that leads the metadata group removes session records by its own. The same on every node |

**Previews: how much of an object the console is sent to show**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--preview-max-size` | `CACTUSFS_PREVIEW_MAX_SIZE` | `32` | the largest object the console is sent to preview, in MiB (`1` to `256`); a larger one is refused (413) before any of it is read, and can be downloaded. Of text, CSV and JSON the first MiB is sent whatever their size. Every preview is a read of up to this much from the drives; a caller has at most 2 running at once and a node 16. A console may ask any gateway. The same on every node |

**Delivery: outbound connections to notification targets and other sites**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--egress-allow` | `CACTUSFS_EGRESS_ALLOW` | none | ranges (CIDRs, separated by commas, such as `127.0.0.1/32`) where the update source (`update-source`, and every address its downloads are redirected to), a notification target or another site may be at an address that is otherwise never connected to: loopback, link-local, and the IPv6 ranges that stand for IPv4 addresses by a gateway's rule (`::/96`, `64:ff9b:1::/48`, `2002::/16`, `::ffff:0:0:0/96`). Private ranges need no entry. A range of every address (`0.0.0.0/0`, `::/0`) and one that could admit nothing (`0.0.0.0/8`, multicast) are refused. The addresses of the metadata services of clouds (169.254.169.254, `fd00:ec2::254`, `fd20:ce::254`, 100.100.100.200) are never connected to, whatever this holds; a range that is allowed is open with everything else in it, so allow the one address a receiver has. The address a name resolves to is judged at every connect. It is read by the update source (ADR-0029), by notification targets, the export of the audit trail included (ADR-0025), and by replication to other sites (ADR-0026). The same on every node |
| `--delivery-timeout` | `CACTUSFS_DELIVERY_TIMEOUT` | `30s` | how long a notification target or another site has to answer one request, once it is sent, and to make progress reading or writing (`1s` to `5m`); one that does not is tried again later. Connecting and the TLS handshake have 5 seconds each besides. It is read by notification targets, the export of the audit trail included (ADR-0025), and by replication to other sites (ADR-0026), where it bounds each request of a version, not the whole version: a version may take as long as its size and replication-rate-mib need. The same on every node |
| `--replication-workers` | `CACTUSFS_REPLICATION_WORKERS` | `4` | how many versions are replicated to other sites at once (`1` to `32`), by the node that leads the metadata group; two versions of one object, or a version and a later change of it, are never sent at once (ADR-0026). The same on every node |
| `--replication-rate-mib` | `CACTUSFS_REPLICATION_RATE_MIB` | `0` | limit on the bytes replication sends to other sites, in MiB per second averaged over 10 seconds (at most `1048576`); `0` is no limit. It is held to over every 10 seconds, with no burst above it: what all the versions being sent read together, by the node that leads (ADR-0026). An attempt at a version is given a day, or the time its size takes at 64 KiB/s when that is longer - or at this rate shared by `replication-workers` to each of the 256 targets a deployment can have, when that is slower still; a transfer that stops moving is ended by `delivery-timeout`. A version that cannot be sent is tried again - one of parts from the parts that arrived, one sent in one request from its start - and holds its target meanwhile. The same on every node |

**Maintenance: background work**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--scrub-interval` | `CACTUSFS_SCRUB_INTERVAL` | `24h` | pause between two passes of the scrubber, which verifies and heals every object; 0 disables it. The same on every node |
| `--scrub-rate-mib` | `CACTUSFS_SCRUB_RATE_MIB` | `64` | limit on how fast the scrubber reads, in MiB per second; 0 is no limit. The same on every node |
| `--lifecycle-interval` | `CACTUSFS_LIFECYCLE_INTERVAL` | `1h` | pause between two passes of the worker that carries out the lifecycle rules of buckets; 0 disables it. The same on every node |
| `--gc-interval` | `CACTUSFS_GC_INTERVAL` | `1h` | pause between two passes of the garbage collector for unreferenced shards; 0 disables it |
| `--gc-grace` | `CACTUSFS_GC_GRACE` | `24h` | minimum age of an unreferenced shard before it is removed (at least `1h`). The same on every node |
| `--access-flush-interval` | `CACTUSFS_ACCESS_FLUSH_INTERVAL` | `30s` | how often the downloads this node counted are recorded with the objects, at least 5s; a node that crashes loses up to this much of the count |

**Updates: checking for releases, and installing one from the console**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--update-source` | `CACTUSFS_UPDATE_SOURCE` | none: nothing is asked | address of the releases JSON - `https://api.github.com/repos/albievan/cactusfs/releases`, or a mirror of it that answers as it does - which the node asks what releases exist. `https` only, with no user information, query or fragment. Nothing is asked of anyone while it is empty, the default. Who answers it decides what is offered, never what is installed: a release is installed only when its signed lists verify against the trusted root, for its version. The same on every node |
| `--update-check-interval` | `CACTUSFS_UPDATE_CHECK_INTERVAL` | `24h` | how often the node asks the update source by itself, give or take a tenth, the first time within 5 minutes of its start (at least `1m`; `0` only when asked to from the console). A node asks at most once a minute, however it is asked. The same on every node |
| `--update-channel` | `CACTUSFS_UPDATE_CHANNEL` | `stable` | which releases are offered: stable, or prerelease for pre-releases too. The same on every node |
| `--update-ca` | `CACTUSFS_UPDATE_CA` | the system's CAs | path of a PEM file of the CA certificates the update source's certificate is verified against; default the system's CAs |
| `--update-trusted-root` | `CACTUSFS_UPDATE_TRUSTED_ROOT` | the root embedded in the binary | path of a Sigstore `trusted_root.json` that the signatures of releases are verified against, instead of the one embedded in the binary: for a node that has been offline across a rotation of Sigstore's keys. A file that does not parse stops the node at start |
| `--upgrade-mode` | `CACTUSFS_UPGRADE_MODE` | `off` | how a release is installed from the console: `off` installs nothing and shows the commands to run instead; `binary` replaces the binary of a node on its own and restarts it (needs `--update-source`; refused by the binary of the container image); `kubernetes` moves the nodes of a chart to the release's image one at a time, this node last (set by the chart's `upgrade.fromConsole.enabled`, which mounts the token it needs; refused without it) |

**Observability: log, audit export and traces**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--log-level` | `CACTUSFS_LOG_LEVEL` | `info` | minimum log level: debug, info, warn or error |
| `--audit-export` | `CACTUSFS_AUDIT_EXPORT` | `false` | write every audit record to standard output as a JSON line with "stream":"audit" |
| `--otlp-endpoint` | `CACTUSFS_OTLP_ENDPOINT` | — | URL of an OTLP/HTTP trace collector, `https` unless the host is a loopback IP address; traces are not exported when empty. A URL with userinfo, a query or a fragment is refused: a credential does not belong in it, and a credential for the collector is not a setting of CactusFS |

<!-- settings:end -->

### An environment file

[`deploy/cactusfs.env.example`](deploy/cactusfs.env.example) lists every variable with
its meaning and its default, each commented out (`cactusfs config --format env` prints
it). Copy it, take the `#` off what you set, and hand it to whatever starts the node. **The
binary itself reads no file**: only its environment and its flags, so that there is one
order of precedence wherever it runs.

```sh
cp deploy/cactusfs.env.example /etc/cactusfs/cactusfs.env     # outside the repository

docker run --env-file /etc/cactusfs/cactusfs.env ... cactusfs      # a container (or podman run)

# a systemd unit:
#   [Service]
#   EnvironmentFile=/etc/cactusfs/cactusfs.env
#   ExecStart=/usr/local/bin/cactusfs server

# a shell (sh, bash, zsh): export what the file sets, then start the node
while IFS= read -r line || [ -n "$line" ]; do
  case $line in CACTUSFS_*=*) export "$line" ;; esac
done </etc/cactusfs/cactusfs.env
cactusfs server
```

```powershell
# PowerShell: set what the file sets, for this session, then start the node
Get-Content cactusfs.env | Where-Object { $_ -match '^[A-Z0-9_]+=' } | ForEach-Object { $k, $v = $_ -split '=', 2; Set-Item "Env:$k" $v }
.\cactusfs.exe server
```

A line of the file is `NAME=value`. Both loops take a line that begins with a variable's
name and leave every other alone - comments, empty lines - and, like
`docker run --env-file`, take the value as it stands: everything after the first `=`, so
a value may hold `=`, spaces, `$` or `#`. What none of them does: take quotes off a value
(a quote becomes part of it), read a value over several lines, drop a comment after a
value, or accept `export NAME=value` or spaces before the name. Write values without
quotes and the file reads the same everywhere. A systemd `EnvironmentFile=` does take
quotes off, and so does a shell that sources the file (`set -a; . file; set +a`), which
besides runs what follows a space in a value as a command and expands `$`: use the loop.

Keep secrets out of that file: `CACTUSFS_KMS_PASSPHRASE` and the root credentials come
from a secret store (a Kubernetes Secret, the secrets of the container engine, the
service manager), and the files that hold a key or a token (`--tls-key`, `--node-tls-key`,
`--kms-key-file`, `--kms-passphrase-file`, `--root-access-key-file`,
`--root-secret-key-file`, `--vault-token-file`) stay outside the
repository. A file named `.env`, `.env.<anything>` or `<anything>.env` - `cactusfs.env`,
say - is ignored by git in every directory and is left out of the build context of the
image (`.gitignore`, `.dockerignore`); `*.example` files are the exception, and hold no
value of a secret. That keeps a copy out of a commit and out of an image; it does not
make the checkout the place for one.

The URL of the trace collector (`--otlp-endpoint`) holds no credential: one with
userinfo, a query or a fragment is refused at start, and the refusal names the URL
without them. CactusFS has no setting that gives the collector a credential; put a
collector that needs none on the nodes' network, or one beside each node that forwards.

## Keys in Vault

With `--kms vault` the KMS key never leaves HashiCorp Vault: the node asks Vault's Transit
engine to wrap and unwrap the data keys of objects and secrets. It is what a cluster
should use; the sealed local key file is for a node on its own and for development.

```sh
vault secrets enable transit
vault write -f transit/keys/cactusfs type=aes256-gcm96      # an AEAD type is required
vault policy write cactusfs - <<'POLICY'
path "transit/encrypt/cactusfs" { capabilities = ["update"] }
path "transit/decrypt/cactusfs" { capabilities = ["update"] }
POLICY
vault token create -policy=cactusfs -no-default-policy -field=token > /run/secrets/vault-token

cactusfs server --data /mnt/d1 --kms vault --vault-addr https://vault.example.test:8200 \
  --vault-key cactusfs --vault-token-file /run/secrets/vault-token --tls-cert s3.pem --tls-key s3.key
```

Vault must be reached over HTTPS. The token file is read at every use, so an agent can
renew it in place; the node does not renew it itself. If Vault cannot be reached the
node does not start, and has no fallback.

## A cluster

The same binary forms a cluster when it is started with `--peers`: at least four nodes
that store, of which three or five also vote on the metadata (`meta` role). Nodes talk to
each other over mutual TLS 1.3 only, with certificates of a CA that issues node
certificates and nothing else; each certificate carries the node's name as its first DNS
name.

### A development cluster with compose

Four nodes on one machine, with Docker or rootless Podman
([`deploy/compose/compose.yaml`](deploy/compose/compose.yaml)). It needs `make` and a
container engine with its compose plugin; the image is built from this tree. `deploy/deploy.sh`
with `DEPLOY_MODE=cluster` runs this same file, and makes and keeps its secrets for you
([Deploying with deploy/deploy.sh](#deploying-with-deploydeploysh)).

```sh
export CACTUSFS_KMS_PASSPHRASE="$(openssl rand -hex 24)"   # keep it: it opens the cluster's key
export CACTUSFS_ROOT_ACCESS_KEY=…  CACTUSFS_ROOT_SECRET_KEY=…   # or find a generated pair in the log, below
make up                                                     # or: make up ENGINE=podman
docker compose -f deploy/compose/compose.yaml cp node1:/var/lib/cactusfs/config/s3-ca.pem .
aws --endpoint-url https://localhost:9000 --ca-bundle s3-ca.pem s3 mb s3://photos
aws --endpoint-url https://localhost:9030 --ca-bundle s3-ca.pem s3 ls        # another node, same answer
make down                                                   # stops it; the volumes, and so the data, stay
```

The passphrase must be the same at every start. No secret enters the environment of a
container, where the engine's `inspect` would show it: `make up` first runs
`scripts/compose-secrets.sh`, which writes the exported passphrase and root credentials
into files of `deploy/compose/secrets/` (a directory of yours alone, ignored by git and
left out of the build context), and compose hands those to the nodes as secrets, at
`/run/secrets/<name>`. Without `make`, run the script before `docker compose up` or
`podman compose up`. The files hold the secrets in plain text on this machine: this is
a development cluster. Without root credentials one node generates a pair and prints it
once, into the container log:
`docker compose -f deploy/compose/compose.yaml logs | grep -A5 'generated root credentials'`.

Published on this machine: S3 on `9000`, `9010`, `9020` and `9030` (nodes 1 to 4, on
every address of the machine), and the admin port of each node on `9001`, `9011`, `9021`
and `9031`, **on `127.0.0.1` only**. Ports 9002 and 9003 are not published. The console
is at `https://localhost:9001/`, with `admin` / `admin` until you change it.

What makes it a development cluster: a one-off `init` service writes throw-away
certificates and **one** sealed KMS key file that all four nodes share
(`cactusfs dev-init`), and all four run on one machine - it survives the loss of a node
process, not of the machine.

What else the development cluster can be told - the log level, the intervals of the
background work, a trace collector, the ports on this machine - is listed in
[`deploy/compose/.env.example`](deploy/compose/.env.example). Copy it to
`deploy/compose/.env` and take the `#` off what you set: `make up` hands the file to
compose when it exists, and starts the cluster the same without one. Compose puts the
values into the environment of the nodes; a variable exported in the shell wins over the
file, and the secrets are exported, not written into it. Each node is told the name of the volume behind each of its drives
(`CACTUSFS_DRIVE_LABELS`: `cactusfs_node1-data1`, ...; with the chart, the claims
`data1-<node>-0`, ...), which the Settings screen shows beside the directory.

Eight drives on four nodes make one erasure set of 4 data and 4 parity shards, two per
node. With any one node down the cluster reads and writes; with two down, objects stay
readable as long as two of the three voters are among the survivors. A node that is cut
off from the others answers nothing rather than something stale. What a node missed
while it was away is rebuilt on it in the background.

### Nodes on separate machines

Give each node its own certificate and the same list of peers:

```sh
cactusfs server --node-id node2 --roles gateway,meta,storage \
  --peers node1=node1.example.test:9002,node2=node2.example.test:9002,node3=node3.example.test:9002,node4=node4.example.test:9002 \
  --node-tls-cert node2.pem --node-tls-key node2.key --node-tls-ca node-ca.pem \
  --tls-cert s3.pem --tls-key s3.key \
  --data /mnt/d1 --data /mnt/d2 --kms local --kms-key-file /etc/cactusfs/kms.json \
  --topology-zone zone-b
```

The cluster forms the first time all its nodes are up; afterwards a node needs only a
majority of the voters to start. Things to know for now:

- **Set the root credentials yourself** (`CACTUSFS_ROOT_ACCESS_KEY`,
  `CACTUSFS_ROOT_SECRET_KEY`) before the first start of a containerised cluster:
  generated ones are printed once on standard error, which a container engine keeps as
  a log.
- **The voters are fixed when the cluster forms.** Gateways can join later; a node
  cannot be given a vote afterwards yet.
- **Every node must open the same KMS key.** In production that is a key in Vault
  (`--kms vault`, [above](#keys-in-vault)). The development cluster shares one sealed
  key file instead (`cactusfs dev-init` writes it, with development certificates).
- **The cluster's storage is fixed when it forms**: storage nodes and drives cannot be
  added afterwards yet.
- **A voter that lost its metadata directory must be removed from the cluster before it
  is started again**, not simply restarted empty.
- **Keep the cluster on one site.** Sites are linked by replication of buckets
  ([Replication between sites](#replication-between-sites)), each site a cluster of its
  own, never by stretching the metadata group across a WAN.

## On Kubernetes

The chart in [`deploy/helm/cactusfs`](deploy/helm/cactusfs/README.md) runs the same
cluster on Kubernetes or k3s: every node is a StatefulSet of one pod with a claim for
its metadata and one for each drive, behind a headless Service for the nodes, a Service
for S3 (`<release>`) and a Service of its own for the admin port (`<release>-admin`).
Pods run as a non-root user with a read-only root filesystem and every capability
dropped, and are admitted under the `restricted` Pod Security Standard. A NetworkPolicy
lets the nodes reach each other and DNS, and nobody reach them until you say who:
`networkPolicy.clientsFrom` for S3, `networkPolicy.adminFrom` for the admin port, each
apart, and `networkPolicy.egressTo` for what the nodes must reach themselves (Vault).
The chart's own README has every value.
`deploy/deploy.sh --docker k3s` or `--docker k8s` renders this chart and applies it
in a namespace of its own, with the Secrets it needs
([Deploying with deploy/deploy.sh](#deploying-with-deploydeploysh)).

Nodes are named `<release>-<pool>-<index>`, with the name of the Helm release exactly
as you give it: `helm install store ...` makes `store-meta-0`, `store-meta-1`,
`store-meta-2` and `store-data-0` with the default pools. That name is the node's ID.

The chart makes no Secret and renders nothing until you name the ones it needs:

| Value | Secret |
|---|---|
| `secrets.root` | root credentials: keys `access-key`, `secret-key` |
| `secrets.tls` | certificate of the S3 and admin listeners (`kubernetes.io/tls`: keys `tls.crt`, `tls.key`) |
| `secrets.nodeCA` | the CA of node certificates: key `ca.crt`. Not for a node on its own |
| `<node ID>-node-tls` | one per node (`kubernetes.io/tls`). The certificate carries exactly one DNS name, the node ID, and no IP address - not the pod's DNS name as well. A pod mounts its own and no other |
| `kms.vault.tokenSecret` | the Vault token, key `token`; with it `kms.vault.addr`, `kms.vault.key` and a `networkPolicy.egressTo` that says where Vault is. `kms.vault.caSecret` (key `ca.crt`) when Vault's CA is not a public one |
| `kms.local.keyFileSecret`, `kms.local.passphraseSecret` | instead of Vault, for one node or for development: keys `kms.json` and `passphrase`; on more than one node only with `dev.localKMS=true` |

**With an image you built.** This works for any build, and is the way to run anything
newer than the last release. Build the image (`make image`, or `docker build`), push it to a registry your
cluster can pull from - or load it into the cluster - and name it:

```sh
helm install cactusfs deploy/helm/cactusfs -n cactusfs -f my-values.yaml \
  --set image.repository=registry.example.test/cactusfs --set image.tag=0.7.0-412
kubectl -n cactusfs get pods        # alive at once, ready when the cluster has formed
```

`image.repository` and `image.tag` name the build to run: an image of `make image` is
tagged `<version>-<build>-<arch>`; push it under `<version>-<build>` (without the
architecture, or as a manifest of both). Pods carry the tag in the label
`app.kubernetes.io/version`. Nothing is pulled from `ghcr.io` when you name an image of
your own.

**With a published release** - `0.7.0` is the only one (see [Releases](#releases)):
without `image.repository` and `image.tag` the chart runs `ghcr.io/albievan/cactusfs` at
the chart's `appVersion`, and the chart itself is at `oci://ghcr.io/albievan/charts/cactusfs`.

`deploy/helm/cactusfs/ci/` has value sets to start from: a cluster with Vault, five
voters, one node, a cluster opened to its clients, and a development cluster on the
disks of one machine (`localVolumes`: the directories
`<basePath>/<node ID>/{meta,data1,...}` must exist and belong to user 65532).

**Upgrading.** `helm upgrade` changes what the nodes are to be and restarts none of
them (`updateStrategy: OnDelete`): every node is a StatefulSet of its own, and nothing
else would keep an upgrade from restarting all of them at once. Bring the change to one
node at a time - nodes that do not vote first, then the voters, the one that leads the
metadata group last - and wait for each to be ready again before the next:

```sh
kubectl -n cactusfs delete pod <node ID>-0
kubectl -n cactusfs wait --for=condition=Ready pod/<node ID>-0 --timeout=5m
```

**Going back.** While nodes of two releases run side by side, none writes a metadata
entry newer than the oldest of them understands, so a node can be taken back to the
release before. When the last node runs a release that raises the entry schema, the
cluster begins to write the new schema by itself, and the first such write ends the way
back: the release before refuses that state rather than misread it. Nothing is lost
then, but no node of the older release starts
([ADR-0016](docs/adr/0016-object-access-and-schema-3.md),
[ADR-0019](docs/adr/0019-bucket-activity-and-schema-4.md),
[ADR-0023](docs/adr/0023-schema-5-usage-outbox-targets.md)). To keep a way back, hold one
node at the release before until you are sure: that is the only way back that can be
walked a node at a time.

**Once every node runs the new release, do not go back a node at a time.** If no write
of the new schema has happened yet, the way back is still there - but only like this:

1. stop **every** node;
2. start all of them on the release before.

A rolling rollback is not safe. A node that is taken back keeps, in the cluster's
records, the schema its newer release registered until it has started and registered
again; until then the cluster goes on writing the new schema, and one such write in that
time - with this release, the leader counting a bucket, or one bucket created by
anybody, through any node - is an entry the node taken back cannot apply. There is no switch that closes the cluster to such
writes meanwhile, so nothing but stopping every node first keeps them out.

If it happens all the same: the node that was taken back stops its metadata at that
entry and guesses nothing. It is not ready (`/readyz`), every request that needs its
metadata fails, and its log and its health say
`metadata state machine stopped at entry <n>: ... metadata entry schema version 5`.
Its data and its state are untouched, and the other nodes go on - but the way back is
over for the whole cluster, and if the nodes taken back are most of those that vote,
the cluster has no metadata quorum and serves nothing until they return. To recover,
start that node on the new release again: it applies the entry it stopped at and
carries on. Nothing is lost.

Which write that is depends on the release you come from:

| From a release of | The way back ends with |
|---|---|
| entry schema 3 | the first upload, the first flushed count of downloads or the first bucket created |
| entry schema 4 (this release raises it to 5) | **within seconds of the last node's upgrade**: the node that leads begins by itself to count what each bucket you already have holds ([Quotas](#quotas)), and that count is a write of schema 5 - as is the first bucket created, a quota set and a target defined. A deployment without a bucket keeps its way back until its first bucket. A node that runs alone has no other node to wait for: it writes schema 5 from its first start on this release, and its first count of a bucket ends the way back |

Two things about the machine, which are not the chart's to do:

- **firewalld.** On a host that runs firewalld, k3s needs its pod and service networks
  in the trusted zone, or pods cannot reach each other and the cluster never forms.
  With the networks k3s uses unless told otherwise:
  `firewall-cmd --permanent --zone=trusted --add-source=10.42.0.0/16 --add-source=10.43.0.0/16 && firewall-cmd --reload`.
- **NodePorts are published past the host firewall.** A Service of type NodePort
  answers on every address of the machine, whatever firewalld says. The admin port has
  a Service of its own so that S3 can be published without it; remember the console
  user `admin` / `admin` before you publish that one.

## Deploying with deploy/deploy.sh

One script, [`deploy/deploy.sh`](deploy/deploy.sh), installs CactusFS on a container
platform, upgrades it, says how it stands, and removes it: Docker Desktop, Docker CE
(on this machine or on a server over SSH), Podman, Rancher Desktop, k3s and Kubernetes.
It is a POSIX `sh` script with its parts in [`deploy/lib/`](deploy/lib); why it is
built the way it is, is in
[ADR-0033](docs/adr/0033-deployment-script.md). It drives the same pieces as the
sections above - one node with four drives, the compose cluster, the Helm chart - and
makes and keeps their secrets for you.

```sh
make image-binaries                                # dist/image: the binaries the image is made of
cp deploy/.env.example deploy/.env                 # KMS=local is the one line it sets
deploy/deploy.sh --docker dd --install --dry-run   # what it would run; changes nothing
deploy/deploy.sh --docker dd --install             # Docker Desktop, one node, four drives
deploy/deploy.sh --status
```

**The settings** are `deploy/.env` (git ignores it, and it never enters the image's build
context), or another file named with `--env FILE`. Copy
[`deploy/.env.example`](deploy/.env.example), which explains every setting with its
default; a key it does not name is refused. The file is read as data, never run: one
`KEY=value` a line, the value exactly as written - a `$(...)`, a backtick or a `;` in it
is text. **It never holds a secret**: a secret is the path of a file that holds it (a
key ending in `_FILE`), or is generated. `KMS` has no default and must be written out:
`KMS=local` (a sealed key file opened by a passphrase) or `KMS=vault`. A flag wins over
the file and the file over the defaults; the environment of your shell is not read, and
every program the script runs gets only `PATH`, `HOME`, `LC_ALL=C`, what it needs and
the variables `TOOL_ENV_PASS` names, so a `DOCKER_HOST`, `DOCKER_CONTEXT`, `KUBECONFIG`
or current context of your shell never decides where it deploys. Settings of the nodes themselves can be added as
`CACTUSFS_*=value` lines (those [`deploy/cactusfs.env.example`](deploy/cactusfs.env.example)
lists, but no secret and none the script sets itself).

**The flags** - `deploy/deploy.sh --help` says the same:

| Flag | What it does |
|---|---|
| `--docker TARGET` | `dd`, `ce`, `podman`, `rancher`, `k3s` or `k8s`. Without it, `DEPLOY_TARGET`; without that, the target of what already runs under `DEPLOY_NAME` (labelled containers, or a labelled namespace); refused when that is none or more than one |
| `--install` | installs; on a deployment that is installed, upgrades it, one node at a time, keeping its data |
| `--status` | how it stands and where it is reached. Exits 0 when it is ready or not installed, 1 when it is installed and not ready |
| `--uninstall` | removes it and keeps its data |
| `--uninstall --purge` | removes it and its data and keys, for good; asks for the deployment's name at the terminal, or takes `--purge-confirm=NAME` |
| `--print-namespace` | Kubernetes: prints the namespace, its Role and the ClusterRole for a cluster's administrator to create beforehand, when your context may not do more than that namespace (ADR-0033 section 7) |
| `--dry-run` | prints the commands that would change something and runs none of them; what it only reads, it still asks |
| `--from-source` | with `--install`: builds the image from this tree with the `Dockerfile` instead of from `dist/image` |
| `--take-image[=NAME]` | with `--install` on Kubernetes, after an upgrade from the console: [below](#upgrade-uninstall-purge) |
| `--adopt`, `--engine-replaced`, `--another-target` | the cases of a lost record, a replaced engine and a second target ([Secrets, and where they live](#secrets-and-where-they-live)) |

Exit status: 0 done, 1 a command failed, 2 refused (nothing was changed), 3 not built
yet. Every deployment has a name, `DEPLOY_NAME` (default `cactusfs`): it names and
labels every container, volume and namespace the script makes, and the script touches
nothing that does not carry the label. Two deployments of two names live side by side.

On this machine it needs a POSIX shell, the command-line tool of the target (`docker`,
`podman` or `nerdctl`; `kubectl`, or `k3s kubectl` on the server) and `curl`;
`ssh` and `ssh-keygen` for a server; for Kubernetes, `docker` or `podman` to run the
helm image the chart is rendered in, or a helm program named with `HELM`; and `openssl`
for `--status` to show when the certificate expires. `make image-binaries` (the default
source of the image, `IMAGE_SOURCE=binaries`) builds in a container engine, as every
`make` target does.

### Targets

Standalone (`DEPLOY_MODE=standalone`, the default) is one node with `DRIVES` drives (4:
2 data + 2 parity); cluster (`DEPLOY_MODE=cluster`) is three voters and a fourth node.
A cluster on the local KMS shares one key file among its nodes and is refused unless
`ALLOW_SHARED_LOCAL_KMS=yes` says so; a cluster in production uses `KMS=vault`.

| Target | `--docker` | Standalone | Cluster | Run on a real engine so far |
|---|---|---|---|---|
| Docker Desktop (macOS, Linux; Windows through WSL 2) | `dd` | yes: one container, labelled named volumes | yes: [`deploy/compose/compose.yaml`](deploy/compose/compose.yaml) itself, four nodes on one machine | standalone on Docker Desktop 29 for macOS (containerd image store): install, a round trip from another machine, upgrade, uninstall, reinstall and purge (PLAN 10.13) |
| Docker CE on this machine | `ce` | yes | yes | `make test-deploy`, both modes (Docker on Linux) |
| Docker CE on a server over SSH | `ce` with `CE_HOST=user@server` | yes | yes: compose runs on the server, from `CE_STATE_DIR` | standalone, from macOS to a Linux server |
| Podman | `podman` | yes | yes: `podman compose`, which needs Podman's API socket (`podman.socket`) | `make test-deploy`, both modes (Podman 5.8 on Linux, SELinux enforcing) |
| Rancher Desktop, moby | `rancher` (`RANCHER_ENGINE=auto` or `moby`) | yes: Docker Desktop's code through the context `rancher-desktop` | yes | Rancher Desktop 1.25 for macOS: standalone and a four-node cluster - install, round trip, upgrade, uninstall, reinstall, purge (PLAN 10.6) |
| Rancher Desktop, containerd | `rancher` (`RANCHER_ENGINE=auto` or `containerd`) | yes, through `nerdctl` | refused: `nerdctl compose` cannot upgrade one node at a time | Rancher Desktop 1.25 for macOS (nerdctl 2.2.2): standalone - install, round trip, an upgrade to another image, uninstall, reinstall, purge (PLAN 10.6) |
| Rancher Desktop's Kubernetes | `rancher` with `RANCHER_KUBERNETES=yes` | yes: the k3s and k8s path through the context `rancher-desktop`, the image built on the engine its k3s sees (`nerdctl --namespace k8s.io` on containerd, `docker --context rancher-desktop` on moby) and run with `imagePullPolicy: Never` | yes | Rancher Desktop 1.25 for macOS (its k3s v1.25), on moby and on containerd: install, round trip through a port-forward, an upgrade to another image, uninstall, reinstall, purge (PLAN 10.6) |
| k3s | `k3s`: here (`k3s kubectl`), on a server (`K3S_SSH=user@server`, where it runs `k3s kubectl`, so the admin kubeconfig stays there), or through a context (`K8S_CONTEXT`) | yes: the chart, rendered and applied | yes | k3s in a container (`make test-k8s`): both modes, an upgrade, uninstall and purge; standalone on a remote k3s server over `K3S_SSH` with `K8S_STORAGE=pvc` (local-path): install, round trip through the NodePort, upgrade, uninstall, reinstall and purge (PLAN 10.13) |
| Kubernetes (1.27 or later aimed at; the version is not checked) | `k8s` with `K8S_CONTEXT` (the current context is never assumed) | yes | yes | no: the fakes only |

Every target is also run by the harness of `make deploy-check`, against recording fakes
of each tool. On Docker targets S3 is published on `BIND` (default `127.0.0.1`) and the
admin port on `ADMIN_BIND`, `127.0.0.1`; in cluster mode the ports are compose's - S3 on
every address of the machine at `S3_PORT`, `+10`, `+20` and `+30`, the admin ports on
`127.0.0.1` - and `BIND` cannot be set. On Kubernetes the script renders the chart with
`helm template` and applies it with `kubectl apply --server-side`; there is **no Helm
release**. S3 is a NodePort by default, and a NodePort or LoadBalancer needs
`K8S_S3_ALLOW_FROM` (address ranges, or `any`); the admin Service stays a ClusterIP
unless `K8S_ADMIN_SERVICE_TYPE` and `K8S_ADMIN_ALLOW_FROM` are both set. The image
reaches a cluster by a push to `K8S_REGISTRY`; by an import into k3s's containerd
(`K3S_SSH`, the command `K3S_IMPORT`, or the k3s of this machine; a cluster of one node
only); for the contexts `docker-desktop` and `rancher-desktop`, from the engine on this
machine; or, with `IMAGE_SOURCE=image`, by the nodes pulling `IMAGE` by its digest.

### Secrets, and where they live

A deployment begins with secrets: its root credentials; on the local KMS **the KMS
passphrase and the sealed key file `kms.json`, which open the data only together**; on
Vault a token instead; and its TLS certificates and keys. Those you do not give as files
(`ROOT_ACCESS_KEY_FILE`, `ROOT_SECRET_KEY_FILE`, `KMS_PASSPHRASE_FILE`, `KMS_KEY_FILE`,
`VAULT_TOKEN_FILE`, `TLS_*_FILE`) are made once, on this machine: the root credentials and
the passphrase from `/dev/urandom`, `kms.json` and the certificates by
`cactusfs dev-init` in a container. They are never on a command line, in a container's
environment, or printed: they go from file to file, on standard input.

Every secret is kept twice: in the deployment's entry of `STATE_DIR` on this machine, and
on the target before anything there starts - on a Docker engine the labelled volumes
`<name>-secrets` and `<name>-config` (a compose cluster: `<name>-config`, and compose's
secret files in the entry's `compose/secrets/`, or on `CE_HOST` in `CE_STATE_DIR` on the
server); on Kubernetes, Secrets of the deployment's namespace. A secret either copy holds
is never made again. One copy missing is put back from the other; two copies that differ
are refused, naming both - the script never chooses. A root credential, passphrase or
`kms.json` given as a file must equal the copies there are; a Vault token or a
certificate of your own given as a file that differs replaces both copies (a rotation),
and the run says so.

How a secret reaches a container differs on Rancher Desktop's containerd engine.

- **What `nerdctl` lacks.** Its `nerdctl` has no `cp -a`. A tar stream on standard
  input or output (`nerdctl cp -`) is not in every release Rancher Desktop bundles: the
  `nerdctl` v2.2.2 of Rancher Desktop 1.25.0 answers "support for reading a tar archive
  from stdin is not implemented yet", or "... writing a tar archive to stdout ...". And
  `nerdctl cp` on a container that never ran does not see the volumes mounted into it.
  So the script uses none of these there.
- **How it works instead.** Rancher Desktop's `nerdctl` is a wrapper that runs
  `nerdctl` as root inside Rancher Desktop's VM (`rdctl shell sudo nerdctl ...`), and
  the script reaches that VM the same way, through `rdctl`.
- **dev-init's container.** The script makes a private directory under `/dev/shm` in
  the VM: it is root's, `0700`, and given to the nodes' user 65532. The script checks
  that it is on a tmpfs before writing anything there, so nothing reaches the VM's
  disk. (The VM's `/tmp` is on its disk.) It extracts the stream there as root, keeping the
  owner (65532) and the modes, with the stream on standard input. Then `nerdctl cp
  <dir>/. <container>:<dir>` copies it in, and the directory is removed. Reading
  dev-init's output is the reverse: `nerdctl cp <container>:<dir>` into such a
  directory, archived there into a file of the deployment's entry on this machine,
  then the directory is removed. If a step fails or the run is stopped, the run's exit
  trap still removes the directory, and then the container.
- **The secrets' volumes.** They have no helper container there. The script asks
  `nerdctl volume inspect` where a volume is in the VM, and accepts only
  `/var/lib/nerdctl/<digest>/volumes/<namespace>/<volume>/_data`. It then extracts the
  stream into that directory, or archives it from there, as root, keeping the owner
  65532 and the modes. Before each put it makes the volume's root 65532 and `0700`. A
  volume that holds files under another owner or mode is refused, with the commands
  that fix it. Nothing else is read or written.
- **`rdctl`.** It must be on `PATH` or beside `nerdctl`: Rancher Desktop puts both in
  `~/.rd/bin`. Without it, an install there is refused before anything is changed.

Upgrades differ on Rancher Desktop's containerd, for two reasons:

- containerd starts again a container stopped with the restart policy `always`, which
  the node has. So before the script stops a node - the old one in an upgrade, or the
  one an uninstall removes - it sets that node's policy to `no` (`nerdctl update
  --restart no`), and gives it back whenever it starts that node again.
- `nerdctl` keeps a stopped container's ports until it is removed. So the old node is
  removed rather than kept as `<name>-previous`, after the script checks that its image
  is still there.

If the new node cannot run, the script removes it and runs the old image again, by its
digest, on the same volumes. The run then fails and says it rolled back and nothing was
upgraded. If the old image does not come up either, the run says the node is down and
gives the command to run it again. If the new node runs but is not ready within
`WAIT_TIMEOUT`, it is left running, because it may already have changed the metadata;
the run fails and gives the command to go back. The same command is printed if the run
is stopped in between. Volumes are never removed.

On every engine, a helper container the run made is removed when the run fails:
`<name>-dev-init`, which holds the passphrase once its input is in, and `<name>-volumes`
(not made on Rancher Desktop's containerd). A rerun removes one left by an earlier run
only when it carries the deployment's labels; one without them is refused.

`STATE_DIR` is `$XDG_STATE_HOME/cactusfs-deploy`, else
`~/.local/state/cactusfs-deploy`, unless the env file names another; one inside the
repository is refused, and one in a folder a sync service copies (Documents, Desktop,
iCloud Drive, OneDrive, Dropbox, Google Drive) is warned of. Directories are 0700 and
files 0600. One entry a deployment, `STATE_DIR/<target-id>/<name>/`, where the target ID
is the target and its Docker or kubectl context - `dd-default`, `ce-default`,
`podman-default`, `rancher-rancher-desktop`, `k3s-local`, `k8s-<context>` - or
`ssh-<host>` for `CE_HOST` and `K3S_SSH` (a character other than a letter, digit, `.`,
`_` or `-` written as `_`: `CE_HOST=deploy@server.example.test` is
`ssh-deploy_server.example.test`):

| In the entry | What it holds |
|---|---|
| `target` | the record: the engine or cluster the deployment was made on, its own ID (the engine's, or the UID of the cluster's `kube-system`) and the KMS. A later run whose settings reach another is refused |
| `secrets/root-access-key`, `secrets/root-secret-key` | the root credentials (S3, the management API) |
| `secrets/kms-passphrase` | `KMS=local`: the passphrase of `kms.json` |
| `secrets/vault-token` | `KMS=vault`: the copy of `VAULT_TOKEN_FILE` |
| `config/` | what `dev-init` wrote: `kms.json` (`KMS=local`), `s3.pem`, `s3.key`, `s3-ca.pem` (the CA clients trust), `node-ca.pem`, `nodes/`, `ready` - or your own files in their place |
| `ssh-known-hosts` | `CE_HOST`: the host keys pinned at the first install ([below](#adding-a-servers-host-key)) |
| `compose/` | a compose cluster on this machine: `compose.yaml`, the generated `override.yaml` and `compose.env`, and `secrets/` |
| `namespace`, `namespace-uid` | Kubernetes: the namespace and its marker, without which the script treats a namespace as somebody else's |
| `images` | the image tags the script made: on k3s those it imported; on a Docker engine and Rancher Desktop's Kubernetes `target <tag> <engine ID>` for those it built or loaded on the target's engine, and `here:<context> <tag> <engine ID>` for a copy built on this machine for `CE_HOST` ([Image tags](#image-tags)) |

A secret you give as a file is read from that file: keep it, as you keep `STATE_DIR`.

**On `KMS=local`, every object is opened by `kms.json` and the passphrase together.**
Losing both copies of either - the entry and the target's - loses every object of the
deployment, and no one can get them back. On Kubernetes an `--uninstall` deletes the
namespace's Secrets and leaves the entry the only copy until the next `--install`.
**Back up `STATE_DIR`, encrypted**, after every install that made or replaced a secret
(here the default `STATE_DIR`; gpg asks for the backup's passphrase):

```sh
tar -C ~/.local/state -cf - cactusfs-deploy | gpg --symmetric --cipher-algo AES256 -o cactusfs-deploy.tar.gpg
# and back:  gpg -d cactusfs-deploy.tar.gpg | tar -C ~/.local/state -xpf -
```

(`age -p` serves as well as `gpg --symmetric`.) Keep the backup's passphrase apart from
the backup, and the backup off this machine. A copy that a sync or backup service makes
of `STATE_DIR` as it is, unencrypted, is a copy of every key it holds.

Three flags are about that record, and none is a setting of the env file:

- `--adopt`: the target holds the deployment's labelled volumes and `STATE_DIR` holds no
  record of them (the deployment was made from another machine, or the record was
  lost). With `--install` their secrets are taken into `STATE_DIR`; with `--uninstall
  --purge` the engine reached is recorded as the deployment's so that they may be
  purged. Without it, that is refused.
- `--engine-replaced`: with `--install`, the engine recorded is gone (a factory reset of
  Docker Desktop, say) and the one the settings reach now takes its place; the run says
  so and records it. Without it, another engine is refused.
- `--another-target`: deploys a name already recorded on another target onto this one
  as well, as a second deployment with secrets of its own.

### Adding a server's host key

Every ssh the script runs - to `CE_HOST`, `K3S_SSH` and `BUILD_HOST`, and the Docker
CLI's to `CE_HOST` - runs with `BatchMode=yes` and `StrictHostKeyChecking=yes`: it never
prompts and never accepts a host key `known_hosts` does not already hold. A server it
has not seen is refused with ssh's own message (exit 2). Add its key yourself, once, and
only after checking it:

1. Ask the server's administrator for the fingerprint of its host key, by a way other
   than this network - read on the server's console with
   `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`, say.
2. Connect once with ssh asking, under the same name the env file gives
   (`ssh -G` resolves it, so a `Host` entry of `~/.ssh/config` with its `HostName` and
   `Port` is honoured):

   ```sh
   ssh -o StrictHostKeyChecking=ask deploy@server.example.test true
   ```

   ssh shows the key's fingerprint. Answer `yes` only when it is the one the
   administrator gave; ssh then adds the key to `~/.ssh/known_hosts`.
3. Run the script again.

`CE_HOST` names no port (the Docker CLI reads it as `ssh://<CE_HOST>`): give the server
a `Host` entry in `~/.ssh/config` with its `HostName` and `Port`, and name that. At the
first install with `CE_HOST` the lines of `known_hosts` that accept the server are
pinned in the entry's `ssh-known-hosts`, and from then on every ssh to it accepts it by
those alone, even if `known_hosts` changes; a key revoked there (`@revoked`) is still
refused. When the server's key is replaced on purpose, check the new fingerprint the
same way, add it to `known_hosts`, and remove `ssh-known-hosts` from the entry: the next
`--install` pins the keys `known_hosts` then holds. `K3S_SSH` and `BUILD_HOST` are
checked against `known_hosts` at every run, and not pinned.

### Offline: OFFLINE=yes and the helm image

What the script fetches is listed in ADR-0033 section 12, with the way to avoid each:
the helm image (Kubernetes), the distroless runtime image the image and the secrets'
helper containers are made from, the image named by `IMAGE_SOURCE=image`, everything
`--from-source` builds from, and Docker CE when `CE_INSTALL_DOCKER=yes`. Nothing else;
a node itself calls nothing on the internet.

`OFFLINE=yes` makes it fetch nothing public: every image it needs must be on the engine
already (asked with `image inspect` first), builds run without pulling, and
`--from-source`, `CE_INSTALL_DOCKER=yes` and an `IMAGE` that is not here are refused.
A push to `K8S_REGISTRY`, your own registry, stays allowed. What to have in place first:

- **The runtime image**, which the image of `IMAGE_SOURCE=binaries`, `dev-init` and the
  helper containers of a Docker target are made from (on the server too, with
  `CE_HOST`); with `IMAGE_SOURCE=image`, `IMAGE` serves instead. Its reference, pinned by digest:
  `sed -n 's/^ARG RUNTIME_IMAGE=//p' Dockerfile`.
- **The helm image**, for a Kubernetes target, in `docker` (the context
  `ENGINE_CONTEXT` names) or `podman` on this machine:

  ```sh
  HELM_REF=$(sed -n 's/^ARG HELM_IMAGE=//p' Dockerfile)   # alpine/helm, pinned by digest
  docker pull "$HELM_REF"            # while this machine may still fetch it
  docker image inspect "$HELM_REF" >/dev/null && echo present
  ```

  The script looks it up by that reference: a copy carried in another way (`docker
  save` and `docker load`) must answer `docker image inspect "$HELM_REF"` too. Or name
  a copy in your own registry with `HELM_IMAGE` (by the same digest:
  `registry.example.test/mirror/helm@sha256:<digest>`), or a helm program on this
  machine with `HELM=/usr/local/bin/helm`; not both.
- **`IMAGE`**, when `IMAGE_SOURCE=image`, on the engine here (or already in
  `K8S_REGISTRY`, for nodes that pull).

### Reaching the deployment

`--install` ends, and `--status` says, where the deployment is reached, which CA to
trust (`config/s3-ca.pem` of the entry) and which files hold the root credentials -
never the credentials themselves:

| Target | S3 | Admin API and console |
|---|---|---|
| Docker, standalone | `https://<BIND>:<S3_PORT>`, by default `https://localhost:9000` | `https://127.0.0.1:9001` (`ADMIN_BIND:ADMIN_PORT`) |
| Docker, cluster | `S3_PORT`, `+10`, `+20`, `+30` (nodes 1 to 4) on every address | `127.0.0.1`, `S3_PORT + 1`, `+11`, `+21`, `+31` |
| Docker CE on `CE_HOST` | the server's address | the server's `127.0.0.1` only: `--status` prints the `ssh -L` command that brings it here |
| Kubernetes | each address of a machine that runs a ready pod, at the NodePort, or the load balancer's | a ClusterIP: `--status` prints the `kubectl port-forward` command (with `K3S_SSH`, the `ssh -L` to the server and the `k3s kubectl port-forward` to run there) |

For a deployment on this machine with the defaults:

```sh
E=~/.local/state/cactusfs-deploy/dd-default/cactusfs
export AWS_ACCESS_KEY_ID="$(cat "$E/secrets/root-access-key")" AWS_SECRET_ACCESS_KEY="$(cat "$E/secrets/root-secret-key")"
aws --endpoint-url https://localhost:9000 --ca-bundle "$E/config/s3-ca.pem" s3 mb s3://photos
```

The development certificate names `localhost`, the loopback addresses and what
`TLS_HOSTS` lists (`s3.example.test,192.0.2.10`, say); a client that reaches S3 by
another name needs it there at the first install, as the certificate cannot be given
names later. **The console's first user is `admin` / `admin`** ([First
sign-in](#first-sign-in)): every install says so. Change its password or delete it
before the admin port is reachable from anywhere but this machine; a non-loopback
`ADMIN_BIND` is warned about.

### Upgrade, uninstall, purge

- **Upgrade**: `--install` again, with a new image (a new `make image-binaries`, an
  `IMAGE`, or `--from-source`). The data is kept. Standalone on Docker: the node is
  stopped and kept as `<name>-previous`, the new one is run on the same volumes and the
  old one removed once the new one answers `/readyz`; when it does not, the run says the
  node is down and how to go back. A cluster: one node at a time - the one that does not
  vote, then the voters, the leader last when the admin API names it - each ready
  before the next; the first that is not ready within `WAIT_TIMEOUT` seconds stops the
  upgrade with a non-zero exit.
- **Kubernetes and the console's upgrade.** The script reads the lock of an upgrade from
  the console ([Checking for releases and upgrading](#checking-for-releases-and-upgrading))
  and refuses, or stops, while it is held; it never takes it. After the console has
  moved the nodes to another image, `--install` stops at the conflict on the image
  field. `--install --take-image` (it asks for the deployment's name at the terminal,
  or takes `--take-image=NAME`) takes the image field back for the script - that field
  and the version label that goes with it, nothing else - and the pods roll one at a
  time. It is refused while the console's lock is held.
- **Uninstall**: `--uninstall` removes the containers, or on Kubernetes the workload,
  its Services, NetworkPolicy, PodDisruptionBudget and Secrets, and keeps the volumes or
  claims, the namespace and the entry of `STATE_DIR`: `--install` runs it again with its
  data.
- **Purge**: `--uninstall --purge` deletes the data and both copies of the keys, for
  good. It asks for the deployment's name at the terminal, or takes
  `--purge-confirm=NAME`; never from the env file. In this order: the data (volumes, or
  claims), a listing that proves none is left, the target's secrets (and the namespace,
  when it is the script's and empty), the image tags it recorded making that nothing
  uses ([Image tags](#image-tags)), and last the deployment's entry of `STATE_DIR` -
  nothing else of it. A step that fails stops the purge with the entry kept. Only what
  carries the deployment's label is deleted, on the engine or cluster recorded for it.
  With `K8S_STORAGE=hostpath` the host directories, and on a StorageClass whose
  `reclaimPolicy` is `Retain` the released PersistentVolumes, are named and left:
  the script deletes no cluster-scoped object.

### Image tags

An image made of `make image-binaries`'s folder takes the time of the commit
`BUILDINFO` names (git's committer time, when this repository holds that commit), so
that another install of them adds no image - of the same binaries, or of the same
folder, by engine:

- `podman build` is given `--timestamp`, and `nerdctl build` `SOURCE_DATE_EPOCH` with
  the output `type=docker,rewrite-timestamp=true` (BuildKit 0.13 or later): the image's
  times and every file's time in its layers are the commit's, so **the same binaries**
  make an image of the same digest and the same tag `cactusfs:<version>-<build>-<12 hex>`.
- `docker buildx build` is given `SOURCE_DATE_EPOCH` and `--provenance=false`, which fix
  the image's own times, but on Docker's `docker` driver the files of the layers keep
  their own times: the engine's exporter drops `rewrite-timestamp` on the classic image
  store and refuses it on the containerd store. There **the same folder** of binaries
  makes the same image; a copy of it made anew (new file times) makes another, and its
  tag is then removed as below.

When git does not find that commit the run says so, and the image takes the time of its
build.

The tags the script makes on an engine that did not hold them - built there, or loaded
there from the engine they were built on (`CE_HOST`) - are recorded in the entry's
`images` file, each with the ID of its engine: the deployment's (`engine-id` of its
`target` file) for the target's engine, and `docker --context <context> info`'s for the
copy built on this machine for `CE_HOST`. A tag that was there before (another
deployment's, or yours) and an `IMAGE` it runs are never recorded, nor is a tag on the
engine of a Kubernetes target other than Rancher Desktop's. Once an upgrade's nodes are
ready, the tag the replaced nodes ran is removed when it is recorded and no container of
that engine, stopped ones included, names it; so are the copies built on this machine
for `CE_HOST` by earlier runs. A compose cluster's `init` container keeps the image of
the first install until an uninstall removes it. A purge removes, before the entry,
every recorded tag no container uses, on `dd`, `ce` (here, or on `CE_HOST` over its SSH
route), `podman`, `rancher` and Rancher Desktop's Kubernetes, each with the engine's own
CLI (`docker --context`, `podman`, `nerdctl` in its namespace); on k3s the tags it
imported, as above. On Rancher Desktop's Kubernetes, after an upgrade and at a purge, a
tag is also kept while a pod or a workload's template (StatefulSet, Deployment,
DaemonSet, Job, CronJob) of any namespace names it, or the digest of its image (a pod's
`imageID`); when those cannot be listed, every tag is kept.

A recorded tag is acted on only on the engine whose ID its line names: when the context
or the settings now reach another engine, or the line has no ID (written by an earlier
build of the script), the tag is named and left. On Rancher Desktop's Kubernetes a line
names the cluster (its `kube-system` UID) and the engine that holds the tag: after
Rancher Desktop is switched between moby and containerd, the tags recorded on the
other engine are named and left. `--engine-replaced` takes the recorded
engine as gone with its images: the lines of the target's engine leave `images`, each
said, and nothing is removed. A tag that cannot be removed is named and left, and the
purge goes on. An uninstall removes no tag: `--install` runs the deployment again with
it.

### Windows through WSL 2

On Windows the script runs inside a WSL 2 distribution, with Docker Desktop's WSL
integration turned on for that distribution: clone the repository there and run
`deploy/deploy.sh` from its shell, as on Linux. `STATE_DIR` is then in the
distribution's home directory. There is no PowerShell entry point, and Git Bash is not
supported (the script does not detect it).

## The S3 API

Path-style addressing, and virtual-hosted-style with `--s3-domain`. SigV4 in the
`Authorization` header, in the query string (presigned URLs, valid for at most seven
days) and for signed streaming (`aws-chunked`) uploads. Errors are S3 XML.

| | Operations |
|---|---|
| Buckets | ListBuckets, CreateBucket, HeadBucket, DeleteBucket, GetBucketLocation |
| Objects | PutObject, GetObject (with one byte range, and `If-Match` and its kin), HeadObject, DeleteObject, DeleteObjects, CopyObject |
| Listings | ListObjectsV2, ListObjects (version 1), ListObjectVersions |
| Multipart uploads | CreateMultipartUpload, UploadPart, UploadPartCopy, CompleteMultipartUpload, AbortMultipartUpload, ListParts, ListMultipartUploads |
| Versioning | PutBucketVersioning, GetBucketVersioning, and `versionId` on GetObject, HeadObject and DeleteObject |
| Object Lock | buckets created with it; Put/GetObjectLockConfiguration (default retention); Put/GetObjectRetention; Put/GetObjectLegalHold; governance bypass. A version under compliance retention or legal hold is deleted by nobody, root included |
| Bucket configuration | Get/Put/Delete of BucketPolicy, BucketCors, BucketTagging and BucketLifecycle; CORS preflight. A bucket policy is the only thing that can open a bucket to anonymous readers, and only to read |
| Object tags | GetObjectTagging, PutObjectTagging, DeleteObjectTagging |
| Replication configuration | PutBucketReplication, GetBucketReplication, DeleteBucketReplication: the role `arn:cactusfs:replication:::<target>` names a target of kind `s3`, every rule names one destination bucket, with a prefix, a priority and delete markers; two enabled rules of one priority whose prefixes overlap are refused. `Filter` with `Tag` or `And`, `SourceSelectionCriteria`, `ExistingObjectReplication`, and the destination's `StorageClass`, `EncryptionConfiguration`, `ReplicationTime`, `Metrics`, `Account` and `AccessControlTranslation` answer `501`. A rule of the first form of the schema (`<Prefix>` in the rule) that says nothing of delete markers does not copy them, where S3 would; one of the later form (`<Filter>`) must say. What a rule matches is sent by the node that leads, and what was there before by a resync ([Replication between sites](#replication-between-sites)). `x-amz-replication-status` is answered on GetObject and HeadObject |
| Notifications | PutBucketNotificationConfiguration, GetBucketNotificationConfiguration: `QueueConfiguration` and `TopicConfiguration` whose ARN is `arn:cactusfs:notify:::<target>`, the events `s3:ObjectCreated:*`, `:Put`, `:CompleteMultipartUpload`, `s3:ObjectRemoved:*`, `:Delete`, `:DeleteMarkerCreated`, and `prefix` and `suffix` filters ([Notifications](#notifications)) |
| Lifecycle rules | expire objects, remove old versions, abort unfinished multipart uploads; carried out by the node that leads, every `--lifecycle-interval` |
| Server-side encryption | always on. SSE-S3 (the default, and `AES256`), SSE-KMS (`aws:kms`, with an encryption context) and SSE-C (a key of the customer's, over TLS and in signed headers only) |
| Payloads | literal, `UNSIGNED-PAYLOAD` and streaming, with `Content-MD5` and CRC32, CRC32C, CRC64NVME and SHA-256 checksums, verified before anything is stored |

Everything else - ACLs, conditional writes and the rest -
answers `501 NotImplemented`, after authentication. The contract is the compatibility
suite in `test/s3compat` (`make s3-compat`): the AWS SDK for Go, the AWS CLI and a
selected list of the S3 tests of Ceph, held to a recorded baseline.

## Management API, documentation and console

The admin listener (`--admin-addr`, port 9001) serves:

- the **management API** under `/api/v1`. Authenticate with an API key,
  `Authorization: Bearer <access key ID>:<secret key>` (the root credentials work the
  same way), or with a console session and its CSRF token. Errors are
  `application/problem+json`, and every request that changes something can be made safe
  to repeat with `Idempotency-Key`;
- its **contract** at `/api/v1/openapi.yaml` and `.json`, and **Swagger UI** at `/docs`,
  served from the binary - nothing is fetched from the internet;
- the **console** at `/`, when the binary was built with it (`make build`, or the
  container image). Sign in with a user that has a password; a binary built with plain
  `go build` says that it has no console.

```sh
curl --cacert ca.pem -H "Authorization: Bearer $ACCESS_KEY:$SECRET_KEY" \
     -H 'Content-Type: application/json' -H 'Idempotency-Key: make-alice-1' \
     -d '{"name":"alice","roles":["read-write"],"password":"a long password"}' \
     https://localhost:9001/api/v1/users
```

What the API has, under `/api/v1` (the contract is
[`api/openapi/admin.yaml`](api/openapi/admin.yaml)):

| Path | For |
|---|---|
| `/session`, `/session/unlock` | sign in, the session, sign out; unlock a locked session with the user's password |
| `/buckets`, `/buckets/{bucket}`, `/buckets/{bucket}/usage` | list, create, read and delete buckets; how many objects a bucket holds and their size - once its usage is counted (`exact`), also its versions, the bytes it stores and holds in uploads, and its quota |
| `/buckets/{bucket}/quota`, `/buckets/{bucket}/usage/recount` | a bucket's quota: read, set and remove it ([Quotas](#quotas)); have its usage counted again (`202`) |
| `/buckets/{bucket}/objects`, `/buckets/{bucket}/object`, `/object/metadata`, `/object/access` | list objects; upload, download and delete one; its metadata; who uploaded it and how often it was downloaded |
| `/buckets/{bucket}/object/preview` | an object's bytes for the console to show it (ADR-0028): PNG, JPEG, GIF, WebP, PDF, DOCX, XLSX, text, CSV and JSON, decided by the stored type and the bytes together; for whoever may read the object and either uploaded the version shown or is allowed `admin:PreviewObject`; always an attachment, never served inline; up to `--preview-max-size`, text to its first MiB; counted as a download |
| `/buckets/{bucket}/policy` | a bucket's policy: the document S3's `GetBucketPolicy` and `PutBucketPolicy` read and write; one that cannot be kept answers `400 InvalidPolicy` |
| `/buckets/{bucket}/subbuckets`, `/buckets/{bucket}/subbuckets/{name}` | sub-buckets (ADR-0017): list (names and ages, never a policy), read with the policy, create or set the policy, delete an empty one; at most 10 a bucket. In the console: a bucket's details |
| `/buckets/{bucket}/tags`, `/buckets/{bucket}/object/tags`, `/buckets/{bucket}/tag-rules`, `/tag-definitions`, `/tag-definitions/{key}`; `X-CactusFS-Tagging` on an upload | tags of buckets and objects, a bucket's tag rules and the catalogue of tags (ADR-0018); tags are sent and answered as lists of `{key, value}`, and an upload's tags as a query string in `X-CactusFS-Tagging`. A bucket made here keeps its tags in lower case from the start (its rule `lowercase`); one made with S3's `CreateBucket` has no rules |
| `/search` | buckets by name and objects by key |
| `/users`, `/users/{name}`, `/users/{name}/keys`, `/keys/{access_key_id}` | users and their API keys |
| `/groups`, `/roles`, `/policies` (and `/{name}` of each) | groups, roles, policies |
| `/policy-catalogue` | what a policy can say on this node: every action a policy can grant, with its group, what it allows, the resources it is asked on and where it may be used; the condition keys and their operators; the resource kinds, the limits and the principals of a bucket policy (`admin:GetPolicyCatalogue`: `cluster-admin`, `bucket-admin`, `auditor`) |
| `/policy-validation`, `/buckets/{bucket}/policy/validation`, `/buckets/{bucket}/subbuckets/{name}/policy/validation` | check a named policy or a new key's inline policy, a bucket's policy, a sub-bucket's policy, without storing it: every fault with where it is, and what each statement covers and whether it opens the bucket to anyone. Allowed to whoever may save that document there ([below](#the-policy-builder-and-who-may-do-this)) |
| `/access/explain` | "Who may do this?": what the authorizer would decide for a user, an API key or anyone, an action and a resource, and by which statements (`admin:ExplainAccess`, which, of the built-in roles, only `cluster-admin` holds); every answer is in the audit trail |
| `/cluster`, `/cluster/maintenance` | nodes, drives and erasure sets; healing, scrubbing and collection |
| `/settings` | the settings of every node |
| `/audit` | the audit trail |
| `/targets`, `/targets/{name}`, `/targets/{name}/status` | define, read, list and delete the targets things are sent to (webhooks and NATS servers; other deployments, of kind `s3`, for replication); a secret and the path of an endpoint are written and never read back, a webhook's signing key is shown once; how delivery to each stands, from the node that leads ([Notifications](#notifications), [Replication between sites](#replication-between-sites)) |
| `/buckets/{bucket}/notifications` | a bucket's notification rules: read, set (in order) and remove - the configuration S3's `?notification` reads and writes |
| `/buckets/{bucket}/replication`, `/buckets/{bucket}/replica` | a bucket's replication configuration (read, set, removed - the document S3's `?replication` reads and writes) and whether it is a replica of another deployment: made one, or promoted ([Replication between sites](#replication-between-sites)) |
| `/buckets/{bucket}/replication/resync` | starts the resync of a bucket, from its first version (`admin:ResyncReplication`, which only `cluster-admin` holds; `202` with the record), walked by the node that leads |
| `/buckets/{bucket}/replication/status` | how far a bucket's replication has come: what waits and what failed, the state of its target and the class of its last failure, what was lost, and the resync (`admin:GetReplicationStatus`: `cluster-admin`, `auditor`) |

The console has a screen for each: **Buckets** (a grid), the object browser of a
bucket, **Search** (the field at the top of the navigation), **Users** (with a user's
API keys), **Groups**, **Roles**, **Policies**, **Who may do this?**, **Tags** (the catalogue of tags),
**Targets** (where notifications and the audit trail are sent, and how delivery to each
stands), **Cluster**, **Settings** and **Audit log**; a bucket's replication, resync and
replica mode are in its details, and the object browser shows the replication status of
an object's current version (`replication_status` of `/object/metadata`). It talks only to the management API and decides nothing itself. The version and
build it runs are shown under the logo.

- **Uploads go on across screens.** Files dropped on a bucket are sent while you use
  any other screen. A bar at the top of every screen, on a phone too, shows how many
  files are on their way and how far they are, and when they are done what was stored
  and what was not, until you dismiss it. The same bar keeps notices of what the
  console did - newest first, for the visit, not stored. Leaving or reloading the page
  while files are being sent asks first.
- **An object's details in its row.** In the object browser a click on an object's
  name opens its details - key, size, ETag, last modified, content type, encryption,
  its uploads and downloads where you may see them, and its tags, which can be edited
  there - directly under its row.
- **Tags.** A bucket's tags and tag rules are in its details, folded until opened; the
  upload form gives the files it sends tags (`X-CactusFS-Tagging`), and once files are
  chosen it says which tags the bucket's rules require - or that the server will tell,
  where you may not read them - and begins with their keys. Tags are sent in the order
  you typed them; the server lower-cases them where the rules say so, and its answer is
  what is shown. A tag a rule refuses is shown with the server's reason (it names the
  rule, never a value), beside the file for an upload.
- **A session that locks while it uploads.** A console session lasts
  `--session-lifetime` (30 minutes) from sign-in, and nothing extends it. If it reaches
  that time while files are still being uploaded with it, it is **locked**, not ended:
  the console shows nothing of its screens, only how many files are left and one bar,
  and asks for your password; the uploads go on. The server lets a locked session store
  objects, read its own state, unlock and sign out, and answers everything else `423`
  with the code `SessionLocked`. The right password unlocks it and gives it a new
  session token and CSRF token and a new `--session-lifetime`. A locked session ends
  `--session-upload-grace` (90 seconds) after its last upload, and at
  `--session-lock-max` (12 hours) after the timeout at the latest; five wrong passwords
  end it at once. `--session-lock-max 0` turns locking off. Without an upload the
  session ends at its timeout, as before
  ([ADR-0027](docs/adr/0027-session-lock.md)).
- **A bucket's details on its page.** Above its objects: how many objects and how much
  storage, when it was made and by whom, its policy, and the last upload and download.
  A figure counted from the index rather than kept as the bucket changes says "not
  exact", and one you may not see says so - neither is shown as 0.
  The details fold away with a click on their heading, which then still says how many
  objects and how much storage the bucket holds; the browser remembers the choice.
- **Previews** ([ADR-0028](docs/adr/0028-object-preview.md)). "Preview" in an object's
  opened row shows its current version in a dialog: PNG, JPEG, GIF and WebP images;
  PDF (with pdf.js, one page at a time, as pictures - no links, forms or scripts);
  DOCX (paragraphs, headings, lists, tables, emphasis and embedded PNG, JPEG and GIF -
  no page layout, styles or charts); XLSX (up to 20 sheets of 1 000 rows by 100
  columns, the values saved with the file - formulas are never evaluated); and the
  first MiB of text and JSON, and of CSV as a table of up to 1 000 rows. Never
  previewed: SVG, HTML, XML, anything with script, PPTX, `.doc`, `.xls`, HEIC, TIFF,
  AVIF, audio, video, objects under SSE-C, and - but for text, CSV and JSON - objects
  over `--preview-max-size` (32 MiB); what an object is shown as is decided by the
  server from its stored type and its bytes together, never from its name. **Who may:** whoever may download the
  object (`s3:GetObject`) and either uploaded the version shown or is allowed
  `admin:PreviewObject` on it - which the built-in roles `bucket-admin`, `read-write`
  and `read-only` hold, `cluster-admin` through `admin:*`, and `auditor` does not. An
  explicit Deny of `admin:PreviewObject` stops the uploader too. A preview counts as a
  download; a refused one is in the audit trail. The console draws every preview
  itself and never opens the object as a page; DOCX and XLSX are read in a worker that
  stops at 64 MiB unpacked or after 10 seconds, and refuses DTDs and entities.
- **Delete all.** Beside "Delete selected", a bucket's page can delete every object of
  the bucket, all pages of it. It says what will happen - how many objects, that a
  bucket keeping versions keeps them and gets delete markers, that versions under Object
  Lock stay - and asks for your password, which the server checks (`POST /session/unlock`,
  the same throttle and audit as an unlock; five wrong passwords end the session)
  before anything is deleted. Each object is still one `DELETE` judged by the
  authorizer, so the password guards against mistakes; it is not a permission
  ([threat model](docs/threat-model.md), open question 32). Deletions, of both kinds,
  go eight at a time, go on across screens with their progress in the bar at the top,
  can be cancelled, and end with a notice of what was deleted and what was not, and why.

### Who uploaded an object, and who downloaded it

For every object the API tells who uploaded it and when, how often it was downloaded and
by whom (`GET /api/v1/buckets/{bucket}/object/access?key=`, shown in the console under an
object's details) - to users with the `cluster-admin` or `auditor` role, or a policy
that allows `admin:GetObjectAccess`; being allowed to read the object is not enough.

The listing of a bucket can carry the same five facts for each of its objects - who
uploaded it and when, its downloads and copies, when it was last downloaded; never by
whom - (`GET /api/v1/buckets/{bucket}/objects?access=true`, shown in the console as
columns of the object browser) to those of them who may also list the bucket -
`cluster-admin` among the built-in roles; an `auditor` may not list a bucket, and asks
about an object by its key. Whoever may list and may not see the facts gets the plain
listing, with `access_included: false`.

These are counts kept as well as can be, recorded every `--access-flush-interval`
(30 s), **not an audit trail**: a node that crashes loses up to that much of the count.

### The grid of buckets

The list of buckets can say more of each bucket (`GET /api/v1/buckets?details=true`,
shown in the console as the grid of buckets): who created it and its three latest
uploads and downloads, with who and when, to those who hold `admin:GetObjectAccess`
on the bucket - the keys of the objects only to those who may also list it
(`s3:ListBucket`) - and how many statements its policy has and whether one of them
allows something to every caller, to those who may read the policy
(`s3:GetBucketPolicy`). An upload or a download is shown only where the caller also
holds `admin:GetObjectAccess` on the object it names, as
`GET /api/v1/buckets/{bucket}/object/access` decides it: a policy that denies the
action by key takes those rows out of the list, whole. Of the built-in
roles a `cluster-admin` is shown all of it, a `bucket-admin` the policy, `read-write`
and `read-only` the plain list; an `auditor` may not list the buckets. A part the
caller may not see is left out (`access_included`, `policy_included`), bucket by
bucket: the list is never refused for it. A bucket created before release 0.7 says
nothing of its creator. The grid's "Quota" column comes from each bucket's usage: a bar
of what counts against the quota, "not set", or "not counted yet"; the button "Quota" in
a row opens the editor of the bucket's quota for whoever may read it
([Quotas](#quotas)).

### Search

`GET /api/v1/search?q=`, and the field at the top of the console's navigation: buckets
by their name and objects by their key, among the buckets you may list. What you type is
read in one of three ways. Plain text finds the names that contain it, whatever their
case (`report`). Text with `*` or `?` is a pattern for the whole name, `*` for any run of
characters and `?` for exactly one (`*.pdf`). Text that begins with `rx:` is a regular
expression in the RE2 syntax, case as written (`rx:^logs/\d{4}/`).

One request looks at a bounded number of keys for a bounded time; when it stops short it
says so (`complete: false`) and "Search further" - the `next_continuation` of the
answer - goes on from there. Every page decides again which buckets are yours to see,
all of them, from the bucket policies as the node that answers has them, so what a page
costs depends on how many buckets there are and not on how many of them are yours. The
node remembers what a policy says until the policy changes: a page takes some 0.2 s
where 5 000 buckets have policies of 16 KiB each (measured on an Apple M3). You run one
search at a time and a node eight; beyond that the answer is 429 with `Retry-After`, and
the console asks once more by itself.

**A limit, said plainly:** when deciding the buckets takes the two seconds, the answer
is 503 with a `detail` that says so. Just after a node has started this passes - it
remembers nothing yet, reads every policy in full (some ten times the work) and gets
further with each request. Past about 10 000 buckets with policies of 16 KiB each, or
150 000 with policies of 1 KiB, it does not pass: search for everybody but the root
identity, who is bound by no bucket policy, then works only within a bucket you name
(`bucket=`).

### Tags and their rules

A bucket's tag rules ([ADR-0018](docs/adr/0018-tag-rules-and-catalogue.md)) are set with
`s3:PutBucketTagRules` - held by `cluster-admin` and `bucket-admin` (through `s3:*`), or
granted by a policy - and every change of them is in the audit trail. What to know
before you set them:

- **A required tag shuts out whoever may store but not tag.** Tagging is a permission of
  its own, as in S3: an upload that carries tags needs `s3:PutObjectTagging` as well as
  `s3:PutObject`, and one that carries none breaks a rule that requires a tag. So once a
  bucket requires any tag, the built-in role `read-write` - which may store objects and
  not tag them - cannot upload to it at all, nor can anybody whose policies grant
  `s3:PutObject` alone; copies and multipart uploads alike. Grant such users
  `s3:PutObjectTagging` on the bucket (an identity policy, the bucket's policy or a
  sub-bucket's) if they are to upload there. Whether `read-write` should hold it, or
  required tags pass under `s3:PutObject` alone, is open (threat model, section 5).
- **A rule can be set that nothing can satisfy.** Rules are not checked against the
  catalogue: with "catalogue only", a required tag the catalogue does not define, or a
  value it does not list, refuses every upload the rule covers; so does a required key
  in capitals where the bucket does not lower-case (the catalogue is lower case), and a
  definition changed afterwards to no longer list a value a rule requires. A definition
  that a rule requires with exactly its key cannot be deleted (409 `EntityInUse`); no
  other use of the catalogue is guarded.
- **A rule for a sub-bucket is a rule for a prefix.** It applies to keys under
  `<name>/` whether or not the sub-bucket exists; deleting the sub-bucket leaves the
  rule, which applies again to one created later with that name.
- **Tags travel in a header on an upload** (`X-CactusFS-Tagging` here, `x-amz-tagging`
  in S3), and every listener holds a request's headers, all together, to 16 KiB - a
  request over that is answered `431` in plain text. Tags may hold any letter of any
  script; a non-ASCII letter is two to four bytes, and each byte three characters once
  URL-encoded, so ten long tags in another script can pass the limit (ten of the longest
  key and value in a three-byte script come to about 35 KB). Tags a rule
  does not require can be set after the upload (`PUT .../object/tags`, or S3's
  `PutObjectTagging`), which take them in the body.
- Changes to an object's tags are not in the audit trail (changes to a bucket's tags,
  its rules and the catalogue are). Tags, rules and the catalogue are stored unencrypted
  in the metadata, like keys and user metadata, and are never logged.

### The policy builder, and "Who may do this?"

The console edits all four kinds of policy with one builder
([ADR-0032](docs/adr/0032-policy-builder.md)): a named policy on the **Policies** screen
(and from a role's page; a built-in policy is shown, not edited), the inline policy of a
new API key on a user's page, a bucket's policy in its details, and a sub-bucket's - when
it is made and afterwards. A policy is made by choosing: each statement's effect; its
actions, from the node's list by group with a search, or as a pattern you type; its
resources, by bucket and prefix or by kind and name of an administrative resource, or as
an ARN you type; its conditions, by key, operator and value; and for a bucket's policy,
its principals - users, or anyone. The JSON beside it follows every change, and a change
you make to the JSON is read back into the builder. **The JSON is what is sent**, as
shown; an unchanged policy is never sent. What the builder cannot read - a key given
twice, an unknown field - turns it off with the reason, and leaves the text alone; an
action or condition the node does not list is kept and marked, never dropped.

- **The list of actions** comes from the node (`GET /api/v1/policy-catalogue`), which
  answers what its own release knows. The console asks for it only when you first change
  a policy, or press "Open the policy builder" - never because you looked at one. Reading
  it needs `admin:GetPolicyCatalogue`, which `cluster-admin`, `bucket-admin` and
  `auditor` hold and `read-write` and `read-only` do not. Without it, or on a node of an
  older release, you get the JSON editor as before, with a line that says why.
- **Checked as you type.** When you stop typing the node checks the policy - with the
  same parser a save goes through - and each fault is shown at its field and in a list
  under the JSON, with its line and "Show in the JSON". With the builder, a save first
  checks the exact text it will send, and sends nothing that has a fault; a bucket's or
  sub-bucket's policy is checked at save even without it. A check is allowed to whoever
  may save that policy there, and to nobody else: `admin:PutPolicy` for a named policy,
  `admin:CreateAPIKey` on the owner for a new key's inline policy (refused to an API key
  that has an inline policy of its own, as making the key is), `s3:PutBucketPolicy` and
  `s3:PutSubBucket` on the bucket. A check stores nothing and reads nothing a save would
  not, so it tells you nothing a save would not. A refused check is in the audit trail
  like every refusal; an allowed check is not recorded, by design (ADR-0032,
  Alternatives): it stores and changes nothing; a save is recorded. The check is a
  convenience: the node parses every save again and refuses what it refused before, now
  with every fault in the problem's `faults`.
- **A bucket opened to anyone is asked about first.** When the node's check says a
  statement of a bucket's policy allows `*`, the save stops and asks: "This policy
  opens *bucket* to anyone" - or "to every user of this deployment", when nothing in it
  can be done without signing in. For each such statement it gives its number and Sid,
  its Resource as written, the actions anyone could do without signing in (only reads,
  as ever), and the actions it names for anyone that still need a user - and says that
  its conditions were not weighed. Everything it says comes from the node; the console
  reckons none of it. On a node that cannot check (an older release) a save of a
  bucket's policy asks instead: "This node could not check the policy, so it cannot say
  whether it opens *bucket* to anyone. Save it as it is?". A bucket's policy is checked
  at its save even where the builder is not shown. A sub-bucket's policy cannot allow
  anyone at all (ADR-0017): its builder offers anyone only to a Deny.

**Who may do this?** is a screen of its own, and "What may *name* do?" on a user's page
opens it for that user. Name a user, an API key or anyone; an action, from the node's
list or typed; the bucket and key, or the administrative resource; and if you like a
listing prefix, the address the request would come from and whether it would arrive over
TLS. "Explain" asks the node (`GET /api/v1/access/explain`), which asks its own
authorizer - the same decision a real request would get, at the node's own time - and
the screen shows the answer as given: allowed or not; whether a policy decided it or a
rule did, or nothing allowed it (denied by default); the statements that matched, each
with its effect and where it is; what the key's own inline policy said; statements whose
condition on the source address was decided without one; what was assumed; and what was
not considered (who made a version, for `admin:PreviewObject`). A disabled user or an
expired key is answered "would not authenticate", with no decision. Asking changes
nothing and is never a permission to do what is asked about.

- **Who may ask.** It needs `admin:ExplainAccess` on the user asked about (for a key,
  its owner; for anyone, `arn:cactusfs:admin:::anonymous`), which of the built-in roles
  only `cluster-admin` holds. An answer about an S3 action on a bucket
  also needs `s3:GetBucketPolicy` on it, and one whose key or listing prefix could lie in
  a sub-bucket `s3:GetSubBucketPolicy`, since the answer would tell what those policies
  say; without them, or when such a policy cannot be read, the answer is `403`. A cited
  statement is named - its policy, its place, its Sid - only where you may read that
  policy; otherwise it says "a policy you may not read", with its effect.
- **Every answer is in the audit trail.** Every answer the node gives once a question
  is allowed - a decision, "would not authenticate", a `400` or a `404` alike - is
  recorded before it is given, with who asked, about whom, the action, the resource and
  the answer, never a policy's text. An answer the node has given is recorded even if
  you have left the page; one whose record cannot be written is not given (`503`, or
  `500` when the node cannot tell whether it was written). A question that cannot be
  decided - the store unavailable, or you gone before the decision - gets neither an
  answer nor a record. A refusal (`403`) is recorded as every refusal is; a request
  turned away as malformed before it is authorized (no subject, two subjects, a user
  name or key ID that cannot be one, or a parameter the contract refuses) is a `400`
  and is not recorded.
- **Grants that gain it on upgrade.** A policy you wrote before this release that allows
  `admin:*` - or another wildcard that matches `admin:ExplainAccess` - on any resource
  pattern that matches `arn:cactusfs:admin:::user/<name>` or
  `arn:cactusfs:admin:::anonymous` now grants it, without anybody granting it. In a
  pattern `*` also spans `/`, so that includes `*`, `arn:cactusfs:admin:::*`,
  `arn:cactusfs:admin:::user/*`, `arn:cactusfs:admin:::user/<prefix>*` and
  `arn:cactusfs:admin:::user/<name>`; `*` and `arn:cactusfs:admin:::*` also cover asking
  about anyone (anonymous). Those who hold it can ask what the users it covers may do
  with admin actions, and with S3 actions on the buckets whose policies they may also
  read (`s3:GetBucketPolicy`, and `s3:GetSubBucketPolicy` where a sub-bucket could hold
  the key). Look for such grants before you upgrade, and narrow them if that is not
  meant (threat model, question 70). Likewise a wildcard such as `admin:Get*` or
  `admin:*` on a resource that matches `arn:cactusfs:admin:::policy-catalogue` now
  includes `admin:GetPolicyCatalogue`, which tells nothing that this page and the
  contract do not.

### Quotas

A bucket can be given a quota ([ADR-0024](docs/adr/0024-quotas.md)): a hard limit, in
bytes, on what it stores.

- **What counts.** Every version the bucket keeps, current or not, at the size it was
  sent, and the parts of multipart uploads that are neither completed nor aborted - in
  the bucket's usage, `stored_bytes + upload_bytes`. A delete marker counts nothing. Not
  counted: the parity on the drives, the metadata, and shards waiting to be collected.
- **Setting it.** `PUT /api/v1/buckets/{bucket}/quota` with `{"quota_bytes": <bytes>}`,
  from 1 to 2^62; `GET` reads it with what counts against it; `DELETE` removes it. In
  the console: "Quota" in the bucket's row of the grid, in GB (1 GB is 10^9 bytes), and
  the row's bar shows how full the bucket is. Setting and removing a quota is for
  `cluster-admin` (`admin:PutBucketQuota`, `admin:DeleteBucketQuota`), reading it also
  for `auditor`; `bucket-admin` may do neither, and no bucket policy can grant it. Every
  change is in the audit trail. Whoever may list a bucket is told its quota with its
  usage. A quota below what the bucket holds is accepted: nothing is removed, and nothing
  more is taken until enough has been deleted.
- **When a write is refused.** A write that would take the bucket past its quota -
  PutObject, CopyObject, UploadPart and UploadPartCopy, and an upload through the
  management API - is answered `403` with the code `QuotaExceeded`, as an S3 XML error
  or a problem of the management API; the AWS SDKs do not repeat it. An overwrite counts
  what it adds less what it replaces where the bucket keeps no versions (or has them
  suspended), and a part less the part of the same number it replaces. Deleting,
  aborting an upload and completing one are never refused, so a full bucket can always
  be emptied - but a bucket full of versions under Object Lock stays full until they
  expire, and an upload that is never finished counts until it is aborted (a lifecycle
  rule that aborts incomplete uploads gives the bytes back).
- **Exact across nodes.** The limit is decided where the metadata entries are applied,
  in their one order, so writers through different nodes cannot pass it together. Each
  node also checks a write before it stores anything, from its own copy of the usage,
  which may be a moment behind: just after a delete through another node, a write may
  be refused and pass when repeated. A write whose length is not declared is refused
  beforehand only when the bucket is already full; otherwise it is stored, refused at
  the end if it does not fit, and its shards removed.
- **A refusal reaches a client that is still sending.** A write refused before its body
  is read would have the connection closed under the client, which sees a reset and not
  the `403`. So the node reads what is left of such a body, up to 8 MiB and for 5
  seconds, and then answers on a connection that stays open. A client that sends more
  than 8 MiB without asking `Expect: 100-continue` first can still see the connection
  closed. aws-sdk-go-v2 asks from 2 MiB: a refused body of up to 8 MiB is then sent and
  read; one declared larger is answered without being invited, and the connection is
  closed if the client has stopped waiting (aws-sdk-go-v2 waits one second) and begun
  to send.
- **Buckets you had before.** A bucket made by this release is counted from its
  creation. Once every node runs this release (metadata level 5), the node that leads
  counts the buckets you already had, by itself - ten thousand records an entry, ten
  entries a second, the buckets taking turns - and that ends the way back to the
  release before ("Going back", under [On Kubernetes](#on-kubernetes)). Until a bucket is counted its usage
  says `exact: false`, the console "not counted yet", and a quota for it is refused with
  `409`. While any node runs the release before, everything of quotas answers `503`.
- **Recount.** `POST /api/v1/buckets/{bucket}/usage/recount` (`cluster-admin`) has a
  bucket counted again while it is used. A recount that finds other figures than were
  kept is a fault of CactusFS: it puts them right, logs the bucket's name, and counts
  it in `cactusfs_bucket_usage_drift_total` - please report it.
- **Watching it.** `cactusfs_buckets_quota{state}` counts the buckets with a quota, at
  90 % of it or more, and full; `cactusfs_quota_refusals_total` the refused writes (not
  in the audit trail; [threat model](docs/threat-model.md), open question 45). No metric
  names a bucket. The alerts `CactusFSBucketQuotaFull`, `CactusFSBucketQuotasUnknown` and
  `CactusFSBucketUsageDrift` are in [`deploy/observability`](deploy/observability/README.md).

### Notifications

A bucket's changes can be told to programs outside the deployment
([ADR-0025](docs/adr/0025-bucket-notifications.md)): S3's event notifications, sent to a
webhook or to a NATS JetStream server, at least once and in the order the changes were
made.

- **Targets come first, and are the administrators'.** A target is a place the
  deployment sends to: `PUT /api/v1/targets/{name}`, or the console's **Targets** screen,
  with its kind (`webhook`: an `https://` URL; `nats`: a `tls://` server and the one
  subject it is published to), its credential (a bearer token; a NATS token, or a user
  and password), the CA certificates it must chain to when they are not public, and the
  buckets whose rules may name it (names, or patterns with `*`). Defining one is
  `admin:PutTarget`, `cluster-admin`'s among the built-in roles; a bucket's owner names a
  target, never an address. Secrets and the path of a webhook's URL are written and never
  read back - not by the API, the console, a log or the audit trail; a target is shown
  with the scheme, host and port of its endpoint. Nothing is connected to when a target
  is defined. The nodes never connect to a loopback or link-local address unless
  `--egress-allow` admits it, nor to a cloud's metadata address, follow no redirect and
  use no proxy. On Kubernetes the chart's NetworkPolicy must let them reach each target:
  `networkPolicy.egressTo`, an address range and a port for each
  ([the chart](deploy/helm/cactusfs/README.md#who-may-reach-it)).
- **Rules say what is told.** S3's `PutBucketNotificationConfiguration`, with a
  `QueueConfiguration` or `TopicConfiguration` whose ARN is
  `arn:cactusfs:notify:::<target>`; or `PUT /api/v1/buckets/{bucket}/notifications`; or
  the bucket's details in the console. A rule names a target, the changes -
  `s3:ObjectCreated:Put` (a put or a copy), `:CompleteMultipartUpload`,
  `s3:ObjectRemoved:DeleteMarkerCreated`, `:Delete` (a version removed for good), or the
  wildcards - and, if it likes, a prefix and a suffix of the key. Setting rules is
  `s3:PutBucketNotification`; a rule naming a target that is not there, or that does not
  admit the bucket, is refused (400), in the same words either way. From then on every
  change a rule matches is told; nothing of before.
- **What is sent** is S3's record, one a message (`cactusfs.notification.object.v1` in
  [`api/asyncapi/events.yaml`](api/asyncapi/events.yaml)): the time, the event name, who
  made the change, the bucket, the key (URL-encoded), size, entity tag, version ID and a
  `sequencer` - of two records the greater was made later. Three differences from S3:
  `eventSource` is `cactusfs:s3`, there is no address of the client, and a copy is told
  as a put. Object contents, user metadata and tags are never sent.
- **At least once, in order.** A message is taken off the outbox, which is replicated
  with the metadata, only once its target has taken it; after a failure or a change of
  the node that leads, up to a hundred are sent again. A receiver deduplicates by the
  ID of the message - `X-Cactusfs-Event-Id` of a webhook, `Nats-Msg-Id` in NATS (which
  JetStream deduplicates within its window) - and answers 2xx to one it has taken
  already. A webhook's 2xx, and JetStream's acknowledgement, are "taken". Any other 4xx
  than 401, 403, 408 and 429 refuses that one message, which is passed and counted
  (`rejected`); a 401 or 403 is of the credential, holds the target `stalled` until it
  is put right, and loses nothing. Anything else - a 5xx, a timeout, a refused
  connection, TLS - is tried again, after a pause that doubles to five minutes. A target
  that takes nothing keeps its messages until the outbox is at its bound; then its oldest
  are dropped, counted as `lost`, and the audit trail says so. Writes never wait for a
  target and never fail for one.
- **NATS.** CactusFS publishes to the target's subject with TLS, and neither makes nor
  changes a stream: make one that takes the subject, with a deduplication window
  (`no_stream` otherwise).
- **The audit trail through a target.** A target defined with `audit: true` is sent
  every record of the audit trail, from the first, in sequence, as the line
  `--audit-export` writes (`cactusfs.audit.exported.v1`, ID `audit-<sequence>`), so that
  a receiver can verify the chain. No record is ever passed: a target that refuses one is
  held at it. Defining or changing such a target needs `admin:GetAuditLog` as well.
- **Watching it.** The Targets screen, and `GET /api/v1/targets/{name}/status`, say of
  each target its state (`ok`, `retrying`, `stalled`), what waits, what was lost and
  rejected, and the class of its last failure - never its words or the endpoint, which
  are in the log of the node that leads. `CactusFSDeliveryStalled` and
  `CactusFSOutboxDropping` are in [`deploy/observability`](deploy/observability/README.md).

**How a webhook receiver verifies a message.** Every request is a `POST` of the record
with these headers:

| Header | Value |
|---|---|
| `X-Cactusfs-Event-Id` | the ID of the message: the same every time it is sent |
| `X-Cactusfs-Event-Type` | `cactusfs.notification.object.v1` or `cactusfs.audit.exported.v1` |
| `X-Cactusfs-Timestamp` | when this attempt was made, in seconds since the Unix epoch |
| `X-Cactusfs-Signature` | `v1=` and the lower-case hexadecimal of HMAC-SHA256 of the timestamp, a full stop and the body, under the target's signing key |
| `Authorization` | `Bearer <secret>`, when the target has one |

The signing key is made by the server when a webhook target is defined, and shown once:
in the answer to that request (`signing_key`) and in the console, until you say you
have copied it. It is used as it is shown, its characters as the bytes of the key. A new
one is had only by asking for it (`rotate_signing_key`, or "Make a new signing key" in
the console); the old one stops being used with the next request. The receiver:

1. reads the body as bytes, before it parses anything;
2. computes HMAC-SHA256, under the signing key, of `X-Cactusfs-Timestamp`, a full stop
   (`.`) and those bytes, and writes it as `v1=` and lower-case hexadecimal;
3. compares that with `X-Cactusfs-Signature` in constant time, and refuses the request
   **with 401** if they differ;
4. refuses a timestamp more than five minutes from its own clock, **with 401** as well;
5. checks `Authorization` if the target has a secret (401 when it is wrong);
6. remembers the IDs it has taken, and answers 2xx to one it has seen.

Why 401, and not 400: CactusFS treats a 401 or 403 as a credential that is wrong, and
holds the target - `stalled`, nothing passed, nothing lost - until it is put right; a
400 or any other 4xx but 408 and 429 is "this one message, never", and that message is
passed and lost. The only request of CactusFS's own that fails to verify is one signed
with a key the receiver does not have yet - after a rotation that has not reached it -
or one whose timestamp a wrong clock refuses: both are put right on the receiver's
side, after which every held message arrives. Answer 400 or 422 only for a message the
receiver has verified and will never take. What a receiver answers a forged request
makes no difference to CactusFS: that answer goes back to the forger, never to the
deployment.

```go
// verify reports whether a request of a CactusFS webhook target is genuine and fresh.
func verify(key []byte, r *http.Request, body []byte, now time.Time) bool {
	ts := r.Header.Get("X-Cactusfs-Timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Sub(time.Unix(sec, 0)).Abs() > 5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(r.Header.Get("X-Cactusfs-Signature")))
}
```

The same in a shell, for a body saved to `body.json` (compare the result with the
header, without the `v1=`). It is for trying a receiver out: `openssl -hmac` takes the key
on its command line, where `ps` shows it to every user of the machine. A receiver in
production verifies in its own process, as the Go above does:

```sh
{ printf '%s.' "$TIMESTAMP"; cat body.json; } | openssl dgst -sha256 -hmac "$SIGNING_KEY" -hex
```

The timestamp is that of the attempt, so a message that waited an hour for its receiver
is not refused for its age; the ID and the type are not signed, but the body carries the
`sequencer` (or, of the audit trail, the `sequence`), which is.

### Replication between sites

The versions of a bucket at one site can be copied to a bucket at another
([ADR-0026](docs/adr/0026-replication-between-sites.md)). The two sites are two
deployments, each with its own cluster, CA, key service, users and keys - never one
metadata group stretched across a WAN. Replication is **asynchronous** and
**active-passive**: the bucket at the other site (the replica) takes versions from its
source and from nobody else until it is promoted. Each version keeps its version ID, its
time and its order; versions may arrive in any order and are put in the order of the
source.

Below, site A is the source and site B the other site. `$A_KEY` and `$B_KEY` stand for
`<access key ID>:<secret key>` of a `cluster-admin` at each site; the addresses are
placeholders. Both sites must run this release on every node (metadata level 5): until
then every call below answers `503`.

**Site A must reach site B's S3 port, 9000,** at a name in site B's certificate - a
node, or a balancer that passes TLS through and caps no request below 5 GiB
([load balancing](docs/load-balancing.md#another-deployment-that-replicates-through-the-balancer)).
Site A connects to private addresses as it is; a loopback or link-local address needs
`--egress-allow` on site A's nodes, and on Kubernetes the chart's NetworkPolicy must let
site A's pods reach it (`networkPolicy.egressTo`). Site B never connects to site A.

#### 1. At site B: the replica bucket

The replica keeps versions. Give it Object Lock too, or a version that carries a
retention or a legal hold holds the whole target with the class `object_lock`:

```sh
aws --endpoint-url https://s3.site-b.example.test:9000 --ca-bundle site-b-ca.pem \
    s3api create-bucket --bucket photos-replica --object-lock-enabled-for-bucket
```

(Without Object Lock: `create-bucket`, then `put-bucket-versioning` with
`Status=Enabled`.) Read site A's deployment ID - `deployment_id` in the answer to
`GET /api/v1/cluster` at site A - and make the bucket its replica:

```sh
curl --cacert site-b-ca.pem -X PUT -H "Authorization: Bearer $B_KEY" \
     -H 'Content-Type: application/json' -H 'Idempotency-Key: photos-replica-mode-1' \
     -d '{"source":"<deployment_id of site A>"}' \
     https://admin.site-b.example.test:9001/api/v1/buckets/photos-replica/replica
```

It answers `{"replica":true,"source":"..."}`. Only `cluster-admin` may
(`admin:PutBucketReplicaMode`). The bucket must keep versions, have no replication
configuration of its own and hold **no version at all** (`409 BucketNotEmpty`
otherwise): a bucket becomes a replica before anything is put into it. From then on it
refuses every new version or delete marker that does not come from site A - the root
identity's too - with `409 InvalidBucketState`. It is read as before, and removing a
version for good is judged by site B's own permissions, Object Lock and lifecycle rules.
Site B's own users, with the actions for it, can still change the tags, retention and
legal hold of a version there; site A's next later change of that version sets them
again as site A has them (a retention there is never shortened).

#### 2. At site B: the key site A sends with

Site A sends with an API key **of site B** that may replicate into that bucket and do
nothing else. This is the whole policy - the replicate actions and those of the later
changes of a version, on the bucket and its objects:

```sh
POLICY='{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
 "Action":["s3:ReplicateObject","s3:ReplicateDelete","s3:PutObjectTagging",
           "s3:DeleteObjectTagging","s3:PutObjectRetention","s3:PutObjectLegalHold"],
 "Resource":["arn:aws:s3:::photos-replica","arn:aws:s3:::photos-replica/*"]}]}'
```

Give it to a user of its own through a role, and give the user's key the same document
as its inline policy, so that the key stays this narrow if the user is given more later
(`TestSiteReplication` does exactly this). In bash or zsh:

```sh
B=https://admin.site-b.example.test:9001/api/v1
b() { curl --cacert site-b-ca.pem -H "Authorization: Bearer $B_KEY" \
           -H 'Content-Type: application/json' -H "Idempotency-Key: $1" "${@:2}"; }
b replicate-policy-1 -d "{\"name\":\"replicate-from-a\",\"document\":$POLICY}" "$B/policies"
b replicate-role-1   -d '{"name":"replicator","policies":["replicate-from-a"]}' "$B/roles"
b replicate-user-1   -d '{"name":"site-a","roles":["replicator"]}' "$B/users"
b replicate-key-1    -d "{\"description\":\"site A replicates\",\"inline_policy\":$POLICY}" \
                     "$B/users/site-a/keys"
```

The last answer holds the key's `access_key_id` and its `secret_key`, shown once. With
that key, site A can add versions and delete markers to the replica, change the tags and
extend the locks of versions there, ask whether a version is there and its entity tag,
and list and abort the uploads it began. It cannot read an object, list a bucket, remove
a version, shorten a compliance retention, write to any other bucket or call site B's
management API. `s3:ReplicateObject` and `s3:ReplicateDelete` are granted only by a
statement that names them: no wildcard grants them - not `s3:*`, so neither
`cluster-admin` nor `bucket-admin` holds them - and no bucket policy can. (The root
identity does hold them: [threat model](docs/threat-model.md), question 52.)

#### 3. At site A: the target

A target of kind `s3` is site B, as site A's administrators name it
([Notifications](#notifications) says what a target is):

```sh
curl --cacert site-a-ca.pem -X PUT -H "Authorization: Bearer $A_KEY" \
     -H 'Content-Type: application/json' -H 'Idempotency-Key: target-site-b-1' \
     -d '{"kind":"s3","endpoint":"https://s3.site-b.example.test:9000",
          "region":"us-east-1","ca_pem":"<PEM of the CA of site B, newlines as \n>",
          "buckets":["photos"],
          "auth":{"mode":"sigv4","id":"<access_key_id>","secret":"<secret_key>"}}' \
     https://admin.site-a.example.test:9001/api/v1/targets/site-b
```

- `endpoint` is `https://`, the host and port of site B's S3 API, and no path.
- `region` is site B's `--region` (`us-east-1` unless it was set): requests are signed
  for it, and another one is refused there (class `auth`).
- `ca_pem` is what site B's S3 certificate chains to; leave it out for a public CA.
- `buckets` are the buckets of site A that may be sent through the target.
- The secret is sealed through site A's key service and never shown again. Defining a
  target is `admin:PutTarget` (`cluster-admin`).

#### 4. At site A: the configuration

```sh
curl --cacert site-a-ca.pem -X PUT -H "Authorization: Bearer $A_KEY" \
     -H 'Content-Type: application/json' -H 'Idempotency-Key: photos-replication-1' \
     -d '{"target":"site-b","destination_bucket":"photos-replica",
          "rules":[{"id":"everything","enabled":true,"priority":1,"delete_markers":true}]}' \
     https://admin.site-a.example.test:9001/api/v1/buckets/photos/replication
```

Each rule gives `id`, `enabled`, `priority` and `delete_markers`, and may give a
`prefix`; of the enabled rules that match a key, the one of the greatest priority
decides. The bucket must keep versions (`400` otherwise; versioning cannot be suspended
afterwards), must not be a replica, and must be one the target admits. S3's
`PutBucketReplication` sets the same: the role `arn:cactusfs:replication:::site-b`, the
destination `arn:aws:s3:::photos-replica` ([The S3 API](#the-s3-api)). Setting it is
`s3:PutReplicationConfiguration`, which a `bucket-admin` holds - for a target an
administrator opened to the bucket, and no other. Make the bucket at site B a replica
first: until it is, the target stalls with `not_replica`.

From then on, every new version a rule matches is sent by the node that leads site A's
metadata group: `replication-workers` (4) at a time, never two of one object at once,
within `replication-rate-mib` when it is set. Sent: new versions with their content
type, user metadata, tags, retention and legal hold; later changes of their tags,
retention and legal hold; delete markers that a caller made, when the rule says
`delete_markers`. **Not sent:** a version removed for good (a source that was taken over
cannot purge the replica), a delete marker of a lifecycle rule, a version under a
customer's key (SSE-C: `FAILED`), a version that is itself a replica, the bucket's
configuration and policy, users and keys. An SSE-KMS version arrives encrypted under
site B's default. Who uploaded is not carried: site B records its key as the uploader.

#### 5. What was there before: a resync

Versions that were in the bucket before the configuration are sent by a resync, and so
are the changes a target lost while it was out of reach for too long:

```sh
curl --cacert site-a-ca.pem -X POST -H "Authorization: Bearer $A_KEY" \
     -H 'Content-Type: application/json' -H 'Idempotency-Key: photos-resync-1' \
     https://admin.site-a.example.test:9001/api/v1/buckets/photos/replication/resync
```

It answers `202` with the record of the resync; it has no body, and must still say
`Content-Type: application/json`. Only `cluster-admin` may (`admin:ResyncReplication`):
a resync puts its load on the link and on site B. The node that leads walks every version
of the bucket, 500 at a time, asks site B for each and sends what it lacks; it goes on
across a change of leader from where it had come, and asking again starts it from the
beginning. A version that is `FAILED` stays so: a resync neither sends it nor changes its
status.

#### Watching it

- `GET /api/v1/buckets/photos/replication/status` (`cluster-admin`, `auditor`):
  `pending` and `failed` versions of the bucket (exact, from its usage record),
  `target_state` (`ok`, `retrying`, `stalled`, or `unsupported` when the node that leads
  cannot be asked) and `last_error_class` of the target,
  `oldest_pending_at`, `lost` and `resync_needed`, and the resync (`state` `running` or
  `done`, `scanned`, `sent`). Any node answers. The console shows the same in the
  bucket's details, with **Resync**.
- Each version says where it stands: `x-amz-replication-status` on a HEAD or GET of it
  (`PENDING`, `COMPLETED`, `FAILED`; `REPLICA` at site B), and `replication_status` in
  the console's object browser for the current version.
- **A stalled target** has a class somebody has to act on: `auth` (site B refuses the
  key - deleted, expired, another region), `no_such_bucket`, `versioning`,
  `not_replica` (the bucket there is not the replica of site A, was promoted, or runs a
  release before replication), `quota` (site B's quota is full), `object_lock` (a lock
  for a bucket there without Object Lock), `tls`, `redirect` (site B, or what is in front
  of it, answered with a redirect, which is never followed), `egress_refused` (an
  address site A does not connect to). It holds every bucket sent through it and loses nothing, and is
  tried again with pauses that grow to five minutes. `timeout`, `connect` and `server`
  are tried again too, and show `stalled` after ten failures in a row.
- **`FAILED` is final**: a version under a customer's key (`sse_c`), or one site B
  refuses for itself (`rejected`: too large, a key too long, other bytes under its ID).
- **Alerts** ([`deploy/observability`](deploy/observability/README.md)):
  `CactusFSReplicationLag` - a change has waited more than 15 minutes to be sent;
  `CactusFSReplicationResyncHeld` - a resync held at a version for 15 minutes;
  `CactusFSOutboxDropping` - changes are dropped at the bound of the outbox, so a target
  loses them (`lost` rises, `resync_needed` turns true). **When `resync_needed` is true,
  ask for a resync** of one bucket sent to that target: once it ends, the node that leads
  resyncs every other bucket sent there by itself, and clears the mark when all are
  through.
- Metrics, among them: `cactusfs_replication_lag_seconds`, `cactusfs_replication_lag_entries`,
  `cactusfs_replication_versions_total{result}`, `cactusfs_replication_bytes_total`,
  `cactusfs_replication_resyncs{state}`, `cactusfs_replication_resync_events_total{event}`;
  none names a bucket or a target.

#### Failing over: promoting the replica

**A planned switchover**, while site A still runs, goes in this order, so that nothing
written at site A is left behind:

1. Stop writes to site A's bucket (its clients, or a bucket policy that denies them).
2. At site A, wait until the bucket's status says `pending` 0 and `resync_needed`
   false - and, if the bucket held versions before its configuration was put, until a
   resync says `done`. `pending` 0 alone says nothing of versions from before the
   configuration, or of changes the target lost.
3. Note `failed`: those versions - under a customer's key, or refused by site B for
   themselves - are not at site B and will not be.
4. Promote the replica at site B (below).
5. Remove the configuration at site A (`DELETE /api/v1/buckets/photos/replication`).
6. Move the clients to site B.

**When site A is lost**, or cannot be asked, promote the replica at site B at once:

```sh
curl --cacert site-b-ca.pem -X PUT -H "Authorization: Bearer $B_KEY" \
     -H 'Content-Type: application/json' -H 'Idempotency-Key: photos-replica-promote-1' \
     -d '{"source":""}' \
     https://admin.site-b.example.test:9001/api/v1/buckets/photos-replica/replica
```

(In the console: **Promote** in the bucket's details, after typing the bucket's name.)
From then on the bucket takes writes, and nothing more from site A; nothing is sent
back. That is the whole of failing over. What follows is the operator's:

- **What site A had not sent is not at site B.** Replication is asynchronous; read
  `pending` and `oldest_pending_at` at site A first, while it can still be asked.
- **Clients need credentials of site B**, and site B's endpoint: users and keys are not
  replicated. Version IDs are the same at both sites, so a client keeps the IDs it had.
- **Stop writes to site A's bucket** if site A comes back, before it takes any: a source
  and a promoted replica that both take writes part for good, and nothing reconciles
  them.
- **Remove the configuration at site A**, there too
  (`DELETE /api/v1/buckets/photos/replication`): the bucket at site B is no longer its
  replica, so the target stalls with `not_replica` - and holds every other bucket sent
  through it - until then. The versions matched and not sent stay `PENDING` at site A.
- **It cannot be undone.** The promoted bucket holds versions, and a bucket that holds a
  version can never be made a replica again.

#### What fail-back is not

There is no fail-back: nothing switches back by itself, a promotion sends nothing back,
and the old source's bucket cannot become a replica of the new source (it holds
versions, whose order there would be wrong). Going back to site A is a new replication
the other way, into an **empty** bucket, made by hand:

1. At site A, make a bucket that keeps versions (with Object Lock if site B's versions
   carry locks) and holds none the replica of site B's deployment ID (step 1, the sites
   swapped). The old bucket can be that bucket only once it holds no version at all and
   its replication configuration is removed - which a version under compliance retention
   prevents; otherwise make a new one.
2. At site A, a key for site B, with the policy of step 2 on the new bucket.
3. At site B, a target to site A and a configuration of the promoted bucket that names
   the new bucket (steps 3 and 4), then a resync (step 5).
4. **Only what site B wrote itself crosses back.** The versions site B took from site A
   before its promotion stay `REPLICA` there, and a replica is never sent on. They are
   still in site A's old bucket, where they were made. If site A was lost they exist only
   at site B, and no replication carries them: writing them again (a copy within the
   bucket) sends them, with new version IDs and times
   ([ADR-0026](docs/adr/0026-replication-between-sites.md), open 18). **When site A's
   old bucket is gone, make those copies before step 5**, or that history is not at
   site A when it takes over.
5. When site B's status says `pending` 0, `resync_needed` false and the resync `done`,
   stop writes at site B, let `pending` reach 0 again (with `resync_needed` still
   false), promote the bucket at site A, remove the configuration at site B and move
   the clients back.

To replicate from site A to site B again afterwards, site B needs a new, empty replica
bucket in turn: every round trip copies the data again.

## Users, keys and the audit trail

Access is decided by IAM-style policies (Effect, Action, Resource, Condition; `s3:*` and
`admin:*` actions): users belong to groups, users and groups hold roles, roles are sets
of policies, and an API key belongs to a user and can never do more than its owner. Five
roles are built in: `cluster-admin`, `bucket-admin`, `read-write`, `read-only` and
`auditor`. Nothing is allowed unless a policy allows it, and an explicit deny wins - with
one exception (ADR-0028): the user who uploaded a version of an object may preview it in
the console without being given `admin:PreviewObject`, when they may also read the
object, and unless a policy denies them the preview. Of the built-in roles,
`bucket-admin`, `read-write` and `read-only` hold `admin:PreviewObject` and
`cluster-admin` holds it through `admin:*`; `auditor`, which reads how the deployment
is run and no object, does not.

An API key is an access key ID and a secret. The secret is shown once, when the key is
made, and stored encrypted through the KMS. A key can expire, can carry an inline policy
that narrows it, and stops working for every node as soon as it is deleted.

Console users are local accounts; passwords are stored as Argon2id hashes, and sessions
last `--session-lifetime` (30 minutes) and are locked, not ended, while they upload
([above](#management-api-documentation-and-console)). A change of a user's password -
by the user or by an administrator - ends every session of that user at once, locked
ones too. Failed sign-ins and failed unlocks count together, per address and per node:
ten in five minutes, then `429`. Behind a balancer that opens its own connections every
user has the balancer's address and shares that count
([load balancing](docs/load-balancing.md#the-source-address-the-nodes-see)).

Every change to identities, every refusal, every failed sign-in, and every answer of
"Who may do this?" and every refusal of it is written to a hash-chained audit trail
(`GET /api/v1/audit`, the console's Audit log) - a request to it that is malformed
before it is authorized is answered `400` and not recorded;
`--audit-export` also writes each record to standard output as a JSON line, and a
target defined with `audit: true` is sent each record, in sequence, as the same line
([Notifications](#notifications)).

## Operations

- **Probes.** `GET /healthz` and `GET /readyz` on the admin listener are unauthenticated
  and say only `ok` or `unavailable`. While a node is starting - a member of a cluster
  waits for the others - `/healthz` answers 200, `/readyz` 503, and everything else is
  refused with 503.
- **Metrics.** `GET /metrics` on the admin listener, in the Prometheus format, needs
  `Authorization: Bearer <access key ID>:<secret key>` of an API key whose owner is
  allowed `admin:GetMetrics`. No metric carries a bucket, a key or an access key.
  [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) lists the metrics.
- **Alerts and dashboards.** [`deploy/observability`](deploy/observability/README.md)
  has Prometheus alerting rules with their unit tests and three Grafana dashboards; the
  chart can render a PodMonitor and a PrometheusRule for the Prometheus Operator
  (`podMonitor.enabled`, `prometheusRule.enabled`; both off by default).
- **Traces.** With `--otlp-endpoint`, spans go to the OTLP/HTTP collector you name;
  without it nothing is recorded and nothing leaves the process.
- **Logs** are JSON on standard output. Secrets, keys, tokens and object contents are
  never logged.
- **Shutdown.** On SIGTERM the node makes `/readyz` answer 503, keeps serving for
  `--drain-delay` (zero unless set; the chart sets it), closes its listeners, lets
  in-flight requests finish within `--shutdown-timeout` and exits 0.
- **Certificates without a restart.** Every `--tls-reload-interval` (one minute) the
  certificate and key files of the listeners and of the node are read again. A new pair
  that is valid is used for new connections; one that is not is refused and logged, and
  the old one stays (`cactusfs_tls_reload_pending` is 1 while it does). Open connections
  are not touched. The node CA is read at start only: a new CA needs a restart. Rotation
  is not revocation: a replaced certificate stays valid until it expires
  ([ADR-0020](docs/adr/0020-certificate-rotation.md)).
- **Which build runs.** See [Versions and builds](#versions-and-builds).

## Versions and builds

Every build says three things about itself: the **version**, the **build** and the
**commit**.

- **The version is the release**, a semantic version without a leading `v`. It is set in
  one place: the one line of [`internal/buildinfo/VERSION`](internal/buildinfo/VERSION).
  The file is embedded in the binary, so a plain `go build` reports it as well. Two files
  that cannot read it repeat it - the default of `ARG VERSION` in the `Dockerfile` and
  `appVersion` in `deploy/helm/cactusfs/Chart.yaml` - and `go test ./internal/buildinfo`
  and `make helm-lint` fail when they differ. To make a release, change those three.
- **The build is a number**: the count of commits the binary was made from
  (`git rev-list --count HEAD`), so it grows with every commit and two builds of one
  release are told apart. It needs the whole history: a shallow clone counts short.
- **The commit** is the first 12 characters of the hash, followed by `-dirty` when files
  that git tracks had uncommitted changes. A new file git does not track yet is not
  counted by `make`: add it before a build that is to be told from its commit.

`make build`, `make build-all`, `make image` and `make up` work the three out
(`scripts/buildid.sh`) and pass them to the linker; each can be given instead:
`make image VERSION=1.0.0 BUILD=7 COMMIT=0123456789ab`. A build that nobody numbered -
plain `go build`, or `docker build .` without arguments, where git is not to be had -
is build `0`, a *local build*; in a checkout, the Go toolchain still records the commit.

```console
$ cactusfs version
cactusfs 0.7.0 build 412 (commit 330028134523, go1.27.2 linux/amd64, image)
$ go build ./cmd/cactusfs && ./cactusfs version
cactusfs 0.7.0 local build (commit 330028134523-dirty, go1.27.2 darwin/arm64, binary)
```

| Where | What it says |
|---|---|
| `cactusfs version` | version, build, commit, Go toolchain, platform, packaging (`image` for the binary of the image, whose binaries `make image-binaries` exports; `binary` otherwise) |
| the log, when a listener opens | `"version"`, `"build"`, `"commit"` |
| `cactusfs_build_info` in `/metrics`, `service.version` in traces | `<version>-<build>` |
| the management API | `server {version, build, commit}` in the answers of `POST /session`, `GET /session` and `GET /cluster` - to a caller who has been authenticated, and to nobody else: no sign-in page, probe, refusal or response header names it. `GET /settings` names the build of every node |
| the console | `v0.7.0 · build 412` under the logo of the navigation, the commit in its hint (`local build` for build `0`); beside the logo on a phone; not shown while the navigation is folded to icons |
| an image of `make image` | tag `cactusfs:<version>-<build>-<arch>`; labels `org.opencontainers.image.version=<version>-<build>` and `org.opencontainers.image.revision=<commit>` |
| a deployment with the chart | `image.tag=<version>-<build>` names the build to run, of `image.repository` (by default `ghcr.io/albievan/cactusfs`, where releases are published); pods and the chart's other objects are labelled `app.kubernetes.io/version` with it. Without a tag the chart runs `appVersion`, the release |

`GET /cluster` says the build of the node that answered. During a rolling upgrade, ask
each node, read the Settings screen, or read the label:
`kubectl -n cactusfs get pods -L app.kubernetes.io/version`.

## Checking for releases and upgrading

Off by default, and two separate opt-ins ([ADR-0029](docs/adr/0029-updates-and-upgrades.md)):
a node asks for releases only once `update-source` is set, and installs one from the
console only with `upgrade-mode`. Until then it asks nobody anything.

**Turning checks on.** Point every node at the project's releases:

```sh
CACTUSFS_UPDATE_SOURCE=https://api.github.com/repos/albievan/cactusfs/releases cactusfs server ...
```

The node asks within 5 minutes of its start and then once a day (`update-check-interval`,
`0` for only when asked), at most once a minute however it is asked, and sends nothing but
a `User-Agent` naming its version: no token, no cookie, nothing of the deployment. Files
of a release are fetched only from the source's host - for GitHub's API, from
`github.com`, which redirects to GitHub's storage; up to three redirects are followed,
each https and each address judged as `egress-allow` says. `update-channel prerelease`
offers pre-releases too.

**A mirror, for an air gap.** Any https server that answers as GitHub does will do:

- at the address you set, a JSON array of releases, each with `tag_name` (`v<version>`),
  `draft`, `prerelease`, `published_at`, `html_url` and `assets`, each asset with `name`,
  `browser_download_url` and `size`;
- the files at those addresses, **on the mirror's own host and port** - an address on
  any other host is refused and not asked: the six binaries, `SHA256SUMS`,
  `SHA256SUMS.sigstore.json`, `digests.txt` and `digests.txt.sigstore.json`, copied
  unchanged from the GitHub Release.

Give the nodes the mirror's CA with `update-ca` when its certificate is not of a CA the
system trusts (there is no way to skip verification), and a mirror on loopback or a
link-local address needs `egress-allow`. Signatures are verified offline against the
Sigstore trusted root embedded in the binary; a site that has been offline across a
rotation of Sigstore's keys copies a current `trusted_root.json` across the gap and names
it with `update-trusted-root`. With the Helm chart both files come from Secrets,
`updates.caSecret` and `updates.trustedRootSecret` (the chart's README).

**What is verified before anything is installed**, on the node, offline:

1. the release's `SHA256SUMS` (binary) or `digests.txt` (image) against its Sigstore
   bundle and the trusted root: certificate chain, transparency-log entry, timestamps;
2. that the certificate names exactly this project's release workflow at the tag of the
   version being installed - so an older signed release cannot pass for a newer one;
3. the binary's SHA-256 against `SHA256SUMS`, or the image by the digest `digests.txt`
   gives it;
4. that the version may be installed from the running one: a newer patch of the same
   minor, or the newest patch of the next minor - never a downgrade, a skipped minor or
   a new major. From 0.7.x to 0.9 goes through 0.8.

What the source lists decides what is offered, never what is installed. A release that
fails any check is refused (`412 ReleaseNotVerified`) and nothing changes; the node's log
says why.

**The Updates screen.** In the console, **Updates** (after Settings) shows the running
build, whether checks are on, when the node last asked and, as a class only, why it could
not; the newer releases with whether each may be installed and why not, and links to
their notes (never fetched); and how to install the next step yourself. A badge under the
version line says when there is one. Reading it needs `admin:GetUpdates`
(`cluster-admin`, `auditor`); "Check now" needs `admin:CheckForUpdates` and installing
`admin:UpgradeCluster` (`cluster-admin` only), all on `arn:cactusfs:admin:::updates`.
Installing is offered only to a console session, asks for your password again, and shows
the progress until the deployment answers on the new release. An API key cannot install.

**Installing: `upgrade-mode`.**

| Mode | Where | What it does |
|---|---|---|
| `off` (default) | any | Installs nothing; the screen shows the commands of your own way: the binary to download and check, or `helm upgrade … --set image.digest=sha256:…` |
| `binary` | a node on its own, run from a binary (not the container image) in a directory it may write | Downloads the binary beside the executable, checks it, keeps the old one as `cactusfs.previous`, moves the new one into place and restarts into it. **On Windows** a running executable is replaced but not restarted: the screen says `restart-required`, and whoever runs the service restarts it |
| `kubernetes` | the Helm chart with `upgrade.fromConsole.enabled` | Moves the nodes to the release's image one at a time, this node last, each by eviction under the PodDisruptionBudget; a node that does not come up is put back and the upgrade stops. Read the chart's [Upgrading from the console](deploy/helm/cactusfs/README.md#upgrading-from-the-console) first: the token it needs reaches more than it looks, and the next `helm upgrade` must name the installed digest or a newer one |

**A cluster of binaries on several machines, and compose, get the commands only**:
restarting machines the node does not control is not attempted. Replace the binary or
the image of one node at a time, and let each be ready before the next, as
[On Kubernetes](#on-kubernetes) describes for the chart.

## Develop

Day-to-day Go work needs only Go: `go build ./...`, `go test ./...`, `go vet ./...`.

The full quality bar runs inside a pinned builder image (the `builder` stage of the
`Dockerfile`), so everyone and CI use the same tool versions. It needs `make` and a
container engine, and nothing else on the host:

```sh
make check                 # every gate
make check ENGINE=podman   # the same with Podman
make help                  # the targets
```

Every target of the tables below runs inside the builder image, except `run`, `up`,
`down` and `remote-<target>`, which run on the host.

The gates, in the order `make check` runs them:

| Target | What it does |
|---|---|
| `make check` | every gate below, in this order |
| `make fmt-check` | fails on Go that `gofumpt` and `goimports` would change |
| `make lint` | `golangci-lint` (with `gosec` and the dependency rule), and a check that no gate is bypassed (`//nolint`, skipped tests) |
| `make vet` | `go vet`, with and without the build tags of the test suites |
| `make cover` | unit tests with the race detector; fails under 80 % coverage of `internal/domain` or `internal/app` |
| `make test-int` | integration tests: the built binary, a four-node cluster of containers of the product image, Vault (testcontainers-go) |
| `make s3-compat` | S3 compatibility against the recorded baseline: the AWS SDK for Go, the AWS CLI and the S3 tests of Ceph |
| `make vuln` | `govulncheck` |
| `make contracts` | generated code in sync with the contracts; no unflagged breaking change of the OpenAPI, AsyncAPI and protobuf contracts |
| `make licences` | every dependency, of the binary and of the console, on the allow-list; the console's notices file what the build of the installed tree gives, and embedded |
| `make web-check` | the console: lint, typecheck, tests |
| `make build-all` | six OS/arch binaries in `dist/` |
| `make image` | `cactusfs:<version>-<build>-amd64` and `-arm64`, each under 60 MB (see [Versions and builds](#versions-and-builds)) |
| `make sbom` | SPDX SBOMs of both images and of the console's packages in `dist/sbom/` (after `make image`) |
| `make sign-verify` | against a throw-away registry, with a key made for the run and no network: images signed by digest, SBOMs attested, what must be refused is refused, and a release rehearsed (`scripts/release.sh --dry-run`); see [Verifying an image](#verifying-an-image) |
| `make helm-lint` | the chart linted, rendered with every value set of `deploy/helm/cactusfs/ci/`, validated offline against the Kubernetes schemas, each render asserted, and what must not be deployed refused; the compose file validated |
| `make observability-check` | the alerting rules and their unit tests (promtool), the dashboards, the chart's PodMonitor and PrometheusRule, and every metric they name held to the binary's registry |
| `make verify-clone` | proves both ways of [building from source](#build-from-source) from a clean export |

Suites outside `make check`:

| Target | What it does |
|---|---|
| `make test-chaos` | fault injection: against the built binary (corruption, lost shards, replaced drives, kill during writes) and against a cluster of four containers (nodes killed during writes, the leader killed, the network partitioned, two nodes away, shards corrupted and deleted on other nodes) |
| `make test-rolling` | a rolling upgrade with the binaries themselves: three voters of the release before this entry schema, built from the history of the repository (so not possible in a clean export), and a fourth node of this tree, upgraded one at a time. From schema 4 to 5 when told nothing; `ROLLING_FROM_COMMIT=0b7d825 ROLLING_TEST=TestRollingUpgradeFromSchema3ToSchema4` runs the one from schema 3 to 4. `ROLLING_TEST` is taken as the beginning of a test's name, so the second also runs the case of a locked console session with nodes of the release before (`TestRollingUpgradeFromSchema3ToSchema4SessionLock`) |
| `make test-k8s` | the chart installed on k3s in one container, with the image this tree builds, under the `restricted` Pod Security Standard: an S3 round trip, a pod away, the NetworkPolicy enforced. Needs a privileged container |
| `make test-deploy` | `deploy/deploy.sh` against this host's engine (`ENGINE=docker` or `podman`), standalone: an install, an object of 5 MiB put and read back over TLS with SigV4, the secrets' volumes readable by the node's user, a port in use refused, an upgrade, `--status`, an uninstall that keeps the volumes, a reinstall and a purge (`test/deploy/lifecycle.sh`). It builds the image for this host first, unless `LIFECYCLE_IMAGE` names one, and removes what it made |

Everything else:

| Target | What it does |
|---|---|
| `make test` | unit tests, fast |
| `make fmt` | format Go sources |
| `make build` | `bin/cactusfs` for this OS, with the console |
| `make run` | build, then run a development node on `./.data` with `--dev-plaintext` (set `CACTUSFS_KMS_PASSPHRASE`) |
| `make up`, `make down` | start and stop the four-node compose cluster ([above](#a-development-cluster-with-compose)); `down` keeps the volumes |
| `make gen` | regenerate code from the contracts (OpenAPI, protobuf, the console's client), and the settings documents |
| `make settings-docs` | writes `deploy/cactusfs.env.example` and the settings tables of this file and `docs/ARCHITECTURE.md` from what the binary prints (part of `make gen`; a unit test fails when they differ) |
| `make s3-compat-record` | records the S3 compatibility baseline anew |
| `make web-install`, `make web-build`, `make web-lock` | the console: install its packages from the lock file, build it, rewrite `web/pnpm-lock.yaml` after a change to `web/package.json` |
| `make tidy` | `go mod tidy` |
| `make shell` | a shell in the builder image |
| `make clean` | remove build output |
| `make release` | publishes a release: for the release workflow, on a tag `v<version>`, alone; it refuses everywhere else (see [Releases](#releases)) |
| `make remote-<target>` | the same target on another machine, below |

CI (`.github/workflows/ci.yml`) runs `make check`, `make test-chaos`, `make test-rolling`
(both upgrades), `make test-k8s` and `make test-deploy` (with Docker and with Podman) as jobs
of their own, and the unit tests on Linux, macOS and Windows.

**On another machine.** When the machine you work on must not run containers,
`make remote-<target>` copies the tree as it is - the commit that is checked out and
what the working tree has changed - to a machine you name and runs the target in that
machine's container engine (`scripts/remote.sh`). The machine needs `make`, `git`, `tar`
and a container engine, and `ssh` must work without a prompt:

```sh
make remote-check REMOTE=user@build-host
make remote-test-chaos REMOTE=user@build-host REMOTE_DIR=cactusfs-src
```

`REMOTE_DIR` is where the copy is put (default `cactusfs-src`, in the home directory of
that user); `REMOTE_VARS="NAME=value ..."` passes make variables to that side. No
address of any machine is kept in this repository.

Notes:

- The builder reaches the container engine through its socket: `/var/run/docker.sock`
  by default, and with `ENGINE=podman` Podman's API socket - `/run/podman/podman.sock`
  for root, `$XDG_RUNTIME_DIR/podman/podman.sock` rootless - which `podman.socket`
  must be serving (`systemctl start podman.socket`, or `systemctl --user` rootless).
  `ENGINE_SOCKET=...` names another. Under `ENGINE=podman` a target that needs the
  engine (`check`, `image`, `image-binaries`, `test-int`, `verify-clone` and the
  others listed as `ENGINE_SOCKET_TARGETS` in the `Makefile`) stops before it starts
  when that socket is not there - `XDG_RUNTIME_DIR` unset included - and says so.
  `make test-int` (within `make check`) pauses a container (`TestSiteReplication`), which
  rootless Podman can do only on cgroup v2 with the `freezer` controller delegated to the
  user: `cat /sys/fs/cgroup/user.slice/user-$(id -u).slice/cgroup.controllers` must list
  the controllers the test containers use, and `podman info` must show
  `cgroupVersion: v2`.
- The builder runs as root inside its container. With rootful Docker on Linux, files it
  creates (`bin/`, `dist/`, `web/dist/`) are owned by root; `make clean` removes them.
- On a shared machine the builder can be held to a part of it:
  `make check BUILDER_LIMITS=--cpus=4,--memory=6g GO_P=2`.

## Verifying an image

**`v0.7.0` is the one release so far, and only its image and chart were published**
([ADR-0021](docs/adr/0021-sbom-and-signatures.md)): they verify as below. Its GitHub
Release was not created, so it has no binaries, `SHA256SUMS` or `digests.txt` to verify
- read the digest from the registry, and trust cosign's verdict on it.

A release is published to `ghcr.io/albievan/cactusfs` and signed **keyless** with
[cosign](https://github.com/sigstore/cosign) (Sigstore): there is no release key to
fetch or to trust. A signature carries a short-lived certificate that names who signed
- the release workflow of this repository, at the tag of the release - and is recorded
in the public transparency log (Rekor). You verify that identity:

```sh
VERSION=0.7.0                 # the release, without the leading v
IDENTITY=https://github.com/albievan/cactusfs/.github/workflows/release.yml@refs/tags/v$VERSION
ISSUER=https://token.actions.githubusercontent.com

# the image: the multi-arch manifest list, or the image of one platform - BY DIGEST
cosign verify --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" \
  ghcr.io/albievan/cactusfs@sha256:<digest>

# what is in it: the SPDX SBOM of a platform image, attached to it and signed
cosign verify-attestation --type spdxjson \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" \
  ghcr.io/albievan/cactusfs@sha256:<digest of the platform image> |
  jq -r '.payload | @base64d | fromjson | .predicate.packages[].name'

# the Helm chart, an OCI artifact
cosign verify --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" \
  ghcr.io/albievan/charts/cactusfs@sha256:<digest of the chart>

# the binaries: the checksums are signed, the binaries are checked against them
cosign verify-blob --new-bundle-format=true --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
```

- **Verify a digest and deploy that digest** (`image.digest` in the chart's values): a
  tag can be moved, a digest cannot. A signature says "this digest is a release", so a
  tag moved to another *released* image still verifies; only a digest pins which one
  runs. The digests are in the notes of the GitHub Release and in its `digests.txt` -
  which is a convenience and is not itself signed: what you trust is cosign's verdict
  on the digest, not the file you read it from.
- Give the identity **exactly**, with the version you mean. A pattern
  (`--certificate-identity-regexp`) that accepts any tag accepts any release; one that
  accepts any workflow or any ref accepts whatever anybody with write access can run.
- The licence the SBOM declares for CactusFS itself is `LicenseRef-CactusFS-Blessing`,
  with the text of [LICENSE.md](LICENSE.md) in the document: the dedication is in the
  project's own words and is not a licence of the SPDX list.
- **Without the internet.** Keyless verification asks Sigstore's public services for
  the roots of trust and the log. For an air-gapped network, verify once where the
  internet is reached and carry the *digest* across; mirror the image together with its
  signature and attestation (`cosign copy`), and verify inside with Sigstore's trusted
  root brought in as a file (`cosign verify --trusted-root <file> ...`, with the same
  identity and issuer) - or sign the digest again with a key of your own, which
  verifies with nothing but that key. CactusFS itself never calls out: it verifies
  nothing at run time.

`make sbom` and `make sign-verify` (both in `make check`) prove the procedure on every
change, without the network: against a throw-away registry and with a key made for the
run, including that an unsigned image, a moved tag, another key and a signature or SBOM
copied from another image are refused. `scripts/release.sh --dry-run`, which that gate
runs, rehearses a release with the same code that publishes one. The keyless path
itself - the workflow's identity, the public log - runs only in a real release: it
signed the image and chart of `v0.7.0`, and failed after that on a permission of the
Sigstore bundle of `SHA256SUMS`, which the dry run could not catch (fixed since).

## Releases

`v0.7.0` (2026-11-13) is the one release so far; its image and chart are published, and
its GitHub Release is not (see [What is not there yet](#what-is-not-there-yet)). A
published tag is never moved: the binaries ship with the next release. This is how one
is made.

A release is made by the workflow [`release.yml`](.github/workflows/release.yml), by a
pushed tag, and by nothing else: `scripts/release.sh --publish` refuses outside that
workflow, on anything but the tag `v<version>` of a clean commit whose tree names that
version, and there is no manual trigger.

1. Name the release in the three places that hold it, in one commit on `main`:
   `internal/buildinfo/VERSION`, the default of `ARG VERSION` in the `Dockerfile` and
   `appVersion` in `deploy/helm/cactusfs/Chart.yaml` (`go test ./internal/buildinfo` and
   `make helm-lint` fail when they differ).
2. When CI is green on that commit: `git tag v<version> && git push origin v<version>`.

The workflow then checks that the tag is `v` + the tree's version and that the commit is
on `main`, runs `make check`, `make test-chaos` and `make test-rolling`, builds, and
publishes:

| What | Where |
|---|---|
| the image, `linux/amd64` + `linux/arm64` | `ghcr.io/albievan/cactusfs:<version>` and `:<version>-<build>` (one manifest list) |
| the image of one platform | `ghcr.io/albievan/cactusfs:<version>-<build>-amd64`, `-arm64` |
| the Helm chart, with the release as its version | `oci://ghcr.io/albievan/charts/cactusfs`, version `<version>` |
| the six binaries, `SHA256SUMS` and its signature `SHA256SUMS.sigstore.json`, the SBOMs (`*.spdx.json`), `digests.txt` | the GitHub Release `v<version>` |

The manifest list, the image of each platform and the chart are signed by digest; each
platform image has its SBOM attached as a signed attestation; the workflow verifies all
of it before it creates the GitHub Release. There is no `latest` tag: name a version,
and deploy its digest.

**A version is released once.** The tag `<version>` is written last, and only where the
registry says that there is none. A run that is started again and builds the very same
manifest list is finished; one that builds another list is refused (exit 20) and the tag
stays where it is - release the next version. If the registry cannot be asked, nothing
is written either (exit 21).

```sh
helm install cactusfs oci://ghcr.io/albievan/charts/cactusfs --version <version> \
  -n cactusfs -f my-values.yaml \
  --set image.digest=sha256:<digest of the manifest list>
```

The chart's default image is `ghcr.io/albievan/cactusfs`, and without a digest or a tag
it runs `:<version>`, the chart's `appVersion`; the digest pins which image that is.

## Layout

```
cmd/cactusfs/        the binary: composition root, settings
internal/domain/     entities, value objects, domain errors (standard library only)
internal/app/        use cases
internal/ports/      interfaces to the outside world
internal/adapters/   implementations and inbound transports
internal/buildinfo/  the version, build and commit of a binary
api/                 OpenAPI, AsyncAPI and protobuf contracts, and the code generated from them
web/                 console (React + TypeScript + Vite)
deploy/              the deployment script (deploy.sh, lib/, .env.example), compose cluster, Helm chart,
                     observability pack, the example environment file
test/                integration, S3 compatibility, chaos, Kubernetes and deployment-script suites
scripts/             the gate scripts behind the make targets
docs/                architecture, threat model, decision records, brand assets
```

## Documentation

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) - the design, the settings and the metrics
- [`docs/threat-model.md`](docs/threat-model.md) - what is protected, from whom, what is
  accepted, and what is still open
- [`docs/adr/`](docs/adr/README.md) - the decisions that are expensive to reverse
- [`deploy/helm/cactusfs/README.md`](deploy/helm/cactusfs/README.md) - the chart
- [`deploy/observability/README.md`](deploy/observability/README.md) - alerts and dashboards
- [`docs/load-balancing.md`](docs/load-balancing.md) - a load balancer in front of
  CactusFS: with compose on Docker Desktop, for a cluster on virtual machines, and on
  k3s or Kubernetes; examples in
  [`deploy/examples/loadbalancer/`](deploy/examples/loadbalancer/README.md)
- [`PLAN.md`](PLAN.md) - milestones, steps, their status and the day-to-day decisions
- [`CLAUDE.md`](CLAUDE.md) - the engineering rules the code is held to

## Security

Read [`docs/threat-model.md`](docs/threat-model.md) before you trust CactusFS with
anything: it names what each control does, what is accepted as a risk, and what is not
mitigated.

To report a vulnerability, please do not open a public issue. Use GitHub's private
vulnerability reporting for this repository (the **Security** tab → **Report a
vulnerability**), where the maintainer has enabled it.

## Licence

CactusFS is dedicated to the public domain by its authors, in the way SQLite is, with a
blessing in place of a legal notice: see [LICENSE.md](LICENSE.md), which is the text
that counts and also says what the dedication does not cover and what a contributor
agrees to. The Go modules and the console packages CactusFS is built with keep their
own licences (MIT, BSD, ISC, Apache-2.0 or MPL-2.0); a binary or an image contains
them, and their notices and conditions go with it. The console's are in one file, made
when the console is built from the packages installed for it: every package's name,
version and licence with the text of its licence and notice files, then those of the
two pdf.js decoders shipped beside the bundle. A binary built with the console serves it
at `/notices.txt` on the console's port (9001); the build fails if a package's licence is
not on the list, a package has no licence or notice file, or the bundle holds code of a
package that is not a production one; `make licences` fails if the file is not what the
build of the installed tree gives (it is generated deterministically).

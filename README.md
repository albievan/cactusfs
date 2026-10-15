# CactusFS

An S3-compatible, erasure-coded, multi-node object store in a single Go binary, with a
web console. Data security comes first: encryption at rest that cannot be switched off,
TLS on every listener, checksums verified on every read, self-healing, role-based access
control and a hash-chained audit trail.

> **Status: pre-release, version 0.7.0. No release has been published yet.**
> Milestones 0 to 7 of [`PLAN.md`](PLAN.md) are done: the standalone store, erasure
> coding, the cluster, identities and policies, the management API with its console, the
> S3 depth listed below, and deployment hardening (the Helm chart, certificate rotation,
> the observability pack, SBOMs, signatures and the release workflow). Milestone 8
> (replication between sites, bucket notifications, quotas) is begun: its metadata
> schema (entry schema 5) and its contracts exist, and none of its features does yet.
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
- [The S3 API](#the-s3-api)
- [Management API, documentation and console](#management-api-documentation-and-console)
- [Users, keys and the audit trail](#users-keys-and-the-audit-trail)
- [Operations](#operations)
- [Versions and builds](#versions-and-builds)
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
- **Management API** (REST/JSON, designed first in OpenAPI) with Swagger UI, and a **web
  console** embedded in the binary: buckets as a grid with their creator, policy and
  latest use; an object browser with upload, download, an object's details opened in
  its own row, and who uploaded and how often an object was downloaded; uploads that go
  on while you use other screens, with a bar at the top of every screen that shows them
  and what the console did; search; users, groups, roles, policies and API keys; the
  cluster; the settings of every node; the audit log. A bucket's page shows its details
  and can delete every object, after asking for your password. A session that times out
  while it uploads is locked, not ended, until your password unlocks it.
- **Deployment.** A distroless, non-root container image; a four-node compose cluster
  for development (Docker or rootless Podman); a Helm chart for Kubernetes and k3s with
  a default-deny NetworkPolicy under the `restricted` Pod Security Standard.
- **Observability.** JSON logs, Prometheus metrics (`cactusfs_*`) behind authentication,
  OpenTelemetry traces to a collector you name, and a pack of alerting rules and Grafana
  dashboards in [`deploy/observability`](deploy/observability/README.md).
- **Supply chain.** Base images pinned by digest, a licence allow-list, SPDX SBOMs, and
  images signed by digest - proved offline in the gate; a release workflow that signs
  keyless, written and not yet run.
- **Air-gap safe.** The binary calls nothing on the internet, sends no telemetry and
  serves no asset from a CDN.

## What is not there yet

Said plainly, so that nobody finds out by trying:

- **No release has been published.** There is no image at `ghcr.io/albievan/cactusfs`
  and no chart at `oci://ghcr.io/albievan/charts` yet; the commands that name them apply
  from the first release. Until then, build the image yourself.
- **Milestone 8: replication between sites, bucket notifications, quotas.** None of the
  three works. Notification targets can be defined, read, listed and deleted
  (`/targets`, with their secrets sealed and never shown) - nothing sends to them yet,
  and their status answers `501`. A bucket's quota, notifications, replication and
  replica mode are in the contract of the management API and answer
  `501 Not Implemented`. Events are published inside the process only;
  there is no NATS or webhook delivery. Nothing limits how much a bucket or a tenant
  stores.
- **Planned, not built:** the console's preview of DOCX and XLSX objects (Goal O: images, text, CSV, JSON and PDF are previewed in a dialog of the object's opened row; those two kinds offer only a download yet); a visual builder of policies in the console (milestone 9). Today a policy is edited as JSON, and a
  bucket's policy through the S3 API only.
- **Sub-buckets** ([ADR-0017](docs/adr/0017-sub-buckets.md)). The rules of the domain,
  the port and the replicated store exist. No use case, no API and no console screen
  does: the authorizer does not read a sub-bucket's policy, and nothing can create one.
- **Tag rules, lower-cased tags and a catalogue of tags**
  ([ADR-0018](docs/adr/0018-tag-rules-and-catalogue.md)). The same: domain and store
  only. Tags of buckets and objects as S3 has them do work; tags given with a multipart
  upload answer `501`.
- **Growing a cluster.** The voters and the storage of a cluster are fixed when it
  forms: storage nodes and drives cannot be added afterwards, and a node cannot be given
  a vote later. Gateways can join.
- **Single sign-on.** Console users are local accounts. The port for another identity
  provider exists (`ports.IdentityProvider`); no OIDC adapter does.
- **Parts of S3:** ACLs, conditional writes (`If-None-Match` on a PUT), replication,
  notifications, inventory, analytics, website hosting, storage classes with a parity of
  their own. What is not implemented answers `501 NotImplemented`.
- **Metadata is not encrypted at rest.** Object data is; bucket names, object keys,
  sizes and user metadata are in the metadata store in the clear
  ([threat model](docs/threat-model.md)).
- **No rotation of the local KMS key**, and no revocation list for node certificates.
- **A rolling upgrade is tested with the binaries themselves for two steps only**: from
  entry schema 3 to 4 and from 4 to 5 (`make test-rolling`). Older states are tested by
  opening fixtures that the older binaries wrote.
- **Entry schema 5 is in the metadata and nothing uses it yet.** A cluster of this
  release counts what every new bucket holds, in its replicated state, and knows quotas,
  targets, a delivery outbox and replica buckets there; only targets can be set through
  the API yet, and a bucket that existed before the upgrade is not counted until a later
  release counts it. Neither is a bucket created through a node that had not yet seen
  that every node knows schema 5 - in the moment after the last node's upgrade, or
  while a node is not registered: it is written as before, and has no usage record
  until that later release counts it.

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
start (both, or neither).

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
  set in `CACTUSFS_ROOT_ACCESS_KEY` and `CACTUSFS_ROOT_SECRET_KEY` (with the chart: the
  Secret named in `secrets.root`, keys `access-key` and `secret-key`). It works for S3,
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
| `--tls-reload-interval` | `CACTUSFS_TLS_RELOAD_INTERVAL` | `1m` | how often the certificate and key files, the node's too, are read again (at least `5s`; `0` never reads them again). A new pair that is valid is used for new connections without a restart; one that is not is refused and logged, and the old one stays (`cactusfs_tls_reload_pending` in `/metrics` is 1 while it does). The node CA is read at start only: a new CA needs a restart. Rotation is not revocation: a replaced certificate and its key remain accepted by peers until that certificate expires; for a compromised key, rotate the CA or remove the node. The files must be regular files (not pipes) |
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
| `--egress-allow` | `CACTUSFS_EGRESS_ALLOW` | none | ranges (CIDRs, separated by commas, such as `127.0.0.1/32`) where a notification target or another site may be at an address that is otherwise never connected to: loopback, link-local, and the IPv6 ranges that stand for IPv4 addresses by a gateway's rule (`::/96`, `64:ff9b:1::/48`, `2002::/16`, `::ffff:0:0:0/96`). Private ranges need no entry. A range of every address (`0.0.0.0/0`, `::/0`) and one that could admit nothing (`0.0.0.0/8`, multicast) are refused. The addresses of the metadata services of clouds (169.254.169.254, `fd00:ec2::254`, `fd20:ce::254`, 100.100.100.200) are never connected to, whatever this holds; a range that is allowed is open with everything else in it, so allow the one address a receiver has. The address a name resolves to is judged at every connect. It has no effect yet: it is read by bucket notifications and replication, which come with PLAN 8B and 8C. The same on every node |
| `--delivery-timeout` | `CACTUSFS_DELIVERY_TIMEOUT` | `30s` | how long a notification target or another site has to answer one request, once it is sent, and to make progress reading or writing (`1s` to `5m`); one that does not is tried again later. Connecting and the TLS handshake have 5 seconds each besides. It has no effect yet: it is read by bucket notifications and replication, which come with PLAN 8B and 8C. The same on every node |
| `--replication-workers` | `CACTUSFS_REPLICATION_WORKERS` | `4` | how many versions are replicated to other sites at once (`1` to `32`). It has no effect yet: it is read by bucket notifications and replication, which come with PLAN 8B and 8C. The same on every node |
| `--replication-rate-mib` | `CACTUSFS_REPLICATION_RATE_MIB` | `0` | limit on the bytes replication sends to other sites, in MiB per second averaged over 10 seconds (at most `1048576`); `0` is no limit. It has no effect yet: it is read by bucket notifications and replication, which come with PLAN 8B and 8C. The same on every node |

**Maintenance: background work**

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--scrub-interval` | `CACTUSFS_SCRUB_INTERVAL` | `24h` | pause between two passes of the scrubber, which verifies and heals every object; 0 disables it. The same on every node |
| `--scrub-rate-mib` | `CACTUSFS_SCRUB_RATE_MIB` | `64` | limit on how fast the scrubber reads, in MiB per second; 0 is no limit. The same on every node |
| `--lifecycle-interval` | `CACTUSFS_LIFECYCLE_INTERVAL` | `1h` | pause between two passes of the worker that carries out the lifecycle rules of buckets; 0 disables it. The same on every node |
| `--gc-interval` | `CACTUSFS_GC_INTERVAL` | `1h` | pause between two passes of the garbage collector for unreferenced shards; 0 disables it |
| `--gc-grace` | `CACTUSFS_GC_GRACE` | `24h` | minimum age of an unreferenced shard before it is removed (at least `1h`). The same on every node |
| `--access-flush-interval` | `CACTUSFS_ACCESS_FLUSH_INTERVAL` | `30s` | how often the downloads this node counted are recorded with the objects, at least 5s; a node that crashes loses up to this much of the count |

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
`--kms-key-file`, `--kms-passphrase-file`, `--vault-token-file`) stay outside the
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
container engine with its compose plugin; the image is built from this tree.

```sh
export CACTUSFS_KMS_PASSPHRASE="$(openssl rand -hex 24)"   # keep it: it opens the cluster's key
export CACTUSFS_ROOT_ACCESS_KEY=…  CACTUSFS_ROOT_SECRET_KEY=…   # or find a generated pair in the log, below
make up                                                     # or: make up ENGINE=podman
docker compose -f deploy/compose/compose.yaml cp node1:/var/lib/cactusfs/config/s3-ca.pem .
aws --endpoint-url https://localhost:9000 --ca-bundle s3-ca.pem s3 mb s3://photos
aws --endpoint-url https://localhost:9030 --ca-bundle s3-ca.pem s3 ls        # another node, same answer
make down                                                   # stops it; the volumes, and so the data, stay
```

The passphrase must be the same at every start. Without root credentials in the
environment one node generates a pair and prints it once, into the container log:
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
values into the environment of the nodes, which read no file themselves; a variable
exported in the shell wins over the file, and the secrets are exported, not written into
it. Each node is told the name of the volume behind each of its drives
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
- **Keep the cluster on one site.** Sites are to be linked by replication (milestone 8,
  not built), not by stretching the metadata group across a WAN.

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

**With an image you built.** No release is published yet, so this is the way that works
today. Build the image (`make image`, or `docker build`), push it to a registry your
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

**With a published release** - from the first release on, see [Releases](#releases):
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
time - with this release, one bucket created by anybody, through any node - is an entry
the node taken back cannot apply. There is no switch that closes the cluster to such
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
| entry schema 4 (this release raises it to 5) | **the first bucket that is created** after the last node was upgraded - through S3 or the management API, on any node. Uploads, downloads, deletes and multipart uploads into the buckets you have do not end it. A node that runs alone has no other node to wait for: its first new bucket ends it |

A later release of milestone 8 will count the buckets you already have by itself, within
seconds of the last node's upgrade, and the way back from schema 5 will then end there.

Two things about the machine, which are not the chart's to do:

- **firewalld.** On a host that runs firewalld, k3s needs its pod and service networks
  in the trusted zone, or pods cannot reach each other and the cluster never forms.
  With the networks k3s uses unless told otherwise:
  `firewall-cmd --permanent --zone=trusted --add-source=10.42.0.0/16 --add-source=10.43.0.0/16 && firewall-cmd --reload`.
- **NodePorts are published past the host firewall.** A Service of type NodePort
  answers on every address of the machine, whatever firewalld says. The admin port has
  a Service of its own so that S3 can be published without it; remember the console
  user `admin` / `admin` before you publish that one.

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
| Lifecycle rules | expire objects, remove old versions, abort unfinished multipart uploads; carried out by the node that leads, every `--lifecycle-interval` |
| Server-side encryption | always on. SSE-S3 (the default, and `AES256`), SSE-KMS (`aws:kms`, with an encryption context) and SSE-C (a key of the customer's, over TLS and in signed headers only) |
| Payloads | literal, `UNSIGNED-PAYLOAD` and streaming, with `Content-MD5` and CRC32, CRC32C, CRC64NVME and SHA-256 checksums, verified before anything is stored |

Everything else - ACLs, conditional writes, replication, notifications and the rest -
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
| `/buckets`, `/buckets/{bucket}`, `/buckets/{bucket}/usage` | list, create, read and delete buckets; how many objects a bucket holds and their size |
| `/buckets/{bucket}/objects`, `/buckets/{bucket}/object`, `/object/metadata`, `/object/access` | list objects; upload, download and delete one; its metadata; who uploaded it and how often it was downloaded |
| `/buckets/{bucket}/object/preview` | an object's bytes for the console to show it (ADR-0028): PNG, JPEG, GIF, WebP, PDF, DOCX, XLSX, text, CSV and JSON, decided by the stored type and the bytes together; for whoever may read the object and either uploaded the version shown or is allowed `admin:PreviewObject`; always an attachment, never served inline; up to `--preview-max-size`, text to its first MiB; counted as a download |
| `/search` | buckets by name and objects by key |
| `/users`, `/users/{name}`, `/users/{name}/keys`, `/keys/{access_key_id}` | users and their API keys |
| `/groups`, `/roles`, `/policies` (and `/{name}` of each) | groups, roles, policies |
| `/cluster`, `/cluster/maintenance` | nodes, drives and erasure sets; healing, scrubbing and collection |
| `/settings` | the settings of every node |
| `/audit` | the audit trail |
| `/targets` and `/targets/{name}` | milestone 8: define, read, list and delete a notification target; its secret and signing key sealed through the KMS, the key shown once; answers `503` until every member counts in schema 5 |
| `/buckets/{bucket}/quota`, `/buckets/{bucket}/usage/recount`, `/buckets/{bucket}/notifications`, `/buckets/{bucket}/replication` (with `/status` and `/resync`), `/buckets/{bucket}/replica`, `/targets/{name}/status` | milestone 8: in the contract, and every one answers `501` - nothing of them is built yet |

The console has a screen for each: **Buckets** (a grid), the object browser of a
bucket, **Search** (the field at the top of the navigation), **Users** (with a user's
API keys), **Groups**, **Roles**, **Policies**, **Cluster**, **Settings** and **Audit
log**. It talks only to the management API and decides nothing itself. The version and
build it runs are shown under the logo.

- **Uploads go on across screens.** Files dropped on a bucket are sent while you use
  any other screen. A bar at the top of every screen, on a phone too, shows how many
  files are on their way and how far they are, and when they are done what was stored
  and what was not, until you dismiss it. The same bar keeps notices of what the
  console did - newest first, for the visit, not stored. Leaving or reloading the page
  while files are being sent asks first.
- **An object's details in its row.** In the object browser a click on an object's
  name opens its details - key, size, ETag, last modified, content type, encryption,
  and its uploads and downloads where you may see them - directly under its row.
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
nothing of its creator.

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

## Users, keys and the audit trail

Access is decided by IAM-style policies (Effect, Action, Resource, Condition; `s3:*` and
`admin:*` actions): users belong to groups, users and groups hold roles, roles are sets
of policies, and an API key belongs to a user and can never do more than its owner. Five
roles are built in: `cluster-admin`, `bucket-admin`, `read-write`, `read-only` and
`auditor`. Nothing is allowed unless a policy allows it, and an explicit deny wins - with
one exception (ADR-0028): the user who uploaded a version of an object may preview it in
the console without being given `admin:PreviewObject`, when they may also read the
object, and unless a policy denies them the preview.

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

Every change to identities, every refusal and every failed sign-in is written to a
hash-chained audit trail (`GET /api/v1/audit`, the console's Audit log);
`--audit-export` also writes each record to standard output as a JSON line.

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
cactusfs 0.7.0 build 412 (commit 330028134523, go1.27.2 linux/amd64)
$ go build ./cmd/cactusfs && ./cactusfs version
cactusfs 0.7.0 local build (commit 330028134523-dirty, go1.27.2 darwin/arm64)
```

| Where | What it says |
|---|---|
| `cactusfs version` | version, build, commit, Go toolchain, platform |
| the log, when a listener opens | `"version"`, `"build"`, `"commit"` |
| `cactusfs_build_info` in `/metrics`, `service.version` in traces | `<version>-<build>` |
| the management API | `server {version, build, commit}` in the answers of `POST /session`, `GET /session` and `GET /cluster` - to a caller who has been authenticated, and to nobody else: no sign-in page, probe, refusal or response header names it. `GET /settings` names the build of every node |
| the console | `v0.7.0 · build 412` under the logo of the navigation, the commit in its hint (`local build` for build `0`); beside the logo on a phone; not shown while the navigation is folded to icons |
| an image of `make image` | tag `cactusfs:<version>-<build>-<arch>`; labels `org.opencontainers.image.version=<version>-<build>` and `org.opencontainers.image.revision=<commit>` |
| a deployment with the chart | `image.tag=<version>-<build>` names the build to run, of `image.repository` (by default `ghcr.io/albievan/cactusfs`, where releases will be published); pods and the chart's other objects are labelled `app.kubernetes.io/version` with it. Without a tag the chart runs `appVersion`, the release |

`GET /cluster` says the build of the node that answered. During a rolling upgrade, ask
each node, read the Settings screen, or read the label:
`kubectl -n cactusfs get pods -L app.kubernetes.io/version`.

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
| `make licences` | every dependency, of the binary and of the console, on the allow-list |
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
(both upgrades) and `make test-k8s` as jobs of their own, and the unit tests on Linux, macOS and
Windows.

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

- The builder reaches the container engine through its socket (`/var/run/docker.sock` by
  default). For rootless Podman:
  `make check ENGINE=podman ENGINE_SOCKET=$XDG_RUNTIME_DIR/podman/podman.sock`.
- The builder runs as root inside its container. With rootful Docker on Linux, files it
  creates (`bin/`, `dist/`, `web/dist/`) are owned by root; `make clean` removes them.
- On a shared machine the builder can be held to a part of it:
  `make check BUILDER_LIMITS=--cpus=4,--memory=6g GO_P=2`.

## Verifying an image

**No release has been published yet.** Everything below applies from the first one;
until then the names and commands are what the release workflow will produce, and
nothing is there to verify ([ADR-0021](docs/adr/0021-sbom-and-signatures.md)).

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
itself - the workflow's identity, the public log - has not run: it cannot before the
first release.

## Releases

**No release has been made yet.** This is how one is made.

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
deploy/              compose cluster, Helm chart, observability pack, the example environment file
test/                integration, S3 compatibility, chaos and Kubernetes suites
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
them, and their notices and conditions go with it.

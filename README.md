# CactusFS

An S3-compatible, erasure-coded, multi-node object store in a single Go binary, with
data security first: encryption at rest and in transit, checksums on every read,
self-healing, RBAC and an audit trail.

> **Status: milestone 2 (erasure coding on local drives).** One node stores objects
> encrypted, checksummed and — with four or more drives — erasure coded, heals itself,
> and speaks an S3-compatible API with SigV4. There is no cluster yet (milestone 3) and
> no users or policies beyond the root key (milestone 4): a node is a single point of
> failure, so do not keep data here that you cannot lose. `PLAN.md` lists what comes
> next.

## Build from source

You need **one** of the following. Nothing else has to be installed.

### With Go alone

Requires the Go version named in `go.mod` (older Go toolchains download it
automatically).

```sh
git clone https://github.com/albievan/cactusfs.git
cd cactusfs
go build ./cmd/cactusfs
./cactusfs version
```

This builds on Linux, macOS and Windows, for amd64 and arm64, without cgo.

### With a container engine alone

Requires Docker or Podman.

```sh
git clone https://github.com/albievan/cactusfs.git
cd cactusfs
docker build -t cactusfs .        # or: podman build -t cactusfs .
docker run --rm cactusfs version
```

The image is distroless, runs as a non-root user and has no shell.

The image, and the binary of `make build`, carry the web console; a binary built with
plain `go build` serves the APIs and says that it has no console.

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
| the management API | `server {version, build, commit}` in the answers of `POST /session`, `GET /session` and `GET /cluster` - to a caller who has been authenticated, and to nobody else: no sign-in page, probe, refusal or response header names it |
| the console | `v0.7.0 · build 412` under the logo of the navigation, the commit in its hint (`local build` for build `0`); beside the logo on a phone; not shown while the navigation is folded to icons |
| an image of `make image` | tag `cactusfs:<version>-<build>-<arch>`; labels `org.opencontainers.image.version=<version>-<build>` and `org.opencontainers.image.revision=<commit>` |
| a deployment with the chart | `image.tag=<version>-<build>` names the build to run; pods and the chart's other objects are labelled `app.kubernetes.io/version` with it. Without a tag the chart runs `appVersion`, the release |

`GET /cluster` says the build of the node that answered. During a rolling upgrade, ask
each node, or read the label: `kubectl -n cactusfs get pods -L app.kubernetes.io/version`.

## Run

A node needs a drive, a key management service and TLS. For local development,
`--dev-plaintext` replaces TLS with HTTP on a loopback address only:

```sh
export CACTUSFS_KMS_PASSPHRASE='choose a long passphrase'
./cactusfs server --dev-plaintext --data ./.data --kms local
```

On its first start a deployment prints generated root credentials (an API key)
**once** on standard error — store them — or uses the ones you supply in
`CACTUSFS_ROOT_ACCESS_KEY` and `CACTUSFS_ROOT_SECRET_KEY`.

A new deployment also has one console user: **`admin`, password `admin`**, with every
right. **Delete that user, or change its password, before anybody else can reach the
admin port** (console → Users). The user is made once: after you change or delete it,
it never comes back. Deleting is the surer of the two: it ends the user's sessions and
removes its API keys, while a changed password leaves a session that is already open
valid until it expires (30 minutes) and leaves in place whatever was made through it.
The compose cluster publishes the admin port on `127.0.0.1` only for this reason.

```sh
export AWS_ACCESS_KEY_ID=…  AWS_SECRET_ACCESS_KEY=…  AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://127.0.0.1:9000 s3 mb s3://photos
aws --endpoint-url http://127.0.0.1:9000 s3 cp cactus.jpg s3://photos/
aws --endpoint-url http://127.0.0.1:9000 s3 ls s3://photos/
```

Give it four or more drives and objects are erasure coded: six drives store every object
as three data and three parity shards and survive the loss of any three. With fewer than
four drives there is no parity, and the node says so. All drives must be present for the
node to start.

To replace a drive that died, stop the node, put an empty drive in its place and start
once with `--allow-drive-replacement`. The node will not do this on its own, because an
empty directory is also what a missing mount looks like, and a replacement is permanent:
the old drive is refused afterwards. The new drive is refilled in the background. One
drive at a time, and only with four or more drives — without parity nothing can be
rebuilt.

Keep `--meta-dir` (default `<first drive>/meta`) with the drives it describes. A node
started with drives that hold data and a metadata directory that knows nothing of them
refuses to start rather than treat the data as garbage.

In production, give it a certificate; it refuses to start without one:

```sh
./cactusfs server --data /mnt/drive1,/mnt/drive2,/mnt/drive3,/mnt/drive4 --kms local \
  --tls-cert server.crt --tls-key server.key
curl --cacert ca.crt https://localhost:9001/readyz      # {"status":"ok"}
```

**Back up the KMS key file (`<first drive>/meta/kms-local.json` by default) and its
passphrase.** Without both, nothing stored can be decrypted.

### Settings

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

#### An environment file

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
a value may hold `=`, spaces, `$` or `#`. What none of them does: take quotes off a value (a quote becomes part
of it), read a value over several lines, drop a comment after a value, or accept
`export NAME=value` or spaces before the name. Write values without quotes and the file
reads the same everywhere. A systemd `EnvironmentFile=` does take quotes off, and so does
a shell that sources the file (`set -a; . file; set +a`), which besides runs what follows
a space in a value as a command and expands `$`: use the loop.

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

Operations:

- `GET /healthz` and `GET /readyz` on the admin listener are unauthenticated and say only
  `ok` or `unavailable`.
- `GET /metrics` needs `Authorization: Bearer <access key ID>:<secret>`.
- Logs are JSON on stdout. On SIGTERM the node makes `/readyz` answer 503, keeps serving
  for the drain delay, closes its listeners, lets in-flight requests finish and exits 0.

Supported S3 operations: ListBuckets, CreateBucket, HeadBucket, DeleteBucket,
GetBucketLocation, PutObject, GetObject (with a byte range), HeadObject, DeleteObject,
DeleteObjects, ListObjectsV2, bucket versioning and object versions, Object Lock
(retention, legal hold, default retention), multipart uploads (create, upload part,
complete, abort, list parts, list uploads), SSE-KMS and SSE-C, and presigned URLs, by path or (with
`--s3-domain`) by virtual host; bucket policies (which alone can open a bucket to
anonymous readers), CORS rules, tags of buckets and objects, and lifecycle rules that
expire objects, old versions and unfinished uploads; CopyObject and UploadPartCopy,
ListObjects (version 1) and conditional reads (`If-Match` and its kin). ACLs, conditional
writes and the rest
answer `501 NotImplemented` for now.

In a container, mount the drives and the certificate and run hardened:

```sh
docker run --rm --read-only --cap-drop ALL -p 9000:9000 -p 9001:9001 \
  -v cactus-data:/var/lib/cactusfs/data1 -v "$PWD/certs:/certs:ro" \
  -e CACTUSFS_DATA=/var/lib/cactusfs/data1 -e CACTUSFS_KMS=local -e CACTUSFS_KMS_PASSPHRASE \
  -e CACTUSFS_TLS_CERT=/certs/server.crt -e CACTUSFS_TLS_KEY=/certs/server.key \
  cactusfs
```

The image runs as UID 65532. Its directories under `/var/lib/cactusfs` (`data1` to
`data4`, `meta`, `config`) belong to that user, so a named volume mounted on one of them
is writable; a volume mounted anywhere else, or a bind mount, must be made writable for
UID 65532 by you.

### Keys in Vault

With `--kms vault` the KMS key never leaves HashiCorp Vault: the node asks Vault's Transit
engine to wrap and unwrap the data keys of objects and secrets.

```sh
vault secrets enable transit
vault write -f transit/keys/cactusfs type=aes256-gcm96      # an AEAD type is required
vault policy write cactusfs - <<'POLICY'
path "transit/encrypt/cactusfs" { capabilities = ["update"] }
path "transit/decrypt/cactusfs" { capabilities = ["update"] }
POLICY
vault token create -policy=cactusfs -no-default-policy -field=token > /run/secrets/vault-token

cactusfs server --data /mnt/d1 --kms vault --vault-addr https://vault.example:8200 \
  --vault-key cactusfs --vault-token-file /run/secrets/vault-token --tls-cert s3.pem --tls-key s3.key
```

Vault must be reached over HTTPS. The token file is read at every use, so an agent can
renew it in place; the node does not renew it itself. If Vault cannot be reached the
node does not start, and has no fallback.

### Users, keys and the audit trail

Access is decided by IAM-style policies: users belong to groups, users and groups hold
roles, roles are sets of policies, and an API key belongs to a user and can never do more
than its owner. Five roles are built in: `cluster-admin`, `bucket-admin`, `read-write`,
`read-only` and `auditor`. Every change to identities, every refusal and every failed
sign-in is written to a hash-chained audit trail; `--audit-export` also writes each
record to standard output as a JSON line.

### Addresses and first sign-in

Everything is served over TLS on two ports of every gateway node. `<host>` is the
machine, or the name your certificate carries.

| What | URL | Sign in with |
|---|---|---|
| S3 API | `https://<host>:9000` | an API key, signed with SigV4 (region `us-east-1` unless set) |
| Console | `https://<host>:9001/` | a console user: **`admin` / `admin`** on a new deployment |
| Swagger UI | `https://<host>:9001/docs` | **Authorize** with `<access key ID>:<secret key>` |
| Management API | `https://<host>:9001/api/v1` | `Authorization: Bearer <access key ID>:<secret key>` |
| API contract | `https://<host>:9001/api/v1/openapi.yaml`, `/api/v1/openapi.json` | nothing |
| Liveness, readiness | `https://<host>:9001/healthz`, `/readyz` | nothing |
| Metrics | `https://<host>:9001/metrics` | an API key allowed `admin:GetMetrics`, as a bearer credential |

Where those ports are, by way of running it:

| Deployment | S3 | Console, Swagger UI, management API |
|---|---|---|
| The binary (`cactusfs server`) | `https://localhost:9000` | `https://localhost:9001` |
| The binary with `--dev-plaintext` | `http://127.0.0.1:9000` | `http://127.0.0.1:9001` |
| Compose cluster (`make up`) | `https://localhost:9000`, `:9010`, `:9020`, `:9030` (nodes 1 to 4) | `https://localhost:9001`, `:9011`, `:9021`, `:9031`, on this machine only |
| Helm chart, inside the cluster | `https://<release>.<namespace>.svc:9000` | `https://<release>-admin.<namespace>.svc:9001` |
| Helm chart, published as NodePorts | `https://<machine>:<service.nodePort>` (`service.type=NodePort`) | `https://<machine>:<adminService.nodePort>` (`adminService.type=NodePort`) |

The two kinds of credentials of a new deployment:

- **Console: user `admin`, password `admin`**, with every right (ADR-0014). The same
  on every new deployment and known to everybody: delete the user or change the
  password first, as said above. It signs in to the console only; it has no API key.
- **API: the root key.** Generated and printed once at the first start, or the pair
  you set in `CACTUSFS_ROOT_ACCESS_KEY` and `CACTUSFS_ROOT_SECRET_KEY` (with the chart:
  the Secret named in `secrets.root`, keys `access-key` and `secret-key`). It works for
  S3, for the management API and in Swagger UI. There is no default API key.

The certificate authority to trust is your own; for the development setups it is
`s3-ca.pem`, which `cactusfs dev-init` writes.

### Management API, documentation and console

The admin listener (`--admin-addr`, port 9001) serves:

- the **management API** under `/api/v1`: buckets and objects, users, groups, roles,
  policies, API keys, cluster status and the audit trail. Authenticate with an API key,
  `Authorization: Bearer <access key ID>:<secret key>`; the root credentials work the
  same way. Errors are `application/problem+json`, and every request that changes
  something can be made safe to repeat with `Idempotency-Key`. For every object it
  tells who uploaded it and when, how often it was downloaded and by whom
  (`GET /api/v1/buckets/{bucket}/object/access?key=`, shown in the console under an
  object's details) - to users with the `cluster-admin` or `auditor` role, or a policy
  that allows `admin:GetObjectAccess`; being allowed to read the object is not enough.
  The listing of a bucket can carry the same five facts for each of its objects - who
  uploaded it and when, its downloads and copies, when it was last downloaded; never by
  whom - (`GET /api/v1/buckets/{bucket}/objects?access=true`, shown in the console as
  columns of the object browser) to those of them who may also list the bucket -
  `cluster-admin` among the built-in roles; an `auditor` may not list a bucket, and asks
  about an object by its key. Whoever may list and may not see the facts gets the plain
  listing, with `access_included: false`.
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
  These are counts kept as well as can be, recorded every `--access-flush-interval`
  (30 s), not an audit trail;
- **search** (`GET /api/v1/search?q=`, and the field at the top of the console's
  navigation): buckets by their name and objects by their key, among the buckets you
  may list. What you type is read in one of three ways. Plain text finds the names that
  contain it, whatever their case (`report`). Text with `*` or `?` is a pattern for the
  whole name, `*` for any run of characters and `?` for exactly one (`*.pdf`). Text that
  begins with `rx:` is a regular expression in the RE2 syntax, case as written
  (`rx:^logs/\d{4}/`). One request looks at a bounded number of keys for a bounded
  time; when it stops short it says so (`complete: false`) and "Search further" - the
  `next_continuation` of the answer - goes on from there. Every page decides again
  which buckets are yours to see, all of them, from the bucket policies as the node
  that answers has them, so what a page costs depends on how many buckets there are
  and not on how many of them are yours. The node remembers what a policy says until
  the policy changes: a page takes some 0.2 s where 5 000 buckets have policies of
  16 KiB each (measured on an Apple M3). You run one search at a time and a node
  eight; beyond that the answer is 429 with `Retry-After`, and the console asks once
  more by itself. **A limit, said plainly:** when deciding the buckets takes the two
  seconds, the answer is 503 with a `detail` that says so. Just after a node has
  started this passes - it remembers nothing yet, reads every policy in full (some ten
  times the work) and gets further with each request. Past about 10 000 buckets with
  policies of 16 KiB each, or 150 000 with policies of 1 KiB, it does not pass:
  search for everybody but the root identity, who is bound by no bucket policy, then
  works only within a bucket you name (`bucket=`);
- its **contract** at `/api/v1/openapi.yaml` and `.json`, and **Swagger UI** at `/docs`,
  served from the binary - nothing is fetched from the internet;
- the **console** at `/`, when the binary was built with it (`make build`, or the
  container image). Sign in with a user that has a password; a binary built with plain
  `go build` says that it has no console.

```
curl --cacert ca.pem -H "Authorization: Bearer $ACCESS_KEY:$SECRET_KEY" \
     -H 'Content-Type: application/json' -H 'Idempotency-Key: make-alice-1' \
     -d '{"name":"alice","roles":["read-write"],"password":"a long password"}' \
     https://localhost:9001/api/v1/users
```

### A cluster

The same binary forms a cluster when it is started with `--peers`: at least four nodes
that store, of which three or five also vote on the metadata (`meta` role). Nodes talk to
each other over mutual TLS 1.3 only, with certificates of a CA that issues node
certificates and nothing else; each certificate carries the node's name as its first DNS
name.

A four-node development cluster on one machine, with Docker or rootless Podman:

```sh
export CACTUSFS_KMS_PASSPHRASE="$(openssl rand -hex 24)"   # keep it: it opens the cluster's key
make up                                                     # or: make up ENGINE=podman
docker compose -f deploy/compose/compose.yaml cp node1:/var/lib/cactusfs/config/s3-ca.pem .
docker compose -f deploy/compose/compose.yaml logs | grep -A5 'generated root credentials'
aws --endpoint-url https://localhost:9000 --ca-bundle s3-ca.pem s3 mb s3://photos
aws --endpoint-url https://localhost:9030 --ca-bundle s3-ca.pem s3 ls        # another node, same answer
make down
```

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
readable as long as two of the three voters are among the survivors. A node that is cut off from the others answers nothing rather than
something stale. What a node missed while it was away is rebuilt on it in the
background.

For nodes on separate machines, give each one its own certificate and the same list:

```sh
cactusfs server --node-id node2 --roles gateway,meta,storage \
  --peers node1=10.0.0.1:9002,node2=10.0.0.2:9002,node3=10.0.0.3:9002,node4=10.0.0.4:9002 \
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
  (`--kms vault`, below). The development cluster shares one sealed key file instead
  (`cactusfs dev-init` writes it, with development certificates).
- **The cluster's storage is fixed when it forms**: storage nodes and drives cannot be
  added afterwards yet.
- **A voter that lost its metadata directory must be removed from the cluster before it
  is started again**, not simply restarted empty.
- **Keep the cluster on one site.** Sites are to be linked by replication, not by
  stretching the metadata group across a WAN.

### On Kubernetes

The chart in `deploy/helm/cactusfs` runs the same cluster on Kubernetes or k3s: every
node is a StatefulSet of one pod with a claim for its metadata and one for each drive,
behind a headless Service for the nodes, a Service for S3 (`<release>`) and a Service of
its own for the admin port (`<release>-admin`). Pods run as a non-root user with a
read-only root filesystem and every capability dropped, and are admitted under the
`restricted` Pod Security Standard. A NetworkPolicy lets the nodes reach each other and
DNS, and nobody reach them until you say who: `networkPolicy.clientsFrom` for S3,
`networkPolicy.adminFrom` for the admin port, each apart, and `networkPolicy.egressTo`
for what the nodes must reach themselves (Vault).

Nodes are named `<release>-<pool>-<index>`, with the name of the Helm release exactly
as you give it: `helm install store ...` makes `store-meta-0`, `store-meta-1`,
`store-meta-2` and `store-data-0` with the default pools. That name is the node's ID.

The chart makes no Secret and renders nothing until you name the ones it needs:

| Value | Secret |
|---|---|
| `secrets.root` | root credentials: keys `access-key`, `secret-key` |
| `secrets.tls` | certificate of the S3 and admin listeners (`kubernetes.io/tls`) |
| `secrets.nodeCA` | the CA of node certificates: key `ca.crt` |
| `<node ID>-node-tls` | one per node (`kubernetes.io/tls`). The certificate carries exactly one DNS name, the node ID, and no IP address - not the pod's DNS name as well. A pod mounts its own and no other |
| `kms.vault.tokenSecret` | the Vault token, key `token` - or, for development, `kms.local.*` with `dev.localKMS=true` |

```sh
helm install cactusfs deploy/helm/cactusfs -n cactusfs -f my-values.yaml --set image.tag=0.7.0-412
kubectl -n cactusfs get pods        # alive at once, ready when the cluster has formed
```

`image.tag` names the build to run, `<version>-<build>` as `make image` tags it (push it
to your registry under that tag, without the architecture or as a manifest of both);
pods carry it in the label `app.kubernetes.io/version`.

`deploy/helm/cactusfs/ci/` has value sets to start from: a cluster with Vault, five
voters, one node, and a development cluster on the disks of one machine
(`localVolumes`: the directories `<basePath>/<node ID>/{meta,data1,...}` must exist and
belong to user 65532).

**Upgrading.** `helm upgrade` changes what the nodes are to be and restarts none of
them (`updateStrategy: OnDelete`): every node is a StatefulSet of its own, and nothing
else would keep an upgrade from restarting all of them at once. Bring the change to one
node at a time - nodes that do not vote first, then the voters, the one that leads the
metadata group last - and wait for each to be ready again before the next:

```sh
kubectl -n cactusfs delete pod <node ID>-0
kubectl -n cactusfs wait --for=condition=Ready pod/<node ID>-0 --timeout=5m
```

Two things about the machine, which are not the chart's to do:

- **firewalld.** On a host that runs firewalld, k3s needs its pod and service networks
  in the trusted zone, or pods cannot reach each other and the cluster never forms.
  With the networks k3s uses unless told otherwise:
  `firewall-cmd --permanent --zone=trusted --add-source=10.42.0.0/16 --add-source=10.43.0.0/16 && firewall-cmd --reload`.
- **NodePorts are published past the host firewall.** A Service of type NodePort
  answers on every address of the machine, whatever firewalld says. The admin port has
  a Service of its own so that S3 can be published without it; remember the console
  user `admin` / `admin` before you publish that one.

## Develop

Day-to-day Go work needs only Go: `go build ./...`, `go test ./...`, `go vet ./...`.

The full quality bar runs inside a pinned builder image, so everyone and CI use the same
tool versions. It needs `make` and a container engine:

```sh
make check                 # every gate
make check ENGINE=podman   # the same with Podman
make help                  # all targets
```

| Target | What it does |
|---|---|
| `make check` | format, lint, vet, unit tests + coverage, integration, S3 compatibility (the AWS SDK for Go, the AWS CLI and the S3 tests of Ceph), vulnerabilities, contracts, licences, console, cross-build, image, Helm, fresh clone |
| `make test` | unit tests, fast |
| `make fmt` | format Go sources |
| `make build` | `bin/cactusfs` for this OS |
| `make build-all` | six OS/arch binaries in `dist/` |
| `make image` | `cactusfs:<version>-<build>-amd64` and `-arm64` (see [Versions and builds](#versions-and-builds)) |
| `make verify-clone` | proves both build paths above from a clean export |
| `make test-chaos` | fault injection against the built binary: corruption, lost shards, replaced drives, kill during writes |
| `make run` | build and run a development node on `./.data` (set `CACTUSFS_KMS_PASSPHRASE`) |

Notes:

- The builder reaches the container engine through its socket (`/var/run/docker.sock` by
  default). For rootless Podman:
  `make check ENGINE=podman ENGINE_SOCKET=$XDG_RUNTIME_DIR/podman/podman.sock`.
- The builder runs as root inside its container. With rootful Docker on Linux, files it
  creates (`bin/`, `dist/`, `web/dist/`) are owned by root; `make clean` removes them.

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
runs, rehearses a release with the same code that publishes one.

## Releases

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
on `main`, runs `make check` and `make test-chaos`, builds, and publishes:

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

```sh
helm install cactusfs oci://ghcr.io/albievan/charts/cactusfs --version <version> \
  --set image.repository=ghcr.io/albievan/cactusfs \
  --set image.digest=sha256:<digest of the manifest list>
```

## Layout

```
cmd/cactusfs/        the binary: composition root
internal/domain/     entities, value objects, domain errors (standard library only)
internal/app/        use cases
internal/ports/      interfaces to the outside world
internal/adapters/   implementations and inbound transports
web/                 console (React + TypeScript + Vite)
test/                integration and S3 compatibility suites
api/                 (later) OpenAPI, AsyncAPI and protobuf contracts
scripts/             the gate scripts behind the make targets
docs/                architecture, threat model, decision records
```

## Documentation

- `docs/ARCHITECTURE.md` — the design
- `docs/threat-model.md` — what is protected and how
- `PLAN.md` — milestones, steps and decisions
- `CLAUDE.md` — engineering rules

## Licence

Public domain, as SQLite is: see [LICENSE.md](LICENSE.md). The modules and packages
CactusFS is built with keep their own licences (MIT, BSD, ISC, Apache-2.0, MPL-2.0).

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

The web console in `web/` is a placeholder. The container build compiles it, but no
build embeds or serves it yet (PLAN 5.4).

## Run

A node needs a drive, a key management service and TLS. For local development,
`--dev-plaintext` replaces TLS with HTTP on a loopback address only:

```sh
export CACTUSFS_KMS_PASSPHRASE='choose a long passphrase'
./cactusfs server --dev-plaintext --data ./.data --kms local
```

On its first start a deployment has no login. It prints generated root credentials
**once** on standard error — store them — or uses the ones you supply in
`CACTUSFS_ROOT_ACCESS_KEY` and `CACTUSFS_ROOT_SECRET_KEY`.

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

Every setting is a flag with a matching `CACTUSFS_*` variable; the flag wins. Secrets are
the exception: they are read from the environment or a file, never from the command line.
`./cactusfs server --help` lists everything; the main ones:

| Flag | Variable | Default |
|---|---|---|
| `--data` | `CACTUSFS_DATA` | — (required; repeat for several drives) |
| `--kms` | `CACTUSFS_KMS` | — (required: `local`) |
| — | `CACTUSFS_KMS_PASSPHRASE` | — (required, or `--kms-passphrase-file`) |
| `--s3-addr` | `CACTUSFS_S3_ADDR` | `:9000` |
| `--admin-addr` | `CACTUSFS_ADMIN_ADDR` | `:9001` |
| `--s3-domain` | `CACTUSFS_S3_DOMAIN` | — (enables `<bucket>.<domain>`) |
| `--tls-cert`, `--tls-key` | `CACTUSFS_TLS_CERT`, `CACTUSFS_TLS_KEY` | — |
| `--dev-plaintext` | `CACTUSFS_DEV_PLAINTEXT` | `false` |
| `--drain-delay` | `CACTUSFS_DRAIN_DELAY` | `0s` |
| `--peers`, `--node-id`, `--roles` | `CACTUSFS_PEERS`, `CACTUSFS_NODE_ID`, `CACTUSFS_ROLES` | — (cluster, see below) |
| `--otlp-endpoint` | `CACTUSFS_OTLP_ENDPOINT` | — (no traces exported) |

Operations:

- `GET /healthz` and `GET /readyz` on the admin listener are unauthenticated and say only
  `ok` or `unavailable`.
- `GET /metrics` needs `Authorization: Bearer <access key ID>:<secret>`.
- Logs are JSON on stdout. On SIGTERM the node makes `/readyz` answer 503, keeps serving
  for the drain delay, closes its listeners, lets in-flight requests finish and exits 0.

Supported S3 operations: ListBuckets, CreateBucket, HeadBucket, DeleteBucket,
GetBucketLocation, PutObject, GetObject (with a byte range), HeadObject, DeleteObject and
ListObjectsV2, by path or (with `--s3-domain`) by virtual host. Multipart uploads,
versioning, presigned URLs, copies, multi-object delete and the rest answer
`501 NotImplemented` for now.

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
- **Every node must open the same KMS key.** Until the Vault adapter exists, the nodes
  share one sealed key file (`cactusfs dev-init` writes one, with development
  certificates). That is a development arrangement, not a production one.
- **The cluster's storage is fixed when it forms**: storage nodes and drives cannot be
  added afterwards yet.
- **A voter that lost its metadata directory must be removed from the cluster before it
  is started again**, not simply restarted empty.
- **Keep the cluster on one site.** Sites are to be linked by replication, not by
  stretching the metadata group across a WAN.

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
| `make check` | format, lint, vet, unit tests + coverage, integration, S3 compatibility, vulnerabilities, contracts, licences, console, cross-build, image, Helm, fresh clone |
| `make test` | unit tests, fast |
| `make fmt` | format Go sources |
| `make build` | `bin/cactusfs` for this OS |
| `make build-all` | six OS/arch binaries in `dist/` |
| `make image` | `cactusfs:<version>-amd64` and `-arm64` |
| `make verify-clone` | proves both build paths above from a clean export |
| `make test-chaos` | fault injection against the built binary: corruption, lost shards, replaced drives, kill during writes |
| `make run` | build and run a development node on `./.data` (set `CACTUSFS_KMS_PASSPHRASE`) |

Notes:

- The builder reaches the container engine through its socket (`/var/run/docker.sock` by
  default). For rootless Podman:
  `make check ENGINE=podman ENGINE_SOCKET=$XDG_RUNTIME_DIR/podman/podman.sock`.
- The builder runs as root inside its container. With rootful Docker on Linux, files it
  creates (`bin/`, `dist/`, `web/dist/`) are owned by root; `make clean` removes them.

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

Not yet chosen. Until a licence file is added, no licence is granted.

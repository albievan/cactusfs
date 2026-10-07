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
four drives there is no parity, and the node says so. A drive that died can be replaced
by an empty one, one at a time, while the node is stopped; it is refilled in the
background. All drives must be present for the node to start.

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
  -v cactus-data:/data -v "$PWD/certs:/certs:ro" \
  -e CACTUSFS_DATA=/data -e CACTUSFS_KMS=local -e CACTUSFS_KMS_PASSPHRASE \
  -e CACTUSFS_TLS_CERT=/certs/server.crt -e CACTUSFS_TLS_KEY=/certs/server.key \
  cactusfs
```

The volume must be writable by UID 65532.

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

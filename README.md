# CactusFS

An S3-compatible, erasure-coded, multi-node object store in a single Go binary, with
data security first: encryption at rest and in transit, checksums on every read,
self-healing, RBAC and an audit trail.

> **Status: milestone 0 (scaffold).** The binary has two commands, `version` and
> `server`, and `server` serves only the health probes. It does not store objects yet.
> `PLAN.md` lists what comes next.

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

`server` refuses to start without TLS:

```sh
./cactusfs server --tls-cert server.crt --tls-key server.key
curl --cacert ca.crt https://localhost:9001/healthz     # {"status":"ok"}
curl --cacert ca.crt https://localhost:9001/readyz      # {"status":"ok"}
```

For local development only, `--dev-plaintext` serves HTTP on a loopback address
(`127.0.0.1` or `::1`) and nowhere else:

```sh
./cactusfs server --dev-plaintext
curl http://127.0.0.1:9001/healthz
```

Every flag has a matching environment variable; the flag wins.

| Flag | Variable | Default |
|---|---|---|
| `--admin-addr` | `CACTUSFS_ADMIN_ADDR` | `:9001` |
| `--tls-cert` | `CACTUSFS_TLS_CERT` | — |
| `--tls-key` | `CACTUSFS_TLS_KEY` | — |
| `--dev-plaintext` | `CACTUSFS_DEV_PLAINTEXT` | `false` |
| `--log-level` | `CACTUSFS_LOG_LEVEL` | `info` |
| `--drain-delay` | `CACTUSFS_DRAIN_DELAY` | `0s` |
| `--shutdown-timeout` | `CACTUSFS_SHUTDOWN_TIMEOUT` | `30s` |

Logs are JSON on stdout. On SIGTERM the server makes `/readyz` answer 503 and keeps
serving for the drain delay, so a load balancer can stop routing to it. Then it closes
the listener, lets in-flight requests finish within the shutdown timeout and exits 0.
With the default drain delay of `0s` the listener closes at once and the 503 is never
seen; set a delay wherever something polls `/readyz`.

In a container, mount the certificate and key and run hardened:

```sh
docker run --rm --read-only --cap-drop ALL -p 9001:9001 \
  -v "$PWD/certs:/certs:ro" \
  -e CACTUSFS_TLS_CERT=/certs/server.crt -e CACTUSFS_TLS_KEY=/certs/server.key \
  cactusfs
```

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
| `make run` | build and run with `--dev-plaintext` |

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

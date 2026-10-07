# CactusFS — one Dockerfile for the product image and for the tool image that
# `make` runs in. It needs nothing but a container engine:
#
#   docker build -t cactusfs .                 (or: podman build -t cactusfs .)
#   docker build --target builder -t cactusfs-builder .
#
# Every base image is public and pinned by digest. To move a pin, change the
# tag AND the digest together.

ARG GO_IMAGE=golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734
ARG NODE_IMAGE=node:24.21.0-trixie-slim@sha256:173f125896c3b47ddf056734c7ea789d04595a6a08769a8f78e0df642781fb66
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG GOLANGCI_LINT_IMAGE=golangci/golangci-lint:v2.14.0@sha256:ad862ba6b3798cbe0fd9fd7408d498fd74fbd2623a92406b2fd3898faf0bf98f
ARG HELM_IMAGE=alpine/helm:4.3.0@sha256:a6cf54599ccb99d90cf0712b30f03fdb3cab062e6b94e0418cc4db7e8a1464b2
ARG DOCKER_CLI_IMAGE=docker:29.8.1-cli@sha256:018edbc908e08fcc9dbf029c812c34251e9b4719e6f71ca0e5eae2a987d014ca
ARG BUF_IMAGE=bufbuild/buf:1.73.0@sha256:75d8f756f0919dcded71e67968a91adfe55fe4dd62c4900c57bbfcd7fe625608
ARG PNPM_VERSION=12.9.1

# --- node: Node.js and pnpm, on the build host's architecture -----------------
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS node
ARG PNPM_VERSION
RUN npm install --global --no-fund --no-audit "pnpm@${PNPM_VERSION}"

# --- web: the console bundle ---------------------------------------------------
FROM node AS web
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

# --- build: the cactusfs binary, cross-compiled for the target platform --------
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" GOTOOLCHAIN=local \
    go build -trimpath -buildvcs=false -tags console \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/cactusfs ./cmd/cactusfs \
 # The directories a volume is mounted on. A volume takes the ownership of the
 # directory it is first mounted on, and the image runs as a user that is not
 # root, so they must exist in the image and belong to that user.
 && mkdir -p /out/var/data1 /out/var/data2 /out/var/data3 /out/var/data4 /out/var/meta /out/var/config

# --- pinned tools that are not Go modules --------------------------------------
FROM ${GOLANGCI_LINT_IMAGE} AS golangci-lint
FROM ${HELM_IMAGE} AS helm
FROM ${DOCKER_CLI_IMAGE} AS docker-cli
FROM ${BUF_IMAGE} AS buf

# --- builder: the image every `make` target runs in ----------------------------
# Go tools (gofumpt, goimports, govulncheck, go-licenses) are NOT installed
# here: they are pinned by `tool` directives in go.mod and run with `go tool`.
FROM ${GO_IMAGE} AS builder
COPY --from=node /usr/local/bin/ /usr/local/bin/
COPY --from=node /usr/local/lib/node_modules/ /usr/local/lib/node_modules/
COPY --from=golangci-lint /usr/bin/golangci-lint /usr/local/bin/golangci-lint
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=buf /usr/local/bin/buf /usr/local/bin/buf
COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker-cli /usr/local/libexec/docker/cli-plugins/ /usr/local/libexec/docker/cli-plugins/
RUN git config --system --add safe.directory '*' \
 && go version && node --version && pnpm --version \
 && golangci-lint version && helm version --short && docker --version && buf --version
ENV CACTUSFS_IN_BUILDER=1 \
    GOTOOLCHAIN=local \
    GOCACHE=/cache/go-build \
    GOMODCACHE=/cache/go-mod \
    GOLANGCI_LINT_CACHE=/cache/golangci-lint \
    CI=true
WORKDIR /src

# --- final: the product image ---------------------------------------------------
# Distroless static, non-root, no shell. Run it with a read-only root
# filesystem and all capabilities dropped.
FROM ${RUNTIME_IMAGE}
ARG VERSION=dev
LABEL org.opencontainers.image.title="cactusfs" \
      org.opencontainers.image.description="S3-compatible, erasure-coded object store" \
      org.opencontainers.image.source="https://github.com/albievan/cactusfs" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/cactusfs /cactusfs
COPY --from=build --chown=65532:65532 /out/var/ /var/lib/cactusfs/
USER 65532:65532
EXPOSE 9000 9001 9002 9003
ENTRYPOINT ["/cactusfs"]
CMD ["server"]

# CactusFS — one Dockerfile for the product image and for the tool image that
# `make` runs in. It needs nothing but a container engine:
#
#   docker build -t cactusfs .                 (or: podman build -t cactusfs .)
#   docker build --target builder -t cactusfs-builder .
#
# Every base image is public and pinned by digest. To move a pin, change the
# tag AND the digest together.

ARG GO_IMAGE=golang:1.27.2-trixie@sha256:e58d6f83b3416618d8bcac2b3dde1b7f7e3c4a77d25e88637f8bbae81536c48d
ARG NODE_IMAGE=node:24.21.0-trixie-slim@sha256:173f125896c3b47ddf056734c7ea789d04595a6a08769a8f78e0df642781fb66
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG GOLANGCI_LINT_IMAGE=golangci/golangci-lint:v2.14.0@sha256:ad862ba6b3798cbe0fd9fd7408d498fd74fbd2623a92406b2fd3898faf0bf98f
ARG HELM_IMAGE=alpine/helm:4.3.0@sha256:a6cf54599ccb99d90cf0712b30f03fdb3cab062e6b94e0418cc4db7e8a1464b2
ARG DOCKER_CLI_IMAGE=docker:29.8.1-cli@sha256:018edbc908e08fcc9dbf029c812c34251e9b4719e6f71ca0e5eae2a987d014ca
ARG BUF_IMAGE=bufbuild/buf:1.73.0@sha256:75d8f756f0919dcded71e67968a91adfe55fe4dd62c4900c57bbfcd7fe625608
ARG OASDIFF_IMAGE=tufin/oasdiff:v1.33.0@sha256:6263a96dd2ef0726c54e21fea9b8e1607eac4841add0079324b424c1f52b819c
ARG PNPM_VERSION=12.9.1
# What the S3 compatibility suite drives the server with, besides the AWS SDK
# for Go: the AWS CLI and the S3 tests of Ceph. They are test tools of the
# builder image only; nothing of them is in the product. Each is pinned by
# the checksum of what is downloaded.
ARG PYTHON_IMAGE=python:3.13-slim-trixie@sha256:70729b46c69b4f1e97c4822c1af3df53a1476cf5ddc6c087c0c10bc3a5678c2f
ARG AWSCLI_VERSION=2.37.12
ARG AWSCLI_SHA256_AMD64=e6d3c672fb36ed4b12c354c39a993eee2b34da1437cafb10bac4480840cfa83f
ARG AWSCLI_SHA256_ARM64=829c76096e05f743267d75098bc7cc7cfae799fc7e568554ebaa6d78e6aba753
ARG S3TESTS_COMMIT=5522d1c351f75bc00ae0f64f742f3f095f5939d9
ARG S3TESTS_SHA256=f0943e3ca3a68f5c8d9ce19d12879a3edca68de3293eef687533ab5caf2aca13

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
FROM ${OASDIFF_IMAGE} AS oasdiff

# --- s3tools: the AWS CLI and ceph/s3-tests, for the compatibility suite --------
FROM ${PYTHON_IMAGE} AS s3tools
ARG TARGETARCH
ARG AWSCLI_VERSION
ARG AWSCLI_SHA256_AMD64
ARG AWSCLI_SHA256_ARM64
ARG S3TESTS_COMMIT
ARG S3TESTS_SHA256
COPY scripts/fetch.py /usr/local/bin/fetch.py
# The AWS CLI, version 2: the installer of the release, by its checksum.
RUN case "${TARGETARCH}" in \
      amd64) arch=x86_64; sum="${AWSCLI_SHA256_AMD64}" ;; \
      arm64) arch=aarch64; sum="${AWSCLI_SHA256_ARM64}" ;; \
      *) echo "no AWS CLI for ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
 && python /usr/local/bin/fetch.py "https://awscli.amazonaws.com/awscli-exe-linux-${arch}-${AWSCLI_VERSION}.zip" "$sum" /tmp/awscli \
 && /tmp/awscli/aws/install --install-dir /opt/aws-cli --bin-dir /opt/aws-cli/bin \
 && rm -rf /tmp/awscli \
 && /opt/aws-cli/bin/aws --version
# ceph/s3-tests at one commit, and the packages it imports, each by its hash.
COPY test/s3compat/tools/requirements.lock /tmp/requirements.lock
RUN python /usr/local/bin/fetch.py "https://github.com/ceph/s3-tests/archive/${S3TESTS_COMMIT}.tar.gz" "${S3TESTS_SHA256}" /tmp/s3tests \
 && mkdir -p /opt/s3tests && mv "/tmp/s3tests/s3-tests-${S3TESTS_COMMIT}" /opt/s3tests/src \
 && python -m venv /opt/s3tests/venv \
 && /opt/s3tests/venv/bin/pip install --no-cache-dir --disable-pip-version-check --require-hashes --no-deps --only-binary :all: -r /tmp/requirements.lock \
 && /opt/s3tests/venv/bin/python -c "import boto3, pytest, munch, isodate, pytz, requests"

# --- builder: the image every `make` target runs in ----------------------------
# Go tools (gofumpt, goimports, govulncheck, go-licenses) are NOT installed
# here: they are pinned by `tool` directives in go.mod and run with `go tool`.
FROM ${GO_IMAGE} AS builder
COPY --from=node /usr/local/bin/ /usr/local/bin/
COPY --from=node /usr/local/lib/node_modules/ /usr/local/lib/node_modules/
COPY --from=golangci-lint /usr/bin/golangci-lint /usr/local/bin/golangci-lint
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=buf /usr/local/bin/buf /usr/local/bin/buf
COPY --from=oasdiff /usr/bin/oasdiff /usr/local/bin/oasdiff
COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker-cli /usr/local/libexec/docker/cli-plugins/ /usr/local/libexec/docker/cli-plugins/
# Python is here for ceph/s3-tests only: the venv refers to it by this path.
COPY --from=s3tools /usr/local/bin/python3.13 /usr/local/bin/python3.13
COPY --from=s3tools /usr/local/lib/libpython3.13.so.1.0 /usr/local/lib/libpython3.13.so.1.0
COPY --from=s3tools /usr/local/lib/python3.13/ /usr/local/lib/python3.13/
COPY --from=s3tools /opt/aws-cli/ /opt/aws-cli/
COPY --from=s3tools /opt/s3tests/ /opt/s3tests/
RUN git config --system --add safe.directory '*' \
 && ldconfig \
 && ln -s /opt/aws-cli/bin/aws /usr/local/bin/aws \
 && ln -s python3.13 /usr/local/bin/python3 && ln -s python3 /usr/local/bin/python \
 && go version && node --version && pnpm --version \
 && golangci-lint version && helm version --short && docker --version && buf --version && oasdiff --version \
 && aws --version && /opt/s3tests/venv/bin/python -c "import ssl, boto3, pytest; print('s3-tests', ssl.OPENSSL_VERSION)"
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

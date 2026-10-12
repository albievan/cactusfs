# CactusFS — one Dockerfile for the product image and for the tool image that
# `make` runs in. It needs nothing but a container engine:
#
#   docker build -t cactusfs .                 (or: podman build -t cactusfs .)
#   docker build --target builder -t cactusfs-builder .
#
# A build that is numbered is told so (`make image` does it; .git is not in
# the build context, so nothing here can count commits):
#
#   docker build --build-arg BUILD=412 --build-arg COMMIT=330028134523 -t cactusfs:0.7.0-412 .
#
# Without the arguments the binary says the release and "local build".
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
# Which build this is. VERSION is the release: internal/buildinfo/VERSION is
# where it is set, and a test of that package holds this default to it. BUILD
# numbers the build (0: not numbered) and COMMIT is the commit it was made
# from; both come from whoever builds.
ARG VERSION=0.7.0
ARG BUILD=0
ARG COMMIT=
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
# What `make helm-lint` judges the chart's renders with: kubeconform, the
# Kubernetes JSON schemas of ONE version (so that it validates with the network
# off) and yq, which scripts/helm-assert.sh reads the renders with. Tools of
# the builder image only. Each is pinned by the checksum of what is downloaded;
# to move a pin, change the version (or commit) AND its checksums together.
# The schemas are single files of yannh/kubernetes-json-schema at one commit,
# from its directory v<version>-standalone-strict: one for each kind the chart
# renders, listed with its checksum in the helmtools stage. (The whole
# repository is several GB.) The version is that of the Kubernetes deployed to.
ARG KUBECONFORM_VERSION=0.8.0
ARG KUBECONFORM_SHA256_AMD64=9bc2bffbf71f261128533edaf912153948b7ff238f9a531ae6d34466ec287883
ARG KUBECONFORM_SHA256_ARM64=1f53fc8e81258197a35e8603054162a5af1de8c5af13746c71ab680d9534ed87
ARG YQ_VERSION=4.54.1
ARG YQ_SHA256_AMD64=e68a456f90c577af3fe4960184b3a3cf5c461e0348407c10f107da3a5fec8972
ARG YQ_SHA256_ARM64=8c27f6e7476fb5acefb9352e3d30f6bc2dc93f1956eb92e8da727173fb7ec6d3
ARG K8S_SCHEMAS_VERSION=1.36.0
ARG K8S_SCHEMAS_COMMIT=817113f21a2643311cbc3e992bb44ffe1c416c54
# What `make observability-check` judges the alerting rules with: promtool, of
# the Prometheus release (Apache-2.0). A tool of the builder image only;
# nothing of it is in the product. Only promtool is unpacked from the release
# archive, which is pinned by its checksum for each architecture; to move the
# pin, change the version AND both checksums. The checksums are those of
#   https://github.com/prometheus/prometheus/releases/download/v<version>/prometheus-<version>.linux-<arch>.tar.gz
# as the release's own sha256sums.txt lists them.
# TODO(PLAN 7.4): the two checksums below are placeholders - the builder image
# does not build until they are replaced with the real ones.
ARG PROMETHEUS_VERSION=3.15.0
ARG PROMTOOL_SHA256_AMD64=2a542df32eac02ee17b9d844fb2aa1de00dafa5476579ba8a3ba862e9d572ea0
ARG PROMTOOL_SHA256_ARM64=f1f90ec08e849d494ca66c611470afc50192f0355f1a61c33f2cbde02d067823

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
ARG VERSION
ARG BUILD
ARG COMMIT
WORKDIR /src
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" GOTOOLCHAIN=local \
    go build -trimpath -buildvcs=false -tags console \
      -ldflags "-s -w \
        -X github.com/albievan/cactusfs/internal/buildinfo.version=${VERSION} \
        -X github.com/albievan/cactusfs/internal/buildinfo.build=${BUILD} \
        -X github.com/albievan/cactusfs/internal/buildinfo.commit=${COMMIT}" \
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

# --- helmtools: kubeconform, its schemas and yq, for the chart's gate ------------
FROM ${PYTHON_IMAGE} AS helmtools
ARG TARGETARCH
ARG KUBECONFORM_VERSION
ARG KUBECONFORM_SHA256_AMD64
ARG KUBECONFORM_SHA256_ARM64
ARG YQ_VERSION
ARG YQ_SHA256_AMD64
ARG YQ_SHA256_ARM64
ARG K8S_SCHEMAS_VERSION
ARG K8S_SCHEMAS_COMMIT
COPY scripts/fetch.py /usr/local/bin/fetch.py
# The binaries of the releases, each by its checksum.
RUN case "${TARGETARCH}" in \
      amd64) kubeconform_sum="${KUBECONFORM_SHA256_AMD64}"; yq_sum="${YQ_SHA256_AMD64}" ;; \
      arm64) kubeconform_sum="${KUBECONFORM_SHA256_ARM64}"; yq_sum="${YQ_SHA256_ARM64}" ;; \
      *) echo "no kubeconform or yq for ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
 && python /usr/local/bin/fetch.py "https://github.com/yannh/kubeconform/releases/download/v${KUBECONFORM_VERSION}/kubeconform-linux-${TARGETARCH}.tar.gz" "$kubeconform_sum" /tmp/kubeconform kubeconform \
 && python /usr/local/bin/fetch.py "https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}/yq_linux_${TARGETARCH}.tar.gz" "$yq_sum" /tmp/yq "yq_linux_${TARGETARCH}" \
 && mkdir -p /opt/helmtools/bin \
 && install -m 0755 /tmp/kubeconform/kubeconform /opt/helmtools/bin/kubeconform \
 && install -m 0755 "/tmp/yq/yq_linux_${TARGETARCH}" /opt/helmtools/bin/yq \
 && rm -rf /tmp/kubeconform /tmp/yq \
 && /opt/helmtools/bin/kubeconform -v && /opt/helmtools/bin/yq --version
# The schema of each kind the chart renders, each file by its checksum, named
# as kubeconform looks for it: <kind>-<group>-<version>.json, the kind in lower
# case, the first label of the group, and no group for the core one. A kind
# that is not in this list fails `make helm-lint` - kubeconform is given no
# other place to look and skips nothing - so a new kind of the chart is a new
# line here.
RUN schema() { python /usr/local/bin/fetch.py --file \
      "https://raw.githubusercontent.com/yannh/kubernetes-json-schema/${K8S_SCHEMAS_COMMIT}/v${K8S_SCHEMAS_VERSION}-standalone-strict/$1.json" \
      "$2" "/opt/kubernetes-schemas/v${K8S_SCHEMAS_VERSION}-standalone-strict/$1.json"; } \
 && schema statefulset-apps-v1           cfa6579916fc2a1be0deb6372a3c67c3747a212131d0cb7b342d645f65639987 \
 && schema service-v1                    8bf019854daed511e7c174896a898173fa65d88ec5937c687a37303d4cc9351b \
 && schema networkpolicy-networking-v1   f6324cc464f62228b0418f438d167208e4f86c7e3677ba30f608e79a8b26ba79 \
 && schema poddisruptionbudget-policy-v1 92b2b2cf7c193b9d10943e7785ebe6eefce439d9e1e34e5fd9e820155a657137 \
 && schema storageclass-storage-v1       9704bb88ec73ed37f8d565dbd2d2ee1e8ee61b5ec63c96a5a61926d6971ef3c9 \
 && schema persistentvolume-v1           ddff73572bf0965cecf9c160949a1c416f4f9e8ade7f46a649767589a1e2783c \
 && ls -l "/opt/kubernetes-schemas/v${K8S_SCHEMAS_VERSION}-standalone-strict"

# --- promtools: promtool, for the gate of the alerting rules ---------------------
FROM ${PYTHON_IMAGE} AS promtools
ARG TARGETARCH
ARG PROMETHEUS_VERSION
ARG PROMTOOL_SHA256_AMD64
ARG PROMTOOL_SHA256_ARM64
COPY scripts/fetch.py /usr/local/bin/fetch.py
# The archive of the release, by its checksum; of it, promtool and no more.
RUN case "${TARGETARCH}" in \
      amd64) sum="${PROMTOOL_SHA256_AMD64}" ;; \
      arm64) sum="${PROMTOOL_SHA256_ARM64}" ;; \
      *) echo "no promtool for ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
 && release="prometheus-${PROMETHEUS_VERSION}.linux-${TARGETARCH}" \
 && python /usr/local/bin/fetch.py "https://github.com/prometheus/prometheus/releases/download/v${PROMETHEUS_VERSION}/${release}.tar.gz" "$sum" /tmp/prometheus "${release}/promtool" \
 && mkdir -p /opt/promtools/bin \
 && install -m 0755 "/tmp/prometheus/${release}/promtool" /opt/promtools/bin/promtool \
 && rm -rf /tmp/prometheus \
 && /opt/promtools/bin/promtool --version

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
# kubeconform, yq and the Kubernetes schemas kubeconform reads from this
# directory instead of the network (scripts/helm-lint.sh).
COPY --from=helmtools /opt/helmtools/bin/ /usr/local/bin/
COPY --from=helmtools /opt/kubernetes-schemas/ /opt/kubernetes-schemas/
RUN git config --system --add safe.directory '*' \
 && ldconfig \
 && ln -s /opt/aws-cli/bin/aws /usr/local/bin/aws \
 && ln -s python3.13 /usr/local/bin/python3 && ln -s python3 /usr/local/bin/python \
 && go version && node --version && pnpm --version \
 && golangci-lint version && helm version --short && docker --version && buf --version && oasdiff --version \
 && aws --version && /opt/s3tests/venv/bin/python -c "import ssl, boto3, pytest; print('s3-tests', ssl.OPENSSL_VERSION)"
RUN kubeconform -v && yq --version && ls -d /opt/kubernetes-schemas/v*-standalone-strict
# promtool, for the alerting rules (scripts/observability.sh).
COPY --from=promtools /opt/promtools/bin/ /usr/local/bin/
RUN promtool --version
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
ARG VERSION
ARG BUILD
ARG COMMIT
LABEL org.opencontainers.image.title="cactusfs" \
      org.opencontainers.image.description="S3-compatible, erasure-coded object store" \
      org.opencontainers.image.source="https://github.com/albievan/cactusfs" \
      org.opencontainers.image.version="${VERSION}-${BUILD}" \
      org.opencontainers.image.revision="${COMMIT}"
COPY --from=build /out/cactusfs /cactusfs
COPY --from=build --chown=65532:65532 /out/var/ /var/lib/cactusfs/
USER 65532:65532
EXPOSE 9000 9001 9002 9003
ENTRYPOINT ["/cactusfs"]
CMD ["server"]

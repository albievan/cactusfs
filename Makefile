# CactusFS — build entry point.
#
# Every target below runs inside the pinned builder image (the `builder` stage
# of the Dockerfile), so each developer and CI use identical tool versions. The
# host needs only `make` and a container engine:
#
#   make check                  # the whole quality bar
#   make check ENGINE=podman    # the same, with Podman
#
# Plain `go build ./cmd/cactusfs`, `go test ./...` and `go vet ./...` work
# without make. Works with GNU Make 3.81 and later.

SHELL := /bin/sh
.DEFAULT_GOAL := help

MODULE        := github.com/albievan/cactusfs
ENGINE        ?= docker
ENGINE_SOCKET ?= /var/run/docker.sock
IMAGE         ?= cactusfs
BUILDER_IMAGE ?= cactusfs-builder:local
CACHE_VOLUME  ?= cactusfs-builder-cache
NODE_VOLUME   ?= cactusfs-builder-node-modules

# Which build this is (scripts/buildid.sh). VERSION is the release, read from
# internal/buildinfo/VERSION, the one place it is set; BUILD numbers the build, by the
# count of commits; COMMIT is the commit, with -dirty when the tree has
# uncommitted changes. Each can be given: make image VERSION=1.0.0 BUILD=7.
# They are worked out here, on the host, and handed to the builder, which
# works them out itself only if it is started without them. Without git, or
# outside a repository, BUILD is 0 and COMMIT is empty - never an error.
BUILDINFO := $(MODULE)/internal/buildinfo
ifeq ($(strip $(VERSION)),)
VERSION := $(shell sh scripts/buildid.sh version)
endif
ifeq ($(strip $(BUILD)),)
BUILD := $(shell sh scripts/buildid.sh build)
endif
ifeq ($(origin COMMIT),undefined)
COMMIT := $(shell sh scripts/buildid.sh commit)
endif

# Targets that run inside the builder image.
BUILDER_TARGETS := gen fmt fmt-check lint vet test cover test-int s3-compat \
	s3-compat-record test-chaos vuln contracts licences web-install web-lock web-check \
	web-build build build-all image helm-lint observability-check verify-clone tidy check

.PHONY: help builder shell run up down clean $(BUILDER_TARGETS)

help:
	@echo "CactusFS make targets (run inside the builder image; ENGINE=docker|podman):"
	@echo "  check          the quality bar: every gate below"
	@echo "  fmt            format Go sources          fmt-check   fail on unformatted Go"
	@echo "  lint           golangci-lint              vet         go vet"
	@echo "  test           unit tests, fast           cover       unit tests + coverage gate"
	@echo "  test-int       integration tests          s3-compat   S3 compatibility vs baseline"
	@echo "  vuln           govulncheck                licences    dependency licence allow-list"
	@echo "  contracts      generated code + API contracts in sync"
	@echo "  web-check      console lint, typecheck, test"
	@echo "  build          cactusfs for this OS -> bin/"
	@echo "  build-all      six OS/arch targets -> dist/"
	@echo "  image          container images for linux/amd64 and linux/arm64 (< 60 MB)"
	@echo "  helm-lint      lint and render the Helm chart"
	@echo "  observability-check  alerting rules and their tests, dashboards, the chart's monitoring templates"
	@echo "  verify-clone   prove a clean export builds with Go alone and with a container engine alone"
	@echo "  gen            regenerate code from API contracts"
	@echo "  tidy           go mod tidy                web-lock    rewrite web/pnpm-lock.yaml"
	@echo "  run            run the standalone binary (host)   shell   open a shell in the builder"
	@echo "  up / down      4-node compose cluster     test-chaos  fault injection"
	@echo "  clean          remove build output"

ifndef CACTUSFS_IN_BUILDER
# ------------------------------------------------------------------------------
# Host side: build the builder image and re-run the target inside it.
# ------------------------------------------------------------------------------

# The OS and architecture `make build` compiles for: this host, not the builder.
UNAME_S := $(shell uname -s 2>/dev/null || echo Windows)
UNAME_M := $(shell uname -m 2>/dev/null || echo x86_64)
ifeq ($(UNAME_S),Darwin)
HOST_GOOS := darwin
else ifeq ($(UNAME_S),Linux)
HOST_GOOS := linux
else
HOST_GOOS := windows
endif
ifneq (,$(filter $(UNAME_M),arm64 aarch64))
HOST_GOARCH := arm64
else
HOST_GOARCH := amd64
endif
ifeq ($(HOST_GOOS),windows)
EXE := .exe
endif

# On a machine that is shared, the builder can be held to a part of it:
#   make check BUILDER_LIMITS=--cpus=4,--memory=6g GO_P=2
# The limits are engine options, separated by commas (so none of them may
# hold a comma: write a range as 0-3). GO_P is how many packages Go builds and
# tests at once, and how many threads it runs. Nothing else of the caller's Go
# settings reaches the builder: GOFLAGS could narrow what the gates test.
BUILDER_LIMITS ?=
GO_P ?=
comma := ,

# The engine socket is passed through for targets that build images and, later,
# for testcontainers. Rootless Podman: ENGINE_SOCKET=$XDG_RUNTIME_DIR/podman/podman.sock
SOCKET_MOUNT := $(if $(wildcard $(ENGINE_SOCKET)),-v $(ENGINE_SOCKET):/var/run/docker.sock)
# Podman's Docker-compatible API has no BuildKit, so use the classic builder.
ifeq ($(ENGINE),podman)
BUILDKIT_ENV := -e DOCKER_BUILDKIT=0
endif

BUILDER_RUN = $(ENGINE) run --rm \
	-v "$(CURDIR)":/src -w /src \
	-v $(CACHE_VOLUME):/cache \
	-v $(NODE_VOLUME):/src/web/node_modules \
	$(SOCKET_MOUNT) $(BUILDKIT_ENV) $(subst $(comma), ,$(BUILDER_LIMITS)) \
	-e VERSION=$(VERSION) -e BUILD=$(BUILD) -e COMMIT=$(COMMIT) -e IMAGE=$(IMAGE) \
	-e HOST_GOOS=$(HOST_GOOS) -e HOST_GOARCH=$(HOST_GOARCH) \
	-e CONTRACT_BASE -e CACTUSFS_TEST_IMAGE \
	$(if $(GO_P),-e GOFLAGS=-p=$(GO_P) -e GOMAXPROCS=$(GO_P))

builder:
	@echo "builder: $(BUILDER_IMAGE) ($(ENGINE))"
	@$(ENGINE) build --quiet --target builder -t $(BUILDER_IMAGE) . >/dev/null

$(BUILDER_TARGETS): builder
	@$(BUILDER_RUN) $(BUILDER_IMAGE) make $@

shell: builder
	$(BUILDER_RUN) -it $(BUILDER_IMAGE) bash

# A development node on ./.data. The KMS passphrase comes from the environment:
#   CACTUSFS_KMS_PASSPHRASE='...' make run
run: build
	./bin/cactusfs$(EXE) server --dev-plaintext --data ./.data --kms local

# The four-node development cluster of deploy/compose/compose.yaml, with the
# engine's own compose (docker compose, or podman compose). The KMS passphrase
# comes from the environment and must be the same at every start:
#   export CACTUSFS_KMS_PASSPHRASE='...'; make up
COMPOSE := $(ENGINE) compose -f deploy/compose/compose.yaml

up:
	CACTUSFS_BUILD=$(BUILD) CACTUSFS_COMMIT=$(COMMIT) $(COMPOSE) up -d --build
	@echo "make up: S3 on https://localhost:9000 (node1), :9010, :9020, :9030 - see deploy/compose/compose.yaml"

# Stops the cluster and keeps its volumes. To delete the data as well:
#   $(ENGINE) compose -f deploy/compose/compose.yaml down --volumes
down:
	$(COMPOSE) down

clean: builder
	@$(BUILDER_RUN) $(BUILDER_IMAGE) rm -rf bin dist web/dist .cache

# The same targets on another machine, for when this one must not run
# containers (see scripts/remote.sh):
#   make remote-check REMOTE=user@host
# REMOTE_DIR is where the tree is copied to; REMOTE_VARS are make variables
# for that side, such as directories to keep the builder's caches in:
#   REMOTE_VARS="CACHE_VOLUME=/data/x/cache NODE_VOLUME=/data/x/node_modules"
remote-%:
	@REMOTE="$(REMOTE)" REMOTE_DIR="$(REMOTE_DIR)" scripts/remote.sh $* ENGINE=$(ENGINE) $(REMOTE_VARS)

else
# ------------------------------------------------------------------------------
# Builder side: the real recipes.
# ------------------------------------------------------------------------------

HOST_GOOS   ?= $(shell go env GOOS)
HOST_GOARCH ?= $(shell go env GOARCH)
PLATFORMS   := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
GO_BUILD    := CGO_ENABLED=0 go build -trimpath -tags console \
	-ldflags "-s -w -X $(BUILDINFO).version=$(VERSION) -X $(BUILDINFO).build=$(BUILD) -X $(BUILDINFO).commit=$(COMMIT)"
GO_FILES     = $$(git ls-files -co --exclude-standard -- '*.go')
PNPM        := pnpm -C web

builder:
	@true

check: fmt-check lint vet cover test-int s3-compat vuln contracts licences \
	web-check build-all image helm-lint observability-check verify-clone
	@echo "make check: PASS"

# The web client is generated with a package of web/, so those are installed.
gen: web-install
	@scripts/gen.sh

tidy:
	go mod tidy

fmt:
	go tool gofumpt -w $(GO_FILES)
	go tool goimports -w -local $(MODULE) $(GO_FILES)

fmt-check:
	@scripts/fmt-check.sh

lint: web-build
	@scripts/no-bypass.sh
	golangci-lint run ./...
	@echo "lint: PASS"

# With the console tag the built console is embedded, so it must be built.
vet: web-build
	go vet ./...
	go vet -tags=integration,fixturegen,s3compat,s3tools,chaos,console,rollingupgrade ./...
	@echo "vet: PASS"

test:
	go test -race -count=1 ./...

cover:
	@scripts/coverage.sh

# The cluster tests run containers of the product image; they build it
# themselves, from this tree, unless CACTUSFS_TEST_IMAGE names one.
test-int:
	go test -race -count=1 -timeout 30m -tags=integration ./...
	@echo "test-int: PASS"

s3-compat:
	@scripts/s3compat.sh check

s3-compat-record:
	@scripts/s3compat.sh record

# Fault injection. Against the built binary: corrupt and delete shards, wipe
# drives, kill the process during writes. Against a cluster of containers:
# kill nodes during writes, kill the leader, partition the network, take two
# nodes away, corrupt and delete shards on other nodes. Must pass before a
# milestone that touches the write path, quorum, Raft or healing is marked done.
test-chaos:
	go test -count=1 -tags=chaos -timeout 40m ./test/chaos/...
	@echo "test-chaos: PASS"

vuln:
	go tool govulncheck ./...
	@echo "vuln: PASS"

contracts: web-install
	@scripts/contracts.sh

licences: web-install
	@scripts/licences.sh

web-install:
	@$(PNPM) install --frozen-lockfile --store-dir /cache/pnpm

# Rewrites web/pnpm-lock.yaml after a change to web/package.json.
web-lock:
	$(PNPM) install --lockfile-only --store-dir /cache/pnpm

web-check: web-install
	$(PNPM) lint
	$(PNPM) typecheck
	$(PNPM) test
	@echo "web-check: PASS"

web-build: web-install
	$(PNPM) build

build: web-build
	GOOS=$(HOST_GOOS) GOARCH=$(HOST_GOARCH) $(GO_BUILD) \
		-o bin/cactusfs$(if $(filter windows,$(HOST_GOOS)),.exe) ./cmd/cactusfs

build-all: web-build
	@set -e; for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; ext=; \
		if [ "$$os" = windows ]; then ext=.exe; fi; \
		echo "build-all: $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch $(GO_BUILD) -o "dist/cactusfs-$$os-$$arch$$ext" ./cmd/cactusfs; \
	done; \
	echo "build-all: PASS"

image:
	@VERSION=$(VERSION) BUILD=$(BUILD) COMMIT=$(COMMIT) scripts/image.sh

helm-lint:
	@scripts/helm-lint.sh
	@CACTUSFS_KMS_PASSPHRASE=only-to-render-the-file docker compose -f deploy/compose/compose.yaml config --quiet
	@echo "compose: deploy/compose/compose.yaml is valid"

# The observability pack of deploy/observability and the chart's templates for
# the Prometheus Operator: promtool on the rules and their tests, the
# dashboards, and a Go test that holds every metric they name to the registry.
observability-check:
	@scripts/observability.sh

verify-clone:
	@scripts/verify-clone.sh

endif

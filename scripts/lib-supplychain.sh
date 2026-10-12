#!/bin/sh
# What scripts/sbom.sh, scripts/sign-verify.sh and scripts/release.sh share
# (PLAN 7.3, ADR-0021). It is sourced, not run, from the root of the repository.
#
# Every command line of syft and of cosign is in this file, and the flags that
# depend on the pinned version of a tool are each in ONE variable below.
#
# WHAT RUNS WHERE
#
#   host engine ---------------------------------------------------------------
#     |  the registry container <run>-registry, on a network of its own
#     |  (<run>-net), its port 5000 published on 127.0.0.1:<random> of the host
#     |
#     |  docker tag / docker push  localhost:<random>/<run>/cactusfs:<tag>
#     |     The ENGINE pushes. Docker accepts plain HTTP for a registry at
#     |     localhost and for no other address without a change to the
#     |     daemon's configuration, which a gate must not need. So the engine
#     |     knows the registry as SC_ENGINE_REG = localhost:<random>.
#     |
#   builder container (this script) -------------------------------------------
#        cosign, syft and the registry helper speak to the registry
#        THEMSELVES, not through the engine. The builder's localhost is not the
#        host's, so the builder is connected to <run>-net for the length of
#        the run (`docker network connect`, by its own container ID, which is
#        its hostname) and knows the registry as SC_DIRECT_REG =
#        <address of the registry on <run>-net>:5000. An address and not a
#        name: a container that joins a network after it has started is not
#        certain to resolve names on it.
#
#   One registry, two names. A signature is of a digest, and the digest is the
#   same under both.
#
#   Outside the builder (CACTUSFS_IN_BUILDER is not 1) there is no network to
#   make: both names are localhost:<random>.
#
# Plain HTTP is spoken on the host's loopback and on that one network only,
# for the minutes of a run, to a registry that holds nothing but what the run
# pushed. Nothing here speaks to a registry that is not the run's.
#
# TWO MODES (SC_MODE), one procedure
#
#   key      the gate and the dry run (`make sign-verify`, release.sh --dry-run):
#            everything above. A key pair made for the run, no transparency
#            log, no network. This is the default and the only mode a gate uses.
#   keyless  a release (release.sh --publish, in the release workflow and
#            nowhere else; PLAN 7.9, ADR-0021): the registry is the public one,
#            over HTTPS, and is the same name for the engine and for the tools
#            (sc_registry_remote); cosign signs with a certificate Fulcio gives
#            the workflow's identity and records it in Rekor (sc_keyless). The
#            proxy that leads nowhere is NOT set: this mode exists to reach the
#            registry, Fulcio and Rekor.
#
#   The steps are the same functions in both: sc_push, the helper's platform,
#   index, digest and retag, sc_sign, sc_verify, sc_attest,
#   sc_verify_attestation, sc_attestation_check. What differs is inside them,
#   and is said where it differs.
#
# WHAT IS CLEANED UP, on every way out (sc_cleanup, trapped): the registry
# container and its anonymous volume, the network, the builder's connection to
# it, the tags this run made in the engine's image store (never an image: the
# tags are second names of cactusfs:<version>-<build>-<arch>), and the
# directory with the keys - and, in a release, with the registry logins of
# docker and helm, which are written nowhere else. Everything is named <run>-..., and nothing that is
# not is touched. Nothing is pruned.

SC_MODULE=github.com/albievan/cactusfs
# A module the binary must link, and two packages the console must have: an
# SBOM without them did not read what it says it read.
SC_GO_DEPENDENCY=github.com/hashicorp/raft
SC_CONSOLE_PACKAGES="react bootstrap"
SC_SBOM_DIR=dist/sbom
SC_ARCHES="amd64 arm64"

# --- flags that depend on the pinned versions (Dockerfile: COSIGN_VERSION, ---
# --- SYFT_VERSION). To move a pin, read the release notes and change them  ---
# --- here, nowhere else.                                                   ---
#
# cosign 2.x. Plain HTTP to the run's registry:
SC_COSIGN_REGISTRY_FLAGS="--allow-insecure-registry=true --allow-http-registry=true"
# Signing and attesting with a key and nothing else: no entry in a
# transparency log, no question asked. (cosign 3 signs through a "signing
# config" fetched from the network unless told --use-signing-config=false,
# and writes a new bundle format: do not move to it without ADR-0021.)
SC_COSIGN_SIGN_FLAGS="--tlog-upload=false --yes=true"
# Verifying with the public key and nothing else. The name is cosign's: there
# is no log entry to look for, by design.
SC_COSIGN_VERIFY_FLAGS="--insecure-ignore-tlog=true"
# --- keyless: what differs from the three variables above, and why -----------
# No registry flags: the registry is public and speaks HTTPS.
# Signing: no --key - cosign asks Fulcio for a certificate with the OIDC token
# GitHub gives the job (ACTIONS_ID_TOKEN_REQUEST_URL and _TOKEN), and no
# --tlog-upload=false - the entry in Rekor is the point: it is what makes a
# signature made with a stolen identity visible, and what lets a certificate
# that lived ten minutes be checked years later. --yes because nobody is there
# to be asked whether the identity may be published in the log.
SC_KEYLESS_SIGN_FLAGS="--yes=true"
# Verifying: no --key and no --insecure-ignore-tlog - the log entry IS looked
# for. Instead, who must have signed: the issuer below and the identity
# sc_keyless is given, both compared exactly.
SC_KEYLESS_ISSUER=https://token.actions.githubusercontent.com
# A file is signed into a bundle (signature, certificate, log entry) of the
# Sigstore bundle format, which is what the name *.sigstore.json promises.
SC_KEYLESS_BUNDLE_FLAGS="--new-bundle-format=true"
# The predicate type of the attestation, for attest and verify-attestation,
# and what cosign writes for it into the statement.
SC_ATTESTATION_TYPE=spdxjson
SC_PREDICATE_TYPE=https://spdx.dev/Document
# How cosign 2.x says "this does not verify". A refusal is counted only when
# the output matches; anything else that fails is a broken gate.
SC_REFUSED_SIGNATURE='no signatures found|no matching signatures'
SC_REFUSED_ATTESTATION='no matching attestations|no attestations found|no signatures found|no matching signatures'
# What a tool says when it gave no verdict at all.
SC_NO_VERDICT='connection refused|no such host|i/o timeout|proxyconnect|dial tcp|unknown flag|unknown command|unknown shorthand|panic:'
# The tags cosign 2.x keeps a signature and an attestation under, next to the
# image: sha256-<hex>.sig and sha256-<hex>.att.
SC_SIGNATURE_SUFFIX=.sig
SC_ATTESTATION_SUFFIX=.att
# syft 1.x: the output format, as "-o <format>=<file>". It writes SPDX 2.3.
SC_SYFT_FORMAT=spdx-json
SC_SPDX_VERSION=SPDX-2.3
# A proxy nobody listens at. cosign and syft are given it for every host but
# the registry, so a call to the internet fails the gate.
SC_NOWHERE=http://127.0.0.1:9

# The licence the SBOM declares for CactusFS itself. LICENSE.md is a dedication
# to the public domain in the project's own words, with a blessing and a grant
# for where a dedication is not recognised. It is not the text of any licence
# of the SPDX list - not "blessing" (SQLite's notice), not "Unlicense", not
# "CC0-1.0" - so it is declared as a licence of this document, with its text
# (ADR-0021).
SC_LICENSE_ID=LicenseRef-CactusFS-Blessing
SC_LICENSE_NAME="CactusFS public-domain dedication with blessing (LICENSE.md)"
SC_LICENSE_FILE=LICENSE.md
SC_LICENSE_URL=https://github.com/albievan/cactusfs/blob/main/LICENSE.md

SC_GATE=supply-chain
# key (the default: the gate, the dry run) or keyless (a release): see the head
# of this file.
SC_MODE=key
SC_KEYLESS_IDENTITY=
SC_REGISTRY_SCHEME=http
SC_REGISTRY_USER=
SC_REGISTRY_PASSWORD=
SC_WORK=
SC_RUN=
SC_REGISTRY=
SC_NET=
SC_SELF=
SC_ENGINE_REG=
SC_DIRECT_REG=

sc_fail() {
	echo "$SC_GATE: FAIL - $1" >&2
	exit 1
}

sc_say() {
	echo "$SC_GATE: $1"
}

# sc_init <gate> <tool>...: the name of the gate, the tools it needs, which
# build this is, a directory for the run and the trap that removes it.
sc_init() {
	SC_GATE=$1
	shift
	LC_ALL=C
	export LC_ALL
	for sc_tool in "$@"; do
		command -v "$sc_tool" >/dev/null 2>&1 ||
			sc_fail "$sc_tool is not installed (it is in the builder image: run the make target)"
	done
	[ -f Dockerfile ] && [ -f go.mod ] || sc_fail "run from the root of the repository"
	if ! docker version >/dev/null 2>&1; then
		sc_fail "no container engine reachable; pass the engine socket (ENGINE_SOCKET=...)"
	fi

	sc_here=$(dirname "$0")
	IMAGE=${IMAGE:-cactusfs}
	VERSION=${VERSION:-$(sh "$sc_here/buildid.sh" version)}
	BUILD=${BUILD:-$(sh "$sc_here/buildid.sh" build)}
	[ -n "$VERSION" ] || sc_fail "no version (internal/buildinfo/VERSION)"

	SC_WORK=$(mktemp -d)
	chmod 700 "$SC_WORK"
	: >"$SC_WORK/tags"
	trap sc_cleanup EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	sc_write_helper
}

# sc_cleanup: see the head of this file. It never fails and never changes the
# status the script ends with.
sc_cleanup() {
	set +e
	if [ -n "$SC_SELF" ] && [ -n "$SC_NET" ]; then
		docker network disconnect "$SC_NET" "$SC_SELF" >/dev/null 2>&1
	fi
	if [ -n "$SC_REGISTRY" ]; then
		docker rm --force --volumes "$SC_REGISTRY" >/dev/null 2>&1
	fi
	if [ -n "$SC_NET" ]; then
		docker network rm "$SC_NET" >/dev/null 2>&1
	fi
	if [ -n "$SC_WORK" ] && [ -f "$SC_WORK/tags" ]; then
		sort -u "$SC_WORK/tags" | while read -r sc_tag; do
			# Without --force: a tag is removed, never an image in use.
			docker image rm "$sc_tag" >/dev/null 2>&1
		done
	fi
	if [ -n "$SC_WORK" ]; then
		rm -rf "$SC_WORK"
	fi
}

# sc_local_tag <arch>: the image `make image` made for the architecture.
sc_local_tag() {
	echo "$IMAGE:$VERSION-$BUILD-$1"
}

# sc_need_images: both images are in the engine's store, each of its
# architecture.
sc_need_images() {
	for sc_arch in $SC_ARCHES; do
		sc_tag=$(sc_local_tag "$sc_arch")
		sc_got=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$sc_tag" 2>/dev/null) ||
			sc_fail "no image $sc_tag; run 'make image' first"
		[ "$sc_got" = "linux/$sc_arch" ] || sc_fail "$sc_tag is $sc_got, want linux/$sc_arch"
	done
}

# sc_sbom_path <arch>|console: where the SBOM is written.
sc_sbom_path() {
	case $1 in
	console) echo "$SC_SBOM_DIR/cactusfs-console-$VERSION-$BUILD.spdx.json" ;;
	*) echo "$SC_SBOM_DIR/cactusfs-$VERSION-$BUILD-linux-$1.spdx.json" ;;
	esac
}

# --- syft ----------------------------------------------------------------------

# sc_syft <source> <name> <version> <file>: THE command line of syft. No
# look for a newer syft, no call anywhere.
sc_syft() {
	SYFT_CHECK_FOR_APP_UPDATE=false \
		HTTPS_PROXY=$SC_NOWHERE HTTP_PROXY=$SC_NOWHERE https_proxy=$SC_NOWHERE http_proxy=$SC_NOWHERE \
		syft scan "$1" --quiet --source-name "$2" --source-version "$3" --output "$SC_SYFT_FORMAT=$4"
}

# sc_sbom_image <arch>: the SBOM of the image of one architecture. The image
# is taken out of the engine's store as an archive and read from the file: the
# same on the classic store and on containerd's, whatever the architecture of
# the machine, and syft needs neither the socket nor a registry.
sc_sbom_image() {
	sc_tag=$(sc_local_tag "$1")
	sc_out=$(sc_sbom_path "$1")
	mkdir -p "$SC_SBOM_DIR"
	rm -f "$sc_out"
	docker image save --output "$SC_WORK/image-$1.tar" "$sc_tag" || sc_fail "docker image save $sc_tag"
	sc_syft "docker-archive:$SC_WORK/image-$1.tar" "$IMAGE" "$VERSION-$BUILD-linux-$1" "$sc_out" ||
		sc_fail "syft did not write the SBOM of $sc_tag"
	rm -f "$SC_WORK/image-$1.tar"
	# syft cannot know the licence of the module the binary is; it is declared.
	sc_helper sbom-license "$sc_out" "$SC_MODULE" "$SC_LICENSE_ID" "$SC_LICENSE_NAME" \
		"$SC_LICENSE_FILE" "$SC_LICENSE_URL" || sc_fail "the licence of $SC_MODULE is not declared in $sc_out"
	sc_sbom_check "$1"
}

# sc_sbom_console: the SBOM of the console's packages, from the lock file and
# nothing else: a copy of it in an empty directory, so that neither
# web/node_modules nor web/dist is read.
sc_sbom_console() {
	sc_out=$(sc_sbom_path console)
	[ -f web/pnpm-lock.yaml ] || sc_fail "no web/pnpm-lock.yaml"
	mkdir -p "$SC_SBOM_DIR" "$SC_WORK/console"
	rm -f "$sc_out"
	cp web/pnpm-lock.yaml "$SC_WORK/console/pnpm-lock.yaml"
	sc_syft "dir:$SC_WORK/console" cactusfs-console "$VERSION-$BUILD" "$sc_out" ||
		sc_fail "syft did not write the SBOM of web/pnpm-lock.yaml"
	rm -rf "$SC_WORK/console"
	sc_sbom_check console
}

# sc_sbom_check <arch>|console: the file is an SPDX document that names what
# it must.
sc_sbom_check() {
	sc_out=$(sc_sbom_path "$1")
	[ -s "$sc_out" ] || sc_fail "no SBOM $sc_out; run 'make sbom' first"
	case $1 in
	console)
		# shellcheck disable=SC2086 # the names are split on purpose
		sc_helper sbom-check "$sc_out" "$SC_SPDX_VERSION" 50 $SC_CONSOLE_PACKAGES
		;;
	*) sc_helper sbom-check "$sc_out" "$SC_SPDX_VERSION" 20 "$SC_MODULE" "$SC_GO_DEPENDENCY" ;;
	esac || sc_fail "$sc_out is not the SBOM it should be"
}

# --- the registry ----------------------------------------------------------------

# sc_registry_start: the throw-away registry, and the two names it is reached
# by (the head of this file). The image is the pin of the Dockerfile.
sc_registry_start() {
	sc_image=$(sed -n 's/^ARG REGISTRY_IMAGE=//p' Dockerfile)
	[ -n "$sc_image" ] || sc_fail "REGISTRY_IMAGE pin not found in Dockerfile"
	case $sc_image in
	*@sha256:*REPLACE_WITH*) sc_fail "REGISTRY_IMAGE in the Dockerfile still holds a placeholder for its digest" ;;
	*@sha256:*) ;;
	*) sc_fail "REGISTRY_IMAGE in the Dockerfile is not pinned by digest" ;;
	esac

	SC_RUN="cactusfs-sc-$(date +%s)-$$"
	if [ "${CACTUSFS_IN_BUILDER:-}" = 1 ]; then
		sc_self=$(hostname)
		docker container inspect "$sc_self" >/dev/null 2>&1 ||
			sc_fail "this container cannot find itself through the engine by its hostname ($sc_self)"
		SC_NET="$SC_RUN-net"
		docker network create "$SC_NET" >/dev/null || sc_fail "docker network create $SC_NET"
		SC_REGISTRY="$SC_RUN-registry"
		docker run --detach --name "$SC_REGISTRY" --network "$SC_NET" \
			--publish 127.0.0.1::5000 "$sc_image" >/dev/null || sc_fail "the registry did not start"
		docker network connect "$SC_NET" "$sc_self" || sc_fail "docker network connect $SC_NET $sc_self"
		SC_SELF=$sc_self
		sc_address=$(docker container inspect \
			--format "{{(index .NetworkSettings.Networks \"$SC_NET\").IPAddress}}" "$SC_REGISTRY")
		[ -n "$sc_address" ] || sc_fail "the registry has no address on $SC_NET"
		SC_DIRECT_REG="$sc_address:5000"
	else
		SC_REGISTRY="$SC_RUN-registry"
		docker run --detach --name "$SC_REGISTRY" \
			--publish 127.0.0.1::5000 "$sc_image" >/dev/null || sc_fail "the registry did not start"
	fi
	sc_port=$(docker port "$SC_REGISTRY" 5000/tcp | sed -n '1s/.*://p')
	case $sc_port in
	'' | *[!0-9]*) sc_fail "the registry's port is not published on the host's loopback" ;;
	esac
	SC_ENGINE_REG="localhost:$sc_port"
	if [ -z "$SC_DIRECT_REG" ]; then
		SC_DIRECT_REG=$SC_ENGINE_REG
	fi

	sc_tries=0
	until sc_helper ping >/dev/null 2>&1; do
		sc_tries=$((sc_tries + 1))
		if [ "$sc_tries" -ge 60 ]; then
			docker logs "$SC_REGISTRY" >&2 || true
			sc_fail "the registry does not answer at $SC_DIRECT_REG"
		fi
		sleep 1
	done
	sc_say "registry $SC_REGISTRY: the engine pushes to $SC_ENGINE_REG, the tools speak to $SC_DIRECT_REG"
}

# sc_registry_remote <registry> <user>: the registry of a release instead of
# the run's - the public one, over HTTPS, one name for the engine and for the
# tools. The password is $SC_REGISTRY_PASSWORD (never an argument, never
# printed). docker is logged in with a configuration directory of the run,
# which cosign reads as well and which goes with SC_WORK; the helper is given
# the same two for the registry's token exchange.
sc_registry_remote() {
	[ -n "$2" ] && [ -n "$SC_REGISTRY_PASSWORD" ] || sc_fail "no user or no token for $1"
	SC_ENGINE_REG=$1
	SC_DIRECT_REG=$1
	SC_REGISTRY_SCHEME=https
	SC_REGISTRY_USER=$2
	DOCKER_CONFIG="$SC_WORK/docker"
	export DOCKER_CONFIG
	mkdir -p "$DOCKER_CONFIG"
	chmod 700 "$DOCKER_CONFIG"
	if ! printf '%s' "$SC_REGISTRY_PASSWORD" |
		docker login "$1" --username "$2" --password-stdin >"$SC_WORK/login" 2>&1; then
		cat "$SC_WORK/login" >&2
		sc_fail "docker login $1 as $2"
	fi
	sc_helper ping || sc_fail "$1 does not answer, or does not accept the token"
	sc_say "registry $1, as $2"
}

# sc_keyless <identity>: from here on cosign signs as whoever the OIDC token
# of the job says it is, and verifies that it was <identity>. See the head of
# this file and SC_KEYLESS_* above.
sc_keyless() {
	[ -n "$1" ] || sc_fail "sc_keyless: no identity to verify"
	SC_MODE=keyless
	SC_KEYLESS_IDENTITY=$1
}

# sc_need_revision <commit>: both images say they were built from the commit
# (the label scripts/image.sh's build gives them).
sc_need_revision() {
	for sc_arch in $SC_ARCHES; do
		sc_tag=$(sc_local_tag "$sc_arch")
		sc_got=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$sc_tag") ||
			sc_fail "no image $sc_tag; run 'make image' first"
		[ "$sc_got" = "$1" ] || sc_fail "$sc_tag was built from '$sc_got' and this tree is $1; run 'make image' again"
	done
}

# sc_push <image in the engine's store> <repository> <tag>: a second name for
# the image, and the engine pushes it.
sc_push() {
	sc_ref="$SC_ENGINE_REG/$2:$3"
	echo "$sc_ref" >>"$SC_WORK/tags"
	docker tag "$1" "$sc_ref" || sc_fail "docker tag $1 $sc_ref"
	docker push --quiet "$sc_ref" >/dev/null || sc_fail "docker push $sc_ref"
}

# sc_digest <repository> <tag or digest>: the digest of what the registry has
# there, counted here from the bytes it sends.
sc_digest() {
	sc_helper digest "$1" "$2"
}

# --- cosign ----------------------------------------------------------------------

# sc_cosign <arguments>: THE way cosign is run. An empty password for the key
# of the run; no way out of the builder but to the registry. Keyless, it is
# run as it is: it must reach the registry, Fulcio and Rekor, and there is no
# key to have a password.
sc_cosign() {
	if [ "$SC_MODE" = keyless ]; then
		cosign "$@"
		return
	fi
	COSIGN_PASSWORD="" \
		HTTPS_PROXY=$SC_NOWHERE HTTP_PROXY=$SC_NOWHERE https_proxy=$SC_NOWHERE http_proxy=$SC_NOWHERE \
		NO_PROXY="${SC_DIRECT_REG%:*},localhost,127.0.0.1" no_proxy="${SC_DIRECT_REG%:*},localhost,127.0.0.1" \
		cosign "$@"
}

# sc_keygen <directory>: a key pair for this run, cosign.key and cosign.pub in
# the directory, which is under SC_WORK and goes with it. The private key is
# never printed, never copied and never in the repository.
sc_keygen() {
	mkdir -p "$1"
	chmod 700 "$1"
	(cd "$1" && sc_cosign generate-key-pair >/dev/null 2>&1) || sc_fail "cosign generate-key-pair"
	[ -s "$1/cosign.key" ] && [ -s "$1/cosign.pub" ] || sc_fail "cosign generate-key-pair wrote no key pair"
}

# sc_ref <repository> <digest>: an image by digest, as the tools name it.
sc_ref() {
	echo "$SC_DIRECT_REG/$1@$2"
}

# In the four functions below the first argument is the key of the run. Keyless
# there is none and it is ignored ("-" by convention): who signs is the job's
# identity, and who must have signed is SC_KEYLESS_IDENTITY.

# sc_sign <private key> <image by digest>
sc_sign() {
	case $2 in
	*@sha256:*) ;;
	*) sc_fail "refusing to sign $2: an image is signed by digest, never by tag" ;;
	esac
	if [ "$SC_MODE" = keyless ]; then
		# shellcheck disable=SC2086 # the flags are split on purpose
		sc_cosign sign $SC_KEYLESS_SIGN_FLAGS "$2"
		return
	fi
	# shellcheck disable=SC2086 # the flags are split on purpose
	sc_cosign sign --key "$1" $SC_COSIGN_SIGN_FLAGS $SC_COSIGN_REGISTRY_FLAGS "$2"
}

# sc_verify <public key> <image>
sc_verify() {
	if [ "$SC_MODE" = keyless ]; then
		sc_cosign verify --certificate-identity "$SC_KEYLESS_IDENTITY" \
			--certificate-oidc-issuer "$SC_KEYLESS_ISSUER" "$2"
		return
	fi
	# shellcheck disable=SC2086 # the flags are split on purpose
	sc_cosign verify --key "$1" $SC_COSIGN_VERIFY_FLAGS $SC_COSIGN_REGISTRY_FLAGS "$2"
}

# sc_attest <private key> <SBOM> <image by digest>
sc_attest() {
	case $3 in
	*@sha256:*) ;;
	*) sc_fail "refusing to attest $3: an image is attested by digest, never by tag" ;;
	esac
	if [ "$SC_MODE" = keyless ]; then
		# shellcheck disable=SC2086 # the flags are split on purpose
		sc_cosign attest --type "$SC_ATTESTATION_TYPE" --predicate "$2" $SC_KEYLESS_SIGN_FLAGS "$3"
		return
	fi
	# shellcheck disable=SC2086 # the flags are split on purpose
	sc_cosign attest --key "$1" --type "$SC_ATTESTATION_TYPE" --predicate "$2" \
		$SC_COSIGN_SIGN_FLAGS $SC_COSIGN_REGISTRY_FLAGS "$3"
}

# sc_verify_attestation <public key> <image>: the attestations that verify,
# one envelope a line, on standard output.
sc_verify_attestation() {
	if [ "$SC_MODE" = keyless ]; then
		sc_cosign verify-attestation --type "$SC_ATTESTATION_TYPE" \
			--certificate-identity "$SC_KEYLESS_IDENTITY" \
			--certificate-oidc-issuer "$SC_KEYLESS_ISSUER" "$2"
		return
	fi
	# shellcheck disable=SC2086 # the flags are split on purpose
	sc_cosign verify-attestation --key "$1" --type "$SC_ATTESTATION_TYPE" \
		$SC_COSIGN_VERIFY_FLAGS $SC_COSIGN_REGISTRY_FLAGS "$2"
}

# sc_sign_blob <file> <bundle> and sc_verify_blob <file> <bundle>: a file, not
# an image - the checksums of the binaries. Keyless only: the gate has no file
# to sign, and a rehearsal with a key would prove another command line.
sc_sign_blob() {
	[ "$SC_MODE" = keyless ] || sc_fail "sc_sign_blob: a file is signed in a release only"
	# shellcheck disable=SC2086 # the flags are split on purpose
	sc_cosign sign-blob $SC_KEYLESS_SIGN_FLAGS $SC_KEYLESS_BUNDLE_FLAGS --bundle "$2" "$1"
}

sc_verify_blob() {
	[ "$SC_MODE" = keyless ] || sc_fail "sc_verify_blob: a file is signed in a release only"
	# shellcheck disable=SC2086 # the flags are split on purpose
	sc_cosign verify-blob $SC_KEYLESS_BUNDLE_FLAGS --bundle "$2" \
		--certificate-identity "$SC_KEYLESS_IDENTITY" \
		--certificate-oidc-issuer "$SC_KEYLESS_ISSUER" "$1"
}

# --- helm: the chart of a release ----------------------------------------------------

# sc_chart_push <chart directory> <version> <repository the chart goes under>
# <directory for the package>: the chart, packaged as the release (its version
# and its appVersion are the release's) and pushed as an OCI artifact; helm
# puts it at <repository>/<name of the chart>:<version>. helm is logged in
# with a file of the run, which goes with SC_WORK. A release only: the
# registry is sc_registry_remote's.
sc_chart_push() {
	[ "$SC_REGISTRY_SCHEME" = https ] || sc_fail "sc_chart_push: a chart is pushed in a release only"
	HELM_REGISTRY_CONFIG="$SC_WORK/helm-registry.json"
	export HELM_REGISTRY_CONFIG
	sc_name=$(sed -n 's/^name: *//p' "$1/Chart.yaml")
	[ -n "$sc_name" ] || sc_fail "$1/Chart.yaml names no chart"
	mkdir -p "$4"
	if ! helm package "$1" --version "$2" --app-version "$2" --destination "$4" >"$SC_WORK/helm" 2>&1; then
		cat "$SC_WORK/helm" >&2
		sc_fail "helm package $1"
	fi
	[ -s "$4/$sc_name-$2.tgz" ] || sc_fail "helm package wrote no $4/$sc_name-$2.tgz"
	if ! printf '%s' "$SC_REGISTRY_PASSWORD" |
		helm registry login "$SC_DIRECT_REG" --username "$SC_REGISTRY_USER" --password-stdin >"$SC_WORK/helm" 2>&1; then
		cat "$SC_WORK/helm" >&2
		sc_fail "helm registry login $SC_DIRECT_REG"
	fi
	if ! helm push "$4/$sc_name-$2.tgz" "oci://$SC_DIRECT_REG/$3" >"$SC_WORK/helm" 2>&1; then
		cat "$SC_WORK/helm" >&2
		sc_fail "helm push $sc_name-$2.tgz oci://$SC_DIRECT_REG/$3"
	fi
	echo "$3/$sc_name"
}

# sc_must <what, in words> <command>...: the command succeeds. What it wrote
# is in $SC_WORK/stdout and $SC_WORK/stderr.
sc_must() {
	sc_what=$1
	shift
	if "$@" >"$SC_WORK/stdout" 2>"$SC_WORK/stderr"; then
		sc_say "ok - $sc_what"
		return 0
	fi
	cat "$SC_WORK/stdout" "$SC_WORK/stderr" >&2
	sc_fail "$sc_what"
}

# sc_refused <what, in words> <expected class of error> <command>...: the
# command fails, and fails as a verification that says no - not as a tool that
# crashed, was given a flag it does not know or could not reach the registry.
sc_refused() {
	sc_what=$1
	sc_class=$2
	shift 2
	sc_status=0
	"$@" >"$SC_WORK/refused" 2>&1 || sc_status=$?
	if [ "$sc_status" -eq 0 ]; then
		cat "$SC_WORK/refused" >&2
		sc_fail "$sc_what: verification succeeded, and it must not"
	fi
	# What the tool only warns of is not its verdict. cosign 2 asks for the
	# public roots of trust even when it is given a key and told to leave the
	# log alone; here that request ends at the proxy that goes nowhere, cosign
	# says so in a warning and goes on with the key. The warning is the proof
	# that nothing left this machine, not a failure to judge.
	grep -v '^WARNING:' "$SC_WORK/refused" >"$SC_WORK/verdict" || true
	if grep -Eiq "$SC_NO_VERDICT" "$SC_WORK/verdict"; then
		cat "$SC_WORK/refused" >&2
		sc_fail "$sc_what: the tool gave no verdict (status $sc_status)"
	fi
	if ! grep -Eq "$sc_class" "$SC_WORK/verdict"; then
		cat "$SC_WORK/refused" >&2
		sc_fail "$sc_what: failed (status $sc_status), but not with '$sc_class'"
	fi
	sc_say "refused, as it must be - $sc_what"
}

# sc_refused_signature <what> <command>... and sc_refused_attestation <what>
# <command>...: sc_refused, with the way cosign refuses each.
sc_refused_signature() {
	sc_what=$1
	shift
	sc_refused "$sc_what" "$SC_REFUSED_SIGNATURE" "$@"
}

sc_refused_attestation() {
	sc_what=$1
	shift
	sc_refused "$sc_what" "$SC_REFUSED_ATTESTATION" "$@"
}

# sc_transplant signature|attestation <repository> <from digest> <to digest>:
# what cosign keeps for one image becomes what it finds for another. cosign
# keeps both in the repository of the image, under tags made of the image's
# digest, and whoever can write to the registry can copy them.
sc_transplant() {
	case $1 in
	signature) sc_suffix=$SC_SIGNATURE_SUFFIX ;;
	attestation) sc_suffix=$SC_ATTESTATION_SUFFIX ;;
	*) sc_fail "sc_transplant: $1" ;;
	esac
	sc_helper retag "$2" "sha256-${3#sha256:}$sc_suffix" "sha256-${4#sha256:}$sc_suffix" >/dev/null ||
		sc_fail "the $1 of $3 is not where cosign keeps it (sha256-<hex>$sc_suffix)"
}

# sc_attestation_check <image digest>: what sc_verify_attestation left in
# $SC_WORK/stdout is of that digest and its SBOM names the Go module.
sc_attestation_check() {
	sc_helper att-check "$SC_WORK/stdout" "$1" "$SC_PREDICATE_TYPE" "$SC_MODULE" ||
		sc_fail "the attestation of $1 is not what was attested"
}

# --- the helper: JSON, and the registry's HTTP API --------------------------------

# sc_helper <command> <arguments>: see the program below. The registry is the
# run's, by the address the builder reaches it at - or, in a release, the one
# of sc_registry_remote, with the user and the token it asks the registry for
# its own token with. Both are empty in the gate, and the helper then never
# authenticates.
sc_helper() {
	SC_REGISTRY_URL="$SC_REGISTRY_SCHEME://$SC_DIRECT_REG" \
		SC_REGISTRY_USER="$SC_REGISTRY_USER" SC_REGISTRY_PASSWORD="$SC_REGISTRY_PASSWORD" \
		python3 "$SC_WORK/helper.py" "$@"
}

sc_write_helper() {
	cat >"$SC_WORK/helper.py" <<'PY'
"""JSON and the registry's HTTP API, for scripts/lib-supplychain.sh.

    sbom-check <file> <spdx version> <least packages> <name>...
    sbom-license <file> <package> <licence id> <licence name> <text file> <url>
    att-check <envelopes> <image digest> <predicate type> <package name>
    ping
    digest <repository> <reference>
    platform <repository> <reference> <architecture>
    index <repository> <tag> <reference>=<architecture>...
    retag <repository> <from reference> <to tag>

The registry is $SC_REGISTRY_URL: the throw-away one of the run, plain HTTP,
which asks for nothing - or, in a release, the public one over HTTPS, which
answers 401 and says where a token is to be had. A token is asked for only
when $SC_REGISTRY_USER is set, only of the registry's own host, and is sent
to nobody else.
"""
import base64
import hashlib
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

OCI_INDEX = "application/vnd.oci.image.index.v1+json"
DOCKER_LIST = "application/vnd.docker.distribution.manifest.list.v2+json"
OCI_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
DOCKER_MANIFEST = "application/vnd.docker.distribution.manifest.v2+json"
INDEXES = (OCI_INDEX, DOCKER_LIST)
ACCEPT = ", ".join((OCI_INDEX, DOCKER_LIST, OCI_MANIFEST, DOCKER_MANIFEST))
# No proxy, whatever the environment says: the registry is next door.
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


class Failure(Exception):
    """What went wrong, in words for the gate's output."""


# The registry's own token, by the scope it was given for.
TOKENS = {}


def token(challenge):
    """The bearer token a 401 of the registry asks for, or None.

    The exchange of the distribution specification: the registry names a
    realm, a service and a scope; the realm gives a token for the user and
    password (basic). Asked only of the registry's own scheme and host, so
    that the password goes nowhere else, and for pull and push of the
    repository named, whatever the request was.
    """
    user = os.environ.get("SC_REGISTRY_USER", "")
    if not user or not challenge.lower().startswith("bearer "):
        return None
    fields = dict(re.findall(r'(\w+)="([^"]*)"', challenge))
    realm = urllib.parse.urlsplit(fields.get("realm", ""))
    registry = urllib.parse.urlsplit(os.environ["SC_REGISTRY_URL"])
    if (realm.scheme, realm.netloc) != (registry.scheme, registry.netloc):
        raise Failure(f"the registry sends to {fields.get('realm')!r} for a token: not its own host")
    query = {}
    if fields.get("service"):
        query["service"] = fields["service"]
    scope = fields.get("scope", "")
    parts = scope.split(":")
    if len(parts) == 3 and parts[0] == "repository":
        scope = f"repository:{parts[1]}:pull,push"
    if scope:
        query["scope"] = scope
    if scope in TOKENS:
        return TOKENS[scope]
    url = urllib.parse.urlunsplit(realm._replace(query=urllib.parse.urlencode(query)))
    request = urllib.request.Request(url, method="GET")
    basic = base64.b64encode(f"{user}:{os.environ.get('SC_REGISTRY_PASSWORD', '')}".encode()).decode()
    request.add_unredirected_header("Authorization", "Basic " + basic)
    try:
        with OPENER.open(request, timeout=60) as response:
            answer = json.loads(response.read())
    except urllib.error.HTTPError as error:
        raise Failure(f"the registry gives no token for {scope or 'itself'}: {error.code}") from error
    except OSError as error:
        raise Failure(f"the registry gives no token: {error}") from error
    found = answer.get("token") or answer.get("access_token")
    if not found:
        raise Failure(f"the registry's answer for {scope or 'itself'} holds no token")
    TOKENS[scope] = found
    return found


def call(method, path, body=None, content_type=None):
    """One request to the registry: (status, content type, body)."""
    url = os.environ["SC_REGISTRY_URL"] + path
    bearer = None
    while True:
        request = urllib.request.Request(url, data=body, method=method)
        request.add_header("Accept", ACCEPT)
        if content_type:
            request.add_header("Content-Type", content_type)
        if bearer:
            # Not carried along when the registry redirects to where it keeps
            # its blobs: the token is the registry's and nobody else's.
            request.add_unredirected_header("Authorization", "Bearer " + bearer)
        try:
            with OPENER.open(request, timeout=60) as response:
                media = response.headers.get("Content-Type", "").split(";")[0].strip()
                return response.status, media, response.read()
        except urllib.error.HTTPError as error:
            # Once: a token the registry does not take is not asked for again.
            if error.code == 401 and bearer is None:
                bearer = token(error.headers.get("WWW-Authenticate", ""))
                if bearer:
                    continue
            raise Failure(f"{method} {path}: the registry answers {error.code}") from error
        except OSError as error:
            raise Failure(f"{method} {path}: {error}") from error


def manifest(repository, reference):
    """A manifest or an index: (digest, media type, bytes)."""
    _, media, body = call("GET", f"/v2/{repository}/manifests/{reference}")
    digest = "sha256:" + hashlib.sha256(body).hexdigest()
    if reference.startswith("sha256:") and reference != digest:
        raise Failure(f"{repository}@{reference}: the registry sent {digest}")
    if not media:
        media = json.loads(body).get("mediaType", "")
    return digest, media, body


def platform(repository, reference, architecture):
    """The descriptor of the linux/<architecture> image manifest behind a reference."""
    digest, media, body = manifest(repository, reference)
    document = json.loads(body)
    if media in INDEXES:
        # An image built with attestations is an index of the image and of
        # them; their platform is unknown/unknown.
        found = [
            entry
            for entry in document.get("manifests", [])
            if entry.get("platform", {}).get("os") == "linux"
            and entry.get("platform", {}).get("architecture") == architecture
        ]
        if len(found) != 1:
            raise Failure(f"{repository}:{reference}: {len(found)} manifests for linux/{architecture}, want 1")
        child, child_media, child_body = manifest(repository, found[0]["digest"])
        return {
            "mediaType": child_media,
            "digest": child,
            "size": len(child_body),
            "platform": found[0]["platform"],
        }
    _, _, raw = call("GET", f"/v2/{repository}/blobs/{document['config']['digest']}")
    config = json.loads(raw)
    if config.get("os") != "linux" or config.get("architecture") != architecture:
        raise Failure(
            f"{repository}:{reference} is {config.get('os')}/{config.get('architecture')}, want linux/{architecture}"
        )
    wanted = {"architecture": architecture, "os": "linux"}
    if config.get("variant"):
        wanted["variant"] = config["variant"]
    return {"mediaType": media, "digest": digest, "size": len(body), "platform": wanted}


def put(repository, tag, media, body):
    """Stores a manifest under a tag and returns its digest."""
    status, _, _ = call("PUT", f"/v2/{repository}/manifests/{tag}", body, media)
    if status not in (200, 201):
        raise Failure(f"PUT {repository}:{tag}: the registry answers {status}")
    return "sha256:" + hashlib.sha256(body).hexdigest()


def index(repository, tag, sources):
    """A manifest list of the platform images behind the sources."""
    children = []
    for source in sources:
        reference, _, architecture = source.partition("=")
        children.append(platform(repository, reference, architecture))
    if len({child["digest"] for child in children}) != len(children):
        raise Failure("two platforms of the list are the same manifest")
    media = DOCKER_LIST if all(child["mediaType"] == DOCKER_MANIFEST for child in children) else OCI_INDEX
    document = {"schemaVersion": 2, "mediaType": media, "manifests": children}
    return put(repository, tag, media, json.dumps(document, indent=2).encode())


def sbom_check(path, version, least, names):
    """The file is an SPDX document of that version that names the packages."""
    with open(path, encoding="utf-8") as file:
        document = json.load(file)
    if document.get("spdxVersion") != version:
        raise Failure(f"{path}: spdxVersion is {document.get('spdxVersion')!r}, want {version!r}")
    packages = {package.get("name") for package in document.get("packages") or []}
    if len(packages) < least:
        raise Failure(f"{path}: {len(packages)} packages, want at least {least}")
    missing = [name for name in names if name not in packages]
    if missing:
        raise Failure(f"{path}: does not name {', '.join(missing)}")
    return f"{path}: {version}, {len(packages)} packages, names {', '.join(names)}"


def sbom_license(path, name, license_id, license_name, text_file, url):
    """Declares the licence of the package: a licence of the document, with its text.

    syft reads what is in the image; the licence of the module the binary is
    it cannot read. The document says what the licence is in full
    (hasExtractedLicensingInfos), because it is on no list.
    """
    with open(path, encoding="utf-8") as file:
        document = json.load(file)
    with open(text_file, encoding="utf-8") as file:
        text = file.read()
    if not license_id.startswith("LicenseRef-") or not text.strip():
        raise Failure(f"{license_id}: not a licence of the document, or {text_file} is empty")
    declared = 0
    for package in document.get("packages") or []:
        if package.get("name") == name:
            package["licenseDeclared"] = license_id
            declared += 1
    if not declared:
        raise Failure(f"{path}: no package {name} to declare the licence of")
    extracted = [
        entry for entry in document.get("hasExtractedLicensingInfos") or [] if entry.get("licenseId") != license_id
    ]
    extracted.append({"licenseId": license_id, "name": license_name, "extractedText": text, "seeAlsos": [url]})
    document["hasExtractedLicensingInfos"] = extracted
    with open(path, "w", encoding="utf-8") as file:
        json.dump(document, file, indent=1, ensure_ascii=False)
        file.write("\n")
    return f"{path}: {name} is declared {license_id} ({declared} package(s))"


def att_check(path, digest, predicate_type, name):
    """Every envelope is a statement about the digest whose SBOM names the package."""
    checked = 0
    with open(path, encoding="utf-8") as file:
        for line in file:
            if not line.startswith("{"):
                continue
            statement = json.loads(base64.b64decode(json.loads(line)["payload"]))
            if statement.get("predicateType") != predicate_type:
                raise Failure(f"predicate type {statement.get('predicateType')!r}, want {predicate_type!r}")
            subjects = {"sha256:" + subject.get("digest", {}).get("sha256", "") for subject in statement.get("subject", [])}
            if subjects != {digest}:
                raise Failure(f"the statement is about {sorted(subjects)}, want {digest}")
            predicate = statement.get("predicate")
            if isinstance(predicate, str):
                predicate = json.loads(predicate)
            packages = {package.get("name") for package in (predicate or {}).get("packages") or []}
            if name not in packages:
                raise Failure(f"the attested SBOM of {digest} does not name {name}")
            checked += 1
    if not checked:
        raise Failure("no attestation was given out")
    return f"{checked} attestation(s) about {digest}, the SBOM names {name}"


def main(arguments):
    command, rest = arguments[0], arguments[1:]
    if command == "sbom-check":
        print(sbom_check(rest[0], rest[1], int(rest[2]), rest[3:]))
    elif command == "sbom-license":
        print(sbom_license(*rest))
    elif command == "att-check":
        print(att_check(*rest))
    elif command == "ping":
        call("GET", "/v2/")
    elif command == "digest":
        print(manifest(*rest)[0])
    elif command == "platform":
        print(platform(*rest)["digest"])
    elif command == "index":
        print(index(rest[0], rest[1], rest[2:]))
    elif command == "retag":
        _, media, body = manifest(rest[0], rest[1])
        print(put(rest[0], rest[2], media, body))
    else:
        raise Failure(f"unknown command {command}")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except Failure as failure:
        print(f"helper: {failure}", file=sys.stderr)
        sys.exit(1)
PY
}

#!/bin/sh
# Holds the image made from a folder of binaries equal to the product image
# (PLAN 10.2, ADR-0033 section 5). Run through `make image-from-binaries`,
# after `make image` (the product images) and `make image-binaries` (the
# folder, dist/image), which the target runs first.
#
#   ENGINE=docker   in the builder, through the engine socket: BuildKit
#                   (`docker buildx build --build-context`)
#   ENGINE=podman   on the host, with the podman CLI (4.4 or later, which has
#                   `podman build --build-context`): Podman's Docker API,
#                   which the builder talks to, has no named build contexts
#
# For each architecture an image is made of the folder by the Dockerfile's
# from-binaries stage, with the folder as the named build context
# `binaries`, and it must be the product image in all that is seen of it:
#   - the configuration: platform, user, entrypoint, command, ports,
#     environment, working directory, volumes, stop signal, health check,
#     labels, and with Docker the shell (Podman does not show one);
#   - as many layers, of which all but the last two (the binary and the
#     volume directories) are the same: the base image's;
#   - /cactusfs and every entry under /var/lib/cactusfs: kind, mode, owner
#     and name, and /var/lib/cactusfs/data1 a directory of 65532:65532;
#   - the binary, byte for byte: the product's, the folder's (SHA256SUMS) and
#     the new image's are one;
#   - the size limit, and on the engine's own architecture the same
#     `version` line, ending ", image)".
# Then the folder must be what the image is made of, not the stage it stands
# in for: an image made of a copy of the folder whose binary has one more
# byte must hold that binary.
#
# A check that cannot run fails, saying why; it is never passed over. Needs
# the engine's CLI, tar and python3 (the builder has them; on a host, so do
# the Linux distributions Podman runs on).
set -eu

ENGINE=${ENGINE:-docker}
pv=
IMAGE=${IMAGE:-cactusfs}
VERSION=${VERSION:-$(sh "$(dirname "$0")/buildid.sh" version)}
BUILD=${BUILD:-$(sh "$(dirname "$0")/buildid.sh" build)}
COMMIT=${COMMIT-$(sh "$(dirname "$0")/buildid.sh" commit)}
BINARIES=${IMAGE_BINARIES_DIR:-dist/image}
LIMIT=$((60 * 1000 * 1000))
ARCHES="amd64 arm64"

fail() {
	echo "image-from-binaries: FAIL - $*" >&2
	exit 1
}
say() { echo "image-from-binaries: $*"; }

command -v python3 >/dev/null 2>&1 || fail "python3 is needed to read the images' files, and there is none"
case $ENGINE in
docker)
	docker version >/dev/null 2>&1 || fail "no container engine reachable; pass the engine socket (ENGINE_SOCKET=...)"
	if [ "${DOCKER_BUILDKIT:-1}" = 0 ] || ! docker buildx version >/dev/null 2>&1; then
		fail "the check needs BuildKit (a named build context), which this engine's API does not offer; it was not run. With Podman: make image-from-binaries ENGINE=podman, which runs it with the podman CLI"
	fi
	native=$(docker version --format '{{.Server.Arch}}')
	;;
podman)
	pv=$(podman version --format '{{.Client.Version}}' 2>/dev/null) || fail "no podman reachable"
	major=${pv%%.*}
	minor=${pv#*.}
	minor=${minor%%.*}
	case $major$minor in
	'' | *[!0-9]*) fail "podman says it is version '$pv', which this script cannot read; it needs 4.4 or later" ;;
	esac
	if [ "$major" -lt 4 ] || { [ "$major" -eq 4 ] && [ "$minor" -lt 4 ]; }; then
		fail "podman $pv has no named build contexts (podman build --build-context); the check needs 4.4 or later, and was not run"
	fi
	native=$(podman info --format '{{.Host.Arch}}')
	;;
*) fail "ENGINE '$ENGINE': want docker or podman" ;;
esac

sum() {
	python3 -c 'import hashlib, sys; print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())' "$1"
}
# The sum SHA256SUMS lists for a path, or nothing.
listed() {
	sed -n "s|^\([0-9a-f]*\)  $1\$|\1|p" "$BINARIES/SHA256SUMS"
}

[ -f "$BINARIES/SHA256SUMS" ] && [ -f "$BINARIES/BUILDINFO" ] || fail "no $BINARIES/SHA256SUMS or BUILDINFO; run 'make image-binaries' first"
for arch in $ARCHES; do
	[ -f "$BINARIES/linux-$arch/cactusfs" ] || fail "no $BINARIES/linux-$arch/cactusfs; run 'make image-binaries'"
	[ "$(sum "$BINARIES/linux-$arch/cactusfs")" = "$(listed "linux-$arch/cactusfs")" ] ||
		fail "$BINARIES/linux-$arch/cactusfs does not match its SHA256SUMS"
done
want=$(printf 'version=%s\nbuild=%s\ncommit=%s\npackaging=image' "$VERSION" "$BUILD" "$COMMIT")
[ "$(cat "$BINARIES/BUILDINFO")" = "$want" ] || fail "$BINARIES/BUILDINFO is not of this build ($VERSION-$BUILD, commit ${COMMIT:-unknown}); run 'make image-binaries'"

work=$(mktemp -d)
containers=
images=
cleanup() {
	for c in $containers; do "$ENGINE" rm -f "$c" >/dev/null 2>&1 || true; done
	for i in $images; do "$ENGINE" image rm -f "$i" >/dev/null 2>&1 || true; done
	rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

build_from() { # folder arch tag
	images="$images $3"
	case $ENGINE in
	docker)
		docker buildx build --quiet --load --platform "linux/$2" --target from-binaries \
			--build-context "binaries=$1" \
			--build-arg "VERSION=$VERSION" --build-arg "BUILD=$BUILD" --build-arg "COMMIT=$COMMIT" -t "$3" . >/dev/null
		;;
	podman)
		# The Docker image format, as Podman's Docker API builds the
		# product image with.
		podman build --quiet --format docker --platform "linux/$2" --target from-binaries \
			--build-context "binaries=$1" \
			--build-arg "VERSION=$VERSION" --build-arg "BUILD=$BUILD" --build-arg "COMMIT=$COMMIT" -t "$3" . >/dev/null
		;;
	esac
}

# What is seen of an image from outside, but its layers. The two engines
# show the health check in different places, and Podman no shell.
config() {
	case $ENGINE in
	docker) engine_fields='healthcheck={{json .Config.Healthcheck}} shell={{json .Config.Shell}}' ;;
	podman) engine_fields='healthcheck={{json .HealthCheck}}' ;;
	esac
	"$ENGINE" image inspect --format \
		"os={{.Os}}/{{.Architecture}} user={{json .Config.User}} entrypoint={{json .Config.Entrypoint}} cmd={{json .Config.Cmd}} ports={{json .Config.ExposedPorts}} env={{json .Config.Env}} workdir={{json .Config.WorkingDir}} volumes={{json .Config.Volumes}} stopsignal={{json .Config.StopSignal}} $engine_fields labels={{json .Config.Labels}}" \
		"$1"
}

# The layers of an image, one a line.
layers() {
	"$ENGINE" image inspect --format '{{range .RootFS.Layers}}{{println .}}{{end}}' "$1" | sed '/^$/d'
}

# Kind, mode, owner (by number) and name of each entry of a tar file, sorted.
listing() {
	python3 -c '
import sys, tarfile
with tarfile.open(sys.argv[1]) as t:
    for m in t.getmembers():
        kind = "d" if m.isdir() else "-" if m.isfile() else "l" if m.issym() else "?"
        name = m.name.rstrip("/") + ("/" if m.isdir() else "")
        print("%s%04o %d/%d %s" % (kind, m.mode & 0o7777, m.uid, m.gid, name))
' "$1" | LC_ALL=C sort
}

# The binary of an image into <dir>/cactusfs, and into <dir>.list the
# listing of it and of /var/lib/cactusfs. Each copy goes to a file first, so
# that a failed copy fails the run.
files() { # image dir
	c=$("$ENGINE" create "$1")
	containers="$containers $c"
	mkdir "$2"
	"$ENGINE" cp "$c:/cactusfs" - >"$2.bin.tar"
	"$ENGINE" cp "$c:/var/lib/cactusfs" - >"$2.var.tar"
	"$ENGINE" rm "$c" >/dev/null
	tar -xf "$2.bin.tar" -C "$2"
	listing "$2.bin.tar" >"$2.list"
	listing "$2.var.tar" >>"$2.list"
}

say "engine $ENGINE${pv:+ $pv}, native linux/$native"
for arch in $ARCHES; do
	tag="$IMAGE:$VERSION-$BUILD-$arch"
	from="$IMAGE:$VERSION-$BUILD-$arch-from-binaries"
	"$ENGINE" image inspect "$tag" >/dev/null 2>&1 || fail "no image $tag in $ENGINE's store; run 'make image' first"
	say "building $from from $BINARIES"
	build_from "$BINARIES" "$arch" "$from"

	expected=$(config "$tag")
	got=$(config "$from")
	if [ "$got" != "$expected" ]; then
		echo "  $tag: $expected" >&2
		echo "  $from: $got" >&2
		fail "$from is not configured as $tag (above)"
	fi

	layers "$tag" >"$work/$arch.product.layers"
	layers "$from" >"$work/$arch.from.layers"
	n=$(wc -l <"$work/$arch.product.layers" | tr -d ' ')
	[ "$n" -ge 2 ] || fail "$tag has $n layers, want the base image's and its own"
	[ "$(wc -l <"$work/$arch.from.layers" | tr -d ' ')" = "$n" ] || fail "$from has not the $n layers of $tag"
	# The layers both begin with are the base image's: all but the last
	# two with BuildKit (the binary, the volume directories); buildah may
	# commit a stage's own changes as one layer.
	common=$(paste -d' ' "$work/$arch.product.layers" "$work/$arch.from.layers" | awk '$1 == $2 { c++; next } { exit } END { print c + 0 }')
	[ "$common" -ge "$((n - 2))" ] || fail "$from is not made on the base image of $tag (they share $common of $n layers)"

	files "$tag" "$work/$arch.product"
	files "$from" "$work/$arch.from"
	if ! diff "$work/$arch.product.list" "$work/$arch.from.list" >&2; then
		fail "the files of $from differ from those of $tag (above: < product, > from binaries)"
	fi
	grep -q '^d[0-7]* 65532/65532 cactusfs/data1/$' "$work/$arch.product.list" ||
		fail "/var/lib/cactusfs/data1 of $tag is not a directory of 65532:65532"

	product_sum=$(sum "$work/$arch.product/cactusfs")
	from_sum=$(sum "$work/$arch.from/cactusfs")
	folder_sum=$(listed "linux-$arch/cactusfs")
	if [ -z "$folder_sum" ] || [ "$product_sum" != "$folder_sum" ] || [ "$from_sum" != "$folder_sum" ]; then
		fail "linux/$arch binaries differ: $tag $product_sum, $BINARIES ${folder_sum:-none}, $from $from_sum"
	fi

	size=$("$ENGINE" image inspect --format '{{.Size}}' "$from")
	[ "$size" -lt "$LIMIT" ] || fail "$from is $size bytes, limit is $LIMIT"

	versions="one binary ($folder_sum)"
	if [ "$arch" = "$native" ]; then
		expected=$("$ENGINE" run --rm --network none "$tag" version)
		got=$("$ENGINE" run --rm --network none "$from" version)
		[ "$got" = "$expected" ] || fail "$from says '$got', $tag says '$expected'"
		case $got in
		*", image)") ;;
		*) fail "$from says '$got': not packaged as an image" ;;
		esac
		versions="$versions, both say '$got'"
	fi

	# The folder is what the image is made of: one more byte in the binary
	# of a copy, and the image holds that binary.
	mkdir "$work/$arch.marked"
	cp -R "$BINARIES/." "$work/$arch.marked/"
	printf '\n' >>"$work/$arch.marked/linux-$arch/cactusfs"
	marked_sum=$(sum "$work/$arch.marked/linux-$arch/cactusfs")
	build_from "$work/$arch.marked" "$arch" "$from-marked"
	files "$from-marked" "$work/$arch.marked-image"
	[ "$(sum "$work/$arch.marked-image/cactusfs")" = "$marked_sum" ] ||
		fail "an image made of a changed folder does not hold the folder's binary: the named build context 'binaries' was not used"

	"$ENGINE" image rm "$from" "$from-marked" >/dev/null
	say "$from equals $tag: $((size / 1000 / 1000)) MB (limit 60 MB); $versions; made of the folder"
done

say "PASS"

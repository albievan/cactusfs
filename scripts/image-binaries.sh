#!/bin/sh
# Writes the binaries of the product image, as files, for an image to be made
# from them (the Dockerfile's from-binaries stage; PLAN 10.2, ADR-0033
# section 5). Run through `make image-binaries`.
#
#   dist/image/linux-amd64/cactusfs
#   dist/image/linux-arm64/cactusfs
#   dist/image/SHA256SUMS      sha256sum of the two binaries, paths relative to dist/image
#   dist/image/BUILDINFO       version=, build=, commit=, packaging=image
#
# They come from the image-binaries stage, which copies them from the build
# stage the product image is made of: the same binaries, built as an image's
# (packaging image), which never offer to replace themselves. The binaries of
# `make build-all` are not these, and are no substitute.
#
# The stage is built as an image of each architecture, a container is created
# from it - never started - and the files are copied out of it: what every
# engine and builder can do (BuildKit, buildah through Podman's API). The Go
# stage cross-compiles on the build host, so no emulation is involved. The
# binary of the engine's own architecture is then run once, and must say the
# build it was given and that it is packaged as an image.
#
#   IMAGE_BINARIES_DIR=<dir>   write there instead of dist/image: a directory
#                              under dist/, replaced only if it is one this
#                              script wrote (it holds SHA256SUMS)
set -eu

VERSION=${VERSION:-$(sh "$(dirname "$0")/buildid.sh" version)}
BUILD=${BUILD:-$(sh "$(dirname "$0")/buildid.sh" build)}
COMMIT=${COMMIT-$(sh "$(dirname "$0")/buildid.sh" commit)}
OUT=${IMAGE_BINARIES_DIR:-dist/image}
ARCHES="amd64 arm64"

fail() {
	echo "image-binaries: FAIL - $*" >&2
	exit 1
}

case /$OUT/ in
*/../* | */./* | *//*) fail "IMAGE_BINARIES_DIR '$OUT' must be a plain path under dist/" ;;
esac
case $OUT in
dist/?*) ;;
*) fail "IMAGE_BINARIES_DIR '$OUT' is not under dist/" ;;
esac
if [ -e "$OUT" ] && [ ! -f "$OUT/SHA256SUMS" ]; then
	fail "$OUT exists and holds no SHA256SUMS: it was not written by this script, and is not replaced"
fi
if ! docker version >/dev/null 2>&1; then
	fail "no container engine reachable; pass the engine socket (ENGINE_SOCKET=...)"
fi
native=$(docker version --format '{{.Server.Arch}}')
case " $ARCHES " in
*" $native "*) ;;
*) fail "the engine's architecture '$native' is not one of $ARCHES: no binary could be run to say it is packaged as an image" ;;
esac

# Written next to OUT and moved into place when whole, so that OUT is never
# half of one build and half of another.
parent=$(dirname "$OUT")
mkdir -p "$parent"
work=$(mktemp -d "$parent/.image-binaries.XXXXXX")
run=image-binaries-$(basename "$work" | tr 'A-Z' 'a-z' | tr -cd 'a-z0-9')
containers=
images=
cleanup() {
	for c in $containers; do docker rm -f "$c" >/dev/null 2>&1 || true; done
	for i in $images; do docker image rm -f "$i" >/dev/null 2>&1 || true; done
	rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

want=$(printf 'version=%s\nbuild=%s\ncommit=%s\npackaging=image' "$VERSION" "$BUILD" "$COMMIT")
mkdir "$work/out"
for arch in $ARCHES; do
	tag="cactusfs-$run:$arch"
	echo "image-binaries: linux/$arch (commit ${COMMIT:-unknown})"
	images="$images $tag"
	docker build --quiet --platform "linux/$arch" --target image-binaries \
		--build-arg "VERSION=$VERSION" --build-arg "BUILD=$BUILD" --build-arg "COMMIT=$COMMIT" -t "$tag" . >/dev/null

	# Created, never started: the command is only what create asks for.
	c=$(docker create --platform "linux/$arch" "$tag" "/linux-$arch/cactusfs")
	containers="$containers $c"
	mkdir "$work/$arch"
	for file in "linux-$arch/cactusfs" BUILDINFO; do
		docker cp "$c:/$file" - >"$work/$arch/copy.tar"
		tar -xf "$work/$arch/copy.tar" -C "$work/$arch"
	done
	docker rm "$c" >/dev/null

	bin=$work/$arch/cactusfs
	[ -f "$bin" ] || fail "the image of linux/$arch holds no /linux-$arch/cactusfs"
	[ -f "$work/$arch/BUILDINFO" ] || fail "the image of linux/$arch holds no /BUILDINFO"
	got=$(cat "$work/$arch/BUILDINFO")
	[ "$got" = "$want" ] || fail "linux/$arch: BUILDINFO says '$got', want '$want'"
	# What the binary records of how it was built: the platform, and no cgo.
	info=$(go version -m "$bin") || fail "linux/$arch: go version -m cannot read the binary"
	for setting in GOOS=linux "GOARCH=$arch" CGO_ENABLED=0; do
		printf '%s\n' "$info" | grep -q "^[[:space:]]*build[[:space:]]*$setting\$" ||
			fail "linux/$arch: the binary was not built with $setting"
	done

	# The binary of the engine's own architecture says what it is; the
	# other cannot be run without emulation, and is the same build of the
	# same source for the other platform (its BUILDINFO above).
	if [ "$arch" = "$native" ]; then
		toolchain=$(printf '%s\n' "$info" | sed -n '1s/^.*: //p')
		if [ -n "$BUILD" ] && [ "$BUILD" != 0 ]; then line="cactusfs $VERSION build $BUILD"; else line="cactusfs $VERSION local build"; fi
		line="$line (${COMMIT:+commit $COMMIT, }$toolchain linux/$arch, image)"
		said=$(docker run --rm --network none --platform "linux/$arch" "$tag" "/linux-$arch/cactusfs" version)
		[ "$said" = "$line" ] || fail "linux/$arch: the binary says '$said', want '$line'"
		echo "image-binaries: linux/$arch says '$said'"
	fi
	docker image rm "$tag" >/dev/null

	mkdir "$work/out/linux-$arch"
	mv "$bin" "$work/out/linux-$arch/cactusfs"
	chmod 0755 "$work/out/linux-$arch/cactusfs"
	cp "$work/$arch/BUILDINFO" "$work/out/BUILDINFO"
done
(
	cd "$work/out"
	for arch in $ARCHES; do sha256sum "linux-$arch/cactusfs"; done >SHA256SUMS
	sha256sum --quiet -c SHA256SUMS
)

rm -rf "$OUT"
mv "$work/out" "$OUT"
sed 's/^/image-binaries: /' "$OUT/SHA256SUMS"
echo "image-binaries: wrote $OUT ($(tr '\n' ' ' <"$OUT/BUILDINFO" | sed 's/ $//'))"
echo "image-binaries: PASS"

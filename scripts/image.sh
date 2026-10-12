#!/bin/sh
# Builds the product image for linux/amd64 and linux/arm64 and enforces the
# size limit. Run through `make image`.
#
# One local image per architecture: cactusfs:<version>-<build>-amd64 and
# -arm64, where <version> is the release and <build> the number of the build
# (scripts/buildid.sh; `make image` passes them, with the commit). The Go
# build stage cross-compiles on the build host, so no emulation is involved.
# Nothing is published here: the signed multi-arch manifest list of a release
# is made of these two by scripts/release.sh, in the release workflow.
set -eu

IMAGE=${IMAGE:-cactusfs}
VERSION=${VERSION:-$(sh "$(dirname "$0")/buildid.sh" version)}
BUILD=${BUILD:-$(sh "$(dirname "$0")/buildid.sh" build)}
COMMIT=${COMMIT-$(sh "$(dirname "$0")/buildid.sh" commit)}
LIMIT=$((60 * 1000 * 1000))

if ! docker version >/dev/null 2>&1; then
	echo "image: FAIL - no container engine reachable; pass the engine socket (ENGINE_SOCKET=...)" >&2
	exit 1
fi

for arch in amd64 arm64; do
	tag="$IMAGE:$VERSION-$BUILD-$arch"
	echo "image: building $tag (commit ${COMMIT:-unknown})"
	docker build --quiet --platform "linux/$arch" \
		--build-arg "VERSION=$VERSION" --build-arg "BUILD=$BUILD" --build-arg "COMMIT=$COMMIT" -t "$tag" . >/dev/null

	got=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$tag")
	if [ "$got" != "linux/$arch" ]; then
		echo "image: FAIL - $tag is $got, want linux/$arch" >&2
		exit 1
	fi
	user=$(docker image inspect --format '{{.Config.User}}' "$tag")
	case "$user" in
	"" | 0 | 0:* | root | root:*)
		echo "image: FAIL - $tag runs as root (user '$user')" >&2
		exit 1
		;;
	esac
	# The image says which build it is, as its tag does.
	label=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$tag")
	if [ "$label" != "$VERSION-$BUILD" ]; then
		echo "image: FAIL - $tag is labelled version '$label', want '$VERSION-$BUILD'" >&2
		exit 1
	fi
	size=$(docker image inspect --format '{{.Size}}' "$tag")
	if [ "$size" -ge "$LIMIT" ]; then
		echo "image: FAIL - $tag is $size bytes, limit is $LIMIT" >&2
		exit 1
	fi
	echo "image: $tag linux/$arch user=$user $((size / 1000 / 1000)) MB (limit 60 MB)"
done

echo "image: tagged $IMAGE:$VERSION-$BUILD-amd64 and $IMAGE:$VERSION-$BUILD-arm64"
echo "image: PASS"

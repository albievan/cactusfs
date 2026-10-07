#!/bin/sh
# Builds the product image for linux/amd64 and linux/arm64 and enforces the
# size limit. Run through `make image`.
#
# One local image per architecture: cactusfs:<version>-amd64 and -arm64. The Go
# build stage cross-compiles on the build host, so no emulation is involved.
# Publishing a signed multi-arch manifest is PLAN step 7.3.
set -eu

IMAGE=${IMAGE:-cactusfs}
VERSION=${VERSION:-dev}
LIMIT=$((60 * 1000 * 1000))

if ! docker version >/dev/null 2>&1; then
	echo "image: FAIL - no container engine reachable; pass the engine socket (ENGINE_SOCKET=...)" >&2
	exit 1
fi

for arch in amd64 arm64; do
	tag="$IMAGE:$VERSION-$arch"
	echo "image: building $tag"
	docker build --quiet --platform "linux/$arch" --build-arg "VERSION=$VERSION" -t "$tag" . >/dev/null

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
	size=$(docker image inspect --format '{{.Size}}' "$tag")
	if [ "$size" -ge "$LIMIT" ]; then
		echo "image: FAIL - $tag is $size bytes, limit is $LIMIT" >&2
		exit 1
	fi
	echo "image: $tag linux/$arch user=$user $((size / 1000 / 1000)) MB (limit 60 MB)"
done

echo "image: PASS"

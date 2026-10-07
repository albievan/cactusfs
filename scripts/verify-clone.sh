#!/bin/sh
# Fresh-clone gate. Proves that a clean export of the repository builds
#
#   1. with Go alone:               go build ./cmd/cactusfs   (stock golang image)
#   2. with a container engine alone: docker build .
#
# and that nothing outside the repository is read. Run through `make verify-clone`.
#
# The export is `git archive` of the working tree as it would be committed
# (tracked and untracked files, minus .gitignore), so the gate can run before
# the commit it guards. The tree is built in a temporary index and object
# directory: nothing is written into .git.
set -eu

if ! docker version >/dev/null 2>&1; then
	echo "verify-clone: FAIL - no container engine reachable; pass the engine socket (ENGINE_SOCKET=...)" >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

objects=$(cd "$(git rev-parse --git-path objects)" && pwd)
mkdir "$work/objects"
tree=$(
	export GIT_INDEX_FILE="$work/index"
	export GIT_OBJECT_DIRECTORY="$work/objects"
	export GIT_ALTERNATE_OBJECT_DIRECTORIES="$objects"
	git add -A
	git write-tree
)
GIT_OBJECT_DIRECTORY="$work/objects" GIT_ALTERNATE_OBJECT_DIRECTORIES="$objects" \
	git archive --format=tar "$tree" >"$work/src.tar"
echo "verify-clone: exported tree $tree ($(tar -tf "$work/src.tar" | grep -vc '/$') files)"

# The stock Go image is the one pinned in the exported Dockerfile.
go_image=$(tar -xOf "$work/src.tar" Dockerfile | sed -n 's/^ARG GO_IMAGE=//p')
if [ -z "$go_image" ]; then
	echo "verify-clone: FAIL - GO_IMAGE pin not found in Dockerfile" >&2
	exit 1
fi

echo "verify-clone: 1/2 Go only - go build ./cmd/cactusfs in $go_image"
docker run --rm -i "$go_image" sh -euc '
	mkdir /clone && tar -xf - -C /clone && cd /clone
	GOTOOLCHAIN=local go build ./cmd/cactusfs
	./cactusfs version
' <"$work/src.tar"

echo "verify-clone: 2/2 container engine only - docker build ."
docker build --quiet -t cactusfs:verify-clone - <"$work/src.tar" >/dev/null
docker run --rm cactusfs:verify-clone version
docker image rm cactusfs:verify-clone >/dev/null

echo "verify-clone: PASS"

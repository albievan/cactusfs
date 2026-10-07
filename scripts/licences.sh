#!/bin/sh
# Licence gate. Every dependency that ships in the binary - Go modules linked
# into cmd/cactusfs on any of the six release platforms, and the console's
# production packages - must carry a licence from the allow-list in CLAUDE.md
# section 3. Run through `make licences`.
set -eu

ALLOWED="MIT,BSD-2-Clause,BSD-3-Clause,ISC,Apache-2.0,MPL-2.0"
MODULE=github.com/albievan/cactusfs
PLATFORMS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for platform in $PLATFORMS; do
	os=${platform%/*}
	arch=${platform#*/}

	# Modules linked into the binary on this platform. The project's own
	# packages and the standard library are not dependencies. `go list` runs
	# on its own so that its failure fails the gate.
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
		go list -deps -tags console -f '{{with .Module}}{{.Path}}{{end}}' ./cmd/... >"$work/list"
	if [ ! -s "$work/list" ]; then
		echo "licences: FAIL - go list returned nothing for $platform" >&2
		exit 1
	fi
	if ! grep -qx "$MODULE" "$work/list"; then
		echo "licences: FAIL - go list for $platform does not include $MODULE itself" >&2
		exit 1
	fi
	sort -u "$work/list" | grep -vx "$MODULE" >"$work/modules" || true

	if [ ! -s "$work/modules" ]; then
		echo "licences: go $platform - links no third-party module"
		continue
	fi
	echo "licences: go $platform - checking $(wc -l <"$work/modules" | tr -d ' ') modules"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
		go tool go-licenses check --allowed_licenses="$ALLOWED" --ignore "$MODULE" ./cmd/...
done

node scripts/web-licences.mjs "$ALLOWED"

echo "licences: PASS"

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

# Modules whose licence the checker cannot classify, read by a person: each
# is Apache-2.0, but its LICENSE file is the short notice ("Licensed under
# the Apache License, Version 2.0 ...") rather than the text of the licence,
# and the checker says it "did not find" one. They are let through at the
# version that was read and only while that notice is still their LICENSE;
# another version is read again before it is added here. Their own
# dependencies are still checked. (PLAN P.4, 2026-11-09: sigstore-go's.)
REVIEWED="
github.com/cyberphone/json-canonicalization@v0.0.0-20241213102144-19d51d7fe467
github.com/in-toto/attestation@v1.2.0
github.com/in-toto/in-toto-golang@v0.11.0
"
ignore="--ignore $MODULE"
for reviewed in $REVIEWED; do
	module=${reviewed%@*}
	want=${reviewed#*@}
	got=$(go list -m -f '{{.Version}}' "$module" 2>/dev/null) || got=
	if [ -z "$got" ]; then
		# No longer required at all: nothing to let through.
		continue
	fi
	if [ "$got" != "$want" ]; then
		echo "licences: FAIL - $module is $got and its licence was read at $want: read it again and update REVIEWED" >&2
		exit 1
	fi
	dir=$(go list -m -f '{{.Dir}}' "$module")
	if ! grep -q 'Licensed under the Apache License, Version 2.0' "$dir/LICENSE"; then
		echo "licences: FAIL - $module@$got: its LICENSE is no longer the Apache-2.0 notice that was read" >&2
		exit 1
	fi
	ignore="$ignore --ignore $module"
done

# The checker is built once for this machine. Running it through `go tool`
# inside the loop would cross-compile the checker itself for each platform.
go build -o "$work/go-licenses" github.com/google/go-licenses/v2

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

	# The binary links Raft, bbolt, gRPC and more: a list without any of
	# them is a list that was not read, and nothing would be checked.
	if [ ! -s "$work/modules" ]; then
		echo "licences: FAIL - go list for $platform names no module but $MODULE: nothing to check" >&2
		exit 1
	fi
	echo "licences: go $platform - checking $(wc -l <"$work/modules" | tr -d ' ') modules"
	# shellcheck disable=SC2086 # the --ignore flags are split on purpose
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
		"$work/go-licenses" check --allowed_licenses="$ALLOWED" $ignore ./cmd/... 2>"$work/log" || {
		cat "$work/log" >&2
		echo "licences: FAIL - go $platform" >&2
		exit 1
	}
done

node scripts/web-licences.mjs "$ALLOWED"

echo "licences: PASS"

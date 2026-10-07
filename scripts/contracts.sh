#!/bin/sh
# Contract gate. Run through `make contracts`.
#
#   1. Generated code is in sync: regenerating changes nothing.
#   2. Every API contract in the repository is validated and checked for
#      breaking changes by the tool named below.
#
# A contract whose checker has not been wired FAILS the gate, so a contract can
# never be added without its check.
set -eu

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Writes a checksum of every file git would commit (deleted files are skipped).
fingerprint() {
	git ls-files -co --exclude-standard >"$work/files"
	if [ ! -s "$work/files" ]; then
		echo "contracts: FAIL - git listed no files" >&2
		exit 1
	fi
	while IFS= read -r file; do
		if [ -f "$file" ]; then
			sha256sum "$file"
		fi
	done <"$work/files" >"$1"
	if [ ! -s "$1" ]; then
		echo "contracts: FAIL - could not checksum the tree" >&2
		exit 1
	fi
}

fingerprint "$work/before"
scripts/gen.sh
fingerprint "$work/after"
before=$(sha256sum <"$work/before")
after=$(sha256sum <"$work/after")
if [ "$before" != "$after" ]; then
	echo "contracts: FAIL - 'make gen' changed the tree; commit the generated code:" >&2
	diff "$work/before" "$work/after" >&2 || true
	exit 1
fi
echo "contracts: generated code is in sync"

status=0
pending() {
	# $1 = path, $2 = what must be wired, $3 = PLAN step
	if [ -n "$(find "$1" -type f 2>/dev/null | head -n 1)" ]; then
		echo "contracts: FAIL - $1 exists but $2 is not wired into scripts/contracts.sh (PLAN $3)" >&2
		status=1
	else
		echo "contracts: $1 - no contract yet ($2 arrives with PLAN $3)"
	fi
}

pending api/openapi "oapi-codegen diff + oasdiff" 5.1
pending api/asyncapi "AsyncAPI validation" 3.6
pending api/proto "buf lint + buf breaking" 3.2

[ "$status" -eq 0 ] && echo "contracts: PASS"
exit "$status"

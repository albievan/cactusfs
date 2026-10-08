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


# Management API: no change that breaks a client of the previous contract.
# (That the generated server, the JSON form and the web client are in sync
# with the contract is the first check above.) The reference is the commit
# this work is based on, as for the node RPC below.
admin=api/openapi/admin.yaml
admin_base=${CONTRACT_BASE:-HEAD}
case "$admin_base" in
*[!0]*) ;;
*) admin_base=HEAD ;;
esac
if [ ! -s "$admin" ]; then
	echo "contracts: FAIL - $admin is missing" >&2
	status=1
elif ! git rev-parse --verify --quiet "$admin_base^{commit}" >/dev/null; then
	echo "contracts: FAIL - reference commit '$admin_base' not found" >&2
	status=1
elif git cat-file -e "$admin_base:$admin" 2>/dev/null; then
	git show "$admin_base:$admin" >"$work/admin-base.yaml"
	if oasdiff breaking "$work/admin-base.yaml" "$admin" --fail-on ERR; then
		echo "contracts: management API - no breaking change against $admin_base"
	else
		echo "contracts: FAIL - breaking change in $admin against $admin_base;" \
			"a change that breaks clients belongs in a new /api/v2" >&2
		status=1
	fi
else
	# Nothing to compare with: check that the tool reads the contract.
	if oasdiff breaking "$admin" "$admin" --fail-on ERR >/dev/null; then
		echo "contracts: management API - $admin_base has no contract yet, nothing to compare with"
	else
		echo "contracts: FAIL - oasdiff cannot read $admin" >&2
		status=1
	fi
fi

# Events: every AsyncAPI document must be valid. The validator runs on the
# packages of web/, so those must be installed (`make contracts` does it).
found=0
for doc in api/asyncapi/*.yaml; do
	[ -f "$doc" ] || continue
	found=1
	if ! node scripts/asyncapi-validate.mjs "$doc"; then
		status=1
	fi
done
if [ "$found" -eq 0 ]; then
	echo "contracts: FAIL - api/asyncapi holds no event contract" >&2
	status=1
fi

# Node RPC: style, formatting, and no change that breaks a node of the
# previous release. The reference is the commit this work is based on;
# CONTRACT_BASE overrides it (CI sets it to the target branch).
if ! buf lint; then
	echo "contracts: FAIL - buf lint" >&2
	status=1
fi
if ! buf format --diff --exit-code >/dev/null; then
	echo "contracts: FAIL - api/proto is not formatted; run 'buf format -w'" >&2
	status=1
fi
base=${CONTRACT_BASE:-HEAD}
case "$base" in
*[!0]*) ;;
*) base=HEAD ;; # all zeros: the first push of a branch has no "before"
esac
if ! git rev-parse --verify --quiet "$base^{commit}" >/dev/null; then
	echo "contracts: FAIL - reference commit '$base' not found" >&2
	status=1
elif git ls-tree -r --name-only "$base" -- api/proto | grep -q '\.proto$'; then
	if buf breaking --against ".git#ref=$base"; then
		echo "contracts: node RPC - no breaking change against $base"
	else
		echo "contracts: FAIL - breaking change in api/proto against $base" >&2
		status=1
	fi
else
	echo "contracts: node RPC - $base has no contract yet, nothing to compare with"
fi

[ "$status" -eq 0 ] && echo "contracts: PASS"
exit "$status"

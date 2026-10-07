#!/bin/sh
# Gate-bypass gate (CLAUDE.md section 10): no lint suppression, no skipped
# test, no -short in Go sources. Findings are fixed, never silenced.
# Run through `make lint`.
set -eu

files=$(git ls-files -co --exclude-standard -- '*.go')
if [ -z "$files" ]; then
	echo "no-bypass: FAIL - no Go files found" >&2
	exit 1
fi

status=0
# shellcheck disable=SC2086 # the file list is split on purpose
hits=$(grep -nE '//[[:space:]]*nolint|\.Skip(f|Now)?\(|testing\.Short\(\)' $files) || status=$?
case "$status" in
0)
	echo "no-bypass: FAIL - lint suppressions or skipped tests:" >&2
	echo "$hits" >&2
	exit 1
	;;
1) echo "no-bypass: PASS" ;;
*)
	echo "no-bypass: FAIL - grep did not run" >&2
	exit 1
	;;
esac

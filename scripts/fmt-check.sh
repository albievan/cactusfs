#!/bin/sh
# Format gate: every Go file is gofumpt- and goimports-clean. A formatter that
# fails to run fails the gate. Run through `make fmt-check`.
set -eu

MODULE=github.com/albievan/cactusfs

files=$(git ls-files -co --exclude-standard -- '*.go')
if [ -z "$files" ]; then
	echo "fmt-check: FAIL - no Go files found" >&2
	exit 1
fi

# shellcheck disable=SC2086 # the file list is split on purpose; Go paths here have no spaces
gofumpt=$(go tool gofumpt -l $files) || {
	echo "fmt-check: FAIL - gofumpt did not run" >&2
	exit 1
}
# shellcheck disable=SC2086
goimports=$(go tool goimports -l -local "$MODULE" $files) || {
	echo "fmt-check: FAIL - goimports did not run" >&2
	exit 1
}

unformatted=$(printf '%s\n%s\n' "$gofumpt" "$goimports" | sed '/^$/d' | sort -u)
if [ -n "$unformatted" ]; then
	echo "fmt-check: FAIL - run 'make fmt' on:" >&2
	echo "$unformatted" >&2
	exit 1
fi
echo "fmt-check: PASS ($(echo "$files" | wc -l | tr -d ' ') files)"

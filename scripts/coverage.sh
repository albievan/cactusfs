#!/bin/sh
# Unit-test gate: runs the unit tests with the race detector and enforces the
# coverage floor on the domain model and the use cases. Run through `make cover`.
set -eu

MODULE=github.com/albievan/cactusfs
MINIMUM=80
TREES="internal/domain internal/app"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

go test -race -count=1 -cover -coverprofile="$work/all.out" ./...

status=0
for tree in $TREES; do
	head -n 1 "$work/all.out" >"$work/tree.out"
	grep "^$MODULE/$tree/" "$work/all.out" >>"$work/tree.out" || true
	if [ "$(wc -l <"$work/tree.out")" -le 1 ]; then
		echo "cover: FAIL - $tree has no measured statements" >&2
		status=1
		continue
	fi
	percent=$(go tool cover -func="$work/tree.out" | awk '$1 == "total:" { sub("%", "", $NF); print $NF }')
	if awk -v p="$percent" -v m="$MINIMUM" 'BEGIN { exit !(p + 0 >= m + 0) }'; then
		echo "cover: $tree $percent % (minimum $MINIMUM %)"
	else
		echo "cover: FAIL - $tree $percent % is below the $MINIMUM % minimum" >&2
		status=1
	fi
done

[ "$status" -eq 0 ] && echo "cover: PASS"
exit "$status"

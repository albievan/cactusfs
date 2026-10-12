#!/bin/sh
# S3 compatibility gate. Runs the suite in test/s3compat and compares the tests
# that pass with the recorded baseline.
#
#   scripts/s3compat.sh check    fail if a baseline test no longer passes
#   scripts/s3compat.sh record   add newly passing tests to the baseline
#
# Run through `make s3-compat` and `make s3-compat-record`.
set -eu

mode=${1:-check}
baseline=test/s3compat/baseline.txt

if [ ! -f "$baseline" ]; then
	echo "s3-compat: FAIL - $baseline is missing; the baseline is never deleted" >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Individual test failures are judged against the baseline below. A package
# that does not build or run is always a failure.
suite_status=0
# In the builder image the AWS CLI and ceph/s3-tests are there, and the tests
# that drive the server with them are built in (tag s3tools).
tags=s3compat
if [ "${CACTUSFS_IN_BUILDER:-}" = 1 ]; then
	tags=s3compat,s3tools
fi
# The tests of ceph/s3-tests have a baseline of their own, which the Go test
# that runs them keeps: recording here records there.
if [ "$mode" = record ]; then
	export CACTUSFS_S3COMPAT_RECORD=1
fi
go test -count=1 -timeout=30m -tags="$tags" -json ./test/s3compat/... >"$work/events.json" 2>"$work/stderr" || suite_status=$?
if [ ! -s "$work/events.json" ]; then
	echo "s3-compat: FAIL - go test produced no output:" >&2
	cat "$work/stderr" >&2
	exit 1
fi
if grep -q '"Action":"build-fail"\|"FailedBuild":' "$work/events.json"; then
	echo "s3-compat: FAIL - the suite does not build:" >&2
	grep '"Action":"build-output"' "$work/events.json" | sed 's/.*"Output":"\(.*\)"}$/\1/' >&2 || true
	exit 1
fi

# One line per top-level test: "<package> <TestName>".
extract() {
	grep "\"Action\":\"$1\"" "$work/events.json" |
		sed -n 's/.*"Package":"\([^"]*\)".*"Test":"\([^"/]*\)".*/\1 \2/p' | sort -u
}
extract pass >"$work/passed.txt"
extract fail >"$work/failed.txt"
sed -e '/^[[:space:]]*#/d' -e '/^[[:space:]]*$/d' "$baseline" | sort -u >"$work/baseline.txt"

passed=$(wc -l <"$work/passed.txt" | tr -d ' ')
failed=$(wc -l <"$work/failed.txt" | tr -d ' ')
recorded=$(wc -l <"$work/baseline.txt" | tr -d ' ')
total=$((passed + failed))

if [ "$suite_status" -ne 0 ] && [ "$total" -eq 0 ]; then
	echo "s3-compat: FAIL - the suite did not run:" >&2
	grep '"Action":"output"' "$work/events.json" | sed 's/.*"Output":"\(.*\)"}$/\1/' >&2 || cat "$work/events.json" >&2
	exit 1
fi

case "$mode" in
check)
	comm -23 "$work/baseline.txt" "$work/passed.txt" >"$work/regressions.txt"
	if [ -s "$work/regressions.txt" ]; then
		echo "s3-compat: FAIL - baseline tests that no longer pass:" >&2
		cat "$work/regressions.txt" >&2
		exit 1
	fi
	# The tool tests are part of the contract wherever they are built: a
	# baseline without them would let them fail unnoticed.
	if [ "$tags" != s3compat ]; then
		for required in TestCephS3Tests TestAWSCLICopyUpAndDown; do
			if ! grep -q " $required$" "$work/baseline.txt"; then
				echo "s3-compat: FAIL - $required is not in $baseline; run 'make s3-compat-record'" >&2
				exit 1
			fi
		done
	fi
	# A baseline that records nothing holds the suite to nothing.
	if [ "$recorded" -eq 0 ]; then
		echo "s3-compat: FAIL - $baseline records no test (the suite has $total);" >&2
		echo "           run 'make s3-compat-record' and commit $baseline" >&2
		exit 1
	fi
	new=$(comm -13 "$work/baseline.txt" "$work/passed.txt" | wc -l | tr -d ' ')
	echo "s3-compat: $passed/$total passed (baseline $recorded, $new not yet recorded)"
	echo "s3-compat: PASS"
	;;
record)
	if [ -s "$baseline" ] && [ -n "$(tail -c 1 "$baseline")" ]; then
		echo >>"$baseline"
	fi
	comm -13 "$work/baseline.txt" "$work/passed.txt" >>"$baseline"
	echo "s3-compat: baseline now records $(sed -e '/^[[:space:]]*#/d' -e '/^[[:space:]]*$/d' "$baseline" | wc -l | tr -d ' ') tests"
	;;
*)
	echo "usage: $0 check|record" >&2
	exit 2
	;;
esac

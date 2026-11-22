#!/bin/sh
# Deployment-script gate (PLAN 10.1, 10.9): every shell file of deploy/ is
# formatted as shfmt formats POSIX sh, parses under dash and under bash, and
# the harness of test/deploy runs the real script against recording fakes.
# With the tag deployrender it also renders every Kubernetes profile of
# test/deploy/testdata/k8s through the script with the builder's helm and
# judges each render with kubeconform -strict against the builder's schemas
# (TestProfilesRender): helm, kubeconform and the schemas are in the builder
# image, and a missing one fails the gate.
# Run through `make deploy-check`, which is part of `make check`.
#
# dash is Debian's /bin/sh and is in the builder image (an essential package
# of its Debian base); bash is there too. A shell that is missing fails the
# gate: it is never left out.
set -eu

# The lifecycle that `make test-deploy` runs against a real engine (PLAN
# 10.5) is held to the same form, though only the host runs it.
lifecycle=test/deploy/lifecycle.sh
files=$(find deploy -type f -name '*.sh' | sort)
if [ -z "$files" ] || [ ! -f "$lifecycle" ]; then
	echo "deploy-check: FAIL - no shell files in deploy/, or no $lifecycle" >&2
	exit 1
fi
files="$files
$lifecycle"

go tool shfmt -ln=posix -d deploy/ "$lifecycle" || {
	echo "deploy-check: FAIL - format deploy/ with: go tool shfmt -ln=posix -w deploy/ $lifecycle" >&2
	exit 1
}

for shell in dash bash; do
	command -v "$shell" >/dev/null 2>&1 || {
		echo "deploy-check: FAIL - $shell is not installed (it is in the builder image: run make deploy-check)" >&2
		exit 1
	}
	for f in $files; do
		"$shell" -n "$f" || {
			echo "deploy-check: FAIL - $shell cannot parse $f" >&2
			exit 1
		}
	done
done

go test -count=1 -tags deployscript,deployrender ./test/deploy/...
echo "deploy-check: PASS ($(echo "$files" | wc -l | tr -d ' ') files)"

#!/bin/sh
# Deployment gate: lints the Helm chart and renders it to prove the templates
# produce manifests. Run through `make helm-lint`.
#
# The chart is PLAN step 7.1. Validation of the rendered manifests against the
# Kubernetes schemas (kubeconform) is still to be added to this gate.
set -eu

CHART=deploy/helm/cactusfs

# The chart deploys the release this tree builds: its appVersion is what
# internal/buildinfo/VERSION names.
if [ -f "$CHART/Chart.yaml" ]; then
	release=$(tr -d ' \t\r\n' <internal/buildinfo/VERSION)
	app=$(sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' "$CHART/Chart.yaml")
	if [ -z "$release" ] || [ "$app" != "$release" ]; then
		echo "helm-lint: FAIL - $CHART/Chart.yaml has appVersion '$app', internal/buildinfo/VERSION names '$release'" >&2
		exit 1
	fi
	echo "helm-lint: appVersion $app is the release"
fi

if [ ! -f "$CHART/Chart.yaml" ]; then
	if [ -n "$(find deploy/helm -type f 2>/dev/null | head -n 1)" ]; then
		echo "helm-lint: FAIL - deploy/helm has files but $CHART/Chart.yaml is missing" >&2
		exit 1
	fi
	echo "helm-lint: no chart yet - nothing under deploy/helm ($CHART arrives with PLAN 7.1)"
	echo "helm-lint: PASS"
	exit 0
fi

# The chart renders nothing without the Secrets it is told to use, so it is
# linted and rendered with each of the value sets in ci/: a cluster with
# Vault, five voters, one node, and a development cluster on one machine.
for values in "$CHART"/ci/*.yaml; do
	helm lint --strict "$CHART" -f "$values" >/dev/null
	helm template cactusfs "$CHART" -n cactusfs -f "$values" >/dev/null
	echo "helm-lint: $(basename "$values") renders"
done
# And refuses what must not be deployed - for the reason it should: each
# file of ci/refused is laid over the cluster's values and names, in its first
# line, words of the refusal it must be met with.
for values in "$CHART"/ci/refused/*.yaml; do
	expect=$(sed -n '1s/^# expect: //p' "$values")
	[ -n "$expect" ] || { echo "helm-lint: FAIL - $values does not say what to expect" >&2; exit 1; }
	if out=$(helm template cactusfs "$CHART" -n cactusfs -f "$CHART/ci/cluster-values.yaml" -f "$values" 2>&1); then
		echo "helm-lint: FAIL - rendered with $(basename "$values")" >&2
		exit 1
	fi
	case $out in
	*"$expect"*) echo "helm-lint: $(basename "$values") refused" ;;
	*)
		echo "helm-lint: FAIL - $(basename "$values") was refused, but not for the reason it names:" >&2
		echo "$out" >&2
		exit 1
		;;
	esac
done
echo "helm-lint: PASS"

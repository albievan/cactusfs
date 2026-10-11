#!/bin/sh
# Deployment gate: lints the Helm chart and renders it to prove the templates
# produce manifests. Run through `make helm-lint`.
#
# The chart is PLAN step 7.1. Validation of the rendered manifests against the
# Kubernetes schemas (kubeconform) is still to be added to this gate.
set -eu

CHART=deploy/helm/cactusfs

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
# And refuses what must not be deployed.
refused() {
	if helm template cactusfs "$CHART" "$@" >/dev/null 2>&1; then
		echo "helm-lint: FAIL - rendered with $*" >&2
		exit 1
	fi
}
refused
refused -f "$CHART/ci/cluster-values.yaml" --set 'pools[0].replicas=4'
refused -f "$CHART/ci/cluster-values.yaml" --set 'pools[1].replicas=0'
refused -f "$CHART/ci/cluster-values.yaml" --set kms.type=local,kms.local.keyFileSecret=k,kms.local.passphraseSecret=p
refused -f "$CHART/ci/cluster-values.yaml" --set secrets.tls=
echo "helm-lint: PASS"

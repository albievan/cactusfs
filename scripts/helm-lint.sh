#!/bin/sh
# Deployment gate: lints the Helm chart and renders it to prove the templates
# produce manifests. Run through `make helm-lint`.
#
# The chart is PLAN step 7.1; manifest schema validation is added with it.
# Until then deploy/helm must be empty: any file there without the chart at the
# expected path fails the gate, so a chart cannot arrive unchecked.
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

helm lint --strict "$CHART"
helm template cactusfs "$CHART" >/dev/null
echo "helm-lint: PASS"

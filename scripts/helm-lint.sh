#!/bin/sh
# Deployment gate: lints the Helm chart, renders it with each value set of
# its ci directory, validates every render against the Kubernetes schemas and
# asserts what the render must say. Run through `make helm-lint`.
#
# The schemas are a directory of the builder image (the Dockerfile says which
# Kubernetes version and from where), so kubeconform reads no network: a kind
# without a schema there fails the gate, and none is skipped.
set -eu

CHART=deploy/helm/cactusfs

# There is a chart, and this gate judges it: without one it fails.
if [ ! -f "$CHART/Chart.yaml" ]; then
	echo "helm-lint: FAIL - $CHART/Chart.yaml is missing: there is no chart to judge" >&2
	exit 1
fi

# The chart deploys the release this tree builds: its appVersion is what
# internal/buildinfo/VERSION names.
release=$(tr -d ' \t\r\n' <internal/buildinfo/VERSION)
app=$(sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' "$CHART/Chart.yaml")
if [ -z "$release" ] || [ "$app" != "$release" ]; then
	echo "helm-lint: FAIL - $CHART/Chart.yaml has appVersion '$app', internal/buildinfo/VERSION names '$release'" >&2
	exit 1
fi
echo "helm-lint: appVersion $app is the release"

for tool in helm kubeconform yq; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "helm-lint: FAIL - $tool is not installed (it is in the builder image: run make helm-lint)" >&2
		exit 1
	}
done
# The one version of the schemas the builder image has.
SCHEMAS=${KUBERNETES_SCHEMAS:-/opt/kubernetes-schemas}
kube=
for dir in "$SCHEMAS"/v*-standalone-strict; do
	[ -d "$dir" ] || continue
	if [ -n "$kube" ]; then
		echo "helm-lint: FAIL - $SCHEMAS has the schemas of more than one Kubernetes version" >&2
		exit 1
	fi
	kube=$(basename "$dir" -standalone-strict)
done
if [ -z "$kube" ]; then
	echo "helm-lint: FAIL - no Kubernetes schemas in $SCHEMAS (they are in the builder image: run make helm-lint)" >&2
	exit 1
fi

# How kubeconform finds a schema in that directory (its own template).
LAYOUT='{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json'

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The chart renders nothing without the Secrets it is told to use, so it is
# linted and rendered with each of the value sets in ci/: a cluster with
# Vault, five voters, one node, a development cluster on one machine, and a
# cluster opened to its clients. Every set is judged before the gate fails.
failed=0
sets=0
for values in "$CHART"/ci/*.yaml; do
	[ -f "$values" ] || continue
	sets=$((sets + 1))
	name=$(basename "$values" .yaml)
	helm lint --strict "$CHART" -f "$values" >/dev/null
	helm template cactusfs "$CHART" -n cactusfs --kube-version "$kube" -f "$values" >"$tmp/$name.yaml"
	echo "helm-lint: $name.yaml renders"
	# -strict: no field the schema does not know, no key twice. No schema
	# location but the directory, and no kind skipped or forgiven.
	if kubeconform -strict -summary -kubernetes-version "${kube#v}" -schema-location "$SCHEMAS/$LAYOUT" "$tmp/$name.yaml"; then
		echo "helm-lint: $name.yaml is valid for Kubernetes $kube"
	else
		echo "helm-lint: FAIL - $name.yaml is not valid for Kubernetes $kube" >&2
		failed=1
	fi
	scripts/helm-assert.sh "$tmp/$name.yaml" "$name" || failed=1
done
if [ "$sets" -eq 0 ]; then
	echo "helm-lint: FAIL - no value set in $CHART/ci: nothing was rendered" >&2
	exit 1
fi
if [ "$failed" -ne 0 ]; then
	echo "helm-lint: FAIL" >&2
	exit 1
fi
# And refuses what must not be deployed - for the reason it should: each
# file of ci/refused is laid over the cluster's values and names, in its first
# line, words of the refusal it must be met with.
refused=0
for values in "$CHART"/ci/refused/*.yaml; do
	[ -f "$values" ] || continue
	refused=$((refused + 1))
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
if [ "$refused" -eq 0 ]; then
	echo "helm-lint: FAIL - no value set in $CHART/ci/refused: no refusal was asked for" >&2
	exit 1
fi
echo "helm-lint: PASS"

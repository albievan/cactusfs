#!/bin/sh
# Observability gate (PLAN 7.4, 7.8): the alerting rules, their tests and the
# dashboards of deploy/observability, and the two templates of the Helm chart
# that hand them to the Prometheus Operator. Run through
# `make observability-check`.
#
#   - promtool checks the rules and runs their unit tests;
#   - the chart's copy of the rules is the same file (the chart can read only
#     its own directory, so there are two, and they may not differ);
#   - every dashboard is JSON, refers to nothing outside itself and uses only
#     panels of Grafana itself;
#   - the chart renders no PodMonitor and no PrometheusRule unless told
#     to; told to, it renders what the README promises, and the rules that
#     come out of the PrometheusRule are the rules that went in;
#   - a Go test reads the rules, their tests and the dashboards and fails on
#     a metric that the binary's registry does not have.
#
# The chart's monitoring templates are judged here and not by
# scripts/helm-lint.sh: that gate gives every render to kubeconform with the
# schemas of Kubernetes itself and skips no kind, and PodMonitor and
# PrometheusRule are kinds of the Prometheus Operator. So the value set that
# turns them on is in ci/monitoring/, where helm-lint does not look, and what
# it renders is asserted with yq.
set -eu
LC_ALL=C
export LC_ALL

PACK=deploy/observability
RULES=$PACK/rules/cactusfs.rules.yaml
RULE_TESTS=$PACK/rules/cactusfs.rules.test.yaml
CHART=deploy/helm/cactusfs
CHART_RULES=$CHART/files/cactusfs.rules.yaml
# Panels that are part of Grafana: no plugin to install, nothing to fetch.
CORE_PANELS="row timeseries stat table text"

for tool in promtool helm yq python3 go; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "observability-check: FAIL - $tool is not installed (it is in the builder image: run make observability-check)" >&2
		exit 1
	}
done
for f in "$RULES" "$RULE_TESTS" "$CHART_RULES" "$CHART/Chart.yaml" "$CHART/ci/cluster-values.yaml"; do
	[ -f "$f" ] || {
		echo "observability-check: FAIL - no file $f" >&2
		exit 1
	}
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

checks=0
failures=0

fail() {
	failures=$((failures + 1))
	echo "observability-check: FAIL - $1" >&2
}

# q <file> <yq expression>: what the expression gives, a line for each
# document that gives anything (yq evaluates it for every document of a
# render). An error of yq becomes text that equals no expectation, so a wrong
# expression fails an assertion and hides none.
q() {
	if q_out=$(yq -N "$2" "$1" 2>&1); then
		printf '%s\n' "$q_out"
	else
		printf '<yq error: %s>\n' "$q_out"
	fi
}

# want <what, in words> <file> <yq expression> <value>
want() {
	checks=$((checks + 1))
	# A file of many documents may answer with an empty line for a document the
	# question does not select; they are not part of the answer.
	want_got=$(q "$2" "$3" | sed '/^$/d')
	if [ "$want_got" != "$4" ]; then
		fail "$1: expected '$4', the render has '$want_got'"
	fi
}

# --- the rules ------------------------------------------------------------------
promtool check rules "$RULES" || {
	echo "observability-check: FAIL - $RULES is not a valid file of rules" >&2
	exit 1
}
echo "observability-check: the rules are valid"
promtool test rules "$RULE_TESTS" || {
	echo "observability-check: FAIL - the rules do not pass the tests of $RULE_TESTS" >&2
	exit 1
}
echo "observability-check: the rules pass their tests"

if cmp -s "$RULES" "$CHART_RULES"; then
	echo "observability-check: the chart carries the same rules"
else
	fail "$CHART_RULES is not a copy of $RULES: cp $RULES $CHART_RULES"
fi

# --- the dashboards ---------------------------------------------------------------
found=0
for dashboard in "$PACK"/dashboards/*.json; do
	[ -f "$dashboard" ] || continue
	found=$((found + 1))
	if python3 - "$dashboard" "$CORE_PANELS" <<'EOF'; then
import json
import sys

path, core = sys.argv[1], set(sys.argv[2].split())
with open(path, encoding="utf-8") as f:
    text = f.read()
problems = []
for remote in ("http:", "https:", "//"):
    if remote in text.lower():
        problems.append(f"contains {remote!r}: a dashboard refers to nothing outside itself")
try:
    board = json.loads(text)
except ValueError as err:
    print(f"observability-check: FAIL - {path} is not JSON: {err}", file=sys.stderr)
    sys.exit(1)
for key in ("uid", "title", "schemaVersion"):
    if not board.get(key):
        problems.append(f"has no {key}")
if "__inputs" in board or "__requires" in board:
    problems.append("has __inputs or __requires: the data source is the variable DS_PROMETHEUS")
queries = 0


def walk(panels):
    global queries
    for panel in panels:
        kind, title = panel.get("type"), panel.get("title")
        if kind not in core:
            problems.append(f"panel {title!r} is of type {kind!r}, which is not a panel of Grafana itself")
        walk(panel.get("panels", []))
        if kind in ("row", "text"):
            continue
        targets = panel.get("targets", [])
        if not targets:
            problems.append(f"panel {title!r} queries nothing")
        for target in [panel] + targets:
            if (target.get("datasource") or {}).get("uid") != "${DS_PROMETHEUS}":
                problems.append(f"panel {title!r} does not read the data source ${{DS_PROMETHEUS}}")
        for target in targets:
            queries += 1
            if not str(target.get("expr", "")).strip():
                problems.append(f"panel {title!r} has a query with no expression")


walk(board.get("panels", []))
if not queries:
    problems.append("has no query")
for problem in problems:
    print(f"observability-check: FAIL - {path} {problem}", file=sys.stderr)
sys.exit(1 if problems else 0)
EOF
		echo "observability-check: $(basename "$dashboard") is a dashboard of core panels with no remote asset"
	else
		failures=$((failures + 1))
	fi
done
if [ "$found" -eq 0 ]; then
	fail "no dashboard in $PACK/dashboards"
fi

# --- the chart's templates -------------------------------------------------------
# Off unless asked for: the chart's own values, with those of a cluster.
helm template cactusfs "$CHART" -n cactusfs -f "$CHART/ci/cluster-values.yaml" >"$tmp/default.yaml"
want "by default, resources of the Prometheus Operator" "$tmp/default.yaml" \
	'select(.kind == "PodMonitor" or .kind == "ServiceMonitor" or .kind == "PrometheusRule") | .kind' ""
want "the default values, podMonitor.enabled" "$CHART/values.yaml" '.podMonitor.enabled' false
want "the default values, prometheusRule.enabled" "$CHART/values.yaml" '.prometheusRule.enabled' false
want "the default values, podMonitor.tlsConfig.insecureSkipVerify" "$CHART/values.yaml" '.podMonitor.tlsConfig.insecureSkipVerify' false

sets=0
for values in "$CHART"/ci/monitoring/*.yaml; do
	[ -f "$values" ] || continue
	sets=$((sets + 1))
	name=$(basename "$values" .yaml)
	helm lint --strict "$CHART" -f "$CHART/ci/cluster-values.yaml" -f "$values" >/dev/null
	helm template cactusfs "$CHART" -n cactusfs -f "$CHART/ci/cluster-values.yaml" -f "$values" >"$tmp/$name.yaml"
	echo "observability-check: $name.yaml renders"
	yq -N 'select(.kind == "PodMonitor")' "$tmp/$name.yaml" >"$tmp/$name.monitor.yaml"
	yq -N 'select(.kind == "PrometheusRule")' "$tmp/$name.yaml" >"$tmp/$name.rule.yaml"
	pm=$tmp/$name.monitor.yaml
	pr=$tmp/$name.rule.yaml

	# One of each, named after the release.
	want "[$name] the PodMonitors" "$tmp/$name.yaml" 'select(.kind == "PodMonitor") | .metadata.name' cactusfs
	# The pods are scraped once: nothing selects them through a Service too.
	want "[$name] the ServiceMonitors" "$tmp/$name.yaml" 'select(.kind == "ServiceMonitor") | .metadata.name' ""
	want "[$name] the PrometheusRules" "$tmp/$name.yaml" 'select(.kind == "PrometheusRule") | .metadata.name' cactusfs

	want "[$name] PodMonitor apiVersion" "$pm" '.apiVersion' monitoring.coreos.com/v1
	want "[$name] PodMonitor label of the value set" "$pm" '.metadata.labels.release' \
		"$(q "$values" '.podMonitor.labels.release')"
	want "[$name] PodMonitor label of the chart" "$pm" '.metadata.labels["app.kubernetes.io/name"]' cactusfs
	# The job is "cactusfs" whatever the release is called: the rules say so.
	# A PodMonitor takes it from a label of the pods, which every pod has.
	want "[$name] PodMonitor jobLabel" "$pm" '.spec.jobLabel' app.kubernetes.io/name
	want "[$name] pods without the label the job is named after" "$tmp/$name.yaml" \
		'select(.kind == "StatefulSet") | select(.spec.template.metadata.labels["app.kubernetes.io/name"] != "cactusfs") | .metadata.name' ""
	want "[$name] PodMonitor namespaces" "$pm" '.spec.namespaceSelector.matchNames | join(",")' cactusfs
	want "[$name] PodMonitor selector" "$pm" \
		'.spec.selector.matchLabels | to_entries | map([.key, .value] | join("=")) | sort | join(",")' \
		"app.kubernetes.io/instance=cactusfs,app.kubernetes.io/name=cactusfs"
	# Every node is scraped, whatever its roles: the selector is that of the
	# release and names no role, every pod carries its labels, and every pod
	# has a container port named admin - the port the endpoint names.
	want "[$name] pods the PodMonitor does not select" "$tmp/$name.yaml" \
		'select(.kind == "StatefulSet") | select(.spec.template.metadata.labels["app.kubernetes.io/instance"] != "cactusfs") | .metadata.name' ""
	want "[$name] pods without a port named admin" "$tmp/$name.yaml" \
		'select(.kind == "StatefulSet") | select([.spec.template.spec.containers[0].ports[].name] | contains(["admin"]) | not) | .metadata.name' ""
	# The value set has nodes that are no gateway - the ones no Service
	# with the admin port is in front of. They are pods like the others.
	want "[$name] nodes that are no gateway" "$tmp/$name.yaml" \
		'select(.kind == "StatefulSet") | select(.spec.template.metadata.labels["cactusfs.io/gateway"] != "true") | .metadata.name' \
		"$(printf 'cactusfs-meta-0\ncactusfs-meta-1\ncactusfs-meta-2')"
	want "[$name] nodes behind the admin Service" "$tmp/$name.yaml" \
		'select(.kind == "StatefulSet") | select(.spec.template.metadata.labels["cactusfs.io/gateway"] == "true") | .metadata.name' \
		cactusfs-data-0
	want "[$name] PodMonitor endpoints" "$pm" '.spec.podMetricsEndpoints | length' 1
	want "[$name] PodMonitor port" "$pm" '.spec.podMetricsEndpoints[0].port' admin
	want "[$name] PodMonitor path" "$pm" '.spec.podMetricsEndpoints[0].path' /metrics
	want "[$name] PodMonitor scheme" "$pm" '.spec.podMetricsEndpoints[0].scheme' https
	want "[$name] PodMonitor interval" "$pm" '.spec.podMetricsEndpoints[0].interval' "$(q "$values" '.podMonitor.interval')"
	want "[$name] PodMonitor scrapeTimeout" "$pm" '.spec.podMetricsEndpoints[0].scrapeTimeout' "$(q "$values" '.podMonitor.scrapeTimeout')"
	want "[$name] PodMonitor credential type" "$pm" '.spec.podMetricsEndpoints[0].authorization.type' Bearer
	want "[$name] PodMonitor credential Secret" "$pm" '.spec.podMetricsEndpoints[0].authorization.credentials.name' \
		"$(q "$values" '.podMonitor.bearerTokenSecret.name')"
	want "[$name] PodMonitor credential key" "$pm" '.spec.podMetricsEndpoints[0].authorization.credentials.key' \
		"$(q "$values" '.podMonitor.bearerTokenSecret.key')"
	want "[$name] PodMonitor serverName" "$pm" '.spec.podMetricsEndpoints[0].tlsConfig.serverName' \
		"$(q "$values" '.podMonitor.tlsConfig.serverName')"
	want "[$name] PodMonitor CA Secret" "$pm" '.spec.podMetricsEndpoints[0].tlsConfig.ca.secret.name' \
		"$(q "$values" '.podMonitor.tlsConfig.caSecret.name')"
	want "[$name] PodMonitor CA key" "$pm" '.spec.podMetricsEndpoints[0].tlsConfig.ca.secret.key' \
		"$(q "$values" '.podMonitor.tlsConfig.caSecret.key')"
	# The certificate is checked unless the values say, in so many words, not to.
	want "[$name] PodMonitor insecureSkipVerify" "$pm" '.spec.podMetricsEndpoints[0].tlsConfig | has("insecureSkipVerify")' \
		"$(q "$values" '.podMonitor.tlsConfig.insecureSkipVerify // false')"
	# No credential is written into the resource: it names a Secret.
	want "[$name] PodMonitor keys of the endpoint" "$pm" '.spec.podMetricsEndpoints[0] | keys | sort | join(",")' \
		authorization,interval,path,port,scheme,scrapeTimeout,tlsConfig
	want "[$name] PodMonitor keys of the credential" "$pm" '.spec.podMetricsEndpoints[0].authorization | keys | sort | join(",")' \
		credentials,type
	# The Services are as they were: the admin port is on the admin Service
	# alone, and the nodes' own Service has the two node ports and no other.
	want "[$name] Services with a port named admin" "$tmp/$name.yaml" \
		'select(.kind == "Service") | select([.spec.ports[].name] | contains(["admin"])) | .metadata.name' cactusfs-admin
	want "[$name] ports of the nodes' Service" "$tmp/$name.yaml" \
		'select(.kind == "Service" and .metadata.name == "cactusfs-nodes") | [.spec.ports[].name] | join(",")' rpc,raft

	want "[$name] PrometheusRule apiVersion" "$pr" '.apiVersion' monitoring.coreos.com/v1
	want "[$name] PrometheusRule label of the value set" "$pr" '.metadata.labels.release' \
		"$(q "$values" '.prometheusRule.labels.release')"
	# What comes out of the chart is what went in - group for group, rule for
	# rule, the templates of the annotations untouched - and promtool reads it.
	# Compared as JSON with sorted keys: Helm writes YAML its own way.
	checks=$((checks + 1))
	yq -N -o=json -I=0 '.groups | sort_keys(..)' "$RULES" >"$tmp/$name.groups.source.json" || fail "[$name] yq cannot read $RULES"
	yq -N -o=json -I=0 '.spec.groups | sort_keys(..)' "$pr" >"$tmp/$name.groups.render.json" || fail "[$name] yq cannot read the PrometheusRule"
	if [ ! -s "$tmp/$name.groups.source.json" ] || ! cmp -s "$tmp/$name.groups.source.json" "$tmp/$name.groups.render.json"; then
		fail "[$name] the groups of the PrometheusRule are not those of $RULES"
	fi
	checks=$((checks + 1))
	if ! grep -qF "{{ \$labels.instance }}" "$tmp/$name.groups.render.json"; then
		fail "[$name] the PrometheusRule has lost the templates of its annotations"
	fi
	yq -N '.spec' "$pr" >"$tmp/$name.rendered.rules.yaml"
	promtool check rules "$tmp/$name.rendered.rules.yaml" || fail "[$name] promtool refuses the rules of the PrometheusRule"
	echo "observability-check: $name.yaml carries the rules as they are written"

	# Who may scrape is the NetworkPolicy's to say, with the value it has
	# for the admin port: one rule for port 9001, and that port alone.
	want "[$name] NetworkPolicy rules for port 9001" "$tmp/$name.yaml" \
		'select(.kind == "NetworkPolicy") | .spec.ingress[] | select(.ports[0].port == 9001) | .ports | length' 1
done
if [ "$sets" -eq 0 ]; then
	fail "no value set in $CHART/ci/monitoring turns the monitoring templates on"
fi

if [ "$failures" -ne 0 ]; then
	echo "observability-check: FAIL - $failures of the checks above" >&2
	exit 1
fi
echo "observability-check: $checks assertions on the chart hold"

# --- the registry -----------------------------------------------------------------
# Every metric a rule, a rule test or a dashboard names is one the binary has.
# And the list of metrics in docs/ARCHITECTURE.md is the registry's.
go test -count=1 -run 'TestObservabilityPack|TestChartCarriesTheSameRules|TestRegisteredSeries|TestArchitectureListsTheMetrics' ./internal/adapters/telemetry/

echo "observability-check: PASS"

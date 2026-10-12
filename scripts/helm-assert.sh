#!/bin/sh
# Asserts what a render of the Helm chart must say (PLAN 7.1, 7.2, 7.6).
#
#   scripts/helm-assert.sh <render-file> <value-set-name>
#
# <render-file> is the output of `helm template` with the value set
# deploy/helm/cactusfs/ci/<value-set-name>.yaml and the release name cactusfs
# (HELM_RELEASE says otherwise). What is expected is worked out from the
# values - the chart's own, with the value set laid over them - and not from
# the render, so that a template that leaves something out is caught.
#
# Every assertion is checked; each one that fails is printed with the value
# set, the resource and what was expected; the exit status is 1 if any failed.
# Run by scripts/helm-lint.sh. Needs yq (mikefarah, version 4) and nothing else
# that a POSIX system has not.
set -eu
LC_ALL=C
export LC_ALL

if [ $# -ne 2 ]; then
	echo "usage: $0 <render-file> <value-set-name>" >&2
	exit 2
fi
RENDER=$1
SET=$(basename "$2" .yaml)
CHART=${HELM_CHART:-deploy/helm/cactusfs}
RELEASE=${HELM_RELEASE:-cactusfs}
SETFILE=$CHART/ci/$SET.yaml

command -v yq >/dev/null 2>&1 || { echo "helm-assert: FAIL - yq is not installed (it is in the builder image: run make helm-lint)" >&2; exit 1; }
for f in "$RENDER" "$SETFILE" "$CHART/values.yaml" "$CHART/Chart.yaml"; do
	[ -f "$f" ] || { echo "helm-assert: FAIL - no file $f" >&2; exit 1; }
done

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

checks=0
failures=0

# fail <resource> <what was expected>
fail() {
	failures=$((failures + 1))
	echo "helm-assert: FAIL [$SET] $1: $2" >&2
}

# q <file> <yq expression>: what the expression gives, a line for each
# document that gives anything. An error of yq becomes text that equals no
# expectation, so a wrong expression fails an assertion and hides none.
q() {
	if q_out=$(yq -N "$2" "$1" 2>&1); then
		printf '%s\n' "$q_out"
	else
		printf '<yq error: %s>\n' "$q_out"
	fi
}

# want <resource> <what is expected, in words> <file> <expression> <value>
want() {
	checks=$((checks + 1))
	want_got=$(q "$3" "$4")
	[ "$want_got" = "$5" ] || fail "$1" "$2 - expected '$5' from $4, got '$want_got'"
}

# have <resource> <what is expected, in words> <file> <expression>: any value
have() {
	checks=$((checks + 1))
	have_got=$(q "$3" "$4")
	case $have_got in
	'' | null | '<yq error:'*) fail "$1" "$2 - expected a value from $4, got '$have_got'" ;;
	esac
}

# number <text>: the text if it is a whole number, or 0
number() {
	case $1 in
	'' | *[!0-9]*) echo 0 ;;
	*) echo "$1" ;;
	esac
}

# has_role <roles> <role>: empty roles are all of them
has_role() {
	[ -z "$1" ] && return 0
	case ",$1," in
	*",$2,"*) return 0 ;;
	esac
	return 1
}

# port_of <URL>: its port, or the port of its scheme
port_of() {
	port_host=${1#*://}
	port_host=${port_host%%/*}
	case $port_host in
	*:*) echo "${port_host##*:}" ;;
	*) case $1 in http://*) echo 80 ;; *) echo 443 ;; esac ;;
	esac
}

# document <kind> <name>: that resource of the render, in a file of its own
document() {
	document_file=$TMP/doc-$1-$2.yaml
	KIND=$1 NAME=$2 yq -N 'select(.kind == strenv(KIND) and .metadata.name == strenv(NAME))' "$RENDER" >"$document_file" 2>/dev/null || :
	echo "$document_file"
}

# A map of labels as sorted text: key=value,key=value
LABELS='to_entries | map([.key, .value] | join("=")) | sort | join(",")'
# A list of NetworkPolicy peers as text, whatever the order of its keys
PEERS='sort_keys(..) | @json'

# peers <file> <path of a list of peers>: the list as one line of JSON
peers() {
	q "$1" "$2 | $PEERS" | tr -d ' \n'
}

# --- what the values say ---------------------------------------------------------
V=$TMP/values.yaml
# ($item is yq's variable, not the shell's.)
if ! yq ea '. as $item ireduce ({}; . * $item)' "$CHART/values.yaml" "$SETFILE" >"$V" 2>"$TMP/merge.err"; then
	echo "helm-assert: FAIL [$SET] cannot lay $SETFILE over $CHART/values.yaml: $(cat "$TMP/merge.err")" >&2
	exit 1
fi
CHARTNAME=$(q "$CHART/Chart.yaml" '.name')
SELECTOR="app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/name=$CHARTNAME"
drives=$(number "$(q "$V" '.drives.count')")
drive_size=$(q "$V" '.drives.size')
meta_size=$(q "$V" '.metadata.size')
class=$(q "$V" '.storageClassName // ""')
local_volumes=$(q "$V" '.localVolumes.enabled // false')
spread_key=$(q "$V" '.spread.topologyKey')
spread_when=$(q "$V" '.spread.whenUnsatisfiable')
kms=$(q "$V" '.kms.type')
policy=$(q "$V" '.networkPolicy.enabled // false')

# The nodes the values ask for: id, voter, stores, gateway - a line each.
NODES=$TMP/nodes
: >"$NODES"
q "$V" '.pools[] | [.name, .replicas, (.roles // "")] | join(" ")' >"$TMP/pools"
total=0
voters=0
gateways=0
while read -r pool replicas roles <&3; do
	[ -n "$pool" ] || continue
	roles=$(printf '%s' "$roles" | tr -d ' ')
	replicas=$(number "$replicas")
	voter=no
	stores=no
	gateway=no
	if has_role "$roles" meta; then voter=yes; fi
	if has_role "$roles" storage; then stores=yes; fi
	if has_role "$roles" gateway; then gateway=yes; fi
	i=0
	while [ "$i" -lt "$replicas" ]; do
		echo "$RELEASE-$pool-$i $voter $stores $gateway" >>"$NODES"
		total=$((total + 1))
		if [ "$voter" = yes ]; then voters=$((voters + 1)); fi
		if [ "$gateway" = yes ]; then gateways=$((gateways + 1)); fi
		i=$((i + 1))
	done
done 3<"$TMP/pools"
clustered=no
if [ "$total" -gt 1 ]; then clustered=yes; fi

checks=$((checks + 1))
[ "$total" -ge 1 ] || fail "values" "at least one node in the pools, found $total"
checks=$((checks + 1))
[ "$gateways" -ge 1 ] || fail "values" "at least one node with the gateway role, or the Services select nothing"
if [ "$clustered" = yes ]; then
	checks=$((checks + 1))
	case $voters in
	3 | 5) ;;
	*) fail "values" "3 or 5 voters in a cluster, found $voters" ;;
	esac
fi

# --- the kinds of the render -------------------------------------------------------
# No Ingress, no Secret, no ServiceAccount or role, nothing of a cloud: the
# chart renders these kinds and no other. StorageClass and PersistentVolume
# only with localVolumes.
q "$RENDER" '.kind' | grep -v '^null$' | grep -v '^$' >"$TMP/kinds" || :
sort -u "$TMP/kinds" >"$TMP/kinds.unique"
while IFS= read -r kind <&3; do
	checks=$((checks + 1))
	case $kind in
	Service | StatefulSet) ;;
	PodDisruptionBudget) [ "$clustered" = yes ] || fail "$kind" "no PodDisruptionBudget for a node on its own" ;;
	NetworkPolicy) [ "$policy" = true ] || fail "$kind" "no NetworkPolicy when networkPolicy.enabled is false" ;;
	StorageClass | PersistentVolume) [ "$local_volumes" = true ] || fail "$kind" "no $kind unless localVolumes.enabled" ;;
	*) fail "$kind" "the chart renders Service, StatefulSet, PodDisruptionBudget and NetworkPolicy (and StorageClass, PersistentVolume with localVolumes) - no $kind" ;;
	esac
done 3<"$TMP/kinds.unique"

# count <kind>: how many resources of that kind the render has
count() {
	grep -c "^$1\$" "$TMP/kinds" || :
}

# --- annotations: none unless the values give them ------------------------------------
service_notes=$(q "$V" "(.service.annotations // {}) | $LABELS")
admin_notes=$(q "$V" "(.adminService.annotations // {}) | $LABELS")
pod_notes=$(q "$V" "(.podAnnotations // {}) | $LABELS")
q "$RENDER" 'select(.metadata.annotations != null) | [.kind, .metadata.name] | join("/")' | grep -v '^$' >"$TMP/annotated" || :
while IFS= read -r resource <&3; do
	checks=$((checks + 1))
	case $resource in
	"Service/$RELEASE") [ -n "$service_notes" ] || fail "$resource" "no annotation unless service.annotations names one" ;;
	"Service/$RELEASE-admin") [ -n "$admin_notes" ] || fail "$resource" "no annotation unless adminService.annotations names one" ;;
	*) fail "$resource" "no annotation: the chart puts none on this resource" ;;
	esac
done 3<"$TMP/annotated"

# --- Services ---------------------------------------------------------------------
checks=$((checks + 1))
services=$(q "$RENDER" 'select(.kind == "Service") | .metadata.name' | sort | tr '\n' ' ')
[ "$services" = "$RELEASE $RELEASE-admin $RELEASE-nodes " ] || fail "Service" "the Services $RELEASE, $RELEASE-admin and $RELEASE-nodes and no other, found: $services"
checks=$((checks + 1))
headless=$(q "$RENDER" 'select(.kind == "Service" and .spec.clusterIP == "None") | .metadata.name' | tr '\n' ' ')
[ "$headless" = "$RELEASE-nodes " ] || fail "Service" "one headless Service (clusterIP: None), $RELEASE-nodes, found: $headless"

R="Service/$RELEASE-nodes"
D=$(document Service "$RELEASE-nodes")
want "$R" "headless" "$D" '.spec.clusterIP' "None"
want "$R" "publishNotReadyAddresses: true, so that nodes find each other before they are ready" "$D" '.spec.publishNotReadyAddresses' "true"
want "$R" "selects every pod of the release" "$D" ".spec.selector | $LABELS" "$SELECTOR"
want "$R" "the node ports 9002 and 9003 and no other" "$D" '[.spec.ports[] | [.name, .port, .targetPort] | join("/")] | join(",")' "rpc/9002/rpc,raft/9003/raft"

# check_service <name> <values key> <port name> <annotations of the values>
check_service() {
	R="Service/$1"
	D=$(document Service "$1")
	[ -s "$D" ] || return 0 # its absence is reported above
	service_type=$(q "$V" ".$2.type")
	service_port=$(q "$V" ".$2.port")
	want "$R" "type $service_type, as $2.type says (ClusterIP unless the values say otherwise)" "$D" '.spec.type' "$service_type"
	want "$R" "not headless" "$D" '.spec.clusterIP' "null"
	want "$R" "selects the pods with the gateway role" "$D" ".spec.selector | $LABELS" "$SELECTOR,cactusfs.io/gateway=true"
	want "$R" "the port $3 and no other" "$D" '[.spec.ports[] | [.name, .port, .targetPort] | join("/")] | join(",")' "$3/$service_port/$3"
	want "$R" "the annotations of $2.annotations and no other" "$D" "(.metadata.annotations // {}) | $LABELS" "$4"
	want "$R" "no externalIPs" "$D" '.spec | has("externalIPs")' "false"
	if [ "$service_type" = ClusterIP ]; then
		want "$R" "no nodePort on a ClusterIP Service" "$D" '[.spec.ports[] | select(has("nodePort"))] | length' "0"
	fi
}
check_service "$RELEASE" service s3 "$service_notes"
check_service "$RELEASE-admin" adminService admin "$admin_notes"

# --- PodDisruptionBudget -----------------------------------------------------------
checks=$((checks + 1))
if [ "$clustered" = yes ]; then
	[ "$(count PodDisruptionBudget)" = 1 ] || fail "PodDisruptionBudget" "one PodDisruptionBudget for a cluster, found $(count PodDisruptionBudget)"
	R="PodDisruptionBudget/$RELEASE"
	D=$(document PodDisruptionBudget "$RELEASE")
	[ -s "$D" ] || fail "$R" "the PodDisruptionBudget named after the release"
	want "$R" "maxUnavailable as podDisruptionBudget.maxUnavailable says" "$D" '.spec.maxUnavailable' "$(q "$V" '.podDisruptionBudget.maxUnavailable')"
	want "$R" "no minAvailable beside maxUnavailable" "$D" '.spec | has("minAvailable")' "false"
	want "$R" "selects every pod of the release" "$D" ".spec.selector.matchLabels | $LABELS" "$SELECTOR"
else
	[ "$(count PodDisruptionBudget)" = 0 ] || fail "PodDisruptionBudget" "none for a node on its own, found $(count PodDisruptionBudget)"
fi

# --- StatefulSets: one for every node -------------------------------------------------
checks=$((checks + 1))
q "$RENDER" 'select(.kind == "StatefulSet") | .metadata.name' | grep -v '^$' | sort >"$TMP/sets.actual" || :
cut -d' ' -f1 "$NODES" | sort >"$TMP/sets.expected"
if ! cmp -s "$TMP/sets.expected" "$TMP/sets.actual"; then
	fail "StatefulSet" "one for every node of the pools ($(tr '\n' ' ' <"$TMP/sets.expected")), found: $(tr '\n' ' ' <"$TMP/sets.actual")"
fi

# Where the claims' storage class comes from: the StorageClass the chart makes
# with localVolumes, or storageClassName, or nowhere - the cluster's default.
if [ "$local_volumes" = true ]; then
	checks=$((checks + 1))
	class=$(q "$RENDER" 'select(.kind == "StorageClass") | .metadata.name' | tr '\n' ' ')
	class=${class% }
	case $class in
	'' | *' '*) fail "StorageClass" "one StorageClass with localVolumes, found: '$class'" ;;
	esac
fi

# container <resource> <file> <path of the container> <probes: yes or no>
container() {
	have "$1" "a CPU request" "$2" "$3.resources.requests.cpu"
	have "$1" "a memory request" "$2" "$3.resources.requests.memory"
	have "$1" "a CPU limit" "$2" "$3.resources.limits.cpu"
	have "$1" "a memory limit" "$2" "$3.resources.limits.memory"
	want "$1" "allowPrivilegeEscalation: false" "$2" "$3.securityContext.allowPrivilegeEscalation" "false"
	want "$1" "readOnlyRootFilesystem: true" "$2" "$3.securityContext.readOnlyRootFilesystem" "true"
	want "$1" "capabilities.drop: [ALL]" "$2" "$3.securityContext.capabilities.drop | join(\",\")" "ALL"
	want "$1" "no capability added" "$2" "$3.securityContext.capabilities.add | length" "0"
	want "$1" "not privileged" "$2" "$3.securityContext.privileged // false" "false"
	want "$1" "runAsNonRoot not taken back by the container" "$2" "[$3.securityContext | select(.runAsNonRoot == false)] | length" "0"
	want "$1" "no hostPort" "$2" "[$3.ports[] | select(has(\"hostPort\"))] | length" "0"
	want "$1" "no preStop hook: the drain delay does that work" "$2" "$3.lifecycle.preStop" "null"
	checks=$((checks + 1))
	container_image=$(q "$2" "$3.image")
	case $container_image in
	'' | null | '<yq error:'*) fail "$1" "an image, got '$container_image'" ;;
	*@sha256:*) ;;
	*:latest) fail "$1" "an image that is not :latest, got $container_image" ;;
	*:*)
		case ${container_image##*:} in
		*/*) fail "$1" "an image with a tag or a digest, got $container_image" ;;
		esac
		;;
	*) fail "$1" "an image with a tag or a digest, got $container_image" ;;
	esac
	if [ "$4" = yes ]; then
		want "$1" "a startup probe of /healthz" "$2" "$3.startupProbe.httpGet.path" "/healthz"
		want "$1" "a liveness probe of /healthz" "$2" "$3.livenessProbe.httpGet.path" "/healthz"
		want "$1" "a readiness probe of /readyz" "$2" "$3.readinessProbe.httpGet.path" "/readyz"
		want "$1" "probes over HTTPS on the admin port" "$2" "[$3.startupProbe.httpGet, $3.livenessProbe.httpGet, $3.readinessProbe.httpGet] | map([.scheme, .port] | join(\"/\")) | join(\",\")" "HTTPS/admin,HTTPS/admin,HTTPS/admin"
	fi
}

P='.spec.template.spec'
# The container of the node itself, and one variable of its environment.
C="$P.containers[] | select(.name == \"cactusfs\")"
claims_total=0
voter_pods=0
while read -r id voter stores gateway <&4; do
	R="StatefulSet/$id"
	D=$(document StatefulSet "$id")
	checks=$((checks + 1))
	if [ ! -s "$D" ]; then
		fail "$R" "a StatefulSet for the node $id"
		continue
	fi

	want "$R" "one pod: a node is one pod, for life" "$D" '.spec.replicas' "1"
	want "$R" "serviceName of the headless Service" "$D" '.spec.serviceName' "$RELEASE-nodes"
	want "$R" "updateStrategy OnDelete: an upgrade restarts no node by itself" "$D" '.spec.updateStrategy.type' "OnDelete"
	want "$R" "pods carry the label app.kubernetes.io/name, which the NetworkPolicy, the PodDisruptionBudget and the Services select" "$D" '.spec.template.metadata.labels["app.kubernetes.io/name"]' "$CHARTNAME"
	want "$R" "pods carry the label app.kubernetes.io/instance" "$D" '.spec.template.metadata.labels["app.kubernetes.io/instance"]' "$RELEASE"
	want "$R" "the selector is the pod's labels of the release and its node" "$D" ".spec.selector.matchLabels | $LABELS" "$SELECTOR,cactusfs.io/node=$id"
	want "$R" "pods carry the label cactusfs.io/node" "$D" '.spec.template.metadata.labels["cactusfs.io/node"]' "$id"
	if [ "$voter" = yes ]; then
		voter_pods=$((voter_pods + 1))
		want "$R" "a voter's pods carry cactusfs.io/voter" "$D" '.spec.template.metadata.labels["cactusfs.io/voter"]' "true"
	else
		want "$R" "no cactusfs.io/voter label on a node that does not vote" "$D" '.spec.template.metadata.labels["cactusfs.io/voter"]' "null"
	fi
	if [ "$gateway" = yes ]; then
		want "$R" "a gateway's pods carry cactusfs.io/gateway" "$D" '.spec.template.metadata.labels["cactusfs.io/gateway"]' "true"
	else
		want "$R" "no cactusfs.io/gateway label on a node without the role" "$D" '.spec.template.metadata.labels["cactusfs.io/gateway"]' "null"
	fi
	want "$R" "the pod annotations of podAnnotations and no other" "$D" "(.spec.template.metadata.annotations // {}) | $LABELS" "$pod_notes"

	# The pod.
	want "$R" "automountServiceAccountToken: false" "$D" "$P.automountServiceAccountToken" "false"
	want "$R" "runAsNonRoot: true" "$D" "$P.securityContext.runAsNonRoot" "true"
	want "$R" "seccompProfile.type: RuntimeDefault" "$D" "$P.securityContext.seccompProfile.type" "RuntimeDefault"
	have "$R" "a user to run as" "$D" "$P.securityContext.runAsUser"
	checks=$((checks + 1))
	[ "$(q "$D" "$P.securityContext.runAsUser")" != 0 ] || fail "$R" "a user other than 0"
	want "$R" "no hostNetwork" "$D" "$P.hostNetwork // false" "false"
	want "$R" "no hostPID" "$D" "$P.hostPID // false" "false"
	want "$R" "no hostIPC" "$D" "$P.hostIPC // false" "false"
	want "$R" "no hostPath volume" "$D" "[$P.volumes[] | select(has(\"hostPath\"))] | length" "0"

	# Every container.
	want "$R" "the container cactusfs" "$D" "[$C | .name] | join(\",\")" "cactusfs"
	n=$(number "$(q "$D" "$P.containers | length")")
	i=0
	while [ "$i" -lt "$n" ]; do
		container "$R container $i" "$D" "$P.containers[$i]" yes
		i=$((i + 1))
	done
	n=$(number "$(q "$D" "$P.initContainers | length")")
	i=0
	while [ "$i" -lt "$n" ]; do
		container "$R initContainer $i" "$D" "$P.initContainers[$i]" no
		i=$((i + 1))
	done

	# Claims: one for the metadata, one for each drive of a node that stores,
	# each mounted where the node is told its directories are.
	claims=meta
	paths=
	labels=
	if [ "$stores" = yes ]; then
		i=1
		while [ "$i" -le "$drives" ]; do
			claims="$claims,data$i"
			paths="$paths${paths:+,}/var/lib/cactusfs/data$i"
			labels="$labels${labels:+,}data$i-$id-0"
			i=$((i + 1))
		done
		claims_total=$((claims_total + drives))
	fi
	claims_total=$((claims_total + 1))
	want "$R" "a claim for the metadata and one for each drive" "$D" '[.spec.volumeClaimTemplates[].metadata.name] | join(",")' "$claims"
	want "$R" "the metadata claim of metadata.size" "$D" '[.spec.volumeClaimTemplates[] | select(.metadata.name == "meta") | .spec.resources.requests.storage] | join(",")' "$meta_size"
	want "$R" "the metadata claim mounted at CACTUSFS_META_DIR" "$D" "[$C | .volumeMounts[] | select(.name == \"meta\") | .mountPath] | join(\",\")" "$(q "$D" "$C | .env[] | select(.name == \"CACTUSFS_META_DIR\") | .value")"
	have "$R" "CACTUSFS_META_DIR" "$D" "$C | .env[] | select(.name == \"CACTUSFS_META_DIR\") | .value"
	want "$R" "CACTUSFS_DATA names a directory for each drive (none on a node that does not store)" "$D" "$C | .env[] | select(.name == \"CACTUSFS_DATA\") | .value" "$paths"
	want "$R" "CACTUSFS_DRIVE_LABELS names the claim of each drive, data<N>-<StatefulSet>-0, in the order of CACTUSFS_DATA (none on a node that does not store)" "$D" "$C | .env[] | select(.name == \"CACTUSFS_DRIVE_LABELS\") | .value" "$labels"
	if [ "$stores" = yes ]; then
		want "$R" "every drive's claim of drives.size" "$D" '[.spec.volumeClaimTemplates[] | select(.metadata.name != "meta") | .spec.resources.requests.storage] | unique | join(",")' "$drive_size"
		want "$R" "every drive's claim mounted at its directory of CACTUSFS_DATA" "$D" "[$C | .volumeMounts[] | select(.name | test(\"^data[0-9]+$\")) | .mountPath] | join(\",\")" "$paths"
	fi
	want "$R" "claims of one node at a time (ReadWriteOnce)" "$D" '[.spec.volumeClaimTemplates[].spec.accessModes[]] | unique | join(",")' "ReadWriteOnce"
	if [ -n "$class" ]; then
		want "$R" "every claim of the storage class $class" "$D" '[.spec.volumeClaimTemplates[].spec.storageClassName] | unique | join(",")' "$class"
	else
		want "$R" "no storageClassName unless the values name one" "$D" '[.spec.volumeClaimTemplates[].spec | select(has("storageClassName"))] | length' "0"
	fi

	# Placement: pods apart by the spread key, voters apart from each other.
	# (A second spread constraint on the same key is refused by the API
	# server, so the voters have an anti-affinity instead: ADR-0015.)
	if [ "$clustered" = yes ]; then
		T="$P.topologySpreadConstraints"
		want "$R" "one topology spread constraint" "$D" "$T | length" "1"
		want "$R" "spread with maxSkew 1" "$D" "${T}[0].maxSkew" "1"
		want "$R" "spread by spread.topologyKey" "$D" "${T}[0].topologyKey" "$spread_key"
		want "$R" "spread with spread.whenUnsatisfiable" "$D" "${T}[0].whenUnsatisfiable" "$spread_when"
		want "$R" "spread among every pod of the release" "$D" "${T}[0].labelSelector.matchLabels | $LABELS" "$SELECTOR"
	else
		want "$R" "no spread constraint for a node on its own" "$D" "$P.topologySpreadConstraints" "null"
	fi
	if [ "$clustered" = yes ] && [ "$voter" = yes ]; then
		if [ "$spread_when" = DoNotSchedule ]; then
			A="$P.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution"
			want "$R" "voters kept apart: one required anti-affinity term" "$D" "$A | length" "1"
			want "$R" "voters kept apart by spread.topologyKey" "$D" "${A}[0].topologyKey" "$spread_key"
			want "$R" "voters kept apart from the release's voters" "$D" "${A}[0].labelSelector.matchLabels | $LABELS" "$SELECTOR,cactusfs.io/voter=true"
		else
			A="$P.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution"
			want "$R" "voters kept apart where they can be: one preferred anti-affinity term" "$D" "$A | length" "1"
			want "$R" "voters kept apart with weight 100" "$D" "${A}[0].weight" "100"
			want "$R" "voters kept apart by spread.topologyKey" "$D" "${A}[0].podAffinityTerm.topologyKey" "$spread_key"
			want "$R" "voters kept apart from the release's voters" "$D" "${A}[0].podAffinityTerm.labelSelector.matchLabels | $LABELS" "$SELECTOR,cactusfs.io/voter=true"
		fi
	else
		want "$R" "no affinity on a node that is not one of several voters" "$D" "$P.affinity" "null"
	fi

	# Drain (7.6): the delay outlasts the readiness probe, and the pod is
	# given longer than the delay and the shutdown timeout together.
	period=$(q "$D" "$C | .readinessProbe.periodSeconds")
	threshold=$(q "$D" "$C | .readinessProbe.failureThreshold")
	drain=$(q "$D" "$C | .env[] | select(.name == \"CACTUSFS_DRAIN_DELAY\") | .value")
	shutdown=$(q "$D" "$C | .env[] | select(.name == \"CACTUSFS_SHUTDOWN_TIMEOUT\") | .value")
	grace=$(q "$D" "$P.terminationGracePeriodSeconds")
	checks=$((checks + 1))
	whole=yes
	for value in "$period" "$threshold" "$grace" "${drain%s}" "${shutdown%s}"; do
		case $value in
		'' | *[!0-9]*) whole=no ;;
		esac
	done
	case $drain in *s) ;; *) whole=no ;; esac
	case $shutdown in *s) ;; *) whole=no ;; esac
	if [ "$whole" = no ]; then
		fail "$R" "whole numbers for the readiness probe's periodSeconds and failureThreshold and for terminationGracePeriodSeconds, and whole seconds with their unit (\"20s\") for CACTUSFS_DRAIN_DELAY and CACTUSFS_SHUTDOWN_TIMEOUT - got '$period', '$threshold', '$grace', '$drain', '$shutdown'"
	else
		drain=${drain%s}
		shutdown=${shutdown%s}
		checks=$((checks + 2))
		[ "$drain" -ge $((period * threshold)) ] || fail "$R" "CACTUSFS_DRAIN_DELAY of at least the readiness probe's periodSeconds x failureThreshold ($period x $threshold), got ${drain}s"
		[ "$grace" -gt $((drain + shutdown)) ] || fail "$R" "terminationGracePeriodSeconds above the drain delay and the shutdown timeout together (${drain}s + ${shutdown}s), got $grace"
	fi
done 4<"$NODES"

if [ "$clustered" = yes ]; then
	checks=$((checks + 1))
	case $voter_pods in
	3 | 5) ;;
	*) fail "StatefulSet" "3 or 5 StatefulSets whose pods vote, found $voter_pods" ;;
	esac
fi
if [ "$local_volumes" = true ]; then
	checks=$((checks + 1))
	[ "$(count PersistentVolume)" = "$claims_total" ] || fail "PersistentVolume" "a volume for every claim ($claims_total), found $(count PersistentVolume)"
fi

# --- NetworkPolicy (7.2) -------------------------------------------------------------
# rules <file> <path of the rules> <key of the peers: from or to>: a line for
# each rule - its ports, sorted, then its peers.
rules() {
	rules_n=$(number "$(q "$1" "$2 | length")")
	rules_i=0
	while [ "$rules_i" -lt "$rules_n" ]; do
		echo "$(q "$1" "[${2}[$rules_i].ports[] | [.protocol, .port] | join(\"/\")] | sort | join(\",\")") $(peers "$1" "${2}[$rules_i].$3")"
		rules_i=$((rules_i + 1))
	done
}

# same_rules <resource> <ingress or egress> <file of expected lines> <file of rendered lines>
same_rules() {
	checks=$((checks + 1))
	sort "$3" >"$3.sorted"
	sort "$4" >"$4.sorted"
	comm -23 "$3.sorted" "$4.sorted" >"$TMP/rules.missing"
	comm -13 "$3.sorted" "$4.sorted" >"$TMP/rules.extra"
	while IFS= read -r line <&3; do
		fail "$1" "$2: a rule for the ports and peers [$line], which the values ask for - it is not rendered"
	done 3<"$TMP/rules.missing"
	while IFS= read -r line <&3; do
		fail "$1" "$2: no rule but those the values ask for - rendered a rule for the ports and peers [$line]"
	done 3<"$TMP/rules.extra"
}

checks=$((checks + 1))
if [ "$policy" != true ]; then
	[ "$(count NetworkPolicy)" = 0 ] || fail "NetworkPolicy" "none when networkPolicy.enabled is false, found $(count NetworkPolicy)"
else
	[ "$(count NetworkPolicy)" = 1 ] || fail "NetworkPolicy" "one NetworkPolicy, found $(count NetworkPolicy)"
	R="NetworkPolicy/$RELEASE"
	D=$(document NetworkPolicy "$RELEASE")
	[ -s "$D" ] || fail "$R" "the NetworkPolicy named after the release"
	want "$R" "selects every pod of the release, by the labels every pod carries" "$D" ".spec.podSelector.matchLabels | $LABELS" "$SELECTOR"
	want "$R" "selects by those labels alone" "$D" '.spec.podSelector | has("matchExpressions")' "false"
	want "$R" "both policy types: what no rule allows is denied, in and out" "$D" '.spec.policyTypes | sort | join(",")' "Egress,Ingress"

	# The chart's own pods, as a peer.
	cat >"$TMP/own.yaml" <<EOF
- podSelector:
    matchLabels:
      app.kubernetes.io/name: $CHARTNAME
      app.kubernetes.io/instance: $RELEASE
EOF
	own=$(peers "$TMP/own.yaml" ".")

	# Ingress: the node ports from the chart's pods only; 9000 from the peers
	# of clientsFrom and 9001 from the peers of adminFrom, each apart, and no
	# rule for a port whose list is empty.
	: >"$TMP/ingress.expected"
	if [ "$clustered" = yes ]; then
		echo "TCP/9002,TCP/9003 $own" >>"$TMP/ingress.expected"
	fi
	if [ "$(q "$V" '.networkPolicy.clientsFrom | length')" != 0 ]; then
		echo "TCP/9000 $(peers "$V" ".networkPolicy.clientsFrom")" >>"$TMP/ingress.expected"
	fi
	if [ "$(q "$V" '.networkPolicy.adminFrom | length')" != 0 ]; then
		echo "TCP/9001 $(peers "$V" ".networkPolicy.adminFrom")" >>"$TMP/ingress.expected"
	fi
	rules "$D" '.spec.ingress' from >"$TMP/ingress.actual"
	same_rules "$R" ingress "$TMP/ingress.expected" "$TMP/ingress.actual"

	# Egress: the chart's pods on the node ports, DNS, and what egressTo names.
	: >"$TMP/egress.expected"
	if [ "$clustered" = yes ]; then
		echo "TCP/9002,TCP/9003 $own" >>"$TMP/egress.expected"
	fi
	if [ "$(q "$V" '.networkPolicy.dns | length')" != 0 ]; then
		echo "TCP/53,UDP/53 $(peers "$V" ".networkPolicy.dns")" >>"$TMP/egress.expected"
	fi
	: >"$TMP/egress.named"
	n=$(number "$(q "$V" '.networkPolicy.egressTo | length')")
	i=0
	while [ "$i" -lt "$n" ]; do
		echo "$(q "$V" "[.networkPolicy.egressTo[$i].ports[] | [\"TCP\", .] | join(\"/\")] | sort | join(\",\")") $(peers "$V" ".networkPolicy.egressTo[$i].to")" >>"$TMP/egress.named"
		i=$((i + 1))
	done
	cat "$TMP/egress.named" >>"$TMP/egress.expected"
	rules "$D" '.spec.egress' to >"$TMP/egress.actual"
	same_rules "$R" egress "$TMP/egress.expected" "$TMP/egress.actual"

	# The addresses the nodes are configured to reach have a rule among those
	# that are named: Vault's port, and the trace collector's.
	if [ "$kms" = vault ]; then
		checks=$((checks + 1))
		port=$(port_of "$(q "$V" '.kms.vault.addr')")
		grep -Eq "^([^ ]*,)?TCP/$port(,[^ ]*)? " "$TMP/egress.named" || fail "$R" "egress: a rule of networkPolicy.egressTo for Vault's port $port (kms.vault.addr)"
	fi
	otlp=$(q "$V" '.otlpEndpoint // ""')
	if [ -n "$otlp" ]; then
		checks=$((checks + 1))
		port=$(port_of "$otlp")
		grep -Eq "^([^ ]*,)?TCP/$port(,[^ ]*)? " "$TMP/egress.named" || fail "$R" "egress: a rule of networkPolicy.egressTo for the trace collector's port $port (otlpEndpoint)"
	fi
fi

if [ "$failures" -ne 0 ]; then
	echo "helm-assert: FAIL [$SET] $failures of $checks assertions do not hold" >&2
	exit 1
fi
echo "helm-assert: $SET - $checks assertions hold"

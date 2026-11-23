# shellcheck shell=sh
# Kubernetes (k3s, k8s, Rancher Desktop's Kubernetes): the values the chart
# is rendered with, the render, the deployment's namespace and its Secrets
# (PLAN 10.9, ADR-0033 sections 6 to 8); installing it and delivering its
# image (PLAN 10.10); upgrading it one node at a time, saying how it stands,
# uninstalling and purging it (PLAN 10.11; ADR-0033 sections 3, 7, 8, 11).
# Sourced by deploy/deploy.sh; not run on its own.
#
# What the script writes on a cluster is namespaced, in its own namespace:
# the namespace itself is made by a create (never an apply) whose object
# already carries the marker - a random UID, kept in the deployment's
# STATE_DIR entry as well - and the restricted Pod Security label. Outside
# the namespace it only reads: the Nodes, the StorageClasses, the Services
# of every namespace and the PersistentVolumes (listed), and kube-system
# (its UID, the cluster's identity). It deletes no cluster-scoped object.
#
# The secrets go to the cluster as Secrets of the namespace, on the standard
# input of kubectl, streamed from their files: no secret is held in a shell
# variable, put on a command line or printed (ADR-0033 section 3). The values
# hold none: they name the Secrets.
#
# Every kubectl and helm runs through k8s_cmd and run/query (common.sh):
# under "env -i", with the context of the settings, or as "k3s kubectl" on
# K3S_SSH over ssh.

# The files of a deployment's STATE_DIR entry this file reads (ADR-0033
# section 3; PLAN 10.3 writes them): the generated secrets, the directory
# "cactusfs dev-init" wrote, in its own layout, and the namespace marker
# this file writes.
#   <entry>/secrets/{root-access-key,root-secret-key,kms-passphrase,vault-token}
#   <entry>/config/{kms.json,s3.pem,s3.key,s3-ca.pem,node-ca.pem,nodes/<id>.{pem,key},ready}
#   <entry>/namespace-uid

# k8s_ns: the deployment's namespace - K8S_NAMESPACE, else DEPLOY_NAME.
k8s_ns() {
	_k_ns=$(setting K8S_NAMESPACE)
	printf '%s' "${_k_ns:-$(setting DEPLOY_NAME)}"
}

# k8s_connect: before the first kubectl on K3S_SSH, the host key of that
# server is checked (host_key_check). Called outside a command substitution,
# so that the check is made once a run.
k8s_connect() {
	if [ "$TARGET" = k3s ] && [ -n "$(setting K3S_SSH)" ]; then
		host_key_check "$(setting K3S_SSH)"
	fi
}

# k8s_cmd <run|query> <argument>...: kubectl as the target has it, the same
# one engine_identity names - "k3s kubectl" on K3S_SSH over ssh, KUBECTL
# with the context of the settings, or "k3s kubectl" here. Never the current
# context of a kubeconfig.
k8s_cmd() {
	_k_how=$1
	shift
	if [ "$TARGET" = k3s ] && [ -n "$(setting K3S_SSH)" ]; then
		"$_k_how" on_host "$(setting K3S_SSH)" k3s kubectl "$@"
	elif [ -n "$(kube_context)" ]; then
		"$_k_how" "$(setting KUBECTL)" --context "$(kube_context)" "$@"
	else
		"$_k_how" k3s kubectl "$@"
	fi
}

# k8s_published <type>: true for a Service type that is reached from outside
# the cluster.
k8s_published() {
	case $1 in
	NodePort | LoadBalancer) return 0 ;;
	esac
	return 1
}

# k8s_ranges_valid <key>: empty, "any", or addresses and ranges separated by
# commas - an IPv4 address with an optional /0 to /32, or an IPv6 address
# (hexadecimal digits and colons) with an optional /0 to /128.
k8s_ranges_valid() {
	_k_list=$(setting "$1")
	case $_k_list in
	'' | any) return 0 ;;
	,* | *, | *,,*) bad "$1" "has an empty entry: write ranges separated by single commas, or 'any'" ;;
	esac
	_k_bad=$(printf '%s\n' "$_k_list" | tr ',' '\n' | awk '
		function octets(a,   n, i, p) {
			n = split(a, p, ".")
			if (n != 4) return 0
			for (i = 1; i <= 4; i++) if (p[i] !~ /^[0-9]+$/ || length(p[i]) > 3 || p[i] + 0 > 255 || p[i] ~ /^0[0-9]/) return 0
			return 1
		}
		# groups <text>: how many groups of 1 to 4 hexadecimal digits the
		# text separated by single colons holds, or -1 when it is not that.
		function groups(s,   n, i, g) {
			if (s == "") return 0
			n = split(s, g, ":")
			for (i = 1; i <= n; i++) if (g[i] !~ /^[0-9a-fA-F][0-9a-fA-F]?[0-9a-fA-F]?[0-9a-fA-F]?$/) return -1
			return n
		}
		# hextets: eight groups, or fewer around one "::" (a second "::"
		# leaves an empty group, which groups refuses).
		function hextets(a,   i, l, r) {
			i = index(a, "::")
			if (i == 0) return groups(a) == 8
			l = groups(substr(a, 1, i - 1))
			r = groups(substr(a, i + 2))
			return l >= 0 && r >= 0 && l + r <= 7
		}
		{
			addr = $0; len = ""
			if (index(addr, "/")) { len = substr(addr, index(addr, "/") + 1); addr = substr(addr, 1, index(addr, "/") - 1) }
			if (addr ~ /:/) { ok = hextets(addr); max = 128 }
			else { ok = octets(addr); max = 32 }
			if (ok && len != "") ok = len ~ /^[0-9]+$/ && length(len) <= 3 && len + 0 <= max
			if (!ok) { print $0; exit }
			if (len != "" && len + 0 == 0) { print "every:" $0; exit }
		}')
	case $_k_bad in
	every:*) bad "$1" "'${_k_bad#every:}' is every address: write 'any', which says so" ;;
	esac
	[ -z "$_k_bad" ] || bad "$1" "'$_k_bad' is not an address or an address range (192.0.2.0/24, 2001:db8::/32): write ranges separated by commas, or 'any'"
}

# k8s_vault_endpoint: "<IPv4 address> <port>" of VAULT_ADDR, for the
# NetworkPolicy, which lets the pods reach an address, never a name. Fails
# when VAULT_ADDR names a host.
k8s_vault_endpoint() {
	_k_hp=${1#https://}
	_k_hp=${_k_hp%%/*}
	case $_k_hp in
	*:*) _k_host=${_k_hp%:*} _k_port=${_k_hp##*:} ;;
	*) _k_host=$_k_hp _k_port=443 ;;
	esac
	case $_k_host in
	*[!0123456789.]* | '' | .* | *. | *..*) return 1 ;;
	esac
	case $_k_port in
	'' | *[!0123456789]*) return 1 ;;
	esac
	printf '%s %s' "$_k_host" "$_k_port"
}

# k8s_digest_ok <reference>: true for an image reference with a digest,
# ...@sha256:<64 hexadecimal digits>.
k8s_digest_ok() {
	case $1 in
	-* | *[!A-Za-z0-9_./:@-]*) return 1 ;;
	*@sha256:????????????????????????????????????????????????????????????????) ;;
	*) return 1 ;;
	esac
	case ${1##*@sha256:} in
	*[!0123456789abcdef]*) return 1 ;;
	esac
	return 0
}

# k8s_helm_image: the image helm renders in - HELM_IMAGE, else the one the
# Dockerfile pins for make helm-lint (ARG HELM_IMAGE), by its digest.
k8s_helm_image() {
	_k_image=$(setting HELM_IMAGE)
	if [ -z "$_k_image" ]; then
		[ -f "$REPO_ROOT/Dockerfile" ] || fail "no Dockerfile in $REPO_ROOT, which pins the helm image; set HELM_IMAGE (with its digest) or HELM"
		_k_image=$(sed -n 's/^ARG HELM_IMAGE=//p' "$REPO_ROOT/Dockerfile" | head -n 1)
	fi
	k8s_digest_ok "$_k_image" || fail "the helm image '$_k_image' is not named by its digest (...@sha256:<64 hex>)"
	printf '%s' "$_k_image"
}

# k8s_validate: the settings of a Kubernetes target, before anything is
# asked of the cluster. Reaching it (ADR-0033 section 8): a Service that is
# published outside the cluster needs the ranges that may reach it; the
# admin Service stays a ClusterIP, closed, unless both its type and its
# ranges are said.
k8s_validate() {
	_k_s3=$(setting K8S_SERVICE_TYPE)
	if k8s_published "$_k_s3"; then
		[ -n "$(setting K8S_S3_ALLOW_FROM)" ] ||
			refuse "K8S_SERVICE_TYPE=$_k_s3 publishes S3 outside the cluster: K8S_S3_ALLOW_FROM must say who may reach it - address ranges separated by commas, such as 192.0.2.0/24, or 'any' (ADR-0033 section 8)"
	else
		[ -z "$(setting K8S_NODE_PORT)" ] || refuse "K8S_NODE_PORT goes with K8S_SERVICE_TYPE=NodePort or LoadBalancer, not $_k_s3"
	fi
	_k_admin=$(setting K8S_ADMIN_SERVICE_TYPE)
	if k8s_published "$_k_admin"; then
		[ -n "$(setting K8S_ADMIN_ALLOW_FROM)" ] ||
			refuse "K8S_ADMIN_SERVICE_TYPE=$_k_admin publishes the admin API and the console, whose first user is admin/admin (ADR-0014): K8S_ADMIN_ALLOW_FROM must say who may reach them, or leave the type ClusterIP and use the port-forward --status prints"
	else
		[ -z "$(setting K8S_ADMIN_NODE_PORT)" ] || refuse "K8S_ADMIN_NODE_PORT goes with K8S_ADMIN_SERVICE_TYPE=NodePort or LoadBalancer, not $_k_admin"
	fi
	if [ -n "$(setting K8S_NODE_PORT)" ] && [ "$(setting K8S_NODE_PORT)" = "$(setting K8S_ADMIN_NODE_PORT)" ]; then
		refuse "K8S_NODE_PORT and K8S_ADMIN_NODE_PORT are the same port"
	fi
	k8s_ranges_valid K8S_S3_ALLOW_FROM
	k8s_ranges_valid K8S_ADMIN_ALLOW_FROM
	if [ -n "$(setting HELM)" ] && [ -n "$(setting HELM_IMAGE)" ]; then
		refuse "HELM and HELM_IMAGE are both set: say one - a helm program here, or the image helm renders in"
	fi
	if [ -n "$(setting HELM_IMAGE)" ] && ! k8s_digest_ok "$(setting HELM_IMAGE)"; then
		bad HELM_IMAGE "must name the image by its digest (...@sha256:<64 hex>): a mirrored or preloaded copy of the one the Dockerfile pins"
	fi
	if [ "$(setting KMS)" = vault ] && ! k8s_vault_endpoint "$(setting VAULT_ADDR)" >/dev/null; then
		bad VAULT_ADDR "must name Vault by its IPv4 address on Kubernetes: the NetworkPolicy lets the pods reach addresses, not names"
	fi
}

# k8s_q <text>: the text as a YAML scalar in single quotes, in which only a
# single quote means anything (written twice).
k8s_q() {
	printf "'%s'" "$(printf '%s' "$1" | sed "s/'/''/g")"
}

# k8s_peers <key>: the NetworkPolicy peers of a list of ranges, a YAML list
# of ipBlocks at the indentation of a value under networkPolicy. "any" is
# every address, written as halves (a policy controller that keeps blocks in
# kernel sets cannot hold one of length 0: docs/load-balancing.md); a lone
# address is a block of one.
k8s_peers() {
	_k_list=$(setting "$1")
	case $_k_list in
	'') printf ' []\n' ;;
	any)
		printf '\n'
		for _k_cidr in 0.0.0.0/1 128.0.0.0/1 ::/1 8000::/1; do
			printf '    - ipBlock: {cidr: %s}\n' "$(k8s_q "$_k_cidr")"
		done
		;;
	*)
		printf '\n'
		printf '%s\n' "$_k_list" | tr ',' '\n' | while IFS= read -r _k_cidr; do
			case $_k_cidr in
			*/*) ;;
			*:*) _k_cidr=$_k_cidr/128 ;;
			*) _k_cidr=$_k_cidr/32 ;;
			esac
			printf '    - ipBlock: {cidr: %s}\n' "$(k8s_q "$_k_cidr")"
		done
		;;
	esac
}

# k8s_node_ids: the node IDs of the deployment, one a line - the chart names
# a node <release>-<pool>-<index>.
k8s_node_ids() {
	_k_name=$(setting DEPLOY_NAME)
	if [ "$(setting DEPLOY_MODE)" = cluster ]; then
		printf '%s\n' "$_k_name-meta-0" "$_k_name-meta-1" "$_k_name-meta-2" "$_k_name-data-0"
	else
		printf '%s\n' "$_k_name-node-0"
	fi
}

# k8s_values <file> <nodes> <first node> <image> [pull policy]: writes the
# chart's values for this deployment into the file, from the settings, the
# number of the cluster's nodes and the name of the first (for
# K8S_STORAGE=hostpath), the image the pods run (a reference with a tag or
# a digest; PLAN 10.4 delivers it) and how the kubelet gets it -
# IfNotPresent, or Never for an image imported into k3s or on the Docker
# engine of a desktop's Kubernetes (PLAN 10.4's routes import and local).
# The values name Secrets and hold none.
#
#   standalone  one pool, one node with every role, DRIVES drives
#   cluster     the chart's pools: 3 voters with every role and one more
#               node that stores and serves, 2 drives each (compose's shape)
#
# A Service published outside the cluster gets externalTrafficPolicy Local,
# so that the pods see the client's address and the ranges of the
# NetworkPolicy mean clients, not nodes. The console never upgrades the
# nodes: the script does.
k8s_values() {
	_k_file=$1
	_k_nodes=$2
	_k_first=$3
	_k_ref=$4
	_k_pull=${5:-IfNotPresent}
	case $_k_pull in
	IfNotPresent | Never) ;;
	*) fail "internal: k8s_values with the pull policy '$_k_pull'" ;;
	esac
	_k_name=$(setting DEPLOY_NAME)
	_k_dmode=$(setting DEPLOY_MODE)
	_k_namespace=$(k8s_ns)
	case $_k_nodes in
	'' | 0 | *[!0123456789]*) fail "internal: k8s_values with $_k_nodes nodes" ;;
	esac
	if [ "$_k_dmode" = cluster ] && [ "$_k_nodes" -eq 2 ]; then
		refuse "DEPLOY_MODE=cluster on a cluster of 2 nodes: the 3 voters are kept on 3 machines - deploy on 1 node (they then share it) or on 3 or more"
	fi
	case $_k_ref in
	'' | -* | *[!A-Za-z0-9_./:@-]*) fail "internal: '$_k_ref' is not an image reference" ;;
	esac
	case $_k_ref in
	*@sha256:*) _k_repo=${_k_ref%@*} _k_tag='' _k_digest=${_k_ref#*@} ;;
	*)
		case ${_k_ref##*/} in
		*:*) _k_repo=${_k_ref%:*} _k_tag=${_k_ref##*:} ;;
		*) _k_repo=$_k_ref _k_tag='' ;;
		esac
		_k_digest=''
		;;
	esac
	if [ "$(setting K8S_STORAGE)" = hostpath ]; then
		[ "$_k_nodes" -eq 1 ] || refuse "K8S_STORAGE=hostpath puts every volume on one machine and is for a cluster of one node; this one has $_k_nodes - use K8S_STORAGE=pvc"
		case $_k_first in
		'' | *[!abcdefghijklmnopqrstuvwxyz0123456789.-]*) fail "the cluster's node is named '$_k_first', which is not a node name" ;;
		esac
	fi
	{
		printf '# The values of the deployment %s, written by deploy/deploy.sh from\n' "$_k_name"
		printf '# its settings (PLAN 10.9). They name Secrets and hold none.\n'
		printf 'image:\n  repository: %s\n  tag: %s\n  digest: %s\n  pullPolicy: %s\n' "$(k8s_q "$_k_repo")" "$(k8s_q "$_k_tag")" "$(k8s_q "$_k_digest")" "$_k_pull"
		if [ "$_k_dmode" = cluster ]; then
			printf 'pools:\n'
			printf '  - {name: meta, replicas: 3, roles: %s}\n' "''"
			printf '  - {name: data, replicas: 1, roles: %s}\n' "$(k8s_q gateway,storage,console)"
			printf 'drives: {count: 2, size: %s}\n' "$(k8s_q "$(setting K8S_DRIVE_SIZE)")"
		else
			printf 'pools:\n'
			printf '  - {name: node, replicas: 1, roles: %s}\n' "''"
			printf 'drives: {count: %s, size: %s}\n' "$(setting DRIVES)" "$(k8s_q "$(setting K8S_DRIVE_SIZE)")"
		fi
		printf 'metadata: {size: %s}\n' "$(k8s_q "$(setting K8S_META_SIZE)")"
		if [ "$(setting K8S_STORAGE)" = hostpath ]; then
			printf 'storageClassName: %s\n' "''"
			printf 'localVolumes: {enabled: true, basePath: %s, nodeName: %s}\n' \
				"$(k8s_q "$(setting K8S_HOSTPATH)/$_k_namespace")" "$(k8s_q "$_k_first")"
		else
			printf 'storageClassName: %s\n' "$(k8s_q "$(setting K8S_STORAGE_CLASS)")"
		fi
		if [ "$_k_dmode" = cluster ]; then
			printf 'secrets: {root: %s, tls: %s, nodeCA: %s}\n' "$(k8s_q "$_k_name-root")" "$(k8s_q "$_k_name-tls")" "$(k8s_q "$_k_name-node-ca")"
		else
			printf 'secrets: {root: %s, tls: %s}\n' "$(k8s_q "$_k_name-root")" "$(k8s_q "$_k_name-tls")"
		fi
		printf 'kms:\n  type: %s\n' "$(setting KMS)"
		if [ "$(setting KMS)" = vault ]; then
			printf '  vault:\n'
			printf '    addr: %s\n' "$(k8s_q "$(setting VAULT_ADDR)")"
			printf '    transitMount: %s\n' "$(k8s_q "$(setting VAULT_TRANSIT_MOUNT)")"
			printf '    key: %s\n' "$(k8s_q "$(setting VAULT_KEY)")"
			printf '    namespace: %s\n' "$(k8s_q "$(setting VAULT_NAMESPACE)")"
			printf '    tokenSecret: %s\n' "$(k8s_q "$_k_name-vault-token")"
			if [ -n "$(setting VAULT_CA_FILE)" ]; then
				printf '    caSecret: %s\n' "$(k8s_q "$_k_name-vault-ca")"
			fi
		else
			printf '  local: {keyFileSecret: %s, passphraseSecret: %s}\n' "$(k8s_q "$_k_name-kms")" "$(k8s_q "$_k_name-kms-passphrase")"
			if [ "$_k_dmode" = cluster ]; then
				# env_validate has refused this without ALLOW_SHARED_LOCAL_KMS=yes.
				printf 'dev: {localKMS: true}\n'
			fi
		fi
		k8s_values_service service K8S_SERVICE_TYPE K8S_NODE_PORT
		k8s_values_service adminService K8S_ADMIN_SERVICE_TYPE K8S_ADMIN_NODE_PORT
		if [ "$_k_nodes" -eq 1 ]; then
			printf 'spread: {whenUnsatisfiable: ScheduleAnyway}\n'
		else
			printf 'spread: {whenUnsatisfiable: DoNotSchedule}\n'
		fi
		k8s_values_settings
		printf 'networkPolicy:\n'
		printf '  clientsFrom:'
		k8s_peers K8S_S3_ALLOW_FROM
		printf '  adminFrom:'
		k8s_peers K8S_ADMIN_ALLOW_FROM
		if [ "$(setting KMS)" = vault ]; then
			_k_vault=$(k8s_vault_endpoint "$(setting VAULT_ADDR)") || fail "internal: VAULT_ADDR names no address"
			printf '  egressTo:\n'
			printf '    - to: [{ipBlock: {cidr: %s}}]\n' "$(k8s_q "${_k_vault% *}/32")"
			printf '      ports: [%s]\n' "${_k_vault#* }"
		else
			printf '  egressTo: []\n'
		fi
		printf 'upgrade:\n  fromConsole: {enabled: false}\n'
	} >"$_k_file"
	if [ "$(setting K8S_STORAGE)" = hostpath ] && [ -z "${_k8_values_said:-}" ]; then
		_k_volumes=$(k8s_node_ids | wc -l | tr -d ' ')
		if [ "$_k_dmode" = cluster ]; then
			_k_volumes=$((_k_volumes * 3))
		else
			_k_volumes=$((_k_volumes * ($(setting DRIVES) + 1)))
		fi
		say "K8S_STORAGE=hostpath makes cluster-scoped objects: the StorageClass $_k_namespace-$_k_name-local and $_k_volumes PersistentVolumes ($_k_namespace-<node ID>-<volume>), directories under $(setting K8S_HOSTPATH)/$_k_namespace on the node $_k_first - not for a shared cluster; a purge deletes none of them (ADR-0033 section 7)"
	fi
}

# k8s_values_service <values key> <type key> <node port key>: a Service's
# values. externalTrafficPolicy Local for one that is published.
k8s_values_service() {
	_k_type=$(setting "$2")
	_k_port=$(setting "$3")
	printf '%s:\n  type: %s\n  nodePort: %s\n' "$1" "$_k_type" "${_k_port:-null}"
	if k8s_published "$_k_type"; then
		printf '  externalTrafficPolicy: Local\n'
	fi
}

# k8s_values_settings: the CACTUSFS_* lines of the env file. The four the
# chart sets from values of its own are given as those values (an extraEnv
# of the same name would be a second entry of one name, which a server-side
# apply refuses); the rest as extraEnv.
k8s_values_settings() {
	_k_extra=
	while IFS= read -r _k_line; do
		[ -n "$_k_line" ] || continue
		_k_key=${_k_line%%=*}
		_k_value=${_k_line#*=}
		case $_k_key in
		CACTUSFS_LOG_LEVEL) printf 'logLevel: %s\n' "$(k8s_q "$_k_value")" ;;
		CACTUSFS_OTLP_ENDPOINT) printf 'otlpEndpoint: %s\n' "$(k8s_q "$_k_value")" ;;
		*) _k_extra="$_k_extra$_k_line$NL" ;;
		esac
	done <<EOF
$PASSED
EOF
	_k_region=$(lookup "$PASSED" CACTUSFS_REGION) || _k_region=
	_k_domain=$(lookup "$PASSED" CACTUSFS_S3_DOMAIN) || _k_domain=
	if [ -n "$_k_region$_k_domain" ]; then
		printf 's3:\n'
		[ -z "$_k_region" ] || printf '  region: %s\n' "$(k8s_q "$_k_region")"
		[ -z "$_k_domain" ] || printf '  domain: %s\n' "$(k8s_q "$_k_domain")"
	fi
	printf 'extraEnv:'
	_k_any=0
	while IFS= read -r _k_line; do
		case ${_k_line%%=*} in
		'' | CACTUSFS_REGION | CACTUSFS_S3_DOMAIN) continue ;;
		esac
		[ "$_k_any" = 1 ] || printf '\n'
		_k_any=1
		printf '  - {name: %s, value: %s}\n' "${_k_line%%=*}" "$(k8s_q "${_k_line#*=}")"
	done <<EOF
$_k_extra
EOF
	[ "$_k_any" = 1 ] || printf ' []\n'
}

# k8s_nodes: "<count> <name of the first>" of the cluster's Nodes, listed
# (read-only, outside the namespace: ADR-0033 section 7). Called as
# "_n=$(k8s_nodes) || exit", so that a failure is not lost.
k8s_nodes() {
	_k_list=$(k8s_cmd query get nodes -o name </dev/null) || fail "listing the cluster's nodes failed"
	_k_list=$(printf '%s\n' "$_k_list" | printable | sed -n 's|^node/||p')
	_k_count=$(printf '%s\n' "$_k_list" | grep -c . || :)
	[ "$_k_count" -gt 0 ] || fail "the cluster lists no node"
	printf '%s %s' "$_k_count" "$(printf '%s\n' "$_k_list" | head -n 1)"
}

# k8s_storage_check: with K8S_STORAGE=pvc, the StorageClass the claims ask
# for exists - K8S_STORAGE_CLASS, or the cluster's default (the annotation,
# or its beta form older clusters still set). Read-only: the StorageClasses
# are listed.
k8s_storage_check() {
	[ "$(setting K8S_STORAGE)" = pvc ] || return 0
	_k_class=$(setting K8S_STORAGE_CLASS)
	if [ -n "$_k_class" ]; then
		_k_list=$(k8s_cmd query get storageclasses -o name </dev/null) || fail "listing the cluster's StorageClasses failed"
		printf '%s\n' "$_k_list" | printable | grep -qx "storageclass.storage.k8s.io/$_k_class" ||
			refuse "K8S_STORAGE_CLASS=$_k_class: the cluster has no StorageClass of that name"
	else
		_k_list=$(k8s_cmd query get storageclasses -o json </dev/null) || fail "listing the cluster's StorageClasses failed"
		printf '%s\n' "$_k_list" | grep -Eq '"storageclass\.(beta\.)?kubernetes\.io/is-default-class": *"true"' ||
			refuse "the cluster has no default StorageClass: name one with K8S_STORAGE_CLASS, or say K8S_STORAGE=hostpath on a cluster of one node of your own"
	fi
}

# k8s_engine <run|query> <argument>...: the container engine here, for the
# helm image - the Docker CLI with the context of the settings, else Podman.
k8s_engine() {
	_k_m=$1
	shift
	if command -v docker >/dev/null 2>&1; then
		"$_k_m" docker --context "$(docker_context)" "$@"
	elif command -v podman >/dev/null 2>&1; then
		"$_k_m" podman "$@"
	else
		refuse "rendering the chart needs docker or podman here, to run the helm image - or HELM, a helm program"
	fi
}

# k8s_workdir <run>: a work directory of mode 0700 - in the deployment's
# STATE_DIR entry when "entry" is said and this is no dry run (what the
# Secrets are compared in), else the system's. Its path on standard output.
k8s_workdir() {
	if [ "$1" = entry ] && [ "$DRY_RUN" = 0 ]; then
		(umask 077 && mkdir -p "$STATE") || fail "$STATE cannot be made"
		_k_d=$(umask 077 && mktemp -d "$STATE/.k8s.XXXXXX") || fail "no work directory in $STATE"
	else
		_k_d=$(umask 077 && mktemp -d) || fail "no temporary directory"
	fi
	chmod 700 "$_k_d" || fail "chmod of $_k_d failed"
	# A path with a single quote is refused, as it was when the trap held
	# it between single quotes (the cleanup quotes it now: show_command).
	case $_k_d in
	*"'"*)
		rm -rf "$_k_d"
		fail "the work directory $_k_d holds a single quote; set STATE_DIR or TMPDIR to a path without one"
		;;
	esac
	printf '%s' "$_k_d"
}

# k8s_in_workdir <entry|tmp> <function> <argument>...: the function, in a
# subshell, with a work directory as its first argument that is removed
# however the run ends - by the run's one EXIT trap, to which its path is
# added, quoted, when it is made (cleanup_add, common.sh), and by this
# function when the subshell ends. The subshell starts with no cleanup of
# its own (cleanup_none): one it adds is its own. Its status is the run's: a
# refusal or failure in it ends the script with that status. "set -e" does
# not hold in such a subshell (it runs on the left of "||"): every step of
# the function checks its own status.
k8s_in_workdir() {
	_k_iw_where=$1
	shift
	_k_iw_dir=$(k8s_workdir "$_k_iw_where") || exit
	cleanup_add rm -rf -- "$_k_iw_dir"
	_k_iw_status=0
	(
		cleanup_none
		trap 'exit 129' HUP
		trap 'exit 130' INT
		trap 'exit 143' TERM
		"$@" "$_k_iw_dir"
	) || _k_iw_status=$?
	rm -rf -- "$_k_iw_dir"
	cleanup_drop rm -rf -- "$_k_iw_dir"
	[ "$_k_iw_status" = 0 ] || exit "$_k_iw_status"
}

# k8s_image_present <image>: true when the engine here has the image; false
# when it answers that it has no such image; any other answer fails the
# run - an engine that cannot be asked is not taken for "absent".
k8s_image_present() {
	_k_err=$(k8s_engine query image inspect "$1" 2>&1 >/dev/null </dev/null) && return 0
	case $(printf '%s' "$_k_err" | tr 'A-Z' 'a-z') in
	*"no such image"* | *"image not known"* | *"failed to find image"*) return 1 ;;
	esac
	fail "asking the engine for $1 failed: $(printf '%s' "$_k_err" | printable | head -n 3)"
}

# k8s_render <values file> [<Kubernetes version>]: the chart rendered with
# the values, on standard output - "helm template", never a release. With
# HELM, that program here; else the helm image (k8s_helm_image) in a
# container of the engine here (k8s_engine) that reaches no network, mounts
# nothing and is given the chart and the values as a tar stream on its
# standard input. The image must be here or is pulled first; OFFLINE=yes
# refuses the pull.
k8s_render() {
	k8s_in_workdir tmp k8s_render_in "$1" "${2:-}"
}

k8s_render_in() {
	_k_values=$1
	_k_kube=$2
	_k_tmp=$3
	_k_release=$(setting DEPLOY_NAME)
	_k_namespace=$(k8s_ns)
	cp -R "$REPO_ROOT/deploy/helm/cactusfs" "$_k_tmp/chart" || fail "copying the chart failed"
	cp "$_k_values" "$_k_tmp/values.yaml" || fail "copying the values failed"
	_k_helm=$(setting HELM)
	if [ -n "$_k_helm" ]; then
		command -v "$_k_helm" >/dev/null 2>&1 || refuse "HELM=$_k_helm: no such program"
		query "$_k_helm" template "$_k_release" "$_k_tmp/chart" --namespace "$_k_namespace" -f "$_k_tmp/values.yaml" \
			${_k_kube:+--kube-version} ${_k_kube:+"$_k_kube"} </dev/null || fail "helm template failed"
		return 0
	fi
	_k_image=$(k8s_helm_image) || exit
	command -v docker >/dev/null 2>&1 || command -v podman >/dev/null 2>&1 ||
		refuse "rendering the chart needs docker or podman here, to run $_k_image - or HELM, a helm program"
	if ! k8s_image_present "$_k_image"; then
		if [ "$(setting OFFLINE)" = yes ]; then
			refuse "OFFLINE=yes, and the helm image $_k_image is not here: load it into the engine, or name a helm program with HELM"
		fi
		# What the pull says goes to standard error: standard output is the
		# render.
		k8s_engine run pull "$_k_image" </dev/null >&2 || fail "pulling $_k_image failed"
		if [ "$DRY_RUN" = 1 ]; then
			say "dry run: the chart is rendered once $_k_image is pulled" >&2
			return 0
		fi
	fi
	# The container: no network, a read-only root, nothing mounted from
	# here, every capability dropped; the tar is unpacked into a tmpfs and
	# helm renders from there. The tar is made first, whole, into the work
	# directory: a stream cut short never reaches helm.
	(cd "$_k_tmp" && env COPYFILE_DISABLE=1 tar --format ustar -cf input.tar chart values.yaml) || fail "packing the chart failed"
	k8s_engine query run --rm -i --pull=never --network none --read-only --tmpfs /work \
		--cap-drop ALL --security-opt no-new-privileges --env HOME=/work \
		--entrypoint sh "$_k_image" \
		-c 'set -e; tar -xo -C /work; exec helm template "$@"' sh \
		"$_k_release" /work/chart --namespace "$_k_namespace" -f /work/values.yaml \
		${_k_kube:+--kube-version} ${_k_kube:+"$_k_kube"} <"$_k_tmp/input.tar" ||
		fail "helm template in $_k_image failed"
}

# --- reading kubectl's JSON ----------------------------------------------------------
#
# "kubectl get -o json" indents by four spaces a level; these read one value
# at a known place, never a key of the same name elsewhere (a key of a
# Secret's data, an annotation's value).

# k8s_json_meta <file> <labels|annotations> <key>: the value of a label or an
# annotation of the object; exit 1 when it has none.
k8s_json_meta() {
	awk -v map="$2" -v key="$3" '
		/^    "metadata": \{/ { meta = 1; next }
		meta && /^    \}/ { meta = 0 }
		meta && $0 ~ "^        \"" map "\": \\{" { inmap = 1; next }
		inmap && /^        \}/ { inmap = 0 }
		inmap && /^            "/ {
			line = $0
			sub(/^ *"/, "", line)
			name = line
			sub(/": .*$/, "", name)
			if (name == key) {
				sub(/^[^"]*": *"/, "", line)
				sub(/",?$/, "", line)
				print line
				found = 1
			}
		}
		END { exit found ? 0 : 1 }
	' "$1"
}

# k8s_json_top <file> <field>: a string field at the top of the object
# (type, kind); exit 1 when it has none.
k8s_json_top() {
	awk -v key="$2" '
		$0 ~ "^    \"" key "\": \"" {
			line = $0
			sub(/^[^"]*"[^"]*": *"/, "", line)
			sub(/",?$/, "", line)
			print line
			found = 1
		}
		END { exit found ? 0 : 1 }
	' "$1"
}

# k8s_json_rv <file>: metadata.resourceVersion of the object.
k8s_json_rv() {
	awk '
		/^    "metadata": \{/ { meta = 1; next }
		meta && /^    \}/ { meta = 0 }
		meta && /^        "resourceVersion": "/ {
			line = $0
			sub(/^[^"]*"resourceVersion": *"/, "", line)
			sub(/",?$/, "", line)
			print line
			found = 1
		}
		END { exit found ? 0 : 1 }
	' "$1"
}

# k8s_secret_b64 <Secret's JSON file> <key>: the base64 of a key of the
# Secret's data; exit 1 when it has no such key.
k8s_secret_b64() {
	awk -v key="$2" '
		/^    "data": \{/ { data = 1; next }
		data && /^    \}/ { data = 0 }
		data {
			line = $0
			sub(/^ *"/, "", line)
			name = line
			sub(/": .*$/, "", name)
			if (name == key) {
				sub(/^[^"]*": *"/, "", line)
				sub(/",?$/, "", line)
				print line
				found = 1
			}
		}
		END { exit found ? 0 : 1 }
	' "$1"
}

# k8s_forbidden <stderr file> <what was asked>: when kubectl's answer says
# Forbidden, fails naming the permission the context lacks (ADR-0033
# section 7: the rules --print-namespace prints).
k8s_forbidden() {
	if grep -qi forbidden "$1"; then
		fail "the context may not $2 (Forbidden): give it the rules deploy/deploy.sh --print-namespace prints (ADR-0033 section 7)"
	fi
}

# --- the namespace ------------------------------------------------------------------

# k8s_state_put <name> <value>: a value that is no secret (the namespace
# marker) into the file of that name in the deployment's STATE_DIR entry -
# the directories 0700, the file 0600. With --dry-run nothing is written.
# Called outside a command substitution: a write that fails ends the run.
k8s_state_put() {
	if [ "$DRY_RUN" = 1 ]; then
		say "dry run: $STATE/$1 is not written" >&2
		return 0
	fi
	(
		umask 077
		mkdir -p "$STATE" && printf '%s\n' "$2" >"$STATE/$1.new" && mv -f "$STATE/$1.new" "$STATE/$1"
	) || fail "writing $STATE/$1 failed"
}

# A function that may refuse or fail and prints its answer is called in a
# command substitution with "|| exit": bash, outside its POSIX mode, does
# not apply "set -e" in one, and the refusal would be lost. One that writes
# is called outside any.

# k8s_marker_ok <text>: true for a marker the script writes, 32 hexadecimal
# digits.
k8s_marker_ok() {
	case $1 in
	'' | *[!0123456789abcdef]*) return 1 ;;
	esac
	[ "${#1}" = 32 ]
}

# k8s_uid_recorded: the namespace marker kept in STATE_DIR, or nothing.
k8s_uid_recorded() {
	[ -f "$STATE/namespace-uid" ] || return 0
	_k_uid=$(printable <"$STATE/namespace-uid" | tr -d '\n')
	k8s_marker_ok "$_k_uid" || refuse "$STATE/namespace-uid does not hold a marker (32 hexadecimal digits); it was changed by hand"
	printf '%s' "$_k_uid"
}

# k8s_uid: sets _k_uid to the namespace marker of the deployment - the one
# kept in STATE_DIR, else a new random one, recorded there (unless
# --dry-run) before it is used. Not in a command substitution.
k8s_uid() {
	_k_uid=$(k8s_uid_recorded) || exit
	if [ -z "$_k_uid" ]; then
		_k_uid=$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n') || fail "no random marker could be read from /dev/urandom"
		k8s_marker_ok "$_k_uid" || fail "no random marker could be read from /dev/urandom"
		k8s_state_put namespace "$(k8s_ns)"
		k8s_state_put namespace-uid "$_k_uid"
	fi
}

# k8s_ns_recorded: the namespace's name recorded in the entry beside its
# marker, or nothing.
k8s_ns_recorded() {
	[ -f "$STATE/namespace" ] || return 0
	printable <"$STATE/namespace" | tr -d '\n'
}

# k8s_ns_record_check: a deployment whose entry records its namespace is
# acted on in that namespace only: settings that name another
# (K8S_NAMESPACE changed, or DEPLOY_NAME with no K8S_NAMESPACE) are refused
# by every action - an install would make a second namespace with the
# first one's secrets, and an uninstall or a purge would act on a namespace
# the entry does not describe.
k8s_ns_record_check() {
	_k_nr=$(k8s_ns_recorded)
	[ -z "$_k_nr" ] || [ "$_k_nr" = "$(k8s_ns)" ] ||
		refuse "$STATE records the namespace $_k_nr, and the settings name $(k8s_ns): a deployment stays in the namespace it was made in; set K8S_NAMESPACE=$_k_nr, or purge the deployment first"
}

# k8s_namespace_yaml <uid>: the deployment's Namespace, marked as the
# script's own and enforcing the restricted Pod Security Standard.
k8s_namespace_yaml() {
	printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n'
	printf '  name: %s\n' "$(k8s_ns)"
	printf '  labels:\n'
	printf '    %s: %s\n' "$LABEL_NAME" "$(k8s_q "$(setting DEPLOY_NAME)")"
	printf '    %s: %s\n' "$LABEL_TARGET" "$(k8s_q "$TARGET")"
	printf '    pod-security.kubernetes.io/enforce: restricted\n'
	printf '  annotations:\n'
	printf '    %s: %s\n' "$MARKER_ANNOTATION" "$(k8s_q "$1")"
}

# k8s_imports: true when the image goes to the cluster by an import into
# k3s (PLAN 10.4's route import): K3S_SSH, K3S_IMPORT, or a k3s here,
# without K8S_REGISTRY. Only then does a purge look for the image tags
# still in use, across namespaces.
k8s_imports() {
	[ "$TARGET" = k3s ] && [ -z "$(setting K8S_REGISTRY)" ] || return 1
	[ -n "$(setting K3S_SSH)" ] || [ -n "$(setting K3S_IMPORT)" ] || [ -z "$(kube_context)" ]
}

# k8s_rbac_yaml: what a context bound to the namespace needs for the script
# (ADR-0033 section 7): a Role in the namespace for the objects it makes and
# reads there, and a ClusterRole for its reads outside - listing Nodes,
# StorageClasses, Services, PersistentVolumes (the purge's check of PLAN
# 10.11) and - when the image is imported into k3s (k8s_imports) - pods,
# StatefulSets, Deployments, DaemonSets, Jobs and CronJobs (whether
# anything still names an image tag a purge would remove), getting its own
# namespace and kube-system by name.
# Bindings name the operator's identity, which only the administrator
# knows; --print-namespace says the commands.
k8s_rbac_yaml() {
	_k_ns=$(k8s_ns)
	printf '%s\n' '---' 'apiVersion: rbac.authorization.k8s.io/v1' 'kind: Role' 'metadata:'
	printf '  name: cactusfs-deploy\n  namespace: %s\n' "$_k_ns"
	printf 'rules:\n'
	printf '  - apiGroups: [""]\n    resources: [secrets, services, persistentvolumeclaims, serviceaccounts]\n'
	printf '    verbs: [get, list, watch, create, update, patch, delete]\n'
	printf '  - apiGroups: [""]\n    resources: [pods]\n    verbs: [get, list, watch, delete]\n'
	printf '  - apiGroups: [""]\n    resources: [pods/portforward]\n    verbs: [create]\n'
	printf '  - apiGroups: [apps]\n    resources: [statefulsets]\n    verbs: [get, list, watch, create, update, patch, delete]\n'
	printf '  - apiGroups: [networking.k8s.io]\n    resources: [networkpolicies]\n    verbs: [get, list, watch, create, update, patch, delete]\n'
	printf '  - apiGroups: [policy]\n    resources: [poddisruptionbudgets]\n    verbs: [get, list, watch, create, update, patch, delete]\n'
	printf '%s\n' '---' 'apiVersion: rbac.authorization.k8s.io/v1' 'kind: ClusterRole' 'metadata:'
	printf '  name: cactusfs-deploy-%s\n' "$_k_ns"
	printf 'rules:\n'
	if k8s_imports; then
		printf '  - apiGroups: [""]\n    resources: [nodes, services, persistentvolumes, pods]\n    verbs: [list]\n'
		printf '  - apiGroups: [apps]\n    resources: [statefulsets, deployments, daemonsets]\n    verbs: [list]\n'
		printf '  - apiGroups: [batch]\n    resources: [jobs, cronjobs]\n    verbs: [list]\n'
	else
		printf '  - apiGroups: [""]\n    resources: [nodes, services, persistentvolumes]\n    verbs: [list]\n'
	fi
	printf '  - apiGroups: [storage.k8s.io]\n    resources: [storageclasses]\n    verbs: [list]\n'
	printf '  - apiGroups: [""]\n    resources: [namespaces]\n    resourceNames: [%s, kube-system]\n    verbs: [get]\n' "$_k_ns"
}

# k8s_print_namespace: --print-namespace - the manifests a cluster's
# administrator creates beforehand when K8S_CONTEXT may act in the
# namespace only: the Namespace (its marker recorded in STATE_DIR, so that
# the install takes it for the deployment's own), the Role and the
# ClusterRole of k8s_rbac_yaml; and, on standard error, the two bindings to
# make for the operator's identity. Asks the cluster nothing.
k8s_print_namespace() {
	k8s_ns_record_check
	k8s_uid
	[ "$DRY_RUN" = 0 ] || warn "dry run: the marker $_k_uid is not recorded, so a namespace made from this manifest would be refused; run again without --dry-run"
	say "the namespace $(k8s_ns), its Role and the ClusterRole of the script's reads outside it, for the cluster's administrator to create (kubectl create -f <file>); the marker is kept in $STATE; then bind them to the identity of K8S_CONTEXT:" >&2
	say "  kubectl create rolebinding cactusfs-deploy -n $(k8s_ns) --role=cactusfs-deploy --user=<identity>" >&2
	say "  kubectl create clusterrolebinding cactusfs-deploy-$(k8s_ns) --clusterrole=cactusfs-deploy-$(k8s_ns) --user=<identity>" >&2
	k8s_namespace_yaml "$_k_uid"
	k8s_rbac_yaml
}

# k8s_tty: true when the run may ask a question at a terminal.
k8s_tty() {
	[ -t 0 ]
}

# k8s_namespace_read <file>: the namespace as the cluster has it, as JSON
# into the file - empty when there is none. Read as JSON, since a jsonpath
# would not be plain enough for ssh. Forbidden names the permission.
k8s_namespace_read() {
	if ! k8s_cmd query get namespace "$(k8s_ns)" --ignore-not-found -o json </dev/null >"$1" 2>"$1.err"; then
		printable <"$1.err" >&2
		k8s_forbidden "$1.err" "get the namespace $(k8s_ns) (get on namespaces, resourceNames [$(k8s_ns)])"
		fail "reading the namespace $(k8s_ns) failed"
	fi
	printable <"$1.err" >&2
}

# k8s_helm_release_check: a namespace holding a Helm release of the
# deployment's name is refused - the script and Helm never manage the same
# objects (ADR-0033 section 6). Helm keeps a release in Secrets of the
# namespace labelled owner=helm.
k8s_helm_release_check() {
	_k_release=$(k8s_cmd query get secrets -n "$(k8s_ns)" -l "owner=helm,name=$(setting DEPLOY_NAME)" -o name </dev/null) ||
		fail "reading the Secrets of the namespace $(k8s_ns) failed"
	[ -z "$_k_release" ] ||
		refuse "the namespace $(k8s_ns) holds a Helm release named $(setting DEPLOY_NAME): the script does not take over what Helm manages - remove it with helm uninstall, or deploy under another DEPLOY_NAME or K8S_NAMESPACE"
}

# k8s_namespace_ensure: the namespace is the deployment's own, or is made so.
# One that exists is the deployment's when its marker is the one in
# STATE_DIR; only then is it asked whether it holds a Helm release. A marker
# of the script's form without a local record, on a namespace labelled with
# the deployment's name, is taken only when the name is typed at a terminal
# (and then recorded again). One that does not exist is made by a create of
# the marked object - never an apply, which would adopt a namespace of the
# same name made a moment before by somebody else. The marker is recorded
# before the create.
k8s_namespace_ensure_in() {
	_k_ns_json=$1/namespace.json
	_k_namespace=$(k8s_ns)
	k8s_namespace_read "$_k_ns_json"
	if [ ! -s "$_k_ns_json" ]; then
		printf absent
		exit 0
	fi
	_k_marker=$(k8s_json_meta "$_k_ns_json" annotations "$MARKER_ANNOTATION") || _k_marker=
	_k_recorded=$(k8s_uid_recorded) || exit
	if [ -n "$_k_marker" ] && [ "$_k_marker" = "$_k_recorded" ]; then
		k8s_helm_release_check
		printf ours
		exit 0
	fi
	if [ -z "$_k_marker" ]; then
		refuse "the namespace $_k_namespace exists and is not the deployment's (it has no $MARKER_ANNOTATION): the script does not take over a namespace it did not make - choose another K8S_NAMESPACE, or have the administrator create it from deploy/deploy.sh --print-namespace"
	fi
	k8s_marker_ok "$_k_marker" || refuse "the namespace $_k_namespace carries a marker that is not one the script writes: not taken over"
	if [ -n "$_k_recorded" ]; then
		refuse "the namespace $_k_namespace is marked $_k_marker, and $STATE records $_k_recorded: it is another deployment's, or the settings point at another cluster"
	fi
	_k_label=$(k8s_json_meta "$_k_ns_json" labels "$LABEL_NAME") || _k_label=
	[ "$_k_label" = "$(setting DEPLOY_NAME)" ] ||
		refuse "the namespace $_k_namespace is marked $_k_marker, but labelled $LABEL_NAME=${_k_label:-<none>}, not $(setting DEPLOY_NAME): not taken over"
	if ! k8s_tty; then
		refuse "the namespace $_k_namespace is marked $_k_marker, and $STATE has no record of it (it was lost, or made on another machine): run at a terminal to confirm it is this deployment's"
	fi
	k8s_helm_release_check
	printf 'deploy: the namespace %s is marked as made by this script, but %s has no record of it.\ndeploy: type the deployment'"'"'s name, %s, to take it as this deployment'"'"'s: ' "$_k_namespace" "$STATE" "$(setting DEPLOY_NAME)" >&2
	IFS= read -r _k_answer || _k_answer=
	[ "$_k_answer" = "$(setting DEPLOY_NAME)" ] || refuse "not confirmed: the namespace $_k_namespace is left as it is"
	printf 'adopt:%s' "$_k_marker"
}

# k8s_namespace_ensure: k8s_namespace_check, then k8s_namespace_make.
k8s_namespace_ensure() {
	k8s_namespace_check
	k8s_namespace_make
}

# k8s_namespace_check: runs k8s_namespace_ensure_in in a command
# substitution with a work directory removed however the run ends (the
# run's EXIT trap holds its path); it sets _k_ne_out to "absent", "ours" or
# "adopt:<marker>" and refuses everything else. It only reads (and asks at
# a terminal).
k8s_namespace_check() {
	_k_ne_dir=$(k8s_workdir tmp) || exit
	cleanup_add rm -rf -- "$_k_ne_dir"
	_k_ne_status=0
	_k_ne_out=$(
		cleanup_none
		trap 'exit 129' HUP
		trap 'exit 130' INT
		trap 'exit 143' TERM
		k8s_namespace_ensure_in "$_k_ne_dir"
	) || _k_ne_status=$?
	rm -rf -- "$_k_ne_dir"
	cleanup_drop rm -rf -- "$_k_ne_dir"
	[ "$_k_ne_status" = 0 ] || exit "$_k_ne_status"
}

# k8s_namespace_make: what k8s_namespace_check found, made so - the marker
# and the namespace written here, outside any command substitution, where a
# failure ends the run.
k8s_namespace_make() {
	case $_k_ne_out in
	ours) ;;
	absent)
		k8s_uid
		say "the namespace $(k8s_ns) is made, marked $MARKER_ANNOTATION=$_k_uid"
		k8s_in_workdir tmp k8s_namespace_create
		;;
	adopt:*)
		k8s_marker_ok "${_k_ne_out#adopt:}" || fail "internal: adopted the marker '${_k_ne_out#adopt:}'"
		k8s_state_put namespace "$(k8s_ns)"
		k8s_state_put namespace-uid "${_k_ne_out#adopt:}"
		;;
	*) fail "internal: the namespace check said '$_k_ne_out'" ;;
	esac
	# An entry made before the name was recorded (10.9's --print-namespace)
	# gets it now.
	[ -n "$(k8s_ns_recorded)" ] || k8s_state_put namespace "$(k8s_ns)"
}

# k8s_namespace_create <work dir>: the create of the marked namespace, from
# a file of the work directory.
k8s_namespace_create() {
	k8s_namespace_yaml "$_k_uid" >"$1/namespace.yaml" || fail "writing the namespace's manifest failed"
	_k_nc_status=0
	k8s_cmd run create -f - <"$1/namespace.yaml" 2>"$1/err" || _k_nc_status=$?
	printable <"$1/err" >&2
	if [ "$_k_nc_status" != 0 ] && grep -qi forbidden "$1/err"; then
		fail "the context may not create the namespace $(k8s_ns) (Forbidden): have the cluster's administrator create it from the manifests deploy/deploy.sh --print-namespace prints (ADR-0033 section 7)"
	fi
	[ "$_k_nc_status" = 0 ] || fail "creating the namespace $(k8s_ns) failed (one of that name made meanwhile is not taken over)"
}

# --- the Secrets --------------------------------------------------------------------
#
# The cluster's copy of a secret is never overwritten from this machine
# (ADR-0033 section 3): a new or empty STATE_DIR - another machine, an old
# backup - must not replace the key file, its passphrase or the root
# credentials the data was written under. So every Secret of the deployment
# is read back first, its label and type checked, and each value compared,
# file to file, with the local copy:
#
#   both, equal          nothing is done
#   both, different      refused, naming both - but a TLS certificate or key,
#                        a Vault token or CA given by its *_FILE setting is a
#                        rotation: the cluster's Secret is replaced (kubectl
#                        replace, at the resourceVersion read back)
#   the cluster's only   copied back into STATE_DIR, with a warning
#   the local only       the Secret is created (create, never apply)
#   neither              made by PLAN 10.3 (secrets_ensure_k8s), after the
#                        copy-backs, else the run stops: not built yet
#
# A local copy must be a readable, non-empty file. A Secret of the
# deployment's name without its label is somebody else's, and refused.
# Values travel through files of a 0700 work directory in the entry and
# whole files on standard input: never a variable, never argv, never a
# stream that could be cut short on its way to kubectl.

# k8s_secret_file <file in the entry> <setting>: the file a secret is read
# from - the one the setting names, else the deployment's STATE_DIR entry's.
k8s_secret_file() {
	_k_given=$(setting "$2")
	if [ -n "$_k_given" ]; then
		printf '%s' "$_k_given"
	else
		printf '%s/%s' "$STATE" "$1"
	fi
}

# k8s_secret_class <setting>: "rotate" when the setting names a file (a
# certificate or token of the operator's own, which may be replaced), else
# "keep".
k8s_secret_class() {
	if [ -n "$(setting "$1")" ]; then
		printf rotate
	else
		printf keep
	fi
}

# k8s_secret_list: the Secrets of the deployment, one line a key:
# "<Secret><TAB><type><TAB><key><TAB><keep|rotate><TAB><file>". Paths only:
# no secret. The root credentials, the KMS passphrase, the key file, the
# node CA and the node certificates and keys are always "keep".
k8s_secret_list() {
	_k_name=$(setting DEPLOY_NAME)
	_k_t=$(printf '\t')
	_k_o="Opaque$_k_t"
	_k_tls="kubernetes.io/tls$_k_t"
	printf '%s\n' "$_k_name-root$_k_t${_k_o}access-key${_k_t}keep$_k_t$(k8s_secret_file secrets/root-access-key ROOT_ACCESS_KEY_FILE)"
	printf '%s\n' "$_k_name-root$_k_t${_k_o}secret-key${_k_t}keep$_k_t$(k8s_secret_file secrets/root-secret-key ROOT_SECRET_KEY_FILE)"
	printf '%s\n' "$_k_name-tls$_k_t${_k_tls}tls.crt$_k_t$(k8s_secret_class TLS_CERT_FILE)$_k_t$(k8s_secret_file config/s3.pem TLS_CERT_FILE)"
	printf '%s\n' "$_k_name-tls$_k_t${_k_tls}tls.key$_k_t$(k8s_secret_class TLS_KEY_FILE)$_k_t$(k8s_secret_file config/s3.key TLS_KEY_FILE)"
	if [ "$(setting DEPLOY_MODE)" = cluster ]; then
		printf '%s\n' "$_k_name-node-ca$_k_t${_k_o}ca.crt${_k_t}keep$_k_t$(k8s_secret_file config/node-ca.pem NODE_CA_FILE)"
		_k_certdir=$(setting NODE_CERT_DIR)
		for _k_id in $(k8s_node_ids); do
			if [ -n "$_k_certdir" ]; then
				_k_crt=$_k_certdir/$_k_id.crt _k_key=$_k_certdir/$_k_id.key
			else
				_k_crt=$STATE/config/nodes/$_k_id.pem _k_key=$STATE/config/nodes/$_k_id.key
			fi
			printf '%s\n' "$_k_id-node-tls$_k_t${_k_tls}tls.crt${_k_t}keep$_k_t$_k_crt"
			printf '%s\n' "$_k_id-node-tls$_k_t${_k_tls}tls.key${_k_t}keep$_k_t$_k_key"
		done
	fi
	if [ "$(setting KMS)" = vault ]; then
		printf '%s\n' "$_k_name-vault-token$_k_t${_k_o}token$_k_t$(k8s_secret_class VAULT_TOKEN_FILE)$_k_t$(k8s_secret_file secrets/vault-token VAULT_TOKEN_FILE)"
		if [ -n "$(setting VAULT_CA_FILE)" ]; then
			printf '%s\n' "$_k_name-vault-ca$_k_t${_k_o}ca.crt${_k_t}rotate$_k_t$(setting VAULT_CA_FILE)"
		fi
	else
		printf '%s\n' "$_k_name-kms$_k_t${_k_o}kms.json${_k_t}keep$_k_t$(k8s_secret_file config/kms.json KMS_KEY_FILE)"
		printf '%s\n' "$_k_name-kms-passphrase$_k_t${_k_o}passphrase${_k_t}keep$_k_t$(k8s_secret_file secrets/kms-passphrase KMS_PASSPHRASE_FILE)"
	fi
}

# k8s_secrets_yaml <work dir> [<file of Secret names> [rv]]: the Secrets -
# those the file names, or all - as YAML documents on standard output, each
# value the base64 of its file, encoded into the work directory first (never
# a variable); with "rv", each carries the resourceVersion of the copy read
# back into <work dir>/<Secret>.json. Its status says whether every file was
# encoded: the caller writes the output to a file and sends it only then.
k8s_secrets_yaml() {
	_k_wd=$1
	_k_only=${2:-}
	_k_rv=${3:-}
	_k_last=
	_k_t=$(printf '\t')
	k8s_secret_list >"$_k_wd/yaml.list" || return 1
	while IFS="$_k_t" read -r _k_secret _k_type _k_key _k_class _k_path; do
		if [ -n "$_k_only" ] && ! grep -qxF "$_k_secret" "$_k_only"; then
			continue
		fi
		if [ "$_k_secret" != "$_k_last" ]; then
			printf '%s\n' '---' 'apiVersion: v1' 'kind: Secret' 'metadata:'
			printf '  name: %s\n  namespace: %s\n' "$_k_secret" "$(k8s_ns)"
			if [ -n "$_k_rv" ]; then
				_k_version=$(k8s_json_rv "$_k_wd/$_k_secret.json") || return 1
				printf '  resourceVersion: %s\n' "$(k8s_q "$_k_version")"
			fi
			printf '  labels:\n'
			printf '    %s: %s\n' "$LABEL_NAME" "$(k8s_q "$(setting DEPLOY_NAME)")"
			printf '    %s: %s\n' "$LABEL_TARGET" "$(k8s_q "$TARGET")"
			printf '    app.kubernetes.io/managed-by: cactusfs-deploy\n'
			printf 'type: %s\ndata:\n' "$_k_type"
			_k_last=$_k_secret
		fi
		[ -f "$_k_path" ] && [ -r "$_k_path" ] && [ -s "$_k_path" ] || return 1
		base64 <"$_k_path" >"$_k_wd/yaml.enc" || return 1
		printf '  %s: ' "$_k_key"
		tr -d '\n' <"$_k_wd/yaml.enc" || return 1
		printf '\n'
	done <"$_k_wd/yaml.list"
	rm -f "$_k_wd/yaml.enc"
}

# k8s_add <file> <line>: a line onto a list of the work directory.
k8s_add() {
	printf '%s\n' "$2" >>"$1" || fail "writing $1 failed"
}

# k8s_local_ok <file>: the local copy of a secret is a readable, non-empty
# regular file - an empty one is a secret lost, not a value to compare.
k8s_local_ok() {
	[ -f "$1" ] && [ -r "$1" ] && [ -s "$1" ]
}

# k8s_secrets_generate <file of what is missing from both copies>: made by
# PLAN 10.3's secrets_ensure_k8s when it is there - the one hook, handed the
# list - else the run stops.
k8s_secrets_generate() {
	if command -v secrets_ensure_k8s >/dev/null 2>&1; then
		# Called plainly: on the left of "||" a step of it that failed would
		# be passed over ("set -e" does not hold there).
		secrets_ensure_k8s "$1"
		return 0
	fi
	not_built "10.3 for Kubernetes" "the secrets of $(setting DEPLOY_NAME) are in neither $STATE nor the namespace $(k8s_ns): $(tr '\n' ';' <"$1")making them on Kubernetes is PLAN 10.3's"
}

# k8s_secrets_ensure: the deployment's Secrets, as the table above says - the
# Kubernetes read-back and put that 10.3's secrets_install calls. Nothing is
# changed before every Secret has been read back and compared; a read that
# fails, a Secret that is not the deployment's, a local copy that is empty
# or unreadable, or a value that differs stops the run with nothing changed.
#
# It runs in the script's shell, not a subshell, so that "set -e" holds in
# every step - the hook's generation and dev-init included (the review of
# 10.10: on the left of "||", a step that failed was passed over) - with its
# work directory removed by the run's one EXIT trap (cleanup_add).
k8s_secrets_ensure() {
	_k_se_dir=$(k8s_workdir entry) || exit
	cleanup_add rm -rf -- "$_k_se_dir"
	k8s_secrets_ensure_in "$_k_se_dir"
	rm -rf -- "$_k_se_dir"
	cleanup_drop rm -rf -- "$_k_se_dir"
}

# k8s_secrets_read <work dir>: every Secret of the deployment, read back by
# name into <work dir>/<Secret>.json (empty when there is none); its label
# and type checked.
k8s_secrets_read() {
	_k_w=$1
	_k_t=$(printf '\t')
	awk -F "$_k_t" '!seen[$1]++ { print $1 "\t" $2 }' "$_k_w/list" >"$_k_w/names" || fail "listing the Secrets failed"
	while IFS="$_k_t" read -r _k_secret _k_type; do
		if ! k8s_cmd query get secret "$_k_secret" -n "$(k8s_ns)" --ignore-not-found -o json </dev/null >"$_k_w/$_k_secret.json" 2>"$_k_w/err"; then
			printable <"$_k_w/err" >&2
			k8s_forbidden "$_k_w/err" "get the Secret $_k_secret in the namespace $(k8s_ns)"
			fail "reading the Secret $_k_secret of the namespace $(k8s_ns) failed; nothing was changed"
		fi
		[ -s "$_k_w/$_k_secret.json" ] || continue
		_k_label=$(k8s_json_meta "$_k_w/$_k_secret.json" labels "$LABEL_NAME") || _k_label=
		[ "$_k_label" = "$(setting DEPLOY_NAME)" ] ||
			refuse "the namespace $(k8s_ns) has a Secret $_k_secret without the label $LABEL_NAME=$(setting DEPLOY_NAME): it is not the deployment's, and is not replaced"
		_k_have=$(k8s_json_top "$_k_w/$_k_secret.json" type) || _k_have=
		[ "$_k_have" = "$_k_type" ] ||
			refuse "the Secret $_k_secret of the namespace $(k8s_ns) is of the type ${_k_have:-<none>}, not $_k_type: it was changed by hand; nothing was changed"
	done <"$_k_w/names"
}

# k8s_secrets_classify <work dir>: compares every key with its local copy
# and writes the lists "create", "rotate", "back" and "missing" of the work
# directory; refuses what differs. In a dry run the comparison is of base64
# with base64, nothing decoded.
k8s_secrets_classify() {
	_k_w=$1
	_k_t=$(printf '\t')
	for _k_list in create rotate back missing; do
		: >"$_k_w/$_k_list" || fail "writing $_k_w/$_k_list failed"
	done
	_k_n=0
	while IFS="$_k_t" read -r _k_secret _k_type _k_key _k_class _k_path; do
		_k_n=$((_k_n + 1))
		_k_json=$_k_w/$_k_secret.json
		if [ -e "$_k_path" ] || [ -L "$_k_path" ]; then
			k8s_local_ok "$_k_path" ||
				refuse "$_k_path, the local copy of $_k_key of the Secret $_k_secret, is empty, unreadable or not a file: a secret that may already protect data is not replaced by nothing; restore it or remove it; nothing was changed"
		fi
		if [ ! -s "$_k_json" ]; then
			if [ -e "$_k_path" ]; then
				grep -qxF "$_k_secret" "$_k_w/create" || k8s_add "$_k_w/create" "$_k_secret"
			else
				k8s_add "$_k_w/missing" "$_k_path"
			fi
			continue
		fi
		k8s_secret_b64 "$_k_json" "$_k_key" >"$_k_w/b64.$_k_n" ||
			refuse "the Secret $_k_secret of the namespace $(k8s_ns) has no key $_k_key: it was changed by hand; nothing was changed"
		if [ -e "$_k_path" ]; then
			if [ "$DRY_RUN" = 1 ]; then
				base64 <"$_k_path" | tr -d '\n' >"$_k_w/mine.$_k_n" || fail "encoding $_k_path failed"
				printf '\n' >>"$_k_w/mine.$_k_n" || fail "writing $_k_w failed"
				_k_cmp=0
				cmp -s "$_k_w/b64.$_k_n" "$_k_w/mine.$_k_n" || _k_cmp=$?
			else
				base64 -d <"$_k_w/b64.$_k_n" >"$_k_w/value.$_k_n" || fail "the key $_k_key of the Secret $_k_secret is not base64"
				_k_cmp=0
				cmp -s "$_k_w/value.$_k_n" "$_k_path" || _k_cmp=$?
			fi
			case $_k_cmp in
			0) continue ;;
			1) ;;
			*) fail "comparing $_k_path with the Secret $_k_secret failed" ;;
			esac
			if [ "$_k_class" = rotate ]; then
				grep -qxF "$_k_secret" "$_k_w/rotate" || k8s_add "$_k_w/rotate" "$_k_secret"
				continue
			fi
			refuse "the Secret $_k_secret of the namespace $(k8s_ns) and $_k_path hold different values of $_k_key: the script never chooses between two copies of a secret the data may depend on - find out which is right, and remove the other (ADR-0033 section 3); nothing was changed"
		fi
		case $_k_path in
		"$STATE"/*) k8s_add "$_k_w/back" "$_k_n$_k_t$_k_secret$_k_t$_k_path" ;;
		*) fail "$_k_path, which the settings name, is not there; the Secret $_k_secret holds $_k_key" ;;
		esac
	done <"$_k_w/list"
}

# k8s_secrets_copy_back <work dir>: what the cluster alone has, into
# STATE_DIR (0600), with a warning; a dry run only says it.
k8s_secrets_copy_back() {
	_k_w=$1
	_k_t=$(printf '\t')
	while IFS="$_k_t" read -r _k_n _k_secret _k_path; do
		warn "$_k_path was missing here: copied back from the Secret $_k_secret of the namespace $(k8s_ns)"
		if [ "$DRY_RUN" = 1 ]; then
			say "dry run: $_k_path is not written"
			continue
		fi
		base64 -d <"$_k_w/b64.$_k_n" >"$_k_w/value.$_k_n" || fail "the Secret $_k_secret is not base64"
		[ -s "$_k_w/value.$_k_n" ] || refuse "the Secret $_k_secret of the namespace $(k8s_ns) holds an empty value: not copied back; nothing more was changed"
		mkdir -p "$(dirname "$_k_path")" || fail "making the directory of $_k_path failed"
		cp "$_k_w/value.$_k_n" "$_k_path.new" || fail "copying back $_k_path failed"
		chmod 600 "$_k_path.new" || fail "copying back $_k_path failed"
		mv -f "$_k_path.new" "$_k_path" || fail "copying back $_k_path failed"
	done <"$_k_w/back"
}

k8s_secrets_ensure_in() {
	_k_w=$1
	umask 077
	k8s_secret_list >"$_k_w/list" || fail "listing the Secrets failed"
	k8s_secrets_read "$_k_w"
	k8s_secrets_classify "$_k_w"
	# What the cluster alone has comes back first, so that what is made
	# next is made only for what is missing from both copies.
	k8s_secrets_copy_back "$_k_w"
	if [ -s "$_k_w/missing" ]; then
		k8s_secrets_generate "$_k_w/missing"
		k8s_secrets_classify "$_k_w"
		if [ -s "$_k_w/missing" ] && [ "$DRY_RUN" = 1 ]; then
			say "dry run: the secrets missing from both copies are made by the run without --dry-run"
			return 0
		fi
		[ ! -s "$_k_w/missing" ] || fail "after the secrets were made, still missing: $(tr '\n' ';' <"$_k_w/missing")"
		if [ "$DRY_RUN" = 0 ]; then
			k8s_secrets_copy_back "$_k_w"
		fi
	fi
	_k_ns=$(k8s_ns)
	if [ -s "$_k_w/rotate" ]; then
		say "rotated: the Secrets $(tr '\n' ' ' <"$_k_w/rotate")in the namespace $_k_ns are replaced by the files the settings name"
		if [ "$DRY_RUN" = 1 ]; then
			k8s_cmd run replace --field-manager=cactusfs-deploy -n "$_k_ns" -f - </dev/null
		else
			k8s_secrets_yaml "$_k_w" "$_k_w/rotate" rv >"$_k_w/rotate.yaml" || fail "encoding the Secrets to rotate failed; nothing was sent"
			k8s_cmd run replace --field-manager=cactusfs-deploy -n "$_k_ns" -f - <"$_k_w/rotate.yaml" ||
				fail "replacing the Secrets $(tr '\n' ' ' <"$_k_w/rotate")failed"
		fi
	fi
	if [ -s "$_k_w/create" ]; then
		say "the Secrets $(tr '\n' ' ' <"$_k_w/create")are created in the namespace $_k_ns"
		if [ "$DRY_RUN" = 1 ]; then
			k8s_cmd run create --field-manager=cactusfs-deploy -n "$_k_ns" -f - </dev/null
		else
			k8s_secrets_yaml "$_k_w" "$_k_w/create" >"$_k_w/create.yaml" || fail "encoding the Secrets failed; nothing was sent"
			k8s_cmd run create --field-manager=cactusfs-deploy -n "$_k_ns" -f - <"$_k_w/create.yaml" ||
				fail "creating the Secrets in the namespace $_k_ns failed (one made meanwhile is not replaced)"
		fi
	fi
}

# --- kubectl's JSON as paths (PLAN 10.10) ---------------------------------------------

# k8s_flat: kubectl's JSON on standard input - as "-o json" prints it, one
# key or one element a line - as one line a value on standard output:
# "<path><TAB><value>", the path the keys and indexes from the top
# (.items[0].metadata.name), the value without its quotes, its escapes as
# they are. Every value is read where it stands, so a key of the same name
# elsewhere - in a label, an annotation, a Secret's data - is never taken
# for it, whatever order the keys come in.
k8s_flat() {
	awk '
		function out(p, v) {
			if (v ~ /^".*"$/) v = substr(v, 2, length(v) - 2)
			print p "\t" v
		}
		{
			line = $0
			sub(/^[ \t]+/, "", line)
			sub(/,$/, "", line)
			if (line == "") next
			if (line == "}" || line == "]") { if (d > 0) d--; next }
			if (line == "{" || line == "[") {
				k = (line == "{") ? "o" : "a"
				if (d == 0) { d = 1; path[1] = ""; kind[1] = k; n[1] = 0; next }
				if (kind[d] == "a") { p = path[d] "[" n[d] "]"; n[d]++ } else p = path[d]
				d++; path[d] = p; kind[d] = k; n[d] = 0
				next
			}
			if (d > 0 && kind[d] == "a") { out(path[d] "[" n[d] "]", line); n[d]++; next }
			if (substr(line, 1, 1) != "\"") next
			rest = substr(line, 2)
			i = index(rest, "\": ")
			if (i == 0) next
			key = substr(rest, 1, i - 1)
			val = substr(rest, i + 3)
			p = (d > 0 ? path[d] : "") "." key
			if (val == "{" || val == "[") { d++; path[d] = p; kind[d] = (val == "{") ? "o" : "a"; n[d] = 0; next }
			if (val == "{}" || val == "[]") next
			out(p, val)
		}'
}

# k8s_at <path>: the value at the path of k8s_flat's lines on standard
# input; exit 1 when there is none.
k8s_at() {
	awk -F '\t' -v p="$1" '$1 == p { print substr($0, length($1) + 2); found = 1; exit } END { exit found ? 0 : 1 }'
}

# k8s_items <field>...: of a list's k8s_flat lines on standard input, one
# line an item, "<field>|<field>|..." - the values at those paths within
# the item (.metadata.name), in the order of the items. A field "ready" is
# the status of the item's condition of type Ready.
k8s_items() {
	awk -F '\t' -v fields="$*" '
		BEGIN { nf = split(fields, want, " ") }
		match($1, /^\.items\[[0-9]+\]/) {
			i = substr($1, 8, RLENGTH - 8) + 0
			if (i + 1 > count) count = i + 1
			f = substr($1, RLENGTH + 1)
			v = substr($0, length($1) + 2)
			val[i, f] = v
			if (match(f, /^\.status\.conditions\[[0-9]+\]\./)) {
				c = substr(f, 1, RLENGTH)
				cond[i, c, substr(f, RLENGTH + 1)] = v
				conds[i, c] = 1
			}
		}
		END {
			for (i = 0; i < count; i++) {
				line = ""
				for (j = 1; j <= nf; j++) {
					if (want[j] == "ready") {
						v = ""
						for (key in conds) {
							split(key, part, SUBSEP)
							if (part[1] == i && cond[i, part[2], "type"] == "Ready") v = cond[i, part[2], "status"]
						}
					} else v = val[i, want[j]]
					line = line (j > 1 ? "|" : "") v
				}
				print line
			}
		}'
}

# --- the cluster and the deployment's objects ------------------------------------------

# k8s_settings: the settings the install, the upgrade, --status and the
# uninstall read, read once.
k8s_settings() {
	_k8_name=$(setting DEPLOY_NAME)
	_k8_ns=$(k8s_ns)
	_k8_mode=$(setting DEPLOY_MODE)
	_k8_wait=$(setting WAIT_TIMEOUT)
	# The chart's selector of the release's pods, which the StatefulSet
	# controller puts on their claims as well.
	_k8_sel="app.kubernetes.io/instance=$_k8_name,app.kubernetes.io/name=cactusfs"
	# The deployment's own Secrets (k8s_secrets_yaml labels them).
	_k8_secret_sel="$LABEL_NAME=$_k8_name,app.kubernetes.io/managed-by=cactusfs-deploy"
	# The node whose StatefulSet holds the console upgrade's lock: the
	# first by name, as the nodes choose it (slices.Min of the node IDs,
	# internal/adapters/upgrade/kubernetes) - <name>-data-0 in cluster mode.
	_k8_first=$(k8s_node_ids | LC_ALL=C sort | head -n 1)
	_k8_lock=none
	_k8_installed=
	_k8_objects=
	_k8_owned=
	_k8_others=
	_k8_secrets=
	_k8_down=
}

# k8s_cluster_id: _engine_id, the cluster's identity - the UID of its
# namespace kube-system (ADR-0033 section 15) - asked of the cluster; one
# that cannot be asked, or does not say, fails the run. Called plainly
# (engine_id_ask, deploy/lib/secrets.sh).
k8s_cluster_id() {
	_k_ci=$(k8s_cmd query get namespace kube-system -o json </dev/null) ||
		fail "could not ask the cluster of $(engine_identity) for its namespace kube-system, whose UID is the cluster's identity (kubectl's message is above; a context bound to a namespace needs get on namespaces, resourceNames [kube-system])"
	_engine_id=$(printf '%s\n' "$_k_ci" | k8s_flat | k8s_at .metadata.uid | printable) || _engine_id=
	case $_engine_id in
	'' | *[!0123456789abcdef-]*) fail "the cluster of $(engine_identity) did not say the UID of its namespace kube-system" ;;
	esac
}

# k8s_cluster_check: the cluster is the one recorded for the deployment
# (PLAN 10.3's record): nothing of a deployment is removed through another
# cluster that answers to the same settings.
k8s_cluster_check() {
	[ -f "$STATE/target" ] || return 0
	k8s_cluster_id
	_k_cc=$(sed -n 's/^engine-id=//p' "$STATE/target" | head -n 1 | printable)
	[ "$_engine_id" = "$_k_cc" ] ||
		refuse "$STATE was made on the cluster '$_k_cc', and the settings ($(engine_identity)) now reach another, '$_engine_id' (the UID of kube-system): nothing of the deployment is changed through it; set the settings back"
	say "cluster ID (the UID of its namespace kube-system): $_engine_id, as recorded"
}

# k8s_api_server: says the API server of the context the settings name,
# read from the kubeconfig here (no question to the cluster); nothing for
# k3s, whose kubectl is its own.
k8s_api_server() {
	[ -n "$(kube_context)" ] || return 0
	_k_as=$(query "$(setting KUBECTL)" --context "$(kube_context)" config view --minify -o 'jsonpath={.clusters[0].cluster.server}' </dev/null) ||
		fail "could not read the API server of the context $(kube_context) (kubectl's message is above)"
	say "API server of the context $(kube_context): $(printf '%s\n' "$_k_as" | printable | head -n 1)"
}

# k8s_objects: _k8_objects, the chart's objects of the release in the
# namespace - StatefulSets, Services, NetworkPolicies and
# PodDisruptionBudgets with its selector's labels - as kubectl names them
# (statefulset.apps/<name>), sorted, separated by spaces; _k8_owned, those
# of them the script applied (metadata.managedFields holds the manager
# cactusfs-deploy with the operation Apply), which are the only ones a
# prune or an uninstall deletes, and _k8_others the rest, which are named
# and left; and _k8_installed, 1 when a StatefulSet is among them. A
# listing that fails fails the run: it is never taken for "none".
k8s_objects() {
	_k_ob=$(k8s_cmd query get statefulsets,services,networkpolicies,poddisruptionbudgets -n "$_k8_ns" -l "$_k8_sel" --show-managed-fields -o json </dev/null) ||
		fail "listing the deployment's objects in the namespace $_k8_ns failed (kubectl's message is above)"
	_k_ob=$(printf '%s\n' "$_k_ob" | printable | k8s_flat | awk -F '\t' '
		match($1, /^\.items\[[0-9]+\]/) {
			i = substr($1, 8, RLENGTH - 8) + 0
			if (i + 1 > count) count = i + 1
			f = substr($1, RLENGTH + 1)
			v = substr($0, length($1) + 2)
			if (f == ".kind") kind[i] = v
			else if (f == ".metadata.name") name[i] = v
			else if (match(f, /^\.metadata\.managedFields\[[0-9]+\]\./)) {
				m = substr(f, 1, RLENGTH)
				field[i, m, substr(f, RLENGTH + 1)] = v
				entries[i, m] = 1
			}
		}
		END {
			for (i = 0; i < count; i++) {
				ours = 0
				for (key in entries) {
					split(key, part, SUBSEP)
					if (part[1] == i && field[i, part[2], "manager"] == "cactusfs-deploy" && field[i, part[2], "operation"] == "Apply") ours = 1
				}
				print kind[i] " " name[i] " " ours
			}
		}')
	_k8_objects=
	_k8_owned=
	_k8_others=
	while read -r _k_ob_kind _k_ob_name _k_ob_ours; do
		[ -n "$_k_ob_name" ] || continue
		_k_ob_res=$(k8s_resource "$_k_ob_kind")
		case $_k_ob_kind in
		StatefulSet | Service | NetworkPolicy | PodDisruptionBudget) ;;
		*) fail "the listing of the deployment's objects gave a $_k_ob_kind" ;;
		esac
		_k8_objects="$_k8_objects $_k_ob_res/$_k_ob_name"
		if [ "$_k_ob_ours" = 1 ]; then
			_k8_owned="$_k8_owned $_k_ob_res/$_k_ob_name"
		else
			_k8_others="$_k8_others $_k_ob_res/$_k_ob_name"
		fi
	done <<OBJECTS
$_k_ob
OBJECTS
	# shellcheck disable=SC2086 # object names, without spaces
	_k8_objects=$(k8s_sorted $_k8_objects)
	# shellcheck disable=SC2086 # object names, without spaces
	_k8_owned=$(k8s_sorted $_k8_owned)
	# shellcheck disable=SC2086 # object names, without spaces
	_k8_others=$(k8s_sorted $_k8_others)
	_k8_installed=
	case " $_k8_objects " in
	*" statefulset.apps/"*) _k8_installed=1 ;;
	esac
}

# k8s_sorted <word>...: the words sorted, separated by single spaces.
k8s_sorted() {
	[ $# -gt 0 ] || return 0
	printf '%s\n' "$@" | LC_ALL=C sort | tr '\n' ' ' | sed 's/ *$//'
}

# k8s_nodes_check: a StatefulSet of the release that is not a node of the
# settings (DEPLOY_MODE or the name changed) holds a node and its data: an
# install never removes one, and is refused before any change.
k8s_nodes_check() {
	for _k_o in $_k8_objects; do
		case $_k_o in
		statefulset.apps/*) ;;
		*) continue ;;
		esac
		k8s_node_ids | grep -qxF "${_k_o#statefulset.apps/}" && continue
		case $_k_o in
		*-meta-[0-9]* | *-data-[0-9]*) _k_nc_was=cluster ;;
		*) _k_nc_was=standalone ;;
		esac
		refuse "the StatefulSet ${_k_o#statefulset.apps/} of $_k8_name in the namespace $_k8_ns is not a node of DEPLOY_MODE=$_k8_mode ($(k8s_node_ids | tr '\n' ' ' | sed 's/ $//')): it holds a node and its data, and an install does not remove one. Run deploy/deploy.sh --uninstall with the DEPLOY_MODE it was installed with (DEPLOY_MODE=$_k_nc_was), which removes the workload and keeps the claims, then --install with the new settings"
	done
}

# k8s_node_ports_check: a NodePort the settings name (K8S_NODE_PORT,
# K8S_ADMIN_NODE_PORT) held by any Service but the deployment's own is
# refused before anything is applied - the Services of every namespace
# are listed (read-only; ADR-0033 section 7). A port left to the cluster
# cannot collide.
k8s_node_ports_check() {
	_k_np=
	if k8s_published "$(setting K8S_SERVICE_TYPE)" && [ -n "$(setting K8S_NODE_PORT)" ]; then
		_k_np=$(setting K8S_NODE_PORT)
	fi
	if k8s_published "$(setting K8S_ADMIN_SERVICE_TYPE)" && [ -n "$(setting K8S_ADMIN_NODE_PORT)" ]; then
		_k_np="$_k_np $(setting K8S_ADMIN_NODE_PORT)"
	fi
	[ -n "$_k_np" ] || return 0
	_k_sv=$(k8s_cmd query get services -A -o json </dev/null) ||
		fail "listing the Services of every namespace failed, which a NodePort is checked against (kubectl's message is above; a context bound to a namespace needs list on services cluster-wide: --print-namespace)"
	_k_held=$(printf '%s\n' "$_k_sv" | k8s_flat | awk -F '\t' '
		match($1, /^\.items\[[0-9]+\]/) {
			i = substr($1, 8, RLENGTH - 8) + 0
			f = substr($1, RLENGTH + 1)
			v = substr($0, length($1) + 2)
			if (f == ".metadata.namespace") ns[i] = v
			else if (f == ".metadata.name") name[i] = v
			else if (f ~ /^\.spec\.ports\[[0-9]+\]\.nodePort$/) { port[++n] = v; of[n] = i }
		}
		END { for (k = 1; k <= n; k++) print ns[of[k]] "/" name[of[k]] " " port[k] }' | printable)
	for _k_port in $_k_np; do
		_k_by=$(printf '%s\n' "$_k_held" | awk -v p="$_k_port" -v mine="$_k8_ns/$_k8_name" -v admin="$_k8_ns/$_k8_name-admin" \
			'$2 == p && $1 != mine && $1 != admin { print $1; exit }')
		[ -z "$_k_by" ] ||
			refuse "the NodePort $_k_port is held by the Service $_k_by: a cluster gives each NodePort to one Service; choose another K8S_NODE_PORT or K8S_ADMIN_NODE_PORT, or leave it empty for the cluster to choose; nothing was changed"
	done
}

# --- the console upgrade's lock (ADR-0029), only read ----------------------------------

# k8s_epoch <time>: the seconds since 1970 of an RFC 3339 time
# (2026-11-22T10:00:00.5Z, or with an offset), as Go writes a heartbeat;
# nothing when it is not one.
k8s_epoch() {
	printf '%s\n' "$1" | awk '
		function days(y, m, d,   era, yoe, doy, doe) {
			if (m <= 2) y--
			era = int((y >= 0 ? y : y - 399) / 400)
			yoe = y - era * 400
			doy = int((153 * (m > 2 ? m - 3 : m + 9) + 2) / 5) + d - 1
			doe = yoe * 365 + int(yoe / 4) - int(yoe / 100) + doy
			return era * 146097 + doe - 719468
		}
		/^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9](\.[0-9]+)?(Z|[+-][0-9][0-9]:[0-9][0-9])$/ {
			t = days(substr($0, 1, 4) + 0, substr($0, 6, 2) + 0, substr($0, 9, 2) + 0) * 86400 + substr($0, 12, 2) * 3600 + substr($0, 15, 2) * 60 + substr($0, 18, 2)
			if (substr($0, length($0)) != "Z") {
				off = substr($0, length($0) - 5)
				s = (substr(off, 1, 1) == "+") ? 1 : -1
				t -= s * (substr(off, 2, 2) * 3600 + substr(off, 5, 2) * 60)
			}
			printf "%d\n", t
		}'
}

# k8s_lock: _k8_lock, what the console upgrade's lock says - the annotation
# cactusfs.io/upgrade on the StatefulSet of the first node, by name: "none";
# "stale", its heartbeat older than twice a node's time (2 x 10 minutes,
# the nodes' DefaultNodeTimeout, as ADR-0029 judges it) and a margin of 300
# seconds for a clock of this machine behind the cluster's (the heartbeat is
# the node's time, the age this machine's: a clock ahead would make a lock
# look older than it is); "fresh"; or
# "unreadable", a lock whose heartbeat cannot be read. Only read: the
# script never writes, takes over or removes it.
k8s_lock() {
	_k_lk=$(k8s_cmd query get statefulset "$_k8_first" -n "$_k8_ns" --ignore-not-found -o json </dev/null) ||
		fail "reading the StatefulSet $_k8_first of the namespace $_k8_ns failed (kubectl's message is above)"
	_k8_lock=none
	_k_lk_beat=
	_k_lk_value=$(printf '%s\n' "$_k_lk" | k8s_flat | k8s_at .metadata.annotations.cactusfs.io/upgrade) || return 0
	_k_lk_beat=$(printf '%s\n' "$_k_lk_value" | sed -n 's/.*\\"heartbeat\\": *\\"\([^\\"]*\)\\".*/\1/p' | head -n 1 | printable)
	_k_lk_at=$(k8s_epoch "$_k_lk_beat")
	if [ -z "$_k_lk_at" ]; then
		_k8_lock=unreadable
		return 0
	fi
	_k_lk_now=$(date +%s)
	if [ $((_k_lk_now - _k_lk_at)) -gt 1500 ]; then
		_k8_lock=stale
	else
		_k8_lock=fresh
	fi
}

# k8s_lock_check <before|during>: a lock with a fresh heartbeat, or one
# whose heartbeat cannot be read (the stricter reading: it may be fresh),
# refuses the run before anything is moved (exit 2), or stops it during an
# upgrade (exit 1). A stale lock is said and left to the nodes, which
# settle it.
k8s_lock_check() {
	k8s_lock
	case $_k8_lock in
	none) return 0 ;;
	stale)
		warn "the StatefulSet $_k8_first holds a console upgrade's lock (cactusfs.io/upgrade) whose heartbeat, $_k_lk_beat, is older than 25 minutes: its holder is gone, and the nodes settle it (ADR-0029); the script neither takes it over nor removes it"
		return 0
		;;
	fresh) _k_lk_why="an upgrade from the console (ADR-0029) holds the lock cactusfs.io/upgrade on the StatefulSet $_k8_first, its heartbeat $_k_lk_beat" ;;
	*) _k_lk_why="the StatefulSet $_k8_first holds the console upgrade's lock cactusfs.io/upgrade, and its heartbeat cannot be read (a lock that does not parse is settled by the nodes when an upgrade from the console starts again)" ;;
	esac
	_k_lk_why="$_k_lk_why: the script and the console would move the same nodes and undo each other's work, and the script never writes, takes over or removes that lock - let the console's run end (its page says how it stands), then run again"
	[ "$1" = during ] || refuse "$_k_lk_why"
	fail "upgrade stopped: $_k_lk_why"
}

# --- pods ------------------------------------------------------------------------------

# k8s_pod <pod>: _k8_pod_uid, _k8_pod_ready (True, False or nothing),
# _k8_pod_rev (its controller-revision-hash) and _k8_pod_image (the image
# its container runs) of the pod as the cluster has it; all empty when
# there is none. Called plainly.
k8s_pod() {
	_k_pj=$(k8s_cmd query get pod "$1" -n "$_k8_ns" --ignore-not-found -o json </dev/null) ||
		fail "reading the pod $1 of the namespace $_k8_ns failed (kubectl's message is above)"
	_k_pf=$(printf '%s\n' "$_k_pj" | printable | k8s_flat)
	_k8_pod_uid=$(printf '%s\n' "$_k_pf" | k8s_at .metadata.uid) || _k8_pod_uid=
	_k8_pod_rev=$(printf '%s\n' "$_k_pf" | k8s_at .metadata.labels.controller-revision-hash) || _k8_pod_rev=
	_k8_pod_image=$(printf '%s\n' "$_k_pf" | k8s_at '.spec.containers[0].image') || _k8_pod_image=
	_k8_pod_ready=$(printf '%s\n' "$_k_pf" | sed 's/^\./.items[0]./' | k8s_items ready)
	[ -n "$_k8_pod_uid" ] || _k8_pod_ready=
}

# k8s_wait_pod <pod> <uid it must not have> <revision> <image>: true once
# the pod is there with another UID than the one given (any, when none is
# given), Ready, of the revision (when given) and running the image -
# asked every 2 seconds until WAIT_TIMEOUT seconds have passed; false when
# it never was.
k8s_wait_pod() {
	_k_wp_start=$(date +%s)
	while :; do
		k8s_pod "$1"
		if [ -n "$_k8_pod_uid" ] && [ "$_k8_pod_uid" != "$2" ] && [ "$_k8_pod_ready" = True ] &&
			{ [ -z "$3" ] || [ "$_k8_pod_rev" = "$3" ]; } && [ "$_k8_pod_image" = "$4" ]; then
			return 0
		fi
		_k_wp_now=$(date +%s)
		[ $((_k_wp_now - _k_wp_start)) -lt "$_k8_wait" ] || return 1
		sleep 2
	done
}

# k8s_revision <node>: _k8_rev, the revision the pod of the node's
# StatefulSet is to run, once its controller has seen the apply
# (status.observedGeneration equal to metadata.generation) - asked every 2
# seconds until WAIT_TIMEOUT seconds have passed.
k8s_revision() {
	_k_rv_start=$(date +%s)
	while :; do
		_k_sj=$(k8s_cmd query get statefulset "$1" -n "$_k8_ns" -o json </dev/null) ||
			fail "reading the StatefulSet $1 of the namespace $_k8_ns failed (kubectl's message is above)"
		_k_sf=$(printf '%s\n' "$_k_sj" | printable | k8s_flat)
		_k_gen=$(printf '%s\n' "$_k_sf" | k8s_at .metadata.generation) || _k_gen=
		_k_obs=$(printf '%s\n' "$_k_sf" | k8s_at .status.observedGeneration) || _k_obs=
		_k8_rev=$(printf '%s\n' "$_k_sf" | k8s_at .status.updateRevision) || _k8_rev=
		if [ -n "$_k_gen" ] && [ "$_k_gen" = "$_k_obs" ] && [ -n "$_k8_rev" ]; then
			return 0
		fi
		_k_rv_now=$(date +%s)
		[ $((_k_rv_now - _k_rv_start)) -lt "$_k8_wait" ] ||
			fail "the controller of the StatefulSet $1 did not take the apply within WAIT_TIMEOUT=$_k8_wait s (generation ${_k_gen:-?}, observed ${_k_obs:-?}); no pod of it was deleted"
		sleep 2
	done
}

# k8s_node_order: the nodes in the order an upgrade replaces them - a node
# that was not ready when the run began first (k8s_down_check: it is away
# already, and replacing it takes no second node away), then those that do
# not vote, then the voters. The admin API, which could name the leader, is
# not asked: it needs a credential, and none is put on a command line; the
# voters go in the order of their index (PLAN 10.11, Decisions).
k8s_node_order() {
	[ -z "$_k8_down" ] || printf '%s\n' "$_k8_down"
	if [ "$_k8_mode" = cluster ]; then
		_k_no="$_k8_name-data-0 $_k8_name-meta-0 $_k8_name-meta-1 $_k8_name-meta-2"
	else
		_k_no=$(k8s_node_ids)
	fi
	for _k_n in $_k_no; do
		[ "$_k_n" = "$_k8_down" ] || printf '%s\n' "$_k_n"
	done
}

# k8s_not_ready: _k8_not_ready, the nodes (by ID, separated by spaces)
# whose pod is not there, not Ready, or being deleted - from one listing of
# the release's pods, which must succeed.
k8s_not_ready() {
	_k_nr_pods=$(k8s_cmd query get pods -n "$_k8_ns" -l "$_k8_sel" -o json </dev/null) ||
		fail "listing the pods of $_k8_name failed (kubectl's message is above)"
	_k_nr_pods=$(printf '%s\n' "$_k_nr_pods" | printable | k8s_flat | k8s_items .metadata.name ready .metadata.deletionTimestamp)
	_k8_not_ready=
	for _k_nr_id in $(k8s_node_ids); do
		printf '%s\n' "$_k_nr_pods" | grep -qx "$_k_nr_id-0|True|" || _k8_not_ready="$_k8_not_ready $_k_nr_id"
	done
	_k8_not_ready=${_k8_not_ready# }
}

# k8s_down_check: before an upgrade begins, every node's pod must be Ready
# but one at most: with two away a cluster is below what it reads and
# writes with, and an upgrade would take a third. One that is not ready is
# replaced first (_k8_down). Refused (exit 2) otherwise, nothing changed.
k8s_down_check() {
	k8s_not_ready
	case $_k8_not_ready in
	'') _k8_down= ;;
	*' '*) refuse "the nodes $_k8_not_ready of $_k8_name are not Ready: an upgrade takes one node away at a time, and with more than one away already it would take the cluster below what it reads and writes with; bring them back first ($(k8s_kubectl_said) get pods -n $_k8_ns)" ;;
	*)
		_k8_down=$_k8_not_ready
		say "upgrade: the node $_k8_down is not Ready; it is replaced first"
		;;
	esac
}

# k8s_kubectl_said: the kubectl an operator types for this target, for the
# messages.
k8s_kubectl_said() {
	if [ "$TARGET" = k3s ] && [ -n "$(setting K3S_SSH)" ]; then
		printf 'ssh %s k3s kubectl' "$(setting K3S_SSH)"
	elif [ -n "$(kube_context)" ]; then
		printf '%s --context %s' "$(setting KUBECTL)" "$(kube_context)"
	else
		printf 'k3s kubectl'
	fi
}

# k8s_wait_all: a new deployment - every node's pod Ready on the image
# within WAIT_TIMEOUT, or the run fails (exit 1) naming the first that is
# not.
k8s_wait_all() {
	if [ "$DRY_RUN" = 1 ]; then
		say "dry run: would wait up to $_k8_wait s for each node's pod to be Ready on $_k8_ref"
		return 0
	fi
	for _k_id in $(k8s_node_ids); do
		k8s_wait_pod "$_k_id-0" '' '' "$_k8_ref" ||
			fail "the node $_k_id is not Ready on $_k8_ref within WAIT_TIMEOUT=$_k8_wait s (its pod: ${_k8_pod_uid:-not there}, ready: ${_k8_pod_ready:-no}); see $(k8s_kubectl_said) describe pod $_k_id-0 -n $_k8_ns, and its logs"
		say "ready: $_k_id runs $_k8_ref"
	done
}

# k8s_upgrade: an installed deployment after the apply - its pods replaced
# one at a time in k8s_node_order (OnDelete: a pod takes the new template
# when it is deleted), each Ready on the new revision and image before the
# next; the first that is not stops the run (exit 1), the nodes after it
# untouched. The console upgrade's lock is read before each node. A pod on
# the new revision and image already is left as it is.
k8s_upgrade() {
	say "upgrade: one node at a time - those that do not vote, then the voters - each Ready on $_k8_ref before the next; it stops at the first that is not (ADR-0033 section 11)"
	for _k_id in $(k8s_node_order); do
		k8s_lock_check during
		if [ "$DRY_RUN" = 1 ]; then
			k8s_cmd run delete pod "$_k_id-0" -n "$_k8_ns" --wait=false </dev/null
			say "dry run: would wait up to $_k8_wait s for $_k_id-0 to be Ready on $_k8_ref before the next node"
			continue
		fi
		k8s_revision "$_k_id"
		k8s_pod "$_k_id-0"
		_k_up_was=$_k8_pod_image
		_k_up_old=
		if [ "$_k8_pod_rev" = "$_k8_rev" ] && [ "$_k8_pod_image" = "$_k8_ref" ]; then
			if [ "$_k8_pod_ready" = True ]; then
				say "upgrade: $_k_id runs $_k8_ref already, and is ready"
				continue
			fi
		else
			# No second node away: every other node's pod is Ready, or this
			# one is not deleted.
			k8s_not_ready
			_k_up_away=
			for _k_n in $_k8_not_ready; do
				[ "$_k_n" = "$_k_id" ] || _k_up_away="$_k_up_away $_k_n"
			done
			[ -z "$_k_up_away" ] ||
				fail "upgrade stopped before the node $_k_id: the nodes$_k_up_away are not Ready, and deleting the pod of $_k_id would take a second node away; nothing more was deleted"
			_k_up_old=$_k8_pod_uid
			k8s_cmd run delete pod "$_k_id-0" -n "$_k8_ns" --wait=false </dev/null ||
				fail "upgrade stopped: deleting the pod $_k_id-0 failed (kubectl's message is above); the nodes after it were not moved"
		fi
		k8s_wait_pod "$_k_id-0" "$_k_up_old" "$_k8_rev" "$_k8_ref" ||
			fail "upgrade stopped: the node $_k_id is not Ready on $_k8_ref within WAIT_TIMEOUT=$_k8_wait s (it ran ${_k_up_was:-an image not known} before); the nodes after it were not moved. See $(k8s_kubectl_said) describe pod $_k_id-0 -n $_k8_ns, and its logs; to go back, install the image it ran: IMAGE_SOURCE=image IMAGE=${_k_up_was:-<the image it ran>}"
		say "upgrade: $_k_id is Ready on $_k8_ref"
	done
}

# --- the install (PLAN 10.10) --------------------------------------------------------

# k8s_kinds <render file>: "<Kind>/<name>" of each document of the render,
# one a line.
k8s_kinds() {
	awk '
		function flush() { if (kind != "") print kind "/" name; kind = ""; name = ""; meta = 0 }
		/^---/ { flush(); next }
		/^kind: / { kind = $2; next }
		/^metadata:/ { meta = 1; next }
		/^[^ #]/ { meta = 0 }
		meta && /^  name: / && name == "" { name = $2; gsub(/"/, "", name) }
		END { flush() }
	' "$1"
}

# k8s_resource <Kind>: the name kubectl gives the kind's objects in "-o
# name", for the kinds an install applies; nothing for another.
k8s_resource() {
	case $1 in
	StatefulSet) printf statefulset.apps ;;
	Service) printf service ;;
	NetworkPolicy) printf networkpolicy.networking.k8s.io ;;
	PodDisruptionBudget) printf poddisruptionbudget.policy ;;
	StorageClass) printf storageclass.storage.k8s.io ;;
	PersistentVolume) printf persistentvolume ;;
	esac
}

# k8s_take_image_check: --take-image is for an installed deployment, while
# no console upgrade's lock is there at all, and is confirmed by the
# deployment's name - --take-image=<name>, or typed at a terminal.
k8s_take_image_check() {
	[ "$TAKE_IMAGE" = 1 ] || return 0
	[ -n "$_k8_installed" ] || refuse "--take-image: $_k8_name is not installed in the namespace $_k8_ns, so there is no image field to take back"
	[ "$_k8_lock" = none ] || refuse "--take-image is refused while the StatefulSet $_k8_first holds the console upgrade's lock (cactusfs.io/upgrade), fresh or not: the console's run is the nodes' to end"
	[ -z "$TAKE_IMAGE_CONFIRM" ] || return 0
	if [ "$DRY_RUN" = 1 ]; then
		say "dry run: --take-image asks for the name of the deployment, $_k8_name, at the terminal (or takes --take-image=$_k8_name)"
		return 0
	fi
	k8s_tty || refuse "--take-image takes the image of the nodes back from the upgrades from the console: type the deployment's name when asked at a terminal, or say --take-image=$_k8_name; nothing was changed"
	printf 'deploy: --take-image forces the image field of the StatefulSets of %s back to the script, from the upgrades from the console. Type its name to go on: ' "$_k8_name" >&2
	_k_ti_answer=
	IFS= read -r _k_ti_answer || :
	[ "$_k_ti_answer" = "$_k8_name" ] || refuse "that is not the name of the deployment, $_k8_name: nothing was changed"
}

# k8s_apply_in <work dir>: the values written, the chart rendered, what the
# render holds checked (the chart's namespaced kinds; the StorageClass and
# PersistentVolumes of K8S_STORAGE=hostpath), the render applied
# server-side in the namespace by the field manager cactusfs-deploy - never
# forced: a field another manager owns is a conflict that stops the run -
# and the chart's objects of the release that the render no longer holds
# deleted (pruned by the release's labels, in the namespace, among the
# chart's kinds; a StatefulSet never: k8s_nodes_check).
k8s_apply_in() {
	_k_w=$1
	k8s_values "$_k_w/values.yaml" "$_k8_nodes" "$_k8_node1" "$_k8_ref" "$_k8_pull" || fail "writing the values failed"
	[ -s "$_k_w/values.yaml" ] || fail "the values written are empty"
	k8s_render "$_k_w/values.yaml" >"$_k_w/render.yaml"
	if [ "$DRY_RUN" = 1 ] && [ ! -s "$_k_w/render.yaml" ]; then
		say "dry run: the render, its apply and the prune of what it no longer holds follow once the helm image is here"
		return 0
	fi
	k8s_kinds "$_k_w/render.yaml" >"$_k_w/kinds" || fail "reading the render failed"
	[ -s "$_k_w/kinds" ] || fail "the render of the chart holds no object"
	: >"$_k_w/names"
	while IFS= read -r _k_kn; do
		_k_kind=${_k_kn%%/*}
		case $_k_kind in
		StatefulSet | Service | NetworkPolicy | PodDisruptionBudget) ;;
		StorageClass | PersistentVolume)
			[ "$(setting K8S_STORAGE)" = hostpath ] ||
				fail "the render holds the cluster-scoped $_k_kn, which only K8S_STORAGE=hostpath makes: nothing was applied"
			;;
		*) fail "the render holds a $_k_kind ($_k_kn), which the script does not apply: nothing was applied" ;;
		esac
		printf '%s/%s\n' "$(k8s_resource "$_k_kind")" "${_k_kn#*/}" >>"$_k_w/names" || fail "writing $_k_w/names failed"
	done <"$_k_w/kinds"
	# What the render no longer holds is pruned when the script applied it;
	# what another field manager made is named and left.
	_k_stale=
	for _k_o in $_k8_owned; do
		case $_k_o in
		statefulset.apps/*) continue ;;
		esac
		grep -qxF "$_k_o" "$_k_w/names" || _k_stale="$_k_stale $_k_o"
	done
	for _k_o in $_k8_others; do
		grep -qxF "$_k_o" "$_k_w/names" ||
			say "left: $_k_o carries the release's labels and was not applied by the script (no manager cactusfs-deploy with the operation Apply)"
	done
	_k_force=
	if [ "$TAKE_IMAGE" = 1 ]; then
		k8s_take_image_plan "$_k_w"
	fi
	if [ -n "$_k_force" ]; then
		say "--take-image: the render is applied once with --force-conflicts by cactusfs-deploy; the dry run showed conflicts on the image and the version label of the pod template alone, which pass to cactusfs-deploy"
	fi
	_k_ap=0
	k8s_cmd run apply --server-side ${_k_force:+--force-conflicts} --field-manager=cactusfs-deploy -n "$_k8_ns" -f - <"$_k_w/render.yaml" 2>"$_k_w/apply.err" || _k_ap=$?
	printable <"$_k_w/apply.err" >&2
	if [ "$_k_ap" != 0 ]; then
		if grep -qi conflict "$_k_w/apply.err"; then
			fail "the apply met a conflict (kubectl's message is above): a field of the deployment's objects - the image, after an upgrade from the console (ADR-0029) - is owned by another field manager; nothing was forced and no node was moved. To take the image back for the script, run --install --take-image; or keep upgrading from the console"
		fi
		fail "the apply to the namespace $_k8_ns failed (kubectl's message is above); no node was moved"
	fi
	if [ -n "$_k_stale" ]; then
		# shellcheck disable=SC2086 # object names, without spaces
		k8s_cmd run delete $_k_stale -n "$_k8_ns" </dev/null ||
			fail "pruning$_k_stale from the namespace $_k8_ns failed (kubectl's message is above)"
		say "pruned what the render no longer holds:$_k_stale"
	fi
}

# The fields --take-image takes: the cactusfs container's image and the
# pod template's version label, which an upgrade from the console patches
# together (internal/adapters/upgrade/kubernetes, VersionLabel).
K8S_TAKE_IMAGE_FIELD='.spec.template.spec.containers[name="cactusfs"].image'
K8S_TAKE_LABEL_FIELD='.spec.template.metadata.labels.app.kubernetes.io/version'

# k8s_take_image_plan <work dir>: --take-image (ADR-0033 section 11) decides
# with one dry run of the whole render's apply on the server
# (--dry-run=server, which changes nothing): no conflict - nothing is
# forced; conflicts on the cactusfs container's image and the pod
# template's version label alone (what an upgrade from the console patches),
# and no other error - _k_force=1: the render is applied once with
# --force-conflicts by cactusfs-deploy, which takes exactly those fields;
# any other conflict or error - refused, nothing of the workload changed.
k8s_take_image_plan() {
	_k_tp=0
	k8s_cmd query apply --server-side --dry-run=server --field-manager=cactusfs-deploy -n "$_k8_ns" -f - <"$1/render.yaml" >/dev/null 2>"$1/dry.err" || _k_tp=$?
	printable <"$1/dry.err" >&2
	if [ "$_k_tp" = 0 ]; then
		say "--take-image: the render applies without a conflict; nothing is forced"
		return 0
	fi
	# By allowlist: every line of kubectl's answer must be part of a
	# conflict as the API server and kubectl write one (apimachinery
	# v0.37.0 managedfields/internal/conflict.go getConflictMessage;
	# kubectl v0.37.0 cmd/apply/apply.go, and cmd/util/helpers.go, which
	# prefixes "error: " to one error and none to each of several): a
	# header of one conflict, whose path follows its last ": "; a header
	# of several, or a "conflicts with" line of a further manager; a
	# "- <path>" line; one of kubectl's advice lines; a "Warning:" line; or
	# nothing. Any other line - an error that is no conflict, a connection
	# that failed - refuses, as does a count of paths other than the
	# headers say.
	cat >"$1/advice" <<'ADVICE'
Please review the fields above--they currently have other managers. Here
are the ways you can resolve this warning:
* If you intend to manage all of these fields, please re-run the apply
  command with the `--force-conflicts` flag.
* If you do not intend to manage all of the fields, please edit your
  manifest to remove references to the fields that should keep their
  current managers.
* You may co-own fields by updating your manifest to match the existing
  value; in this case, you'll become the manager if the other manager(s)
  stop managing the field (remove it from their configuration).
See https://kubernetes.io/docs/reference/using-api/server-side-apply/#conflicts
ADVICE
	printable <"$1/dry.err" >"$1/dry.txt" || fail "reading kubectl's answer failed"
	# (The manager is matched as "[^"]*" and what follows: the awk of macOS
	# does not match ".*": here.)
	awk '
		NR == FNR { advice[$0] = 1; next }
		$0 == "" || ($0 in advice) || /^Warning: / { next }
		/^(error: )?Apply failed with 1 conflict: conflict with "[^"]*".*: .+$/ {
			path = $0
			while ((i = index(path, ": ")) > 0) path = substr(path, i + 2)
			said += 1
			print "path " path
			next
		}
		/^(error: )?Apply failed with [0-9]+ conflicts: conflicts with "[^"]*".*:$/ {
			n = $0
			sub(/^(error: )?Apply failed with /, "", n)
			sub(/ conflicts: .*$/, "", n)
			said += n
			next
		}
		/^conflicts with "[^"]*".*:$/ { next }
		/^- .+$/ { print "path " substr($0, 3); read += 1; next }
		{ print "bad " $0 }
		END { print "said " said + 0 }
	' "$1/advice" "$1/dry.txt" >"$1/parsed" || fail "reading kubectl's answer failed"
	_k_tp_bad=$(sed -n 's/^bad //p' "$1/parsed" | head -n 1)
	[ -z "$_k_tp_bad" ] ||
		refuse "--take-image: the dry run of the apply failed with something that is not a conflict (kubectl's message is above): '$_k_tp_bad'; nothing was changed of the workload"
	_k_tp_said=$(sed -n 's/^said //p' "$1/parsed")
	sed -n 's/^path //p' "$1/parsed" >"$1/paths" || fail "reading kubectl's answer failed"
	_k_tp_read=$(grep -c . "$1/paths") || _k_tp_read=0
	[ "$_k_tp_said" -gt 0 ] ||
		refuse "--take-image: the dry run of the apply failed (kubectl's message is above), and not with a conflict; nothing was changed of the workload"
	[ "$_k_tp_read" = "$_k_tp_said" ] ||
		refuse "--take-image: the dry run's answer counts $_k_tp_said conflicts and names $_k_tp_read fields: it is not read as kubectl writes one; nothing was changed of the workload"
	_k_other=$(grep -vxF -e "$K8S_TAKE_IMAGE_FIELD" -e "$K8S_TAKE_LABEL_FIELD" "$1/paths" | sort -u | tr '\n' ' ') || :
	[ -z "$_k_other" ] ||
		refuse "--take-image takes the image and the version label alone, and the apply would also conflict on $_k_other(kubectl's message is above): another field manager changed more than an upgrade from the console does; nothing was changed of the workload"
	_k_force=1
}

# k8s_containerd_name <reference>: the name containerd gives an image
# imported under the reference (docker.io/library/ for a name of one part).
k8s_containerd_name() {
	case $1 in
	*/*)
		case ${1%%/*} in
		*.* | *:* | localhost) printf '%s' "$1" ;;
		*) printf 'docker.io/%s' "$1" ;;
		esac
		;;
	*) printf 'docker.io/library/%s' "$1" ;;
	esac
}

# k8s_images_record: an image imported into k3s (PLAN 10.4's route import)
# is recorded in the entry's file "images", by the name containerd gives
# it, so that a purge removes those tags and no other (PLAN 10.11).
k8s_images_record() {
	[ "${_img_route:-}" = import ] && [ "$DRY_RUN" = 0 ] || return 0
	_k_ir=$(k8s_containerd_name "$RESOLVED_IMAGE")
	# Recorded by an earlier install of this deployment: the script's.
	if [ -f "$STATE/images" ] && grep -qxF "$_k_ir" "$STATE/images"; then
		return 0
	fi
	case $_k8_image_known in
	no) ;;
	yes)
		say "image: $_k_ir was in the containerd of k3s before the import: it is not recorded, and a purge leaves it"
		return 0
		;;
	*)
		say "image: $_k_ir was imported by K3S_IMPORT's command, on a node the script cannot ask: it is not recorded, and a purge leaves it"
		return 0
		;;
	esac
	if [ -f "$STATE/images" ] && grep -qxF "$_k_ir" "$STATE/images"; then
		return 0
	fi
	(umask 077 && printf '%s\n' "$_k_ir" >>"$STATE/images") || fail "writing $STATE/images failed"
}

# k8s_ctr <run|query> <word>...: "k3s ctr -n k8s.io" with the words, over
# K3S_SSH or here; nothing for K3S_IMPORT, whose node the script cannot ask
# (exit 3, said by the caller).
k8s_ctr() {
	_k_ctr_how=$1
	shift
	if [ "$TARGET" = k3s ] && [ -n "$(setting K3S_SSH)" ]; then
		"$_k_ctr_how" on_host "$(setting K3S_SSH)" k3s ctr -n k8s.io "$@"
	elif [ "$TARGET" = k3s ] && [ -z "$(setting K3S_IMPORT)" ]; then
		"$_k_ctr_how" k3s ctr -n k8s.io "$@"
	else
		return 3
	fi
}

# k8s_image_known: _k8_image_known - "no" when the containerd of k3s has no
# image of the name the import is to give it (only then is the tag the
# script's to remove at a purge), "yes" when it has, and a reason when that
# cannot be asked (K3S_IMPORT's node). Asked of "k3s ctr -n k8s.io images
# ls -q name==<name>" (containerd's filter syntax: the name, one a line,
# nothing when there is none), before image_deliver imports.
k8s_image_known() {
	_k8_image_known=
	[ "${_img_route:-}" = import ] && [ "$DRY_RUN" = 0 ] || return 0
	_k_ik_name=$(k8s_containerd_name "$RESOLVED_IMAGE")
	plain "$_k_ik_name" || fail "the image name $_k_ik_name is not one the script hands to ctr"
	_k_ik_status=0
	_k_ik=$(k8s_ctr query images ls -q "name==$_k_ik_name" </dev/null) || _k_ik_status=$?
	case $_k_ik_status in
	0) ;;
	3)
		_k8_image_known="K3S_IMPORT's node, which the script cannot ask"
		return 0
		;;
	*) fail "asking the containerd of k3s for $_k_ik_name failed (its message is above)" ;;
	esac
	if printf '%s\n' "$_k_ik" | printable | grep -qxF "$_k_ik_name"; then
		_k8_image_known=yes
	else
		_k8_image_known=no
	fi
}

# k8s_install: --install on Kubernetes (PLAN 10.10; ADR-0033 sections 6, 7,
# 11). Everything that can refuse is asked first and changes nothing: the
# cluster's identity against the record and the secrets' files
# (secrets_check_k8s), the nodes and the storage, the namespace (ours,
# absent, or taken at a terminal), what of the release is there and the
# console upgrade's lock, --take-image, the NodePorts, the image
# (image_check). Then: the image made (image_make), the namespace made, the
# Secrets (secrets_install), the image delivered (image_deliver), the chart
# rendered and applied server-side, what it no longer holds pruned; then a
# new deployment's pods waited for, or an installed one's replaced one at a
# time.
k8s_install() {
	k8s_settings
	k8s_ns_record_check
	k8s_connect
	RESOLVED_IMAGE=
	RESOLVED_NATIVE_IMAGE=
	secrets_check_k8s
	k8s_api_server
	_k8_n=$(k8s_nodes) || exit
	_k8_nodes=${_k8_n%% *}
	_k8_node1=${_k8_n#* }
	k8s_storage_check
	# The values, written once into a directory of their own with a
	# reference of the right form: what they refuse (a cluster mode on two
	# nodes, hostpath on more than one) is refused now, and what hostpath
	# makes outside the namespace is said before anything is made.
	k8s_in_workdir tmp k8s_values_check
	_k8_values_said=1
	k8s_namespace_check
	case $_k_ne_out in
	ours | adopt:*) k8s_objects ;;
	esac
	k8s_nodes_check
	if [ -n "$_k8_installed" ]; then
		k8s_lock_check before
		k8s_down_check
		say "installed: $_k8_name in the namespace $_k8_ns ($_k8_objects); this run upgrades it one node at a time, keeping its claims"
	fi
	k8s_take_image_check
	k8s_node_ports_check
	image_check

	# Changes from here on: the image where it is made, the namespace, the
	# Secrets - before anything starts in the namespace - the image on the
	# cluster, the workload.
	image_make
	k8s_namespace_make
	secrets_install
	k8s_image_known
	image_deliver
	_k8_ref=${RESOLVED_IMAGE:-}
	case $_k8_ref in
	'' | -* | *[!A-Za-z0-9_./:@-]*) fail "the image reference for the pods is not one: '$(printf '%s' "$_k8_ref" | printable)'" ;;
	esac
	k8s_images_record
	_k8_pull=IfNotPresent
	case ${_img_route:-} in
	import | local) _k8_pull=Never ;;
	esac
	say "image: the pods run $_k8_ref (imagePullPolicy $_k8_pull)"
	if [ "$(setting K8S_STORAGE)" = hostpath ]; then
		say "K8S_STORAGE=hostpath: the directories $(setting K8S_HOSTPATH)/$_k8_ns/<node ID>/<volume> must be on the node $_k8_node1, owned by 65532 (and labelled container_file_t under SELinux): the script does not make them (ADR-0033 section 7)"
	fi
	k8s_in_workdir entry k8s_apply_in
	if [ -n "$_k8_installed" ]; then
		k8s_upgrade
	else
		k8s_wait_all
	fi
	k8s_reach_said
}

# k8s_values_check <work dir>: the values written with a reference of the
# right form, and thrown away (k8s_install).
k8s_values_check() {
	k8s_values "$1/values.yaml" "$_k8_nodes" "$_k8_node1" cactusfs:check || fail "writing the values failed"
	[ -s "$1/values.yaml" ] || fail "the values written are empty"
}

# k8s_reach_said: how the deployment is reached, at the end of an install.
k8s_reach_said() {
	say "S3: the Service $_k8_name in the namespace $_k8_ns ($(setting K8S_SERVICE_TYPE)); admin API and console: the Service $_k8_name-admin ($(setting K8S_ADMIN_SERVICE_TYPE)); deploy/deploy.sh --status lists every address and probes them"
	k8s_port_forward_said
	if [ -f "$STATE/config/s3-ca.pem" ]; then
		say "the CA to trust: $STATE/config/s3-ca.pem"
	fi
	say "root credentials: $(k8s_secret_file secrets/root-access-key ROOT_ACCESS_KEY_FILE) and $(k8s_secret_file secrets/root-secret-key ROOT_SECRET_KEY_FILE) (never printed)"
	say "a new deployment has the console user admin with the password admin (ADR-0014): change its password or delete the user before the admin Service is reachable from anywhere but this machine"
	if k8s_published "$(setting K8S_ADMIN_SERVICE_TYPE)"; then
		warn "K8S_ADMIN_SERVICE_TYPE=$(setting K8S_ADMIN_SERVICE_TYPE) publishes the admin API and console to $(setting K8S_ADMIN_ALLOW_FROM): change the password of the console user admin, or delete it, now (ADR-0014)"
	fi
}

# k8s_port_forward_said: the commands that reach the admin port from this
# machine - kubectl port-forward; with K3S_SSH, the pair ssh -L to the
# server's loopback and k3s kubectl port-forward on the server.
k8s_port_forward_said() {
	_k_pf_port=9001
	if [ "$TARGET" = k3s ] && [ -n "$(setting K3S_SSH)" ]; then
		say "the admin API from this machine: ssh -L $_k_pf_port:127.0.0.1:$_k_pf_port -- $(setting K3S_SSH), and in that session k3s kubectl port-forward -n $_k8_ns service/$_k8_name-admin $_k_pf_port:$_k_pf_port; then https://localhost:$_k_pf_port"
	else
		say "the admin API from this machine: $(k8s_kubectl_said) port-forward -n $_k8_ns service/$_k8_name-admin $_k_pf_port:$_k_pf_port; then https://localhost:$_k_pf_port"
	fi
}

# --- --status (PLAN 10.11) -----------------------------------------------------------

# k8s_namespace_peek: _k8_ns_state - "absent", "ours" (its marker is the one
# STATE records) or "other" - of the namespace, and _k_np_flat, what it
# holds (k8s_flat); read-only, asking nothing at a terminal.
k8s_namespace_peek() {
	_k_np_json=$(k8s_cmd query get namespace "$_k8_ns" --ignore-not-found -o json </dev/null) ||
		fail "reading the namespace $_k8_ns failed (kubectl's message is above)"
	_k_np_flat=
	if [ -z "$_k_np_json" ]; then
		_k8_ns_state=absent
		return 0
	fi
	_k_np_flat=$(printf '%s\n' "$_k_np_json" | printable | k8s_flat)
	_k_np_marker=$(printf '%s\n' "$_k_np_flat" | k8s_at ".metadata.annotations.$MARKER_ANNOTATION") || _k_np_marker=
	_k_np_recorded=$(k8s_uid_recorded) || exit
	if [ -n "$_k_np_marker" ] && [ "$_k_np_marker" = "$_k_np_recorded" ]; then
		_k8_ns_state=ours
	else
		_k8_ns_state=other
	fi
}

# k8s_tls_name: the name the admin port is asked for over TLS - the first
# name of TLS_HOSTS that is a plain DNS name or IPv4 address, else
# localhost, which dev-init's certificates always hold.
k8s_tls_name() {
	_k_tn_ifs=$IFS
	IFS=,
	for _k_tn in $(setting TLS_HOSTS); do
		case $_k_tn in
		*'*'* | *:* | '') ;;
		*)
			IFS=$_k_tn_ifs
			printf '%s' "$_k_tn"
			return 0
			;;
		esac
	done
	IFS=$_k_tn_ifs
	printf 'localhost'
}

# k8s_probe <address> <port> <path>: _k8_probe, the HTTP status the admin
# port at the address answers for the path, over TLS with the deployment's
# CA (TLS_CA_FILE, else STATE's), or 000 when nothing answers. Never
# through a proxy, never without verifying.
k8s_probe() {
	_k_pr_name=$(k8s_tls_name)
	_k_pr_addr=$1
	case $_k_pr_addr in
	*:*) _k_pr_addr="[$_k_pr_addr]" ;;
	esac
	_k_pr_port=$2
	_k_pr_path=$3
	set -- curl -g -s -o /dev/null --noproxy '*' --max-time 5 -w '%{http_code}'
	_k_pr_ca=$(setting TLS_CA_FILE)
	[ -n "$_k_pr_ca" ] || _k_pr_ca=$STATE/config/s3-ca.pem
	[ ! -f "$_k_pr_ca" ] || set -- "$@" --cacert "$_k_pr_ca"
	_k_pr=$(query "$@" --connect-to "$_k_pr_name:$_k_pr_port:$_k_pr_addr:$_k_pr_port" "https://$_k_pr_name:$_k_pr_port$_k_pr_path" </dev/null 2>/dev/null) || :
	_k8_probe=$(printf '%s' "$_k_pr" | printable | tr -d '\n')
	[ -n "$_k8_probe" ] || _k8_probe=000
}

# k8s_services: _k8_svc, a line a Service of the deployment that is there,
# "<name>|<type>|<node port>|<load balancer ip>|<load balancer hostname>".
k8s_services() {
	_k_sj=$(k8s_cmd query get services "$_k8_name" "$_k8_name-admin" -n "$_k8_ns" --ignore-not-found -o json </dev/null) ||
		fail "reading the Services of $_k8_name failed (kubectl's message is above)"
	_k8_svc=$(printf '%s\n' "$_k_sj" | printable | k8s_flat |
		k8s_items .metadata.name .spec.type '.spec.ports[0].nodePort' '.status.loadBalancer.ingress[0].ip' '.status.loadBalancer.ingress[0].hostname')
}

# k8s_addresses: _k8_addrs, "<machine> <address>" a line, the InternalIP and
# ExternalIP of each of the cluster's nodes (listed, read-only).
k8s_addresses() {
	_k_ad=$(k8s_cmd query get nodes -o json </dev/null) || fail "listing the cluster's nodes failed (kubectl's message is above)"
	_k8_addrs=$(printf '%s\n' "$_k_ad" | printable | k8s_flat | awk -F '\t' '
		match($1, /^\.items\[[0-9]+\]/) {
			i = substr($1, 8, RLENGTH - 8) + 0
			f = substr($1, RLENGTH + 1)
			v = substr($0, length($1) + 2)
			if (f == ".metadata.name") name[i] = v
			else if (match(f, /^\.status\.addresses\[[0-9]+\]\./)) {
				a = substr(f, 1, RLENGTH)
				addr[i, a, substr(f, RLENGTH + 1)] = v
				list[i, a] = 1
			}
		}
		END {
			for (key in list) {
				split(key, part, SUBSEP)
				t = addr[part[1], part[2], "type"]
				if (t == "InternalIP" || t == "ExternalIP") print name[part[1]] " " addr[part[1], part[2], "address"]
			}
		}' | sort)
}

# k8s_status: --status on Kubernetes - the pods (machine, readiness, image,
# version), every address (a NodePort on the addresses of the machines that
# run a ready pod of a gateway, which externalTrafficPolicy Local serves
# only there; a load balancer's ingress), /healthz and /readyz of the
# published admin port with the CA, the certificates' expiry, a console
# upgrade's lock, and the port-forward commands when no admin port
# answered. Read-only. Exits 0 when every node is ready or nothing is
# installed, 1 when it is installed and not ready.
k8s_status() {
	k8s_settings
	k8s_ns_record_check
	k8s_connect
	k8s_namespace_peek
	case $_k8_ns_state in
	absent)
		say "$_k8_name is not installed: $(engine_identity) has no namespace $_k8_ns"
		return 0
		;;
	other) refuse "the namespace $_k8_ns is not this deployment's: it does not carry the marker $STATE records (deploy.cactusfs.io/owner-uid)" ;;
	esac
	k8s_objects
	_k_st_pods=$(k8s_cmd query get pods -n "$_k8_ns" -l "$_k8_sel" -o json </dev/null) ||
		fail "listing the pods of $_k8_name failed (kubectl's message is above)"
	_k_st_pods=$(printf '%s\n' "$_k_st_pods" | printable | k8s_flat |
		k8s_items .metadata.name .spec.nodeName ready '.spec.containers[0].image' .metadata.labels.app.kubernetes.io/version .metadata.labels.cactusfs.io/gateway)
	if [ -z "$_k8_objects" ] && [ -z "$_k_st_pods" ]; then
		say "$_k8_name is not installed in the namespace $_k8_ns (the namespace, its claims and $STATE are kept: --install runs it again with them; --uninstall --purge deletes them)"
		return 0
	fi
	_k_st_ok=1
	_k_st_gw=
	for _k_id in $(k8s_node_ids); do
		_k_st_line=$(printf '%s\n' "$_k_st_pods" | grep "^$_k_id-0|" | head -n 1) || _k_st_line=
		if [ -z "$_k_st_line" ]; then
			say "node: $_k_id has no pod"
			_k_st_ok=0
			continue
		fi
		IFS='|' read -r _k_st_pod _k_st_node _k_st_ready _k_st_image _k_st_version _k_st_gateway <<STATUS
$_k_st_line
STATUS
		if [ "$_k_st_ready" = True ]; then
			_k_st_said=ready
			[ "$_k_st_gateway" != true ] || _k_st_gw="$_k_st_gw $_k_st_node"
		else
			_k_st_said="NOT ready"
			_k_st_ok=0
		fi
		say "node: $_k_id, pod $_k_st_pod on ${_k_st_node:-no machine yet}, $_k_st_said, image $_k_st_image, version ${_k_st_version:-unknown}"
	done
	k8s_lock
	case $_k8_lock in
	fresh) say "an upgrade from the console holds the lock cactusfs.io/upgrade on $_k8_first (heartbeat $_k_lk_beat): --install waits for it to end" ;;
	stale | unreadable) say "the StatefulSet $_k8_first holds a console upgrade's lock that is $_k8_lock (heartbeat ${_k_lk_beat:-unreadable}); the nodes settle it" ;;
	esac
	k8s_services
	_k8_addrs=
	if printf '%s\n' "$_k8_svc" | grep -Eq '\|(NodePort|LoadBalancer)\|'; then
		k8s_addresses
	fi
	_k_st_answered=0
	while IFS='|' read -r _k_sv_name _k_sv_type _k_sv_port _k_sv_ip _k_sv_host; do
		[ -n "$_k_sv_name" ] || continue
		_k_sv_in=9000
		_k_sv_what=S3
		if [ "$_k_sv_name" = "$_k8_name-admin" ]; then
			_k_sv_in=9001
			_k_sv_what="admin API and console"
		fi
		case $_k_sv_type in
		NodePort | LoadBalancer)
			for _k_node in $_k_st_gw; do
				for _k_a in $(printf '%s\n' "$_k8_addrs" | awk -v n="$_k_node" '$1 == n { print $2 }'); do
					say "$_k_sv_what: https://$_k_a:$_k_sv_port (NodePort, on the machine $_k_node, which runs a ready pod)"
					[ "$_k_sv_in" = 9001 ] || continue
					k8s_probe "$_k_a" "$_k_sv_port" /healthz
					_k_hz=$_k8_probe
					k8s_probe "$_k_a" "$_k_sv_port" /readyz
					say "  /healthz $_k_hz, /readyz $_k8_probe"
					[ "$_k8_probe" != 200 ] || _k_st_answered=1
				done
			done
			[ -n "$_k_st_gw" ] || say "$_k_sv_what: the NodePort $_k_sv_port, on no machine yet: no pod of a gateway is ready"
			for _k_lb in $_k_sv_ip $_k_sv_host; do
				say "$_k_sv_what: https://$_k_lb:$_k_sv_in (load balancer)"
				[ "$_k_sv_in" = 9001 ] || continue
				k8s_probe "$_k_lb" 9001 /healthz
				_k_hz=$_k8_probe
				k8s_probe "$_k_lb" 9001 /readyz
				say "  /healthz $_k_hz, /readyz $_k8_probe"
				[ "$_k8_probe" != 200 ] || _k_st_answered=1
			done
			;;
		*) say "$_k_sv_what: inside the cluster only, https://$_k_sv_name.$_k8_ns.svc:$_k_sv_in ($_k_sv_type)" ;;
		esac
	done <<SERVICES
$_k8_svc
SERVICES
	[ "$_k_st_answered" = 1 ] || k8s_port_forward_said
	docker_cert_expiry "$(k8s_secret_file config/s3.pem TLS_CERT_FILE)"
	if [ "$_k8_mode" = cluster ]; then
		for _k_id in $(k8s_node_ids); do
			if [ -n "$(setting NODE_CERT_DIR)" ]; then
				docker_cert_expiry "$(setting NODE_CERT_DIR)/$_k_id.crt"
			else
				docker_cert_expiry "$STATE/config/nodes/$_k_id.pem"
			fi
		done
	fi
	if [ "$_k_st_ok" = 0 ]; then
		say "not ready: $(k8s_kubectl_said) get pods -n $_k8_ns -l $_k8_sel, then describe pod and logs"
		exit 1
	fi
}

# --- --uninstall and --purge (PLAN 10.11) --------------------------------------------

# k8s_purge_confirm: a purge goes on only with the deployment's name - from
# --purge-confirm (deploy.sh has compared it), or typed at a terminal.
k8s_purge_confirm() {
	[ -z "$PURGE_CONFIRM" ] || return 0
	if [ "$DRY_RUN" = 1 ]; then
		say "dry run: a purge asks for the name of the deployment, $_k8_name, at the terminal (or takes --purge-confirm=$_k8_name)"
		return 0
	fi
	k8s_tty ||
		refuse "--purge deletes the data and the keys of $_k8_name for good: type its name when asked at a terminal, or say --purge-confirm=$_k8_name; nothing was deleted"
	printf 'deploy: --purge deletes the claims, the Secrets and the namespace %s of %s, and its secrets in %s, for good. Type its name to go on: ' "$_k8_ns" "$_k8_name" "$STATE" >&2
	_k_pc_answer=
	IFS= read -r _k_pc_answer || :
	[ "$_k_pc_answer" = "$_k8_name" ] ||
		refuse "that is not the name of the deployment, $_k8_name: nothing was deleted"
}

# k8s_secret_names: _k8_secrets, the deployment's own Secrets in the
# namespace (labelled by k8s_secrets_yaml), as kubectl names them.
k8s_secret_names() {
	_k_sn=$(k8s_cmd query get secrets -n "$_k8_ns" -l "$_k8_secret_sel" -o name </dev/null) ||
		fail "listing the Secrets of $_k8_name in the namespace $_k8_ns failed (kubectl's message is above)"
	_k8_secrets=$(printf '%s\n' "$_k_sn" | printable | sed '/^$/d' | sort | tr '\n' ' ' | sed 's/ *$//')
}

# k8s_claims: _k8_claims, the release's claims in the namespace - with the
# labels of its pods' selector, which the StatefulSet controller gives them
# - by their names, separated by spaces.
k8s_claims() {
	_k_cl=$(k8s_cmd query get persistentvolumeclaims -n "$_k8_ns" -l "$_k8_sel" -o json </dev/null) ||
		fail "listing the claims of $_k8_name in the namespace $_k8_ns failed (kubectl's message is above)"
	_k8_claims=$(printf '%s\n' "$_k_cl" | printable | k8s_flat | k8s_items .metadata.name | sed '/^$/d' | sort | tr '\n' ' ' | sed 's/ *$//')
}

# k8s_uninstall: --uninstall, and with --purge k8s_purge (ADR-0033 sections
# 3 and 7). Everything is asked before anything is removed: the record and
# the confirmation of a purge, the cluster (the one recorded), the namespace
# (the deployment's), a console upgrade's lock, the release's objects and
# Secrets. Then the workload, the Services, the NetworkPolicy and the
# PodDisruptionBudget are deleted; an uninstall deletes the deployment's
# Secrets too and keeps the claims, the namespace and STATE.
k8s_uninstall() {
	k8s_settings
	if [ "$PURGE" = 1 ]; then
		[ "$ADOPT" = 0 ] ||
			refuse "--adopt is not for a purge on Kubernetes: a namespace is purged only with its record in $STATE (ADR-0033 section 7)"
		[ -f "$STATE/target" ] && [ -f "$STATE/namespace-uid" ] && [ -n "$(k8s_ns_recorded)" ] ||
			refuse "$STATE holds no record of $_k8_name (its target, its namespace's name and marker), so nothing says the namespace $_k8_ns of $(engine_identity) is its own: a purge deletes only a deployment recorded there; nothing was deleted"
	fi
	k8s_ns_record_check
	[ "$PURGE" = 0 ] || k8s_purge_confirm
	k8s_connect
	k8s_cluster_check
	k8s_namespace_peek
	case $_k8_ns_state in
	absent)
		say "the cluster of $(engine_identity) has no namespace $_k8_ns"
		if [ "$PURGE" = 1 ]; then
			k8s_images_remove
			k8s_purge_left
			k8s_purge_entry
		else
			say "kept: $STATE: --install makes the deployment again with its secrets"
		fi
		return 0
		;;
	other) refuse "the namespace $_k8_ns is not this deployment's: it does not carry the marker $STATE records (deploy.cactusfs.io/owner-uid); nothing was removed" ;;
	esac
	k8s_lock_check before
	k8s_objects
	k8s_secret_names
	# Before anything is deleted: every Secret about to go has its value in
	# STATE_DIR, or in the file the settings name.
	[ -z "$_k8_secrets" ] || k8s_secrets_verify
	for _k_o in $_k8_others; do
		say "left: $_k_o carries the release's labels and was not applied by the script (no manager cactusfs-deploy with the operation Apply)"
	done
	if [ -n "$_k8_owned" ]; then
		# shellcheck disable=SC2086 # object names, without spaces
		k8s_cmd run delete $_k8_owned -n "$_k8_ns" </dev/null ||
			fail "deleting the workload of $_k8_name failed (kubectl's message is above)"
		say "removed: $_k8_owned"
	else
		say "no workload of $_k8_name in the namespace $_k8_ns that the script applied"
	fi
	if [ "$PURGE" = 1 ]; then
		k8s_purge
		return 0
	fi
	case " $_k8_others " in
	*" statefulset.apps/"*)
		say "kept: the Secrets ${_k8_secrets:-(none)}, as a StatefulSet carrying the release's labels that the script did not apply is left, and its pods may use them"
		;;
	*)
		if [ -n "$_k8_secrets" ]; then
			# shellcheck disable=SC2086 # Secret names, without spaces
			k8s_cmd run delete $_k8_secrets -n "$_k8_ns" </dev/null ||
				fail "deleting the Secrets of $_k8_name failed (kubectl's message is above)"
			say "removed the Secrets $_k8_secrets (their copies in $STATE are kept)"
		fi
		;;
	esac
	k8s_claims
	say "kept: the claims ${_k8_claims:-(none)}, the namespace $_k8_ns and $STATE: --install runs the deployment again with them"
}

# k8s_secrets_verify: before an uninstall or a purge deletes the
# deployment's Secrets, each is read back and compared with its local copy
# (k8s_secrets_read and k8s_secrets_classify, read-only): refused (exit 2,
# nothing deleted) when a Secret of the deployment is not one of those the
# settings name, when a value is in the cluster only, when it differs from
# its local copy (a file given included), or when a file the settings name
# cannot be read - the cluster's copy would be the only one, and it would
# be gone. What neither copy holds is no loss.
k8s_secrets_verify() {
	_k_sv_dir=$(k8s_workdir entry) || exit
	cleanup_add rm -rf -- "$_k_sv_dir"
	k8s_secrets_verify_in "$_k_sv_dir"
	rm -rf -- "$_k_sv_dir"
	cleanup_drop rm -rf -- "$_k_sv_dir"
}

k8s_secrets_verify_in() {
	_k_w=$1
	_k_t=$(printf '\t')
	k8s_secret_list >"$_k_w/list" || fail "listing the Secrets failed"
	for _k_sn in $_k8_secrets; do
		awk -F '\t' '{ print $1 }' "$_k_w/list" | grep -qxF "${_k_sn#secret/}" && continue
		if [ "$PURGE" = 1 ]; then
			# A confirmed purge deletes the deployment and its keys: a
			# Secret of it the settings no longer name goes too.
			warn "the Secret ${_k_sn#secret/} of $_k8_name is not one the settings name (they changed since it was made); the purge deletes it with the rest"
			continue
		fi
		refuse "the namespace $_k8_ns holds the Secret ${_k_sn#secret/} of $_k8_name, which is not one the settings name (it was made under other settings: DEPLOY_MODE, KMS or VAULT_CA_FILE), so its value has no copy the uninstall checks against: it is not deleted, and nothing was removed. Run --uninstall with the settings it was made under, or save it ($(k8s_kubectl_said) get secret ${_k_sn#secret/} -n $_k8_ns -o yaml) and delete it by hand"
	done
	while IFS="$_k_t" read -r _k_secret _k_type _k_key _k_class _k_path; do
		case $_k_path in
		"$STATE"/*) ;;
		*)
			k8s_local_ok "$_k_path" ||
				refuse "$_k_path, which the settings name for $_k_key of the Secret $_k_secret, cannot be read: the Secret would then hold the only copy, and it is not deleted; nothing was removed"
			;;
		esac
	done <"$_k_w/list"
	k8s_secrets_read "$_k_w"
	k8s_secrets_classify "$_k_w"
	if [ -s "$_k_w/back" ]; then
		refuse "$(awk -F '\t' '{ print $3 }' "$_k_w/back" | tr '\n' ' ')is missing from $STATE and held by the cluster's Secrets alone: deleting them would lose it. Run --install, which copies it back, then remove the deployment; nothing was removed"
	fi
	if [ -s "$_k_w/rotate" ]; then
		refuse "the Secrets $(tr '\n' ' ' <"$_k_w/rotate")hold other values than the files the settings name: deleting them would lose the cluster's; run --install (which rotates them) or give the files they were made with; nothing was removed"
	fi
	say "the Secrets of $_k8_name match their copies in $STATE and the files the settings name"
}

# k8s_purge: after the workload is gone, in the order of ADR-0033 section
# 3: the release's claims; a listing that proves none is left, and no
# PersistentVolume bound to one (those a StorageClass with reclaimPolicy
# Retain leaves Released, and under hostpath the host directories, are
# named and left: the script deletes no cluster-scoped object); the
# deployment's Secrets, proved gone the same way; the namespace
# (k8s_ns_delete); the image tags it imported into k3s; and last the
# deployment's entry of STATE_DIR. A step that fails ends the run with the
# entry kept.
k8s_purge() {
	k8s_claims
	_k_pg_claims=$_k8_claims
	if [ -n "$_k_pg_claims" ]; then
		_k_pg_names=
		for _k_c in $_k_pg_claims; do _k_pg_names="$_k_pg_names persistentvolumeclaim/$_k_c"; done
		# shellcheck disable=SC2086 # claim names, without spaces
		k8s_cmd run delete $_k_pg_names -n "$_k8_ns" --wait=false </dev/null ||
			fail "purge: deleting the claims of $_k8_name failed (kubectl's message is above); $STATE is kept"
		say "purge: deleting the claims $_k_pg_claims"
	fi
	if [ "$DRY_RUN" = 0 ]; then
		k8s_purge_proof "$_k_pg_claims"
	fi
	if [ -n "$_k8_secrets" ]; then
		# shellcheck disable=SC2086 # Secret names, without spaces
		k8s_cmd run delete $_k8_secrets -n "$_k8_ns" </dev/null ||
			fail "purge: deleting the Secrets of $_k8_name failed (kubectl's message is above); $STATE is kept"
		say "purge: deleted the Secrets $_k8_secrets"
	fi
	if [ "$DRY_RUN" = 0 ]; then
		k8s_secret_names
		[ -z "$_k8_secrets" ] || fail "purge: the Secrets $_k8_secrets of $_k8_name are still there after they were deleted; $STATE is kept"
		say "purge: no Secret of $_k8_name is left in the namespace $_k8_ns"
	fi
	k8s_in_workdir tmp k8s_ns_delete
	k8s_images_remove
	k8s_purge_left
	k8s_purge_entry
}

# k8s_purge_left: what a purge leaves on purpose, named: under
# K8S_STORAGE=hostpath the StorageClass and the PersistentVolumes it made
# (cluster-scoped: the script deletes none) and the host directories.
k8s_purge_left() {
	[ "$(setting K8S_STORAGE)" = hostpath ] || return 0
	say "purge: left, cluster-scoped and never deleted by the script: the StorageClass $_k8_ns-$_k8_name-local and the PersistentVolumes $_k8_ns-<node ID>-<volume>, and the directories $(setting K8S_HOSTPATH)/$_k8_ns on the node - ciphertext nothing opens once the keys are purged; removing them is the cluster's administrator's"
}

# k8s_purge_proof <claims>: none of the release's claims is left (the
# listing by its labels, asked again every 2 seconds up to WAIT_TIMEOUT,
# while the pods that held them end), and no PersistentVolume bound to one
# of them - each gone, or Released and kept by reclaimPolicy Retain, which
# is named and left. Else the run fails, keeping STATE.
k8s_purge_proof() {
	_k_pp_start=$(date +%s)
	while :; do
		k8s_claims
		[ -n "$_k8_claims" ] || break
		_k_pp_now=$(date +%s)
		[ $((_k_pp_now - _k_pp_start)) -lt "$_k8_wait" ] ||
			fail "purge: the claims $_k8_claims of $_k8_name are still there after WAIT_TIMEOUT=$_k8_wait s; their Secrets and $STATE are kept"
		sleep 2
	done
	say "purge: no claim of $_k8_name is left in the namespace $_k8_ns"
	[ -n "$1" ] || return 0
	_k_pp_kept=
	while :; do
		_k_pv=$(k8s_cmd query get persistentvolumes -o json </dev/null) ||
			fail "purge: listing the PersistentVolumes failed (kubectl's message is above; a context bound to a namespace needs list on persistentvolumes: --print-namespace); the Secrets and $STATE are kept"
		_k_pv=$(printf '%s\n' "$_k_pv" | printable | k8s_flat |
			k8s_items .metadata.name .spec.claimRef.namespace .spec.claimRef.name .status.phase .spec.persistentVolumeReclaimPolicy .spec.local.path .spec.hostPath.path)
		_k_pp_bound=
		_k_pp_kept=
		while IFS='|' read -r _k_pv_name _k_pv_ns _k_pv_claim _k_pv_phase _k_pv_policy _k_pv_local _k_pv_host; do
			[ -n "$_k_pv_name" ] && [ "$_k_pv_ns" = "$_k8_ns" ] || continue
			case " $1 " in
			*" $_k_pv_claim "*) ;;
			*) continue ;;
			esac
			if [ "$_k_pv_policy" = Retain ] && [ "$_k_pv_phase" = Released ]; then
				_k_pp_kept="$_k_pp_kept $_k_pv_name${_k_pv_local:+ ($_k_pv_local on its node)}${_k_pv_host:+ ($_k_pv_host on its node)}"
			else
				_k_pp_bound="$_k_pp_bound $_k_pv_name (${_k_pv_phase:-no phase})"
			fi
		done <<VOLUMES
$_k_pv
VOLUMES
		[ -n "$_k_pp_bound" ] || break
		_k_pp_now=$(date +%s)
		[ $((_k_pp_now - _k_pp_start)) -lt "$_k8_wait" ] ||
			fail "purge: the PersistentVolumes$_k_pp_bound of the claims of $_k8_name are still there after WAIT_TIMEOUT=$_k8_wait s; the Secrets and $STATE are kept"
		sleep 2
	done
	say "purge: no PersistentVolume is bound to a claim of $_k8_name"
	if [ -n "$_k_pp_kept" ]; then
		say "purge: left, as their StorageClass's reclaimPolicy Retain keeps them:$_k_pp_kept - ciphertext that nothing opens once the keys are purged; the script deletes no cluster-scoped object or host directory: removing them is the cluster's administrator's"
	fi
}

# k8s_ns_delete <work dir>: the namespace, deleted only when it carries the
# marker STATE records and is empty when read again - nothing in it but
# what Kubernetes puts in every namespace (the ConfigMap kube-root-ca.crt,
# the ServiceAccount default) - by "delete --raw" with a DeleteOptions body
# whose preconditions name its uid and the resourceVersion it was found
# empty at (ADR-0033 section 7). One that holds anything else is named and
# left; one the context may not list or delete is left to the cluster's
# administrator.
k8s_ns_delete() {
	k8s_namespace_peek
	case $_k8_ns_state in
	absent)
		say "purge: the namespace $_k8_ns is gone already"
		return 0
		;;
	other)
		warn "the namespace $_k8_ns no longer carries the marker $STATE records: it is left"
		return 0
		;;
	esac
	_k_nd_uid=$(printf '%s\n' "$_k_np_flat" | k8s_at .metadata.uid) || _k_nd_uid=
	_k_nd_rv=$(printf '%s\n' "$_k_np_flat" | k8s_at .metadata.resourceVersion) || _k_nd_rv=
	case $_k_nd_uid in
	'' | *[!0123456789abcdef-]*) fail "purge: the namespace $_k8_ns has no uid that can be read; $STATE is kept" ;;
	esac
	case $_k_nd_rv in
	'' | *[!0123456789]*) fail "purge: the namespace $_k8_ns has no resourceVersion that can be read; $STATE is kept" ;;
	esac
	# Every kind the API offers that can be listed in a namespace (events
	# but: Kubernetes writes them of whatever happens), listed in it - reads
	# only. What cannot be listed leaves the namespace to the administrator.
	_k_nd_status=0
	k8s_cmd query api-resources --verbs=list --namespaced -o name </dev/null >"$1/kinds" 2>"$1/err" || _k_nd_status=$?
	printable <"$1/err" >&2
	if [ "$_k_nd_status" = 0 ]; then
		_k_nd_kinds=$(printable <"$1/kinds" | grep -vx -e '' -e events -e events.events.k8s.io | tr '\n' ',' | sed 's/,$//') || _k_nd_kinds=
		[ -n "$_k_nd_kinds" ] || _k_nd_status=1
	fi
	if [ "$_k_nd_status" = 0 ]; then
		plain "$_k_nd_kinds" || _k_nd_status=1
	fi
	if [ "$_k_nd_status" = 0 ]; then
		k8s_cmd query get "$_k_nd_kinds" -n "$_k8_ns" -o name </dev/null >"$1/left" 2>"$1/err" || _k_nd_status=$?
		printable <"$1/err" >&2
	fi
	if [ "$_k_nd_status" != 0 ]; then
		warn "what is left in the namespace $_k8_ns could not all be listed (kubectl's message is above): it is left, without the deployment, to the cluster's administrator"
		return 0
	fi
	_k_own_dir=$1
	k8s_ns_own >"$1/own" || fail "writing $1/own failed"
	_k_nd_left=$(printable <"$1/left" | sed '/^$/d' | grep -vxF -f "$1/own" | grep -v -f "$1/own-prefixes" | tr '\n' ' ' | sed 's/ *$//') || :
	if [ -n "$_k_nd_left" ]; then
		say "purge: the namespace $_k8_ns is left: it holds $_k_nd_left, which is not the deployment's"
		return 0
	fi
	printf '{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"%s","resourceVersion":"%s"}}\n' "$_k_nd_uid" "$_k_nd_rv" >"$1/delete.json" ||
		fail "writing $1/delete.json failed"
	_k_nd_status=0
	k8s_cmd run delete --raw "/api/v1/namespaces/$_k8_ns" -f - <"$1/delete.json" 2>"$1/err" || _k_nd_status=$?
	printable <"$1/err" >&2
	if [ "$_k_nd_status" != 0 ]; then
		if grep -qi forbidden "$1/err"; then
			warn "the context may not delete the namespace $_k8_ns (Forbidden): it is left, empty, to the cluster's administrator"
			return 0
		fi
		fail "purge: deleting the namespace $_k8_ns failed (kubectl's message is above; a precondition that failed means it changed after it was found empty); $STATE is kept"
	fi
	say "purge: deleted the namespace $_k8_ns (uid $_k_nd_uid, resourceVersion $_k_nd_rv: the preconditions guard the Namespace object itself, not what it holds - an object made in it after it was found empty goes with it)"
}

# k8s_ns_own: what may be left in the deployment's namespace when it is
# removed, one a line as "kubectl get -o name" prints it - what Kubernetes
# puts in every namespace (the ConfigMap kube-root-ca.crt, the
# ServiceAccount default) and the deployment's own objects being deleted
# (its pods, Services, the endpoints Kubernetes made of them); and into
# <work dir>/own-prefixes the beginnings of the names Kubernetes gives what
# it makes of them (EndpointSlices, ControllerRevisions), as grep patterns.
k8s_ns_own() {
	printf '%s\n' configmap/kube-root-ca.crt serviceaccount/default
	for _k_no_id in $(k8s_node_ids); do
		printf '%s\n' "pod/$_k_no_id-0" "statefulset.apps/$_k_no_id"
	done
	for _k_sv in "$_k8_name" "$_k8_name-admin" "$_k8_name-nodes"; do
		printf '%s\n' "service/$_k_sv" "endpoints/$_k_sv"
	done
	{
		for _k_sv in "$_k8_name" "$_k8_name-admin" "$_k8_name-nodes"; do
			printf '^endpointslice\\.discovery\\.k8s\\.io/%s-[a-z0-9]*$\n' "$_k_sv"
		done
		for _k_no_id in $(k8s_node_ids); do
			printf '^controllerrevision\\.apps/%s-[a-z0-9]*$\n' "$_k_no_id"
		done
	} >"$_k_own_dir/own-prefixes"
}

# k8s_images_remove: the image tags the script's import made in k3s (the
# entry's file "images": only tags containerd lacked before the import) -
# removed from its containerd over K3S_SSH or here, but a tag that a pod of
# any namespace still names (its containers' image, or the image and
# imageID its status reports), or the pod template of a StatefulSet,
# Deployment, DaemonSet, Job or CronJob of any namespace, which is named
# and kept; K3S_IMPORT's node cannot be asked, so its tags are left. Nothing else is removed. A removal
# that fails is said and the purge goes on: a tag holds no secret.
k8s_images_remove() {
	[ -f "$STATE/images" ] || return 0
	if [ -n "$(setting K3S_IMPORT)" ]; then
		say "purge: the images in $STATE/images were imported by K3S_IMPORT's command, on a node the script cannot ask: they are left"
		return 0
	fi
	# A tag a pod, or the pod template of a StatefulSet, Deployment,
	# DaemonSet, Job or CronJob, of any namespace still names is kept: its
	# image is in use, or will be. What cannot be listed keeps every tag.
	_k_used=
	for _k_ri_kinds in pods statefulsets,deployments,daemonsets,jobs,cronjobs; do
		_k_ri_status=0
		_k_ri=$(k8s_cmd query get "$_k_ri_kinds" -A -o json </dev/null) || _k_ri_status=$?
		if [ "$_k_ri_status" != 0 ]; then
			warn "the $_k_ri_kinds of every namespace could not be listed (kubectl's message is above): the images in $STATE/images are kept"
			return 0
		fi
		_k_used="$_k_used$NL$(printf '%s\n' "$_k_ri" | printable | k8s_flat | awk -F '\t' '
			$1 ~ /[Cc]ontainers\[[0-9]+\]\.image$/ || $1 ~ /[Cc]ontainerStatuses\[[0-9]+\]\.(image|imageID)$/ { print substr($0, length($1) + 2) }' | sort -u)"
	done
	_k_used_names=
	for _k_u in $_k_used; do
		_k_used_names="$_k_used_names$(k8s_containerd_name "$_k_u")$NL"
	done
	while IFS= read -r _k_im; do
		[ -n "$_k_im" ] || continue
		if ! plain "$_k_im"; then
			warn "$STATE/images holds a name that is not one, which is left: $(printf '%s' "$_k_im" | printable)"
			continue
		fi
		if printf '%s\n' "$_k_used_names" | grep -qxF "$_k_im"; then
			say "purge: the image $_k_im is kept: a pod or a workload's template still names it"
			continue
		fi
		_k_rm_status=0
		k8s_ctr run images rm "$_k_im" </dev/null || _k_rm_status=$?
		case $_k_rm_status in
		0) say "purge: removed the image $_k_im it imported" ;;
		3) say "purge: the image $_k_im it imported is left" ;;
		*) warn "removing the image $_k_im from k3s failed (its message is above); it is left" ;;
		esac
	done <"$STATE/images"
}

# k8s_purge_entry: last, the deployment's entry of STATE_DIR, and nothing
# else of it (its target's directory only when that is left empty).
k8s_purge_entry() {
	if [ -d "$STATE" ]; then
		case $STATE in
		*/"$_k8_name") ;;
		*) fail "internal: $STATE is not a deployment's entry" ;;
		esac
		run rm -rf -- "$STATE"
		if [ "$DRY_RUN" = 0 ]; then
			rmdir "${STATE%/*}" 2>/dev/null || :
		fi
		say "purge: deleted $STATE"
	fi
	say "purged: $_k8_name and its secrets are gone from $(engine_identity) and from this machine"
}

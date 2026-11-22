# shellcheck shell=sh
# Kubernetes (k3s, k8s, Rancher Desktop's Kubernetes): the values the chart
# is rendered with, the render, the deployment's namespace and its Secrets
# (PLAN 10.9, ADR-0033 sections 6 to 8). Sourced by deploy/deploy.sh; not
# run on its own. Installing, upgrading and removing are PLAN 10.10 and 10.11.
#
# What the script writes on a cluster is namespaced, in its own namespace:
# the namespace itself is made by a create (never an apply) whose object
# already carries the marker - a random UID, kept in the deployment's
# STATE_DIR entry as well - and the restricted Pod Security label. Outside
# the namespace it only reads: the Nodes and the StorageClasses (listed) here,
# the Services of every namespace and kube-system (PLAN 10.3, 10.10).
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

# k8s_values <file> <nodes> <first node> <image>: writes the chart's values
# for this deployment into the file, from the settings, the number of the
# cluster's nodes and the name of the first (for K8S_STORAGE=hostpath), and
# the image the pods run (a reference with a tag or a digest; PLAN 10.4
# delivers it). The values name Secrets and hold none.
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
		printf 'image:\n  repository: %s\n  tag: %s\n  digest: %s\n' "$(k8s_q "$_k_repo")" "$(k8s_q "$_k_tag")" "$(k8s_q "$_k_digest")"
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
	if [ "$(setting K8S_STORAGE)" = hostpath ]; then
		_k_volumes=$(k8s_node_ids | wc -l | tr -d ' ')
		if [ "$_k_dmode" = cluster ]; then
			_k_volumes=$((_k_volumes * 3))
		else
			_k_volumes=$((_k_volumes * ($(setting DRIVES) + 1)))
		fi
		say "K8S_STORAGE=hostpath makes cluster-scoped objects: the StorageClass $_k_namespace-$_k_name-local and $_k_volumes PersistentVolumes, directories under $(setting K8S_HOSTPATH)/$_k_namespace on the node $_k_first - not for a shared cluster (ADR-0033 section 7)"
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
	# The path is put into a trap between single quotes: one holding a quote
	# would end them.
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
# when the subshell ends however it ends (a trap, signals included). The
# subshell's status is the run's: a refusal or failure in it ends the script
# with that status. "set -e" does not hold in such a subshell (it runs on
# the left of "||"): every step of the function checks its own status.
k8s_in_workdir() {
	_k_iw_where=$1
	shift
	_k_iw_dir=$(k8s_workdir "$_k_iw_where") || exit
	_k_iw_status=0
	(
		# Expanded now: the function run in here may assign any variable,
		# and the trap must remove this directory and no other.
		# shellcheck disable=SC2064
		trap "rm -rf '$_k_iw_dir'" EXIT
		trap 'exit 129' HUP
		trap 'exit 130' INT
		trap 'exit 143' TERM
		"$@" "$_k_iw_dir"
	) || _k_iw_status=$?
	rm -rf "$_k_iw_dir"
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
		k8s_engine run pull "$_k_image" </dev/null || fail "pulling $_k_image failed"
		if [ "$DRY_RUN" = 1 ]; then
			say "dry run: the chart is rendered once $_k_image is pulled"
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
		k8s_state_put namespace-uid "$_k_uid"
	fi
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

# k8s_rbac_yaml: what a context bound to the namespace needs for the script
# (ADR-0033 section 7): a Role in the namespace for the objects it makes and
# reads there, and a ClusterRole for its reads outside - listing Nodes,
# StorageClasses, Services and PersistentVolumes (the purge's check of PLAN
# 10.11), getting its own namespace and kube-system by name. Bindings name the operator's identity, which only the administrator
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
	printf '  - apiGroups: [""]\n    resources: [nodes, services, persistentvolumes]\n    verbs: [list]\n'
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

# k8s_namespace_ensure: runs k8s_namespace_ensure_in in a command
# substitution with a work directory removed however it ends; it prints
# "absent", "ours" or "adopt:<marker>" and refuses everything else. What is
# written - the marker, the namespace - is written here, outside it, where
# a failure ends the run.
k8s_namespace_ensure() {
	_k_ne_dir=$(k8s_workdir tmp) || exit
	_k_ne_status=0
	_k_ne_out=$(
		# Expanded now, as in k8s_in_workdir.
		# shellcheck disable=SC2064
		trap "rm -rf '$_k_ne_dir'" EXIT
		trap 'exit 129' HUP
		trap 'exit 130' INT
		trap 'exit 143' TERM
		k8s_namespace_ensure_in "$_k_ne_dir"
	) || _k_ne_status=$?
	rm -rf "$_k_ne_dir"
	[ "$_k_ne_status" = 0 ] || exit "$_k_ne_status"
	case $_k_ne_out in
	ours) ;;
	absent)
		k8s_uid
		say "the namespace $(k8s_ns) is made, marked $MARKER_ANNOTATION=$_k_uid"
		_k_ne_dir=$(k8s_workdir tmp) || exit
		if ! k8s_namespace_yaml "$_k_uid" >"$_k_ne_dir/namespace.yaml"; then
			rm -rf "$_k_ne_dir"
			fail "writing the namespace's manifest failed"
		fi
		_k_ne_status=0
		k8s_cmd run create -f - <"$_k_ne_dir/namespace.yaml" 2>"$_k_ne_dir/err" || _k_ne_status=$?
		printable <"$_k_ne_dir/err" >&2
		if [ "$_k_ne_status" != 0 ] && grep -qi forbidden "$_k_ne_dir/err"; then
			rm -rf "$_k_ne_dir"
			fail "the context may not create the namespace $(k8s_ns) (Forbidden): have the cluster's administrator create it from the manifests deploy/deploy.sh --print-namespace prints (ADR-0033 section 7)"
		fi
		rm -rf "$_k_ne_dir"
		[ "$_k_ne_status" = 0 ] || fail "creating the namespace $(k8s_ns) failed (one of that name made meanwhile is not taken over)"
		;;
	adopt:*)
		k8s_marker_ok "${_k_ne_out#adopt:}" || fail "internal: adopted the marker '${_k_ne_out#adopt:}'"
		k8s_state_put namespace-uid "${_k_ne_out#adopt:}"
		;;
	*) fail "internal: the namespace check said '$_k_ne_out'" ;;
	esac
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
		secrets_ensure_k8s "$1" || fail "making the secrets that are missing failed"
		return 0
	fi
	not_built "10.3 for Kubernetes" "the secrets of $(setting DEPLOY_NAME) are in neither $STATE nor the namespace $(k8s_ns): $(tr '\n' ';' <"$1")making them on Kubernetes is PLAN 10.3's"
}

# k8s_secrets_ensure: the deployment's Secrets, as the table above says - the
# Kubernetes read-back and put that 10.3's secrets_install calls. Nothing is
# changed before every Secret has been read back and compared; a read that
# fails, a Secret that is not the deployment's, a local copy that is empty
# or unreadable, or a value that differs stops the run with nothing changed.
k8s_secrets_ensure() {
	k8s_in_workdir entry k8s_secrets_ensure_in
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

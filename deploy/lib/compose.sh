# shellcheck shell=sh
# A cluster on a Docker engine - Docker Desktop (dd), Docker CE here or on
# CE_HOST (ce), Podman (podman) and Rancher Desktop's moby (rancher, PLAN
# 10.6): DEPLOY_MODE=cluster, through deploy/compose/compose.yaml itself
# (PLAN 10.8; ADR-0033 sections 3, 4, 9, 10, 11). Sourced by deploy/deploy.sh; not run on its own.
#
# What a deployment named <name> is - the compose project <name>, every
# container, volume and network of it labelled io.cactusfs.deploy.name=<name>
# and io.cactusfs.deploy.target=<target> by the override, and nothing else
# touched:
#
#   init, node1 to node4   compose.yaml's services: three voters and node4,
#                          which stores and serves but does not vote
#   <name>_nodeN-meta, <name>_nodeN-data1, <name>_nodeN-data2
#                          their volumes, made by compose
#   <name>-config          dev-init's directory (kms.json, the certificates,
#                          ready), a volume of PLAN 10.3 that the override
#                          names as compose's "config" volume: init finds it
#                          complete and writes nothing
#
# and in the deployment's entry of STATE_DIR (on CE_HOST, in CE_STATE_DIR of
# the server: the same layout), compose/:
#
#   compose.yaml           a copy of deploy/compose/compose.yaml, unchanged
#   override.yaml          what this file writes: the image (pull_policy
#                          never), the labels, the init service's --hosts
#                          and --validity, the drives' labels, the restart
#                          policy, the console's upgrade off, the config
#                          volume, Vault, and the env file's CACTUSFS_*
#                          lines compose.yaml does not interpolate
#   compose.env            every value compose.yaml interpolates, handed to
#                          compose with --env-file: compose runs under
#                          "env -i", so nothing of the operator's shell is
#                          read (ADR-0033 section 10)
#   secrets/               compose's file secrets - the root credentials,
#                          the passphrase or the Vault token - the one bind
#                          mount of the script (ADR-0033 section 10): the
#                          directory 0700, the files 0444, kept by
#                          secrets.sh as the target's copy of the secrets
#   s3-ca.pem              on CE_HOST, the CA of the S3 certificate, for
#                          the server's curl to ask /readyz
#
# compose.yaml cannot be told to drop its build: or its ports by an
# override, so compose runs "up --no-build", S3 is published on every
# address at <base>00, <base>10, <base>20 and <base>30 (S3_PORT=<base>00),
# the admin ports on 127.0.0.1 at <base>01 to <base>31, and BIND, ADMIN_BIND
# and DRIVES are refused when they are not their defaults.
#
# Install: everything that can refuse is asked first - the engine on this
# machine (compose bind-mounts files of STATE_DIR; a remote engine only
# through CE_HOST), compose there, containers and volumes of the names
# without the labels, the ports - then the secrets (secrets_install: the
# config volume and the secret files, before anything starts), the image,
# the compose files, a relabel under enforcing SELinux, and "up". Again on a
# deployment that runs: an upgrade, one service at a time - node4, the
# voters, the leader last when the admin API names it - each ready on
# /readyz before the next, stopping at the first that is not.
#
# A function that can refuse or fail is never run inside $(...): those that
# answer set variables (_cp_*) and are called plainly.

# compose_cluster: true for a deployment this file builds - cluster mode on
# Docker Desktop, Docker CE (here or on CE_HOST), Podman, or Rancher
# Desktop's moby engine through "docker --context rancher-desktop" (PLAN
# 10.6; its containerd engine is refused by rancher_resolve, before
# anything is asked: nerdctl's compose cannot upgrade one node at a time).
# Kubernetes is 10.10's.
compose_cluster() {
	on_kubernetes && return 1
	[ "$(setting DEPLOY_MODE)" = cluster ] || return 1
	case $TARGET in
	dd | ce | podman) return 0 ;;
	rancher) [ "$(rancher_engine)" = moby ] ;;
	*) return 1 ;;
	esac
}

# The services of compose.yaml that are nodes; node4 does not vote.
COMPOSE_NODES='node1 node2 node3 node4'

# cluster_ports: in cluster mode on a Docker engine the ports are those
# deploy/compose/compose.yaml publishes, node n on <base><n-1>0 (S3) and
# <base><n-1>1 (admin, on 127.0.0.1), with one base, CACTUSFS_PORT_BASE_1
# to _4, for the four nodes: S3_PORT is node 1's S3 port, so a whole number
# of hundreds, and ADMIN_PORT the port after it. The admin address and the
# drives are compose.yaml's too, so ADMIN_BIND and DRIVES stay at their
# defaults, as BIND does (env.sh): an override adds ports, never replaces
# them.
cluster_ports() {
	_cp_s3=$(setting S3_PORT)
	case $_cp_s3 in
	*00) ;;
	*) bad S3_PORT "must end in 00 in cluster mode: compose.yaml publishes node 1 on <base>00, node 2 on <base>10, node 3 on <base>20 and node 4 on <base>30 (S3_PORT=9000 gives the base 90)" ;;
	esac
	[ "$_cp_s3" -le 65500 ] || bad S3_PORT "must be at most 65500 in cluster mode: node 4's admin port is S3_PORT + 31"
	[ "$(setting ADMIN_PORT)" = $((_cp_s3 + 1)) ] ||
		bad ADMIN_PORT "must be S3_PORT + 1 ($((_cp_s3 + 1))) in cluster mode: compose.yaml publishes the admin port of each node beside its S3 port"
	[ "$(lookup "$(env_defaults)" ADMIN_BIND)" = "$(setting ADMIN_BIND)" ] ||
		bad ADMIN_BIND "cannot be set in cluster mode: compose.yaml publishes the admin ports on 127.0.0.1 only (the console user admin of ADR-0014)"
	[ "$(lookup "$(env_defaults)" DRIVES)" = "$(setting DRIVES)" ] ||
		bad DRIVES "cannot be set in cluster mode: compose.yaml gives each of its four nodes two drives"
	_cp_base=$((_cp_s3 / 100))
}

# compose_s3_port <n> and compose_admin_port <n>: the ports of node n on
# the engine's machine.
compose_s3_port() {
	printf '%s' $((_cp_base * 100 + ($1 - 1) * 10))
}
compose_admin_port() {
	printf '%s' $((_cp_base * 100 + ($1 - 1) * 10 + 1))
}

# compose_settings: the settings this file reads, read once, with those of
# secrets.sh, and the compose directory - this machine's in the entry, or
# CE_STATE_DIR/<target-id>/<name>/compose on CE_HOST, whose host key is
# checked first - against the keys pinned in STATE_DIR, or at a first
# contact against known_hosts, whose lines the install pins (ce_begin,
# deploy/lib/ce.sh, PLAN 10.7) - and whose home directory is asked next.
compose_settings() {
	_cp_name=$(setting DEPLOY_NAME)
	_cp_ce_host=
	[ "$TARGET" != ce ] || _cp_ce_host=$(setting CE_HOST)
	_cp_wait=$(setting WAIT_TIMEOUT)
	_cp_local=$STATE/compose
	# The PATH compose and curl are run with on CE_HOST, under env -i.
	_cp_rpath=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
	_cp_rhome=
	cluster_ports
	if [ -n "$_cp_ce_host" ]; then
		# What ce.sh reads: the server, and for CE_INSTALL_DOCKER the ports
		# of node 1 (compose_ports_check asks all eight once the engine is
		# there). secrets_settings sets _s_ce_host again below.
		_s_ce_host=$_cp_ce_host
		_dk_bind=0.0.0.0
		_dk_s3_port=$_cp_s3
		_dk_admin_bind=127.0.0.1
		_dk_admin_port=$((_cp_s3 + 1))
		ce_begin connect
		compose_remote_home
		case $(setting CE_STATE_DIR) in
		/*) _cp_rbase=$(setting CE_STATE_DIR) ;;
		*) _cp_rbase=$_cp_rhome/$(setting CE_STATE_DIR) ;;
		esac
		_cp_dir=$_cp_rbase/$(target_id)/$_cp_name/compose
		plain "$_cp_dir" || refuse "$_cp_dir, the deployment's directory on $_cp_ce_host, is not a path this script hands to ssh: set CE_STATE_DIR to one of letters, digits and _./=:@+,-"
		# The record names the server's compose directory (state_lines):
		# another CE_STATE_DIR would leave the deployment's files - its
		# secrets - where no later run looks.
		if [ -f "$STATE/target" ]; then
			_cs_rec=$(sed -n 's/^compose-dir=//p' "$STATE/target" | head -n 1 | printable)
			[ -z "$_cs_rec" ] || [ "$_cs_rec" = "$_cp_dir" ] ||
				refuse "$STATE was made with its compose files in $_cs_rec on $_cp_ce_host, and CE_STATE_DIR now puts them in $_cp_dir: set CE_STATE_DIR back"
		fi
	else
		_cp_rbase=
		_cp_dir=$_cp_local
	fi
	secrets_settings
	# What docker.sh's /readyz and messages read.
	_dk_tls_hosts=$(setting TLS_HOSTS)
	_dk_admin_bind=127.0.0.1
	_dk_wait=$_cp_wait
}

# compose_remote_home: _cp_rhome, the home directory of the ssh user on
# CE_HOST - where CE_STATE_DIR is when it is relative, and the HOME compose
# runs with there.
compose_remote_home() {
	_rh=$(query on_host "$_cp_ce_host" printenv HOME </dev/null) ||
		fail "could not ask $_cp_ce_host for its home directory (ssh's message is above)"
	_cp_rhome=$(printf '%s' "$_rh" | printable | tr -d '\n')
	case $_cp_rhome in
	/?*) ;;
	*) fail "$_cp_ce_host did not say an absolute home directory: '$_cp_rhome'" ;;
	esac
	plain "$_cp_rhome" || refuse "the home directory of the ssh user on $_cp_ce_host, $_cp_rhome, is not a path this script hands to ssh"
}

# compose_cmd <run|query> <word>...: compose of the target - the server's
# "docker compose" over ssh for CE_HOST, under "env -i" there with only a
# PATH and the user's HOME; else this machine's "docker --context <context>
# compose" or "podman compose", which tool runs under "env -i" with PATH,
# HOME and what the engine needs (XDG_RUNTIME_DIR for rootless Podman).
# Nothing of the operator's shell is read by compose: its values come from
# the --env-file of compose_project.
compose_cmd() {
	_cc_how=$1
	shift
	if [ -n "$_cp_ce_host" ]; then
		"$_cc_how" on_host "$_cp_ce_host" env -i "PATH=$_cp_rpath" "HOME=$_cp_rhome" docker compose "$@"
	elif [ "$TARGET" = podman ]; then
		"$_cc_how" podman compose "$@"
	else
		"$_cc_how" docker --context "$_s_context" compose "$@"
	fi
}

# compose_project <run|query> <word>...: compose_cmd for the deployment's
# project, from the files of its compose directory.
compose_project() {
	_cpj_how=$1
	shift
	compose_cmd "$_cpj_how" --project-name "$_cp_name" -f "$_cp_dir/compose.yaml" -f "$_cp_dir/override.yaml" \
		--env-file "$_cp_dir/compose.env" "$@"
}

# compose_lf <key>: the Go template of a container's label in a listing -
# the Docker CLI's and Podman's differ.
compose_lf() {
	if [ "$TARGET" = podman ]; then
		printf '{{index .Labels "%s"}}' "$1"
	else
		printf '{{.Label "%s"}}' "$1"
	fi
}

# compose_refusals: what cluster mode on a Docker engine does not carry to
# the nodes yet - the operator's node certificates and a CA for Vault - is
# refused rather than left out; and the env file's CACTUSFS_* values that go
# into compose.env must mean nothing but themselves to compose's reader of
# env files (no quote, #, space or backslash).
compose_refusals() {
	for _cr_key in NODE_CA_FILE NODE_CERT_DIR; do
		[ -z "$(setting "$_cr_key")" ] ||
			refuse "$_cr_key is not put into the config volume of a compose cluster yet: its nodes use the node certificates dev-init makes; leave it empty"
	done
	if [ "$_s_kms" = vault ] && [ -n "$(setting VAULT_CA_FILE)" ]; then
		refuse "VAULT_CA_FILE is not put into the config volume of a Docker target yet, so the nodes would not trust that Vault; leave it empty when Vault's certificate is issued by a public CA (the image trusts those)"
	fi
	_cr_vars=$(compose_vars)
	_cr_fixed=$(compose_fixed_keys)
	while IFS= read -r _cr_line; do
		[ -n "$_cr_line" ] || continue
		case ${_cr_line%%=*} in
		CACTUSFS_TOPOLOGY_*) refuse "${_cr_line%%=*} is a setting of each node, and compose.yaml places its four nodes itself (CACTUSFS_TOPOLOGY_ZONE zone-a and zone-b): a line of the env file would give every node the same; leave it out in cluster mode" ;;
		esac
		! printf '%s\n' "$_cr_fixed" | grep -qx "${_cr_line%%=*}" ||
			refuse "${_cr_line%%=*} is set by compose.yaml for each node; leave it out of the env file in cluster mode"
		printf '%s\n' "$_cr_vars" | grep -qx "${_cr_line%%=*}" || continue
		case ${_cr_line#*=} in
		*[!A-Za-z0-9_./:@+,=%-]*) refuse "the value of ${_cr_line%%=*} goes to compose in an env file, and holds a character other than letters, digits and _./:@+,=%-, which compose's reader of env files may read as quoting or a comment" ;;
		esac
	done <<EOF
$PASSED
EOF
}

# compose_fixed_keys: the settings compose.yaml gives the nodes itself
# (values, not ${...}), one a line.
compose_fixed_keys() {
	sed 's/#.*//' "$REPO_ROOT/deploy/compose/compose.yaml" | grep -E '^ +CACTUSFS_[A-Z0-9_]+: ' | grep -v '[$][{]' |
		sed 's/^ *\([A-Z0-9_]*\):.*/\1/' | sort -u
}

# compose_vars: the variables compose.yaml interpolates, one a line - read
# from the file itself, so that compose.env holds every one of them.
compose_vars() {
	sed 's/#.*//' "$REPO_ROOT/deploy/compose/compose.yaml" | grep -o '\${[A-Z][A-Z0-9_]*' | sed 's/^\${//' | sort -u
}

# compose_engine_local: compose bind-mounts the secret files of STATE_DIR,
# so the engine must be on this machine (or a VM that shares its files):
# a Docker context whose endpoint is not a local socket, or a Podman that
# is a remote service on Linux, is refused - a server goes through CE_HOST,
# where compose runs on the server itself.
compose_engine_local() {
	[ -z "$_cp_ce_host" ] || return 0
	if [ "$TARGET" = podman ]; then
		_el=$(target_engine query info --format '{{.Host.ServiceIsRemote}}' </dev/null) ||
			fail "could not ask $(engine) where it runs (its message is above)"
		_el=$(printf '%s' "$_el" | printable | tr -d '\n')
		[ "$_el" = true ] || return 0
		_el_why="cluster mode bind-mounts files of STATE_DIR, and this Podman is a remote service"
		[ "$(uname -s | printable)" = Darwin ] ||
			refuse "$_el_why: the files are on this machine, not its; run the script on that machine, or use DEPLOY_TARGET=ce with CE_HOST"
		# On macOS every Podman is a service in a VM: only one of this
		# machine (a Podman machine, whose connection is on the loopback
		# address) shares its files.
		_el=$(target_engine query system connection list --format '{{.Default}} {{.URI}}' </dev/null) ||
			fail "could not ask Podman for its connections (its message is above)"
		_el=$(printf '%s\n' "$_el" | printable | sed -n 's/^true //p' | head -n 1)
		case $_el in
		ssh://127.0.0.1:* | ssh://localhost:* | ssh://*@127.0.0.1:* | ssh://*@localhost:* | unix://*) ;;
		*) refuse "$_el_why whose default connection, '$_el', is not a Podman machine of this machine (127.0.0.1 or localhost): the files are on this machine, not its; use DEPLOY_TARGET=ce with CE_HOST for a server" ;;
		esac
		return 0
	fi
	_el=$(target_engine query context inspect --format '{{.Endpoints.docker.Host}}' "$_s_context" </dev/null) ||
		fail "could not ask the Docker CLI where the context $_s_context points (its message is above)"
	_el=$(printf '%s' "$_el" | printable | tr -d '\n')
	case $_el in
	unix://* | npipe://*) ;;
	*) refuse "cluster mode bind-mounts files of STATE_DIR, and the context $_s_context reaches the engine at '$_el', not on this machine: a DOCKER_HOST engine elsewhere is refused for cluster mode; for a server, use DEPLOY_TARGET=ce with CE_HOST, where compose runs on the server" ;;
	esac
}

# compose_check_tool: compose is there - the server's for CE_HOST - and
# reaches the engine itself, before anything changes: "compose version"
# never asks the engine, so "compose ls", which lists the engine's compose
# projects, is asked too. podman compose with docker-compose as its
# provider reaches Podman through its API socket (podman.socket), which a
# Podman of its own does not need: without it "up" would fail only after the
# secrets were put. podman-compose, which has no "ls", talks to podman
# itself, which the run has asked already.
compose_check_tool() {
	if ! _ct_ver=$(compose_cmd query version </dev/null); then
		if [ -n "$_cp_ce_host" ]; then
			refuse "docker compose does not answer on $_cp_ce_host (its message is above): install the compose plugin of Docker CE there"
		elif [ "$TARGET" = podman ]; then
			refuse "podman compose does not answer (its message is above): install docker-compose or podman-compose, which podman compose runs"
		fi
		refuse "docker compose does not answer (its message is above): install the compose plugin"
	fi
	if [ "$TARGET" = podman ]; then
		case $(printf '%s\n' "$_ct_ver" | printable) in
		*podman-compose*) return 0 ;;
		esac
	fi
	compose_cmd query ls </dev/null >/dev/null && return 0
	if [ -n "$_cp_ce_host" ]; then
		refuse "docker compose on $_cp_ce_host cannot reach its engine (compose ls failed; its message is above): start Docker there (systemctl enable --now docker), and make the docker the ssh user runs reach it; nothing was changed"
	elif [ "$TARGET" = podman ]; then
		refuse "podman compose cannot reach Podman (compose ls failed; its message is above): its provider, docker-compose, talks to Podman's API socket, which is not running - start it with systemctl --user enable --now podman.socket (as root: systemctl enable --now podman.socket; on macOS the Podman machine serves it: podman machine start); nothing was changed"
	fi
	refuse "docker compose cannot reach the engine of the context $_s_context (compose ls failed; its message is above): start Docker; nothing was changed"
}

# compose_containers: what the engine holds of the deployment's containers,
# by its label - _cp_ctrs ("name|service|state|image" a line), _cp_svcs (the
# nodes among them) and _cp_helpers (the helpers a run of secrets.sh left).
# One of another target, or that compose did not make (a standalone node
# of the name), is refused; so is a container of a compose project of the
# deployment's name that does not carry its label - compose would take it
# over.
compose_containers() {
	_cp_ctrs=
	_cp_svcs=
	_cp_helpers=
	_cc_out=$(target_engine query ps -a --filter "label=$LABEL_NAME=$_cp_name" \
		--format "{{.Names}}|$(compose_lf "$LABEL_TARGET")|$(compose_lf com.docker.compose.service)|{{.State}}|{{.Image}}" </dev/null) ||
		fail "could not list the containers of $(engine) (its message is above)"
	_cc_out=$(printf '%s\n' "$_cc_out" | printable)
	while IFS='|' read -r _cc_n _cc_t _cc_s _cc_state _cc_img; do
		[ -n "$_cc_n" ] || continue
		[ "$_cc_t" = "$TARGET" ] ||
			refuse "the container $_cc_n is the deployment's on the target '$_cc_t', not $TARGET: run with --docker $_cc_t, or choose another DEPLOY_NAME"
		case $_cc_s in
		init) ;;
		node1 | node2 | node3 | node4) _cp_svcs="$_cp_svcs $_cc_s" ;;
		'')
			case $_cc_n in
			"$_cp_name-volumes" | "$_cp_name-dev-init")
				_cp_helpers="$_cp_helpers $_cc_n"
				continue
				;;
			esac
			refuse "the container $_cc_n carries the deployment's name and was not made by compose (a standalone node of $_cp_name?): the script does not turn a deployment into the other mode; remove it with DEPLOY_MODE=standalone, or choose another DEPLOY_NAME"
			;;
		*) refuse "the container $_cc_n is the service '$_cc_s', which compose.yaml has not: it is not this deployment's; remove it, or choose another DEPLOY_NAME" ;;
		esac
		_cp_ctrs="$_cp_ctrs$_cc_n|$_cc_s|$_cc_state|$_cc_img
"
	done <<EOF
$_cc_out
EOF
	_cc_out=$(target_engine query ps -a --filter "label=com.docker.compose.project=$_cp_name" --format "{{.Names}}|$(compose_lf "$LABEL_NAME")" </dev/null) ||
		fail "could not list the containers of $(engine) (its message is above)"
	_cc_out=$(printf '%s\n' "$_cc_out" | printable)
	while IFS='|' read -r _cc_n _cc_o; do
		[ -n "$_cc_n" ] || continue
		[ "$_cc_o" = "$_cp_name" ] ||
			refuse "the container $_cc_n belongs to a compose project named $_cp_name that the script did not make (it has no label $LABEL_NAME=$_cp_name): compose would take it over; remove that project, or choose another DEPLOY_NAME"
	done <<EOF
$_cc_out
EOF
}

# compose_volume_names: the volumes compose makes for the deployment, one a
# line: <name>_nodeN-meta, -data1 and -data2.
compose_volume_names() {
	for _cvn in $COMPOSE_NODES; do
		printf '%s\n' "${_cp_name}_$_cvn-meta" "${_cp_name}_$_cvn-data1" "${_cp_name}_$_cvn-data2"
	done
}

# compose_volumes: the deployment's volumes the engine holds - _cp_vols
# (compose's, by their names) and _cp_cfg (1 when <name>-config is there) -
# by one listing by label and one inspect of their targets, then one listing
# by name that finds a volume of compose's names without the label. Each
# listing and inspect must succeed. A volume of the name of another target,
# or not of a compose cluster (a standalone deployment's), is refused, as is
# one of compose's names without the label.
compose_volumes() {
	_cp_vols=
	_cp_cfg=
	_cv_out=$(target_engine query volume ls --filter "label=$LABEL_NAME=$_cp_name" --format '{{.Name}}' </dev/null) ||
		fail "could not list the volumes of $(engine) (its message is above)"
	_cv_names=$(printf '%s\n' "$_cv_out" | printable | sed '/^$/d' | sort)
	_cv_expected=$(compose_volume_names)
	if [ -n "$_cv_names" ]; then
		# shellcheck disable=SC2086 # volume names, without spaces
		_cv_out=$(target_engine query volume inspect --format "{{.Name}} {{index .Labels \"$LABEL_TARGET\"}}" $_cv_names </dev/null) ||
			fail "could not inspect the volumes of the deployment (the engine's message is above)"
		_cv_out=$(printf '%s\n' "$_cv_out" | printable)
		for _cv in $_cv_names; do
			_cv_target=$(printf '%s\n' "$_cv_out" | sed -n "s|^$_cv ||p" | head -n 1)
			[ "$_cv_target" = "$TARGET" ] ||
				refuse "the volume $_cv is the deployment's on the target '$_cv_target', not $TARGET: run with --docker $_cv_target, or choose another DEPLOY_NAME"
			if [ "$_cv" = "$_cp_name-config" ]; then
				_cp_cfg=1
			elif printf '%s\n' "$_cv_expected" | grep -qxF "$_cv"; then
				_cp_vols="$_cp_vols $_cv"
			else
				refuse "the volume $_cv carries the deployment's name and is not one of its compose cluster's (a standalone deployment's?): the script does not turn a deployment into the other mode; purge it with DEPLOY_MODE=standalone, or choose another DEPLOY_NAME"
			fi
		done
	fi
	_cv_out=$(target_engine query volume ls --filter "name=${_cp_name}_" --format '{{.Name}}' </dev/null) ||
		fail "could not list the volumes of $(engine) (its message is above)"
	for _cv in $(printf '%s\n' "$_cv_out" | printable); do
		printf '%s\n' "$_cv_expected" | grep -qxF "$_cv" || continue
		case " $_cp_vols " in
		*" $_cv "*) ;;
		*) refuse "a volume named $_cv is there without the label $LABEL_NAME=$_cp_name: compose would take it over, and the script never takes over what it did not make; remove or rename it, or choose another DEPLOY_NAME" ;;
		esac
	done
	[ -n "$_cp_vols" ] || return 0
	if [ ! -f "$STATE/target" ] && [ "$ADOPT" != 1 ]; then
		refuse "the deployment's volumes ($(printf '%s' "$_cp_vols" | sed 's/^ //')) are on $(engine), and $STATE holds no record of them: they were made from another machine or another STATE_DIR, or the record was lost; say --adopt to take the deployment's secrets into $STATE"
	fi
	if [ -z "$_cp_cfg" ] && [ ! -f "$STATE/config/ready" ]; then
		refuse "the deployment's data volumes are on $(engine), and neither the volume $_cp_name-config nor $STATE/config holds the key file and certificates they were made with: new ones would open nothing; put back the entry of STATE_DIR, or purge the deployment"
	fi
}

# compose_ports_check: every port of the four nodes is free, or held by the
# deployment's own containers - asked before anything changes. A port
# another container of the engine publishes is refused naming it; then a
# connection to each port the deployment does not hold already - from this
# machine through curl (never a proxy), on CE_HOST through the server's
# curl, where only "connection refused" (exit 7) is free - is refused when
# it is answered. S3 is published on every address, the admin ports on
# 127.0.0.1.
compose_ports_check() {
	_pc_out=$(target_engine query ps --format "{{.Names}}|{{.Ports}}|$(compose_lf "$LABEL_NAME")" </dev/null) ||
		fail "could not list the containers of $(engine) (its message is above)"
	_pc_out=$(printf '%s\n' "$_pc_out" | printable)
	_pc_mine=
	_pc_list=
	for _pc_n in 1 2 3 4; do
		_pc_list="$_pc_list $(compose_s3_port "$_pc_n"):0.0.0.0 $(compose_admin_port "$_pc_n"):127.0.0.1"
	done
	while IFS='|' read -r _pc_name _pc_ports _pc_owner; do
		[ -n "$_pc_name" ] || continue
		if [ "$_pc_owner" = "$_cp_name" ]; then
			_pc_mine="$_pc_mine,$_pc_ports"
			continue
		fi
		for _pc_pair in $_pc_list; do
			if docker_ports_hold "${_pc_pair%%:*}" "$_pc_ports" "${_pc_pair#*:}"; then
				refuse "port ${_pc_pair%%:*} is published by the container $_pc_name: choose another S3_PORT (cluster mode uses S3_PORT to S3_PORT + 31), or stop it"
			fi
		done
	done <<EOF
$_pc_out
EOF
	if [ -z "$_cp_ce_host" ]; then
		command -v curl >/dev/null 2>&1 ||
			refuse "curl is needed: the script asks with it whether a port is free and whether a node is ready"
	fi
	for _pc_pair in $_pc_list; do
		_pc_port=${_pc_pair%%:*}
		if docker_ports_hold "$_pc_port" "$_pc_mine" "${_pc_pair#*:}"; then
			continue
		fi
		if [ -n "$_cp_ce_host" ]; then
			_pc_status=0
			query on_host "$_cp_ce_host" env -i "PATH=$_cp_rpath" curl -q -s -o /dev/null --max-time 3 --noproxy 127.0.0.1 "http://127.0.0.1:$_pc_port/" </dev/null || _pc_status=$?
			case $_pc_status in
			7) ;;
			127) refuse "curl is not installed on $_cp_ce_host: the script asks with it there whether a port is free and whether a node is ready" ;;
			255) fail "ssh to $_cp_ce_host failed while a port was asked (ssh's message is above)" ;;
			*) refuse "port $_pc_port on $_cp_ce_host is in use (something answered on 127.0.0.1:$_pc_port there): choose another S3_PORT, or stop what holds it" ;;
			esac
			continue
		fi
		_pc_conn=$(query curl -q -g -s -o /dev/null --noproxy '*' --max-time 3 -w '%{num_connects}' "http://127.0.0.1:$_pc_port/" </dev/null 2>/dev/null) || :
		case $(printf '%s' "$_pc_conn" | printable | tr -d '\n') in
		'' | 0) ;;
		*) refuse "port $_pc_port is in use (something answered on 127.0.0.1:$_pc_port): choose another S3_PORT (cluster mode uses S3_PORT to S3_PORT + 31), or stop what holds it" ;;
		esac
	done
}

# yaml_q <value>: the value as a YAML string in single quotes, in which only
# a single quote means anything (written twice). No control character
# reaches here (env.sh refuses them in CACTUSFS_* values).
yaml_q() {
	printf "'%s'" "$(printf '%s' "$1" | sed "s/'/''/g")"
}

# compose_labels <indent>: the labels of everything the override names.
compose_labels() {
	printf '%slabels:\n' "$1"
	printf '%s  %s: %s\n' "$1" "$LABEL_NAME" "$(yaml_q "$_cp_name")" "$1" "$LABEL_TARGET" "$(yaml_q "$TARGET")"
}

# compose_passed_node: the env file's CACTUSFS_* lines that compose.yaml
# does not interpolate, as the lines of a node's environment.
compose_passed_node() {
	_pn_vars=$(compose_vars)
	while IFS= read -r _pn_line; do
		[ -n "$_pn_line" ] || continue
		! printf '%s\n' "$_pn_vars" | grep -qx "${_pn_line%%=*}" || continue
		printf '      %s: %s\n' "${_pn_line%%=*}" "$(yaml_q "${_pn_line#*=}")"
	done <<EOF
$PASSED
EOF
}

# compose_override: the override, on standard output.
compose_override() {
	_co_policy=unless-stopped
	[ "$TARGET" != podman ] || _co_policy=always
	echo "# Written by deploy/deploy.sh (PLAN 10.8) for the deployment $_cp_name on $TARGET;"
	echo "# written again by every --install. compose.yaml beside it is the repository's,"
	echo "# unchanged; compose runs them with up --no-build and compose.env."
	echo "services:"
	echo "  init:"
	echo "    image: $(yaml_q "$_dk_image")"
	echo "    pull_policy: never"
	compose_labels '    '
	echo "    command:"
	if [ "$_s_kms" = vault ]; then
		# With Vault the config volume holds no key file to seal: init only
		# says its version (it would need a passphrase file to run).
		echo "      - version"
	else
		set -- dev-init --dir /var/lib/cactusfs/config --nodes node1,node2,node3,node4 \
			--kms-passphrase-file /run/secrets/kms-passphrase --validity "$_s_validity"
		[ -z "$_s_hosts" ] || set -- "$@" --hosts "$_s_hosts"
		for _co_w in "$@"; do
			echo "      - $(yaml_q "$_co_w")"
		done
	fi
	for _co_node in $COMPOSE_NODES; do
		echo "  $_co_node:"
		echo "    image: $(yaml_q "$_dk_image")"
		echo "    pull_policy: never"
		echo "    restart: $_co_policy"
		compose_labels '    '
		echo "    environment:"
		echo "      CACTUSFS_DRIVE_LABELS: $(yaml_q "${_cp_name}_$_co_node-data1,${_cp_name}_$_co_node-data2")"
		echo "      CACTUSFS_UPGRADE_MODE: 'off'"
		if [ "$_s_kms" = vault ]; then
			echo "      CACTUSFS_KMS: 'vault'"
			echo "      CACTUSFS_KMS_KEY_FILE: ''"
			echo "      CACTUSFS_KMS_PASSPHRASE_FILE: ''"
			echo "      CACTUSFS_VAULT_ADDR: $(yaml_q "$(setting VAULT_ADDR)")"
			echo "      CACTUSFS_VAULT_TRANSIT_MOUNT: $(yaml_q "$(setting VAULT_TRANSIT_MOUNT)")"
			echo "      CACTUSFS_VAULT_KEY: $(yaml_q "$(setting VAULT_KEY)")"
			echo "      CACTUSFS_VAULT_NAMESPACE: $(yaml_q "$(setting VAULT_NAMESPACE)")"
			echo "      CACTUSFS_VAULT_TOKEN_FILE: '/run/secrets/vault-token'"
		fi
		compose_passed_node
		if [ "$_s_kms" = vault ]; then
			echo "    secrets: [vault-token]"
		fi
	done
	echo "volumes:"
	echo "  config:"
	echo "    external: true"
	echo "    name: $(yaml_q "$_cp_name-config")"
	for _co_node in $COMPOSE_NODES; do
		for _co_vol in meta data1 data2; do
			echo "  $_co_node-$_co_vol:"
			compose_labels '    '
		done
	done
	echo "networks:"
	echo "  default:"
	compose_labels '    '
	if [ "$_s_kms" = vault ]; then
		# compose.yaml's kms-passphrase secret needs a file: with Vault an
		# empty one, which no node reads.
		echo "secrets:"
		echo "  kms-passphrase:"
		echo "    file: ./none/kms-passphrase"
		echo "  vault-token:"
		echo "    file: ./secrets/vault-token"
	fi
}

# compose_env: compose.env, on standard output - every variable compose.yaml
# interpolates, with the value the deployment gives it: the image, the port
# bases, the env file's CACTUSFS_* value or nothing (the node's default).
# One the script does not know fails the run: compose.yaml and this file
# are kept in step.
compose_env() {
	echo "# Written by deploy/deploy.sh (PLAN 10.8): every value compose.yaml interpolates."
	for _ce_var in $(compose_vars); do
		case $_ce_var in
		CACTUSFS_IMAGE) _ce_value=$_dk_image ;;
		CACTUSFS_BUILD) _ce_value=0 ;;
		CACTUSFS_COMMIT) _ce_value= ;;
		CACTUSFS_PORT_BASE_1 | CACTUSFS_PORT_BASE_2 | CACTUSFS_PORT_BASE_3 | CACTUSFS_PORT_BASE_4) _ce_value=$_cp_base ;;
		*)
			if ! _ce_value=$(lookup "$PASSED" "$_ce_var"); then
				_ce_value=
				[ -n "$(cactusfs_settings | awk -v name="$_ce_var" '$1 == name && $2 == "setting" { print }')" ] || {
					echo "compose.yaml interpolates $_ce_var, which deploy/lib/compose.sh does not know" >"$_cp_stage/.unknown"
				}
			fi
			;;
		esac
		printf '%s=%s\n' "$_ce_var" "$_ce_value"
	done
}

# compose_put_file <file>: standard input into the file, 0600, written
# beside it and renamed into place.
compose_put_file() {
	_pf_tmp=$(mktemp "${1%/*}/.new.XXXXXX")
	cat >"$_pf_tmp"
	chmod 0600 "$_pf_tmp"
	mv -f "$_pf_tmp" "$1"
}

# compose_files: the compose directory of the entry - compose.yaml copied,
# the override and compose.env written (and for Vault the empty passphrase
# file) - then, on CE_HOST, the same files sent to the server's compose
# directory as a tar stream, with the CA of the S3 certificate.
compose_files() {
	if [ "$DRY_RUN" = 1 ]; then
		say "dry run: would write $_cp_local/compose.yaml (deploy/compose/compose.yaml), override.yaml and compose.env"
		[ -z "$_cp_ce_host" ] || say "dry run: and send them to $_cp_dir on $_cp_ce_host"
		return 0
	fi
	umask 077
	mkdir -p "$_cp_local"
	chmod 0700 "$_cp_local"
	_cp_stage=$_cp_local
	rm -f "$_cp_stage/.unknown"
	compose_put_file "$_cp_local/compose.yaml" <"$REPO_ROOT/deploy/compose/compose.yaml"
	compose_override | compose_put_file "$_cp_local/override.yaml"
	compose_env | compose_put_file "$_cp_local/compose.env"
	if [ -f "$_cp_stage/.unknown" ]; then
		_cf_why=$(printable <"$_cp_stage/.unknown")
		rm -f "$_cp_stage/.unknown"
		fail "internal: $_cf_why"
	fi
	set -- compose.yaml override.yaml compose.env
	if [ "$_s_kms" = vault ]; then
		mkdir -p "$_cp_local/none"
		chmod 0700 "$_cp_local/none"
		[ -f "$_cp_local/none/kms-passphrase" ] || : >"$_cp_local/none/kms-passphrase"
		chmod 0444 "$_cp_local/none/kms-passphrase"
		set -- "$@" none
	fi
	say "compose: wrote $_cp_local/compose.yaml, override.yaml and compose.env"
	[ -n "$_cp_ce_host" ] || return 0
	if [ -f "$STATE/config/s3-ca.pem" ]; then
		compose_put_file "$_cp_local/s3-ca.pem" <"$STATE/config/s3-ca.pem"
		set -- "$@" s3-ca.pem
	fi
	tar -c -f "$_cp_local/.send.tar" -C "$_cp_local" "$@"
	run on_host "$_cp_ce_host" tar -x -o -f - -C "$_cp_dir" <"$_cp_local/.send.tar"
	rm -f "$_cp_local/.send.tar"
	say "compose: sent them to $_cp_dir on $_cp_ce_host"
}

# compose_relabel: under enforcing SELinux - this machine's, or CE_HOST's -
# a bind-mounted file needs the label of container files: the directory of
# compose's secret files (and Vault's empty passphrase file) is relabelled
# container_file_t, as its owner. No SELinux tools: nothing to do.
compose_relabel() {
	_rl_status=0
	if [ -n "$_cp_ce_host" ]; then
		_rl=$(query on_host "$_cp_ce_host" getenforce </dev/null) || _rl_status=$?
		case $_rl_status in
		0) ;;
		127) return 0 ;;
		*) fail "could not ask $_cp_ce_host whether SELinux is enforcing (getenforce exited $_rl_status; its message is above)" ;;
		esac
	else
		command -v getenforce >/dev/null 2>&1 || return 0
		_rl=$(query getenforce </dev/null) || fail "getenforce failed (its message is above)"
	fi
	[ "$(printf '%s' "$_rl" | printable | tr -d '\n')" = Enforcing ] || return 0
	set -- "$_s_secdir"
	[ "$_s_kms" != vault ] || set -- "$@" "$_cp_dir/none"
	if [ -n "$_cp_ce_host" ]; then
		run on_host "$_cp_ce_host" chcon -R -t container_file_t "$@" </dev/null
	else
		run chcon -R -t container_file_t "$@" </dev/null
	fi
	say "compose: SELinux is enforcing: relabelled $* container_file_t, so the nodes may read the files bind-mounted from it"
}

# compose_tls_name: the name /readyz and the admin API are asked for (as
# docker.sh's).
compose_tls_name() {
	docker_tls_name
}

# compose_readyz <n>: _dk_ready, what /readyz of node n's admin port
# answers - 200 when ready. From this machine as docker.sh asks it (TLS
# with the deployment's CA, never a proxy); on CE_HOST by the server's curl
# under env -i, whose exit 0 is 200 (it fails on anything else).
compose_readyz() {
	_dk_admin_port=$(compose_admin_port "$1")
	if [ -z "$_cp_ce_host" ]; then
		docker_readyz
		return 0
	fi
	_rz_name=$(compose_tls_name)
	set -- env -i "PATH=$_cp_rpath" curl -q -s -f -o /dev/null --max-time 5 --noproxy "$_rz_name"
	[ ! -f "$STATE/config/s3-ca.pem" ] || set -- "$@" --cacert "$_cp_dir/s3-ca.pem"
	set -- "$@" --resolve "$_rz_name:$_dk_admin_port:127.0.0.1" "https://$_rz_name:$_dk_admin_port/readyz"
	_rz_status=0
	query on_host "$_cp_ce_host" "$@" </dev/null || _rz_status=$?
	case $_rz_status in
	0) _dk_ready=200 ;;
	127) fail "curl is not installed on $_cp_ce_host: the script asks with it there whether a node is ready" ;;
	255) fail "ssh to $_cp_ce_host failed while /readyz was asked (ssh's message is above)" ;;
	*) _dk_ready="not 200 (curl on $_cp_ce_host exited $_rz_status)" ;;
	esac
}

# compose_wait <service>: true once /readyz of the node answers 200, asked
# every 2 seconds until WAIT_TIMEOUT seconds have passed; false when it
# never did.
compose_wait() {
	_cw_n=${1#node}
	_cw_start=$(date +%s)
	while :; do
		compose_readyz "$_cw_n"
		[ "$_dk_ready" != 200 ] || return 0
		_cw_now=$(date +%s)
		[ $((_cw_now - _cw_start)) -lt "$_cp_wait" ] || return 1
		sleep 2
	done
}

# compose_leader: _cp_leader, the voter that leads the metadata group as
# the admin API names it (GET /api/v1/cluster, the root credentials as the
# bearer API key on curl's standard input - never on a command line), asked
# of each voter in turn; empty when none says.
compose_leader() {
	_cp_leader=
	_cl_name=$(compose_tls_name)
	for _cl_n in 1 2 3; do
		_cl_port=$(compose_admin_port "$_cl_n")
		if [ -n "$_cp_ce_host" ]; then
			set -- on_host "$_cp_ce_host" env -i "PATH=$_cp_rpath" curl -q -s -f --max-time 5 --noproxy "$_cl_name"
			[ ! -f "$STATE/config/s3-ca.pem" ] || set -- "$@" --cacert "$_cp_dir/s3-ca.pem"
			set -- "$@" --resolve "$_cl_name:$_cl_port:127.0.0.1"
		else
			set -- curl -q -g -s -f --noproxy '*' --max-time 5
			[ ! -f "$STATE/config/s3-ca.pem" ] || set -- "$@" --cacert "$STATE/config/s3-ca.pem"
			set -- "$@" --connect-to "$_cl_name:$_cl_port:127.0.0.1:$_cl_port"
		fi
		set -- "$@" -H @- "https://$_cl_name:$_cl_port/api/v1/cluster"
		_cl_out=$({
			printf 'Authorization: Bearer '
			tr -d '\n' <"$STATE/secrets/root-access-key"
			printf ':'
			tr -d '\n' <"$STATE/secrets/root-secret-key"
			printf '\n'
		} | query "$@" 2>/dev/null) || continue
		_cp_leader=$(printf '%s' "$_cl_out" | printable | tr -d '\n' |
			sed -n 's/.*"leader"[[:space:]]*:[[:space:]]*"\(node[123]\)".*/\1/p' | head -n 1)
		[ -z "$_cp_leader" ] || return 0
	done
}

# compose_cli: the command an operator types to reach the project, for the
# messages.
compose_cli() {
	if [ -n "$_cp_ce_host" ]; then
		printf 'ssh %s docker compose --project-name %s -f %s/compose.yaml -f %s/override.yaml --env-file %s/compose.env' \
			"$_cp_ce_host" "$_cp_name" "$_cp_dir" "$_cp_dir" "$_cp_dir"
	elif [ "$TARGET" = podman ]; then
		printf 'podman compose --project-name %s -f %s/compose.yaml -f %s/override.yaml --env-file %s/compose.env' \
			"$_cp_name" "$(shown "$_cp_dir")" "$(shown "$_cp_dir")" "$(shown "$_cp_dir")"
	else
		printf 'docker --context %s compose --project-name %s -f %s/compose.yaml -f %s/override.yaml --env-file %s/compose.env' \
			"$_s_context" "$_cp_name" "$(shown "$_cp_dir")" "$(shown "$_cp_dir")" "$(shown "$_cp_dir")"
	fi
}

# compose_upgrade [force]: one service at a time - node4, which does not
# vote, then the voters, the leader last when the admin API names it - each
# made again from the new override (up --no-deps) and ready on /readyz
# before the next; the first that is not stops the run, saying which nodes
# were not touched. "force" makes each again even when its configuration is
# unchanged (--force-recreate): a rotated file of compose's secrets is a new
# file, and a bind mount of a single file keeps the old one until the
# container is made again.
compose_upgrade() {
	_cu_force=
	[ "${1:-}" != force ] || _cu_force=--force-recreate
	compose_leader
	_cu_voters=
	for _cu in node1 node2 node3; do
		[ "$_cu" = "$_cp_leader" ] || _cu_voters="$_cu_voters $_cu"
	done
	if [ -n "$_cp_leader" ]; then
		say "upgrade: the admin API names $_cp_leader the leader; it goes last"
		_cu_voters="$_cu_voters $_cp_leader"
	else
		say "upgrade: no voter named the leader (the admin API did not answer): the voters go in order"
	fi
	_cu_order="node4$_cu_voters"
	if [ -n "${_cp_first:-}" ]; then
		_cu_rest=
		for _cu in $_cu_order; do
			[ "$_cu" = "$_cp_first" ] || _cu_rest="$_cu_rest $_cu"
		done
		_cu_order="$_cp_first$_cu_rest"
	fi
	say "upgrade: one node at a time: $_cu_order"
	_cu_done=
	for _cu in $_cu_order; do
		# shellcheck disable=SC2086 # one flag, or none
		if ! compose_project run up -d --no-build --no-deps $_cu_force "$_cu" </dev/null; then
			fail "upgrade: compose could not make $_cu again (its message is above); upgraded before it:${_cu_done:- none}; not touched after it: $(compose_after "$_cu" "$_cu_order"). Its log: $(compose_cli) logs $_cu$(compose_pending_note "$_cu_done" "$_cu_order")"
		fi
		if [ "$DRY_RUN" = 1 ]; then
			say "dry run: would wait up to $_cp_wait s for /readyz of $_cu (127.0.0.1:$(compose_admin_port "${_cu#node}")) to answer 200"
			continue
		fi
		if ! compose_wait "$_cu"; then
			fail "upgrade stopped: $_cu did not become ready within WAIT_TIMEOUT=$_cp_wait s on $_dk_image (/readyz of 127.0.0.1:$(compose_admin_port "${_cu#node}") answered $_dk_ready); upgraded before it:${_cu_done:- none}; not touched after it, on their image: $(compose_after "$_cu" "$_cu_order"). Its log: $(compose_cli) logs $_cu$(compose_pending_note "$_cu_done $_cu" "$_cu_order")"
		fi
		say "upgrade: $_cu is ready on $_dk_image"
		_cu_done="$_cu_done $_cu"
	done
}

# compose_pending_note <made again> <order>: with a rotation pending, which
# nodes hold the new files - those made again - and which the old ones,
# for a failure's message.
compose_pending_note() {
	[ -n "$_cu_force" ] || return 0
	_pn_old=
	for _pn in $2; do
		case " $1 " in
		*" $_pn "*) ;;
		*) _pn_old="$_pn_old $_pn" ;;
		esac
	done
	printf '. The rotated files (%s) are held by the nodes made again:%s; the others still hold the old ones:%s. %s is kept: the next --install makes every node again while no more than one node is down; otherwise --uninstall, then --install' \
		"$(tr '\n' ' ' <"$_cp_pending" | sed 's/ $//')" "${1:- none}" "${_pn_old:- none}" "$_cp_pending"
}

# compose_after <service> <order>: the services after it in the order.
compose_after() {
	_ca_seen=
	_ca_out=
	for _ca in $2; do
		if [ -n "$_ca_seen" ]; then
			_ca_out="$_ca_out $_ca"
		fi
		[ "$_ca" != "$1" ] || _ca_seen=1
	done
	printf '%s' "${_ca_out# }"
	[ -n "$_ca_out" ] || printf 'none'
}

# compose_secret_modes: every file of compose's secret files 0444 and the
# directory 0700, whatever a copy or a restore of STATE_DIR left them: a
# node (65532) reads a bind-mounted file by its other bits.
compose_secret_modes() {
	if [ -z "$_cp_ce_host" ]; then
		for _sm_f in $(files_of "$_s_secdir"); do
			chmod 0444 "$_s_secdir/$_sm_f"
		done
		chmod 0700 "$_s_secdir"
		return 0
	fi
	set --
	for _sm_f in $(files_of "$STATE/secrets"); do
		set -- "$@" "$_s_secdir/$_sm_f"
	done
	[ $# -eq 0 ] || run on_host "$_cp_ce_host" chmod 0444 "$@" </dev/null
	run on_host "$_cp_ce_host" chmod 0700 "$_s_secdir" </dev/null
}

# compose_ready_all: before an upgrade, /readyz of every node. One node
# that is not ready - failed on a new image or a new secret, stopped, or
# without its container - is _cp_first, the node the upgrade makes first,
# so that the readiness of each node before the next keeps a second one
# from going down: that is how an upgrade that failed is rolled back or
# tried again. Two or more not ready are refused (nothing has changed):
# taking one more down would leave the cluster without the nodes it needs.
compose_ready_all() {
	_cp_first=
	_ra_down=
	for _ra in 1 2 3 4; do
		compose_readyz "$_ra"
		[ "$_dk_ready" = 200 ] || _ra_down="$_ra_down node$_ra (/readyz of 127.0.0.1:$(compose_admin_port "$_ra") answered $_dk_ready)"
	done
	case $_ra_down in
	'') return 0 ;;
	' node'?' ('*' node'?' ('*)
		_ra_pending=
		[ ! -f "$STATE/compose/recreate-pending" ] ||
			_ra_pending=". A rotation is pending ($(tr '\n' ' ' <"$STATE/compose/recreate-pending" | sed 's/ $//'), $STATE/compose/recreate-pending): the --install after --uninstall starts every node with the rotated files and removes the marker"
		refuse "more than one node is not ready:$_ra_down; an upgrade takes one node down at a time and would leave the cluster without the nodes it needs. See $(compose_cli) logs; to start again with the volumes kept, run --uninstall, then --install$_ra_pending"
		;;
	esac
	_cp_first=${_ra_down# }
	_cp_first=${_cp_first%% *}
	say "$_cp_first is not ready:${_ra_down#" $_cp_first"}; the upgrade makes it first"
}

# compose_ce_engine: on CE_HOST, the engine compose reaches there (the
# server's own docker, under env -i) is the one DOCKER_HOST=ssh:// reaches
# - the engine whose ID the record holds - or the run is refused: docker
# info's ID, read from its text (the template's braces are not a word ssh
# is handed).
compose_ce_engine() {
	[ -n "$_cp_ce_host" ] || return 0
	engine_id_ask
	_ce_info=$(query on_host "$_cp_ce_host" env -i "PATH=$_cp_rpath" "HOME=$_cp_rhome" docker info </dev/null) ||
		fail "could not ask the docker of $_cp_ce_host who it is (its message is above)"
	_ce_id=$(printf '%s\n' "$_ce_info" | printable | sed -n 's/^ *ID: *//p' | head -n 1)
	[ "$_ce_id" = "$_engine_id" ] ||
		refuse "compose on $_cp_ce_host reaches the engine '$_ce_id', and DOCKER_HOST=ssh://$_cp_ce_host reaches '$_engine_id': the volumes and the containers would be on two engines; make the docker the ssh user runs there reach the same engine with a current docker context of that user (docker context use <context> on $_cp_ce_host): compose runs there under env -i, so DOCKER_HOST is not read there"
}

# compose_install: --install of a cluster on a Docker engine.
compose_install() {
	# A deployment STATE_DIR records in the other mode is refused before
	# anything is asked - of the server on CE_HOST too.
	state_mode_check
	compose_settings
	compose_refusals
	tls_hosts_warn "on every address of the machine of its engine, at the ports $_cp_s3, $((_cp_s3 + 10)), $((_cp_s3 + 20)) and $((_cp_s3 + 30)) (a compose cluster)"
	# On CE_HOST, CE_INSTALL_DOCKER=yes installs Docker CE (and its compose
	# plugin) once what refuses without an engine has been asked - the
	# image's such refusals among it (PLAN 10.4's image_precheck; ce.sh).
	if [ -n "$_cp_ce_host" ]; then
		docker_image_precheck
		[ "$(setting CE_INSTALL_DOCKER)" != yes ] || ce_install_docker
	fi
	compose_engine_local
	compose_check_tool
	compose_ce_engine
	compose_containers
	compose_volumes
	compose_ports_check
	# PLAN 10.4's image_check, as docker_install asks it: it only reads, and
	# refuses before anything changes.
	docker_image_check
	if [ -n "$_cp_svcs" ]; then
		compose_ready_all
		say "installed: the services$_cp_svcs; this run upgrades them one at a time, keeping their volumes"
	fi
	# What the secrets refuse is asked before the image is made (a dry run
	# says what it would do instead, in secrets_install).
	[ "$DRY_RUN" = 1 ] || secrets_check

	# Nothing of the deployment has been changed so far. The image is made
	# where it is made - built or pulled, and checked - then the secrets -
	# the config volume and compose's secret files - before anything starts
	# (PLAN 10.3); on CE_HOST the host keys are pinned once the entry is
	# there; then the image is delivered to the target's engine (PLAN 10.4).
	docker_image_make
	secrets_install
	[ -z "$_cp_ce_host" ] || ce_pin_record
	docker_image_ref
	say "image: $_dk_image"
	compose_files
	if [ "$DRY_RUN" = 0 ]; then
		compose_secret_modes
		compose_relabel
	fi

	_cp_pending=$STATE/compose/recreate-pending
	# The images the nodes ran before this run: an upgrade removes those
	# the script recorded making once every node is ready (PLAN 10.14).
	_ci_old=
	while IFS='|' read -r _ci_n _ci_s _ci_state _ci_img; do
		case $_ci_s in
		node?) _ci_old="$_ci_old $_ci_img" ;;
		esac
	done <<EOF
$_cp_ctrs
EOF
	if [ -n "$_cp_svcs" ]; then
		if [ -f "$_cp_pending" ]; then
			say "rotated, not yet held by every node: $(tr '\n' ' ' <"$_cp_pending" | sed 's/ $//') - every node is made again, one at a time, so that its mounts hold the new files"
			compose_upgrade force
		else
			compose_upgrade
		fi
	else
		if ! compose_project run up -d --no-build </dev/null; then
			fail "compose could not start the cluster (its message is above); its logs: $(compose_cli) logs"
		fi
		if [ "$DRY_RUN" = 1 ]; then
			say "dry run: would wait up to $_cp_wait s for /readyz of each node to answer 200"
			return 0
		fi
		for _ci in $COMPOSE_NODES; do
			compose_wait "$_ci" ||
				fail "$_ci did not become ready within WAIT_TIMEOUT=$_cp_wait s (/readyz of 127.0.0.1:$(compose_admin_port "${_ci#node}") answered $_dk_ready); its log: $(compose_cli) logs $_ci"
			say "$_ci is ready"
		done
	fi
	[ "$DRY_RUN" = 0 ] || return 0
	if [ -f "$_cp_pending" ]; then
		rm -f "$_cp_pending"
		say "compose: every node holds the rotated files and is ready: removed $_cp_pending"
	fi
	# shellcheck disable=SC2086 # image references, without spaces
	docker_images_replaced $_ci_old
	compose_reach
	say "ready: the cluster $_cp_name runs $_dk_image"
	docker_admin_notice
	if [ "$TARGET" = podman ]; then
		say "Podman starts containers again after a reboot only through its podman-restart service, which starts those with the restart policy always - the nodes have it: systemctl --user enable podman-restart.service, and for a rootless user loginctl enable-linger \"\$USER\" (as root: systemctl enable podman-restart.service)"
	fi
}

# compose_reach: where the cluster is reached, and where its CA and root
# credentials are kept (by their files, never their contents).
compose_reach() {
	_rc_name=$(compose_tls_name)
	for _rc_n in 1 2 3 4; do
		if [ -n "$_cp_ce_host" ]; then
			say "S3 of node$_rc_n: https://<an address of $_cp_ce_host>:$(compose_s3_port "$_rc_n")"
		else
			say "S3 of node$_rc_n: https://<any address of this machine>:$(compose_s3_port "$_rc_n") (on this machine: https://$_rc_name:$(compose_s3_port "$_rc_n"))"
		fi
	done
	if [ -n "$_cp_ce_host" ]; then
		say "admin API and console, on $_cp_ce_host's 127.0.0.1 only: ssh -L $(compose_admin_port 1):127.0.0.1:$(compose_admin_port 1) $_cp_ce_host, then https://localhost:$(compose_admin_port 1)"
	else
		say "admin API and console: https://127.0.0.1:$(compose_admin_port 1) (node1; node2 to node4 on $(compose_admin_port 2), $(compose_admin_port 3) and $(compose_admin_port 4))"
	fi
	if [ -f "$STATE/config/s3-ca.pem" ]; then
		say "the CA to trust: $STATE/config/s3-ca.pem"
	fi
	say "root credentials: $STATE/secrets/root-access-key and $STATE/secrets/root-secret-key (never printed)"
}

# compose_status: --status - each node's container, state, image and
# version, the volumes, where it is reached, when its certificate expires
# and what /readyz says. Read-only. Exits 0 when every node is ready or
# nothing is installed, 1 when it is installed and a node is not ready.
compose_status() {
	state_mode_check
	compose_settings
	_st_out=$(target_engine query ps -a --filter "label=$LABEL_NAME=$_cp_name" \
		--format "$(compose_lf com.docker.compose.service)|{{.Names}}|{{.State}}|{{.Image}}|$(compose_lf "$LABEL_TARGET")|$(compose_lf org.opencontainers.image.version)" </dev/null) ||
		fail "could not list the containers of $(engine) (its message is above)"
	_st_out=$(printf '%s\n' "$_st_out" | printable)
	docker_labelled_names
	if [ -z "$(printf '%s\n' "$_st_out" | grep '^node[1234]|')" ]; then
		say "$_cp_name is not installed on $(engine)"
		[ -z "$_dk_labelled" ] || say "its volumes are kept: $_dk_labelled (--install runs it again with them; --uninstall --purge deletes them)"
		return 0
	fi
	_st_bad=
	for _st_svc in $COMPOSE_NODES; do
		_st_line=$(printf '%s\n' "$_st_out" | grep "^$_st_svc|" | head -n 1) || :
		if [ -z "$_st_line" ]; then
			say "$_st_svc: no container"
			_st_bad=1
			continue
		fi
		IFS='|' read -r _st_s _st_name _st_state _st_image _st_target _st_version <<EOF
$_st_line
EOF
		[ "$_st_target" = "$TARGET" ] ||
			refuse "the container $_st_name is the deployment's on the target '$_st_target', not $TARGET: run with --docker $_st_target"
		compose_readyz "${_st_svc#node}"
		say "$_st_svc: $_st_name, $_st_state, image $_st_image, version ${_st_version:-unknown}, /readyz $_dk_ready"
		[ "$_dk_ready" = 200 ] || _st_bad=1
	done
	say "volumes: ${_dk_labelled:-none}"
	compose_reach
	docker_cert_expiry
	if [ -n "$_st_bad" ]; then
		say "not ready: the logs: $(compose_cli) logs"
		exit 1
	fi
	say "/readyz: 200 on every node"
}

# compose_uninstall: --uninstall - the containers of the deployment (by its
# label) stopped and removed, and its labelled network - and with --purge
# the data and the secrets, in the order of ADR-0033 section 3, compose's
# secret files on CE_HOST with the secrets. Everything is asked before
# anything is removed.
compose_uninstall() {
	# A purge is confirmed before anything is asked - of the server on
	# CE_HOST too.
	state_mode_check
	_s_name=$(setting DEPLOY_NAME)
	[ "$PURGE" = 0 ] || docker_purge_confirm
	compose_settings
	_dk_labelled=
	if [ "$PURGE" = 1 ]; then
		docker_engine_check purge
	else
		docker_engine_check uninstall
	fi
	compose_containers
	_cu_names=
	while IFS='|' read -r _cu_n _cu_rest; do
		[ -z "$_cu_n" ] || _cu_names="$_cu_names $_cu_n"
	done <<EOF
$_cp_ctrs
EOF
	_cu_nets=$(target_engine query network ls --filter "label=$LABEL_NAME=$_cp_name" --filter "label=$LABEL_TARGET=$TARGET" --format '{{.Name}}' </dev/null) ||
		fail "could not list the networks of $(engine) (its message is above)"
	_cu_nets=$(printf '%s\n' "$_cu_nets" | printable | sed '/^$/d' | tr '\n' ' ' | sed 's/ *$//')
	if [ "$PURGE" = 1 ]; then
		docker_labelled
		# Every volume the purge would delete is the cluster's: compose's
		# 12 or the config volume - never another deployment's of the name.
		_cu_expected=$(compose_volume_names)
		for _cu_v in $_dk_labelled; do
			[ "$_cu_v" != "$_cp_name-config" ] || continue
			printf '%s\n' "$_cu_expected" | grep -qxF "$_cu_v" ||
				refuse "the volume $_cu_v carries the deployment's name and is not one of its compose cluster's (a standalone deployment's?): nothing is deleted; purge it with DEPLOY_MODE=standalone"
		done
		_dk_have_ctr=
		[ -z "$_cu_names$_cp_helpers" ] || _dk_have_ctr=1
		_dk_have_prev=
		docker_adopt_record
	fi
	if [ -n "$_cu_names" ]; then
		# shellcheck disable=SC2086 # container names, without spaces
		target_engine run stop -t 40 $_cu_names </dev/null
		# shellcheck disable=SC2086 # container names, without spaces
		target_engine run rm $_cu_names </dev/null
		say "removed the containers$_cu_names"
	else
		say "no container of $_cp_name on $(engine)"
	fi
	for _cu_n in $_cp_helpers; do
		target_engine run rm -f "$_cu_n" </dev/null
		say "removed $_cu_n, a helper a run left behind"
	done
	if [ -n "$_cu_nets" ]; then
		# shellcheck disable=SC2086 # network names, without spaces
		target_engine run network rm $_cu_nets </dev/null
		say "removed the network $_cu_nets"
	fi
	if [ "$PURGE" = 0 ]; then
		docker_labelled_names
		say "kept: the volumes ${_dk_labelled:-(none)}, compose's files and $STATE: --install runs the cluster again with them"
		return 0
	fi
	docker_purge compose_purge_secret_files
}

# compose_purge_secret_files: in a purge, after the secrets' volume and
# before the local entry: on CE_HOST the server's copy of compose's secret
# files, then the rest of the deployment's directory there, and a check
# that it is gone. Here they are in the entry, which goes last.
compose_purge_secret_files() {
	[ -n "$_cp_ce_host" ] || return 0
	_pf_entry=${_cp_dir%/compose}
	case $_pf_entry in
	"$_cp_rbase"/*/"$_cp_name") ;;
	*) fail "internal: $_pf_entry is not the deployment's directory on $_cp_ce_host" ;;
	esac
	run on_host "$_cp_ce_host" rm -rf -- "$_cp_dir/secrets" </dev/null
	say "purge: deleted compose's secret files $_cp_dir/secrets on $_cp_ce_host"
	run on_host "$_cp_ce_host" rm -rf -- "$_pf_entry" </dev/null
	[ "$DRY_RUN" = 0 ] || return 0
	_pf_status=0
	query on_host "$_cp_ce_host" test -e "$_pf_entry" </dev/null || _pf_status=$?
	case $_pf_status in
	1) ;;
	0) fail "purge: $_pf_entry is still there on $_cp_ce_host; $STATE is kept" ;;
	*) fail "purge: could not ask $_cp_ce_host whether $_pf_entry is gone (exit $_pf_status); $STATE is kept" ;;
	esac
	say "purge: deleted $_pf_entry on $_cp_ce_host"
}

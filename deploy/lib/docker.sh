# shellcheck shell=sh
# A standalone deployment on a Docker engine - Docker Desktop (dd), Docker CE
# on this machine (ce) and Podman (podman): install, upgrade, status,
# uninstall and purge (PLAN 10.5; ADR-0033 sections 3, 4, 8, 11). Sourced by
# deploy/deploy.sh; not run on its own.
#
# What a deployment named <name> is on the engine - every object labelled
# io.cactusfs.deploy.name=<name> and io.cactusfs.deploy.target=<target>, and
# nothing else touched:
#
#   <name>             the node: one container of the product image, run as
#                      65532 with a read-only root filesystem, every
#                      capability dropped, no new privileges, restarted unless
#                      stopped; S3 published on BIND:S3_PORT, the admin port
#                      on ADMIN_BIND:ADMIN_PORT (the loopback address unless
#                      the operator names another: ADR-0014's admin/admin)
#   <name>-previous    during an upgrade only: the node it replaces, stopped,
#                      removed once the new one is ready
#   <name>-data1..N    its drives, DRIVES of them (1 to 4)
#   <name>-meta        its metadata directory
#   <name>-secrets     the root credentials, and the KMS passphrase or the
#                      Vault token (PLAN 10.3), mounted read-only
#   <name>-config      dev-init's directory: kms.json, the certificates and
#                      ready (PLAN 10.3), mounted read-only
#
# The node is given every secret as a file (CACTUSFS_*_FILE, and
# CACTUSFS_KMS_KEY_FILE naming the key file of the config volume, so it never
# makes a key file of its own); its environment names paths, never a value.
#
# Install: everything that can refuse is asked first - an unlabelled
# container or volume of the deployment's names, an upgrade left unfinished,
# the drives of a deployment that has run, a port another holds - then the
# image is checked (image_check, PLAN 10.4), still before any change; then
# the image is made (image_make) and the secrets go to the volumes
# (secrets_install, PLAN 10.3); then the image is delivered to the target's
# engine (docker_image_ref), the volumes the node lacks are made, and the
# node is run from RESOLVED_IMAGE with --pull never, ready within
# WAIT_TIMEOUT. Again on a deployment that is installed: an
# upgrade - the node is stopped and renamed <name>-previous, the new one is
# run with the same volumes, and the old one is removed once the new one is
# ready; if it is not, the run says the node is down and how to go back.
# Uninstall removes the container and keeps every volume. A purge deletes,
# in this order, the data volumes, then proves by a listing that none is
# left, then the secrets' volumes (proved the same way), and last the
# deployment's entry of STATE_DIR; a step that fails ends the run with the
# entry kept.
#
# A function that can refuse or fail is never run inside $(...), where its
# exit would end only the subshell: those that answer set variables
# (_dk_*) and are called plainly.

# docker_standalone: true for a deployment this file builds - standalone, on
# Docker Desktop, Docker CE on this machine, or Podman. Docker CE over ssh
# (PLAN 10.7), Rancher Desktop (10.6), cluster mode (10.8) and Kubernetes
# (10.10, 10.11) are other steps'.
docker_standalone() {
	on_kubernetes && return 1
	[ "$(setting DEPLOY_MODE)" = standalone ] || return 1
	case $TARGET in
	dd | podman) return 0 ;;
	ce) [ -z "$(setting CE_HOST)" ] ;;
	*) return 1 ;;
	esac
}

# docker_settings: the settings this file reads, read once, with those of
# secrets.sh (target_engine and the helpers need them).
docker_settings() {
	secrets_settings
	_dk_ctr=$_s_name
	_dk_prev=$_s_name-previous
	_dk_drives=$(setting DRIVES)
	_dk_bind=$(setting BIND)
	_dk_admin_bind=$(setting ADMIN_BIND)
	_dk_s3_port=$(setting S3_PORT)
	_dk_admin_port=$(setting ADMIN_PORT)
	_dk_wait=$(setting WAIT_TIMEOUT)
	_dk_tls_hosts=$(setting TLS_HOSTS)
}

# docker_image_ref: _dk_image, the reference of the image the node runs -
# the only place the Docker targets take it from. Called plainly, never in
# $(...): PLAN 10.4's functions (deploy/lib/image.sh) check host keys and
# set variables. docker_install has run image_check (docker_image_check)
# before its first change and image_make (docker_image_make) before the
# secrets - dev-init may run in the image made; this delivers the image to
# the target's engine (image_deliver) and takes the reference from
# RESOLVED_IMAGE. Without image.sh, IMAGE_SOURCE=image takes IMAGE as
# given, and any other source stops the run as not built yet.
docker_image_ref() {
	_dk_image=
	if command -v image_deliver >/dev/null 2>&1; then
		image_deliver
		_dk_image=${RESOLVED_IMAGE:-}
	elif [ "$(setting IMAGE_SOURCE)" = image ]; then
		_dk_image=$(setting IMAGE)
	else
		not_built 10.4 "the image, $(setting IMAGE_SOURCE): built and put on $(engine) (IMAGE_SOURCE=image with an IMAGE that is there works now)"
	fi
	case $_dk_image in
	'' | -* | *[!A-Za-z0-9_./:@-]*) fail "the image reference for the node is not one: '$(printf '%s' "$_dk_image" | printable)'" ;;
	esac
}

# docker_dial <address>: the address a client on this machine dials for a
# port published on <address> - the loopback address for every address,
# IPv6 in brackets.
docker_dial() {
	case $1 in
	0.0.0.0) printf '127.0.0.1' ;;
	:: | '[::]') printf '[::1]' ;;
	*:*) printf '[%s]' "$1" ;;
	*) printf '%s' "$1" ;;
	esac
}

# docker_loopback <address>: true for an address of this machine only.
docker_loopback() {
	case $1 in
	127.* | ::1) return 0 ;;
	esac
	return 1
}

# docker_publish <address> <port on this machine> <port of the node>: the
# value of --publish.
docker_publish() {
	case $1 in
	*:*) printf '[%s]:%s:%s' "$1" "$2" "$3" ;;
	*) printf '%s:%s:%s' "$1" "$2" "$3" ;;
	esac
}

# docker_tls_name: the name /readyz is asked for over TLS - the first name
# of TLS_HOSTS that is a plain DNS name or IPv4 address (the certificate
# holds it, dev-init's and an operator's alike), else localhost, which
# dev-init's certificates always hold.
docker_tls_name() {
	_tn_ifs=$IFS
	IFS=,
	for _tn in $_dk_tls_hosts; do
		case $_tn in
		*'*'* | *:* | '') ;;
		*)
			IFS=$_tn_ifs
			printf '%s' "$_tn"
			return 0
			;;
		esac
	done
	IFS=$_tn_ifs
	printf 'localhost'
}

# docker_readyz: _dk_ready, the HTTP status /readyz of the admin port
# answers - over TLS, with the deployment's CA from STATE_DIR when it has
# one (dev-init's), else the system's - or 000 when nothing answers. Never
# through a proxy, never without verifying.
docker_readyz() {
	_rz_name=$(docker_tls_name)
	_rz_addr=$(docker_dial "$_dk_admin_bind")
	set -- curl -g -s -o /dev/null --noproxy '*' --max-time 5 -w '%{http_code}'
	if [ -f "$STATE/config/s3-ca.pem" ]; then
		set -- "$@" --cacert "$STATE/config/s3-ca.pem"
	fi
	set -- "$@" --connect-to "$_rz_name:$_dk_admin_port:$_rz_addr:$_dk_admin_port" "https://$_rz_name:$_dk_admin_port/readyz"
	_rz=$(query "$@" </dev/null 2>/dev/null) || :
	_dk_ready=$(printf '%s' "$_rz" | printable | tr -d '\n')
	[ -n "$_dk_ready" ] || _dk_ready=000
}

# docker_wait_ready: true once /readyz answers 200, asked every 2 seconds
# until WAIT_TIMEOUT seconds have passed; false when it never did.
docker_wait_ready() {
	_wr_start=$(date +%s)
	while :; do
		docker_readyz
		[ "$_dk_ready" != 200 ] || return 0
		_wr_now=$(date +%s)
		[ $((_wr_now - _wr_start)) -lt "$_dk_wait" ] || return 1
		sleep 2
	done
}

# docker_find <container>: _dk_found (1 or empty), and when it is there
# _dk_found_owner, _dk_found_target, _dk_found_state and _dk_found_image -
# the container of that exact name, by one listing and one inspect, each of
# which must succeed.
docker_find() {
	_dk_found=
	_dk_found_owner=
	_dk_found_target=
	_dk_found_state=
	_dk_found_image=
	_df_out=$(target_engine query ps -a --filter "name=$1" --format '{{.Names}}' </dev/null) ||
		fail "could not list the containers of $(engine) (its message is above)"
	for _df_n in $(printf '%s\n' "$_df_out" | printable); do
		[ "$_df_n" != "$1" ] || _dk_found=1
	done
	[ -n "$_dk_found" ] || return 0
	_df_out=$(target_engine query container inspect --format "{{index .Config.Labels \"$LABEL_NAME\"}}|{{index .Config.Labels \"$LABEL_TARGET\"}}|{{.State.Status}}|{{.Config.Image}}" "$1" </dev/null) ||
		fail "could not inspect the container $1 (the engine's message is above)"
	_df_out=$(printf '%s\n' "$_df_out" | printable | head -n 1)
	IFS='|' read -r _dk_found_owner _dk_found_target _dk_found_state _dk_found_image <<EOF
$_df_out
EOF
}

# docker_owned <container>: after docker_find, a container that is not the
# deployment's on this target - without its name's label, or another
# target's - is refused: the script never takes over what it did not make.
docker_owned() {
	[ "$_dk_found_owner" = "$_s_name" ] ||
		refuse "a container named $1 is there without the label $LABEL_NAME=$_s_name (its label says '$_dk_found_owner'): the script never takes over what it did not make; remove or rename it, or choose another DEPLOY_NAME"
	[ "$_dk_found_target" = "$TARGET" ] ||
		refuse "the container $1 is the deployment's on the target '$_dk_found_target', not $TARGET: run with --docker $_dk_found_target, or choose another DEPLOY_NAME"
}

# docker_container: what the engine holds of the node's container:
# _dk_have_ctr (1 or empty), _dk_ctr_state and _dk_ctr_image; one that is
# not the deployment's is refused.
docker_container() {
	docker_find "$_dk_ctr"
	_dk_have_ctr=$_dk_found
	_dk_ctr_state=$_dk_found_state
	_dk_ctr_image=$_dk_found_image
	[ -z "$_dk_have_ctr" ] || docker_owned "$_dk_ctr"
}

# docker_previous_check: <name>-previous is there only while an upgrade
# runs. One that is there before an install is an upgrade that did not
# finish: refused, saying how to finish it either way (a container of that
# name that is not the deployment's is refused as such).
docker_previous_check() {
	docker_find "$_dk_prev"
	[ -n "$_dk_found" ] || return 0
	docker_owned "$_dk_prev"
	_pv=$(docker_cli)
	refuse "an upgrade did not finish: $_dk_prev holds the node it replaced (image $_dk_found_image, $_dk_found_state). If $_dk_ctr runs and --status says it is ready, remove the old one: $_pv rm $_dk_prev. Otherwise put it back: $_pv rm -f $_dk_ctr; $_pv rename $_dk_prev $_dk_ctr; $_pv start $_dk_ctr. Then run again"
}

# docker_volumes: the data and metadata volumes of the deployment the engine
# holds, by their exact names - one listing and one inspect, each of which
# must succeed: _dk_vols (their names), _dk_data (how many data volumes),
# _dk_data_max (the highest drive number), _dk_meta (1 when the metadata
# volume is there) and _dk_config (1 when the config volume is there). One of
# those names without the deployment's labels is refused.
docker_volumes() {
	_dk_vols=
	_dk_data=0
	_dk_data_max=0
	_dk_meta=
	_dk_config=
	_dv_out=$(target_engine query volume ls --filter "name=$_s_name-" --format '{{.Name}}' </dev/null) ||
		fail "could not list the volumes of $(engine) (its message is above)"
	for _dv in $(printf '%s\n' "$_dv_out" | printable); do
		case $_dv in
		"$_s_name-config") _dk_config=1 ;;
		"$_s_name-meta") _dk_vols="$_dk_vols $_dv" ;;
		"$_s_name"-data*)
			_dv_n=${_dv#"$_s_name"-data}
			case $_dv_n in
			'' | 0* | *[!0123456789]*) continue ;;
			esac
			_dk_vols="$_dk_vols $_dv"
			;;
		esac
	done
	[ -n "$_dk_vols" ] || return 0
	# shellcheck disable=SC2086 # volume names, without spaces
	_dv_out=$(target_engine query volume inspect --format "{{.Name}} {{index .Labels \"$LABEL_NAME\"}} {{index .Labels \"$LABEL_TARGET\"}}" $_dk_vols </dev/null) ||
		fail "could not inspect the volumes$_dk_vols (the engine's message is above)"
	_dv_out=$(printf '%s\n' "$_dv_out" | printable)
	for _dv in $_dk_vols; do
		_dv_line=$(printf '%s\n' "$_dv_out" | sed -n "s|^$_dv ||p" | head -n 1)
		_dv_owner=${_dv_line%% *}
		_dv_target=
		case $_dv_line in
		*' '*) _dv_target=${_dv_line#* } ;;
		esac
		[ "$_dv_owner" = "$_s_name" ] ||
			refuse "a volume named $_dv is there without the label $LABEL_NAME=$_s_name (its label says '$_dv_owner'): the script never takes over what it did not make; remove or rename it, or choose another DEPLOY_NAME"
		[ "$_dv_target" = "$TARGET" ] ||
			refuse "the volume $_dv is the deployment's on the target '$_dv_target', not $TARGET: run with --docker $_dv_target, or choose another DEPLOY_NAME"
		case $_dv in
		"$_s_name-meta") _dk_meta=1 ;;
		*)
			_dv_n=${_dv#"$_s_name"-data}
			_dk_data=$((_dk_data + 1))
			[ "$_dv_n" -le "$_dk_data_max" ] || _dk_data_max=$_dv_n
			;;
		esac
	done
}

# docker_drives_check: the volumes there agree with the settings. A drive
# beyond DRIVES would be left out of the node, and once the node has run
# (its metadata volume is there) another number of drives is refused: the
# script never changes the drives of a deployment. Data with no record of
# its deployment, or without the key file that opens it, is refused.
docker_drives_check() {
	if [ "$_dk_data_max" -gt "$_dk_drives" ]; then
		refuse "the deployment has the drive volume $_s_name-data$_dk_data_max and DRIVES=$_dk_drives: a drive would be left out; the script never changes the drives of a deployment - set DRIVES back"
	fi
	if [ -n "$_dk_meta" ] && [ "$_dk_data" != "$_dk_drives" ]; then
		refuse "the deployment has run with $_dk_data drive volumes and DRIVES=$_dk_drives: the script never changes the drives of a deployment - set DRIVES=$_dk_data"
	fi
	[ -n "$_dk_vols" ] || return 0
	if [ ! -f "$STATE/target" ] && [ "$ADOPT" != 1 ]; then
		refuse "the deployment's volumes ($(printf '%s' "$_dk_vols" | sed 's/^ //')) are on $(engine), and $STATE holds no record of them: they were made from another machine or another STATE_DIR, or the record was lost; say --adopt to take the deployment's secrets from its volumes into $STATE"
	fi
	if [ -z "$_dk_config" ] && [ ! -f "$STATE/config/ready" ]; then
		refuse "the deployment's data volumes ($(printf '%s' "$_dk_vols" | sed 's/^ //')) are on $(engine), and neither the volume $_s_name-config nor $STATE/config holds the key file and certificates they were made with: new ones would open nothing; put back the entry of STATE_DIR, or purge the deployment"
	fi
}

# docker_ports_check: S3_PORT on BIND and ADMIN_PORT on ADMIN_BIND are free,
# or held by the deployment's own node - asked before anything changes. A
# port another container of the engine publishes is refused naming it; then
# a connection to each port the node does not hold already, through curl
# (never a proxy): one that is answered is refused.
docker_ports_check() {
	# The node's own bindings, while it holds them - running, paused or
	# restarting: "port <container>" says one a line, "9000/tcp ->
	# 127.0.0.1:9000" - never read from ps, whose Ports column groups
	# consecutive ports into a range.
	_pc_mine=
	if [ -n "$_dk_have_ctr" ]; then
		case $_dk_ctr_state in
		running | paused | restarting) _pc_ask=1 ;;
		*) _pc_ask= ;;
		esac
	else
		_pc_ask=
	fi
	if [ -n "$_pc_ask" ]; then
		_pc_mine=$(target_engine query port "$_dk_ctr" </dev/null) ||
			fail "could not ask $(engine) for the ports of $_dk_ctr (its message is above)"
		_pc_mine=$(printf '%s\n' "$_pc_mine" | printable)
	fi
	_pc_out=$(target_engine query ps --format '{{.Names}}|{{.Ports}}' </dev/null) ||
		fail "could not list the containers of $(engine) (its message is above)"
	_pc_out=$(printf '%s\n' "$_pc_out" | printable)
	while IFS='|' read -r _pc_name _pc_ports; do
		[ -n "$_pc_name" ] || continue
		[ "$_pc_name" != "$_dk_ctr" ] || continue
		for _pc_pair in "$_dk_s3_port $_dk_bind" "$_dk_admin_port $_dk_admin_bind"; do
			_pc_port=${_pc_pair%% *}
			if docker_ports_hold "$_pc_port" "$_pc_ports" "${_pc_pair#* }"; then
				refuse "port $_pc_port is published by the container $_pc_name: choose another S3_PORT or ADMIN_PORT, or stop it"
			fi
		done
	done <<EOF
$_pc_out
EOF
	command -v curl >/dev/null 2>&1 ||
		refuse "curl is needed: the script asks with it whether a port is free and whether the node is ready"
	for _pc_pair in "$_dk_bind $_dk_s3_port S3_PORT" "$_dk_admin_bind $_dk_admin_port ADMIN_PORT"; do
		_pc_addr=${_pc_pair%% *}
		_pc_rest=${_pc_pair#* }
		_pc_port=${_pc_rest% *}
		if printf '%s\n' "$_pc_mine" | grep -q ":$_pc_port\$"; then
			continue
		fi
		_pc_dial=$(docker_dial "$_pc_addr")
		_pc_conn=$(query curl -g -s -o /dev/null --noproxy '*' --max-time 3 -w '%{num_connects}' "http://$_pc_dial:$_pc_port/" </dev/null 2>/dev/null) || :
		case $(printf '%s' "$_pc_conn" | printable | tr -d '\n') in
		'' | 0) ;;
		*) refuse "port $_pc_port on $_pc_addr is in use (something answered on $_pc_dial:$_pc_port): choose another ${_pc_rest#* }, or stop what holds it" ;;
		esac
	done
}

# docker_ports_hold <port> <Ports of ps> <bind address>: true when the column
# publishes that TCP port on the bind address checked - "127.0.0.1:9000->9000/tcp",
# or a range of them, "0.0.0.0:9000-9001->9000-9001/tcp", as Docker groups
# consecutive ports. A binding counts when its host address is every address
# (0.0.0.0, ::, [::] or none) or the bind address itself, or when the bind
# address is every address; a UDP or SCTP binding, and a port only exposed
# ("9002/tcp"), publish nothing on it.
docker_ports_hold() {
	_ph_ifs=$IFS
	IFS=,
	for _ph_spec in $2; do
		_ph_spec=${_ph_spec# }
		case $_ph_spec in
		*'->'*) ;;
		*) continue ;;
		esac
		# Another protocol does not hold the TCP port.
		case $_ph_spec in
		*/udp | */sctp) continue ;;
		esac
		_ph_bound=${_ph_spec%%->*}
		_ph_host=${_ph_bound##*:}
		_ph_ip=
		[ "$_ph_host" = "$_ph_bound" ] || _ph_ip=${_ph_bound%:*}
		# A binding on another address of this machine does not hold the
		# port on the address checked - unless either is every address.
		case $_ph_ip in
		'' | 0.0.0.0 | :: | '[::]') ;;
		"$3" | "[$3]") ;;
		*)
			case $3 in
			0.0.0.0 | :: | '[::]') ;;
			*) continue ;;
			esac
			;;
		esac
		_ph_first=${_ph_host%-*}
		_ph_last=${_ph_host#*-}
		case $_ph_first$_ph_last in
		'' | *[!0123456789]*) continue ;;
		esac
		if [ "$1" -ge "$_ph_first" ] && [ "$1" -le "$_ph_last" ]; then
			IFS=$_ph_ifs
			return 0
		fi
	done
	IFS=$_ph_ifs
	return 1
}

# docker_vault_ca: a CA for Vault is not put into the volumes yet, so a
# node on a Docker engine could not trust a Vault whose certificate the
# system's CAs do not.
docker_vault_ca() {
	[ "$_s_kms" = vault ] || return 0
	[ -z "$(setting VAULT_CA_FILE)" ] ||
		refuse "VAULT_CA_FILE is not put into the config volume of a Docker target yet, so the node would not trust that Vault; leave it empty when Vault's certificate is issued by a public CA (the image trusts those)"
}

# docker_run_node <image>: the node's container, from the image, with the
# deployment's volumes and its settings.
docker_run_node() {
	_rn_image=$1
	_rn_root=/var/lib/cactusfs
	# Podman starts containers again after a reboot only through its
	# podman-restart service, which starts those whose policy is "always" -
	# not "unless-stopped" (podman-restart.service: start --all --filter
	# restart-policy=always). Docker keeps "unless-stopped".
	_rn_restart=unless-stopped
	[ "$TARGET" != podman ] || _rn_restart=always
	# The node listens inside on the port numbers it is published on
	# (CACTUSFS_S3_ADDR, CACTUSFS_ADMIN_ADDR below): one number for each
	# port, wherever it is read. The image's EXPOSE (9000 to 9003) is
	# metadata only, and it has no HEALTHCHECK.
	set -- run -d --name "$_dk_ctr" --label "$LABEL_NAME=$_s_name" --label "$LABEL_TARGET=$TARGET" \
		--user 65532:65532 --read-only --cap-drop ALL --security-opt no-new-privileges \
		--restart "$_rn_restart" --stop-timeout 40 --pull never \
		--publish "$(docker_publish "$_dk_bind" "$_dk_s3_port" "$_dk_s3_port")" \
		--publish "$(docker_publish "$_dk_admin_bind" "$_dk_admin_port" "$_dk_admin_port")" \
		--mount "type=volume,source=$_s_name-secrets,target=$_rn_root/secrets,readonly" \
		--mount "type=volume,source=$_s_name-config,target=$_rn_root/config,readonly" \
		--mount "type=volume,source=$_s_name-meta,target=$_rn_root/meta"
	_rn_data=
	_rn_labels=
	_rn_i=1
	while [ "$_rn_i" -le "$_dk_drives" ]; do
		set -- "$@" --mount "type=volume,source=$_s_name-data$_rn_i,target=$_rn_root/data$_rn_i"
		_rn_data="$_rn_data${_rn_data:+,}$_rn_root/data$_rn_i"
		_rn_labels="$_rn_labels${_rn_labels:+,}$_s_name-data$_rn_i"
		_rn_i=$((_rn_i + 1))
	done
	set -- "$@" --env "CACTUSFS_S3_ADDR=:$_dk_s3_port" --env "CACTUSFS_ADMIN_ADDR=:$_dk_admin_port" \
		--env "CACTUSFS_DATA=$_rn_data" --env "CACTUSFS_DRIVE_LABELS=$_rn_labels" \
		--env "CACTUSFS_META_DIR=$_rn_root/meta" \
		--env "CACTUSFS_TLS_CERT=$_rn_root/config/s3.pem" --env "CACTUSFS_TLS_KEY=$_rn_root/config/s3.key" \
		--env "CACTUSFS_ROOT_ACCESS_KEY_FILE=$_rn_root/secrets/root-access-key" \
		--env "CACTUSFS_ROOT_SECRET_KEY_FILE=$_rn_root/secrets/root-secret-key" \
		--env CACTUSFS_DRAIN_DELAY=2s --env CACTUSFS_UPGRADE_MODE=off
	if [ "$_s_kms" = local ]; then
		set -- "$@" --env CACTUSFS_KMS=local \
			--env "CACTUSFS_KMS_KEY_FILE=$_rn_root/config/kms.json" \
			--env "CACTUSFS_KMS_PASSPHRASE_FILE=$_rn_root/secrets/kms-passphrase"
	else
		set -- "$@" --env CACTUSFS_KMS=vault \
			--env "CACTUSFS_VAULT_ADDR=$(setting VAULT_ADDR)" \
			--env "CACTUSFS_VAULT_TRANSIT_MOUNT=$(setting VAULT_TRANSIT_MOUNT)" \
			--env "CACTUSFS_VAULT_KEY=$(setting VAULT_KEY)" \
			--env "CACTUSFS_VAULT_TOKEN_FILE=$_rn_root/secrets/vault-token"
		if [ -n "$(setting VAULT_NAMESPACE)" ]; then
			set -- "$@" --env "CACTUSFS_VAULT_NAMESPACE=$(setting VAULT_NAMESPACE)"
		fi
	fi
	# The settings of the env file for the nodes (env.sh lets through only
	# those deploy/cactusfs.env.example names, never a secret or one of the
	# above).
	while IFS= read -r _rn_line; do
		[ -z "$_rn_line" ] || set -- "$@" --env "$_rn_line"
	done <<EOF
$PASSED
EOF
	set -- "$@" "$_rn_image" server
	target_engine run "$@" </dev/null
}

# docker_down <why>: an install whose node is not running or not ready: the
# run fails, saying so - and on an upgrade that the node is down, that the
# node it replaced is kept, stopped, as <name>-previous, and how to go back
# to it.
docker_down() {
	_dd_cli=$(docker_cli)
	if [ -z "$_dk_have_ctr" ]; then
		fail "the node $_dk_ctr $1; its log: $_dd_cli logs $_dk_ctr"
	fi
	_dd_back="$_dd_cli rm -f $_dk_ctr; $_dd_cli rename $_dk_prev $_dk_ctr; $_dd_cli start $_dk_ctr"
	_dd_boot=
	if [ "$TARGET" = podman ]; then
		_dd_back="$_dd_back; $_dd_cli update --restart always $_dk_ctr (Podman 5.1 or later; else run --install again as below)"
		[ -n "$_dk_prev_cleared" ] ||
			_dd_boot=". $_dk_prev keeps the restart policy always, so podman-restart.service would start it at a reboot"
	fi
	fail "the node is DOWN: the new $_dk_ctr ($_dk_image) $1; its log: $_dd_cli logs $_dk_ctr. The node it replaced is kept, stopped, as $_dk_prev (image $_dk_ctr_image)$_dd_boot. To go back to it: $_dd_back - or remove $_dk_prev and run --install again with IMAGE_SOURCE=image IMAGE=$_dk_ctr_image"
}

# docker_previous_norestart: on Podman, <name>-previous is kept from being
# started at a reboot by podman-restart.service, which starts every
# container whose policy is "always": its policy is set to "no" with
# "podman update --restart no" (Podman 5.1 and later; release notes of
# 5.1.0). An older Podman refuses the flag: the run says so and goes on -
# the old node is stopped and removed as soon as the new one is ready, and
# kept only when it is not (docker_down then says it would start at a
# reboot). Sets _dk_prev_cleared. Docker's "unless-stopped" does not start a
# container that was stopped, so nothing is needed there.
docker_previous_norestart() {
	_dk_prev_cleared=
	[ "$TARGET" = podman ] || {
		_dk_prev_cleared=1
		return 0
	}
	if target_engine run update --restart no "$_dk_prev" </dev/null; then
		_dk_prev_cleared=1
	else
		warn "this Podman cannot clear the restart policy of $_dk_prev (podman update --restart needs Podman 5.1 or later): until it is removed, once the new node is ready, a reboot would start it with podman-restart.service"
	fi
}

# docker_image_check: PLAN 10.4's image_check, before the install's first
# change - its refusals (exit 2) are then true; nothing is built or pulled.
docker_image_check() {
	if command -v image_check >/dev/null 2>&1; then
		# What an earlier run or the environment left is never taken.
		RESOLVED_IMAGE=
		RESOLVED_NATIVE_IMAGE=
		image_check
	fi
}

# docker_image_make: PLAN 10.4's image_make - the image built, pulled and
# checked where it is made, nothing shipped - before the secrets, so that
# dev-init can run in an image of this machine's architecture made from the
# same source (image_native, asked by secrets.sh's devinit_check). The
# secrets' own checks (secrets_check) are asked before it; an image made on
# an engine is not a change of the deployment, so what the secrets refuse
# after it (two copies that differ) still changes nothing of the
# deployment.
docker_image_make() {
	if command -v image_make >/dev/null 2>&1; then
		image_make
	fi
}

# docker_install: --install of a standalone deployment on a Docker engine.
docker_install() {
	docker_settings
	docker_vault_ca
	docker_container
	docker_previous_check
	docker_volumes
	docker_drives_check
	docker_ports_check
	docker_image_check
	if [ -n "$_dk_have_ctr" ]; then
		say "installed: the container $_dk_ctr ($_dk_ctr_state, image $_dk_ctr_image); this run upgrades it, keeping its volumes"
	fi
	# What the secrets refuse is asked before the image is made (a dry run
	# says what it would do instead, in secrets_install).
	[ "$DRY_RUN" = 1 ] || secrets_check

	# Nothing has been changed so far but the images the secrets need (the
	# helper's, dev-init's) when the engine lacked them. The image is made
	# where it is made - built or pulled, and checked; nothing of the
	# deployment - then the secrets, before anything starts on the engine
	# (PLAN 10.3).
	docker_image_make
	secrets_install
	docker_image_ref
	say "image: $_dk_image"

	# The volumes the node lacks, labelled; the metadata volume last, so that
	# its presence says the drives were all made.
	_in_i=1
	while [ "$_in_i" -le "$_dk_drives" ]; do
		case " $_dk_vols " in
		*" $_s_name-data$_in_i "*) ;;
		*) target_engine run volume create --label "$LABEL_NAME=$_s_name" --label "$LABEL_TARGET=$TARGET" "$_s_name-data$_in_i" </dev/null ;;
		esac
		_in_i=$((_in_i + 1))
	done
	[ -n "$_dk_meta" ] ||
		target_engine run volume create --label "$LABEL_NAME=$_s_name" --label "$LABEL_TARGET=$TARGET" "$_s_name-meta" </dev/null

	# An upgrade keeps the node it replaces, stopped and renamed, until the
	# new one is ready.
	if [ -n "$_dk_have_ctr" ]; then
		say "upgrade: stopping $_dk_ctr (image $_dk_ctr_image) and keeping it as $_dk_prev until the new node is ready; its volumes are kept"
		target_engine run stop -t 40 "$_dk_ctr" </dev/null
		if ! target_engine run rename "$_dk_ctr" "$_dk_prev" </dev/null; then
			# The node was stopped and could not be set aside: it is started
			# again, and the run says whether that worked.
			_in_cli=$(docker_cli)
			if target_engine run start "$_dk_ctr" </dev/null; then
				fail "upgrade: $_dk_ctr could not be renamed $_dk_prev (the engine's message is above); it was started again and runs $_dk_ctr_image as before - nothing was upgraded"
			fi
			fail "upgrade: $_dk_ctr could not be renamed $_dk_prev, nor started again (the engine's messages are above): the node is DOWN. Start it with: $_in_cli start $_dk_ctr"
		fi
		docker_previous_norestart
	fi
	if ! docker_run_node "$_dk_image"; then
		docker_down "could not be run (the engine's message is above)"
	fi
	if [ "$DRY_RUN" = 1 ]; then
		say "dry run: would wait up to $_dk_wait s for /readyz of the admin port to answer 200"
		[ -z "$_dk_have_ctr" ] || say "dry run: then would remove $_dk_prev"
		return 0
	fi
	if ! docker_wait_ready; then
		docker_down "did not become ready within WAIT_TIMEOUT=$_dk_wait s (/readyz of https://$(docker_dial "$_dk_admin_bind"):$_dk_admin_port answered $_dk_ready)"
	fi
	if [ -n "$_dk_have_ctr" ]; then
		target_engine run rm "$_dk_prev" </dev/null
		say "upgrade: removed $_dk_prev, the node the new one replaced"
	fi
	docker_reach
	say "ready: $_dk_ctr runs $_dk_image"
	docker_admin_notice
	if [ "$TARGET" = podman ]; then
		say "Podman starts containers again after a reboot only through its podman-restart service, which starts those with the restart policy always - the node has it: systemctl --user enable podman-restart.service, and for a rootless user loginctl enable-linger \"\$USER\" (as root: systemctl enable podman-restart.service)"
	fi
}

# docker_admin_notice: a new deployment has the console user admin with the
# password admin (ADR-0014). Said at every install; warned about when the
# admin port is published beyond this machine.
docker_admin_notice() {
	say "a new deployment has the console user admin with the password admin (ADR-0014): change its password or delete the user before the admin port is reachable from anywhere but this machine"
	docker_loopback "$_dk_admin_bind" ||
		warn "ADMIN_BIND=$_dk_admin_bind publishes the admin API and console beyond this machine: change the password of the console user admin, or delete it, now (ADR-0014)"
}

# docker_cli: the command an operator types to reach the engine, for the
# messages.
docker_cli() {
	if [ "$TARGET" = podman ]; then
		printf 'podman'
	else
		printf 'docker --context %s' "$_s_context"
	fi
}

# docker_reach: where the deployment is reached, and where its CA and root
# credentials are kept (by their files, never their contents).
docker_reach() {
	case $_dk_bind in
	0.0.0.0 | ::) _rc_s3="https://<any address of this machine>:$_dk_s3_port" ;;
	*) _rc_s3="https://$(docker_dial "$_dk_bind"):$_dk_s3_port" ;;
	esac
	say "S3: $_rc_s3 (on this machine: https://$(docker_tls_name):$_dk_s3_port)"
	say "admin API and console: https://$(docker_dial "$_dk_admin_bind"):$_dk_admin_port"
	if [ -f "$STATE/config/s3-ca.pem" ]; then
		say "the CA to trust: $STATE/config/s3-ca.pem"
	fi
	say "root credentials: $STATE/secrets/root-access-key and $STATE/secrets/root-secret-key (never printed)"
}

# docker_cert_expiry: when the S3 certificate of STATE_DIR expires, read by
# openssl when it is installed - only its notAfter is printed (a
# certificate is public; its key is never read).
docker_cert_expiry() {
	_ce=$STATE/config/s3.pem
	if [ ! -f "$_ce" ]; then
		say "certificate: $_ce is not there"
		return 0
	fi
	if ! command -v openssl >/dev/null 2>&1; then
		say "certificate: $_ce (its expiry is shown when openssl is installed)"
		return 0
	fi
	_ce_end=$(openssl x509 -noout -enddate -in "$_ce" 2>/dev/null | printable | sed -n 's/^notAfter=//p' | head -n 1) || _ce_end=
	if [ -n "$_ce_end" ]; then
		say "certificate: $_ce expires $_ce_end"
	else
		say "certificate: $_ce could not be read by openssl"
	fi
}

# docker_labelled_names: _dk_labelled, the volumes of the engine labelled
# with the deployment's name, sorted, separated by spaces. A listing that
# fails fails the run: it is never taken for "none".
docker_labelled_names() {
	_dn_out=$(target_engine query volume ls --filter "label=$LABEL_NAME=$_s_name" --format '{{.Name}}' </dev/null) ||
		fail "could not list the volumes of $(engine) (its message is above)"
	_dk_labelled=$(printf '%s\n' "$_dn_out" | printable | sed '/^$/d' | sort | tr '\n' ' ' | sed 's/ *$//')
}

# docker_labelled [after]: docker_labelled_names, each checked to carry this
# target too: another target's is not this deployment's to delete. Before
# anything is deleted that refuses the run (exit 2); "after" a deletion it
# fails it (exit 1), since something has changed by then.
docker_labelled() {
	docker_labelled_names
	[ -n "$_dk_labelled" ] || return 0
	# shellcheck disable=SC2086 # volume names, without spaces
	_dl_out=$(target_engine query volume inspect --format "{{.Name}} {{index .Labels \"$LABEL_TARGET\"}}" $_dk_labelled </dev/null) ||
		fail "could not inspect the volumes of the deployment (the engine's message is above)"
	_dl_out=$(printf '%s\n' "$_dl_out" | printable)
	for _dl in $_dk_labelled; do
		_dl_target=$(printf '%s\n' "$_dl_out" | sed -n "s|^$_dl ||p" | head -n 1)
		[ "$_dl_target" != "$TARGET" ] || continue
		_dl_why="the volume $_dl carries the deployment's name and the target '$_dl_target', not $TARGET: it is not this deployment's; nothing more is deleted, and $STATE is kept"
		if [ "${1:-}" = after ]; then
			fail "purge: $_dl_why"
		fi
		refuse "$_dl_why"
	done
}

# docker_status: --status - the node's container, its image and version,
# the volumes, where it is reached, when its certificate expires and what
# /readyz says. Read-only. Exits 0 when the node is ready or nothing is
# installed, 1 when it is installed and not ready.
docker_status() {
	docker_settings
	if [ "$TARGET" = podman ]; then
		_st_format="{{.Names}}|{{.State}}|{{.Image}}|{{index .Labels \"$LABEL_TARGET\"}}|{{index .Labels \"org.opencontainers.image.version\"}}"
	else
		_st_format="{{.Names}}|{{.State}}|{{.Image}}|{{.Label \"$LABEL_TARGET\"}}|{{.Label \"org.opencontainers.image.version\"}}"
	fi
	_st_out=$(target_engine query ps -a --filter "label=$LABEL_NAME=$_s_name" --format "$_st_format" </dev/null) ||
		fail "could not list the containers of $(engine) (its message is above)"
	_st_line=$(printf '%s\n' "$_st_out" | printable | grep "^$_dk_ctr|" | head -n 1) || :
	docker_labelled_names
	if [ -z "$_st_line" ]; then
		say "$_s_name is not installed on $(engine)"
		[ -z "$_dk_labelled" ] || say "its volumes are kept: $_dk_labelled (--install runs it again with them; --uninstall --purge deletes them)"
		return 0
	fi
	IFS='|' read -r _st_name _st_state _st_image _st_target _st_version <<EOF
$_st_line
EOF
	[ "$_st_target" = "$TARGET" ] ||
		refuse "the container $_st_name is the deployment's on the target '$_st_target', not $TARGET: run with --docker $_st_target"
	say "node: $_st_name, $_st_state, image $_st_image, version ${_st_version:-unknown}"
	say "volumes: ${_dk_labelled:-none}"
	docker_reach
	docker_cert_expiry
	docker_readyz
	say "/readyz: $_dk_ready"
	[ "$_dk_ready" = 200 ] || {
		say "not ready: its log: $(docker_cli) logs $_dk_ctr"
		exit 1
	}
}

# docker_engine_check <purge|uninstall>: the engine is the one the
# deployment was recorded on (PLAN 10.3's record): nothing of a deployment
# is removed through another engine that happens to answer to the same
# settings. A purge of a deployment with no record is refused unless
# --adopt says the labelled objects on this engine are its own; then only
# the engine's ID is asked here (_dk_adopt), and the record is written by
# docker_adopt_record once what is labelled has been found and checked.
docker_engine_check() {
	_dk_adopt=
	if [ ! -f "$STATE/target" ]; then
		[ "$1" = purge ] || return 0
		[ "$ADOPT" = 1 ] ||
			refuse "$STATE holds no record of $_s_name, so nothing says $(engine) is the engine it was made on: a purge deletes only a deployment recorded there. If the volumes labelled $LABEL_NAME=$_s_name on $(engine) are this deployment's, say --adopt"
		engine_id_ask
		_dk_adopt=1
		return 0
	fi
	engine_id_ask
	_ec_recorded=$(sed -n 's/^engine-id=//p' "$STATE/target" | head -n 1 | printable)
	[ "$_engine_id" = "$_ec_recorded" ] ||
		refuse "$STATE was made on the engine '$_ec_recorded', and the settings ($(engine_identity)) now reach another, '$_engine_id': nothing of the deployment is removed through it; set the settings back"
}

# docker_adopt_record: with --adopt, after the container, the node an
# upgrade kept and the labelled volumes have been found and checked, and
# before the first of them is stopped or removed: the record naming the
# engine reached is written, so a purge cut short leaves it. When the
# engine holds nothing labelled with the deployment's name, the run is
# refused and STATE kept: the settings may reach another engine than the
# one the deployment is on.
docker_adopt_record() {
	[ -n "$_dk_adopt" ] || return 0
	if [ -z "$_dk_have_ctr" ] && [ -z "$_dk_have_prev" ] && [ -z "$_dk_labelled" ]; then
		refuse "--adopt: $(engine) (engine $_engine_id) holds no container or volume labelled $LABEL_NAME=$_s_name; the settings may reach another engine than the deployment's. Nothing was deleted, and $STATE is kept"
	fi
	if [ "$DRY_RUN" = 1 ]; then
		say "dry run: would record $STATE/target ($(engine_identity), engine $_engine_id, KMS=$_s_kms) before the purge (--adopt)"
		return 0
	fi
	umask 077
	mkdir -p "$STATE"
	chmod 0700 "$STATE" "${STATE%/*}"
	{
		engine_identity
		echo
		printf 'engine-id=%s\nkms=%s\n' "$_engine_id" "$_s_kms"
	} | put_new "$STATE/target"
	say "state: --adopt: recorded $STATE/target ($(engine_identity), engine $_engine_id, KMS=$_s_kms)"
}

# docker_purge_confirm: a purge goes on only with the deployment's name -
# from --purge-confirm (deploy.sh has compared it), or typed at a terminal.
docker_purge_confirm() {
	[ -z "$PURGE_CONFIRM" ] || return 0
	if [ "$DRY_RUN" = 1 ]; then
		say "dry run: a purge asks for the name of the deployment, $_s_name, at the terminal (or takes --purge-confirm=$_s_name)"
		return 0
	fi
	[ -t 0 ] ||
		refuse "--purge deletes the data and the keys of $_s_name for good: type its name when asked at a terminal, or say --purge-confirm=$_s_name; nothing was deleted"
	printf 'deploy: --purge deletes every volume of %s on %s and its secrets in %s, for good. Type its name to go on: ' "$_s_name" "$(engine)" "$STATE" >&2
	_pp_answer=
	IFS= read -r _pp_answer || :
	[ "$_pp_answer" = "$_s_name" ] ||
		refuse "that is not the name of the deployment, $_s_name: nothing was deleted"
}

# docker_uninstall: --uninstall, and with --purge the data and the secrets,
# in the order of ADR-0033 section 3. Everything is asked before anything
# is removed: the engine, the container, the node an unfinished upgrade
# kept (name and target labels both), and for a purge the labelled volumes.
docker_uninstall() {
	docker_settings
	_dk_labelled=
	if [ "$PURGE" = 1 ]; then
		docker_purge_confirm
		docker_engine_check purge
	else
		docker_engine_check uninstall
	fi
	docker_container
	# The node an unfinished upgrade kept and the helpers a run left behind:
	# found and checked by both labels now, removed after the node.
	_dk_have_prev=
	_un_extra=
	for _un in "$_dk_prev" "$_s_name-volumes" "$_s_name-dev-init"; do
		docker_find "$_un"
		[ -n "$_dk_found" ] || continue
		docker_owned "$_un"
		_un_extra="$_un_extra $_un"
		[ "$_un" != "$_dk_prev" ] || _dk_have_prev=1
	done
	if [ "$PURGE" = 1 ]; then
		# A labelled volume of another target refuses the purge.
		docker_labelled
		docker_adopt_record
	fi
	if [ -n "$_dk_have_ctr" ]; then
		target_engine run stop -t 40 "$_dk_ctr" </dev/null
		target_engine run rm "$_dk_ctr" </dev/null
		say "removed the container $_dk_ctr"
	else
		say "no container $_dk_ctr on $(engine)"
	fi
	for _un in $_un_extra; do
		target_engine run rm -f "$_un" </dev/null
		if [ "$_un" = "$_dk_prev" ]; then
			say "removed $_dk_prev, the node an unfinished upgrade kept"
		else
			say "removed $_un, a helper a run left behind"
		fi
	done
	if [ "$PURGE" = 0 ]; then
		docker_labelled_names
		say "kept: the volumes ${_dk_labelled:-(none)} and $STATE: --install runs the deployment again with them"
		return 0
	fi
	docker_purge
}

# docker_purge: the data volumes; a listing that proves none is left; the
# secrets' volumes; a listing that proves no volume of the deployment is
# left; and last the deployment's entry of STATE_DIR, and nothing else of
# it. A step that fails - a listing or an inspect included - ends the run,
# and the entry - the keys - is kept.
docker_purge() {
	# _dk_labelled: docker_uninstall listed and checked them before it
	# removed anything.
	_pg_data=
	_pg_keys=
	for _pg in $_dk_labelled; do
		case $_pg in
		"$_s_name-secrets" | "$_s_name-config") _pg_keys="$_pg_keys $_pg" ;;
		*) _pg_data="$_pg_data $_pg" ;;
		esac
	done
	if [ -n "$_pg_data" ]; then
		# shellcheck disable=SC2086 # volume names, without spaces
		target_engine run volume rm $_pg_data </dev/null
		say "purge: deleted the data volumes$_pg_data"
	fi
	if [ "$DRY_RUN" = 0 ]; then
		docker_labelled after
		for _pg in $_dk_labelled; do
			case $_pg in
			"$_s_name-secrets" | "$_s_name-config") ;;
			*) fail "purge: the volume $_pg of the deployment is still there after it was deleted; its secrets and $STATE are kept" ;;
			esac
		done
		say "purge: no data volume of $_s_name is left on $(engine)"
	fi
	if [ -n "$_pg_keys" ]; then
		# shellcheck disable=SC2086 # volume names, without spaces
		target_engine run volume rm $_pg_keys </dev/null
		say "purge: deleted the secrets' volumes$_pg_keys"
	fi
	if [ "$DRY_RUN" = 0 ]; then
		docker_labelled after
		[ -z "$_dk_labelled" ] ||
			fail "purge: the volumes $_dk_labelled of the deployment are still there; $STATE is kept"
		say "purge: no volume of $_s_name is left on $(engine)"
	fi
	if [ -d "$STATE" ]; then
		case $STATE in
		*/"$_s_name") ;;
		*) fail "internal: $STATE is not a deployment's entry" ;;
		esac
		run rm -rf -- "$STATE"
		if [ "$DRY_RUN" = 0 ]; then
			rmdir "${STATE%/*}" 2>/dev/null || :
		fi
		say "purge: deleted $STATE"
	fi
	say "purged: $_s_name and its secrets are gone from $(engine) and from this machine"
}

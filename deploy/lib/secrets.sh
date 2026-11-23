# shellcheck shell=sh
# The secrets of a deployment, and its two copies of them (PLAN 10.3,
# ADR-0033 section 3). Sourced by deploy/deploy.sh; not run on its own.
#
# A deployment begins with its root credentials; on the local KMS the
# passphrase and the sealed key file kms.json, which open the data only
# together; on Vault a token instead of both; and its certificates and their
# keys. Every one is kept twice:
#
#   here, in the deployment's entry of STATE_DIR (directories 0700, files
#   0600), STATE below:
#     STATE/target                   the record of the deployment: the
#                                    settings' engine (engine_identity), the
#                                    engine's own ID (engine-id=) and the KMS
#                                    it was made with (kms=)
#     STATE/secrets/root-access-key  the root credentials, generated from
#     STATE/secrets/root-secret-key  /dev/urandom unless ROOT_*_FILE
#     STATE/secrets/kms-passphrase   KMS=local: generated, or KMS_PASSPHRASE_FILE
#     STATE/secrets/vault-token      KMS=vault: VAULT_TOKEN_FILE
#     STATE/config/                  what "cactusfs dev-init" wrote - kms.json
#                                    (KMS=local), s3.pem, s3.key, s3-ca.pem,
#                                    node-ca.pem, nodes/, ready - with the
#                                    operator's own files in their place
#                                    (TLS_*_FILE, KMS_KEY_FILE)
#
#   and on the target, before anything there starts - on a Docker engine
#   (dd, ce here or on CE_HOST, podman), standalone, the two labelled named
#   volumes <name>-secrets and <name>-config, holding the same files; the
#   node is given them as CACTUSFS_*_FILE and CACTUSFS_KMS_KEY_FILE (PLAN
#   10.5), so it never makes a key file of its own. In cluster mode on a
#   Docker engine (PLAN 10.8, deploy/lib/compose.sh) the config unit is the
#   same labelled volume <name>-config, which compose mounts as its "config"
#   volume (so its init service finds it complete and writes nothing), and
#   the secrets unit is the directory of compose's file secrets,
#   compose/secrets/ of the entry - or of CE_STATE_DIR on CE_HOST - (0700,
#   its files 0444: bind mounts, ADR-0033 section 10), read back and put
#   as tar streams like a volume. On Rancher Desktop (PLAN 10.6) the copy is
#   the same two volumes, on its moby engine through the Docker CLI and on
#   its containerd engine through nerdctl (deploy/lib/rancher.sh: a put
#   there is a tar stream owned by 65532, nerdctl's cp having no -a). On
#   Kubernetes (PLAN 10.10) the target's copy is the Secrets of the
#   deployment's namespace, read back and compared by deploy/lib/k8s.sh
#   (k8s_secrets_ensure, PLAN 10.9), what neither copy holds made here
#   (secrets_ensure_k8s). Rancher Desktop's Kubernetes keeps its copy as
#   10.6 builds; secrets_where and secrets_install are where they are
#   dispatched.
#
# Never made again: a secret either copy holds is used as it is. One copy
# missing is put back from the other - the target's from STATE before
# anything starts there, STATE's from the target, saying so - and two copies
# that differ are refused, naming both: the script never chooses. Only what
# neither copy holds is made: the root credentials and the passphrase from
# /dev/urandom, kms.json and the certificates by "cactusfs dev-init" on this
# machine, in a container of its own architecture, its input and output tar
# streams. A put onto the target writes only names the target lacks.
#
# Given as a file: the root credentials, the passphrase and kms.json must
# equal every copy there is, or the run is refused - a new one would open
# nothing. A Vault token or a certificate of the operator's (TLS_*_FILE)
# that differs is a rotation: it replaces both copies, the target's first,
# then STATE's, and the run says what it rotated.
#
# The secrets of one deployment belong to one engine: another engine behind
# the same settings is refused, unless --engine-replaced says the recorded
# one is gone (a factory reset) and this one takes its place. Volumes of the
# deployment on the target with no record of them in STATE are taken into
# STATE only with --adopt.
#
# No secret is ever in a shell variable, on a command line or printed: each
# goes from a file to a file, or to a command's standard input, by a
# redirection, and copies are compared by cmp. What is said of a secret is
# its file's name.

# secrets_where: "volumes" when this file keeps the target's copy - a
# deployment on a Docker engine or Rancher Desktop's engine, standalone or
# a compose cluster (PLAN 10.8: its secrets unit a directory, its config
# unit a volume) - or "kubernetes" for the Secrets of a namespace (PLAN
# 10.10). Every target is one of the two.
secrets_where() {
	if on_kubernetes; then
		echo kubernetes
	else
		case $TARGET in
		dd | ce | podman | rancher) echo volumes ;;
		*) fail "internal: no place for the secrets of the target $TARGET" ;;
		esac
	fi
}

# secrets_settings: the settings this file reads, read once (setting forks
# a shell each time it is asked).
secrets_settings() {
	_s_name=$(setting DEPLOY_NAME)
	_s_kms=$(setting KMS)
	_s_root_id=$(setting ROOT_ACCESS_KEY_FILE)
	_s_root_key=$(setting ROOT_SECRET_KEY_FILE)
	_s_phrase=$(setting KMS_PASSPHRASE_FILE)
	_s_keyfile=$(setting KMS_KEY_FILE)
	_s_token=$(setting VAULT_TOKEN_FILE)
	_s_cert=$(setting TLS_CERT_FILE)
	_s_tlskey=$(setting TLS_KEY_FILE)
	_s_ca=$(setting TLS_CA_FILE)
	_s_offline=$(setting OFFLINE)
	_s_source=$(setting IMAGE_SOURCE)
	_s_image=$(setting IMAGE)
	_s_rel=$(setting RELEASE_DIR)
	_s_hosts=$(setting TLS_HOSTS)
	_s_validity=$(setting TLS_VALIDITY)
	_s_context=$(docker_context)
	_s_ce_host=
	[ "$TARGET" != ce ] || _s_ce_host=$(setting CE_HOST)
	_s_mode=$(setting DEPLOY_MODE)
	# In cluster mode the secrets unit is the directory of compose's file
	# secrets on the machine compose runs on: compose.sh sets _cp_dir, the
	# deployment's compose directory there, before it calls anything here.
	_s_secdir=
	if [ "$_s_mode" = cluster ] && ! on_kubernetes; then
		[ -n "${_cp_dir:-}" ] || fail "internal: cluster mode without its compose directory (compose_settings)"
		_s_secdir=$_cp_dir/secrets
	fi
	# The node IDs dev-init issues certificates for: the standalone node of
	# a Docker engine is the deployment's name, a compose cluster's nodes
	# are compose.yaml's node1 to node4, and the chart names its nodes
	# <name>-<pool>-<index> (k8s_node_ids).
	_s_nodes=$_s_name
	[ "$_s_mode" != cluster ] || _s_nodes=node1,node2,node3,node4
	# The engine here, where dev-init runs: Podman for the podman target,
	# and for a Kubernetes target when there is no Docker CLI here (as
	# image.sh chooses); else the Docker CLI with its context.
	_s_local=docker
	if [ "$TARGET" = podman ]; then
		_s_local=podman
	elif on_kubernetes; then
		_s_nodes=$(k8s_node_ids | tr '\n' ',' | sed 's/,$//')
		if ! command -v docker >/dev/null 2>&1 && command -v podman >/dev/null 2>&1; then
			_s_local=podman
		fi
	fi
}

# unit_place <unit>: where the target keeps a unit, for the messages - its
# labelled volume, or for the secrets of a compose cluster the directory of
# compose's file secrets.
unit_place() {
	if [ "$1" = secrets ] && [ -n "$_s_secdir" ]; then
		if [ -n "$_s_ce_host" ]; then
			printf "compose's secret files %s on %s" "$_s_secdir" "$_s_ce_host"
		else
			printf "compose's secret files %s" "$_s_secdir"
		fi
	else
		printf 'the volume %s-%s' "$_s_name" "$1"
	fi
}

# secdir_exists: true when the directory of compose's secret files is
# there - asked of CE_HOST over ssh, where "test -d" answers 0 or 1 and
# anything else fails the run (an error is never taken for "not there").
secdir_exists() {
	if [ -z "$_s_ce_host" ]; then
		[ -d "$_s_secdir" ]
		return
	fi
	_se_status=0
	query on_host "$_s_ce_host" test -d "$_s_secdir" </dev/null || _se_status=$?
	case $_se_status in
	0) return 0 ;;
	1) return 1 ;;
	*) fail "could not ask $_s_ce_host whether $_s_secdir is there (exit $_se_status; ssh's message is above)" ;;
	esac
}

# secdir_fetch <tar file>: the directory of compose's secret files as a tar
# stream of "secrets", into the file - on CE_HOST through ssh's standard
# output.
secdir_fetch() {
	if [ -z "$_s_ce_host" ]; then
		tar -c -f "$1" -C "${_s_secdir%/*}" secrets
	else
		query on_host "$_s_ce_host" tar -c -f - -C "${_s_secdir%/*}" secrets </dev/null >"$1" ||
			fail "could not read $(unit_place secrets) (ssh's message is above)"
	fi
}

# secdir_make: the directory of compose's secret files, made with the
# directories above it of the deployment's entry, each 0700 - on CE_HOST
# those below CE_STATE_DIR.
secdir_make() {
	if [ -z "$_s_ce_host" ]; then
		mkdir -p "$_s_secdir"
		chmod 0700 "$_s_secdir" "${_s_secdir%/*}"
		return 0
	fi
	run on_host "$_s_ce_host" mkdir -p "$_s_secdir" </dev/null
	_sm_compose=${_s_secdir%/*}
	_sm_entry=${_sm_compose%/*}
	run on_host "$_s_ce_host" chmod 0700 "${_sm_entry%/*}" "$_sm_entry" "$_sm_compose" "$_s_secdir" </dev/null
}

# secdir_put <tar file> <name>...: the names of the tar stream extracted
# into the directory of compose's file secrets, and each file made 0444:
# the nodes run as 65532 and a bind-mounted file keeps its owner and mode
# (ADR-0033 section 10); the 0700 directory keeps them the operator's.
secdir_put() {
	_dp_tar=$1
	shift
	_dp_files=
	for _dp_name in "$@"; do
		[ "$_dp_name" != . ] || continue
		[ -d "$STATE/secrets/$_dp_name" ] || _dp_files="$_dp_files $_dp_name"
	done
	if [ -z "$_s_ce_host" ]; then
		tar -x -o -f "$_dp_tar" -C "$_s_secdir"
		for _dp_name in $_dp_files; do
			chmod 0444 "$_s_secdir/$_dp_name"
		done
		chmod 0700 "$_s_secdir"
		return 0
	fi
	run on_host "$_s_ce_host" tar -x -o -f - -C "$_s_secdir" <"$_dp_tar"
	set --
	for _dp_name in $_dp_files; do
		set -- "$@" "$_s_secdir/$_dp_name"
	done
	[ $# -eq 0 ] || run on_host "$_s_ce_host" chmod 0444 "$@" </dev/null
	run on_host "$_s_ce_host" chmod 0700 "$_s_secdir" </dev/null
}

# target_engine <run|query> <word>...: a command of the target's engine -
# Podman, nerdctl in its namespace (Rancher Desktop on containerd), the
# Docker engine of CE_HOST over ssh (its host key checked first), or the
# Docker CLI with its context.
target_engine() {
	_te_how=$1
	shift
	if [ "$TARGET" = podman ]; then
		"$_te_how" podman "$@"
	elif on_nerdctl; then
		"$_te_how" nerdctl --namespace "$_rd_ns" "$@"
	elif [ -n "$_s_ce_host" ]; then
		"$_te_how" "DOCKER_HOST=ssh://$_s_ce_host" docker "$@"
	else
		"$_te_how" docker --context "$_s_context" "$@"
	fi
}

# local_engine <run|query> <word>...: a command of the engine of this
# machine, where dev-init runs: the target's own, but for CE_HOST, whose
# engine is another machine's (then the Docker CLI with its context here).
local_engine() {
	_le_how=$1
	shift
	if [ "$_s_local" = podman ]; then
		"$_le_how" podman "$@"
	elif on_nerdctl; then
		"$_le_how" nerdctl --namespace "$_rd_ns" "$@"
	else
		"$_le_how" docker --context "$_s_context" "$@"
	fi
}

# runtime_image: the distroless base the product image is built on, as the
# Dockerfile pins it by digest.
runtime_image() {
	_ri=$(sed -n 's/^ARG RUNTIME_IMAGE=//p' "$REPO_ROOT/Dockerfile" 2>/dev/null | head -n 1)
	case $_ri in
	*@sha256:*) ;;
	*) fail "the Dockerfile of $REPO_ROOT names no RUNTIME_IMAGE pinned by digest (ARG RUNTIME_IMAGE=...@sha256:...)" ;;
	esac
	case $_ri in
	-* | *[!A-Za-z0-9_./:@-]*) fail "the Dockerfile's RUNTIME_IMAGE is not an image reference" ;;
	esac
	printf '%s' "$_ri"
}

# helper_image: the image the helper containers are made from (never
# started): IMAGE with IMAGE_SOURCE=image - the product image, on the same
# base and with the same user, which the target needs anyway - else the
# runtime image.
helper_image() {
	if [ "$_s_source" = image ]; then
		printf '%s' "$_s_image"
	else
		runtime_image
	fi
}

# image_ready <engine> <image> [platform]: the image is on the engine before
# a container is made from it with --pull never: asked with "image
# inspect", and pulled when it is not there - one of the fetches of
# ADR-0033 section 12 - unless OFFLINE=yes, which refuses instead.
image_ready() {
	_ir_status=0
	# nerdctl finds <repository>@<digest> by "image inspect" only when it is
	# tagged latest (deploy/lib/image.sh, image_given_ref): it is asked for
	# the bare digest there.
	_ir_ref=$2
	if on_nerdctl; then
		case $_ir_ref in
		*@sha256:*) _ir_ref=sha256:${_ir_ref##*@sha256:} ;;
		esac
	fi
	"$1" query image inspect --format '{{.Id}}' "$_ir_ref" </dev/null >/dev/null 2>&1 || _ir_status=$?
	[ "$_ir_status" != 0 ] || return 0
	[ "$_s_offline" != yes ] ||
		refuse "the image $2 is not on the engine and OFFLINE=yes: load it there first (docker load or podman load of a saved copy)"
	if [ -n "${3:-}" ]; then
		"$1" run pull --platform "$3" "$2" </dev/null
	else
		"$1" run pull "$2" </dev/null
	fi
}

# native_arch: the architecture of this machine, as images name it.
native_arch() {
	_na=$(uname -m | printable)
	case $_na in
	x86_64 | amd64) echo amd64 ;;
	arm64 | aarch64) echo arm64 ;;
	*) refuse "this machine is $_na: dev-init runs in a container of this machine's own architecture, amd64 or arm64, never emulated" ;;
	esac
}

# sha256_line <file>: "<hash>  <file>", as sha256sum writes it.
sha256_line() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1"
	else
		shasum -a 256 "$1"
	fi
}

# helper_clear <engine> <container>: a helper container left by a run that
# stopped half way is removed - only one the script made. The engine's
# listing, which must succeed, is matched by exact name (an error is never
# taken for "not there"); one of that name without the deployment's labels -
# its name and this target - is refused: the script never takes over what it
# did not make.
helper_clear() {
	_hc_out=$("$1" query ps -a --filter "name=$2" --format '{{.Names}}' </dev/null) ||
		fail "could not list the containers of the engine (its message is above)"
	_hc_found=
	for _hc_name in $(printf '%s\n' "$_hc_out" | printable); do
		[ "$_hc_name" != "$2" ] || _hc_found=1
	done
	[ -n "$_hc_found" ] || return 0
	_hc_owner=$("$1" query container inspect --format "{{index .Config.Labels \"$LABEL_NAME\"}}|{{index .Config.Labels \"$LABEL_TARGET\"}}" "$2" </dev/null) ||
		fail "could not inspect the container $2 (the engine's message is above)"
	_hc_owner=$(printf '%s' "$_hc_owner" | printable | tr -d '\n')
	_hc_target=${_hc_owner#*|}
	[ "$_hc_target" != "$_hc_owner" ] || _hc_target=
	_hc_owner=${_hc_owner%%|*}
	[ "$_hc_owner" = "$_s_name" ] ||
		refuse "a container named $2 is there without the label $LABEL_NAME=$_s_name (its label says '$_hc_owner'): the script never takes over what it did not make; remove or rename it, or choose another DEPLOY_NAME"
	[ "$_hc_target" = "$TARGET" ] ||
		refuse "the container $2 is the deployment's on the target '$_hc_target', not $TARGET: the script never takes over what it did not make; run with --docker $_hc_target, or remove it"
	"$1" run rm -f "$2" </dev/null
}

# put_new <file> [length]: standard input into a new file of mode 0600,
# which must not be there yet: written beside it, its length checked when
# one is given, and linked into place - never half written, never written
# over.
put_new() {
	_pn_tmp=$(mktemp "${1%/*}/.new.XXXXXX")
	cat >"$_pn_tmp"
	chmod 0600 "$_pn_tmp"
	if [ -n "${2:-}" ] && [ "$(wc -c <"$_pn_tmp" | tr -d ' ')" != "$2" ]; then
		rm -f "$_pn_tmp"
		fail "could not write $2 characters into $1"
	fi
	if ! ln "$_pn_tmp" "$1" 2>/dev/null; then
		rm -f "$_pn_tmp"
		fail "$1 is there already, and a secret is never written over"
	fi
	rm -f "$_pn_tmp"
}

# copy_new <from> <to>: a copy of a file, as put_new writes one.
copy_new() {
	mkdir -p "${2%/*}"
	put_new "$2" <"$1"
}

# replace_file <from> <to>: the file at <to> replaced by a copy of <from>,
# written beside it and renamed into place. Only for a rotation and the
# record of a replaced engine.
replace_file() {
	mkdir -p "${2%/*}"
	_rf_tmp=$(mktemp "${2%/*}/.new.XXXXXX")
	cat <"$1" >"$_rf_tmp"
	chmod 0600 "$_rf_tmp"
	mv -f "$_rf_tmp" "$2"
}

# generate <file> <characters> <length>: a new secret of that many
# characters drawn from /dev/urandom, every one of the set equally likely
# (tr keeps the bytes of the set and drops the rest).
generate() {
	mkdir -p "${1%/*}"
	tr -dc "$2" </dev/urandom | head -c "$3" | put_new "$1" "$3"
}

# files_of <directory>: the files under it, relative to it, one a line -
# never a name beginning with a dot (the script's own temporary files).
# Only names the script and dev-init give are there: no space in any.
files_of() {
	[ -d "$1" ] || return 0
	(CDPATH='' cd -- "$1" && find . -name '.?*' -prune -o -type f -print) | sed 's|^\./||' | sort
}

# only_plain <directory> <what>: what was taken out of a tar stream holds
# nothing but regular files and directories - no link, device or fifo.
only_plain() {
	[ -z "$(find "$1" ! -type f ! -type d)" ] ||
		refuse "$2 holds something that is neither a file nor a directory (a link, a device or a fifo): nothing of it is used"
}

# modes <directory>: directories 0700, files 0600, below it and itself.
modes() {
	find "$1" -type d -exec chmod 0700 {} +
	find "$1" -type f -exec chmod 0600 {} +
}

# The places of the run: the target's copies as they were fetched
# ($SECRETS_WORK/target/<unit>), the files staged for an engine, dev-init's
# output. A directory of the entry (0700), removed when the run ends however
# it ends - and with it the directories of the entry that this run made and
# left empty.
SECRETS_WORK=

# have <unit> <file>: either copy holds the file.
have() {
	[ -f "$STATE/$1/$2" ] || [ -f "$SECRETS_WORK/target/$1/$2" ]
}

# rotation_given <unit>/<file>: sets _rg to the file given that rotates it -
# the Vault token, or the operator's certificate, key and CA (with their
# certificate only, as given_check reads them) - and _rk to its key; both
# empty when none is given.
rotation_given() {
	_rk=
	_rg=
	case $1 in
	secrets/vault-token) _rk=VAULT_TOKEN_FILE _rg=$_s_token ;;
	config/s3.pem) _rk=TLS_CERT_FILE _rg=$_s_cert ;;
	config/s3.key) _rk=TLS_KEY_FILE _rg=$_s_tlskey ;;
	config/s3-ca.pem) _rk=TLS_CA_FILE _rg=$_s_ca ;;
	esac
	case $1 in
	config/*) [ -n "$_s_cert" ] || _rg= ;;
	esac
	[ -n "$_rg" ] || _rk=
}

# rotated <unit>/<file>: true when a file given for it is a rotation.
rotated() {
	case " $_si_rotate " in
	*" $1="*) return 0 ;;
	esac
	return 1
}

# secrets_plan: what --dry-run says of the secrets: which local copies are
# there (by their names), and what a run would do. Nothing is read from a
# file and nothing is asked of the target.
secrets_plan() {
	if [ "$(setting DEPLOY_MODE)" = cluster ]; then
		say "secrets: kept in $STATE (directories 0700, files 0600) and on $(engine) in the labelled volume $(setting DEPLOY_NAME)-config and compose's secret files (compose/secrets/, files 0444), put there before anything starts"
	else
		say "secrets: kept in $STATE (directories 0700, files 0600) and on $(engine) in the labelled volumes $(setting DEPLOY_NAME)-secrets and $(setting DEPLOY_NAME)-config, put there before anything starts"
	fi
	for _sp in secrets/root-access-key secrets/root-secret-key secrets/kms-passphrase secrets/vault-token config/kms.json config/ready; do
		if [ -f "$STATE/$_sp" ]; then
			say "secrets: $STATE/$_sp is there and is used as it is"
		fi
	done
	say "secrets: what neither copy holds is made once - the root credentials and the passphrase from /dev/urandom, kms.json and the certificates by cactusfs dev-init in a container of this machine's architecture - and two copies that differ are refused"
}

# secrets_check: what secrets_install asks before anything is written or
# made - the files given, the engine's ID against the record, the volumes
# of the target and their labels, the helper's image, and when dev-init
# will run (_si_devinit) what it needs - each refusal (exit 2) before any
# change. Asked once: docker_install asks it before PLAN 10.4's image_make,
# so that it refuses before an image is built or pulled, and secrets_install
# asks it again (nothing then). Under --from-source dev-init runs in the
# image image_make makes: secrets_install asks devinit_check after it.
secrets_check() {
	[ -z "${_sc_done:-}" ] || return 0
	_sc_done=1
	umask 077
	secrets_settings
	_si_rotate=
	for _si_key in TLS_CERT_FILE TLS_CA_FILE; do
		if [ "$_si_key" = TLS_CERT_FILE ]; then _si_file=$_s_cert; else _si_file=$_s_ca; fi
		if [ -n "$_si_file" ] && { [ ! -f "$_si_file" ] || [ ! -r "$_si_file" ]; }; then
			bad "$_si_key" "is not a file that can be read"
		fi
	done
	if [ -n "$_s_ce_host" ]; then
		host_key_check "$_s_ce_host"
	fi
	engine_id_check

	# What the target holds: one listing, which must succeed, of the volumes
	# whose names hold the deployment's - matched exactly here, never in
	# part - and one inspect of those there, each refused without the
	# deployment's label.
	_si_have=
	_si_out=$(target_engine query volume ls --filter "name=$_s_name-" --format '{{.Name}}' </dev/null) ||
		fail "could not list the volumes of the engine (its message is above)"
	_si_found=
	for _si_vol in $(printf '%s\n' "$_si_out" | printable); do
		case $_si_vol in
		"$_s_name-config") _si_found="$_si_found $_si_vol" ;;
		"$_s_name-secrets") [ -n "$_s_secdir" ] || _si_found="$_si_found $_si_vol" ;;
		esac
	done
	if [ -n "$_si_found" ]; then
		# shellcheck disable=SC2086 # volume names, without spaces
		_si_out=$(target_engine query volume inspect --format "{{.Name}} $(rancher_volume_label "$LABEL_NAME")" $_si_found </dev/null) ||
			fail "could not inspect the volumes$_si_found (the engine's message is above)"
		_si_out=$(printf '%s\n' "$_si_out" | printable)
		for _si_vol in $_si_found; do
			_si_owner=$(printf '%s\n' "$_si_out" | sed -n "s|^$_si_vol ||p" | head -n 1)
			[ "$_si_owner" = "$_s_name" ] ||
				refuse "a volume named $_si_vol is there without the label $LABEL_NAME=$_s_name (its label says '$_si_owner'): the script never takes over what it did not make; remove or rename it, or choose another DEPLOY_NAME"
			_si_have="$_si_have ${_si_vol#"$_s_name"-}"
		done
	fi
	# A compose cluster's secrets unit is a directory, there or not.
	if [ -n "$_s_secdir" ] && secdir_exists; then
		_si_have="$_si_have secrets"
	fi
	if [ -n "$_si_have" ] && [ ! -f "$STATE/target" ] && [ "$ADOPT" != 1 ]; then
		if [ -n "$_s_secdir" ]; then
			_si_what=$(for _u in $_si_have; do
				unit_place "$_u"
				printf ', '
			done | sed 's/, $//')
		else
			_si_what="the deployment's volumes ($(for _u in $_si_have; do printf '%s ' "$_s_name-$_u"; done | sed 's/ $//'))"
		fi
		refuse "$_si_what are on $(engine), and $STATE holds no record of them: they were made from another machine or another STATE_DIR, or the record was lost; say --adopt to take the secrets they hold into $STATE"
	fi
	_si_img=$(helper_image)
	image_ready target_engine "$_si_img"
	# dev-init is asked for what it needs first when it will run: neither
	# copy of the certificates is there, and no file gives all they hold.
	_si_devinit=
	case "$_si_have " in
	*" config "*) ;;
	*)
		if [ ! -f "$STATE/config/ready" ] && [ "$(secrets_devinit)" = yes ]; then
			_si_devinit=1
		fi
		;;
	esac
	[ -z "$_si_devinit" ] || [ "$_s_source" = source ] || devinit_check
}

# secrets_install: every secret of the deployment in both of its places,
# before anything starts on the target. Run by --install on a target whose
# copy secrets_where says this step keeps. Everything that refuses is asked
# before STATE is written: first secrets_check, then what needs the copy of
# the target read.
secrets_install() {
	if [ "$(secrets_where)" = kubernetes ]; then
		secrets_install_k8s
		return 0
	fi
	if [ "$DRY_RUN" = 1 ]; then
		secrets_plan
		return 0
	fi
	secrets_check
	# Under --from-source dev-init runs in the image PLAN 10.4's image_make
	# made, so it is asked for what it needs only now.
	[ -z "$_si_devinit" ] || devinit_check

	secrets_begin
	secrets_fetch "$_si_have"
	# Every copy is compared before anything is copied or made.
	for _si_unit in secrets config; do
		_si_local=$STATE/$_si_unit
		_si_there=$SECRETS_WORK/target/$_si_unit
		for _si_file in $(files_of "$_si_there"); do
			[ -f "$_si_local/$_si_file" ] || continue
			cmp -s "$_si_local/$_si_file" "$_si_there/$_si_file" && continue
			# A rotation stopped half way - the volume's copy replaced by the
			# file given, the local one not yet - is finished by that file.
			rotation_given "$_si_unit/$_si_file"
			if [ -n "$_rg" ] && cmp -s "$_rg" "$_si_there/$_si_file"; then
				continue
			fi
			refuse "$_si_local/$_si_file and $_si_file in $(unit_place "$_si_unit") differ: two copies of one secret, and the script never chooses between them (ADR-0033 section 3); find out which one the deployment uses and remove the other, then run again"
		done
	done
	given_check "$_s_root_id" ROOT_ACCESS_KEY_FILE secrets root-access-key keep
	given_check "$_s_root_key" ROOT_SECRET_KEY_FILE secrets root-secret-key keep
	given_check "$_s_phrase" KMS_PASSPHRASE_FILE secrets kms-passphrase keep
	given_check "$_s_keyfile" KMS_KEY_FILE config kms.json keep
	given_check "$_s_token" VAULT_TOKEN_FILE secrets vault-token rotate
	if [ -n "$_s_cert" ] && have config ready; then
		given_check "$_s_cert" TLS_CERT_FILE config s3.pem rotate
		given_check "$_s_tlskey" TLS_KEY_FILE config s3.key rotate
		given_check "$_s_ca" TLS_CA_FILE config s3-ca.pem rotate
	fi
	secrets_whole

	# Nothing is refused from here on: STATE is written.
	state_record
	for _si_unit in secrets config; do
		_si_local=$STATE/$_si_unit
		_si_there=$SECRETS_WORK/target/$_si_unit
		for _si_file in $(files_of "$_si_there"); do
			[ ! -f "$_si_local/$_si_file" ] || continue
			! rotated "$_si_unit/$_si_file" || continue
			copy_new "$_si_there/$_si_file" "$_si_local/$_si_file"
			warn "the local copy $_si_local/$_si_file was missing: copied back from $(unit_place "$_si_unit")"
		done
	done
	secrets_make
	config_make
	# A compose cluster's nodes hold a rotated file only once they are made
	# again: the rotation is marked as pending first, and the marker goes
	# only when every node has been made again and is ready (compose.sh).
	if [ -n "$_s_secdir" ] && [ -n "$_si_rotate" ]; then
		recreate_pending_mark
	fi
	secrets_put "$_si_have"
	for _si_entry in $_si_rotate; do
		rotation_given "${_si_entry%%=*}"
		replace_file "$_rg" "$STATE/${_si_entry%%=*}"
		say "secrets: rotated $_rk: $STATE/${_si_entry%%=*} replaced, after the volume's copy"
	done
	cleanup_drop secrets_end "$SECRETS_WORK" "$STATE" "$_sb_top"
	secrets_end "$SECRETS_WORK" "$STATE" "$_sb_top"
	SECRETS_WORK=
	if [ -n "$_s_secdir" ]; then
		say "secrets: $STATE, $(unit_place secrets) and $(unit_place config) hold the same secrets"
	else
		say "secrets: $STATE and the volumes $_s_name-secrets and $_s_name-config hold the same secrets"
	fi
}

# engine_id_ask: _engine_id, the engine's own ID - "docker info"'s ID, or
# for Podman its host and the root of its store - asked of the target's
# engine, with only printable characters. An engine that cannot be asked,
# or says nothing, fails the run. Shared by engine_id_check and the Docker
# targets' purge (docker_engine_check, deploy/lib/docker.sh); called
# plainly, never in a command substitution.
engine_id_ask() {
	if on_kubernetes; then
		k8s_cluster_id
		return 0
	fi
	if [ "$TARGET" = podman ]; then
		_eia=$(target_engine query info --format '{{.Host.Hostname}}:{{.Store.GraphRoot}}' </dev/null) ||
			fail "could not ask $(engine) who it is (its message is above)"
	else
		_eia=$(target_engine query info --format '{{.ID}}' </dev/null) ||
			fail "could not ask $(engine) who it is (its message is above)"
	fi
	_engine_id=$(printf '%s' "$_eia" | printable | tr -d '\n')
	[ -n "$_engine_id" ] || fail "$(engine) did not say its ID"
}

# engine_id_check: the engine's own ID - "docker info"'s ID, or for Podman
# its host and the root of its store (a weak identifier: it tells another
# store apart, not two machines of one name) - asked of the target's engine
# (for CE_HOST, the server's, over ssh). It is said, and recorded with the
# deployment. A later install that meets another engine behind the same
# settings is refused: the secrets of one deployment are never given to a
# second engine. --engine-replaced says the recorded engine is gone - a
# factory reset - and this one takes its place: the record then names this
# one (state_record). A KMS other than the one recorded is refused.
engine_id_check() {
	engine_id_ask
	if on_kubernetes; then
		_eic_what=cluster
		say "cluster ID (the UID of its namespace kube-system): $_engine_id"
	else
		_eic_what=engine
		say "engine ID: $_engine_id"
	fi
	_engine_rerecord=
	[ -f "$STATE/target" ] || return 0
	_eic_recorded=$(sed -n 's/^engine-id=//p' "$STATE/target" | head -n 1 | printable)
	if [ "$_eic_recorded" != "$_engine_id" ]; then
		[ "$ENGINE_REPLACED" = 1 ] ||
			refuse "$STATE was made on the $_eic_what '$_eic_recorded', and the settings ($(engine_identity)) now reach another, '$_engine_id': the secrets of one deployment are never given to a second $_eic_what. Set the settings back; or, if the $_eic_what '$_eic_recorded' is gone and this one replaced it (a factory reset, a cluster made again), say --engine-replaced"
		warn "--engine-replaced: the deployment made on the $_eic_what '$_eic_recorded' is taken as gone; its secrets in $STATE now belong to the $_eic_what '$_engine_id', and are put there"
		_engine_rerecord=1
	fi
	_eic_kms=$(sed -n 's/^kms=//p' "$STATE/target" | head -n 1 | printable)
	[ "$_eic_kms" = "$_s_kms" ] ||
		refuse "$STATE was made with KMS=$_eic_kms, and the settings now say KMS=$_s_kms: the script does not move a deployment to another KMS"
	state_mode_check
}

# state_mode_check: the deployment's mode is the one its record names - a
# line "mode=cluster" for a compose cluster (PLAN 10.8), none for a
# standalone node, as every record of 10.3 and 10.5 is. The script never
# turns one into the other: their volumes, and the nodes the data was
# written by, differ.
state_mode_check() {
	[ -f "$STATE/target" ] || return 0
	_smc=$(sed -n 's/^mode=//p' "$STATE/target" | head -n 1 | printable)
	[ -n "$_smc" ] || _smc=standalone
	[ "$_smc" = "$(setting DEPLOY_MODE)" ] ||
		refuse "$STATE was made with DEPLOY_MODE=$_smc, and the settings now say DEPLOY_MODE=$(setting DEPLOY_MODE): the script does not turn a deployment into the other mode; set DEPLOY_MODE back, or give the new one another DEPLOY_NAME"
}

# state_lines: the lines of the record after the engine's identity - its
# ID, the KMS, and for a compose cluster its mode.
# On CE_HOST a cluster's record also names the server's compose directory
# (compose.sh's _cp_rbase and _cp_dir), which a later CE_STATE_DIR must
# keep.
state_lines() {
	printf 'engine-id=%s\nkms=%s\n' "$_engine_id" "$_s_kms"
	[ "$(setting DEPLOY_MODE)" != cluster ] || printf 'mode=cluster\n'
	[ "$(setting DEPLOY_MODE)" != cluster ] || [ -z "${_cp_rbase:-}" ] || printf 'compose-dir=%s\n' "$_cp_dir"
}

# secrets_begin: the deployment's entry of STATE_DIR and the directory of
# its target, only the operator's (0700), and the run's directory in it.
# What this run makes of them and leaves empty is removed at its end
# (secrets_end), so a run refused after this point leaves no entry behind.
secrets_begin() {
	_sb_top=
	_sb_path=$STATE
	while [ -n "$_sb_path" ] && [ ! -d "$_sb_path" ]; do
		_sb_top=$_sb_path
		_sb_path=${_sb_path%/*}
	done
	mkdir -p "$STATE"
	chmod 0700 "$STATE" "${STATE%/*}"
	SECRETS_WORK=$(mktemp -d "$STATE/.work.XXXXXX")
	# The paths the cleanup works on are written into the run's one EXIT
	# trap now, quoted, not read when it fires (cleanup_add, common.sh):
	# nothing a function does in between - by assigning a variable of the
	# same name - can point it elsewhere.
	cleanup_add secrets_end "$SECRETS_WORK" "$STATE" "$_sb_top"
}

# secrets_end <run's directory> <entry> <first directory this run made, or
# ''>: the run's directory removed - only a .work. directory of the entry,
# whatever it is given - and the directories of the entry this run made,
# when they are empty. Its variables are its own (_send_*).
secrets_end() {
	_send_work=$1
	_send_entry=$2
	_send_top=$3
	case $_send_work in
	"$_send_entry"/.work.*) rm -rf "$_send_work" ;;
	esac
	[ -n "$_send_top" ] || return 0
	_send_path=$_send_entry
	while rmdir "$_send_path" 2>/dev/null; do
		[ "$_send_path" != "$_send_top" ] || break
		_send_path=${_send_path%/*}
	done
	return 0
}

# state_record: the record of the deployment, written into its entry at the
# first install - the settings' engine (engine_identity, which state_check
# in env.sh compares on every run), the engine's ID and the KMS - and
# written again, naming the engine that replaced the recorded one, with
# --engine-replaced.
state_record() {
	if [ -f "$STATE/target" ]; then
		[ -n "$_engine_rerecord" ] || return 0
		{
			head -n 1 "$STATE/target"
			state_lines
		} >"$SECRETS_WORK/target.new"
		replace_file "$SECRETS_WORK/target.new" "$STATE/target"
		say "state: recorded the engine $_engine_id in $STATE/target, in place of the one it replaced"
		return 0
	fi
	{
		engine_identity
		echo
		state_lines
	} | put_new "$STATE/target"
	say "state: recorded $STATE/target ($(engine_identity), engine $_engine_id, KMS=$_s_kms)"
}

# secrets_fetch "<unit>...": the target's copy of each unit whose volume is
# there, read through a helper container that mounts it and is never
# started, as a tar stream - and checked to hold only files and directories.
secrets_fetch() {
	mkdir "$SECRETS_WORK/target"
	[ -n "$1" ] || return 0
	_sf_ctr=$_s_name-volumes
	_sf_vols=
	for _sf_unit in $1; do
		[ "$_sf_unit" = secrets ] && [ -n "$_s_secdir" ] && continue
		_sf_vols="$_sf_vols $_sf_unit"
	done
	# shellcheck disable=SC2086 # unit names, without spaces
	[ -z "$_sf_vols" ] || io_container "$_sf_ctr" $_sf_vols
	for _sf_unit in $1; do
		if [ "$_sf_unit" = secrets ] && [ -n "$_s_secdir" ]; then
			secdir_fetch "$SECRETS_WORK/fetch.tar"
		else
			target_engine query cp "$_sf_ctr:/var/lib/cactusfs/$_sf_unit" - </dev/null >"$SECRETS_WORK/fetch.tar"
			[ "$TARGET" != podman ] || volume_root_check "$SECRETS_WORK/fetch.tar" "$_s_name-$_sf_unit"
		fi
		tar -x -o -f "$SECRETS_WORK/fetch.tar" -C "$SECRETS_WORK/target"
		rm -f "$SECRETS_WORK/fetch.tar"
		[ -d "$SECRETS_WORK/target/$_sf_unit" ] || mkdir "$SECRETS_WORK/target/$_sf_unit"
	done
	[ -z "$_sf_vols" ] || target_engine run rm -f "$_sf_ctr" </dev/null
	only_plain "$SECRETS_WORK/target" "a volume of the deployment"
}

# volume_root_check <tar> <volume>: on Podman, the root of a secrets'
# volume read out by the helper (the first entry of the tar stream, a
# directory) must be owned by the nodes' user, 65532: a volume made before
# secrets_volume_create gave it that owner kept the root root:root 0700
# when the helper's image has no directory where it is mounted (Podman's cp
# into a container never started gives the root no owner), and the node
# cannot open it. Such a volume is refused, with the way back; it is not
# changed (the helper runs as 65532, without the capability to chown). The
# owner is read from the entry's ustar header (bytes 108-114, octal; the
# type at byte 156), with od; a stream whose first entry is not a directory - an
# extended header first - is not judged.
volume_root_check() {
	_vrc_type=$(od -A n -t c -j 156 -N 1 "$1" | tr -d ' \n') || _vrc_type=
	[ "$_vrc_type" = 5 ] || return 0
	# Seven octal digits; the eighth byte is their end (NUL or space).
	_vrc_uid=$(od -A n -t c -j 108 -N 7 "$1" | tr -cd '0-7' | sed 's/^0*//')
	[ "$_vrc_uid" != 177774 ] || return 0
	refuse "the root of the volume $2 is owned by the user $(printf '%d' "0${_vrc_uid:-0}"), not by the nodes' user 65532 (a Podman volume made by an earlier run of this script, whose helper's copy gave its root no owner): the node cannot open it. To make it again: deploy/deploy.sh --uninstall, then podman volume rm $_s_name-secrets $_s_name-config, then deploy/deploy.sh --install, which puts the secrets back from $STATE - the data volumes are kept"
}

# secrets_volume_create <unit>: the volume <name>-<unit>, labelled with the
# deployment's name and target. On Podman it is owned by the nodes' user from
# its creation (--opt o=uid=65532,gid=65532: podman v5.4.0
# pkg/domain/infra/abi/parse/parse.go VolumeOptions maps uid= and gid= to
# WithVolumeUID/GID and WithVolumeNoChown, and libpod
# runtime_volume_common.go chowns the volume's _data to them when it makes
# it; volume_internal.go needsMount mounts nothing for those options alone):
# the helper is never started, and Podman's "cp" into such a container sets
# no owner on the volume's root, which stays root:root 0700 - the node, as
# 65532, could not open it. On Docker the root takes the owner of the
# helper's image directory when it has one (the copy-up into an empty
# volume), and "cp -a"'s "." entry sets it otherwise.
secrets_volume_create() {
	if [ "$TARGET" = podman ]; then
		target_engine run volume create --opt o=uid=65532,gid=65532 --label "$LABEL_NAME=$_s_name" --label "$LABEL_TARGET=$TARGET" "$_s_name-$1" </dev/null
	else
		target_engine run volume create --label "$LABEL_NAME=$_s_name" --label "$LABEL_TARGET=$TARGET" "$_s_name-$1" </dev/null
	fi
}

# io_container <container> <unit>...: a helper container on the target that
# mounts the volume of each unit at /var/lib/cactusfs/<unit> - named volumes
# only, never a path of a host - made from helper_image as the nodes' user and
# never started: what is copied in with "cp -a" is owned by that user (65532).
# The product image (IMAGE_SOURCE=image) has those directories, owned by
# 65532 (the Dockerfile's volumes stage): on Docker an empty volume first
# mounted there takes their owner and mode. The runtime image, the helper's
# for binaries and source, has none: Docker's "cp -a" gives the root to
# 65532 from the stream's "." entry; Podman's does not, so on Podman the
# volume is made owned by 65532 (secrets_volume_create).
io_container() {
	_io_ctr=$1
	shift
	helper_clear target_engine "$_io_ctr"
	_io_mounts=
	for _io_unit in "$@"; do
		_io_mounts="$_io_mounts --mount type=volume,source=$_s_name-$_io_unit,target=/var/lib/cactusfs/$_io_unit"
	done
	# shellcheck disable=SC2086 # the mounts are words without spaces
	target_engine run create --name "$_io_ctr" --label "$LABEL_NAME=$_s_name" --label "$LABEL_TARGET=$TARGET" \
		--user 65532:65532 --pull never --network none --cap-drop ALL --security-opt no-new-privileges $_io_mounts "$_si_img" true </dev/null
}

# given_check <file given> <key> <unit> <file> <keep|rotate>: a file given
# for a secret against every copy there is. "keep": one that differs is
# refused. "rotate": one that differs is noted, and replaces both copies.
given_check() {
	[ -n "$1" ] || return 0
	_gc_old=
	if [ -f "$STATE/$3/$4" ] && ! cmp -s "$1" "$STATE/$3/$4"; then
		_gc_old="$STATE/$3/$4"
	elif [ -f "$SECRETS_WORK/target/$3/$4" ] && ! cmp -s "$1" "$SECRETS_WORK/target/$3/$4"; then
		_gc_old="$4 in $(unit_place "$3")"
	fi
	[ -n "$_gc_old" ] || return 0
	if [ "$5" = rotate ]; then
		_si_rotate="$_si_rotate $3/$4=$2"
		return 0
	fi
	refuse "$2 ($1) and $_gc_old differ: the deployment was made with the one it has, which a new one would not open; the script never replaces it - give the file it was made with, or none"
}

# secrets_whole: what the copies hold together is whole, or the run is
# refused before anything is made - a part made again beside what is there
# would never match it.
secrets_whole() {
	_sw_half=
	if have secrets root-access-key && ! have secrets root-secret-key && [ -z "$_s_root_key" ]; then
		_sw_half=1
	fi
	if have secrets root-secret-key && ! have secrets root-access-key && [ -z "$_s_root_id" ]; then
		_sw_half=1
	fi
	if [ -n "$_sw_half" ]; then
		refuse "one of the root credentials is in $STATE/secrets or $(unit_place secrets) and the other is in neither: the deployment began with both; give them with ROOT_ACCESS_KEY_FILE and ROOT_SECRET_KEY_FILE"
	fi
	# (A KMS_KEY_FILE always comes with its KMS_PASSPHRASE_FILE: env.sh.)
	if [ "$_s_kms" = local ] && ! have secrets kms-passphrase && [ -z "$_s_phrase" ] && have config kms.json; then
		refuse "the key file kms.json is there and its passphrase is in neither $STATE/secrets nor $(unit_place secrets): a new passphrase would open nothing; give the one it was made with in KMS_PASSPHRASE_FILE"
	fi
	if [ -n "$(files_of "$STATE/config")$(files_of "$SECRETS_WORK/target/config")" ] && ! have config ready; then
		refuse "the certificates and key file in $STATE/config and the volume $_s_name-config are not complete (no 'ready': a run stopped while making them): nothing in them is made again; if nothing was ever started with them, remove both and run again"
	fi
	if [ "$_s_kms" = local ] && have config ready && ! have config kms.json && [ -z "$_s_keyfile" ]; then
		refuse "the certificates in $STATE/config or the volume $_s_name-config are complete, and kms.json is in neither: the deployment's objects are sealed under a key file a new one would not open; give the one it was made with in KMS_KEY_FILE"
	fi
	if rotated config/s3.pem && [ -z "$_s_ca" ] && have config s3-ca.pem; then
		refuse "TLS_CERT_FILE replaces the certificate, and s3-ca.pem, the CA of the certificate it replaces, would be kept and handed to clients: give the CA that issued the new certificate in TLS_CA_FILE"
	fi
}

# secrets_make: the secrets neither copy holds and no file gives, made
# once. A secret given as a file is copied into STATE.
secrets_make() {
	_sm=$STATE/secrets
	mkdir -p "$_sm"
	for _sm_pair in "$_s_root_id:root-access-key:ROOT_ACCESS_KEY_FILE" "$_s_root_key:root-secret-key:ROOT_SECRET_KEY_FILE" \
		"$_s_phrase:kms-passphrase:KMS_PASSPHRASE_FILE" "$_s_token:vault-token:VAULT_TOKEN_FILE"; do
		_sm_key=${_sm_pair##*:}
		_sm_rest=${_sm_pair%:*}
		_sm_file=${_sm_rest##*:}
		_sm_given=${_sm_rest%:*}
		if [ -n "$_sm_given" ] && [ ! -f "$_sm/$_sm_file" ] && ! rotated "secrets/$_sm_file"; then
			copy_new "$_sm_given" "$_sm/$_sm_file"
			say "secrets: took $_sm_key into $_sm/$_sm_file"
		fi
	done
	if [ ! -f "$_sm/root-access-key" ]; then
		# The access key ID in the alphabet of iam.GenerateAccessKeyID; the
		# secret key in letters and digits, 62 characters (iam.GenerateSecret
		# draws from 64, with + and /, which a file read by hand trips over).
		generate "$_sm/root-access-key" 'A-Z2-7' 20
		generate "$_sm/root-secret-key" 'A-Za-z0-9' 40
		say "secrets: made the root credentials, $_sm/root-access-key and $_sm/root-secret-key (never printed: read them there)"
	fi
	if [ "$_s_kms" = local ] && [ ! -f "$_sm/kms-passphrase" ]; then
		generate "$_sm/kms-passphrase" 'A-Za-z0-9' 48
		say "secrets: made the KMS passphrase, $_sm/kms-passphrase - back up $STATE: kms.json opens nothing without it"
	fi
	modes "$_sm"
}

# secrets_devinit: "yes" when dev-init makes something the deployment
# keeps - the certificates, when TLS_CERT_FILE does not give them, or
# kms.json on the local KMS, when KMS_KEY_FILE does not.
secrets_devinit() {
	if [ -n "${_s_devinit_all:-}" ]; then
		# On Kubernetes, a configuration neither copy holds is dev-init's
		# whole: the node certificates of a cluster are in it too.
		echo yes
	elif [ -z "$_s_cert" ]; then
		echo yes
	elif [ "$_s_kms" = local ] && [ -z "$_s_keyfile" ]; then
		echo yes
	else
		echo no
	fi
}

# config_make: the certificates and kms.json, when neither copy holds them:
# what dev-init writes, with the operator's own files in their place, and
# "ready" last. Moved into STATE whole. When they are there, a kms.json
# that neither copy holds is taken from KMS_KEY_FILE (secrets_whole has
# refused the run without one).
config_make() {
	if [ -f "$STATE/config/ready" ]; then
		if [ "$_s_kms" = local ] && [ ! -f "$STATE/config/kms.json" ]; then
			copy_new "$_s_keyfile" "$STATE/config/kms.json"
			say "secrets: took KMS_KEY_FILE into $STATE/config/kms.json"
		fi
		return 0
	fi
	_cm=$SECRETS_WORK/config
	if [ "$(secrets_devinit)" = yes ]; then
		devinit_run "$_cm"
	else
		mkdir "$_cm"
	fi
	if [ -n "$_s_cert" ]; then
		rm -f "$_cm/s3.pem" "$_cm/s3.key" "$_cm/s3-ca.pem"
		copy_new "$_s_cert" "$_cm/s3.pem"
		copy_new "$_s_tlskey" "$_cm/s3.key"
		[ -z "$_s_ca" ] || copy_new "$_s_ca" "$_cm/s3-ca.pem"
	fi
	if [ "$_s_kms" = local ]; then
		if [ -n "$_s_keyfile" ]; then
			rm -f "$_cm/kms.json"
			copy_new "$_s_keyfile" "$_cm/kms.json"
		fi
	else
		# Vault wraps the keys: dev-init's key file, made with a passphrase
		# that was never kept, is not the deployment's.
		rm -f "$_cm/kms.json"
	fi
	[ -f "$_cm/ready" ] || date -u '+%Y-%m-%dT%H:%M:%SZ' | put_new "$_cm/ready"
	modes "$_cm"
	[ ! -d "$STATE/config" ] || rmdir "$STATE/config"
	mv "$_cm" "$STATE/config"
	if [ "$_s_kms" = local ]; then
		say "secrets: made $STATE/config (the certificates and kms.json)"
	else
		say "secrets: made $STATE/config (the certificates)"
	fi
}

# devinit_check: what dev-init needs, asked before anything is written: an
# image to run it in of this machine's architecture - the binary of make
# image-binaries, checked against SHA256SUMS and BUILDINFO, in the runtime
# image, IMAGE, or under --from-source RESOLVED_NATIVE_IMAGE (image_native,
# PLAN 10.4) - on this machine's engine, and no container in the way
# of its own (one the script left, when a run stopped half way, is removed;
# another is refused).
devinit_check() {
	[ -z "${_dc_done:-}" ] || return 0
	_dc_done=1
	_dc_arch=$(native_arch)
	case $_s_source in
	binaries)
		[ -f "$_s_rel/linux-$_dc_arch/cactusfs" ] && [ -f "$_s_rel/SHA256SUMS" ] && [ -f "$_s_rel/BUILDINFO" ] ||
			refuse "no binary for this machine in $_s_rel (linux-$_dc_arch/cactusfs, with SHA256SUMS and BUILDINFO): run make image-binaries; dev-init runs from it, in a container of this machine's architecture"
		grep -qx 'packaging=image' "$_s_rel/BUILDINFO" ||
			refuse "$_s_rel/BUILDINFO does not say packaging=image: those are not the binaries of make image-binaries"
		_dc_sum=$(CDPATH='' cd -- "$_s_rel" && sha256_line "linux-$_dc_arch/cactusfs")
		grep -qxF "$_dc_sum" "$_s_rel/SHA256SUMS" ||
			refuse "$_s_rel/linux-$_dc_arch/cactusfs does not match $_s_rel/SHA256SUMS: run make image-binaries again"
		_dc_img=$(runtime_image)
		;;
	image) _dc_img=$_s_image ;;
	*)
		# From source: an image of this machine's architecture, made by
		# PLAN 10.4's image_native from the same tree, after image_make -
		# which the install of a standalone deployment on a Docker engine
		# runs (docker_install); a target whose install does not make the
		# image yet stops here.
		command -v image_native >/dev/null 2>&1 && [ -n "${RESOLVED_IMAGE:-}" ] ||
			not_built 10.4 "the image of this machine's architecture that dev-init runs in, built from source"
		[ -n "${RESOLVED_NATIVE_IMAGE:-}" ] || image_native
		_dc_img=$RESOLVED_NATIVE_IMAGE
		;;
	esac
	image_ready local_engine "$_dc_img" "linux/$_dc_arch"
	helper_clear local_engine "$_s_name-dev-init"
}

# devinit_run <directory>: "cactusfs dev-init" in a container of this
# machine's architecture - the binary of make image-binaries in the runtime
# image, IMAGE, or under --from-source the image image_native made (PLAN
# 10.4) - with the passphrase and the binary copied in as a tar
# stream and its directory copied out as one; no path of this machine is
# mounted. Given --nodes (the deployment's name, the standalone node's),
# --hosts (TLS_HOSTS) and --validity (TLS_VALIDITY). What it prints is
# printed with only printable characters.
devinit_run() {
	_dr_to=$1
	_dr_arch=$(native_arch)
	_dr_ctr=$_s_name-dev-init
	_dr_in=$SECRETS_WORK/dev-init-in
	mkdir "$_dr_in"
	if [ "$_s_kms" = local ] && [ -f "$STATE/secrets/kms-passphrase" ]; then
		copy_new "$STATE/secrets/kms-passphrase" "$_dr_in/kms-passphrase"
	elif [ "$_s_kms" = local ]; then
		# On Kubernetes a passphrase given as a file is read from it
		# (deploy/lib/k8s.sh, k8s_secret_file).
		copy_new "$_s_phrase" "$_dr_in/kms-passphrase"
	else
		# dev-init always seals a key file; with Vault it is thrown away.
		generate "$_dr_in/kms-passphrase" 'A-Za-z0-9' 48
	fi
	devinit_check
	if [ "$_s_source" = binaries ]; then
		# shellcheck disable=SC2046 # the options of rancher_tar_owner, words without spaces
		tar -c -f "$SECRETS_WORK/dev-init.tar" $(rancher_tar_owner) -C "$_s_rel/linux-$_dr_arch" cactusfs -C "$_dr_in" kms-passphrase
		_dr_img=$(runtime_image)
		set -- "$_dr_img" /tmp/cactusfs
	else
		# IMAGE, or under --from-source the image of this machine's
		# architecture image_native made: devinit_check's choice.
		# shellcheck disable=SC2046 # the options of rancher_tar_owner, words without spaces
		tar -c -f "$SECRETS_WORK/dev-init.tar" $(rancher_tar_owner) -C "$_dr_in" kms-passphrase
		set -- --entrypoint /cactusfs "$_dc_img"
	fi
	rm -f "$_dr_in/kms-passphrase"
	set -- "$@" dev-init --dir /tmp/out --nodes "$_s_nodes" --validity "$_s_validity" --kms-passphrase-file /tmp/kms-passphrase
	[ -z "$_s_hosts" ] || set -- "$@" --hosts "$_s_hosts"
	local_engine run create --name "$_dr_ctr" --label "$LABEL_NAME=$_s_name" --label "$LABEL_TARGET=$TARGET" \
		--platform "linux/$_dr_arch" --user 65532:65532 --pull never --network none --cap-drop ALL --security-opt no-new-privileges "$@" </dev/null ||
		fail "making the container $_dr_ctr failed (the engine's message is above)"
	# nerdctl's cp has no -a: the stream is owned by 65532 (rancher_tar_owner).
	if on_nerdctl; then
		local_engine run cp - "$_dr_ctr:/tmp" <"$SECRETS_WORK/dev-init.tar" ||
			fail "copying the passphrase into $_dr_ctr failed (the engine's message is above)"
	else
		local_engine run cp -a - "$_dr_ctr:/tmp" <"$SECRETS_WORK/dev-init.tar" ||
			fail "copying the passphrase into $_dr_ctr failed (the engine's message is above)"
	fi
	rm -f "$SECRETS_WORK/dev-init.tar"
	{
		local_engine run start -a "$_dr_ctr" </dev/null 2>&1 || echo failed >"$SECRETS_WORK/dev-init.failed"
	} | printable
	[ ! -f "$SECRETS_WORK/dev-init.failed" ] || fail "dev-init failed in its container (what it said is above)"
	local_engine query cp "$_dr_ctr:/tmp/out" - </dev/null >"$SECRETS_WORK/dev-init.tar" ||
		fail "copying what dev-init wrote out of $_dr_ctr failed (the engine's message is above)"
	local_engine run rm -f "$_dr_ctr" </dev/null || fail "removing the container $_dr_ctr failed (the engine's message is above)"
	tar -x -o -f "$SECRETS_WORK/dev-init.tar" -C "$_dr_in" || fail "what dev-init wrote is not a tar stream"
	rm -f "$SECRETS_WORK/dev-init.tar"
	only_plain "$_dr_in" "what dev-init gave back"
	[ -f "$_dr_in/out/ready" ] && [ -f "$_dr_in/out/s3.pem" ] ||
		fail "dev-init wrote no complete directory (no ready or s3.pem in what it gave back)"
	mv "$_dr_in/out" "$_dr_to"
	rm -rf "$_dr_in"
}

# put_list <unit> <volume made in this run: 0|1>: what STATE holds of a
# unit that its volume lacks, one a line, as tar is given it without
# recursion: the volume's own directory when it was just made, then a
# directory missing there before what it holds. A file being rotated is
# not here: it is put from the file given.
put_list() {
	[ "$2" = 0 ] || echo .
	_pl_there=$SECRETS_WORK/target/$1
	_pl_dirs=' '
	for _pl_file in $(files_of "$STATE/$1"); do
		! rotated "$1/$_pl_file" || continue
		[ ! -f "$_pl_there/$_pl_file" ] || continue
		_pl_dir=${_pl_file%/*}
		if [ "$_pl_dir" != "$_pl_file" ] && [ ! -d "$_pl_there/$_pl_dir" ]; then
			case $_pl_dirs in
			*" $_pl_dir "*) ;;
			*)
				_pl_dirs="$_pl_dirs$_pl_dir "
				echo "$_pl_dir"
				;;
			esac
		fi
		echo "$_pl_file"
	done
}

# rotate_list <unit>: the rotations of a unit whose volume does not hold
# the file given yet, "<unit>/<file>=<key>" a line.
rotate_list() {
	for _rl_entry in $_si_rotate; do
		case $_rl_entry in
		"$1"/*) ;;
		*) continue ;;
		esac
		rotation_given "${_rl_entry%%=*}"
		_rl_there=$SECRETS_WORK/target/${_rl_entry%%=*}
		if [ -f "$_rl_there" ] && cmp -s "$_rg" "$_rl_there"; then
			continue
		fi
		echo "$_rl_entry"
	done
}

# secrets_put "<unit>...": the target is given what it lacks, before
# anything starts there: a volume that is not there is made with the
# deployment's labels, and into each volume - fetched and compared in this
# run, or made in it - go the names it lacks, as a tar stream of exactly
# those names through a helper container: a put never writes over what is
# there. The one deliberate exception is a rotation (given_check), whose
# file is put over the volume's, and says so.
secrets_put() {
	_sp_units=
	for _sp_unit in secrets config; do
		case " $1 " in
		*" $_sp_unit "*) _sp_made=0 ;;
		*) _sp_made=1 ;;
		esac
		if [ "$_sp_made" = 1 ] || [ -n "$(rotate_list "$_sp_unit")" ] || [ -n "$(put_list "$_sp_unit" 0)" ]; then
			_sp_units="$_sp_units $_sp_unit"
		fi
	done
	[ -n "$_sp_units" ] || return 0
	_sp_vols=
	for _sp_unit in $_sp_units; do
		if [ "$_sp_unit" = secrets ] && [ -n "$_s_secdir" ]; then
			case " $1 " in
			*" $_sp_unit "*) ;;
			*) secdir_make ;;
			esac
			continue
		fi
		_sp_vols="$_sp_vols $_sp_unit"
		case " $1 " in
		*" $_sp_unit "*) ;;
		*) secrets_volume_create "$_sp_unit" ;;
		esac
	done
	_sp_ctr=$_s_name-volumes
	# shellcheck disable=SC2086 # unit names, without spaces
	[ -z "$_sp_vols" ] || io_container "$_sp_ctr" $_sp_vols
	for _sp_unit in $_sp_units; do
		case " $1 " in
		*" $_sp_unit "*) _sp_made=0 ;;
		*) _sp_made=1 ;;
		esac
		_sp_list=$(put_list "$_sp_unit" "$_sp_made")
		if [ -n "$_sp_list" ]; then
			# shellcheck disable=SC2046,SC2086 # names and options without spaces, the list is the point
			tar -c -f "$SECRETS_WORK/put.tar" --no-recursion $(rancher_tar_owner) -C "$STATE/$_sp_unit" $_sp_list
			# shellcheck disable=SC2086 # names without spaces
			unit_put "$_sp_unit" "$SECRETS_WORK/put.tar" $_sp_list
			rm -f "$SECRETS_WORK/put.tar"
			say "secrets: put into $(unit_place "$_sp_unit") what it lacked of $STATE/$_sp_unit: $(printf '%s' "$_sp_list" | grep -vx '\.' | tr '\n' ' ' | sed 's/ $//')"
		fi
		_sp_stage=$SECRETS_WORK/rotate-$_sp_unit
		_sp_names=
		for _sp_entry in $(rotate_list "$_sp_unit"); do
			_sp_path=${_sp_entry%%=*}
			rotation_given "$_sp_path"
			mkdir -p "$_sp_stage"
			copy_new "$_rg" "$_sp_stage/${_sp_path#*/}"
			_sp_names="$_sp_names ${_sp_path#*/}"
		done
		if [ -n "$_sp_names" ]; then
			# shellcheck disable=SC2046,SC2086 # names and options without spaces
			tar -c -f "$SECRETS_WORK/put.tar" --no-recursion $(rancher_tar_owner) -C "$_sp_stage" $_sp_names
			# shellcheck disable=SC2086 # names without spaces
			unit_put "$_sp_unit" "$SECRETS_WORK/put.tar" $_sp_names
			rm -rf "$SECRETS_WORK/put.tar" "$_sp_stage"
			say "secrets: rotated in $(unit_place "$_sp_unit"), from the files given:$_sp_names"
		fi
	done
	[ -z "$_sp_vols" ] || target_engine run rm -f "$_sp_ctr" </dev/null
}

# recreate_pending_mark: STATE/compose/recreate-pending lists the files a
# rotation replaces in a compose cluster (with those of a rotation still
# pending), written before the target's copy is replaced - also for a
# cluster on CE_HOST, whose marker is kept here. compose_install makes
# every node again while it is there.
recreate_pending_mark() {
	_rp=$STATE/compose/recreate-pending
	mkdir -p "$STATE/compose"
	chmod 0700 "$STATE/compose"
	{
		[ ! -f "$_rp" ] || cat "$_rp"
		for _rp_entry in $_si_rotate; do
			printf '%s\n' "${_rp_entry%%=*}"
		done
	} | sort -u >"$_rp.new"
	chmod 0600 "$_rp.new"
	mv -f "$_rp.new" "$_rp"
	say "secrets: the rotation of$(for _rp_entry in $_si_rotate; do printf ' %s' "${_rp_entry%%=*}"; done) is pending in $_rp until every node of the cluster has been made again and is ready"
}

# unit_put <unit> <tar file> <name>...: a tar stream of those names into the
# target's copy of the unit - its volume, through the helper secrets_put
# made, or the directory of compose's file secrets.
unit_put() {
	_up_unit=$1
	shift
	if [ "$_up_unit" = secrets ] && [ -n "$_s_secdir" ]; then
		secdir_put "$@"
		return 0
	fi
	# nerdctl's cp has no -a: the stream is owned by 65532
	# (rancher_tar_owner, secrets_put).
	if on_nerdctl; then
		target_engine run cp - "$_s_name-volumes:/var/lib/cactusfs/$_up_unit" <"$1"
	else
		target_engine run cp -a - "$_s_name-volumes:/var/lib/cactusfs/$_up_unit" <"$1"
	fi
}

# --- Kubernetes (PLAN 10.10) ------------------------------------------------------
#
# The target's copy is the Secrets of the deployment's namespace, which
# deploy/lib/k8s.sh reads back and compares, file to file, with the local
# copy (k8s_secrets_ensure, PLAN 10.9): created where the cluster lacks
# them, copied back where only the cluster has them, refused where the two
# differ, rotated for a certificate or token of the operator's. What neither
# copy holds is made here, by the one hook k8s.sh calls, secrets_ensure_k8s:
# the root credentials and the passphrase from /dev/urandom, kms.json and the
# certificates by dev-init on this machine. A secret given by its *_FILE
# setting is read from that file there; one that would open the data or
# begins the deployment (the root credentials, the passphrase, kms.json) must
# equal the copy STATE holds of it, or the run is refused.

# secrets_check_k8s: what secrets_install_k8s asks before anything is
# written or made, each refusal before any change (k8s_install asks it
# before the image is made): the files given, the cluster's identity
# against the record (engine_id_check: the UID of kube-system), a
# certificate given that replaces STATE's while STATE keeps the CA of the
# one it replaces, and - when dev-init may run, which is when STATE holds
# no complete configuration - what dev-init needs (under --from-source
# devinit_run asks it, after the image is made).
secrets_check_k8s() {
	[ -z "${_sc_done:-}" ] || return 0
	_sc_done=1
	umask 077
	secrets_settings
	for _si_key in TLS_CERT_FILE TLS_CA_FILE; do
		if [ "$_si_key" = TLS_CERT_FILE ]; then _si_file=$_s_cert; else _si_file=$_s_ca; fi
		if [ -n "$_si_file" ] && { [ ! -f "$_si_file" ] || [ ! -r "$_si_file" ]; }; then
			bad "$_si_key" "is not a file that can be read"
		fi
	done
	engine_id_check
	if [ -n "$_s_cert" ] && [ -z "$_s_ca" ] && [ -f "$STATE/config/s3-ca.pem" ] && [ -f "$STATE/config/s3.pem" ] &&
		! cmp -s "$_s_cert" "$STATE/config/s3.pem"; then
		refuse "TLS_CERT_FILE replaces the certificate of $STATE/config/s3.pem, and s3-ca.pem, the CA of the certificate it replaces, would be kept and handed to clients: give the CA that issued the new certificate in TLS_CA_FILE"
	fi
	_si_devinit=
	if [ ! -f "$STATE/config/ready" ]; then
		if [ "$(secrets_devinit)" = yes ] || { [ "$(setting DEPLOY_MODE)" = cluster ] && [ -z "$(setting NODE_CA_FILE)" ]; }; then
			_si_devinit=1
		fi
	fi
	if [ -n "$_si_devinit" ] && [ "$_s_source" != source ] && [ "$DRY_RUN" = 0 ]; then
		devinit_check
	fi
}

# secrets_install_k8s: secrets_install on Kubernetes. The given files are
# compared with STATE's copies first; then the namespace's Secrets are read
# back and compared (k8s_secrets_ensure), what neither copy holds is made
# (secrets_ensure_k8s), what the cluster lacks is created; then the record
# (the cluster's ID) is written, and a certificate or token of the
# operator's that rotated the cluster's Secret replaces STATE's copy too -
# the target's first, as on a Docker engine. In a dry run the Secrets are
# read back and compared, and nothing is made or written.
secrets_install_k8s() {
	secrets_check_k8s
	if [ "$DRY_RUN" = 1 ]; then
		k8s_secrets_ensure
		return 0
	fi
	_si_rotate=
	secrets_begin
	given_check "$_s_root_id" ROOT_ACCESS_KEY_FILE secrets root-access-key keep
	given_check "$_s_root_key" ROOT_SECRET_KEY_FILE secrets root-secret-key keep
	given_check "$_s_phrase" KMS_PASSPHRASE_FILE secrets kms-passphrase keep
	given_check "$_s_keyfile" KMS_KEY_FILE config kms.json keep
	given_check "$_s_token" VAULT_TOKEN_FILE secrets vault-token rotate
	given_check "$_s_cert" TLS_CERT_FILE config s3.pem rotate
	given_check "$_s_tlskey" TLS_KEY_FILE config s3.key rotate
	given_check "$_s_ca" TLS_CA_FILE config s3-ca.pem rotate
	k8s_secrets_ensure
	state_record
	for _si_entry in $_si_rotate; do
		rotation_given "${_si_entry%%=*}"
		[ -n "$_rg" ] || continue
		replace_file "$_rg" "$STATE/${_si_entry%%=*}"
		say "secrets: rotated $_rk: $STATE/${_si_entry%%=*} replaced, after the namespace's Secret"
	done
	cleanup_drop secrets_end "$SECRETS_WORK" "$STATE" "$_sb_top"
	secrets_end "$SECRETS_WORK" "$STATE" "$_sb_top"
	SECRETS_WORK=
	say "secrets: $STATE and the Secrets of the namespace $(k8s_ns) hold the same secrets"
}

# secrets_ensure_k8s <file>: the hook of k8s_secrets_ensure (deploy/lib/k8s.sh)
# - the secrets neither copy holds, by the paths in STATE the file lists one
# a line, made once, after what the cluster alone held was copied back. What
# the copies hold together must be whole first, or nothing is made: half a
# root pair, kms.json without its passphrase, or part of the configuration
# (a run stopped while making it, a file removed) is refused - a part made
# again beside what is there would never match it. Then, in this order: the
# root credentials and the passphrase from /dev/urandom (dev-init seals
# kms.json with the passphrase), and the configuration by dev-init on this
# machine (config_make, with the operator's own files in their place). In a
# dry run nothing is made.
secrets_ensure_k8s() {
	_sek_root=
	_sek_phrase=
	_sek_config=
	while IFS= read -r _sek; do
		case $_sek in
		"$STATE/secrets/root-access-key" | "$STATE/secrets/root-secret-key") _sek_root=1 ;;
		"$STATE/secrets/kms-passphrase") _sek_phrase=1 ;;
		"$STATE/config/"*) _sek_config="$_sek_config ${_sek#"$STATE/config/"}" ;;
		"$STATE/secrets/vault-token") refuse "KMS=vault: the Vault token is in neither $STATE nor the Secret $_s_name-vault-token; give it with VAULT_TOKEN_FILE" ;;
		*) fail "internal: secrets_ensure_k8s was handed $_sek" ;;
		esac
	done <"$1"
	if [ -n "$_sek_root" ]; then
		if [ -f "$STATE/secrets/root-access-key" ] || [ -f "$STATE/secrets/root-secret-key" ] || [ -n "$_s_root_id" ] || [ -n "$_s_root_key" ]; then
			refuse "one of the root credentials is in $STATE/secrets, a file given or the Secret $_s_name-root, and the other is in none of them: the deployment began with both; give them with ROOT_ACCESS_KEY_FILE and ROOT_SECRET_KEY_FILE"
		fi
	fi
	if [ -n "$_sek_phrase" ] && { [ -f "$STATE/config/kms.json" ] || [ -n "$_s_keyfile" ]; }; then
		refuse "the key file kms.json is there and its passphrase is in neither $STATE/secrets nor the Secret $_s_name-kms-passphrase: a new passphrase would open nothing; give the one it was made with in KMS_PASSPHRASE_FILE"
	fi
	if [ -n "$_sek_config" ] && [ -n "$(files_of "$STATE/config")" ]; then
		if [ -f "$STATE/config/ready" ] && [ "$_sek_config" = " kms.json" ]; then
			refuse "the certificates in $STATE/config are complete, and kms.json is in neither it nor the Secret $_s_name-kms: the deployment's objects are sealed under a key file a new one would not open; give the one it was made with in KMS_KEY_FILE"
		fi
		refuse "$STATE/config holds part of the deployment's certificates and key file, and${_sek_config} is in neither it nor the namespace's Secrets: nothing in it is made again; if nothing was ever started with them, remove $STATE/config and the deployment's Secrets, then run again"
	fi
	if [ "$DRY_RUN" = 1 ]; then
		say "dry run: would make what neither copy holds:$([ -z "$_sek_root" ] || printf ' the root credentials')$([ -z "$_sek_phrase" ] || printf ' the KMS passphrase')$([ -z "$_sek_config" ] || printf ' the certificates and key file (dev-init)')"
		return 0
	fi
	[ -n "${SECRETS_WORK:-}" ] || fail "internal: secrets_ensure_k8s outside secrets_install"
	mkdir -p "$STATE/secrets"
	if [ -n "$_sek_root" ]; then
		generate "$STATE/secrets/root-access-key" 'A-Z2-7' 20
		generate "$STATE/secrets/root-secret-key" 'A-Za-z0-9' 40
		say "secrets: made the root credentials, $STATE/secrets/root-access-key and $STATE/secrets/root-secret-key (never printed: read them there)"
	fi
	if [ -n "$_sek_phrase" ]; then
		generate "$STATE/secrets/kms-passphrase" 'A-Za-z0-9' 48
		say "secrets: made the KMS passphrase, $STATE/secrets/kms-passphrase - back up $STATE: kms.json opens nothing without it"
	fi
	modes "$STATE/secrets"
	if [ -n "$_sek_config" ]; then
		_s_devinit_all=1
		config_make
	fi
}

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
#   10.5), so it never makes a key file of its own. Rancher Desktop (10.6),
#   cluster mode on Docker (compose's config volume, 10.8) and Kubernetes
#   (Secrets of the namespace: read back and compared by 10.9, wired here by
#   10.10) keep their copies as those steps build;
#   secrets_where and secrets_install are where they are dispatched.
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

# secrets_where: "volumes" when this step keeps the target's copy - a
# standalone deployment on a Docker engine - else the step of PLAN
# milestone 10 that will.
secrets_where() {
	if on_kubernetes; then
		echo 10.10
	elif [ "$(setting DEPLOY_MODE)" = cluster ]; then
		echo 10.8
	else
		case $TARGET in
		dd | ce | podman) echo volumes ;;
		*) echo 10.6 ;;
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
}

# target_engine <run|query> <word>...: a command of the target's engine -
# Podman, the Docker engine of CE_HOST over ssh (its host key checked
# first), or the Docker CLI with its context.
target_engine() {
	_te_how=$1
	shift
	if [ "$TARGET" = podman ]; then
		"$_te_how" podman "$@"
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
	if [ "$TARGET" = podman ]; then
		"$_le_how" podman "$@"
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
	"$1" query image inspect --format '{{.Id}}' "$2" </dev/null >/dev/null 2>&1 || _ir_status=$?
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
# taken for "not there"); one of that name without the deployment's label
# is refused: the script never takes over what it did not make.
helper_clear() {
	_hc_out=$("$1" query ps -a --filter "name=$2" --format '{{.Names}}' </dev/null) ||
		fail "could not list the containers of the engine (its message is above)"
	_hc_found=
	for _hc_name in $(printf '%s\n' "$_hc_out" | printable); do
		[ "$_hc_name" != "$2" ] || _hc_found=1
	done
	[ -n "$_hc_found" ] || return 0
	_hc_owner=$("$1" query container inspect --format "{{index .Config.Labels \"$LABEL_NAME\"}}" "$2" </dev/null) ||
		fail "could not inspect the container $2 (the engine's message is above)"
	_hc_owner=$(printf '%s' "$_hc_owner" | printable | tr -d '\n')
	[ "$_hc_owner" = "$_s_name" ] ||
		refuse "a container named $2 is there without the label $LABEL_NAME=$_s_name (its label says '$_hc_owner'): the script never takes over what it did not make; remove or rename it, or choose another DEPLOY_NAME"
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
	say "secrets: kept in $STATE (directories 0700, files 0600) and on $(engine) in the labelled volumes $(setting DEPLOY_NAME)-secrets and $(setting DEPLOY_NAME)-config, put there before anything starts"
	for _sp in secrets/root-access-key secrets/root-secret-key secrets/kms-passphrase secrets/vault-token config/kms.json config/ready; do
		if [ -f "$STATE/$_sp" ]; then
			say "secrets: $STATE/$_sp is there and is used as it is"
		fi
	done
	say "secrets: what neither copy holds is made once - the root credentials and the passphrase from /dev/urandom, kms.json and the certificates by cactusfs dev-init in a container of this machine's architecture - and two copies that differ are refused"
}

# secrets_install: every secret of the deployment in both of its places,
# before anything starts on the target. Run by --install on a target whose
# copy secrets_where says this step keeps. Everything that refuses is asked
# before STATE is written.
secrets_install() {
	if [ "$DRY_RUN" = 1 ]; then
		secrets_plan
		return 0
	fi
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
		"$_s_name-secrets" | "$_s_name-config") _si_found="$_si_found $_si_vol" ;;
		esac
	done
	if [ -n "$_si_found" ]; then
		# shellcheck disable=SC2086 # volume names, without spaces
		_si_out=$(target_engine query volume inspect --format "{{.Name}} {{index .Labels \"$LABEL_NAME\"}}" $_si_found </dev/null) ||
			fail "could not inspect the volumes$_si_found (the engine's message is above)"
		_si_out=$(printf '%s\n' "$_si_out" | printable)
		for _si_vol in $_si_found; do
			_si_owner=$(printf '%s\n' "$_si_out" | sed -n "s|^$_si_vol ||p" | head -n 1)
			[ "$_si_owner" = "$_s_name" ] ||
				refuse "a volume named $_si_vol is there without the label $LABEL_NAME=$_s_name (its label says '$_si_owner'): the script never takes over what it did not make; remove or rename it, or choose another DEPLOY_NAME"
			_si_have="$_si_have ${_si_vol#"$_s_name"-}"
		done
	fi
	if [ -n "$_si_have" ] && [ ! -f "$STATE/target" ] && [ "$ADOPT" != 1 ]; then
		refuse "the deployment's volumes ($(for _u in $_si_have; do printf '%s ' "$_s_name-$_u"; done | sed 's/ $//')) are on $(engine), and $STATE holds no record of them: they were made from another machine or another STATE_DIR, or the record was lost; say --adopt to take the secrets they hold into $STATE"
	fi
	_si_img=$(helper_image)
	image_ready target_engine "$_si_img"
	# dev-init is asked for what it needs first when it will run: neither
	# copy of the certificates is there, and no file gives all they hold.
	case "$_si_have " in
	*" config "*) ;;
	*)
		if [ ! -f "$STATE/config/ready" ] && [ "$(secrets_devinit)" = yes ]; then
			devinit_check
		fi
		;;
	esac

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
			refuse "$_si_local/$_si_file and $_si_file in the volume $_s_name-$_si_unit differ: two copies of one secret, and the script never chooses between them (ADR-0033 section 3); find out which one the deployment uses and remove the other, then run again"
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
			warn "the local copy $_si_local/$_si_file was missing: copied back from the volume $_s_name-$_si_unit"
		done
	done
	secrets_make
	config_make
	secrets_put "$_si_have"
	for _si_entry in $_si_rotate; do
		rotation_given "${_si_entry%%=*}"
		replace_file "$_rg" "$STATE/${_si_entry%%=*}"
		say "secrets: rotated $_rk: $STATE/${_si_entry%%=*} replaced, after the volume's copy"
	done
	trap - EXIT HUP INT TERM
	secrets_end "$SECRETS_WORK" "$STATE" "$_sb_top"
	SECRETS_WORK=
	say "secrets: $STATE and the volumes $_s_name-secrets and $_s_name-config hold the same secrets"
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
	if [ "$TARGET" = podman ]; then
		_eic=$(target_engine query info --format '{{.Host.Hostname}}:{{.Store.GraphRoot}}' </dev/null) ||
			fail "could not ask $(engine) who it is (its message is above)"
	else
		_eic=$(target_engine query info --format '{{.ID}}' </dev/null) ||
			fail "could not ask $(engine) who it is (its message is above)"
	fi
	_engine_id=$(printf '%s' "$_eic" | printable | tr -d '\n')
	[ -n "$_engine_id" ] || fail "$(engine) did not say its ID"
	say "engine ID: $_engine_id"
	_engine_rerecord=
	[ -f "$STATE/target" ] || return 0
	_eic_recorded=$(sed -n 's/^engine-id=//p' "$STATE/target" | head -n 1 | printable)
	if [ "$_eic_recorded" != "$_engine_id" ]; then
		[ "$ENGINE_REPLACED" = 1 ] ||
			refuse "$STATE was made on the engine '$_eic_recorded', and the settings ($(engine_identity)) now reach another, '$_engine_id': the secrets of one deployment are never given to a second engine. Set the settings back; or, if the engine '$_eic_recorded' is gone and this one replaced it (a factory reset), say --engine-replaced"
		warn "--engine-replaced: the deployment made on the engine '$_eic_recorded' is taken as gone; its secrets in $STATE now belong to the engine '$_engine_id', and are put there"
		_engine_rerecord=1
	fi
	_eic_kms=$(sed -n 's/^kms=//p' "$STATE/target" | head -n 1 | printable)
	[ "$_eic_kms" = "$_s_kms" ] ||
		refuse "$STATE was made with KMS=$_eic_kms, and the settings now say KMS=$_s_kms: the script does not move a deployment to another KMS"
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
	# The paths the cleanup works on are written into the trap now, quoted,
	# not read when it fires: nothing a function does in between - by
	# assigning a variable of the same name - can point it elsewhere.
	trap "secrets_end $(shown "$SECRETS_WORK") $(shown "$STATE") $(shown "$_sb_top")" EXIT
	trap 'exit 129' HUP
	trap 'exit 130' INT
	trap 'exit 143' TERM
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
			printf 'engine-id=%s\nkms=%s\n' "$_engine_id" "$_s_kms"
		} >"$SECRETS_WORK/target.new"
		replace_file "$SECRETS_WORK/target.new" "$STATE/target"
		say "state: recorded the engine $_engine_id in $STATE/target, in place of the one it replaced"
		return 0
	fi
	{
		engine_identity
		echo
		printf 'engine-id=%s\nkms=%s\n' "$_engine_id" "$_s_kms"
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
	# shellcheck disable=SC2086 # unit names, without spaces
	io_container "$_sf_ctr" $1
	for _sf_unit in $1; do
		target_engine query cp "$_sf_ctr:/$_sf_unit" - </dev/null >"$SECRETS_WORK/fetch.tar"
		tar -x -o -f "$SECRETS_WORK/fetch.tar" -C "$SECRETS_WORK/target"
		rm -f "$SECRETS_WORK/fetch.tar"
		[ -d "$SECRETS_WORK/target/$_sf_unit" ] || mkdir "$SECRETS_WORK/target/$_sf_unit"
	done
	target_engine run rm -f "$_sf_ctr" </dev/null
	only_plain "$SECRETS_WORK/target" "a volume of the deployment"
}

# io_container <container> <unit>...: a helper container on the target that
# mounts the volume of each unit at /<unit> - named volumes only, never a
# path of a host - made from helper_image as the nodes' user and never
# started: what is copied in with "cp -a" is owned by that user (65532).
io_container() {
	_io_ctr=$1
	shift
	helper_clear target_engine "$_io_ctr"
	_io_mounts=
	for _io_unit in "$@"; do
		_io_mounts="$_io_mounts --mount type=volume,source=$_s_name-$_io_unit,target=/$_io_unit"
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
		_gc_old="$4 in the volume $_s_name-$3"
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
		refuse "one of the root credentials is in $STATE/secrets or the volume $_s_name-secrets and the other is in neither: the deployment began with both; give them with ROOT_ACCESS_KEY_FILE and ROOT_SECRET_KEY_FILE"
	fi
	# (A KMS_KEY_FILE always comes with its KMS_PASSPHRASE_FILE: env.sh.)
	if [ "$_s_kms" = local ] && ! have secrets kms-passphrase && [ -z "$_s_phrase" ] && have config kms.json; then
		refuse "the key file kms.json is there and its passphrase is in neither $STATE/secrets nor the volume $_s_name-secrets: a new passphrase would open nothing; give the one it was made with in KMS_PASSPHRASE_FILE"
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
	if [ -z "$_s_cert" ]; then
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
# image, or IMAGE - on this machine's engine, and no container in the way
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
	*) not_built 10.4 "the image of this machine's architecture that dev-init runs in, built from source" ;;
	esac
	image_ready local_engine "$_dc_img" "linux/$_dc_arch"
	helper_clear local_engine "$_s_name-dev-init"
}

# devinit_run <directory>: "cactusfs dev-init" in a container of this
# machine's architecture - the binary of make image-binaries in the runtime
# image, or IMAGE - with the passphrase and the binary copied in as a tar
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
	if [ "$_s_kms" = local ]; then
		copy_new "$STATE/secrets/kms-passphrase" "$_dr_in/kms-passphrase"
	else
		# dev-init always seals a key file; with Vault it is thrown away.
		generate "$_dr_in/kms-passphrase" 'A-Za-z0-9' 48
	fi
	devinit_check
	if [ "$_s_source" = binaries ]; then
		tar -c -f "$SECRETS_WORK/dev-init.tar" -C "$_s_rel/linux-$_dr_arch" cactusfs -C "$_dr_in" kms-passphrase
		_dr_img=$(runtime_image)
		set -- "$_dr_img" /tmp/cactusfs
	else
		tar -c -f "$SECRETS_WORK/dev-init.tar" -C "$_dr_in" kms-passphrase
		set -- --entrypoint /cactusfs "$_s_image"
	fi
	rm -f "$_dr_in/kms-passphrase"
	set -- "$@" dev-init --dir /tmp/out --nodes "$_s_name" --validity "$_s_validity" --kms-passphrase-file /tmp/kms-passphrase
	[ -z "$_s_hosts" ] || set -- "$@" --hosts "$_s_hosts"
	local_engine run create --name "$_dr_ctr" --label "$LABEL_NAME=$_s_name" --label "$LABEL_TARGET=$TARGET" \
		--platform "linux/$_dr_arch" --user 65532:65532 --pull never --network none --cap-drop ALL --security-opt no-new-privileges "$@" </dev/null
	local_engine run cp -a - "$_dr_ctr:/tmp" <"$SECRETS_WORK/dev-init.tar"
	rm -f "$SECRETS_WORK/dev-init.tar"
	{
		local_engine run start -a "$_dr_ctr" </dev/null 2>&1 || echo failed >"$SECRETS_WORK/dev-init.failed"
	} | printable
	[ ! -f "$SECRETS_WORK/dev-init.failed" ] || fail "dev-init failed in its container (what it said is above)"
	local_engine query cp "$_dr_ctr:/tmp/out" - </dev/null >"$SECRETS_WORK/dev-init.tar"
	local_engine run rm -f "$_dr_ctr" </dev/null
	tar -x -o -f "$SECRETS_WORK/dev-init.tar" -C "$_dr_in"
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
	for _sp_unit in $_sp_units; do
		case " $1 " in
		*" $_sp_unit "*) ;;
		*) target_engine run volume create --label "$LABEL_NAME=$_s_name" --label "$LABEL_TARGET=$TARGET" "$_s_name-$_sp_unit" </dev/null ;;
		esac
	done
	_sp_ctr=$_s_name-volumes
	# shellcheck disable=SC2086 # unit names, without spaces
	io_container "$_sp_ctr" $_sp_units
	for _sp_unit in $_sp_units; do
		case " $1 " in
		*" $_sp_unit "*) _sp_made=0 ;;
		*) _sp_made=1 ;;
		esac
		_sp_list=$(put_list "$_sp_unit" "$_sp_made")
		if [ -n "$_sp_list" ]; then
			# shellcheck disable=SC2086 # names without spaces, the list is the point
			tar -c -f "$SECRETS_WORK/put.tar" --no-recursion -C "$STATE/$_sp_unit" $_sp_list
			target_engine run cp -a - "$_sp_ctr:/$_sp_unit" <"$SECRETS_WORK/put.tar"
			rm -f "$SECRETS_WORK/put.tar"
			say "secrets: put into the volume $_s_name-$_sp_unit what it lacked of $STATE/$_sp_unit: $(printf '%s' "$_sp_list" | grep -vx '\.' | tr '\n' ' ' | sed 's/ $//')"
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
			# shellcheck disable=SC2086 # names without spaces
			tar -c -f "$SECRETS_WORK/put.tar" --no-recursion -C "$_sp_stage" $_sp_names
			target_engine run cp -a - "$_sp_ctr:/$_sp_unit" <"$SECRETS_WORK/put.tar"
			rm -rf "$SECRETS_WORK/put.tar" "$_sp_stage"
			say "secrets: rotated in the volume $_s_name-$_sp_unit, from the files given:$_sp_names"
		fi
	done
	target_engine run rm -f "$_sp_ctr" </dev/null
}

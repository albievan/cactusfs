# shellcheck shell=sh
# The settings of deploy/deploy.sh: the env file, read as data and never as
# code; the flags that set a setting; the defaults; which target is meant,
# and the engine or cluster the tools are pointed at. Sourced by
# deploy/deploy.sh; not run on its own.
#
# The env file (deploy/.env, or the file of --env) is one setting a line:
#
#   KEY=value
#
# KEY matches ^[A-Z][A-Z0-9_]*$ and is a key of the table below, or a
# setting of "cactusfs server" (CACTUSFS_*, below). The value is everything
# after the first "=", exactly as it stands: nothing in it is expanded,
# substituted or run - no quotes are taken off, no $, backtick or ; means
# anything - with one exception: a leading "~/" in a key that ends in _DIR
# or _FILE is the home directory. A value that is empty counts as not set.
# Blank lines and lines whose first character other than spaces and tabs is
# "#" are skipped. Any other line - a key in lower case, an unknown key, a
# line without "=", a key given twice, a line ending in a carriage return -
# is refused with its line number. A key is named in a refusal only when it
# has the form of a key: text before the "=" that is not one might be part
# of a secret pasted in, so only its length is said.
#
# The env file never holds a secret (CLAUDE.md section 2). A secret is named
# by a key that ends in _FILE and holds the path of a file that holds it;
# a key that looks like an inline secret is refused and pointed there.
#
# One key's value is a command: K3S_IMPORT. It is split into words at
# spaces and run as those words, never by a shell; a character a shell would
# read (quotes, $, backtick, ;, |, &, <, >, *, ...) is refused.
#
# Precedence: a flag, then the env file, then the default of the table. The
# environment of the process is not read for any setting (XDG_STATE_HOME and
# HOME place the default STATE_DIR, as they place every user's state), and
# the variables that would point the tools elsewhere (DOCKER_HOST,
# DOCKER_CONTEXT, CONTAINER_HOST, KUBECONFIG, DOCKER_CONFIG, CACTUSFS_* ...)
# never reach one: every tool runs under "env -i" (tool, in common.sh).

# The keys of the env file, with their defaults. deploy/.env.example
# explains each of them, and test/deploy holds the two lists equal: every
# key here is in the example with the same default, and every key the
# example names is here. KMS has no default: it must be stated. An empty
# STATE_DIR is ${XDG_STATE_HOME:-~/.local/state}/cactusfs-deploy.
env_defaults() {
	cat <<'KEYS'
DEPLOY_TARGET=
DEPLOY_NAME=cactusfs
DEPLOY_MODE=standalone
KMS=
ALLOW_SHARED_LOCAL_KMS=no
KMS_KEY_FILE=
KMS_PASSPHRASE_FILE=
VAULT_ADDR=
VAULT_TRANSIT_MOUNT=transit
VAULT_KEY=
VAULT_TOKEN_FILE=
VAULT_CA_FILE=
VAULT_NAMESPACE=
ROOT_ACCESS_KEY_FILE=
ROOT_SECRET_KEY_FILE=
STATE_DIR=
OFFLINE=no
IMAGE_SOURCE=binaries
RELEASE_DIR=dist/image
IMAGE=
BUILD_HOST=
ALLOW_DIRTY=no
ENGINE_CONTEXT=
DRIVES=4
BIND=127.0.0.1
ADMIN_BIND=127.0.0.1
S3_PORT=9000
ADMIN_PORT=9001
TLS_HOSTS=
TLS_VALIDITY=8760h
TLS_CERT_FILE=
TLS_KEY_FILE=
TLS_CA_FILE=
NODE_CA_FILE=
NODE_CERT_DIR=
CE_HOST=
CE_INSTALL_DOCKER=no
CE_INSTALL_DOCKER_FORCE=no
CE_STATE_DIR=.cactusfs-deploy
RANCHER_ENGINE=auto
RANCHER_KUBERNETES=no
K3S_SSH=
K3S_IMPORT=
KUBECTL=kubectl
K8S_CONTEXT=
TOOL_ENV_PASS=
K8S_NAMESPACE=
K8S_REGISTRY=
K8S_STORAGE=pvc
K8S_STORAGE_CLASS=
K8S_DRIVE_SIZE=100Gi
K8S_META_SIZE=10Gi
K8S_SERVICE_TYPE=NodePort
K8S_NODE_PORT=
K8S_S3_ALLOW_FROM=
K8S_ADMIN_SERVICE_TYPE=ClusterIP
K8S_ADMIN_NODE_PORT=
K8S_ADMIN_ALLOW_FROM=
HELM_IMAGE=
HELM=
WAIT_TIMEOUT=600
KEYS
}

# The settings of "cactusfs server" that the script sets itself on every
# node, so that a CACTUSFS_* line of the env file may not: where the node
# listens and is reached (the script publishes the ports, the chart names
# the peers), its drives and metadata directory, who it is in a cluster,
# the paths of its certificates and keys, its KMS and Vault (from KMS and
# VAULT_* above), the files of its root credentials, how it is upgraded
# (the script, or the chart's upgrader), how long it drains (the chart
# works it out from its probes), and the files of the update CA and trusted
# root (no file is put on a node for them yet). Also a confirmation that
# holds for one start only: an env file would repeat it at every start.
# A setting the example marks as a secret is refused as well, whatever this
# list says; every other setting the example names passes to the nodes.
env_managed() {
	cat <<'MANAGED'
CACTUSFS_S3_ADDR
CACTUSFS_ADMIN_ADDR
CACTUSFS_DEV_PLAINTEXT
CACTUSFS_DRAIN_DELAY
CACTUSFS_SHUTDOWN_TIMEOUT
CACTUSFS_DATA
CACTUSFS_DRIVE_LABELS
CACTUSFS_META_DIR
CACTUSFS_ALLOW_DRIVE_REPLACEMENT
CACTUSFS_NODE_ID
CACTUSFS_ROLES
CACTUSFS_PEERS
CACTUSFS_RPC_ADDR
CACTUSFS_RAFT_ADDR
CACTUSFS_ADVERTISE_RPC
CACTUSFS_ADVERTISE_RAFT
CACTUSFS_TOPOLOGY_HOST
CACTUSFS_TLS_CERT
CACTUSFS_TLS_KEY
CACTUSFS_NODE_TLS_CERT
CACTUSFS_NODE_TLS_KEY
CACTUSFS_NODE_TLS_CA
CACTUSFS_KMS
CACTUSFS_KMS_KEY_FILE
CACTUSFS_KMS_PASSPHRASE_FILE
CACTUSFS_VAULT_ADDR
CACTUSFS_VAULT_TRANSIT_MOUNT
CACTUSFS_VAULT_KEY
CACTUSFS_VAULT_TOKEN_FILE
CACTUSFS_VAULT_CA_CERT
CACTUSFS_VAULT_SERVER_NAME
CACTUSFS_VAULT_NAMESPACE
CACTUSFS_ROOT_ACCESS_KEY_FILE
CACTUSFS_ROOT_SECRET_KEY_FILE
CACTUSFS_UPGRADE_MODE
CACTUSFS_UPDATE_CA
CACTUSFS_UPDATE_TRUSTED_ROOT
MANAGED
}

# The targets --docker and DEPLOY_TARGET name.
TARGETS='dd ce podman rancher k3s k8s'

# What was read: "KEY=value" a line. FLAG_SETTINGS from the flags,
# FILE_SETTINGS from the env file, SETTINGS the result, and SOURCES where
# each came from (flag, file or default). PASSED holds the CACTUSFS_* lines
# for the nodes, in the order of the file. Everything starts empty, so that
# nothing of the environment of the process is taken for a setting.
FLAG_SETTINGS=
FILE_SETTINGS=
SETTINGS=
SOURCES=
PASSED=
ENV_FILE=

# lookup <store> <key>: prints the value of the key in the store, and fails
# when the store does not hold it.
lookup() {
	_lookup_key=$2
	while IFS= read -r _lookup_line; do
		case $_lookup_line in
		"$_lookup_key="*)
			printf '%s' "${_lookup_line#*=}"
			return 0
			;;
		esac
	done <<EOF
$1
EOF
	return 1
}

# setting <key>: the value of a setting, after precedence and validation.
setting() {
	lookup "$SETTINGS" "$1" || fail "internal: no setting $1"
}

# env_known <key>: true for a key of the table.
env_known() {
	lookup "$(env_defaults)" "$1" >/dev/null
}

# env_flag <key> <value>: a setting given by a flag. A value is one line: a
# line break in it would add a line - another setting - to FLAG_SETTINGS.
env_flag() {
	case $2 in
	*"$NL"* | *"$CR"*) refuse "the flag that sets $1 has a value with a line break in it" ;;
	esac
	FLAG_SETTINGS="$FLAG_SETTINGS$1=$2
"
}

# env_line_refused <line number> <text>
env_line_refused() {
	refuse "$ENV_FILE:$1: $2"
}

# env_parse <file>: reads the env file into FILE_SETTINGS and PASSED. A
# refused line names its number and the key, never the value: a value could
# be a secret written where it does not belong.
env_parse() {
	ENV_FILE=$1
	[ -f "$ENV_FILE" ] && [ -r "$ENV_FILE" ] || refuse "--env: no file $ENV_FILE that can be read"
	_n=0
	while IFS= read -r _line || [ -n "$_line" ]; do
		_n=$((_n + 1))
		case $_line in
		*"$CR"*) env_line_refused "$_n" "the line ends in a carriage return (a Windows line ending): save the file with Unix line endings" ;;
		*[!' 	']*) ;;
		*) continue ;;
		esac
		case ${_line#"${_line%%[!' 	']*}"} in
		'#'*) continue ;;
		esac
		case $_line in
		*=*) ;;
		*) env_line_refused "$_n" "no '=' on the line: a line is KEY=value" ;;
		esac
		_key=${_line%%=*}
		_value=${_line#*=}
		case $_key in
		'' | [!ABCDEFGHIJKLMNOPQRSTUVWXYZ]* | *[!ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_]*)
			case $_key in
			*[abcdefghijklmnopqrstuvwxyz]*) _why="is not upper case: a key is written as in deploy/.env.example, such as DEPLOY_NAME" ;;
			*) _why="is not a key: a key is A-Z, 0-9 and _, beginning with a letter, from the start of the line to the '='" ;;
			esac
			# Not repeated: it may be part of a secret pasted in.
			env_line_refused "$_n" "the text before the first '=' (${#_key} characters) $_why"
			;;
		CACTUSFS_*)
			env_cactusfs "$_n" "$_key" "$_value"
			continue
			;;
		PURGE_CONFIRM)
			env_line_refused "$_n" "PURGE_CONFIRM is not a setting: deleting the data is confirmed at each run, with --purge-confirm <name> or at the prompt"
			;;
		esac
		env_inline_secret "$_n" "$_key"
		env_known "$_key" || env_line_refused "$_n" "unknown key $_key: deploy/.env.example lists every key"
		if lookup "$FILE_SETTINGS" "$_key" >/dev/null; then
			env_line_refused "$_n" "$_key is set twice"
		fi
		FILE_SETTINGS="$FILE_SETTINGS$_key=$_value
"
	done <"$ENV_FILE"
}

# env_inline_secret <line number> <key>: refuses a key that would hold a
# secret in the file itself, and names the key that takes its file instead.
env_inline_secret() {
	case $2 in
	*_FILE) return 0 ;;
	esac
	if env_known "${2}_FILE"; then
		env_line_refused "$1" "$2 would put a secret in the env file, which never holds one: write ${2}_FILE=<path of a file that holds it>, or leave it out and one is generated"
	fi
	secret_name "$1" "$2"
}

# secret_name <line number> <key>: refuses a key whose name says it holds a
# secret - one of the script's own keys, or a setting of the nodes.
secret_name() {
	case $2 in
	*SECRET* | *PASSWORD* | *PASSPHRASE* | *TOKEN* | *CREDENTIAL*)
		env_line_refused "$1" "$2 looks like a secret, and the env file never holds one: secrets are given as files, by the keys that end in _FILE (deploy/.env.example)"
		;;
	esac
}

# cactusfs_settings: "NAME secret" or "NAME setting" for every setting that
# deploy/cactusfs.env.example names - the file "cactusfs config" writes. A
# setting is a secret when its explanation says it is one, or that it is the
# path of a file that holds one.
cactusfs_settings() {
	awk '
		/^#CACTUSFS_[A-Z0-9_]*=/ {
			name = substr($0, 2)
			sub(/=.*/, "", name)
			print name, (text ~ /A secret,|holds a secret/ ? "secret" : "setting")
			text = ""
			next
		}
		/^# / { text = text " " substr($0, 3); next }
		{ text = "" }
	' "$DEPLOY_DIR/cactusfs.env.example"
}

# env_cactusfs <line number> <key> <value>: a setting for the nodes passes
# when deploy/cactusfs.env.example names it, it is not a secret - by the
# example's word or by its name - the script does not set it itself, and its
# value holds no $, backtick or control character (no setting of the nodes
# needs one, and the value goes on into files and commands of later steps).
# The value is never repeated: it may be a secret pasted in.
env_cactusfs() {
	secret_name "$1" "$2"
	_kind=$(cactusfs_settings | awk -v name="$2" '$1 == name { print $2 }')
	case $_kind in
	'') env_line_refused "$1" "$2 is not a setting of cactusfs server: deploy/cactusfs.env.example names every one" ;;
	secret) env_line_refused "$1" "$2 is a secret, and the env file never holds one: the script makes or takes the node's secrets itself (ROOT_*_FILE, KMS_PASSPHRASE_FILE, VAULT_TOKEN_FILE, TLS_*_FILE)" ;;
	esac
	if env_managed | grep -qx "$2"; then
		env_line_refused "$1" "$2 is set by the script itself on every node (the list is in deploy/lib/env.sh); use the script's own keys in deploy/.env.example"
	fi
	if lookup "$PASSED" "$2" >/dev/null; then
		env_line_refused "$1" "$2 is set twice"
	fi
	case $3 in
	*'$'* | *'`'* | *[[:cntrl:]]*)
		env_line_refused "$1" "the value of $2 holds a \$, a backtick or a control character: no setting of the nodes takes one"
		;;
	esac
	PASSED="$PASSED$2=$3
"
}

# env_path <key> <value>: the value of a key that names a path on this
# machine: a leading ~/ is the home directory, and a relative path is
# relative to the repository (the directory deploy/ is in).
env_path() {
	case $2 in
	'') return 0 ;;
	'~/'*)
		[ -n "${HOME:-}" ] || refuse "$1: '~/' needs a home directory, and HOME is not set"
		printf '%s/%s' "${HOME%/}" "${2#'~/'}"
		;;
	'~'*) refuse "$1=$2: only a leading '~/' is expanded" ;;
	/*) printf '%s' "$2" ;;
	*) printf '%s/%s' "$REPO_ROOT" "$2" ;;
	esac
}

# default_state_dir: where state is kept when STATE_DIR is empty, as the XDG
# base directories place a user's state.
default_state_dir() {
	case ${XDG_STATE_HOME:-} in
	/*) printf '%s/cactusfs-deploy' "${XDG_STATE_HOME%/}" ;;
	*)
		[ -n "${HOME:-}" ] || refuse "STATE_DIR is not set, and neither XDG_STATE_HOME nor HOME is there to place it"
		printf '%s/.local/state/cactusfs-deploy' "${HOME%/}"
		;;
	esac
}

# env_resolve: SETTINGS and SOURCES from the flags, the file and the
# defaults, in that order.
env_resolve() {
	SETTINGS=
	SOURCES=
	while IFS= read -r _line; do
		[ -n "$_line" ] || continue
		_key=${_line%%=*}
		_value=${_line#*=}
		_from=default
		if _flag=$(lookup "$FLAG_SETTINGS" "$_key"); then
			_value=$_flag
			_from=flag
		elif _file=$(lookup "$FILE_SETTINGS" "$_key") && [ -n "$_file" ]; then
			_value=$_file
			_from=file
		fi
		if [ "$_key" = STATE_DIR ] && [ -z "$_value" ]; then
			_value=$(default_state_dir)
		fi
		case $_key in
		CE_STATE_DIR) ;; # a directory on the server, never this machine's
		*_DIR | *_FILE) _value=$(env_path "$_key" "$_value") ;;
		esac
		SETTINGS="$SETTINGS$_key=$_value
"
		SOURCES="$SOURCES$_key=$_from
"
	done <<EOF
$(env_defaults)
EOF
}

# --- validation ---------------------------------------------------------------

# bad <key> <text>: a setting with a value that is refused. The value is
# shown: no setting holds a secret.
bad() {
	refuse "$1=$(setting "$1"): $2"
}

one_of() {
	_one_key=$1
	_one_value=$(setting "$1")
	shift
	for _one in "$@"; do
		[ "$_one_value" != "$_one" ] || return 0
	done
	bad "$_one_key" "must be one of: $*"
}

yes_no() {
	one_of "$1" yes no
}

# number <key> <min> <max> [empty]: a whole number in a range; "empty" lets
# it be empty.
number() {
	_num=$(setting "$1")
	if [ -z "$_num" ] && [ "${4:-}" = empty ]; then
		return 0
	fi
	case $_num in
	'' | *[!0123456789]* | 0?*) bad "$1" "must be a whole number from $2 to $3" ;;
	esac
	[ "${#_num}" -le 6 ] && [ "$_num" -ge "$2" ] && [ "$_num" -le "$3" ] || bad "$1" "must be a whole number from $2 to $3"
}

# chars <key> <characters allowed, as in a bracket expression> <what is allowed>
chars() {
	case $(setting "$1") in
	*[!$2]*) bad "$1" "may hold $3 only" ;;
	esac
}

# dns_label <key> [empty]: lower-case letters, digits and hyphens, a letter
# first, at most 40 characters (it names containers, volumes and objects).
dns_label() {
	_label=$(setting "$1")
	if [ -z "$_label" ] && [ "${2:-}" = empty ]; then
		return 0
	fi
	case $_label in
	'' | [!abcdefghijklmnopqrstuvwxyz]* | *[!abcdefghijklmnopqrstuvwxyz0123456789-]* | *-)
		bad "$1" "must be lower-case letters, digits and hyphens, beginning with a letter and not ending with a hyphen"
		;;
	esac
	[ "${#_label}" -le 40 ] || bad "$1" "must be at most 40 characters"
}

# ssh_host <key>: empty, or a destination ssh is given as it is - plain, by
# the rule of scripts/remote.sh, so that it is never an option or code.
ssh_host() {
	_host=$(setting "$1")
	[ -z "$_host" ] || plain "$_host" || bad "$1" "must be user@host or host: letters, digits and _./=:@+,- only, not beginning with '-'"
}

# word <key>: empty, or one plain word.
word() {
	_word=$(setting "$1")
	[ -z "$_word" ] || plain "$_word" || bad "$1" "must be one word of letters, digits and _./=:@+,- not beginning with '-'"
}

# no_equals <key>: a program the script runs - an "=" in its name would be
# read by env as a variable to set, not a program to run.
no_equals() {
	case $(setting "$1") in
	*=*) bad "$1" "names a program, and a program's name holds no '='" ;;
	esac
}

# The variables of the script itself, which TOOL_ENV_PASS may not name: a
# tool would be given the script's value, not the caller's. test/deploy
# holds this list equal to the upper-case names the script assigns.
script_vars() {
	cat <<'VARS'
ACTION
ADOPT
ANOTHER_TARGET
CR
DEPLOY_DIR
DRY_RUN
ENGINE_REPLACED
ENV_FILE
FILE_SETTINGS
FLAG_SETTINGS
FOUND
IFS
LABEL_NAME
LABEL_TARGET
LC_ALL
MARKER_ANNOTATION
NL
PASSED
PURGE
PURGE_CONFIRM
REPO_ROOT
SECRETS_WORK
SETTINGS
SOURCES
SSH_CHECKED
STATE
TARGET
TARGETS
TARGET_FROM
VARS
}

# tool_env_pass: TOOL_ENV_PASS is empty, or names separated by commas of
# variables of the caller's environment that every tool is given as well -
# what a kubectl exec plugin or a proxy needs (AWS_PROFILE, CLOUDSDK_CONFIG,
# HTTPS_PROXY, NO_PROXY, ...). Names only, never values. A name that would
# undo what "env -i" closes is refused: what points a tool at an engine, a
# cluster or its configuration (DOCKER_*, CONTAINER*, KUBECONFIG, NERDCTL_*,
# PODMAN_*, XDG_*), the settings of the nodes (CACTUSFS_*), what the script
# sets itself (PATH, HOME, LC_*, SSH_*, its own variables and settings),
# what makes a shell or the loader run code (BASH_ENV, ENV, SHELLOPTS,
# BASHOPTS, IFS, PS4, LD_*, DYLD_*), and a name that looks like a secret.
tool_env_pass() {
	_pass=$(setting TOOL_ENV_PASS)
	[ -n "$_pass" ] || return 0
	case $_pass in
	*[!ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_,]* | ,* | *, | *,,*)
		bad TOOL_ENV_PASS "must be names of variables separated by commas, such as HTTPS_PROXY,NO_PROXY,AWS_PROFILE: A-Z, 0-9 and _, not beginning with a digit"
		;;
	esac
	_old_ifs=$IFS
	IFS=,
	# shellcheck disable=SC2086 # the split at commas is the point
	set -- $_pass
	IFS=$_old_ifs
	for _pn in "$@"; do
		case $_pn in
		[0123456789]*) bad TOOL_ENV_PASS "$_pn is not a name: a name does not begin with a digit" ;;
		DOCKER_* | CONTAINER* | KUBECONFIG | NERDCTL_* | PODMAN_* | XDG_* | CACTUSFS_* | PATH | HOME | LC_* | SSH_* | \
			BASH_ENV | ENV | SHELLOPTS | BASHOPTS | IFS | PS4 | LD_* | DYLD_*)
			bad TOOL_ENV_PASS "$_pn would give a tool what the script keeps from it on purpose (deploy/.env.example, TOOL_ENV_PASS)"
			;;
		*SECRET* | *PASSWORD* | *PASSPHRASE* | *TOKEN* | *CREDENTIAL*)
			bad TOOL_ENV_PASS "$_pn looks like a secret: a secret is never handed on through the environment"
			;;
		esac
		if script_vars | grep -qx "$_pn" || env_known "$_pn"; then
			bad TOOL_ENV_PASS "$_pn is a variable or setting of the script itself: a tool would be given the script's value"
		fi
	done
}

# image_ref <key>: empty, or a reference to an image.
image_ref() {
	_ref=$(setting "$1")
	case $_ref in
	'') ;;
	-* | *[!A-Za-z0-9_./:@-]*) bad "$1" "must be an image reference, such as registry.example.test/cactusfs:0.7.0" ;;
	esac
}

# size <key>: a size of a claim, such as 100Gi.
size() {
	_size=$(setting "$1")
	case $_size in
	*Mi | *Gi | *Ti) _size=${_size%?i} ;;
	*) _size=x ;;
	esac
	case $_size in
	'' | 0* | *[!0123456789]*) bad "$1" "must be a whole number with Mi, Gi or Ti, such as 100Gi" ;;
	esac
}

# together <key> <key> <what happens with neither>: both are given, or
# neither.
together() {
	if [ -z "$(setting "$1")" ]; then
		[ -z "$(setting "$2")" ] && return 0
	else
		[ -n "$(setting "$2")" ] && return 0
	fi
	refuse "$1 and $2 are given together, or neither (then $3)"
}

# allow_from <key>: empty, "any", or addresses and ranges (CIDRs) separated
# by commas.
allow_from() {
	case $(setting "$1") in
	'' | any) ;;
	*) chars "$1" '0123456789abcdefABCDEF.:/,' "'any', or IP addresses and CIDRs separated by commas" ;;
	esac
}

# path <key>: a path on this machine (already made absolute).
path() {
	case $(setting "$1") in
	'' | /*) ;;
	*) bad "$1" "must be a path" ;;
	esac
}

# secret_file <key>: empty, or a file that can be read. Only whether it is
# there is asked: what it holds is never read into the script (PLAN 10.3
# streams it to where it goes).
secret_file() {
	path "$1"
	_secret_path=$(setting "$1")
	[ -z "$_secret_path" ] || { [ -f "$_secret_path" ] && [ -r "$_secret_path" ]; } || bad "$1" "is not a file that can be read"
}

# k3s_import_words: the words of K3S_IMPORT, one a line. Split at spaces
# only, with no shell: a character a shell would read is refused first.
k3s_import_words() {
	_import=$(setting K3S_IMPORT)
	case $_import in
	*[!A-Za-z0-9_./=:@+,' '-]*)
		bad K3S_IMPORT "is a command split at spaces and run without a shell: letters, digits, spaces and _./=:@+,- only (no quotes, \$, backtick, ;, |, &, <, >, *)"
		;;
	esac
	# Pathname expansion is off for the whole script (set -f): only the
	# split at spaces happens here.
	_old_ifs=$IFS
	IFS=' '
	# shellcheck disable=SC2086 # the split is the point
	set -- $_import
	IFS=$_old_ifs
	if [ -n "$_import" ] && [ $# -eq 0 ]; then
		bad K3S_IMPORT "holds no word: leave it empty, or write the command that imports an image"
	fi
	case ${1:-} in
	-*) bad K3S_IMPORT "begins with '$1': the first word is the program, and only the words after it may begin with '-'" ;;
	esac
	for _w in "$@"; do
		printf '%s\n' "$_w"
	done
}

# sync_root <directory>: true when the directory is in a folder that a file
# sync service copies elsewhere - no place for secrets.
sync_root() {
	_home=${HOME:-/nonexistent}
	case $1 in
	"$_home/Library/Mobile Documents"* | "$_home/Library/CloudStorage"* | "$_home/Documents" | "$_home/Documents/"* | "$_home/Desktop" | "$_home/Desktop/"* | *OneDrive* | *Dropbox* | *iCloud* | *"Google Drive"* | *GoogleDrive*) return 0 ;;
	esac
	return 1
}

# physical <path>: an absolute path with the symbolic links of the part of it
# that exists resolved - the longest leading part that is a directory,
# entered with "cd -P" and named by the external "pwd -P", which returns the
# case the filesystem stores (bash's builtin keeps the case as typed) - and
# the rest, which does not exist yet, appended as it is. Fails with 1 when
# that directory cannot be entered, and with 2 when a part of the rest is a
# symbolic link after all (one that points nowhere yet: it could point into
# the repository once its target is made).
physical() {
	_phys_base=$1
	_phys_rest=
	while [ ! -d "$_phys_base" ]; do
		[ ! -L "$_phys_base" ] || return 2
		_phys_rest=/${_phys_base##*/}$_phys_rest
		_phys_base=${_phys_base%/*}
		[ -n "$_phys_base" ] || _phys_base=/
	done
	_phys_base=$(CDPATH='' cd -P -- "$_phys_base" && env pwd -P) || return 1
	printf '%s%s' "${_phys_base%/}" "$_phys_rest"
}

# env_validate: every setting, on its own and with the others. Nothing is
# asked of a machine here.
env_validate() {
	case $(setting DEPLOY_TARGET) in
	'') ;;
	*) one_of DEPLOY_TARGET $TARGETS ;;
	esac
	dns_label DEPLOY_NAME
	one_of DEPLOY_MODE standalone cluster
	case $(setting KMS) in
	'') refuse "KMS is not set: write KMS=local (a sealed key file, as deploy/.env.example explains) or KMS=vault" ;;
	*) one_of KMS local vault ;;
	esac
	yes_no ALLOW_SHARED_LOCAL_KMS
	if [ "$(setting KMS)" = local ] && [ "$(setting DEPLOY_MODE)" = cluster ] && [ "$(setting ALLOW_SHARED_LOCAL_KMS)" != yes ]; then
		refuse "KMS=local with DEPLOY_MODE=cluster shares one key file among the nodes: use KMS=vault, or say ALLOW_SHARED_LOCAL_KMS=yes"
	fi
	secret_file KMS_KEY_FILE
	secret_file KMS_PASSPHRASE_FILE
	if [ -n "$(setting KMS_KEY_FILE)" ]; then
		[ "$(setting KMS)" = local ] || refuse "KMS_KEY_FILE is the key file of KMS=local; with KMS=$(setting KMS) leave it empty"
		[ -n "$(setting KMS_PASSPHRASE_FILE)" ] || refuse "KMS_KEY_FILE needs KMS_PASSPHRASE_FILE: a key file opens only with its passphrase"
	fi
	case $(setting VAULT_ADDR) in
	'' | https://*) ;;
	*) bad VAULT_ADDR "must be https://host:port" ;;
	esac
	word VAULT_ADDR
	word VAULT_TRANSIT_MOUNT
	word VAULT_KEY
	secret_file VAULT_TOKEN_FILE
	path VAULT_CA_FILE
	word VAULT_NAMESPACE
	if [ "$(setting KMS)" = vault ]; then
		for _key in VAULT_ADDR VAULT_KEY VAULT_TOKEN_FILE; do
			[ -n "$(setting "$_key")" ] || refuse "KMS=vault needs $_key"
		done
	fi
	secret_file ROOT_ACCESS_KEY_FILE
	secret_file ROOT_SECRET_KEY_FILE
	together ROOT_ACCESS_KEY_FILE ROOT_SECRET_KEY_FILE "they are generated"

	path STATE_DIR
	case /$(setting STATE_DIR)/ in
	*/../*) bad STATE_DIR "holds a '..': write the directory without one" ;;
	esac
	_phys_status=0
	_state_physical=$(physical "$(setting STATE_DIR)") || _phys_status=$?
	case $_phys_status in
	0) ;;
	2) bad STATE_DIR "has a symbolic link that points nowhere on its way: it could point into the repository later; remove it or write the directory without it" ;;
	*) bad STATE_DIR "cannot be resolved: a directory on its way cannot be entered" ;;
	esac
	for _state_path in "$(setting STATE_DIR)" "$_state_physical"; do
		case $_state_path in
		"$REPO_ROOT" | "$REPO_ROOT"/*) bad STATE_DIR "is inside the repository ($_state_physical with its links resolved): it holds secrets, and must be outside it" ;;
		esac
	done
	if sync_root "$(setting STATE_DIR)" || sync_root "$_state_physical"; then
		warn "STATE_DIR $(setting STATE_DIR) is in a folder a sync service may copy elsewhere; it will hold secrets - set STATE_DIR outside it"
	fi
	yes_no OFFLINE
	one_of IMAGE_SOURCE binaries source image
	path RELEASE_DIR
	image_ref IMAGE
	if [ "$(setting IMAGE_SOURCE)" = image ] && [ -z "$(setting IMAGE)" ]; then
		refuse "IMAGE_SOURCE=image needs IMAGE, the reference of the image"
	fi
	ssh_host BUILD_HOST
	yes_no ALLOW_DIRTY
	word ENGINE_CONTEXT

	number DRIVES 1 4
	chars BIND '0123456789abcdefABCDEF.:' "an IP address"
	chars ADMIN_BIND '0123456789abcdefABCDEF.:' "an IP address"
	[ -n "$(setting BIND)" ] || bad BIND "must be an IP address"
	if [ "$(setting DEPLOY_MODE)" = cluster ] && [ "$(lookup "$(env_defaults)" BIND)" != "$(setting BIND)" ]; then
		bad BIND "cannot be set in cluster mode: the ports are those deploy/compose/compose.yaml publishes, and an override cannot change them, because compose adds port bindings rather than replacing them"
	fi
	[ -n "$(setting ADMIN_BIND)" ] || bad ADMIN_BIND "must be an IP address"
	number S3_PORT 1024 65535
	number ADMIN_PORT 1024 65535
	[ "$(setting S3_PORT)" != "$(setting ADMIN_PORT)" ] || refuse "S3_PORT and ADMIN_PORT are the same port"

	chars TLS_HOSTS 'A-Za-z0-9.:,*-' "DNS names and IP addresses separated by commas"
	# How long dev-init's certificates are valid, in hours: from a day to
	# ten years.
	_validity=$(setting TLS_VALIDITY)
	case $_validity in
	[123456789]*h) ;;
	*) bad TLS_VALIDITY "must be a number of hours, such as 8760h (a year)" ;;
	esac
	case ${_validity%h} in
	*[!0123456789]*) bad TLS_VALIDITY "must be a number of hours, such as 8760h (a year)" ;;
	esac
	[ "${#_validity}" -le 7 ] && [ "${_validity%h}" -ge 24 ] && [ "${_validity%h}" -le 87600 ] ||
		bad TLS_VALIDITY "must be from 24h to 87600h (ten years)"
	path TLS_CERT_FILE
	secret_file TLS_KEY_FILE
	path TLS_CA_FILE
	together TLS_CERT_FILE TLS_KEY_FILE "a development certificate is made"
	path NODE_CA_FILE
	path NODE_CERT_DIR

	ssh_host CE_HOST
	yes_no CE_INSTALL_DOCKER
	yes_no CE_INSTALL_DOCKER_FORCE
	case $(setting CE_STATE_DIR) in
	'~'*) bad CE_STATE_DIR "is a directory on the server: write it relative to the home directory there, or absolute" ;;
	esac
	word CE_STATE_DIR
	[ -n "$(setting CE_STATE_DIR)" ] || bad CE_STATE_DIR "must be a directory"

	one_of RANCHER_ENGINE auto moby containerd
	yes_no RANCHER_KUBERNETES

	ssh_host K3S_SSH
	k3s_import_words >/dev/null
	word KUBECTL
	[ -n "$(setting KUBECTL)" ] || bad KUBECTL "must name kubectl"
	no_equals KUBECTL
	word K8S_CONTEXT
	tool_env_pass
	dns_label K8S_NAMESPACE empty
	image_ref K8S_REGISTRY
	one_of K8S_STORAGE pvc hostpath
	case $(setting K8S_STORAGE_CLASS) in
	'') ;;
	*) chars K8S_STORAGE_CLASS 'a-z0-9.-' "lower-case letters, digits, '.' and '-'" ;;
	esac
	size K8S_DRIVE_SIZE
	size K8S_META_SIZE
	one_of K8S_SERVICE_TYPE ClusterIP NodePort LoadBalancer
	number K8S_NODE_PORT 30000 32767 empty
	allow_from K8S_S3_ALLOW_FROM
	one_of K8S_ADMIN_SERVICE_TYPE ClusterIP NodePort LoadBalancer
	number K8S_ADMIN_NODE_PORT 30000 32767 empty
	allow_from K8S_ADMIN_ALLOW_FROM
	image_ref HELM_IMAGE
	word HELM
	no_equals HELM
	number WAIT_TIMEOUT 1 86400

	if [ "$(setting OFFLINE)" = yes ]; then
		[ "$(setting CE_INSTALL_DOCKER)" != yes ] || refuse "OFFLINE=yes and CE_INSTALL_DOCKER=yes: installing Docker CE fetches it from Docker's repository"
		[ "$(setting IMAGE_SOURCE)" != source ] || refuse "OFFLINE=yes and an image built from source (--from-source, IMAGE_SOURCE=source): the build pulls the Dockerfile's base images, Go modules and npm packages; use IMAGE_SOURCE=binaries or an IMAGE that is here"
		# OFFLINE refuses public pulls only: K8S_REGISTRY, the operator's own
		# registry, stays allowed. Every image the run needs must be present
		# (HELM_IMAGE or HELM, the runtime image, IMAGE): checked with "image
		# inspect" where each is used, and built with "docker build
		# --pull=false" or "podman build --pull=never" (PLAN 10.4, 10.9).
	fi
}

# env_show: every setting and where it came from, then what passes to the
# nodes. For --dry-run.
env_show() {
	say "settings (a flag, then ${ENV_FILE:-no env file}, then the defaults):"
	while IFS= read -r _line; do
		[ -n "$_line" ] || continue
		_key=${_line%%=*}
		say "setting $_key=$(setting "$_key") ($(lookup "$SOURCES" "$_key"))"
	done <<EOF
$(env_defaults)
EOF
	while IFS= read -r _line; do
		[ -z "$_line" ] || say "node setting $_line"
	done <<EOF
$PASSED
EOF
}

# --- the tools' environment -------------------------------------------------------

# Every tool runs under "env -i" (tool, in common.sh): nothing of the
# caller's environment points it at another engine or cluster than the
# settings say. What it is pointed at is said on its command line:

# docker_context: the context the Docker CLI is given, always explicitly:
# ENGINE_CONTEXT, else rancher-desktop for Rancher Desktop, else "default",
# the local daemon.
docker_context() {
	_context=$(setting ENGINE_CONTEXT)
	if [ -n "$_context" ]; then
		printf '%s' "$_context"
	elif [ "${TARGET:-}" = rancher ]; then
		printf 'rancher-desktop'
	else
		printf 'default'
	fi
}

# kube_context: the context kubectl is given - K8S_CONTEXT, or for Rancher
# Desktop's Kubernetes rancher-desktop. Empty: none is named (k3s, which
# runs its own kubectl).
kube_context() {
	_context=$(setting K8S_CONTEXT)
	if [ -n "$_context" ]; then
		printf '%s' "$_context"
	elif [ "${TARGET:-}" = rancher ]; then
		printf 'rancher-desktop'
	fi
}

# --- the target -----------------------------------------------------------------

# Containers and volumes the script makes carry the label
# io.cactusfs.deploy.name=<DEPLOY_NAME> and io.cactusfs.deploy.target=<target>;
# a namespace it makes carries the same two labels. That is how what runs is
# recognised, and what limits what the script touches.
LABEL_NAME=io.cactusfs.deploy.name
LABEL_TARGET=io.cactusfs.deploy.target
# The annotation that marks a namespace as the script's own: a random UID,
# also kept in the deployment's STATE_DIR entry (ADR-0033 section 7; made by
# PLAN 10.9).
MARKER_ANNOTATION=deploy.cactusfs.io/owner-uid

# FOUND: the targets the probes saw, one a line.
FOUND=

found() {
	case $1 in
	'') return 0 ;;
	esac
	for _target in $TARGETS; do
		if [ "$1" = "$_target" ]; then
			printf '%s\n' "$FOUND" | grep -qx "$1" || FOUND="$FOUND$1
"
			return 0
		fi
	done
	warn "a deployment named $(setting DEPLOY_NAME) carries the target '$1', which this script does not know; not considered"
}

# printable: standard input with every character that is neither printable
# nor a line break taken out - for what an engine or a file answers, before
# it is printed or compared.
printable() {
	tr -cd '[:print:]\n'
}

# probe_engines: containers of the deployment under the Docker CLI (with
# the context of docker_context; the label says dd, ce or rancher), Podman,
# and nerdctl (Rancher Desktop with containerd). Read-only. An engine that
# is not installed or does not answer is not considered. (PLAN 10.5 and
# 10.6 refine what is asked.)
probe_engines() {
	_name=$(setting DEPLOY_NAME)
	if command -v docker >/dev/null 2>&1; then
		if _out=$(query docker --context "$(docker_context)" ps -a --filter "label=$LABEL_NAME=$_name" --format "{{.Label \"$LABEL_TARGET\"}}" </dev/null 2>/dev/null); then
			for _t in $(printf '%s\n' "$_out" | printable); do found "$_t"; done
		fi
	fi
	if command -v podman >/dev/null 2>&1; then
		if _out=$(query podman ps -a --filter "label=$LABEL_NAME=$_name" --format "{{index .Labels \"$LABEL_TARGET\"}}" </dev/null 2>/dev/null); then
			for _t in $(printf '%s\n' "$_out" | printable); do found "$_t"; done
		fi
	fi
	if command -v nerdctl >/dev/null 2>&1; then
		if _out=$(query nerdctl ps -a --filter "label=$LABEL_NAME=$_name" --format '{{.ID}}' </dev/null 2>/dev/null) && [ -n "$_out" ]; then
			found rancher
		fi
	fi
}

# probe_kubernetes: a namespace of the deployment - through KUBECTL in the
# context K8S_CONTEXT when one is set (never the current context of a
# kubeconfig the settings do not name), and through "k3s kubectl" on
# K3S_SSH when that is set, after its host key is checked. Read-only. Each
# Kubernetes target is asked for by its own label selector, name and target
# together, with "-o name": nothing of a column is parsed, and every word is
# plain enough for ssh. ssh's own messages are kept on standard error. (PLAN
# 10.10 refines what is asked.)
probe_kubernetes() {
	_name=$(setting DEPLOY_NAME)
	_context=$(setting K8S_CONTEXT)
	_kubectl=$(setting KUBECTL)
	_ssh=$(setting K3S_SSH)
	if [ -n "$_ssh" ] && command -v ssh >/dev/null 2>&1; then
		host_key_check "$_ssh"
	fi
	for _t in k3s k8s rancher; do
		set -- get namespaces -l "$LABEL_NAME=$_name,$LABEL_TARGET=$_t" -o name
		if [ -n "$_context" ] && command -v "$_kubectl" >/dev/null 2>&1; then
			_out=$(query "$_kubectl" --context "$_context" "$@" </dev/null 2>/dev/null) || _out=
			[ -z "$_out" ] || found "$_t"
		fi
		if [ -n "$_ssh" ] && command -v ssh >/dev/null 2>&1; then
			_out=$(query on_host "$_ssh" k3s kubectl "$@" </dev/null) || _out=
			[ -z "$_out" ] || found "$_t"
		fi
	done
}

# resolve_target: TARGET and how it was found - --docker, else DEPLOY_TARGET,
# else what runs under the name DEPLOY_NAME. Refused when nothing says, when
# what says is empty, or when more than one target runs a deployment of that
# name.
resolve_target() {
	TARGET=$(setting DEPLOY_TARGET)
	case $(lookup "$SOURCES" DEPLOY_TARGET) in
	flag) TARGET_FROM=--docker ;;
	file) TARGET_FROM=DEPLOY_TARGET ;;
	*)
		TARGET_FROM="what runs"
		probe_engines
		probe_kubernetes
		_count=$(printf '%s' "$FOUND" | grep -c . || :)
		case $_count in
		0) refuse "no target: nothing named $(setting DEPLOY_NAME) runs here; say --docker <$(echo "$TARGETS" | tr ' ' '|')> or DEPLOY_TARGET" ;;
		1) TARGET=$(printf '%s' "$FOUND" | tr -d '\n') ;;
		*) refuse "a deployment named $(setting DEPLOY_NAME) runs on more than one target ($(printf '%s' "$FOUND" | tr '\n' ' ' | sed 's/ $//')): say which with --docker" ;;
		esac
		;;
	esac
	[ -n "$TARGET" ] || refuse "no target: $TARGET_FROM names none; say --docker <$(echo "$TARGETS" | tr ' ' '|')> or DEPLOY_TARGET"
}

# on_kubernetes: true when the target is a Kubernetes cluster.
on_kubernetes() {
	case $TARGET in
	k3s | k8s) return 0 ;;
	rancher) [ "$(setting RANCHER_KUBERNETES)" = yes ] ;;
	*) return 1 ;;
	esac
}

# target_validate: the settings that depend on the target.
target_validate() {
	if [ "$TARGET" = k8s ] && [ -z "$(setting K8S_CONTEXT)" ]; then
		refuse "--docker k8s needs K8S_CONTEXT: the context of your kubeconfig to deploy to is named, never taken from what is current"
	fi
}

# engine_identity: one line that says which engine or cluster the tools are
# pointed at - printed before anything changes, and recorded with the state
# so that a later run that resolves differently is refused.
engine_identity() {
	case $TARGET in
	dd) printf 'docker --context %s' "$(docker_context)" ;;
	podman) printf 'podman (local)' ;;
	rancher)
		if on_kubernetes; then
			printf 'kubectl --context %s' "$(kube_context)"
		else
			printf 'Rancher Desktop, engine %s, docker --context %s' "$(setting RANCHER_ENGINE)" "$(docker_context)"
		fi
		;;
	ce)
		if [ -n "$(setting CE_HOST)" ]; then
			printf 'docker over ssh://%s' "$(setting CE_HOST)"
		else
			printf 'docker --context %s' "$(docker_context)"
		fi
		;;
	k3s)
		if [ -n "$(setting K3S_SSH)" ]; then
			printf 'k3s kubectl over ssh %s' "$(setting K3S_SSH)"
		elif [ -n "$(kube_context)" ]; then
			printf '%s --context %s' "$(setting KUBECTL)" "$(kube_context)"
		else
			printf 'k3s kubectl (local)'
		fi
		;;
	k8s) printf '%s --context %s' "$(setting KUBECTL)" "$(kube_context)" ;;
	esac
}

# target_id: a stable identity of the engine or cluster, which places the
# deployment's state: STATE_DIR/<target-id>/<name>/. For a target over ssh,
# the destination; for the others the target and its context. The
# engine's own ID is not part of the path: it is recorded in the entry and
# compared by every install (engine_id_check, PLAN 10.3); the cluster's,
# the UID of its kube-system namespace, is PLAN 10.10's.
target_id() {
	if [ "$TARGET" = ce ] && [ -n "$(setting CE_HOST)" ]; then
		_id="ssh-$(setting CE_HOST)"
	elif [ "$TARGET" = k3s ] && [ -n "$(setting K3S_SSH)" ]; then
		_id="ssh-$(setting K3S_SSH)"
	elif on_kubernetes; then
		_id="$TARGET-$(kube_context)"
		[ -n "$(kube_context)" ] || _id="$TARGET-local"
	else
		_id="$TARGET-$(docker_context)"
	fi
	# One path component, whatever the destination holds.
	printf '%s' "$_id" | tr -c 'A-Za-z0-9._-' '_'
}

# state_check: the first line of STATE_DIR/<target-id>/<name>/target
# records the settings' engine or cluster the deployment was made on
# (written at its first install by state_record, deploy/lib/secrets.sh,
# with the engine's own ID, which an install compares: engine_id_check).
# Refused (ADR-0033 section 15): a run whose settings point this entry at
# another one; and a name that is recorded under another target and not
# under this one - unless --another-target says a second deployment of
# that name is meant. Only read here, compared as text, never run.
state_check() {
	_state_dir=$(setting STATE_DIR)
	_name=$(setting DEPLOY_NAME)
	_tid=$(target_id)
	STATE=$_state_dir/$_tid/$_name
	if [ -f "$STATE/target" ]; then
		_recorded=$(head -n 1 "$STATE/target" | printable)
		[ "$_recorded" = "$(engine_identity)" ] ||
			refuse "$STATE was made on $_recorded; the settings now point at $(engine_identity): set them back, or remove the deployment first"
		return 0
	fi
	[ -d "$_state_dir" ] || return 0
	_elsewhere=
	for _other in $(ls -1 "$_state_dir"); do
		[ "$_other" != "$_tid" ] || continue
		[ -d "$_state_dir/$_other/$_name" ] || continue
		_elsewhere="$_elsewhere $_other"
	done
	if [ -n "$_elsewhere" ] && [ "$ANOTHER_TARGET" != 1 ]; then
		refuse "a deployment named $_name is recorded on another target ($(printf '%s' "$_elsewhere" | sed 's/^ //'), in $_state_dir) and not on this one ($_tid): check the settings, or say --another-target to deploy a second one"
	fi
}

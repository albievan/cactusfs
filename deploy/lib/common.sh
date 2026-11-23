# shellcheck shell=sh
# What every part of deploy/deploy.sh uses: how it speaks, how it fails, how
# it runs a command, and how it reaches another machine. Sourced by
# deploy/deploy.sh; not run on its own.
#
# Exit status of the script:
#   0  done
#   1  a command it ran failed
#   2  refused: a flag, a line of the env file or a setting is wrong; nothing
#      was changed
#   3  the action is not built yet (PLAN milestone 10); nothing was changed
#
# Secrets are never held in a shell variable, put on a command line or
# printed: they stream from their file to the standard input of the command
# that needs them (PLAN 10.3). So nothing here has a secret to hide, and a
# trace of the script (sh -x) shows none.

# say <text>: a line of progress, on standard output.
say() {
	printf 'deploy: %s\n' "$*"
}

# warn <text>: a warning, on standard error.
warn() {
	printf 'deploy: warning: %s\n' "$*" >&2
}

# refuse <text>: the script will not go on; nothing has been changed.
refuse() {
	printf 'deploy: %s\n' "$*" >&2
	exit 2
}

# fail <text>: something the script ran went wrong.
fail() {
	printf 'deploy: FAIL - %s\n' "$*" >&2
	exit 1
}

# plain <value>: true when the value cannot mean anything to a shell, so it
# may be put on the command line ssh hands to another machine's shell. The
# rule of scripts/remote.sh: letters, digits and _./=:@+,- only, not empty,
# not beginning with a hyphen (an option of ssh).
plain() {
	case $1 in
	*[!A-Za-z0-9_./=:@+,-]* | '' | -*) return 1 ;;
	esac
	return 0
}

# shown <argument>: the argument as it is printed - quoted when it holds
# anything a shell would read.
shown() {
	case $1 in
	*[!A-Za-z0-9_./=:@+,-]* | '')
		printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
		;;
	*) printf '%s' "$1" ;;
	esac
}

# show_command <word>...: one command line as it would be typed.
show_command() {
	_line=
	for _word in "$@"; do
		_line="$_line${_line:+ }$(shown "$_word")"
	done
	printf '%s\n' "$_line"
}

# The two line breaks, which no value of a flag, a setting or an argument
# handed to another machine may hold.
NL='
'
CR=$(printf '\r')

# tool [NAME=value] <command> <argument>...: runs a program that reaches an
# engine, a cluster or another machine (docker, podman, nerdctl, kubectl,
# k3s, ssh, curl, helm) under "env -i": it sees PATH, HOME and LC_ALL=C, and
# only what it needs besides - SSH_AUTH_SOCK for ssh and for an engine
# reached over ssh, XDG_RUNTIME_DIR for podman (rootless Podman finds its
# socket there), and the variables TOOL_ENV_PASS names that the caller's
# environment holds (validated in env.sh - never one that looks like a
# secret - and read with printenv) - never the rest of the caller's
# environment (DOCKER_HOST, DOCKER_CONFIG, CONTAINERS_CONF,
# CONTAINERD_ADDRESS, KUBECONFIG, CACTUSFS_*, ...: ADR-0033 section 15). The one variable that may be given is the
# engine of a target over ssh, DOCKER_HOST=ssh://<destination> or
# CONTAINER_HOST=ssh://<destination>, and only for a destination whose host
# key host_key_check has checked.
tool() {
	_tool_set=
	_tool_agent=0
	[ $# -gt 0 ] || fail "internal: tool without a command"
	case $1 in
	*=*)
		_tool_set=$1
		shift
		case $_tool_set in
		DOCKER_HOST=ssh://* | CONTAINER_HOST=ssh://*) ;;
		*) fail "internal: tool passes only DOCKER_HOST or CONTAINER_HOST=ssh://<destination> to a tool, not ${_tool_set%%=*}" ;;
		esac
		case $SSH_CHECKED in
		*" ${_tool_set#*=ssh://} "*) ;;
		*) fail "internal: the host key of ${_tool_set#*=ssh://} was not checked before its engine was used (host_key_check)" ;;
		esac
		_tool_agent=1
		;;
	esac
	[ $# -gt 0 ] || fail "internal: tool without a command"
	case $1 in
	'' | -* | *=*) fail "internal: '$1' is not a command tool runs" ;;
	ssh) _tool_agent=1 ;;
	podman) [ -z "${XDG_RUNTIME_DIR:-}" ] || set -- "XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR" "$@" ;;
	esac
	if [ "$_tool_agent" = 1 ] && [ -n "${SSH_AUTH_SOCK:-}" ]; then
		set -- "SSH_AUTH_SOCK=$SSH_AUTH_SOCK" "$@"
	fi
	_tool_pass=
	if [ -n "${SETTINGS:-}" ]; then
		_tool_pass=$(setting TOOL_ENV_PASS)
	fi
	_tool_ifs=$IFS
	IFS=,
	for _tool_name in $_tool_pass; do
		if _tool_value=$(printenv "$_tool_name"); then
			set -- "$_tool_name=$_tool_value" "$@"
		fi
	done
	IFS=$_tool_ifs
	[ -z "$_tool_set" ] || set -- "$_tool_set" "$@"
	# The engine of CE_HOST: the Docker CLI starts "ssh" from its PATH with
	# the user's configuration, so the PATH it is given begins with the
	# directory of the shim deploy/lib/ce.sh writes, which runs the real ssh
	# with the deployment's pinned host keys (ce_shim).
	_tool_path=${PATH-}
	if [ -n "${_ce_pin_host:-}" ] && [ "$_tool_set" = "DOCKER_HOST=ssh://$_ce_pin_host" ]; then
		_tool_path=$_ce_work/bin:$_tool_path
	fi
	env -i ${PATH+"PATH=$_tool_path"} ${HOME+"HOME=$HOME"} LC_ALL=C "$@"
}

# invoke <command> <argument>...: what run and query start - on_host as it
# is (it runs ssh through tool itself), anything else through tool.
invoke() {
	case $1 in
	on_host) "$@" ;;
	*) tool "$@" ;;
	esac
}

# run <command> <argument>...: a command that changes something. Printed;
# with --dry-run it is only printed.
run() {
	say "run: $(show_command "$@")"
	[ "$DRY_RUN" = 1 ] && return 0
	invoke "$@"
}

# query <command> <argument>...: a command that only reads - what runs, what
# exists. It runs with --dry-run too, which then prints it. A query that
# reads nothing from standard input is given </dev/null by its caller, so
# that ssh never waits on the terminal.
query() {
	[ "$DRY_RUN" = 1 ] && say "query: $(show_command "$@")" >&2
	invoke "$@"
}

# The destinations whose host key host_key_check has accepted in this run,
# each between spaces.
SSH_CHECKED=' '

# host_key_check <destination>: before a destination is first used - by
# on_host, or as the engine of DOCKER_HOST=ssh:// - ssh runs "true" there
# with ssh's own message kept on standard error. ssh's exit 255 (a host key
# it does not know or that has changed, or a host it cannot reach) refuses
# the run there and then, so nothing else is run on that machine.
host_key_check() {
	case $SSH_CHECKED in
	*" $1 "*) return 0 ;;
	esac
	plain "$1" || refuse "'$1' is not a destination this script hands to ssh"
	SSH_CHECKED="$SSH_CHECKED$1 "
	_hk_status=0
	query on_host "$1" true </dev/null || _hk_status=$?
	case $_hk_status in
	0) ;;
	255) refuse "ssh to $1 failed (exit 255, ssh's message is above): its host key is not in known_hosts, has changed, or the host did not answer; the script never accepts a host key it does not know - check the key's fingerprint with the server's administrator and add it to known_hosts as README.md says (Deploying with deploy/deploy.sh, adding a server's host key), then run again" ;;
	*) fail "ssh to $1 ran 'true' and it exited $_hk_status" ;;
	esac
}

# on_host <destination> <word>...: runs the words on another machine over
# ssh. ssh hands them to that machine's shell, so the destination must be
# plain (the rule of scripts/remote.sh) and so must every word, except that a
# word after the first may begin with a hyphen (an option of the command
# there, or "-" for standard input), as remote.sh lets a value do. No word
# may be empty (the remote shell would drop it), and the first is a command:
# plain, and without "=" (the remote shell would read NAME=value as an
# assignment). One that is not is refused, never quoted. ssh never asks,
# never accepts a host key it does not already know, and gives up on a host
# that does not answer within 20 seconds. Multiplexing is off
# (ControlPath=none): every connection is checked on its own. The
# destination's host key must have been checked first (host_key_check).
# For CE_HOST once its keys are pinned (deploy/lib/ce.sh), ssh is given the
# pin as its only known_hosts, and nothing else that could accept a key:
# no DNS (VerifyHostKeyDNS), no KnownHostsCommand (IgnoreUnknown keeps an
# ssh older than 8.5, which lacks it, from refusing the option), no keys
# learnt from the server (UpdateHostKeys) and no check of its address
# (CheckHostIP) - the same options as the shim's (ce_shim).
on_host() {
	_dest=$1
	shift
	plain "$_dest" || refuse "'$_dest' is not a destination this script hands to ssh"
	[ $# -gt 0 ] || fail "internal: on_host $_dest without a command"
	case $1 in
	*=*) refuse "'$1' would be read as an assignment by the shell of $_dest, not run: the first word is a command" ;;
	esac
	plain "$1" || refuse "'$1' is not a command this script hands to ssh"
	for _word in "$@"; do
		[ -n "$_word" ] || refuse "an empty word is not handed to ssh: the shell of $_dest would drop it"
		plain "x$_word" || refuse "'$_word' is not a word this script hands to ssh"
	done
	case $SSH_CHECKED in
	*" $_dest "*) ;;
	*) fail "internal: the host key of $_dest was not checked before it was used (host_key_check)" ;;
	esac
	if [ -n "${_ce_pin_host:-}" ] && [ "$_dest" = "$_ce_pin_host" ]; then
		tool ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=20 -o ControlPath=none \
			-o "UserKnownHostsFile=$_ce_work/known_hosts" -o GlobalKnownHostsFile=none \
			-o VerifyHostKeyDNS=no -o UpdateHostKeys=no -o CheckHostIP=no -o IgnoreUnknown=KnownHostsCommand -o KnownHostsCommand=none -- "$_dest" "$@"
	else
		tool ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=20 -o ControlPath=none -- "$_dest" "$@"
	fi
}

# The cleanups of the run: one EXIT trap for the whole script (PLAN 10.10),
# whose text is written again whenever a cleanup is added or dropped. Each
# cleanup is a command whose words - paths among them - are quoted by
# show_command when it is added, so that nothing is read when the trap
# fires: a function that later assigns a variable of the same name cannot
# point it elsewhere. They run last added, first: a work directory inside
# the deployment's entry goes before the entry's own cleanup (secrets_end).
# HUP, INT and TERM end the run as 129, 130 and 143, which runs them. A
# subshell that adds its own starts from none (cleanup_none): a subshell's
# trap must never run the cleanups of the shell that started it.
CLEANUPS=

# cleanup_add <word>...: a command the EXIT trap runs.
cleanup_add() {
	CLEANUPS="$(show_command "$@")$NL$CLEANUPS"
	cleanup_trap
}

# cleanup_drop <word>...: the command, as cleanup_add was given it, is no
# longer run by the trap (the caller has done it).
cleanup_drop() {
	_cd_line=$(show_command "$@")
	_cd_rest=
	while IFS= read -r _cd; do
		[ -n "$_cd" ] || continue
		[ "$_cd" = "$_cd_line" ] || _cd_rest="$_cd_rest$_cd$NL"
	done <<EOF
$CLEANUPS
EOF
	CLEANUPS=$_cd_rest
	cleanup_trap
}

# cleanup_none: in a subshell, none of the cleanups of the shell that
# started it.
cleanup_none() {
	CLEANUPS=
	trap - EXIT
}

# cleanup_trap: the traps, as CLEANUPS says.
cleanup_trap() {
	if [ -z "$CLEANUPS" ]; then
		trap - EXIT
		return 0
	fi
	_ct=
	while IFS= read -r _cd; do
		[ -z "$_cd" ] || _ct="$_ct$_cd; "
	done <<EOF
$CLEANUPS
EOF
	# shellcheck disable=SC2064 # expanded now, on purpose: see above
	trap "$_ct" EXIT
	trap 'exit 129' HUP
	trap 'exit 130' INT
	trap 'exit 143' TERM
}

# not_built <step> <text>: the stub of an action that a later step of PLAN
# milestone 10 builds. Says what it will do and ends the script.
not_built() {
	_step=$1
	shift
	say "$*"
	printf 'deploy: not built yet (PLAN %s)\n' "$_step" >&2
	exit 3
}

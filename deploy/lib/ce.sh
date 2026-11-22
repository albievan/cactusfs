# shellcheck shell=sh
# Docker CE on a server over SSH, CE_HOST=user@server (PLAN 10.7; ADR-0033
# sections 8, 12 and 16). Sourced by deploy/deploy.sh; not run on its own.
#
# A standalone deployment there is the one deploy/lib/docker.sh makes: its
# engine is the server's, reached with DOCKER_HOST=ssh://<CE_HOST>
# (target_engine, deploy/lib/secrets.sh), its secrets are made on this
# machine - dev-init runs on this machine's engine (local_engine) - and put
# into the server's labelled volumes. What this file adds:
#
#   The host keys, pinned by ssh itself. At the first install the script's
#   own ssh connects with BatchMode=yes and StrictHostKeyChecking=yes, so a
#   key the user's known_hosts does not hold is refused; then the lines of
#   those known_hosts files that name the server (ssh-keygen -F, under the
#   name ssh -G says ssh looks it up by: its HostKeyAlias, or its HostName
#   with a port other than 22 in brackets; hashed lines, @cert-authority and
#   @revoked lines as they are) are copied into a known_hosts of the run, and
#   ssh must accept the server with that file alone before anything else
#   runs; the install keeps it as STATE/ssh-known-hosts. From then on every
#   ssh to the server - the script's (on_host) and the Docker CLI's - runs
#   with -o UserKnownHostsFile=<the pin> -o GlobalKnownHostsFile=none
#   -o StrictHostKeyChecking=yes: ssh enforces the pin, a key it does not
#   hold is refused even if known_hosts was changed to accept it. Nothing is
#   read from ssh's messages, which a server could write to.
#
#   The Docker CLI's ssh. DOCKER_HOST=ssh:// makes the Docker CLI start
#   "ssh" from its PATH with the user's configuration, which could turn
#   host key checking off. Its PATH begins with a directory of the run
#   (tool, deploy/lib/common.sh) holding a shim named ssh that runs the real
#   ssh, by its absolute path, with the options above first (an option given
#   first wins over a later one and over the configuration).
#
#   CE_INSTALL_DOCKER=yes: Docker CE from Docker's own repository on a server
#   that has no Docker - Debian, Ubuntu, RHEL, Rocky Linux, AlmaLinux or
#   CentOS Stream, as /etc/os-release says. Everything is asked first, the
#   server's ports included; an unknown distribution is refused, and so are a
#   server where k3s, k0s, MicroK8s or Kubernetes is installed or runs
#   (Docker sets the iptables FORWARD policy to DROP, which breaks pod
#   networking) and one with podman-docker, unless CE_INSTALL_DOCKER_FORCE=yes.
#   Docker's signing key is fetched over HTTPS (never -k) into a directory of
#   its own, its fingerprint compared with the one Docker publishes, and only
#   then installed where the package manager trusts it. A user other than
#   root is added to the group docker only with CE_DOCKER_GROUP=yes. The one
#   fetch from the internet the script makes, refused under OFFLINE=yes
#   (env.sh).
#
#   The server's ports and /readyz. The node publishes the admin port on
#   the server's loopback address, so whether a port is free and whether
#   the node is ready are asked on the server, by its curl over ssh: the
#   probe of a port by curl's exit status (7: nothing answered), /readyz by
#   the status line of its headers (-D -), over TLS with the deployment's CA
#   streamed on ssh's standard input (--cacert /dev/stdin) - never -k, never
#   a proxy (--noproxy the name asked), the address given with --resolve,
#   and -q first, so no .curlrc there is read.
#
#   Where it is reached: install and --status print S3 at the HostName ssh
#   -G names for the server, and the ssh -L command that brings the admin
#   port to this machine.
#
# ssh is handed only plain words (on_host, deploy/lib/common.sh): a file the
# server needs - the repository's definition - goes on standard input to
# dd, never into a command line.

# _ce_trap: the cleanup of the run's work directory, a command whose path
# was written into it when the directory was made (ce_work_begin); the
# traps of secrets.sh carry it too. _ce_pin_host: the destination whose
# keys are pinned in the work directory - on_host and tool give it the pin.
_ce_trap=
_ce_pin_host=
_ce_work=

# ce_fingerprint <deb|rpm>: the fingerprint of the key Docker signs its
# packages with, as Docker's installation pages publish it - of
# https://download.docker.com/linux/{debian,ubuntu}/gpg, and of
# .../{centos,rhel}/gpg.
ce_fingerprint() {
	case $1 in
	deb) printf '9DC858229FC7DD38854AE2D88D81803C0EBFCD88' ;;
	rpm) printf '060A61C51B558A7F742B77AAC52FEB6B621E9F35' ;;
	esac
}

# ce_work_begin: the run's work directory (0700, in TMPDIR) for the pinned
# known_hosts and the ssh shim, removed when the run ends however it ends.
# Its path must be plain: it is handed to ssh in an option.
ce_work_begin() {
	umask 077
	_cw_tmp=${TMPDIR:-/tmp}
	_cw_tmp=${_cw_tmp%/}
	_ce_work=$(umask 077 && mktemp -d "$_cw_tmp/cactusfs-ce.XXXXXX") ||
		fail "could not make a directory in $_cw_tmp for the run (mktemp -d)"
	_ce_trap="ce_work_end $(shown "$_ce_work")"
	ce_trap_set
	case $_ce_work in
	*:*) _cw_ok= ;;
	*) _cw_ok=1 ;;
	esac
	if [ -z "$_cw_ok" ] || ! plain "$_ce_work"; then
		refuse "the run's directory $_ce_work is not a path ssh can be given in an option and PATH can begin with: set TMPDIR to a directory whose path is letters, digits and _./=@+,- only"
	fi
	mkdir "$_ce_work/bin"
}

# ce_trap_set: the cleanup of the work directory on exit, and an exit on the
# signals that end a run, so that the cleanup runs then too.
ce_trap_set() {
	trap "$_ce_trap" EXIT
	trap 'exit 129' HUP
	trap 'exit 130' INT
	trap 'exit 143' TERM
}

# ce_work_end <directory>: the work directory removed - only one of the form
# ce_work_begin makes, whatever it is given.
ce_work_end() {
	case $1 in
	*/cactusfs-ce.?*) rm -rf -- "$1" ;;
	esac
	return 0
}

# ce_shim: the ssh the Docker CLI finds first on its PATH - a script that
# runs the real ssh, found by its absolute path now, with the pin as its
# only known_hosts, strict checking, no prompt and no multiplexing, nothing
# else that could accept a key (DNS, KnownHostsCommand, keys the server
# offers, its address: on_host's pinned options), those options given
# first.
ce_shim() {
	_cs_ssh=$(command -v ssh) || refuse "ssh is not installed: CE_HOST is reached with it"
	case $_cs_ssh in
	/*) ;;
	*) refuse "ssh is not a program of this machine ('$_cs_ssh'): CE_HOST is reached with it" ;;
	esac
	{
		printf '#!/bin/sh\n'
		printf '# deploy/deploy.sh: the ssh of DOCKER_HOST=ssh://%s, with its pinned host keys.\n' "$_s_ce_host"
		printf 'exec %s -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=20 -o ControlPath=none -o %s -o GlobalKnownHostsFile=none -o VerifyHostKeyDNS=no -o UpdateHostKeys=no -o CheckHostIP=no -o IgnoreUnknown=KnownHostsCommand -o KnownHostsCommand=none "$@"\n' \
			"$(shown "$_cs_ssh")" "$(shown "UserKnownHostsFile=$_ce_work/known_hosts")"
	} >"$_ce_work/bin/ssh"
	chmod 0700 "$_ce_work/bin/ssh"
}

# ce_check <why>: ssh connects to CE_HOST and runs "true" (on_host: with the
# pin once _ce_pin_host names it, else with the user's known_hosts); ssh's
# exit 255 refuses the run, saying why with ssh's own message above it.
ce_check() {
	case $SSH_CHECKED in
	*" $_s_ce_host "*) ;;
	*) SSH_CHECKED="$SSH_CHECKED$_s_ce_host " ;;
	esac
	_cc_status=0
	query on_host "$_s_ce_host" true </dev/null || _cc_status=$?
	case $_cc_status in
	0) ;;
	255) refuse "ssh to $_s_ce_host failed (exit 255, ssh's message is above): $1" ;;
	*) fail "ssh to $_s_ce_host ran 'true' and it exited $_cc_status" ;;
	esac
}

# ce_ssh_config: what ssh -G says of CE_HOST, read on this machine: the host
# it connects to (_ce_hostname), the name its keys are looked up by in
# known_hosts (_ce_keyname) and the known_hosts files it reads
# (_ce_khfiles).
ce_ssh_config() {
	_sg=$(query ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=20 -o ControlPath=none -G -- "$_s_ce_host" </dev/null) ||
		fail "ssh -G could not read ssh's configuration for $_s_ce_host (its message is above)"
	_sg=$(printf '%s\n' "$_sg" | printable)
	_ce_hostname=$(printf '%s\n' "$_sg" | sed -n 's/^hostname //p' | head -n 1)
	_sg_port=$(printf '%s\n' "$_sg" | sed -n 's/^port //p' | head -n 1)
	_sg_alias=$(printf '%s\n' "$_sg" | sed -n 's/^hostkeyalias //p' | head -n 1)
	_ce_khfiles="$(printf '%s\n' "$_sg" | sed -n 's/^userknownhostsfile //p' | head -n 1) $(printf '%s\n' "$_sg" | sed -n 's/^globalknownhostsfile //p' | head -n 1)"
	case $_ce_hostname in
	'' | *[!A-Za-z0-9.:_-]*) fail "ssh -G names no host for $_s_ce_host that the script can use ('$_ce_hostname')" ;;
	esac
	if [ -n "$_sg_alias" ] && [ "$_sg_alias" != none ]; then
		_ce_keyname=$_sg_alias
	elif [ -n "$_sg_port" ] && [ "$_sg_port" != 22 ]; then
		_ce_keyname="[$_ce_hostname]:$_sg_port"
	else
		_ce_keyname=$_ce_hostname
	fi
}

# ce_pin_find: the lines of the known_hosts files ssh reads that name the
# server, as ssh-keygen -F finds them (hashed, @cert-authority and @revoked
# lines included, as they are), each once, into the run's known_hosts.
ce_pin_find() {
	ce_khfile_lines "$_ce_work/known_hosts"
	awk '!seen[$0]++' "$_ce_work/known_hosts" >"$_ce_work/known_hosts.new"
	mv -f "$_ce_work/known_hosts.new" "$_ce_work/known_hosts"
	[ -s "$_ce_work/known_hosts" ] ||
		fail "ssh accepted the host key of $_s_ce_host, and ssh-keygen -F found no line for $_ce_keyname in the known_hosts files ssh -G names ($(printf '%s' "$_ce_khfiles" | sed 's/^ *//')): the key cannot be pinned; put the server's key in one of them under that name"
}

# ce_khfile_lines <file>: what ssh-keygen -F finds for _ce_keyname in each
# known_hosts file ssh -G names that is there (~/ expanded), written into
# the file, comments and empty lines left out. A failure of ssh-keygen other
# than "not found" fails the run (called plainly, never in $(...)).
ce_khfile_lines() {
	: >"$1"
	for _kl in $_ce_khfiles; do
		case $_kl in
		none) continue ;;
		'~/'*) _kl=$HOME/${_kl#'~/'} ;;
		esac
		[ -f "$_kl" ] || continue
		_kl_status=0
		_kl_out=$(query ssh-keygen -F "$_ce_keyname" -f "$_kl" </dev/null) || _kl_status=$?
		case $_kl_status in
		0 | 1) ;;
		*) fail "ssh-keygen -F $_ce_keyname -f $_kl exited $_kl_status (its message is above)" ;;
		esac
		printf '%s\n' "$_kl_out" | printable | sed -e '/^#/d' -e '/^$/d' >>"$1"
	done
}

# ce_revoked: on a run with a pin, the @revoked lines the user's known_hosts
# files hold for the server are added to the run's copy of the pin - a key
# revoked since the first install is refused - and nothing else of them: a
# key they accept that the pin does not hold is never taken. The pin in
# STATE is not changed.
ce_revoked() {
	ce_khfile_lines "$_ce_work/found"
	_rv_lines=$(sed -n '/^@revoked /p' "$_ce_work/found")
	[ -n "$_rv_lines" ] || return 0
	printf '%s\n' "$_rv_lines" >>"$_ce_work/known_hosts"
	awk '!seen[$0]++' "$_ce_work/known_hosts" >"$_ce_work/known_hosts.new"
	mv -f "$_ce_work/known_hosts.new" "$_ce_work/known_hosts"
	say "host key of $_s_ce_host: known_hosts revokes $(printf '%s\n' "$_rv_lines" | wc -l | tr -d ' ') key(s) for $_ce_keyname, refused with the pin"
}

# ce_begin <install|status|uninstall|connect>: the first thing a run on
# CE_HOST does, before the engine there is used: the work directory and the
# pin - the pinned keys checked, or at a first contact the user's
# known_hosts and then the lines copied from it - and the shim. For an
# install, the questions and refusals that need no engine, then
# CE_INSTALL_DOCKER=yes. "connect" is the pin alone: a compose cluster
# (deploy/lib/compose.sh) asks its own refusals first and then runs
# ce_install_docker itself.
ce_begin() {
	ce_work_begin
	if [ -f "$STATE/ssh-known-hosts" ]; then
		cat "$STATE/ssh-known-hosts" >"$_ce_work/known_hosts"
		ce_ssh_config
		ce_revoked
		_ce_pin_host=$_s_ce_host
		ce_shim
		ce_check "its host key is not one of those pinned in $STATE/ssh-known-hosts at the deployment's first install (or it is revoked in known_hosts, or it did not answer); nothing else was run there. If the server's key was replaced on purpose, check the new fingerprint with its administrator, then remove $STATE/ssh-known-hosts - the next --install pins the keys known_hosts then holds"
		say "host key of $_s_ce_host: one of those pinned in $STATE/ssh-known-hosts"
	else
		ce_check "its host key is not in known_hosts, has changed, or the host did not answer; the script never accepts a host key it does not know - check the key's fingerprint with the server's administrator and add it to known_hosts as README.md says (Deploying with deploy/deploy.sh, adding a server's host key), then run again"
		ce_ssh_config
		ce_pin_find
		_ce_pin_host=$_s_ce_host
		ce_shim
		ce_check "known_hosts accepted its key, and the lines copied from it for $_ce_keyname do not: the key cannot be pinned"
		say "host key of $_s_ce_host: accepted by known_hosts; its lines for $_ce_keyname are used alone from now on, and an install pins them in $STATE/ssh-known-hosts"
	fi
	if [ "$1" = install ] && [ "$(setting CE_INSTALL_DOCKER)" = yes ]; then
		ce_install_docker
	fi
}

# ce_pin_record: at an install, once secrets_install has written the
# deployment's entry, the keys this run used are pinned there when none are
# - at the first install, or the first after an entry made without them.
ce_pin_record() {
	[ ! -f "$STATE/ssh-known-hosts" ] || return 0
	if [ "$DRY_RUN" = 1 ]; then
		say "dry run: would pin the host keys of $_s_ce_host ($_ce_keyname) in $STATE/ssh-known-hosts"
		return 0
	fi
	copy_new "$_ce_work/known_hosts" "$STATE/ssh-known-hosts"
	say "state: pinned the host keys of $_s_ce_host ($_ce_keyname) in $STATE/ssh-known-hosts: from now on ssh accepts that server with them alone"
}

# ce_ask_raw <word>...: a question to CE_HOST - _ce_out, what it printed
# (only printable characters), and _ce_status, its exit status (255: ssh
# failed); the question's own messages are not shown (a missing file, a
# command not found). Standard input is the caller's: </dev/null, or the CA
# of /readyz.
ce_ask_raw() {
	[ "$DRY_RUN" = 0 ] || say "query: $(show_command on_host "$_s_ce_host" "$@")" >&2
	_ce_status=0
	_ce_out=$(on_host "$_s_ce_host" "$@" 2>/dev/null) || _ce_status=$?
	_ce_out=$(printf '%s\n' "$_ce_out" | printable)
}

# ce_ask <word>...: ce_ask_raw, and ssh's exit 255 fails the run.
ce_ask() {
	ce_ask_raw "$@"
	[ "$_ce_status" != 255 ] ||
		fail "ssh to $_s_ce_host failed (exit 255) on the question '$(show_command "$@")': the server answered a moment ago; run again"
}

# ce_dial <address>: the address the server's curl dials for a port
# published on it - the loopback address for every address, IPv6 without
# brackets (curl's --resolve takes it so, and brackets are not plain).
ce_dial() {
	case $1 in
	0.0.0.0) printf '127.0.0.1' ;;
	:: | '[::]') printf '::1' ;;
	*) printf '%s' "$1" ;;
	esac
}

# ce_port_answered <address> <port>: _ce_answered - "free" when nothing
# answers on the port of that address of the server, asked there by curl,
# whose exit 7 says nothing did; "used" for any other answer; "nocurl" when
# the server has no curl.
ce_port_answered() {
	ce_ask curl -q -s -o /dev/null --max-time 3 --noproxy port.invalid \
		--resolve "port.invalid:$2:$(ce_dial "$1")" "http://port.invalid:$2/" </dev/null
	case $_ce_status in
	7) _ce_answered=free ;;
	127) _ce_answered=nocurl ;;
	*) _ce_answered=used ;;
	esac
}

# ce_ports_free [later]: S3_PORT on BIND and ADMIN_PORT on ADMIN_BIND of the
# server answer nothing - asked before Docker CE is installed (nothing of the
# deployment holds them then). A server without curl is refused - or, with
# "later", when CE_INSTALL_DOCKER will install curl, said: docker_ports_check
# asks again once it is there.
ce_ports_free() {
	for _cp_pair in "$_dk_bind $_dk_s3_port S3_PORT" "$_dk_admin_bind $_dk_admin_port ADMIN_PORT"; do
		_cp_addr=${_cp_pair%% *}
		_cp_rest=${_cp_pair#* }
		_cp_port=${_cp_rest% *}
		ce_port_answered "$_cp_addr" "$_cp_port"
		case $_ce_answered in
		used) refuse "port $_cp_port on $_cp_addr of $_s_ce_host is in use (something answered there): choose another ${_cp_rest#* }, or stop what holds it; nothing was changed" ;;
		nocurl)
			[ "${1:-}" = later ] ||
				refuse "curl is needed on $_s_ce_host: the script asks with it, there, whether a port is free and whether the node is ready; install curl there; nothing was changed"
			say "ports: $_s_ce_host has no curl yet; its ports are asked once the install has put it there"
			return 0
			;;
		esac
	done
}

# ce_readyz: _dk_ready, the HTTP status /readyz of the admin port answers on
# the server, or 000 when nothing does or ssh fails - asked by the server's
# curl, over TLS with the deployment's CA from STATE_DIR given on standard
# input (else the server's CAs), for the name docker_tls_name gives, at the
# admin address. Never through a proxy, never without verifying.
ce_readyz() {
	_cr_name=$(docker_tls_name)
	_cr_in=/dev/null
	set -- curl -q -s -o /dev/null -D - --max-time 5 --noproxy "$_cr_name" \
		--resolve "$_cr_name:$_dk_admin_port:$(ce_dial "$_dk_admin_bind")"
	if [ -f "$STATE/config/s3-ca.pem" ]; then
		set -- "$@" --cacert /dev/stdin
		_cr_in=$STATE/config/s3-ca.pem
	fi
	set -- "$@" "https://$_cr_name:$_dk_admin_port/readyz"
	ce_ask_raw "$@" <"$_cr_in"
	_dk_ready=
	[ "$_ce_status" = 255 ] ||
		_dk_ready=$(printf '%s\n' "$_ce_out" | sed -n 's|^HTTP/[0-9.]* \([0-9][0-9][0-9]\).*|\1|p' | tail -n 1)
	[ -n "$_dk_ready" ] || _dk_ready=000
}

# ce_reach: where a deployment on CE_HOST is reached - S3 on the server's
# BIND, at the host ssh connects to, and the admin port on its loopback
# address, brought here by ssh -L.
ce_reach() {
	case $_ce_hostname in
	*:*) _cr_server="[$_ce_hostname]" ;;
	*) _cr_server=$_ce_hostname ;;
	esac
	case $_dk_bind in
	0.0.0.0 | ::) _cr_s3="https://$_cr_server:$_dk_s3_port" ;;
	127.* | ::1) _cr_s3="https://$(docker_dial "$_dk_bind"):$_dk_s3_port on $_cr_server only (ssh -L as for the admin port)" ;;
	*) _cr_s3="https://$(docker_dial "$_dk_bind"):$_dk_s3_port" ;;
	esac
	say "S3: $_cr_s3"
	_cr_admin=$(docker_dial "$_dk_admin_bind")
	say "admin API and console: https://$_cr_admin:$_dk_admin_port on $_cr_server; from this machine: ssh -N -L $_dk_admin_port:$_cr_admin:$_dk_admin_port -- $_s_ce_host (another free local port in place of the first $_dk_admin_port if that one is taken), then https://localhost:$_dk_admin_port"
}

# ce_os_value <key> <os-release>: the value of the key, its quotes taken off.
ce_os_value() {
	printf '%s\n' "$2" | sed -n "s/^$1=//p" | head -n 1 | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\\(.*\\)'\$/\\1/"
}

# ce_distribution <os-release>: what Docker CE is installed from on this
# distribution - _ci_family (deb or rpm), _ci_repo (the directory of
# download.docker.com/linux/), _ci_suite (Debian's and Ubuntu's release
# name), _ci_key (where its signing key goes), _ci_list (the repository's
# definition) and _ci_os (its name) - by ID alone: a derivative that only
# says ID_LIKE is not one Docker publishes packages for, and is refused.
ce_distribution() {
	_cd_id=$(ce_os_value ID "$1")
	_cd_like=$(ce_os_value ID_LIKE "$1")
	_ci_os=$(ce_os_value PRETTY_NAME "$1")
	[ -n "$_ci_os" ] || _ci_os=$_cd_id
	_ci_suite=
	case $_cd_id in
	debian | ubuntu)
		_ci_family=deb
		_ci_repo=$_cd_id
		_ci_suite=$(ce_os_value VERSION_CODENAME "$1")
		if [ -z "$_ci_suite" ] && [ "$_cd_id" = ubuntu ]; then
			_ci_suite=$(ce_os_value UBUNTU_CODENAME "$1")
		fi
		case $_ci_suite in
		'' | *[!a-z0-9]*) refuse "CE_INSTALL_DOCKER: /etc/os-release of $_s_ce_host ($_ci_os) names no release (VERSION_CODENAME) that Docker's repository could be chosen by; install Docker CE there yourself, then run without CE_INSTALL_DOCKER; nothing was changed" ;;
		esac
		_ci_key=/etc/apt/keyrings/docker.asc
		_ci_list=/etc/apt/sources.list.d/docker.sources
		;;
	rhel | rocky | almalinux | centos)
		_ci_family=rpm
		_ci_repo=centos
		[ "$_cd_id" != rhel ] || _ci_repo=rhel
		_ci_key=/etc/pki/rpm-gpg/docker-ce.asc
		_ci_list=/etc/yum.repos.d/docker-ce.repo
		;;
	*)
		refuse "CE_INSTALL_DOCKER: $_s_ce_host runs $_ci_os (ID=$_cd_id${_cd_like:+, ID_LIKE=$_cd_like}), which is not a distribution the script installs Docker CE on - Debian, Ubuntu, RHEL, Rocky Linux, AlmaLinux and CentOS Stream are; install Docker CE there yourself, then run without CE_INSTALL_DOCKER (CE_INSTALL_DOCKER_FORCE does not change this); nothing was changed"
		;;
	esac
}

# ce_repo_text: the definition of Docker's repository for the distribution,
# as Docker's installation pages write it - for apt the deb822 source
# docker.sources (its Architectures, dpkg's, asked of the server), for dnf
# the docker-ce-stable repository of
# https://download.docker.com/linux/{centos,rhel}/docker-ce.repo - trusting
# the key at _ci_key only (gpgkey a file, where Docker's file names the URL
# the key was fetched from and checked at). dnf's $basearch and $releasever
# are its own variables, written as they are.
ce_repo_text() {
	if [ "$_ci_family" = deb ]; then
		printf 'Types: deb\nURIs: https://download.docker.com/linux/%s\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: %s\n' \
			"$_ci_repo" "$_ci_suite" "$_ci_arch" "$_ci_key"
	else
		# shellcheck disable=SC2016 # dnf's variables, not the shell's
		printf '[docker-ce-stable]\nname=Docker CE Stable - $basearch\nbaseurl=https://download.docker.com/linux/%s/$releasever/$basearch/stable\nenabled=1\ngpgcheck=1\ngpgkey=file://%s\n' \
			"$_ci_repo" "$_ci_key"
	fi
}

# ce_run <word>...: a change on CE_HOST, as root - through "sudo -n" when
# the user is not root; one that fails fails the run.
ce_run() {
	_cx_status=0
	# shellcheck disable=SC2086 # "sudo -n", or nothing
	run on_host "$_s_ce_host" $_ci_sudo "$@" </dev/null || _cx_status=$?
	[ "$_cx_status" = 0 ] ||
		fail "CE_INSTALL_DOCKER: '$(show_command "$@")' failed on $_s_ce_host (exit $_cx_status; its message is above)"
}

# ce_key_fail <text>: the key's directory removed on the server, and the run
# failed - nothing has trusted the key.
ce_key_fail() {
	run on_host "$_s_ce_host" rm -rf -- "$_kc_dir" </dev/null ||
		warn "could not remove $_kc_dir on $_s_ce_host; remove it there"
	fail "CE_INSTALL_DOCKER: $1; nothing was installed"
}

# ce_key: Docker's signing key, fetched by the server's curl as the user
# into a directory of its own (mktemp -d), read there by the server's gpg
# with that directory as its home - one primary key, of the fingerprint
# Docker publishes - and only then installed at _ci_key (0644), as root; the
# directory is removed. Another key, or none, is never put in place.
ce_key() {
	_kc_want=$(ce_fingerprint "$_ci_family")
	if [ "$DRY_RUN" = 1 ]; then
		_kc_dir=/tmp/tmp.XXXXXXXXXX
		run on_host "$_s_ce_host" mktemp -d
		run on_host "$_s_ce_host" curl -q -fsSL --proto =https --tlsv1.2 -o "$_kc_dir/docker.asc" "https://download.docker.com/linux/$_ci_repo/gpg"
		say "dry run: would check with gpg on $_s_ce_host that $_kc_dir/docker.asc holds one key, of the fingerprint $_kc_want"
		ce_run install -m 0644 "$_kc_dir/docker.asc" "$_ci_key"
		run on_host "$_s_ce_host" rm -rf -- "$_kc_dir"
		return 0
	fi
	say "run: $(show_command on_host "$_s_ce_host" mktemp -d)"
	_kc_dir=$(on_host "$_s_ce_host" mktemp -d </dev/null) ||
		fail "CE_INSTALL_DOCKER: could not make a directory for Docker's key on $_s_ce_host (mktemp -d; its message is above); nothing was installed"
	_kc_dir=$(printf '%s' "$_kc_dir" | printable | tr -d '\n')
	case $_kc_dir in
	*/../* | */.. | */./* | */.) _kc_dir= ;;
	/*/tmp.?*) ;;
	*) _kc_dir= ;;
	esac
	if [ -z "$_kc_dir" ] || ! plain "$_kc_dir"; then
		fail "CE_INSTALL_DOCKER: mktemp -d on $_s_ce_host did not name a new directory of the form /.../tmp.XXXXXXXXXX: nothing is removed there, and nothing was installed"
	fi
	_kc_status=0
	run on_host "$_s_ce_host" curl -q -fsSL --proto =https --tlsv1.2 -o "$_kc_dir/docker.asc" "https://download.docker.com/linux/$_ci_repo/gpg" </dev/null || _kc_status=$?
	[ "$_kc_status" = 0 ] || ce_key_fail "curl could not fetch https://download.docker.com/linux/$_ci_repo/gpg (exit $_kc_status; its message is above)"
	ce_ask gpg --homedir "$_kc_dir" --batch --with-colons --show-keys "$_kc_dir/docker.asc" </dev/null
	_kc_pubs=$(printf '%s\n' "$_ce_out" | grep -c '^pub:') || :
	_kc_fpr=$(printf '%s\n' "$_ce_out" | sed -n 's/^fpr:\{9\}\([0-9A-F][0-9A-F]*\):.*/\1/p' | head -n 1)
	if [ "$_ce_status" != 0 ] || [ "$_kc_pubs" != 1 ] || [ "$_kc_fpr" != "$_kc_want" ]; then
		ce_key_fail "what https://download.docker.com/linux/$_ci_repo/gpg gave is not Docker's key (gpg exited $_ce_status, $_kc_pubs keys, fingerprint '${_kc_fpr:-none}'; Docker's is $_kc_want): it was never put at $_ci_key, and was removed with $_kc_dir on $_s_ce_host"
	fi
	say "Docker CE: the key fetched is Docker's, $_kc_want"
	_kc_status=0
	# shellcheck disable=SC2086 # "sudo -n", or nothing
	run on_host "$_s_ce_host" $_ci_sudo install -m 0644 "$_kc_dir/docker.asc" "$_ci_key" </dev/null || _kc_status=$?
	[ "$_kc_status" = 0 ] || ce_key_fail "could not install the key at $_ci_key (exit $_kc_status; its message is above)"
	run on_host "$_s_ce_host" rm -rf -- "$_kc_dir" </dev/null ||
		warn "could not remove $_kc_dir on $_s_ce_host; remove it there"
}

# ce_kubernetes: _ci_k8s, what says k3s, k0s, MicroK8s or Kubernetes is on
# the server - their directories and programs that are there (installed
# counts, the stricter reading of "runs"), and their processes that run,
# by the names ps -e gives them.
ce_kubernetes() {
	ce_ask ls -d /etc/rancher /var/lib/rancher /usr/local/bin/k3s /etc/kubernetes /var/lib/kubelet /usr/bin/kubelet /usr/local/bin/kubelet \
		/snap/microk8s /var/snap/microk8s /var/lib/k0s /usr/local/bin/k0s /etc/k0s </dev/null
	case $_ce_status in
	0 | 1 | 2) ;;
	*) fail "CE_INSTALL_DOCKER: could not ask $_s_ce_host whether Kubernetes is installed (ls -d exited $_ce_status)" ;;
	esac
	_ci_k8s=$(printf '%s\n' "$_ce_out" | sed '/^$/d' | tr '\n' ' ' | sed 's/ *$//')
	ce_ask ps -e -o comm= </dev/null
	if [ "$_ce_status" != 0 ]; then
		warn "CE_INSTALL_DOCKER: could not list the processes of $_s_ce_host (ps -e exited $_ce_status): Kubernetes is looked for by its files only"
		return 0
	fi
	for _ck in $(printf '%s\n' "$_ce_out" | sed 's/^ *//; s/ *$//' | sort -u); do
		case $_ck in
		kubelet | kubelite | k3s | k3s-* | k0s | k0s*)
			_ci_k8s="$_ci_k8s${_ci_k8s:+ }process $_ck"
			;;
		esac
	done
}

# ce_install_docker: CE_INSTALL_DOCKER=yes. A server that has Docker
# already ("docker --version" says "Docker version") is left as it is.
# Otherwise every question first - the distribution, root or passwordless
# sudo, the group docker, k3s or Kubernetes, podman-docker, the ports - and
# then, refused or forced, Docker CE from Docker's repository. The questions
# that need the engine (the deployment's containers and volumes, other
# containers' ports, the engine's ID) can only come after it is installed:
# docker_install asks them then, and refuses before anything of the
# deployment is made. With --dry-run the commands are printed and the run
# ends there: the engine they would make cannot be asked.
ce_install_docker() {
	_ci_h=$_s_ce_host
	ce_ask docker --version </dev/null
	_ci_shim=
	if [ "$_ce_status" = 0 ]; then
		_ci_ver=$(printf '%s\n' "$_ce_out" | sed '/^$/d' | head -n 1)
		case $_ci_ver in
		'Docker version '*)
			say "Docker CE: $_ci_h has Docker already ($_ci_ver): CE_INSTALL_DOCKER=yes installs nothing"
			return 0
			;;
		esac
		_ci_shim="docker there is not Docker: it says '${_ci_ver:-nothing}' (podman-docker)"
	fi
	ce_ask cat /etc/os-release </dev/null
	[ "$_ce_status" = 0 ] ||
		fail "CE_INSTALL_DOCKER: could not read /etc/os-release on $_ci_h (cat exited $_ce_status)"
	ce_distribution "$_ce_out"

	ce_ask id -u </dev/null
	_ci_uid=$(printf '%s' "$_ce_out" | tr -d '\n')
	[ "$_ce_status" = 0 ] && [ -n "$_ci_uid" ] ||
		fail "CE_INSTALL_DOCKER: could not ask $_ci_h who its user is (id -u exited $_ce_status)"
	_ci_sudo=
	_ci_user=
	if [ "$_ci_uid" != 0 ]; then
		ce_ask id -un </dev/null
		_ci_user=$(printf '%s' "$_ce_out" | tr -d '\n')
		if [ "$_ce_status" != 0 ] || ! plain "$_ci_user"; then
			fail "CE_INSTALL_DOCKER: could not ask $_ci_h the name of its user (id -un exited $_ce_status)"
		fi
		ce_ask sudo -n true </dev/null
		[ "$_ce_status" = 0 ] ||
			refuse "CE_INSTALL_DOCKER installs packages as root, and the user $_ci_user of $_ci_h has no sudo without a password (sudo -n true exited $_ce_status): log in as root, give the user passwordless sudo (which is root on that server), or install Docker CE there yourself; nothing was changed"
		_ci_sudo='sudo -n'
	fi
	if [ "$_ci_family" = deb ]; then
		ce_ask dpkg --print-architecture </dev/null
		_ci_arch=$(printf '%s' "$_ce_out" | tr -d '\n')
		case $_ce_status:$_ci_arch in
		0:[a-z]*) ;;
		*) fail "CE_INSTALL_DOCKER: could not ask $_ci_h its architecture (dpkg --print-architecture exited $_ce_status)" ;;
		esac
		case $_ci_arch in
		*[!a-z0-9]*) fail "CE_INSTALL_DOCKER: dpkg on $_ci_h names the architecture '$_ci_arch'" ;;
		esac
	fi

	ce_kubernetes
	_ci_podman=
	if [ "$_ci_family" = deb ]; then
		ce_ask dpkg-query -W podman-docker </dev/null
		# "podman-docker<TAB><version>": the tab is not printable, and a
		# version follows only when the package is there.
		if [ "$_ce_status" = 0 ] && [ "$(printf '%s' "$_ce_out" | tr -d '\n')" != podman-docker ]; then
			_ci_podman="the package podman-docker is installed"
		fi
	else
		ce_ask rpm -q podman-docker </dev/null
		[ "$_ce_status" != 0 ] || _ci_podman="the package podman-docker is installed"
	fi
	# The engine is reached as the user of CE_HOST (DOCKER_HOST=ssh://),
	# which needs the group docker - root on that server: given only when
	# CE_DOCKER_GROUP=yes says so.
	if [ -n "$_ci_user" ] && [ "$(setting CE_DOCKER_GROUP)" != yes ]; then
		refuse "CE_INSTALL_DOCKER: the engine is reached as $_ci_user, who needs the group docker on $_ci_h, which is root there: say CE_DOCKER_GROUP=yes to have the install add it (sudo -n usermod -aG docker $_ci_user), or install Docker CE and run that command yourself, or log in as root; nothing was changed"
	fi

	_ci_force=$(setting CE_INSTALL_DOCKER_FORCE)
	if [ -n "$_ci_k8s" ]; then
		if [ "$_ci_force" = yes ]; then
			warn "CE_INSTALL_DOCKER_FORCE=yes: installing Docker CE on $_ci_h, where k3s, k0s, MicroK8s or Kubernetes is ($_ci_k8s): Docker sets the iptables FORWARD policy to DROP, which breaks pod networking there"
		else
			refuse "CE_INSTALL_DOCKER: k3s, k0s, MicroK8s or Kubernetes is on $_ci_h ($_ci_k8s), and Docker sets the iptables FORWARD policy to DROP, which breaks pod networking; deploy there with --docker k3s or k8s, or say CE_INSTALL_DOCKER_FORCE=yes; nothing was changed"
		fi
	fi
	for _ci_why in "$_ci_shim" "$_ci_podman"; do
		[ -n "$_ci_why" ] || continue
		if [ "$_ci_force" = yes ]; then
			warn "CE_INSTALL_DOCKER_FORCE=yes: installing Docker CE on $_ci_h, though $_ci_why; its package manager replaces it"
		else
			refuse "CE_INSTALL_DOCKER: on $_ci_h $_ci_why, which Docker CE conflicts with; remove it, deploy with --docker podman on that machine, or say CE_INSTALL_DOCKER_FORCE=yes; nothing was changed"
		fi
	done
	# The ports, while nothing of the deployment can hold them; Debian's
	# install brings curl when the server has none yet.
	if [ "$_ci_family" = deb ]; then ce_ports_free later; else ce_ports_free; fi

	# Nothing has been changed so far.
	if [ -n "$_ci_sudo" ]; then _ci_as="as $_ci_user through sudo"; else _ci_as="as root"; fi
	say "Docker CE: installing it on $_ci_h ($_ci_os), $_ci_as, from https://download.docker.com/linux/$_ci_repo"
	if [ "$_ci_family" = deb ]; then
		ce_run env DEBIAN_FRONTEND=noninteractive apt-get update
		ce_run env DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl gnupg
		ce_run install -m 0755 -d /etc/apt/keyrings
	fi
	ce_key
	[ "$_ci_family" = deb ] || ce_run rpm --import "$_ci_key"
	say "Docker CE: the repository, $_ci_list on $_ci_h:"
	ce_repo_text | sed 's/^/deploy:   /'
	_cx_status=0
	# shellcheck disable=SC2086 # "sudo -n", or nothing
	ce_repo_text | run on_host "$_ci_h" $_ci_sudo dd "of=$_ci_list" status=none || _cx_status=$?
	[ "$_cx_status" = 0 ] ||
		fail "CE_INSTALL_DOCKER: could not write $_ci_list on $_ci_h (exit $_cx_status; its message is above)"
	if [ "$_ci_family" = deb ]; then
		ce_run env DEBIAN_FRONTEND=noninteractive apt-get update
		ce_run env DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
	else
		ce_run dnf -y install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
	fi
	ce_run systemctl enable --now docker
	if [ -n "$_ci_user" ]; then
		ce_run usermod -aG docker "$_ci_user"
		say "Docker CE: added $_ci_user to the group docker on $_ci_h (CE_DOCKER_GROUP=yes), so that DOCKER_HOST=ssh:// reaches the engine as that user; the group is root on that server"
	fi
	if [ "$DRY_RUN" = 1 ]; then
		say "dry run: Docker CE is not on $_ci_h yet, so what the install would do with its engine is not asked"
		exit 0
	fi
	say "Docker CE: installed on $_ci_h"
}

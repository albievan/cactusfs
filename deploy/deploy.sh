#!/bin/sh
# Deploys CactusFS to a container platform, upgrades it, says how it stands
# and removes it - one script for Docker Desktop, Docker CE (here or on a
# server over SSH), Podman, Rancher Desktop, k3s and Kubernetes (PLAN
# milestone 10, ADR-0033).
#
#   deploy/deploy.sh --docker dd --install
#   deploy/deploy.sh --status
#   deploy/deploy.sh --env site-a.env --install --dry-run
#
# Its settings are deploy/.env (or --env FILE), a copy of
# deploy/.env.example: data, never code - see deploy/lib/env.sh. Run
# deploy/deploy.sh --help for the flags.
set -euf
LC_ALL=C
export LC_ALL

# The external pwd, not the shell's: on a case-insensitive filesystem
# (macOS) bash's builtin "pwd -P" keeps the case the path was typed in, and
# the utility returns the case that is stored - the one physical compares.
DEPLOY_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && env pwd -P)
REPO_ROOT=$(dirname -- "$DEPLOY_DIR")
# shellcheck source=deploy/lib/common.sh
. "$DEPLOY_DIR/lib/common.sh"
# shellcheck source=deploy/lib/env.sh
. "$DEPLOY_DIR/lib/env.sh"
# shellcheck source=deploy/lib/secrets.sh
. "$DEPLOY_DIR/lib/secrets.sh"
# shellcheck source=deploy/lib/k8s.sh
. "$DEPLOY_DIR/lib/k8s.sh"
# shellcheck source=deploy/lib/docker.sh
. "$DEPLOY_DIR/lib/docker.sh"
# shellcheck source=deploy/lib/image.sh
. "$DEPLOY_DIR/lib/image.sh"
# shellcheck source=deploy/lib/ce.sh
. "$DEPLOY_DIR/lib/ce.sh"
# shellcheck source=deploy/lib/compose.sh
. "$DEPLOY_DIR/lib/compose.sh"

usage() {
	cat <<'USAGE'
Usage: deploy/deploy.sh [--docker TARGET] [--env FILE] ACTION [--dry-run]

Actions (exactly one):
  --install       install the deployment; on one that is installed, upgrade
                  it, one node at a time, keeping its data
  --status        say how the deployment stands and where it is reached
  --uninstall     remove the deployment and keep its data
  --uninstall --purge
                  remove the deployment and its data; asks for its name at
                  the terminal, or takes it from --purge-confirm
  --print-namespace
                  print the manifests of the deployment's namespace, its
                  marker included, and of the Role (in it) and ClusterRole
                  (list nodes, services and storageclasses; get namespaces
                  <namespace> and kube-system) a K8S_CONTEXT bound to that
                  namespace needs, for a cluster's administrator to create
                  beforehand and bind to the operator (k3s, k8s)

Flags:
  --docker TARGET dd (Docker Desktop), ce (Docker CE, here or CE_HOST),
                  podman, rancher (Rancher Desktop), k3s or k8s; without it,
                  DEPLOY_TARGET, else what runs under DEPLOY_NAME
  --env FILE      the settings; default deploy/.env (deploy/.env.example
                  explains every one)
  --purge         with --uninstall: delete the data too
  --purge-confirm=NAME
                  with --purge: the deployment's name, confirming it without
                  a terminal (never a setting of the env file)
  --another-target
                  deploy a name that is already recorded on another target
                  (in STATE_DIR) onto this one as well; without it, that is
                  refused as settings pointing at the wrong engine or cluster
  --adopt         with --install: take the secrets held by the deployment's
                  labelled volumes on the target into STATE_DIR when
                  STATE_DIR holds no record of the deployment (it was made
                  from another machine, or the record was lost); without
                  it, that is refused (never a setting of the env file).
                  With --uninstall --purge: record the engine reached as
                  the deployment's, so that its labelled volumes may be
                  purged though STATE_DIR holds no record of them
  --engine-replaced
                  with --install: the engine recorded for the deployment is
                  gone (a factory reset of Docker Desktop, say), and the one
                  the settings now reach replaces it: the deployment's
                  secrets are put there and the record names it; without
                  it, another engine is refused (never a setting)
  --from-source   with --install: build the image from this tree with the
                  Dockerfile (IMAGE_SOURCE=source)
  --dry-run       print the commands that would change something, run none
                  of them; what runs is still asked (read-only)
  --help          this text

A flag wins over the env file, the env file over the defaults; the
environment of the shell is not read for settings.

Exit status: 0 done, 1 a command failed, 2 refused (nothing changed),
3 not built yet (what was done before is said above it). --status exits 0
when the deployment is ready or is not installed, 1 when it is installed
and not ready.
USAGE
}

ACTION=
PURGE=0
PURGE_CONFIRM=
ANOTHER_TARGET=0
ADOPT=0
ENGINE_REPLACED=0
DRY_RUN=0
# The image of the run (PLAN 10.4): what image_make and image_native set,
# never what the environment holds.
RESOLVED_IMAGE=
RESOLVED_ARCH=
RESOLVED_NATIVE_IMAGE=
env_file=

# set_action <action>: one action a run.
set_action() {
	[ -z "$ACTION" ] || refuse "$ACTION and --$1 together: one action a run (deploy/deploy.sh --help)"
	ACTION=--$1
}

# flag_given <flag> <what> <value>: a flag's value is there - not empty -
# and is one line.
flag_given() {
	[ -n "$3" ] || refuse "$1 needs $2"
	case $3 in
	*"$NL"* | *"$CR"*) refuse "$1 needs $2 on one line: its value has a line break in it" ;;
	esac
}

# flag_value <flag> <what> <argument count> <next argument>: the value of a
# flag given as two arguments; a missing or empty one, or another flag in
# its place, is refused.
flag_value() {
	if [ "$3" -lt 2 ]; then
		refuse "$1 needs $2"
	fi
	flag_given "$1" "$2" "$4"
	case $4 in
	-*) refuse "$1 needs $2, not '$4'" ;;
	esac
}

while [ $# -gt 0 ]; do
	case $1 in
	--help | -h)
		usage
		exit 0
		;;
	--docker)
		flag_value --docker "a target: $TARGETS" $# "${2:-}"
		env_flag DEPLOY_TARGET "$2"
		shift
		;;
	--docker=*)
		flag_given --docker "a target: $TARGETS" "${1#--docker=}"
		env_flag DEPLOY_TARGET "${1#--docker=}"
		;;
	--env)
		flag_value --env "a file" $# "${2:-}"
		env_file=$2
		shift
		;;
	--env=*)
		flag_given --env "a file" "${1#--env=}"
		env_file=${1#--env=}
		;;
	--install) set_action install ;;
	--status) set_action status ;;
	--uninstall) set_action uninstall ;;
	--print-namespace) set_action print-namespace ;;
	--another-target) ANOTHER_TARGET=1 ;;
	--adopt) ADOPT=1 ;;
	--engine-replaced) ENGINE_REPLACED=1 ;;
	--purge) PURGE=1 ;;
	--purge-confirm)
		flag_value --purge-confirm "the deployment's name" $# "${2:-}"
		PURGE_CONFIRM=$2
		shift
		;;
	--purge-confirm=*)
		flag_given --purge-confirm "the deployment's name" "${1#--purge-confirm=}"
		PURGE_CONFIRM=${1#--purge-confirm=}
		;;
	--from-source) env_flag IMAGE_SOURCE source ;;
	--dry-run) DRY_RUN=1 ;;
	*) refuse "unknown argument '$1' (deploy/deploy.sh --help)" ;;
	esac
	shift
done

[ -n "$ACTION" ] || {
	usage >&2
	refuse "say what to do: --install, --status, --uninstall or --print-namespace"
}
[ "$PURGE" = 0 ] || [ "$ACTION" = --uninstall ] || refuse "--purge goes with --uninstall"
[ -z "$PURGE_CONFIRM" ] || [ "$PURGE" = 1 ] || refuse "--purge-confirm goes with --purge"
[ "$ADOPT" = 0 ] || [ "$ACTION" = --install ] || [ "$PURGE" = 1 ] || refuse "--adopt goes with --install, or with --uninstall --purge"
[ "$ENGINE_REPLACED" = 0 ] || [ "$ACTION" = --install ] || refuse "--engine-replaced goes with --install"
if lookup "$FLAG_SETTINGS" IMAGE_SOURCE >/dev/null && [ "$ACTION" != --install ]; then
	refuse "--from-source goes with --install"
fi

# The env file: --env, else deploy/.env when there is one, else none (the
# defaults; KMS must then still be said, so a run without a file is refused).
if [ -n "$env_file" ]; then
	env_parse "$env_file"
elif [ -f "$DEPLOY_DIR/.env" ]; then
	env_parse "$DEPLOY_DIR/.env"
fi
env_resolve
env_validate
if [ -n "$PURGE_CONFIRM" ] && [ "$PURGE_CONFIRM" != "$(setting DEPLOY_NAME)" ]; then
	refuse "--purge-confirm $PURGE_CONFIRM is not the name of the deployment, $(setting DEPLOY_NAME)"
fi
# Nothing of the environment points a tool elsewhere: every tool runs under
# "env -i" (tool, deploy/lib/common.sh).
resolve_target
target_validate
state_check

# header: what was resolved, before anything is done.
header() {
	[ "$DRY_RUN" = 0 ] || say "dry run: nothing that changes anything is run"
	say "target $TARGET (from $TARGET_FROM), deployment $(setting DEPLOY_NAME), $(setting DEPLOY_MODE)"
	say "engine: $(engine_identity)"
	say "state: $STATE"
	if [ "$DRY_RUN" = 1 ]; then
		env_show
		if [ -n "$(setting K3S_IMPORT)" ]; then
			say "K3S_IMPORT runs: $(k3s_import_words | while IFS= read -r _w; do printf '[%s] ' "$_w"; done)"
		fi
	fi
}
# An action that prints a manifest keeps standard output for it.
case $ACTION in
--print-namespace) header >&2 ;;
*) header ;;
esac

# --- the actions ------------------------------------------------------------------
# A standalone deployment on Docker Desktop, Docker CE on this machine or on
# a server over SSH, or Podman is built (PLAN 10.5, 10.7,
# deploy/lib/docker.sh and deploy/lib/ce.sh), and a cluster on Docker
# Desktop, Docker CE here or on CE_HOST, or Podman (PLAN 10.8,
# deploy/lib/compose.sh). The rest are stubs: each says what the step of
# PLAN milestone 10 that builds it will do, and ends with "not built yet";
# a stub changes nothing but the secrets PLAN 10.3 keeps.

# docker_step: the step that builds the Docker-engine actions of this target.
docker_step() {
	if [ "$TARGET" = rancher ]; then
		echo 10.6
	else
		echo 10.5
	fi
}

# engine: where the containers run.
engine() {
	case $TARGET in
	podman) echo "Podman" ;;
	rancher) echo "Rancher Desktop (engine $(setting RANCHER_ENGINE))" ;;
	ce) if [ -n "$(setting CE_HOST)" ]; then echo "Docker CE on $(setting CE_HOST) over SSH"; else echo "Docker CE"; fi ;;
	*) echo "Docker Desktop" ;;
	esac
}

# kubernetes: where the namespace is, for k3s and k8s (and Rancher Desktop's
# Kubernetes).
kubernetes() {
	_ns=$(setting K8S_NAMESPACE)
	_where="namespace ${_ns:-$(setting DEPLOY_NAME)}"
	if [ -n "$(setting K3S_SSH)" ]; then
		_where="$_where of k3s on $(setting K3S_SSH)"
	elif [ -n "$(setting K8S_CONTEXT)" ]; then
		_where="$_where of context $(setting K8S_CONTEXT)"
	fi
	echo "$_where"
}

do_install() {
	if compose_cluster; then
		compose_install
		return 0
	fi
	if docker_standalone; then
		docker_install
		return 0
	fi
	case $(setting IMAGE_SOURCE) in
	source) _image="built from this tree with the Dockerfile" ;;
	binaries) _image="built from the binaries in $(setting RELEASE_DIR)" ;;
	image) _image="$(setting IMAGE), by digest" ;;
	esac
	# The secrets go to the target before anything starts there (PLAN
	# 10.3): built for a standalone deployment on a Docker engine; the
	# other targets keep theirs as the step secrets_where names builds.
	if [ "$(secrets_where)" = volumes ]; then
		secrets_install
		_secrets="the secrets of $STATE, in the volumes $(setting DEPLOY_NAME)-secrets and $(setting DEPLOY_NAME)-config"
	else
		_secrets="the secrets of $STATE, made once when missing and put on the target before anything starts (PLAN 10.3, built for this target by $(secrets_where))"
	fi
	_helm=$(setting HELM)
	[ -n "$_helm" ] || _helm=$(setting HELM_IMAGE)
	[ -n "$_helm" ] || _helm="the helm image the Dockerfile pins"
	if on_kubernetes; then
		k8s_validate
		not_built 10.10 "install: $_secrets; the image $_image (PLAN 10.4); the chart rendered with $_helm and applied server-side to $(kubernetes), storage $(setting K8S_STORAGE), S3 as $(setting K8S_SERVICE_TYPE) (PLAN 10.9, 10.10)"
	fi
	not_built "$(docker_step)" "install: $_secrets; the image $_image (PLAN 10.4); on $(engine): $(setting DRIVES) labelled volumes, S3 on $(setting BIND):$(setting S3_PORT), admin on $(setting ADMIN_BIND):$(setting ADMIN_PORT), ready within $(setting WAIT_TIMEOUT) s"
}

do_status() {
	if compose_cluster; then
		compose_status
		return 0
	fi
	if docker_standalone; then
		docker_status
		return 0
	fi
	if on_kubernetes; then
		not_built 10.11 "status: the pods, every address and /readyz of $(kubernetes)"
	fi
	not_built "$(docker_step)" "status: the version, the addresses and /readyz of the containers on $(engine)"
}

do_uninstall() {
	if compose_cluster; then
		compose_uninstall
		return 0
	fi
	if docker_standalone; then
		docker_uninstall
		return 0
	fi
	if [ "$PURGE" = 1 ]; then
		if [ -n "$PURGE_CONFIRM" ]; then
			_what="the deployment and its data (confirmed by --purge-confirm)"
		else
			_what="the deployment and its data, once its name is typed at the terminal"
		fi
	else
		_what="the deployment, keeping its data"
	fi
	if on_kubernetes; then
		not_built 10.11 "uninstall: $_what, in $(kubernetes)"
	fi
	not_built "$(docker_step)" "uninstall: $_what, on $(engine)"
}

do_print_namespace() {
	on_kubernetes || refuse "--print-namespace is for a Kubernetes target (k3s, k8s, rancher with RANCHER_KUBERNETES=yes), not $TARGET"
	k8s_print_namespace
}

case $ACTION in
--install) do_install ;;
--status) do_status ;;
--uninstall) do_uninstall ;;
--print-namespace) do_print_namespace ;;
esac

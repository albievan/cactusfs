# shellcheck shell=sh
# Rancher Desktop as a target (PLAN 10.6; ADR-0033 section 9). Sourced by
# deploy/deploy.sh; not run on its own.
#
# Rancher Desktop runs one of two engines, and brings a k3s of its own:
#
#   moby        a Docker engine behind the Docker context rancher-desktop:
#               the deployment is the one of Docker Desktop - standalone
#               (deploy/lib/docker.sh, PLAN 10.5) or a compose cluster
#               (deploy/lib/compose.sh, PLAN 10.8) - through
#               "docker --context rancher-desktop" (ENGINE_CONTEXT, when it
#               is set, names another).
#   containerd  every engine command is nerdctl, always given its
#               namespace - "default" for the deployment's containers and
#               volumes, "k8s.io" for an image meant for Rancher Desktop's
#               k3s, which sees no other namespace. A standalone deployment
#               is docker.sh's with the differences of nerdctl (below); a
#               compose cluster is refused: nerdctl's compose has no
#               "up --no-deps", which the rolling upgrade of compose.sh runs
#               one node at a time, and no "compose ls" (nerdctl v2.4.2,
#               cmd/nerdctl/compose).
#
# RANCHER_KUBERNETES=yes hands the deployment to the Kubernetes path (PLAN
# 10.10, deploy/lib/k8s.sh) with the context rancher-desktop and the image
# made on the engine here (image.sh's route "local": "docker --context
# rancher-desktop", or "nerdctl --namespace k8s.io"); the secrets are then
# the namespace's Secrets (PLAN 10.9), not volumes.
#
# What nerdctl does otherwise than the Docker CLI, and how the script meets
# it (each read in nerdctl v2.4.2's source):
#   - "cp" has no -a (docs/command-reference.md: "Unimplemented docker cp
#     flags: --archive"); what it extracts keeps the owner the tar stream
#     says (it runs tar as root, without --no-same-owner, from the host into
#     the container: pkg/containerutil/cp_linux.go). So the tar streams the
#     script puts into a container are made owned by the nodes' user,
#     65532, with no names (rancher_tar_owner) - the owner "cp -a" gives on
#     Docker. It copies into and out of a container that never ran by
#     mounting its snapshot, and resolves a volume's mount point from the
#     container's spec (cp_resolve_linux.go): the helpers are never started,
#     as on Docker.
#   - "volume inspect" renders Labels as a pointer to a map, nil for a volume
#     without labels (pkg/inspecttypes/native/volume.go): its label is read
#     with "with" first (rancher_volume_label), so an unlabelled volume says
#     nothing and is refused as such, as on Docker.
#   - "ps" has no .State; .Status says "Up", "Created" or "Exited ...", and
#     a container's labels hold none of its image's: --status reads the
#     version label of the image.
#   - "image inspect" says .Id, the digest of the configuration, but finds
#     an image by its name or by the digest of its target - the manifest
#     (pkg/idutil/imagewalker, pkg/cmd/image/inspect.go) - which its
#     RepoDigests name ("<repository>@<target digest>", always one at
#     least), and "build --iidfile" writes the target's digest
#     (containerimage.digest of buildctl's metadata,
#     pkg/cmd/builder/build.go): image.sh takes an image's ID from its
#     RepoDigests on nerdctl, and runs and checks it by that ID. "image tag"
#     takes a name, not an ID (pkg/cmd/image/tag.go), so a tag is made from
#     the name that was checked to name that ID.
#   - "build" has --iidfile and no --metadata-file; --pull is a boolean.
#   - a restart policy is what containerd's restart plugin can do
#     (pkg/cmd/container/run_restart.go): "always" is the one every
#     version has, and a container stopped by "nerdctl stop" stays stopped
#     under it - the node runs with "always".
#   - the engine's ID is containerd's (info's .ID, the UUID of its
#     introspection service).

# _rd_engine: the engine of Rancher Desktop once rancher_resolve has run
# (moby or containerd); _rd_ns: nerdctl's namespace - default, or k8s.io
# for Rancher Desktop's Kubernetes.
_rd_engine=
_rd_ns=default
# _rd_said: what the header says of the engine an ENGINE_CONTEXT reaches.
_rd_said=

# rancher_resolve: for --docker rancher, the engine - RANCHER_ENGINE as
# set, or for "auto" the one that answers: moby when the Docker CLI's
# context (rancher-desktop, or ENGINE_CONTEXT) answers "info" with an ID,
# else containerd when nerdctl does; neither is refused. Asked once,
# before anything else is asked of the engine (read-only; a dry run asks
# too). A compose cluster on containerd is refused here, before anything is
# changed - with RANCHER_ENGINE=containerd before the engine is asked
# anything. An ENGINE_CONTEXT on moby must reach Rancher Desktop
# (rancher_context_check).
rancher_resolve() {
	[ "$TARGET" = rancher ] || return 0
	[ -z "$_rd_engine" ] || return 0
	if on_kubernetes; then
		_rd_ns=k8s.io
	fi
	_rd_engine=$(setting RANCHER_ENGINE)
	case $_rd_engine in
	moby)
		command -v docker >/dev/null 2>&1 ||
			refuse "RANCHER_ENGINE=moby, and there is no docker here: Rancher Desktop's Docker CLI is in ~/.rd/bin; put it on PATH"
		;;
	containerd)
		rancher_cluster_check
		command -v nerdctl >/dev/null 2>&1 ||
			refuse "RANCHER_ENGINE=containerd, and there is no nerdctl here: Rancher Desktop's nerdctl is in ~/.rd/bin; put it on PATH"
		;;
	*)
		_rd_engine=
		if command -v docker >/dev/null 2>&1; then
			_rr_id=$(query docker --context "$(docker_context)" info --format '{{.ID}}' </dev/null 2>/dev/null) || _rr_id=
			[ -z "$(printf '%s' "$_rr_id" | printable | tr -d '\n')" ] || _rd_engine=moby
		fi
		if [ -z "$_rd_engine" ] && command -v nerdctl >/dev/null 2>&1; then
			_rr_id=$(query nerdctl --namespace "$_rd_ns" info --format '{{.ID}}' </dev/null 2>/dev/null) || _rr_id=
			[ -z "$(printf '%s' "$_rr_id" | printable | tr -d '\n')" ] || _rd_engine=containerd
		fi
		[ -n "$_rd_engine" ] ||
			refuse "Rancher Desktop does not answer: neither the Docker context $(docker_context) (its moby engine) nor nerdctl (its containerd engine) said who it is. Start Rancher Desktop, put its tools (~/.rd/bin) on PATH, or say RANCHER_ENGINE=moby or containerd"
		;;
	esac
	rancher_cluster_check
	rancher_context_check
	return 0
}

# rancher_cluster_check: a compose cluster on containerd is refused:
# nerdctl's compose has no "up --no-deps" and no "compose ls".
rancher_cluster_check() {
	if [ "$_rd_engine" = containerd ] && ! on_kubernetes && [ "$(setting DEPLOY_MODE)" = cluster ]; then
		refuse "DEPLOY_MODE=cluster on Rancher Desktop's containerd engine: its nerdctl compose has no 'up --no-deps', with which a cluster is upgraded one node at a time, and no 'compose ls'; switch Rancher Desktop to moby (RANCHER_ENGINE=moby), or use RANCHER_KUBERNETES=yes"
	fi
}

# rancher_context_check: on moby with ENGINE_CONTEXT set, the engine that
# context reaches must be Rancher Desktop's - not Docker Desktop's or a
# server's behind a context of another name - by "docker info": its .Name,
# the host name of the machine the engine runs on, is lima-0 on macOS and
# Linux (Rancher Desktop's Lima instance is "0", lima.InstanceName in its
# rdctl, src/go/rdctl/pkg/lima/name.go, and Lima names an instance's host
# lima-<instance>; lima-rancher-desktop, the name of older releases, is
# taken too), or its .OperatingSystem names Rancher Desktop (the WSL
# distribution "rancher-desktop" on Windows). Refused otherwise; the header
# says the context and what its engine said (_rd_said).
rancher_context_check() {
	[ "$_rd_engine" = moby ] && [ -n "$(setting ENGINE_CONTEXT)" ] || return 0
	_rc=$(query docker --context "$(docker_context)" info --format '{{.Name}}|{{.OperatingSystem}}' </dev/null) ||
		refuse "the Docker context $(docker_context) (ENGINE_CONTEXT) does not answer 'docker info' (its message is above): Rancher Desktop's moby is asked who it is through it"
	_rc=$(printf '%s\n' "$_rc" | printable | head -n 1)
	_rc_name=${_rc%%|*}
	_rc_os=
	case $_rc in
	*'|'*) _rc_os=${_rc#*|} ;;
	esac
	case $_rc_name in
	lima-0 | lima-rancher-desktop) ;;
	*)
		case $_rc_os in
		*'Rancher Desktop'*) ;;
		*) refuse "ENGINE_CONTEXT=$(docker_context) reaches the engine '$_rc_name' ($_rc_os), which is not Rancher Desktop's (lima-0, or an operating system that names Rancher Desktop): name Rancher Desktop's context, or leave ENGINE_CONTEXT empty for rancher-desktop" ;;
		esac
		;;
	esac
	_rd_said="Rancher Desktop's moby through the Docker context $(docker_context) (ENGINE_CONTEXT): the engine $_rc_name, $_rc_os"
}

# rancher_engine: the engine rancher_resolve found, or RANCHER_ENGINE as
# set when it has not run (Rancher Desktop's Kubernetes, whose engine is
# asked only when its image is made).
rancher_engine() {
	if [ -n "$_rd_engine" ]; then
		printf '%s' "$_rd_engine"
	else
		setting RANCHER_ENGINE
	fi
}

# on_nerdctl: true when the engine commands are nerdctl's - Rancher Desktop
# on containerd (rancher_resolve has run).
on_nerdctl() {
	[ "$TARGET" = rancher ] && [ "$_rd_engine" = containerd ]
}

# rancher_volume_label <label>: the template that reads a volume's label in
# "volume inspect" - for nerdctl's Labels, a pointer to a map that is nil
# when the volume has none, through "with" first; for the Docker CLI and
# Podman, as it always was.
rancher_volume_label() {
	if on_nerdctl; then
		printf '{{with .Labels}}{{index . "%s"}}{{end}}' "$1"
	else
		printf '{{index .Labels "%s"}}' "$1"
	fi
}

# rancher_tar_owner: the options of tar that make the entries of a stream
# it creates owned by the nodes' user and group, 65532, by number and with
# no names - for a stream put into a container by nerdctl's cp, which
# extracts as root and keeps the owner the stream says (no -a: the owner is
# not the container's user otherwise). GNU tar and bsdtar both take them
# (--owner, --group and --numeric-owner). Nothing for the other engines,
# whose "cp -a" gives the container's user.
rancher_tar_owner() {
	if on_nerdctl; then
		printf '%s\n' --owner=65532 --group=65532 --numeric-owner
	fi
}

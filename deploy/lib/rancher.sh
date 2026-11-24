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
#     flags: --archive"); what it extracts keeps the owner the source says
#     (it runs tar as root, without --no-same-owner, from the host into
#     the container: pkg/containerutil/cp_linux.go). So the tar streams the
#     script puts into a container are made owned by the nodes' user,
#     65532, with no names (rancher_tar_owner) - the owner "cp -a" gives on
#     Docker. And "cp -", a stream on standard input, is not in the nerdctl
#     of every Rancher Desktop (v2.2.2 of Rancher Desktop 1.25.0 refuses
#     it): the stream is extracted in a private directory of Rancher
#     Desktop's VM, which nerdctl then copies in (nerdctl_put, below). It
#     copies into and out of a container that never ran by mounting its
#     snapshot - only its snapshot: on v2.2.2 a volume mounted into a
#     created container is not seen ("destination parent does not exist",
#     "source does not exist"; the real Rancher Desktop, 2026-11-23), though
#     v2.4.2's cp_resolve_linux.go reads the mounts from the spec. So on
#     nerdctl the secrets' volumes are read and filled in place, in the VM,
#     at the directory "volume inspect" names (nerdctl_volume_put,
#     nerdctl_volume_get), with no helper container; dev-init's container,
#     whose /tmp is its snapshot's, is copied into and out of by cp.
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
#     version has - the node runs with "always". Under it containerd's
#     restart monitor starts again a container "nerdctl stop" stopped
#     (v2.2.2, the real Rancher Desktop): a node that is stopped - the old
#     node of an upgrade, or one an uninstall removes - has its policy set
#     to "no" with "update --restart no" before the stop, and given back
#     ("always") whenever it is started again (docker_restart_policy,
#     deploy/lib/docker.sh).
#   - a stopped container keeps the host ports it published reserved until
#     it is removed (v2.2.2; Docker frees them at the stop): the old node of
#     an upgrade cannot be kept beside the new one, so it is removed, and
#     its image run again, by its digest, should the new node not run
#     (docker_upgrade_nerdctl, docker_rollback).
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
		refuse "DEPLOY_MODE=cluster on Rancher Desktop's containerd engine: its nerdctl compose has no 'up --no-deps', with which a cluster is upgraded one node at a time, and no 'compose ls'; switch Rancher Desktop to moby (RANCHER_ENGINE=moby)"
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
# asked only by an install and by a purge that removes image tags).
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
# no names - for a stream put into a container on nerdctl, extracted as
# root in Rancher Desktop's VM and copied in by nerdctl's cp, both keeping
# the owner the stream says (nerdctl_put; no -a: the owner is not the
# container's user otherwise). GNU tar and bsdtar both take them
# (--owner, --group and --numeric-owner). Nothing for the other engines,
# whose "cp -a" gives the container's user.
rancher_tar_owner() {
	if on_nerdctl; then
		printf '%s\n' --owner=65532 --group=65532 --numeric-owner
	fi
}

# How a tar stream reaches a container on containerd. "nerdctl cp -" -
# a tar stream on its standard input - is not in every nerdctl Rancher
# Desktop bundles: v2.2.2, of Rancher Desktop 1.25.0, says "support for
# reading a tar archive from stdin is not implemented yet" although its
# usage names "-" (v2.4.2 implements it), and its "cp <container>:<path> -"
# says "support for writing a tar archive to stdout is not implemented
# yet". So the script uses neither: a stream put is extracted inside
# Rancher Desktop's VM, where nerdctl runs, into a private directory, and
# nerdctl copies that directory in (nerdctl_put); a stream read is a
# directory nerdctl copies out into one, archived there (nerdctl_get). Rancher
# Desktop's nerdctl is a wrapper - "rdctl shell sudo nerdctl ..." - so the
# copy's source is a path of the VM, and nerdctl, as root there, archives
# it and extracts it into the container as root, keeping every owner and
# mode (pkg/containerutil/cp_linux.go: tar -c of the source, tar -x into
# the container without --no-same-owner).

# _rd_rdctl: Rancher Desktop's rdctl once rancher_rdctl has found it.
_rd_rdctl=

# rancher_rdctl: _rd_rdctl - rdctl on PATH, or the one beside nerdctl, as
# Rancher Desktop installs them together (~/.rd/bin, and its application's
# resources). False when there is none: the caller refuses or fails with
# rancher_rdctl_missing.
rancher_rdctl() {
	[ -z "$_rd_rdctl" ] || return 0
	if command -v rdctl >/dev/null 2>&1; then
		_rd_rdctl=rdctl
		return 0
	fi
	_rr_nerdctl=$(command -v nerdctl 2>/dev/null) || _rr_nerdctl=
	case $_rr_nerdctl in
	/*/nerdctl)
		if [ -x "${_rr_nerdctl%/*}/rdctl" ] && [ -f "${_rr_nerdctl%/*}/rdctl" ]; then
			_rd_rdctl=${_rr_nerdctl%/*}/rdctl
			return 0
		fi
		;;
	esac
	return 1
}

# rancher_rdctl_missing: what is said when there is no rdctl.
rancher_rdctl_missing() {
	printf '%s' "Rancher Desktop's containerd engine, and there is no rdctl on PATH or beside nerdctl: the secrets reach a container through a private directory of Rancher Desktop's VM, which rdctl makes (nerdctl's 'cp -' is not in every release Rancher Desktop bundles); put Rancher Desktop's tools (~/.rd/bin) on PATH"
}

# RANCHER_VM_DIR: the template of the private directory in Rancher
# Desktop's VM - under /dev/shm, a tmpfs: a secret never reaches the VM's
# disk. Not /tmp, which on the VM is the disk (ext4 on /dev/vda1, mounted
# at /mnt/data; only /, /run and /dev/shm are tmpfs - the real Rancher
# Desktop 1.25.0). Checked to be a tmpfs before anything is written there
# (rancher_vm_dir).
RANCHER_VM_DIR=/dev/shm/cactusfs-deploy.XXXXXX

# nerdctl_put <tar file> <container>:<directory>: the tar stream - owned by
# the nodes' user, 65532, by number (rancher_tar_owner) - into the
# directory of a container that never ran, on nerdctl:
#   1. a private directory of the VM (mktemp -d, root's, 0700), given to
#      the nodes' user (chown 65532:65532) - the stream's "." entry, when it
#      has one, sets its owner and mode as the stream says;
#   2. the stream extracted there by root, keeping owners and modes
#      (tar -x --same-owner -p), from standard input through rdctl;
#   3. "nerdctl cp <dir>/. <container>:<directory>": what the directory
#      holds, and its own owner and mode, into the container's directory;
#   4. the directory removed (rm -rf) - also by the run's EXIT trap
#      (cleanup_add) when a step fails or the run is stopped.
# The secret is only ever on a standard input: never in an argument, an
# environment or the output. Each step is checked here, as the function
# may be called on the left of "||", where "set -e" does not hold.
nerdctl_put() {
	_np_tar=$1
	_np_to=$2
	rancher_rdctl || fail "$(rancher_rdctl_missing)"
	if [ "$DRY_RUN" = 1 ]; then
		say "run: $(show_command "$_rd_rdctl" shell sudo mktemp -d "$RANCHER_VM_DIR")"
		say "run: $(show_command "$_rd_rdctl" shell sudo chown 65532:65532 "$RANCHER_VM_DIR")"
		say "run: $(show_command "$_rd_rdctl" shell sudo tar -x --same-owner -p -C "$RANCHER_VM_DIR" -f -)"
		say "run: $(show_command nerdctl --namespace "$_rd_ns" cp "$RANCHER_VM_DIR/." "$_np_to")"
		say "run: $(show_command "$_rd_rdctl" shell sudo rm -rf "$RANCHER_VM_DIR")"
		return 0
	fi
	rancher_vm_dir
	_np_dir=$_rvd
	run "$_rd_rdctl" shell sudo chown 65532:65532 "$_np_dir" </dev/null ||
		fail "giving the directory $_np_dir of Rancher Desktop's VM to the nodes' user failed (rdctl's message is above)"
	run "$_rd_rdctl" shell sudo tar -x --same-owner -p -C "$_np_dir" -f - <"$_np_tar" ||
		fail "extracting the stream into $_np_dir in Rancher Desktop's VM failed (its message is above); the directory is removed"
	run nerdctl --namespace "$_rd_ns" cp "$_np_dir/." "$_np_to" </dev/null ||
		fail "copying $_np_dir of Rancher Desktop's VM into $_np_to failed (nerdctl's message is above); the directory is removed"
	run "$_rd_rdctl" shell sudo rm -rf "$_np_dir" </dev/null ||
		fail "removing the directory $_np_dir of Rancher Desktop's VM failed (rdctl's message is above); remove it: rdctl shell sudo rm -rf $_np_dir"
	cleanup_drop rancher_vm_remove "$_rd_rdctl" "$_np_dir"
}

# rancher_vm_remove <rdctl> <directory>: the cleanup of nerdctl_put and
# nerdctl_get - the private directory of the VM removed; a failure is said, and the other
# cleanups still run.
rancher_vm_remove() {
	run "$1" shell sudo rm -rf "$2" </dev/null ||
		warn "the directory $2 of Rancher Desktop's VM, which holds secrets, could not be removed: remove it - rdctl shell sudo rm -rf $2"
}

# nerdctl_get <container>:<directory> <tar file>: the directory of a
# container that never ran, as a tar stream into the file - the layout of
# "docker cp <container>:<directory> -", every entry under the directory's
# own name - on nerdctl, whose "cp <container>:<path> -" is not in every
# release either (v2.2.2, of Rancher Desktop 1.25.0: "support for writing
# a tar archive to stdout is not implemented yet"):
#   1. a private directory of the VM (rancher_vm_dir: mktemp -d, root's,
#      0700, on a tmpfs), whose removal is in the run's EXIT trap from
#      then on;
#   2. "nerdctl cp <container>:<directory> <dir>/": the directory copied
#      into it under its own name (nerdctl extracts as root there);
#   3. "rdctl shell sudo tar -c -C <dir> -f - <name>": the stream, on
#      standard output, into the file - never into an argument or the
#      output;
#   4. the directory removed (rm -rf).
# What it holds is a secret, read into the run's own directory of the
# entry (SECRETS_WORK); the reader checks it holds only files and
# directories, as for every engine. The private directory is a change of
# the VM, made and removed in the call: no dry run reaches here - the
# helper a fetch reads through is made first, and dev-init runs - and one
# that did would be an error of the script.
nerdctl_get() {
	_ng_from=$1
	_ng_tar=$2
	_ng_name=${_ng_from##*/}
	case $_ng_name in
	'' | . | .. | *[!A-Za-z0-9_.-]*) fail "internal: nerdctl_get of $_ng_from" ;;
	esac
	[ "$DRY_RUN" = 0 ] || fail "internal: nerdctl_get in a dry run"
	rancher_rdctl || fail "$(rancher_rdctl_missing)"
	rancher_vm_dir
	_ng_dir=$_rvd
	run nerdctl --namespace "$_rd_ns" cp "$_ng_from" "$_ng_dir/" </dev/null ||
		fail "copying $_ng_from into $_ng_dir of Rancher Desktop's VM failed (nerdctl's message is above); the directory is removed"
	query "$_rd_rdctl" shell sudo tar -c -C "$_ng_dir" -f - "$_ng_name" </dev/null >"$_ng_tar" ||
		fail "reading $_ng_dir/$_ng_name of Rancher Desktop's VM as a tar stream failed (its message is above); the directory is removed"
	run "$_rd_rdctl" shell sudo rm -rf "$_ng_dir" </dev/null ||
		fail "removing the directory $_ng_dir of Rancher Desktop's VM failed (rdctl's message is above); remove it: rdctl shell sudo rm -rf $_ng_dir"
	cleanup_drop rancher_vm_remove "$_rd_rdctl" "$_ng_dir"
}

# rancher_vm_dir: _rvd, a new private directory of Rancher Desktop's VM
# (RANCHER_VM_DIR; root's, 0700). mktemp's answer must be the template with
# its X's made letters and digits - one line, nothing else - or the run
# fails and nothing is removed or written (it is not a directory the
# script knows it made). The directory's removal is then added to the
# run's EXIT trap (rancher_vm_remove; the caller drops it once it has
# removed it), and the file system it is on must be a tmpfs ("stat -f -c
# %T"), else the run fails before anything is written there. Called
# plainly.
rancher_vm_dir() {
	say "run: $(show_command "$_rd_rdctl" shell sudo mktemp -d "$RANCHER_VM_DIR")"
	_rvd=$(tool "$_rd_rdctl" shell sudo mktemp -d "$RANCHER_VM_DIR" </dev/null) ||
		fail "making a private directory in Rancher Desktop's VM failed (rdctl's message is above)"
	_rvd_said=$(printf '%s' "$_rvd" | printable | tr '\n' ' ')
	_rvd_base=${RANCHER_VM_DIR%.*}.
	_rvd_x=${RANCHER_VM_DIR##*.}
	_rvd_suffix=${_rvd#"$_rvd_base"}
	case $_rvd in
	"$_rvd_base"*) ;;
	*) _rvd_suffix= ;;
	esac
	case $_rvd_suffix in
	'' | *[!A-Za-z0-9]*) _rvd_suffix= ;;
	esac
	[ "${#_rvd_suffix}" = "${#_rvd_x}" ] ||
		fail "mktemp in Rancher Desktop's VM answered '$_rvd_said', which is not a directory of the template $RANCHER_VM_DIR: nothing is written or removed there; look for a directory ${_rvd_base}* in the VM (rdctl shell) and remove it"
	cleanup_add rancher_vm_remove "$_rd_rdctl" "$_rvd"
	_rvd_fs=$(query "$_rd_rdctl" shell sudo stat -f -c %T "$_rvd" </dev/null) ||
		fail "could not ask Rancher Desktop's VM what file system $_rvd is on (rdctl's message is above): nothing was written there"
	_rvd_fs=$(printf '%s' "$_rvd_fs" | printable | tr -d '\n')
	[ "$_rvd_fs" = tmpfs ] ||
		fail "$_rvd in Rancher Desktop's VM is on '$_rvd_fs', not a tmpfs: a secret would reach the VM's disk, so nothing was written there"
}

# The secrets' volumes on nerdctl. A created container's cp sees only its
# snapshot (above), so a volume is read and filled where it is in Rancher
# Desktop's VM: the directory "volume inspect" names, as root there, through
# rdctl. Nothing is made in the VM besides what the volume holds.

# nerdctl_volume_dir <volume>: _nvd, the volume's directory in Rancher
# Desktop's VM - "volume inspect"'s .Mountpoint, which must be exactly
# /var/lib/nerdctl/<hex>/volumes/<namespace>/<volume>/_data (nerdctl's data
# root, the digest of containerd's address, the namespace and the volume's
# own name): any other answer is an error of the script or of nerdctl, and
# nothing is read from or written to it. A read (query).
nerdctl_volume_dir() {
	_nvd=$(query nerdctl --namespace "$_rd_ns" volume inspect --format '{{.Mountpoint}}' "$1" </dev/null) ||
		fail "could not ask nerdctl where the volume $1 is (its message is above)"
	_nvd_said=$(printf '%s' "$_nvd" | printable | tr '\n' ' ')
	_nvd_hex=${_nvd#/var/lib/nerdctl/}
	_nvd_hex=${_nvd_hex%/volumes/"$_rd_ns"/"$1"/_data}
	case $_nvd in
	/var/lib/nerdctl/*/volumes/"$_rd_ns"/"$1"/_data) ;;
	*) _nvd_hex= ;;
	esac
	case $_nvd_hex in
	'' | *[!0-9a-f]*)
		fail "internal: nerdctl says the volume $1 is at '$_nvd_said', which is not /var/lib/nerdctl/<digest>/volumes/$_rd_ns/$1/_data: nothing is read from or written to it"
		;;
	esac
}

# nerdctl_volume_put <volume> <tar file>: the stream - owned by 65532 by
# number (rancher_tar_owner) - extracted as root into the volume's
# directory in the VM, keeping its owners and modes (tar -x --same-owner
# -p), after the volume's root is made the nodes' user's, 0700 (chown,
# chmod), as "cp -a" makes it on Docker - also for a volume made earlier
# and never filled. The stream is on standard input only. A change (run).
nerdctl_volume_put() {
	rancher_rdctl || fail "$(rancher_rdctl_missing)"
	nerdctl_volume_dir "$1"
	# The volume's root is the nodes' user's, 0700, whether the stream has
	# its "." entry or not: a volume made by an earlier run and never filled
	# keeps nerdctl's root:root 0755 otherwise.
	run "$_rd_rdctl" shell sudo chown 65532:65532 "$_nvd" </dev/null ||
		fail "giving the volume $1 to the nodes' user in Rancher Desktop's VM failed (its message is above)"
	run "$_rd_rdctl" shell sudo chmod 0700 "$_nvd" </dev/null ||
		fail "making the volume $1 private to the nodes' user in Rancher Desktop's VM failed (its message is above)"
	run "$_rd_rdctl" shell sudo tar -x --same-owner -p -C "$_nvd" -f - <"$2" ||
		fail "extracting the stream into the volume $1 in Rancher Desktop's VM failed (its message is above)"
}

# nerdctl_volume_get <volume> <unit> <tar file>: the volume as a tar stream
# into the file, in the layout of "docker cp <container>:<dir> -" that
# secrets_fetch reads - every entry under the unit's name: the directory
# _data archived from its parent, its name made the unit's
# (--transform, GNU tar's, which Rancher Desktop's VM has). The stream goes
# to the file only. A read (query).
nerdctl_volume_get() {
	rancher_rdctl || fail "$(rancher_rdctl_missing)"
	nerdctl_volume_dir "$1"
	query "$_rd_rdctl" shell sudo tar -c -C "${_nvd%/_data}" -f - "--transform=s,^_data,$2," _data </dev/null >"$3" ||
		fail "reading the volume $1 in Rancher Desktop's VM as a tar stream failed (its message is above)"
	nerdctl_volume_root_check "$3" "$1"
}

# nerdctl_volume_root_check <tar> <volume>: the first entry of a volume's
# stream (nerdctl_volume_get) is the volume's root, a directory, which must
# be owned by 65532:65532 with the mode 0700 - read from its ustar header
# (the type at byte 156, the mode at 100, the owner at 108 and the group at
# 116, octal), as volume_root_check does for Podman. A root otherwise is
# refused when the volume holds anything - the node could not open it, or
# another could read it - with the command that fixes it; an empty volume
# (a run stopped before its put) is left to the put, which sets its root
# (nerdctl_volume_put).
nerdctl_volume_root_check() {
	_nrc_type=$(od -A n -t c -j 156 -N 1 "$1" | tr -d ' \n') || _nrc_type=
	_nrc_mode=$(od -A n -t c -j 100 -N 7 "$1" | tr -cd '0-7')
	_nrc_uid=$(od -A n -t c -j 108 -N 7 "$1" | tr -cd '0-7' | sed 's/^0*//')
	_nrc_gid=$(od -A n -t c -j 116 -N 7 "$1" | tr -cd '0-7' | sed 's/^0*//')
	_nrc_perm=$(printf '%s' "$_nrc_mode" | sed 's/.*\(...\)$/\1/')
	if [ "$_nrc_type" = 5 ] && [ "$_nrc_uid" = 177774 ] && [ "$_nrc_gid" = 177774 ] && [ "$_nrc_perm" = 700 ]; then
		return 0
	fi
	[ "$(tar -t -f "$1" | wc -l | tr -d ' ')" != 1 ] || return 0
	refuse "the root of the volume $2 is not the nodes' user's with the mode 0700 (owner $(printf '%d' "0${_nrc_uid:-0}"):$(printf '%d' "0${_nrc_gid:-0}"), mode ${_nrc_perm:-?}): the node could not open it, or another user could. Fix it in Rancher Desktop's VM - rdctl shell sudo chown 65532:65532 $_nvd; rdctl shell sudo chmod 0700 $_nvd - then run again"
}

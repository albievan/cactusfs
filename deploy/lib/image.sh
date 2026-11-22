# shellcheck shell=sh
# The image a deployment runs: where it comes from, the platform it is made
# for, its tag, what is checked before it is shipped, and how it reaches the
# target (PLAN 10.4; ADR-0033 sections 5, 12 and 16). Sourced by
# deploy/deploy.sh; not run on its own.
#
# The install of a target (PLAN 10.5 to 10.8, 10.10) calls, outside a
# command substitution (they check host keys and set variables):
#
#   image_check    BEFORE THE INSTALL'S FIRST CHANGE (the secrets included):
#                  decides and refuses (exit 2) - the route to the target,
#                  the host keys, whether the target is on another machine,
#                  the target's platform, the folder or tree the image is
#                  made of, a tree with changes for a target on another
#                  machine, OFFLINE (the base image there and of the
#                  target's platform; a given image there) - and changes
#                  nothing: it only reads (info, inspect, git, kubectl get).
#   image_make     makes the image ready where it is made (the engine here,
#                  BUILD_HOST's, or for IMAGE_SOURCE=image the engine that
#                  pulls it) - built, pulled, tagged - and checks it: its ID,
#                  platform, labels, configuration and, on an engine of the
#                  target's architecture, the version line it prints. A given
#                  image that had to be pulled is checked after the pull, and
#                  may be refused then. Nothing is shipped.
#   image_resolve  image_check, then image_make: for a caller with nothing
#                  to change before the image.
#   image_deliver  gets it to the target by the route image_check chose, and
#                  checks the copy there.
#   image_native   (after image_make; PLAN 10.5 wires it for dev-init under
#                  --from-source, in place of secrets.sh's stub) an image of
#                  the architecture of the engine here, on that engine:
#                  RESOLVED_NATIVE_IMAGE. For IMAGE_SOURCE=binaries and
#                  source only: with IMAGE_SOURCE=image dev-init runs IMAGE.
#
# They set RESOLVED_IMAGE, the reference the target runs; image_check sets
# RESOLVED_ARCH, the target's architecture (amd64 or arm64):
#
#   built (binaries, source)  cactusfs:<version>-<build>-<12 hex of the image
#                             ID>: a new content is a new tag, so a pod with
#                             IfNotPresent rolls; after a push to
#                             K8S_REGISTRY, <K8S_REGISTRY>@sha256:<digest>
#   IMAGE_SOURCE=image        IMAGE by its digest - as given, or the engine's
#                             RepoDigests; when it is moved as a tar stream
#                             (load, import), which keeps no registry digest,
#                             cactusfs:<its version label>-<12 hex of its ID>
#
# What the installs that use them must do:
#   - Run image_check before anything changes, so that its refusals (exit 2,
#     "nothing changed") are true.
#   - On a Docker engine, RESOLVED_IMAGE is run with "--pull never": it is on
#     the engine, and a pull would fetch something else of that name.
#   - On the routes import and local, the chart's pods are given
#     imagePullPolicy: Never - the image is on the node and nowhere else.
#   - For the chart's values, a reference repo@sha256:<digest> is split into
#     repository and digest, and repo:tag into repository and tag
#     (k8s_values does it): never both.
#
# The routes (_img_route):
#   same      made on the engine the target runs it on: nothing to move
#   load      saved on the engine it was made on and loaded on the target's
#             (CE_HOST through DOCKER_HOST=ssh://, Podman, a Docker context);
#             the copy there is compared with the one built: platform,
#             labels, layers and configuration
#   import    saved, then imported into k3s's containerd - "k3s ctr -n k8s.io
#             images import -" over K3S_SSH or here, or the words of
#             K3S_IMPORT - with the image on standard input; one node only.
#             Then the node's image ID (crictl inspecti) must be the
#             configuration digest of the tar sent (its manifest.json)
#   local     the engine here is the cluster's (Rancher Desktop's
#             Kubernetes, the contexts docker-desktop and rancher-desktop,
#             whose API server must be on this machine); one node only. On
#             Rancher Desktop's containerd that engine is nerdctl in the
#             namespace k8s.io, the one its k3s sees (PLAN 10.6)
#   registry  pushed to K8S_REGISTRY; the pods pull it by its digest
#   nodes     IMAGE_SOURCE=image by its digest on Kubernetes without a push
#             or an import (or IMAGE already under K8S_REGISTRY): checked on
#             the engine here, then pulled by the nodes
#
# An image is looked up by its full ID from the moment it is built or
# inspected: it is checked, tagged and run by that ID, and a tag is checked
# to name it again right before the image is saved under that tag (a saved
# image carries the name it was saved by). A build's ID is what the engine
# knows the image by: buildx writes the configuration's digest to
# --iidfile, which is the image's ID in Docker's image store, and the
# manifest's to --metadata-file (containerimage.digest), which is its ID in
# Docker's containerd store; the first the engine finds is taken, and the
# build tag must name it. nerdctl (Rancher Desktop on containerd, PLAN 10.6)
# finds an image by its name or by the digest of its target, the manifest,
# which its --iidfile holds and its RepoDigests name: on nerdctl that digest
# is the image's ID here (image_fields), and a tag is made from the name
# that was checked to name it - nerdctl's "image tag" takes no ID
# (image_tag_source). Every engine command runs through run or query
# (common.sh), under "env -i", with the engine named on its command line;
# ssh's host keys are checked first. A work directory (an image's ID file,
# a tar stream, a digest) is a directory of its own, removed however the run
# ends - a Ctrl-C included - by a subshell in the foreground
# (image_in_workdir).

# image_settings: the settings this file reads, read once, and the tools of
# the engines it uses.
image_settings() {
	_img_source=$(setting IMAGE_SOURCE)
	_img_given=$(setting IMAGE)
	_img_rel=$(setting RELEASE_DIR)
	_img_build_host=$(setting BUILD_HOST)
	_img_dirty_ok=$(setting ALLOW_DIRTY)
	_img_offline=$(setting OFFLINE)
	_img_registry=$(setting K8S_REGISTRY)
	_img_import=$(setting K3S_IMPORT)
	_img_name=$(setting DEPLOY_NAME)
	_img_kubectl=$(setting KUBECTL)
	_img_context=$(docker_context)
	_img_kube=$(kube_context)
	_img_ce_host=
	[ "$TARGET" != ce ] || _img_ce_host=$(setting CE_HOST)
	_img_k3s_ssh=
	[ "$TARGET" != k3s ] || _img_k3s_ssh=$(setting K3S_SSH)
	_img_k8s=0
	if on_kubernetes; then
		_img_k8s=1
	fi
	# Rancher Desktop's engine (PLAN 10.6): asked here for its Kubernetes,
	# where deploy.sh does not ask it; nothing when it was asked already.
	rancher_resolve
	# The engine here: Podman for the podman target, nerdctl for Rancher
	# Desktop on containerd (in the namespace of rancher_resolve: default,
	# or k8s.io for its Kubernetes), the Docker CLI with its context
	# otherwise - for a Kubernetes target, Podman when there is no Docker
	# CLI here.
	if [ "$TARGET" = podman ]; then
		_img_ltool=podman
	elif on_nerdctl; then
		_img_ltool=nerdctl
	elif [ "$_img_k8s" = 1 ] && [ "$TARGET" != rancher ] && ! command -v docker >/dev/null 2>&1; then
		command -v podman >/dev/null 2>&1 ||
			refuse "the image is made with docker or podman here (or on BUILD_HOST), and neither is installed"
		_img_ltool=podman
	else
		_img_ltool=docker
	fi
	# The engine the image is made on: BUILD_HOST's Docker engine, or the
	# engine here.
	_img_btool=$_img_ltool
	[ -z "$_img_build_host" ] || _img_btool=docker
	_img_ttool=docker
	[ "$TARGET" != podman ] || _img_ttool=podman
	! on_nerdctl || _img_ttool=nerdctl
	# Where the image is, once made or pulled (image_route sets them), and
	# BuildKit's builder there.
	_img_at=
	_img_at_tool=
	_img_builder=
	_img_hex=
	_img_sig=
	_img_config=
	_img_ref_at=
	_img_given_absent=0
	_img_checked=0
	_img_stop=refuse
	_img_route=
	_img_remote=1
	_img_remote_why=
}

# image_build_engine <how> <word>...: the engine the image is made on -
# BUILD_HOST's Docker engine through DOCKER_HOST=ssh://, or the engine here.
image_build_engine() {
	_ibe_how=$1
	shift
	if [ -n "$_img_build_host" ]; then
		"$_ibe_how" "DOCKER_HOST=ssh://$_img_build_host" docker "$@"
	else
		image_local_engine "$_ibe_how" "$@"
	fi
}

# image_local_engine <how> <word>...: the engine here - Podman, nerdctl in
# its namespace, or the Docker CLI with its context. Docker Desktop's and
# Rancher Desktop's Kubernetes see its images (the route local); dev-init
# runs on it.
image_local_engine() {
	_ile_how=$1
	shift
	if [ "$_img_ltool" = podman ]; then
		"$_ile_how" podman "$@"
	elif [ "$_img_ltool" = nerdctl ]; then
		"$_ile_how" nerdctl --namespace "$_rd_ns" "$@"
	else
		"$_ile_how" docker --context "$_img_context" "$@"
	fi
}

# image_target_engine <how> <word>...: the engine a Docker target runs its
# containers on - Podman, nerdctl in its namespace (Rancher Desktop on
# containerd), the Docker engine of CE_HOST through DOCKER_HOST=ssh://, or
# the Docker CLI with its context.
image_target_engine() {
	_ite_how=$1
	shift
	if [ "$TARGET" = podman ]; then
		"$_ite_how" podman "$@"
	elif [ "$_img_ttool" = nerdctl ]; then
		"$_ite_how" nerdctl --namespace "$_rd_ns" "$@"
	elif [ -n "$_img_ce_host" ]; then
		"$_ite_how" "DOCKER_HOST=ssh://$_img_ce_host" docker "$@"
	else
		"$_ite_how" docker --context "$_img_context" "$@"
	fi
}

# image_tool_of <engine>: the program of one of the three engines above -
# docker, podman or nerdctl.
image_tool_of() {
	case $1 in
	image_target_engine) printf '%s' "$_img_ttool" ;;
	image_local_engine) printf '%s' "$_img_ltool" ;;
	*) printf '%s' "$_img_btool" ;;
	esac
}

# image_tag_source <name>: what "image tag" is given as the image tagged -
# its ID (_img_hex), but on nerdctl, whose "image tag" takes a name and not
# an ID (nerdctl v2.4.2 pkg/cmd/image/tag.go), the name given, which was
# checked to name that ID; the tag made is checked again after.
image_tag_source() {
	if [ "$_img_at_tool" = nerdctl ]; then
		printf '%s' "$1"
	else
		printf '%s' "$_img_hex"
	fi
}

# image_import <how> [word]...: the command that imports an image, read as a
# tar stream on its standard input, into k3s's containerd - K3S_IMPORT's
# words as they are (split at spaces by k3s_import_words, never by a shell),
# "k3s ctr -n k8s.io images import -" over K3S_SSH, or the same here. The
# words after <how> are not used.
image_import() {
	_iim_how=$1
	if [ -n "$_img_import" ]; then
		_iim_words=$(k3s_import_words) || exit
		set --
		while IFS= read -r _iim_word; do
			set -- "$@" "$_iim_word"
		done <<EOF
$_iim_words
EOF
		"$_iim_how" "$@"
	elif [ -n "$_img_k3s_ssh" ]; then
		"$_iim_how" on_host "$_img_k3s_ssh" k3s ctr -n k8s.io images import -
	else
		"$_iim_how" k3s ctr -n k8s.io images import -
	fi
}

# image_repo <reference>: the repository of a reference, without its tag or
# digest.
image_repo() {
	_irp=${1%%@*}
	case ${_irp##*/} in
	*:*) _irp=${_irp%:*} ;;
	esac
	printf '%s' "$_irp"
}

# image_norm <reference>: a reference as Docker writes the images of Docker
# Hub - without docker.io/library/ or docker.io/ - which Podman writes out.
image_norm() {
	case $1 in
	docker.io/library/*) printf '%s' "${1#docker.io/library/}" ;;
	docker.io/*) printf '%s' "${1#docker.io/}" ;;
	*) printf '%s' "$1" ;;
	esac
}

# image_digest_ok <reference>: true for <repository>@sha256:<64 hex>.
image_digest_ok() {
	case $1 in
	?*@sha256:*) ;;
	*) return 1 ;;
	esac
	_ido_hex=${1##*@sha256:}
	case $_ido_hex in
	'' | *[!0-9a-f]*) return 1 ;;
	esac
	[ "${#_ido_hex}" = 64 ]
}

# image_route: how the image reaches the target, from the settings alone,
# and the engine it is made or pulled on (_img_at, _img_at_tool,
# _img_builder). A route that cannot work is refused here, before anything
# is asked of a machine.
image_route() {
	case $_img_given in
	*@*) image_digest_ok "$_img_given" || bad IMAGE "names a digest that is not sha256:<64 hex>" ;;
	esac
	_img_at=image_build_engine
	_img_at_tool=$_img_btool
	if [ "$_img_k8s" = 0 ]; then
		if [ "$_img_source" = image ]; then
			# The target's engine pulls IMAGE itself.
			_img_route=same
			_img_at=image_target_engine
			_img_at_tool=$_img_ttool
			[ -z "$_img_build_host" ] || warn "BUILD_HOST=$_img_build_host is not used: with IMAGE_SOURCE=image nothing is built"
		elif [ -z "$_img_build_host" ] && [ -z "$_img_ce_host" ]; then
			_img_route=same
		elif [ "$_img_build_host" = "$_img_ce_host" ]; then
			_img_route=same
		else
			_img_route=load
		fi
	else
		if [ -n "$_img_registry" ] && [ -n "$_img_import" ]; then
			refuse "K8S_REGISTRY and K3S_IMPORT are both set: say one way for the image to reach the cluster - pushed to the registry, or imported into the containerd of its one node"
		fi
		case $_img_registry in
		'') ;;
		*/*)
			case ${_img_registry##*/} in
			*:* | *@*) bad K8S_REGISTRY "is the repository the image is pushed to, such as registry.example.test/cactusfs: the script gives the tag, and the pods the digest" ;;
			esac
			;;
		*) bad K8S_REGISTRY "names a registry, not a repository in it: write <host>[:<port>]/<path>, such as registry.example.test/cactusfs" ;;
		esac
		if [ "$_img_source" = image ] && [ -n "$_img_registry" ] &&
			[ "$(image_norm "$(image_repo "$_img_given")")" = "$(image_norm "$_img_registry")" ]; then
			# IMAGE is in K8S_REGISTRY already: nothing to push.
			_img_route=nodes
		elif [ -n "$_img_registry" ]; then
			_img_route=registry
		elif [ -n "$_img_import" ] || [ -n "$_img_k3s_ssh" ]; then
			_img_route=import
		elif [ "$TARGET" = rancher ] || [ "$_img_kube" = docker-desktop ] || [ "$_img_kube" = rancher-desktop ]; then
			if [ "$_img_ltool" = nerdctl ]; then
				# Rancher Desktop on containerd: its k3s sees the namespace
				# k8s.io of the containerd nerdctl reaches (rancher_resolve
				# found nerdctl there).
				:
			else
				[ "$_img_ltool" = docker ] && command -v docker >/dev/null 2>&1 ||
					refuse "the cluster of $(engine_identity) sees the images of the Docker engine here, and there is no docker here"
			fi
			_img_route=local
		elif [ "$TARGET" = k3s ] && [ -z "$_img_kube" ]; then
			_img_route=import
		elif [ "$_img_source" = image ]; then
			_img_route=nodes
		else
			refuse "the image cannot reach the cluster of $(engine_identity): set K8S_REGISTRY, a registry its nodes pull from - or, on a k3s of one node, K3S_SSH or K3S_IMPORT, to import it"
		fi
		if [ "$_img_route" = nodes ]; then
			# Checked here before the nodes pull it.
			_img_at=image_local_engine
			_img_at_tool=$_img_ltool
		fi
	fi
	if [ "$_img_at" = image_build_engine ] && [ -n "$_img_build_host" ]; then
		_img_builder=default
	else
		_img_builder=$_img_context
	fi
}

# image_route_said: the route, as the run says it.
image_route_said() {
	case $_img_route in
	same) echo "made on the target's engine, nothing to move" ;;
	load) echo "saved there and loaded on $(image_target_engine show_command)" ;;
	import) echo "saved there and imported into the containerd of the cluster's one node" ;;
	local)
		if [ "$_img_ltool" = nerdctl ]; then
			echo "in the namespace $_rd_ns of the containerd here (nerdctl), whose images the cluster's one node sees"
		else
			echo "on the Docker engine here, whose images the cluster's one node sees"
		fi
		;;
	registry) echo "pushed to $_img_registry, pulled by the nodes by its digest" ;;
	nodes) echo "checked here, then pulled by the nodes by its digest" ;;
	esac
}

# image_docker_local <context>: true when the Docker context's endpoint is
# on this machine - a unix:// socket or a Windows npipe:// - and false for
# any other (ssh://, tcp://): the engine of such a context is another
# machine's. The endpoint is said in _idl_endpoint.
image_docker_local() {
	_idl_endpoint=$(query docker context inspect "$1" --format '{{.Endpoints.docker.Host}}' </dev/null) ||
		fail "could not inspect the Docker context $1 (its message is above)"
	_idl_endpoint=$(printf '%s\n' "$_idl_endpoint" | printable | head -n 1)
	case $_idl_endpoint in
	unix://?* | npipe://?*) return 0 ;;
	esac
	return 1
}

# image_kube_local: true when the API server of the context is on this
# machine, by its URL: a loopback address, localhost, or Docker Desktop's
# name for this machine, kubernetes.docker.internal. The URL is said in
# _ikl_server.
image_kube_local() {
	_ikl_server=$(query "$_img_kubectl" --context "$_img_kube" config view --minify -o 'jsonpath={.clusters[0].cluster.server}' </dev/null) ||
		fail "could not read the API server of the context $_img_kube (kubectl's message is above)"
	_ikl_server=$(printf '%s\n' "$_ikl_server" | printable | head -n 1)
	case $_ikl_server in
	https://*) ;;
	*) return 1 ;;
	esac
	# The host: up to the first "/" after the scheme, then without a port
	# (an IPv6 address is in brackets).
	_ikl_host=${_ikl_server#https://}
	_ikl_host=${_ikl_host%%/*}
	case $_ikl_host in
	\[*\]:* | \[*\]) _ikl_host=${_ikl_host%%]*}] ;;
	*:*) _ikl_host=${_ikl_host%%:*} ;;
	esac
	case $_ikl_host in
	localhost | kubernetes.docker.internal | '[::1]') return 0 ;;
	esac
	image_loopback_v4 "$_ikl_host"
}

# image_loopback_v4 <host>: true for a dotted-quad IPv4 address of
# 127.0.0.0/8 - four numbers of 0 to 255 without leading zeros, the first
# 127 - and for nothing else (127.example.test is a name).
image_loopback_v4() {
	_ilv=$1
	case $_ilv in
	127.*.*.*) ;;
	*) return 1 ;;
	esac
	_ilv_ifs=$IFS
	IFS=.
	# shellcheck disable=SC2086 # the split at dots is the point
	set -- $_ilv
	IFS=$_ilv_ifs
	[ $# = 4 ] || return 1
	for _ilv_part in "$@"; do
		case $_ilv_part in
		'' | *[!0123456789]* | 0?*) return 1 ;;
		esac
		[ "${#_ilv_part}" -le 3 ] && [ "$_ilv_part" -le 255 ] || return 1
	done
	return 0
}

# image_remote_decide: _img_remote is 1, with _img_remote_why saying why,
# when the image goes to - or is built on - another machine than this one:
# BUILD_HOST, CE_HOST, a Docker context whose endpoint is not a local
# socket, a Podman whose service is remote (podman info's
# Host.ServiceIsRemote - true for a Podman machine, a virtual machine, as on
# macOS), K3S_SSH, K3S_IMPORT (its words may reach anywhere), a push, and a
# cluster but a k3s here and the desktops' (whose API server image_route
# found on this machine). Else 0.
image_remote_decide() {
	_img_remote=1
	if [ "$_img_at" = image_build_engine ] && [ -n "$_img_build_host" ]; then
		_img_remote_why="built on $_img_build_host"
		return 0
	fi
	if [ -n "$_img_ce_host" ]; then
		_img_remote_why="Docker CE on $_img_ce_host"
		return 0
	fi
	if [ "$_img_k8s" = 1 ]; then
		case $_img_route in
		registry)
			_img_remote_why="pushed to $_img_registry"
			return 0
			;;
		local)
			_img_remote=0
			return 0
			;;
		esac
		if [ -n "$_img_k3s_ssh" ]; then
			_img_remote_why="k3s on $_img_k3s_ssh"
		elif [ -n "$_img_import" ]; then
			_img_remote_why="imported by K3S_IMPORT"
		elif [ "$TARGET" = k3s ] && [ -z "$_img_kube" ]; then
			_img_remote=0
		else
			_img_remote_why="the cluster of $(engine_identity)"
		fi
		return 0
	fi
	if [ "$_img_ttool" = nerdctl ]; then
		# Rancher Desktop's containerd runs in its virtual machine on this
		# machine, as Docker Desktop's engine does: here (PLAN 10.6).
		_img_remote=0
		return 0
	fi
	if [ "$TARGET" = podman ]; then
		_ird=$(query podman info --format '{{.Host.ServiceIsRemote}}' </dev/null) ||
			fail "could not ask podman where its service is (its message is above)"
		case $(printf '%s\n' "$_ird" | printable | head -n 1) in
		false) _img_remote=0 ;;
		true) _img_remote_why="Podman's service is on another machine (podman info: Host.ServiceIsRemote)" ;;
		*) fail "podman does not say whether its service is remote (Host.ServiceIsRemote)" ;;
		esac
		return 0
	fi
	if image_docker_local "$_img_context"; then
		_img_remote=0
	else
		_img_remote_why="the Docker context $_img_context, whose endpoint is '$_idl_endpoint'"
	fi
}

# image_engine_arch <engine> <tool>: the architecture of an engine (amd64 or
# arm64), from its "info"; another platform is refused. Called as
# "_a=$(image_engine_arch ...) || exit".
image_engine_arch() {
	if [ "$2" = podman ]; then
		_iea_format='{{.Host.OS}}/{{.Host.Arch}}'
	else
		_iea_format='{{.OSType}}/{{.Architecture}}'
	fi
	_iea_out=$("$1" query info --format "$_iea_format" </dev/null) ||
		fail "could not ask $("$1" show_command) for its platform (its message is above)"
	_iea_out=$(printf '%s\n' "$_iea_out" | printable | head -n 1)
	case $_iea_out in
	linux/x86_64 | linux/amd64) echo amd64 ;;
	linux/aarch64 | linux/arm64) echo arm64 ;;
	*) refuse "$("$1" show_command) runs '$_iea_out': the image of CactusFS is made for linux/amd64 and linux/arm64" ;;
	esac
}

# image_nodes_of <arch>: how many of the cluster's Nodes carry the label
# kubernetes.io/arch=<arch> (listed by that selector, "-o name": nothing of
# a column is parsed, and every word is plain enough for ssh).
image_nodes_of() {
	_ino_out=$(k8s_cmd query get nodes -l "kubernetes.io/arch=$1" -o name </dev/null) ||
		fail "listing the cluster's nodes failed"
	printf '%s\n' "$_ino_out" | printable | grep -c '^node/' || :
}

# image_target_arch: RESOLVED_ARCH, the target's architecture - the target
# engine's, or the one every Node of the cluster has (kubernetes.io/arch).
# An import, and the local route, reach one node's images only: on a cluster
# of more than one node they are refused, and K8S_REGISTRY is required.
image_target_arch() {
	if [ "$_img_k8s" = 0 ]; then
		RESOLVED_ARCH=$(image_engine_arch image_target_engine "$_img_ttool") || exit
		return 0
	fi
	_ita_nodes=$(k8s_nodes) || exit
	_ita_count=${_ita_nodes%% *}
	case $_img_route in
	import | local)
		[ "$_ita_count" = 1 ] ||
			refuse "the cluster of $(engine_identity) has $_ita_count nodes, and an image imported or loaded here reaches the containerd of one node only: a pod placed on another would find no image (ErrImageNeverPull). Push it to a registry the nodes pull from: set K8S_REGISTRY"
		;;
	esac
	_ita_amd=$(image_nodes_of amd64) || exit
	_ita_arm=$(image_nodes_of arm64) || exit
	if [ "$_ita_amd" = "$_ita_count" ]; then
		RESOLVED_ARCH=amd64
	elif [ "$_ita_arm" = "$_ita_count" ]; then
		RESOLVED_ARCH=arm64
	else
		refuse "the $_ita_count nodes of the cluster of $(engine_identity) are not all of one architecture (kubernetes.io/arch: $_ita_amd amd64, $_ita_arm arm64): the image is made for one platform, linux/amd64 or linux/arm64"
	fi
}

# image_meant_valid <what> [given]: the version, build and commit read from
# it are what a tag, a label and a command line may hold. The commit is 12
# hex digits (scripts/buildid.sh's), and of an image given 7 to 40, each
# with -dirty or not, or unknown.
image_meant_valid() {
	case $_img_version in
	'' | *[!0-9A-Za-z.-]*) image_stop "$1 gives the version '$_img_version', which is not one: letters, digits, '.' and '-'" ;;
	esac
	[ "${#_img_version}" -le 64 ] || image_stop "$1 gives a version longer than 64 characters"
	case $_img_build in
	'' | *[!0-9]*) image_stop "$1 gives the build '$_img_build', which is not a number" ;;
	esac
	[ "${#_img_build}" -le 12 ] || image_stop "$1 gives a build longer than 12 digits"
	_imv_commit=${_img_commit%-dirty}
	case $_imv_commit in
	'') [ -z "$_img_commit" ] || image_stop "$1 gives the commit '$_img_commit', which is not one" ;;
	*[!0-9a-f]*) image_stop "$1 gives the commit '$_img_commit', which is not hex digits (and -dirty)" ;;
	*)
		if [ "${2:-}" = given ]; then
			[ "${#_imv_commit}" -ge 7 ] && [ "${#_imv_commit}" -le 40 ] ||
				image_stop "$1 gives the commit '$_img_commit', which is not 7 to 40 hex digits (and -dirty)"
		else
			[ "${#_imv_commit}" = 12 ] || image_stop "$1 gives the commit '$_img_commit', which is not 12 hex digits (and -dirty)"
		fi
		;;
	esac
}

# image_binaries_meant: the folder of make image-binaries in RELEASE_DIR -
# the binary of RESOLVED_ARCH, executable, listed in SHA256SUMS with its
# sum, and BUILDINFO saying version=, build=, commit= and packaging=image
# and nothing else. A binary packaged otherwise (make build-all's,
# packaging=binary) is refused: in a container it would offer to replace
# itself (ADR-0029).
image_binaries_meant() {
	_ibm_bin=$_img_rel/linux-$RESOLVED_ARCH/cactusfs
	for _ibm_file in SHA256SUMS BUILDINFO "linux-$RESOLVED_ARCH/cactusfs"; do
		[ -f "$_img_rel/$_ibm_file" ] ||
			refuse "no $_img_rel/$_ibm_file: run make image-binaries, which writes the binaries of the image for linux/amd64 and linux/arm64 with SHA256SUMS and BUILDINFO (the image is for linux/$RESOLVED_ARCH)"
	done
	[ -x "$_ibm_bin" ] || refuse "$_ibm_bin is not executable, and the image keeps its mode: run make image-binaries again"
	image_buildinfo_meant
	_ibm_sum=$(CDPATH='' cd -- "$_img_rel" && sha256_line "linux-$RESOLVED_ARCH/cactusfs") ||
		fail "the checksum of $_ibm_bin could not be made"
	grep -qxF "$_ibm_sum" "$_img_rel/SHA256SUMS" ||
		refuse "$_ibm_bin does not match $_img_rel/SHA256SUMS: run make image-binaries again"
}

# image_buildinfo_meant: BUILDINFO of the binaries says packaging=image
# and nothing but its four lines - _img_version, _img_build and
# _img_commit - of a version and commit that are valid.
image_buildinfo_meant() {
	_ibm_info=$_img_rel/BUILDINFO
	_ibm_packaging=$(sed -n 's/^packaging=//p' "$_ibm_info" | head -n 1 | printable)
	[ "$_ibm_packaging" = image ] ||
		refuse "$_ibm_info says packaging '$_ibm_packaging', not image: these are not the binaries of make image-binaries (a binary of make build-all, packaging binary, would offer to replace itself in a container)"
	_img_version=$(sed -n 's/^version=//p' "$_ibm_info" | head -n 1 | printable)
	_img_build=$(sed -n 's/^build=//p' "$_ibm_info" | head -n 1 | printable)
	_img_commit=$(sed -n 's/^commit=//p' "$_ibm_info" | head -n 1 | printable)
	_ibm_want=$(printf 'version=%s\nbuild=%s\ncommit=%s\npackaging=image' "$_img_version" "$_img_build" "$_img_commit")
	[ "$(cat "$_ibm_info")" = "$_ibm_want" ] ||
		refuse "$_ibm_info holds more or other than the four lines version=, build=, commit= and packaging=image: run make image-binaries again"
	image_meant_valid "$_ibm_info"
}

# image_git <word>...: git in the repository, through query (under "env
# -i": no GIT_DIR of the caller points it elsewhere). Its messages are
# dropped: a tree without git's answer is said by the caller.
image_git() {
	query git -C "$REPO_ROOT" "$@" </dev/null 2>/dev/null
}

# image_source_meant: the version, build and commit of this tree, as
# scripts/buildid.sh says them - the release of internal/buildinfo/VERSION,
# the count of commits, the commit - except that "-dirty" here also counts
# files git does not track and does not ignore: they are in the build
# context. Without git, or outside a repository, the build is 0 and the
# commit unknown, which image_dirty_check refuses for a target on another
# machine and lets through for one here.
image_source_meant() {
	_img_version=
	if [ -r "$REPO_ROOT/internal/buildinfo/VERSION" ]; then
		_img_version=$(tr -d ' \t\r\n' <"$REPO_ROOT/internal/buildinfo/VERSION")
	fi
	_img_build=0
	_img_commit=
	if command -v git >/dev/null 2>&1; then
		_img_build=$(image_git rev-list --count HEAD) || _img_build=0
		_img_build=$(printf '%s' "$_img_build" | printable)
		_img_commit=$(image_git rev-parse --verify --short=12 'HEAD^{commit}') || _img_commit=
		_img_commit=$(printf '%s' "$_img_commit" | printable)
	fi
	case $_img_build in
	'' | *[!0-9]*) _img_build=0 ;;
	esac
	case $_img_commit in
	*[!0-9a-f]*) _img_commit= ;;
	esac
	if [ -n "$_img_commit" ]; then
		_ism_status=$(image_git --no-optional-locks status --porcelain) ||
			fail "git status in $REPO_ROOT failed"
		[ -z "$_ism_status" ] || _img_commit=$_img_commit-dirty
	fi
	image_meant_valid "$REPO_ROOT/internal/buildinfo/VERSION"
}

# image_dirty_check <what> <commit> [given]: a tree with uncommitted
# changes - a commit ending in -dirty - is refused for a target on another
# machine (image_remote_decide) unless ALLOW_DIRTY=yes; so is a commit that
# is not known, but for an image given (IMAGE_SOURCE=image), whose label
# may lack one.
image_dirty_check() {
	[ "$_img_remote" = 1 ] || return 0
	case $2 in
	*-dirty) _idc_why="is of a tree with uncommitted changes (commit $2)" ;;
	'')
		[ "${3:-}" != given ] || return 0
		_idc_why="is of no known commit (no git, or not a repository)"
		;;
	*) return 0 ;;
	esac
	image_dirty_refuse "$1 $_idc_why"
}

# image_stop <text>: what image_meant_valid, image_dirty_refuse and
# image_given_inspect do with an image they do not take: refuse (exit 2) -
# nothing has changed - or, once a given image has been pulled
# (_img_stop=fail, image_given_inspect's mode), fail (exit 1).
image_stop() {
	if [ "$_img_stop" = fail ]; then
		fail "$1 (said after the image was pulled)"
	fi
	refuse "$1"
}

# image_dirty_refuse <what>: refused for a target on another machine, or a
# warning with ALLOW_DIRTY=yes.
image_dirty_refuse() {
	if [ "$_img_dirty_ok" = yes ]; then
		warn "ALLOW_DIRTY=yes: $1, and goes to $_img_remote_why"
		return 0
	fi
	image_stop "$1, and the target is on another machine ($_img_remote_why): commit the changes and run again, or say ALLOW_DIRTY=yes"
}

# image_dockerfile_check: an image made of binaries for a target on another
# machine takes the rest of the image - the base, the user, the labels -
# from this tree's Dockerfile: that Dockerfile must be the one of the
# binaries' commit - HEAD is BUILDINFO's commit, and the Dockerfile has no
# change git sees - or the run is refused (ALLOW_DIRTY=yes: a warning).
image_dockerfile_check() {
	[ "$_img_remote" = 1 ] || return 0
	_idf_head=
	_idf_status=unknown
	if command -v git >/dev/null 2>&1; then
		_idf_head=$(image_git rev-parse --verify --short=12 'HEAD^{commit}') || _idf_head=
		_idf_head=$(printf '%s' "$_idf_head" | printable)
		if [ -n "$_idf_head" ]; then
			_idf_status=$(image_git --no-optional-locks status --porcelain -- Dockerfile) || _idf_status=unknown
		fi
	fi
	if [ -z "$_idf_head" ]; then
		image_dirty_refuse "the Dockerfile of $REPO_ROOT, which the image of the binaries is made with, is of no known commit (no git, or not a repository)"
	elif [ -n "$_idf_status" ]; then
		image_dirty_refuse "the Dockerfile of $REPO_ROOT, which the image of the binaries is made with, has uncommitted changes"
	elif [ "$_idf_head" != "${_img_commit%-dirty}" ]; then
		image_dirty_refuse "the Dockerfile of $REPO_ROOT, which the image of the binaries is made with, is of the commit $_idf_head, and the binaries of ${_img_commit:-no known commit}"
	fi
}

# image_present <engine> <reference>: true when the engine has the image,
# false when it answers that it has no such image; any other answer fails
# the run - an engine that cannot be asked is not taken for "absent".
image_present() {
	_ipr_err=$("$1" query image inspect --format '{{.Id}}' "$2" 2>&1 >/dev/null </dev/null) && return 0
	case $(printf '%s' "$_ipr_err" | tr 'A-Z' 'a-z') in
	*"no such image"* | *"image not known"* | *"failed to find image"*) return 1 ;;
	esac
	fail "asking $("$1" show_command) for $2 failed: $(printf '%s' "$_ipr_err" | printable | head -n 3)"
}

# image_hex <ID>: the 64 hex digits of an image ID (sha256:<hex> or <hex>);
# fails on anything else.
image_hex() {
	_ihx=${1#sha256:}
	case $_ihx in
	'' | *[!0-9a-f]*) fail "'$1' is not an image ID (sha256)" ;;
	esac
	[ "${#_ihx}" = 64 ] || fail "'$1' is not an image ID (sha256)"
	printf '%s' "$_ihx"
}

# image_fields <engine> <reference>: what is checked of an image, read with
# one inspect - _if_hex (its ID, 64 hex), _if_id (the first 12),
# _if_digests (its RepoDigests, separated by spaces), _if_plat
# (os/architecture), _if_rev and _if_ver (its OCI revision and version
# labels), _if_layers (the digests of its layers' content, RootFS.Layers),
# _if_config (its configuration: user, entrypoint, command, environment,
# working directory, labels, ports, volumes - as JSON) and _if_sig (all of
# it but the ID and the digests: what a save and a load keep, whatever ID
# each engine gives the image). An answer of more than one line, or with a
# control character - a label holding one - fails the run: no part of it
# is taken.
# image_config_format: the part of image_fields' format that reads the
# configuration - all of it, as the engine's CLI renders it. Asking field by
# field does not work everywhere: Docker 29's CLI hands the template the
# configuration as a map with no key for a field the image does not set
# (".Config.Volumes" then fails), while older Docker and Podman hand it a
# struct, on which "index" fails. The whole object renders on both.
image_config_format() {
	printf '%s' '{{json .Config}}'
}

image_fields() {
	_if_out=$("$1" query image inspect --format '{{.Id}}|{{range .RepoDigests}}{{.}} {{end}}|{{.Os}}/{{.Architecture}}|{{index .Config.Labels "org.opencontainers.image.revision"}}|{{index .Config.Labels "org.opencontainers.image.version"}}|{{range .RootFS.Layers}}{{.}} {{end}}|'"$(image_config_format)" "$2" </dev/null) ||
		fail "could not inspect the image $2 on $("$1" show_command) (its message is above)"
	case $_if_out in
	*[[:cntrl:]]*) fail "the inspect of the image $2 on $("$1" show_command) is more than one line, or holds a control character: a label of the image holds a line break or a control character" ;;
	esac
	_if_hex=$(image_hex "${_if_out%%|*}") || exit
	_if_id=$(printf '%.12s' "$_if_hex")
	_if_out=${_if_out#*|}
	_if_digests=${_if_out%%|*}
	_if_sig=${_if_out#*|}
	_if_plat=${_if_sig%%|*}
	_if_out=${_if_sig#*|}
	_if_rev=${_if_out%%|*}
	_if_out=${_if_out#*|}
	_if_ver=${_if_out%%|*}
	_if_out=${_if_out#*|}
	_if_layers=${_if_out%%|*}
	_if_config=${_if_out#*|}
	# nerdctl says .Id, the configuration's digest, and finds an image by
	# its target's digest, which each of its RepoDigests names (PLAN 10.6):
	# that digest is the image's ID on nerdctl.
	# _if_cfg keeps the configuration's digest, which a saved tar names.
	_if_cfg=$_if_hex
	if [ "$(image_tool_of "$1")" = nerdctl ]; then
		_if_target=
		for _if_d in $_if_digests; do
			_if_target=${_if_d##*@}
			break
		done
		[ -n "$_if_target" ] || fail "nerdctl names no digest of the image $2 (RepoDigests): its target cannot be told"
		_if_hex=$(image_hex "$_if_target") || exit
		_if_id=$(printf '%.12s' "$_if_hex")
	fi
}

# image_version_check <image ID> <version> <build> <commit>: on the engine
# the image is on, when it is of the target's architecture, the image's
# binary is run once - no network, a read-only root, every capability
# dropped - and must print the version line of that build, packaged as an
# image. On another architecture it would need emulation, and is not run:
# its labels were checked.
image_version_check() {
	if [ "$DRY_RUN" = 1 ]; then
		say "dry run: the version line of the image is read when the run is not a dry run"
		return 0
	fi
	_ivc_native=$(image_engine_arch "$_img_at" "$_img_at_tool") || exit
	if [ "$_ivc_native" != "$RESOLVED_ARCH" ]; then
		say "image: $("$_img_at" show_command) is linux/$_ivc_native and the image linux/$RESOLVED_ARCH: its version line is not read (that would need emulation); its labels were checked"
		return 0
	fi
	_ivc_line=$("$_img_at" query run --rm --pull never --network none --read-only --cap-drop ALL --security-opt no-new-privileges "$1" version </dev/null) ||
		fail "running the version of the image $1 failed (the engine's message is above)"
	_ivc_line=$(printf '%s\n' "$_ivc_line" | printable | head -n 1)
	if [ "$3" = 0 ]; then
		_ivc_head="cactusfs $2 local build ("
	else
		_ivc_head="cactusfs $2 build $3 ("
	fi
	[ -z "$4" ] || _ivc_head="${_ivc_head}commit $4, "
	case $_ivc_line in
	"$_ivc_head"*" linux/$RESOLVED_ARCH, image)") ;;
	*) fail "the image $1 says '$_ivc_line', not the build meant ('$_ivc_head... linux/$RESOLVED_ARCH, image)')" ;;
	esac
	say "image: it says '$_ivc_line'"
}

# image_offline_base <engine> <tool> <arch>: with OFFLINE=yes, an image
# made of binaries needs its base, the Dockerfile's RUNTIME_IMAGE, on the
# engine that builds it, and for the platform it is built for: BuildKit
# resolves a base of another platform from the registry, which OFFLINE
# forbids. Docker is asked for that platform when its CLI can be (image
# inspect --platform: an image store may hold several platforms of one
# image); else the one platform the engine shows must be it. Refused
# otherwise, before anything is made.
image_offline_base() {
	[ "$_img_offline" = yes ] && [ "$_img_source" = binaries ] || return 0
	_iob_base=$(runtime_image) || exit
	image_present "$1" "$_iob_base" ||
		refuse "OFFLINE=yes, and the base image $_iob_base is not on $("$1" show_command): load it there first (docker load or podman load of a copy saved for linux/$3)"
	_iob_by=
	if [ "$2" = docker ]; then
		_iob_help=$("$1" query image inspect --help </dev/null 2>/dev/null) || _iob_help=
		case $_iob_help in
		*--platform*) _iob_by=platform ;;
		esac
	fi
	if [ -n "$_iob_by" ]; then
		_iob_plat=$("$1" query image inspect --platform "linux/$3" --format '{{.Os}}/{{.Architecture}}' "$_iob_base" </dev/null) ||
			refuse "OFFLINE=yes, and the base image $_iob_base on $("$1" show_command) has no linux/$3 variant (its message is above): load a copy saved for linux/$3 there first"
	else
		_iob_plat=$("$1" query image inspect --format '{{.Os}}/{{.Architecture}}' "$_iob_base" </dev/null) ||
			fail "could not inspect the base image $_iob_base on $("$1" show_command) (its message is above)"
	fi
	_iob_plat=$(printf '%s\n' "$_iob_plat" | printable | head -n 1)
	[ "$_iob_plat" = "linux/$3" ] && return 0
	if [ "$2" = docker ] && [ -z "$_iob_by" ]; then
		refuse "OFFLINE=yes, and the base image $_iob_base on $("$1" show_command) is for $_iob_plat, and the image is built for linux/$3: BuildKit would fetch linux/$3 from the registry. This docker cannot be asked for another platform of an image (image inspect --platform); build on an engine of linux/$3, or load a copy of the base saved for linux/$3 there"
	fi
	refuse "OFFLINE=yes, and the base image $_iob_base on $("$1" show_command) is for $_iob_plat, and the image is built for linux/$3: the build would fetch linux/$3 from the registry; load a copy of the base saved for linux/$3 there first"
}

# image_build: the image made of the folder (the Dockerfile's from-binaries
# stage, the folder as the named build context "binaries") or of this tree
# (the Dockerfile's product image), for linux/RESOLVED_ARCH, on _img_at:
#   - Docker through BuildKit's builder named explicitly (--builder: the
#     context's own, of the docker driver, or "default" on BUILD_HOST), so
#     that no other builder chosen in the user's configuration pulls or
#     loads elsewhere; Podman with "build --format docker";
#   - the Go stage cross-compiles and the image stages have no RUN, so
#     nothing is emulated;
#   - the version, build and commit meant as build arguments;
#   - OFFLINE=yes: "--pull=false" or "--pull=never" (image_offline_base has
#     found the base, of the platform, on the engine);
#   - a stale build tag removed first; the image's IDs written to files
#     (--iidfile, and with Docker --metadata-file), and the one the engine
#     knows taken - the build tag must name it - and every later step done
#     by that ID.
# Then checked - its platform, revision and version labels, its version
# line when native - and tagged cactusfs:<version>-<build>-<12 hex of its
# ID>; the build tag is removed.
image_build() {
	_ib_work=cactusfs:deploy-$_img_name-build-$RESOLVED_ARCH
	if image_present "$_img_at" "$_ib_work"; then
		"$_img_at" run image rm "$_ib_work" </dev/null || fail "removing the stale build tag $_ib_work failed"
	fi
	if [ "$_img_at_tool" = podman ]; then
		set -- build --format docker --platform "linux/$RESOLVED_ARCH"
		[ "$_img_offline" != yes ] || set -- "$@" --pull=never
	elif [ "$_img_at_tool" = nerdctl ]; then
		# nerdctl's build: BuildKit of Rancher Desktop, --pull a boolean, no
		# --metadata-file (its --iidfile holds the target's digest).
		set -- build --platform "linux/$RESOLVED_ARCH"
		[ "$_img_offline" != yes ] || set -- "$@" --pull=false
	else
		set -- buildx build --builder "$_img_builder" --load --platform "linux/$RESOLVED_ARCH"
		[ "$_img_offline" != yes ] || set -- "$@" --pull=false
	fi
	if [ "$_img_source" = binaries ]; then
		set -- "$@" --target from-binaries --build-context "binaries=$_img_rel"
	fi
	set -- "$@" --build-arg "VERSION=$_img_version" --build-arg "BUILD=$_img_build" --build-arg "COMMIT=$_img_commit" -t "$_ib_work"
	if [ "$DRY_RUN" = 1 ]; then
		if [ "$_img_at_tool" != docker ]; then
			"$_img_at" run "$@" --iidfile '<a work directory>/iid' "$REPO_ROOT" </dev/null
		else
			"$_img_at" run "$@" --iidfile '<a work directory>/iid' --metadata-file '<a work directory>/metadata.json' "$REPO_ROOT" </dev/null
		fi
		RESOLVED_IMAGE=cactusfs:$_img_version-$_img_build-dry-run
		_img_ref_at=$RESOLVED_IMAGE
		say "dry run: once built, the image is checked and tagged cactusfs:$_img_version-$_img_build-<12 hex of its ID>"
		return 0
	fi
	_ib_ids=$(image_in_workdir image_build_in "$@") || exit
	_img_config=$(image_hex "${_ib_ids%% *}") || exit
	_ib_manifest=
	case $_ib_ids in
	*' '?*) _ib_manifest=$(image_hex "${_ib_ids#* }") || exit ;;
	esac
	# The ID the engine knows the image by: the configuration's digest in
	# Docker's image store and in Podman, the manifest's in Docker's
	# containerd store.
	_img_hex=
	for _ib_id in "$_img_config" "$_ib_manifest"; do
		[ -n "$_ib_id" ] || continue
		if image_present "$_img_at" "$_ib_id"; then
			_img_hex=$_ib_id
			break
		fi
	done
	image_fields "$_img_at" "$_ib_work"
	if [ -z "$_img_hex" ]; then
		case $_if_hex in
		"$_img_config" | "$_ib_manifest") _img_hex=$_if_hex ;;
		*) fail "the engine knows the image built by neither of its IDs (configuration $_img_config, manifest ${_ib_manifest:-not given}), and its build tag $_ib_work names $_if_hex: nothing is tagged or shipped" ;;
		esac
	fi
	[ "$_if_hex" = "$_img_hex" ] || fail "the build tag $_ib_work names $_if_hex, not the image built, $_img_hex"
	[ "$_if_plat" = "linux/$RESOLVED_ARCH" ] ||
		fail "the image built is for $_if_plat, not linux/$RESOLVED_ARCH"
	[ "$_if_rev" = "$_img_commit" ] ||
		fail "the revision label of the image built is '$_if_rev', not the commit meant, '$_img_commit'"
	[ "$_if_ver" = "$_img_version-$_img_build" ] ||
		fail "the version label of the image built is '$_if_ver', not $_img_version-$_img_build"
	_img_sig=$_if_sig
	RESOLVED_IMAGE=cactusfs:$_img_version-$_img_build-$_if_id
	"$_img_at" run image tag "$(image_tag_source "$_ib_work")" "$RESOLVED_IMAGE" </dev/null || fail "tagging the image $RESOLVED_IMAGE failed"
	# A tag made from a name (nerdctl) names the image checked.
	[ "$_img_at_tool" != nerdctl ] || image_tag_check
	"$_img_at" run image rm "$_ib_work" </dev/null || fail "removing the build's tag $_ib_work failed"
	_img_ref_at=$RESOLVED_IMAGE
	if [ "$_img_at_tool" = nerdctl ]; then
		# nerdctl's ID file holds the manifest's digest; the configuration's
		# is its inspect's .Id (image_fields, of the tag just checked), which
		# a saved tar is checked against.
		_img_config=$_if_cfg
		say "image: built $_img_hex (manifest; configuration $_img_config), tagged $RESOLVED_IMAGE"
	else
		say "image: built $_img_hex (configuration $_img_config), tagged $RESOLVED_IMAGE"
	fi
	image_version_check "$_img_hex" "$_img_version" "$_img_build" "$_img_commit"
}

# image_build_in <directory> <word>...: the build, its IDs written into the
# directory and printed as one line - the ID file's, then (Docker) the
# metadata's containerimage.digest when it gives one - the only line on
# standard output (what run says, and the build's own output, go to standard
# error here). buildx writes its metadata as indented JSON, a key a line.
image_build_in() {
	_ibi_dir=$1
	shift
	if [ "$_img_at_tool" != docker ]; then
		"$_img_at" run "$@" --iidfile "$_ibi_dir/iid" "$REPO_ROOT" </dev/null >&2 ||
			fail "building the image failed (the builder's message is above)"
	else
		"$_img_at" run "$@" --iidfile "$_ibi_dir/iid" --metadata-file "$_ibi_dir/metadata.json" "$REPO_ROOT" </dev/null >&2 ||
			fail "building the image failed (the builder's message is above)"
	fi
	[ -s "$_ibi_dir/iid" ] || fail "the build wrote no image ID"
	_ibi_ids=$(printable <"$_ibi_dir/iid" | tr -d '\n')
	if [ -f "$_ibi_dir/metadata.json" ]; then
		_ibi_manifest=$(sed -n 's/^ *"containerimage\.digest": *"\(sha256:[0-9a-f]*\)".*$/\1/p' "$_ibi_dir/metadata.json" | head -n 1)
		[ -z "$_ibi_manifest" ] || _ibi_ids="$_ibi_ids $_ibi_manifest"
	fi
	printf '%s' "$_ibi_ids"
}

# image_given_inspect <refuse|fail>: IMAGE on the engine, inspected once -
# its ID, RepoDigests and configuration kept - and checked: its platform,
# the version label (it is an image of CactusFS's Dockerfile), its labels as
# BUILDINFO's are checked, and its commit for a target on another machine.
# An image not taken is refused before anything changed, and fails the run
# once it has been pulled (image_stop).
image_given_inspect() {
	_img_stop=$1
	image_fields "$_img_at" "$(image_given_ref)"
	if [ "$(image_given_ref)" != "$_img_given" ] && ! image_given_listed "$_if_digests"; then
		image_stop "the image of the digest of $_img_given on $("$_img_at" show_command) does not name the repository $(image_repo "$_img_given") among its RepoDigests"
	fi
	_img_hex=$_if_hex
	_img_sig=$_if_sig
	_ig_digests=$_if_digests
	_ig_id=$_if_id
	[ "$_if_plat" = "linux/$RESOLVED_ARCH" ] ||
		image_stop "the image $_img_given is for $_if_plat, and the target is linux/$RESOLVED_ARCH"
	[ -n "$_if_ver" ] ||
		image_stop "the image $_img_given has no label org.opencontainers.image.version: it is not an image of CactusFS's Dockerfile"
	case $_if_ver in
	*-*) ;;
	*) image_stop "the version label of $_img_given is '$_if_ver', not <version>-<build>" ;;
	esac
	_img_version=${_if_ver%-*}
	_img_build=${_if_ver##*-}
	_img_commit=$_if_rev
	image_meant_valid "the labels of the image $_img_given" given
	[ -n "$_if_rev" ] || warn "the image $_img_given has no revision label: which commit it is of is not known"
	image_dirty_check "the image $_img_given" "$_if_rev" given
	_img_stop=refuse
}

# image_given_ref: what IMAGE is looked up by on the engine it is made on -
# IMAGE, but on nerdctl, for IMAGE=<repository>@sha256:<hex>, the bare
# digest: nerdctl's "image inspect" of <repository>@<digest> takes only an
# image named <repository>:latest of that digest (nerdctl v2.4.2
# pkg/cmd/image/inspect.go: a reference with a digest and no tag is matched
# against the tag "latest"), while one of a bare digest takes any image of
# it. The repository is then checked among its RepoDigests
# (image_given_listed).
image_given_ref() {
	if [ "$_img_at_tool" = nerdctl ] && image_digest_ok "$_img_given"; then
		printf 'sha256:%s' "${_img_given##*@sha256:}"
	else
		printf '%s' "$_img_given"
	fi
}

# image_given_listed "<RepoDigests>": true when IMAGE's repository and
# digest are among them (Docker Hub's prefixes taken off, as image_norm
# does).
image_given_listed() {
	_igl_want=$(image_norm "$(image_repo "$_img_given")")@sha256:${_img_given##*@sha256:}
	for _igl in $1; do
		[ "$(image_norm "$_igl")" != "$_igl_want" ] || return 0
	done
	return 1
}

# image_given_present: true when the engine IMAGE is made on holds it -
# image_present, and on nerdctl by its bare digest with its repository among
# the RepoDigests (image_given_ref).
image_given_present() {
	image_present "$_img_at" "$(image_given_ref)" || return 1
	[ "$(image_given_ref)" != "$_img_given" ] || return 0
	_igp=$("$_img_at" query image inspect --format '{{range .RepoDigests}}{{.}} {{end}}' "$(image_given_ref)" </dev/null) ||
		fail "could not inspect the image $(image_given_ref) on $("$_img_at" show_command) (its message is above)"
	image_given_listed "$(printf '%s\n' "$_igp" | printable)"
}

# image_given_check: IMAGE_SOURCE=image, before anything changes - named by
# its digest when the nodes pull it; there, and then inspected and checked
# (image_given_inspect); or not there, which OFFLINE=yes refuses and
# image_given_make pulls.
image_given_check() {
	if [ "$_img_route" = nodes ] && ! image_digest_ok "$_img_given"; then
		refuse "IMAGE_SOURCE=image on $(engine_identity) without a push or an import: the nodes pull IMAGE themselves, so it is named by its digest (IMAGE=<repository>@sha256:<64 hex>)"
	fi
	if image_given_present; then
		_img_given_absent=0
		image_given_inspect refuse
	else
		[ "$_img_offline" != yes ] ||
			refuse "OFFLINE=yes, and the image $_img_given is not on $("$_img_at" show_command): load it there first (docker load or podman load of a saved copy)"
		_img_given_absent=1
	fi
}

# image_given_make: IMAGE pulled for linux/RESOLVED_ARCH when it was not
# there (and then inspected and checked - a refusal now comes after the
# pull), its version line read when native, and then named by its digest,
# or tagged by its ID when it is moved as a tar stream.
image_given_make() {
	if [ "$_img_given_absent" = 1 ]; then
		"$_img_at" run pull --platform "linux/$RESOLVED_ARCH" "$_img_given" </dev/null || fail "pulling $_img_given failed"
		if [ "$DRY_RUN" = 1 ]; then
			RESOLVED_IMAGE=$_img_given
			_img_ref_at=$_img_given
			say "dry run: once pulled, $_img_given is checked and deployed by its digest"
			return 0
		fi
		image_given_inspect fail
	fi
	image_version_check "$_img_hex" "$_img_version" "$_img_build" "$_img_commit"
	case $_img_route in
	same | nodes)
		if image_digest_ok "$_img_given"; then
			RESOLVED_IMAGE=$_img_given
		else
			_ig_repo=$(image_norm "$(image_repo "$_img_given")")
			RESOLVED_IMAGE=
			# nerdctl names every image by its target's digest, from a
			# registry or not, and runs none by that name: an image given
			# by a tag is tagged by its ID, below.
			[ "$_img_at_tool" != nerdctl ] || _ig_digests=
			for _ig_line in $_ig_digests; do
				case $(image_norm "$_ig_line") in
				"$_ig_repo@sha256:"*) [ -n "$RESOLVED_IMAGE" ] || RESOLVED_IMAGE=$_ig_line ;;
				esac
			done
			if [ -z "$RESOLVED_IMAGE" ] || ! image_digest_ok "$RESOLVED_IMAGE"; then
				# An image of this engine that no registry holds (built or
				# loaded here): on the engine that runs it, its ID pins the
				# content as well as a digest would, so it is tagged by that
				# ID, as a tar stream's copy is. The nodes of a cluster that
				# pull for themselves need the digest (image_given_check).
				[ "$_img_route" = same ] ||
					fail "the image $_img_given has no digest on $("$_img_at" show_command) (it was not pulled from a registry), and the nodes pull it by its digest: name it IMAGE=<repository>@sha256:<64 hex>, or use IMAGE_SOURCE=binaries"
				RESOLVED_IMAGE=cactusfs:$_img_version-$_img_build-$_ig_id
				"$_img_at" run image tag "$(image_tag_source "$_img_given")" "$RESOLVED_IMAGE" </dev/null || fail "tagging the image $RESOLVED_IMAGE failed"
				[ "$_img_at_tool" != nerdctl ] || [ "$DRY_RUN" = 1 ] || image_tag_check
			fi
		fi
		;;
	*)
		# Moved as a tar stream, which keeps no registry digest: the tag
		# names the ID, which pins the content.
		RESOLVED_IMAGE=cactusfs:$_img_version-$_img_build-$_ig_id
		"$_img_at" run image tag "$(image_tag_source "$_img_given")" "$RESOLVED_IMAGE" </dev/null || fail "tagging the image $RESOLVED_IMAGE failed"
		[ "$_img_at_tool" != nerdctl ] || [ "$DRY_RUN" = 1 ] || image_tag_check
		;;
	esac
	_img_ref_at=$RESOLVED_IMAGE
	say "image: $_img_given is $_img_hex; the target runs $RESOLVED_IMAGE"
}

# image_check: see the top of this file. It only reads; everything that
# refuses is asked here.
image_check() {
	RESOLVED_IMAGE=
	RESOLVED_ARCH=
	image_settings
	if [ "$_img_offline" = yes ] && [ "$_img_source" = source ]; then
		refuse "OFFLINE=yes and an image built from source (--from-source, IMAGE_SOURCE=source): the build pulls the Dockerfile's base images, Go modules and npm packages; use IMAGE_SOURCE=binaries or an IMAGE that is here"
	fi
	image_route
	if [ "$_img_route" = nodes ] && [ "$_img_offline" = yes ] &&
		{ [ -z "$_img_registry" ] || [ "$(image_norm "$(image_repo "$_img_given")")" != "$(image_norm "$_img_registry")" ]; }; then
		refuse "OFFLINE=yes, and the nodes would pull $_img_given from its registry: an air-gapped cluster pulls from the operator's own registry - push it there and name it, or set K8S_REGISTRY to push it"
	fi
	# The host keys of every machine reached over ssh, before any is used.
	[ -z "$_img_ce_host" ] || host_key_check "$_img_ce_host"
	if [ "$_img_at" = image_build_engine ] && [ -n "$_img_build_host" ]; then
		host_key_check "$_img_build_host"
	fi
	[ "$_img_k8s" = 0 ] || k8s_connect
	if [ "$_img_route" = local ] && ! image_kube_local; then
		refuse "the context $_img_kube names the API server '$_ikl_server', which is not on this machine: its nodes do not see the images of the Docker engine here; set K8S_REGISTRY"
	fi
	image_remote_decide
	image_target_arch
	case $_img_source in
	binaries)
		image_binaries_meant
		image_dirty_check "the binaries of $_img_rel" "$_img_commit"
		image_dockerfile_check
		say "image: made of the binaries in $_img_rel - version $_img_version, build $_img_build, commit ${_img_commit:-unknown} - for linux/$RESOLVED_ARCH on $("$_img_at" show_command); $(image_route_said)"
		image_offline_base "$_img_at" "$_img_at_tool" "$RESOLVED_ARCH"
		;;
	source)
		image_source_meant
		image_dirty_check "this tree" "$_img_commit"
		say "image: built from $REPO_ROOT with its Dockerfile - version $_img_version, build $_img_build, commit ${_img_commit:-unknown} - for linux/$RESOLVED_ARCH on $("$_img_at" show_command); $(image_route_said)"
		;;
	image)
		say "image: $_img_given for linux/$RESOLVED_ARCH on $("$_img_at" show_command); $(image_route_said)"
		image_given_check
		;;
	esac
	_img_checked=1
}

# image_precheck: on CE_HOST, the refusals of image_check that need no
# engine and no other machine, asked before anything is done to the server
# (ce_begin install, ce_install_docker): OFFLINE=yes with a build from
# source; for binaries, SHA256SUMS and BUILDINFO there, packaging=image and
# BUILDINFO's four lines, and a binary that is not its SHA256SUMS line (the
# architecture of the server's engine is not known yet: each binary listed
# is checked); a tree with uncommitted changes, or of no known commit, for
# that target on another machine (unless ALLOW_DIRTY=yes). image_check asks
# all of it again, with the engine. Only reads.
image_precheck() {
	image_settings
	[ -n "$_img_ce_host" ] || return 0
	if [ "$_img_offline" = yes ] && [ "$_img_source" = source ]; then
		refuse "OFFLINE=yes and an image built from source (--from-source, IMAGE_SOURCE=source): the build pulls the Dockerfile's base images, Go modules and npm packages; use IMAGE_SOURCE=binaries or an IMAGE that is here"
	fi
	_img_remote=1
	_img_remote_why="Docker CE on $_img_ce_host"
	case $_img_source in
	binaries)
		for _ipc_file in SHA256SUMS BUILDINFO; do
			[ -f "$_img_rel/$_ipc_file" ] ||
				refuse "no $_img_rel/$_ipc_file: run make image-binaries, which writes the binaries of the image for linux/amd64 and linux/arm64 with SHA256SUMS and BUILDINFO"
		done
		image_buildinfo_meant
		for _ipc_arch in amd64 arm64; do
			_ipc_rel=linux-$_ipc_arch/cactusfs
			[ -f "$_img_rel/$_ipc_rel" ] || continue
			_ipc_sum=$(CDPATH='' cd -- "$_img_rel" && sha256_line "$_ipc_rel") ||
				fail "the checksum of $_img_rel/$_ipc_rel could not be made"
			grep -qxF "$_ipc_sum" "$_img_rel/SHA256SUMS" ||
				refuse "$_img_rel/$_ipc_rel does not match $_img_rel/SHA256SUMS: run make image-binaries again"
		done
		image_dirty_check "the binaries of $_img_rel" "$_img_commit"
		image_dockerfile_check
		;;
	source)
		image_source_meant
		image_dirty_check "this tree" "$_img_commit"
		;;
	esac
}

# image_make: see the top of this file.
image_make() {
	[ "${_img_checked:-0}" = 1 ] || fail "internal: image_make before image_check"
	case $_img_source in
	binaries | source) image_build ;;
	image) image_given_make ;;
	esac
}

# image_resolve: image_check, then image_make.
image_resolve() {
	image_check
	image_make
}

# image_native: RESOLVED_NATIVE_IMAGE, an image of the architecture of the
# engine here, on that engine - what dev-init runs in under --from-source
# (PLAN 10.5 wires it): the image image_make made when it is there and of
# that architecture, else one made the same way for it (the folder's binary
# of that architecture, or this tree), checked as image_make checks - with
# OFFLINE=yes, its base found here for that platform first. Called after
# image_make, outside a command substitution; what image_deliver needs is
# kept. Not for IMAGE_SOURCE=image: dev-init then runs IMAGE itself
# (deploy/lib/secrets.sh).
image_native() {
	[ -n "${_img_route:-}" ] && [ "${_img_checked:-0}" = 1 ] || fail "internal: image_native before image_make"
	case $_img_source in
	image) fail "internal: image_native is for IMAGE_SOURCE=binaries and source; with IMAGE_SOURCE=image, dev-init runs IMAGE itself (deploy/lib/secrets.sh)" ;;
	esac
	_in_arch=$(image_engine_arch image_local_engine "$_img_ltool") || exit
	_in_here=0
	case $_img_at in
	image_build_engine) [ -n "$_img_build_host" ] || _in_here=1 ;;
	image_target_engine) [ -n "$_img_ce_host" ] || [ "$_img_ttool" != "$_img_ltool" ] || _in_here=1 ;;
	image_local_engine) _in_here=1 ;;
	esac
	if [ "$_in_here" = 1 ] && [ "$_in_arch" = "$RESOLVED_ARCH" ]; then
		RESOLVED_NATIVE_IMAGE=$_img_ref_at
		say "image: dev-init runs in $RESOLVED_NATIVE_IMAGE, the image of this run"
		return 0
	fi
	_in_save="$_img_at|$_img_at_tool|$_img_builder|$RESOLVED_ARCH|$RESOLVED_IMAGE|$_img_ref_at|$_img_hex|$_img_config|$_img_sig"
	_in_route=$_img_route
	_img_at=image_local_engine
	_img_at_tool=$_img_ltool
	_img_builder=$_img_context
	RESOLVED_ARCH=$_in_arch
	_img_route=local-native
	if [ "$_img_source" = binaries ]; then
		image_binaries_meant
		image_offline_base "$_img_at" "$_img_at_tool" "$RESOLVED_ARCH"
	fi
	image_build
	RESOLVED_NATIVE_IMAGE=$RESOLVED_IMAGE
	say "image: dev-init runs in $RESOLVED_NATIVE_IMAGE, made for this machine's engine (linux/$_in_arch)"
	_img_route=$_in_route
	_img_at=${_in_save%%|*}
	_in_save=${_in_save#*|}
	_img_at_tool=${_in_save%%|*}
	_in_save=${_in_save#*|}
	_img_builder=${_in_save%%|*}
	_in_save=${_in_save#*|}
	RESOLVED_ARCH=${_in_save%%|*}
	_in_save=${_in_save#*|}
	RESOLVED_IMAGE=${_in_save%%|*}
	_in_save=${_in_save#*|}
	_img_ref_at=${_in_save%%|*}
	_in_save=${_in_save#*|}
	_img_hex=${_in_save%%|*}
	_in_save=${_in_save#*|}
	_img_config=${_in_save%%|*}
	_img_sig=${_in_save#*|}
}

# image_in_workdir <function> <argument>...: the function, in a subshell
# in the foreground, given a new work directory of mode 0700 (under TMPDIR)
# as its first argument. The subshell removes it when it ends, however it
# ends - its own EXIT trap, with HUP, INT and TERM ending it as 129, 130 and
# 143 - and so does this function after it. In the foreground, a Ctrl-C (an
# INT to the terminal's process group) or a TERM to the process group
# reaches the subshell and the tool it runs at once. A HUP, INT or TERM sent
# to the script's process alone is taken by the shell when the step it
# waits on ends - a shell runs a trap only after its foreground command -
# and the run then ends as above. The subshell's status is the run's, and
# what it prints on standard output is the caller's. "set -e" does not hold
# in it: every step checks its own status.
image_in_workdir() {
	# Under TMPDIR, named: macOS's mktemp -d without a template does not
	# read TMPDIR.
	_img_iw_tmp=${TMPDIR:-/tmp}
	_img_iw_dir=$(umask 077 && mktemp -d "${_img_iw_tmp%/}/cactusfs-deploy.XXXXXX") || fail "no temporary directory in $_img_iw_tmp"
	_img_iw_status=0
	(
		# The path is expanded now, quoted: the function may assign any
		# variable, and the trap removes this directory and no other.
		# shellcheck disable=SC2064
		trap "rm -rf -- $(shown "$_img_iw_dir")" EXIT
		trap 'exit 129' HUP
		trap 'exit 130' INT
		trap 'exit 143' TERM
		_img_iw_fn=$1
		shift
		"$_img_iw_fn" "$_img_iw_dir" "$@"
	) || _img_iw_status=$?
	rm -rf -- "$_img_iw_dir"
	[ "$_img_iw_status" = 0 ] || exit "$_img_iw_status"
}

# image_tag_check: the tag RESOLVED_IMAGE still names the image built, right
# before the image is saved by it (a saved image carries the name it was
# saved by, so the save is by name).
image_tag_check() {
	image_fields "$_img_at" "$RESOLVED_IMAGE"
	[ "$_if_hex" = "$_img_hex" ] ||
		fail "the tag $RESOLVED_IMAGE names the image $_if_hex now, not the one checked, $_img_hex: run again"
}

# image_stream <engine> <reference> <to> <verify|-> <word>...: the image
# saved by the engine, as a tar stream, given to the standard input of the
# command of <to> (an engine's load, image_import): printed as the one pipe
# it is, run through a file of a work directory, so that a save that fails
# is never taken for an image. <verify>, when given, is then called in the
# same subshell with the configuration digest of the tar sent.
image_stream() {
	_ist_from=$1
	_ist_ref=$2
	_ist_to=$3
	_ist_verify=$4
	shift 4
	say "run: $("$_ist_from" show_command save "$_ist_ref") | $("$_ist_to" show_command "$@")"
	[ "$DRY_RUN" = 0 ] || return 0
	image_in_workdir image_stream_in "$_ist_from" "$_ist_ref" "$_ist_to" "$_ist_verify" "$@"
}

image_stream_in() {
	_isi_dir=$1
	_isi_from=$2
	_isi_ref=$3
	_isi_to=$4
	_isi_verify=$5
	shift 5
	"$_isi_from" invoke save "$_isi_ref" </dev/null >"$_isi_dir/image.tar" ||
		fail "saving $_isi_ref failed (the engine's message is above)"
	[ -s "$_isi_dir/image.tar" ] || fail "saving $_isi_ref gave nothing"
	_isi_config=
	if [ "$_isi_verify" != - ]; then
		# Before it is sent, the tar holds the image checked: of its
		# manifest.json's entries, the one whose layers (its configuration's
		# rootfs.diff_ids) are those of the image - built or given - and,
		# when the configuration's digest is known (a build's --iidfile),
		# that one.
		_isi_path=$(image_tar_config_path "$_isi_dir/image.tar" "$_isi_ref") || exit
		_isi_config=${_isi_path#blobs/sha256/}
		_isi_config=$(image_hex "${_isi_config%.json}") || exit
		if [ -n "$_img_config" ] && [ "$_isi_config" != "$_img_config" ]; then
			fail "the tar saved of $_isi_ref holds the configuration $_isi_config, not the one built, $_img_config: nothing was sent"
		fi
	fi
	"$_isi_to" invoke "$@" <"$_isi_dir/image.tar" ||
		fail "$("$_isi_to" show_command "$@") failed (its message is above)"
	[ "$_isi_verify" = - ] || "$_isi_verify" "$_isi_config"
}

# image_tar_config_path <tar> <reference>: the file of the configuration of
# the image checked, of those a saved tar holds. Each entry of its
# manifest.json names one by "Config" - <hex>.json (Docker's image store,
# Podman) or blobs/sha256/<hex> (an OCI layout, Docker's containerd store,
# which may save several platforms of one image); a name of another form,
# or none, fails the run. The entry taken is the one whose layers (its
# configuration's rootfs.diff_ids) are the image's; with none, the run
# fails.
image_tar_config_path() {
	_itc=$(tar -x -O -f "$1" manifest.json 2>/dev/null) || fail "the image $2 was saved without a manifest.json"
	# An entry a line: each begins with "{"; a nested object (LayerSources)
	# comes after "Config".
	_itc=$(printf '%s' "$_itc" | tr -d '\n' | tr '{' '\n')
	_itc_n=0
	_itc_found=
	_itc_want=$(image_sig_part 4)
	while IFS= read -r _itc_entry; do
		case $_itc_entry in
		*'"RepoTags"'* | *'"Layers"'* | *'"Config"'*) ;;
		*) continue ;;
		esac
		_itc_n=$((_itc_n + 1))
		_itc_path=$(printf '%s\n' "$_itc_entry" | sed -n 's/^.*"Config" *: *"\([^"]*\)".*$/\1/p')
		case $_itc_path in
		blobs/sha256/* | *.json) ;;
		*) fail "the manifest.json of the image $2 names the configuration '$_itc_path'" ;;
		esac
		_itc_hex=${_itc_path#blobs/sha256/}
		_itc_hex=${_itc_hex%.json}
		case $_itc_hex in
		'' | *[!0-9a-f]*) fail "the manifest.json of the image $2 names the configuration '$_itc_path'" ;;
		esac
		[ "${#_itc_hex}" = 64 ] || fail "the manifest.json of the image $2 names the configuration '$_itc_path'"
		case $_itc_path in
		"blobs/sha256/$_itc_hex" | "$_itc_hex.json") ;;
		*) fail "the manifest.json of the image $2 names the configuration '$_itc_path'" ;;
		esac
		_itc_layers=$(image_tar_layers "$1" "$_itc_path" "$2") || exit
		if [ -z "$_itc_found" ] && [ "$_itc_layers" = "$_itc_want" ]; then
			_itc_found=$_itc_path
		fi
	done <<EOF
$_itc
EOF
	[ "$_itc_n" -gt 0 ] || fail "the manifest.json of the image $2 names the configuration ''"
	[ -n "$_itc_found" ] ||
		fail "the tar saved of $2 holds other layers than the image checked, in each of its $_itc_n images: nothing was sent"
	printf '%s' "$_itc_found"
}

# image_tar_layers <tar> <configuration file> <reference>: the digests of
# the layers' content of the image a saved tar holds - its configuration's
# rootfs.diff_ids - separated by single spaces.
image_tar_layers() {
	_itl=$(tar -x -O -f "$1" "$2" 2>/dev/null) || fail "the tar of the image $3 holds no configuration $2"
	_itl=$(printf '%s' "$_itl" | tr -d '\n' | sed -n 's/^.*"diff_ids" *: *\[\([^]]*\)\].*$/\1/p' | tr ',"' '  ')
	# shellcheck disable=SC2086 # the split into words is the point
	image_words $_itl
}

# image_words <word>...: the words, separated by single spaces.
image_words() {
	printf '%s' "$*"
}

# image_sig_part <n>: a part of the image checked (_img_sig) - 1 platform, 2
# revision label, 3 version label, 4 layers (separated by single spaces), 5
# configuration.
image_sig_part() {
	_isp=$_img_sig
	_isp_n=$1
	while [ "$_isp_n" -gt 1 ]; do
		_isp=${_isp#*|}
		_isp_n=$((_isp_n - 1))
	done
	if [ "$1" = 5 ]; then
		printf '%s' "$_isp"
		return 0
	fi
	_isp=${_isp%%|*}
	if [ "$1" = 4 ]; then
		# shellcheck disable=SC2086 # the split into words is the point
		image_words $_isp
		return 0
	fi
	printf '%s' "$_isp"
}

# image_import_verify <configuration digest>: after an import, the node's
# containerd holds the image sent - its image ID, as crictl inspecti gives
# it (status.id, the configuration's digest), is the configuration of the
# tar, which image_stream_in has checked against the one built. Over
# K3S_SSH or here; K3S_IMPORT's words import on a node the script cannot
# ask, which is said.
image_import_verify() {
	if [ -n "$_img_import" ]; then
		warn "the image was imported by K3S_IMPORT's own command, on a node the script cannot ask: that the node holds the configuration $1 is not checked"
		return 0
	fi
	if [ -n "$_img_k3s_ssh" ]; then
		_iiv=$(query on_host "$_img_k3s_ssh" k3s crictl inspecti -q -o json "$RESOLVED_IMAGE" </dev/null) ||
			fail "asking the node for $RESOLVED_IMAGE after the import failed (crictl's message is above)"
	else
		_iiv=$(query k3s crictl inspecti -q -o json "$RESOLVED_IMAGE" </dev/null) ||
			fail "asking the node for $RESOLVED_IMAGE after the import failed (crictl's message is above)"
	fi
	_iiv=$(printf '%s\n' "$_iiv" | sed -n 's/^ *"id": *"sha256:\([0-9a-f]*\)".*$/\1/p' | head -n 1)
	[ "$_iiv" = "$1" ] ||
		fail "after the import the node's image $RESOLVED_IMAGE is '${_iiv:-not there}', not the configuration sent, $1"
	say "image: the node holds $RESOLVED_IMAGE, configuration $1"
}

# image_loaded <engine>: the image loaded on the engine is the one built:
# the same platform, labels, layers and configuration (an engine may give
# it another ID: Docker's image store gives the configuration's digest, its
# containerd store the manifest's). A difference is named by its parts,
# never by their values: the environment may hold an operator's values.
image_loaded() {
	image_fields "$1" "$RESOLVED_IMAGE"
	[ "$_if_sig" = "$_img_sig" ] && return 0
	_il_parts=
	[ "$_if_plat" = "$(image_sig_part 1)" ] || _il_parts="$_il_parts, platform"
	[ "$_if_rev" = "$(image_sig_part 2)" ] || _il_parts="$_il_parts, revision label"
	[ "$_if_ver" = "$(image_sig_part 3)" ] || _il_parts="$_il_parts, version label"
	# shellcheck disable=SC2086 # the split into words is the point
	[ "$(image_words $_if_layers)" = "$(image_sig_part 4)" ] || _il_parts="$_il_parts, layers"
	[ "$_if_config" = "$(image_sig_part 5)" ] || _il_parts="$_il_parts, configuration (user, entrypoint, command, environment, working directory, labels, ports or volumes)"
	[ -n "$_il_parts" ] || _il_parts=", its description"
	fail "the image $RESOLVED_IMAGE on $("$1" show_command) is not the one sent: it differs in its ${_il_parts#, }"
}

# image_push: the image built tagged <K8S_REGISTRY>:<its tag> by its ID and
# pushed - the operator's own registry, which OFFLINE=yes allows - and then
# named by the digest the registry gave it (Docker's RepoDigests; podman
# push --digestfile).
image_push() {
	_ipu_to=$_img_registry:${RESOLVED_IMAGE##*:}
	if [ "$DRY_RUN" = 1 ]; then
		"$_img_at" run image tag "${_img_hex:-$RESOLVED_IMAGE}" "$_ipu_to" </dev/null
		"$_img_at" run push "$_ipu_to" </dev/null
		RESOLVED_IMAGE=$_ipu_to
		say "dry run: once pushed, the pods run $_img_registry by the digest the push gives"
		return 0
	fi
	"$_img_at" run image tag "$(image_tag_source "$RESOLVED_IMAGE")" "$_ipu_to" </dev/null || fail "tagging $_ipu_to failed"
	if [ "$_img_at_tool" = podman ]; then
		_ipu_digest=$(image_in_workdir image_podman_push "$_ipu_to") || exit
	else
		"$_img_at" run push "$_ipu_to" </dev/null || fail "pushing $_ipu_to failed (the engine's message is above)"
		image_fields "$_img_at" "$_ipu_to"
		[ "$_if_hex" = "$_img_hex" ] || fail "the tag $_ipu_to names the image $_if_hex, not the one built, $_img_hex"
		_ipu_digest=
		for _ipu_line in $_if_digests; do
			case $(image_norm "$_ipu_line") in
			"$(image_norm "$_img_registry")@sha256:"*) [ -n "$_ipu_digest" ] || _ipu_digest=${_ipu_line##*@} ;;
			esac
		done
	fi
	image_digest_ok "$_img_registry@$_ipu_digest" || fail "the push of $_ipu_to gave no digest"
	RESOLVED_IMAGE=$_img_registry@$_ipu_digest
	say "image: pushed; the pods run $RESOLVED_IMAGE"
}

# image_podman_push <directory> <reference>: podman push, its digest written
# into the directory and printed - the only line on standard output (what
# run says goes to standard error here).
image_podman_push() {
	"$_img_at" run push --digestfile "$1/digest" "$2" </dev/null >&2 ||
		fail "pushing $2 failed (podman's message is above)"
	[ -f "$1/digest" ] || fail "podman push wrote no digest for $2"
	printable <"$1/digest" | tr -d '\n'
}

# image_deliver: see the top of this file.
image_deliver() {
	case ${_img_route:-} in
	same)
		say "image: $RESOLVED_IMAGE is on the target's engine"
		;;
	nodes)
		say "image: the nodes pull $RESOLVED_IMAGE by its digest"
		;;
	load)
		[ "$DRY_RUN" = 1 ] || image_tag_check
		image_stream "$_img_at" "$RESOLVED_IMAGE" image_target_engine - load
		[ "$DRY_RUN" = 1 ] || image_loaded image_target_engine
		say "image: $RESOLVED_IMAGE loaded on $(image_target_engine show_command)"
		;;
	import)
		[ "$DRY_RUN" = 1 ] || image_tag_check
		image_stream "$_img_at" "$RESOLVED_IMAGE" image_import image_import_verify
		say "image: $RESOLVED_IMAGE imported into the containerd of the cluster's one node"
		;;
	local)
		if [ "$_img_at" = image_build_engine ] && [ -n "$_img_build_host" ]; then
			[ "$DRY_RUN" = 1 ] || image_tag_check
			image_stream "$_img_at" "$RESOLVED_IMAGE" image_local_engine - load
			[ "$DRY_RUN" = 1 ] || image_loaded image_local_engine
			say "image: $RESOLVED_IMAGE loaded on the Docker engine here, whose images the cluster's one node sees"
		else
			say "image: $RESOLVED_IMAGE is $(image_route_said)"
		fi
		;;
	registry)
		image_push
		;;
	*) fail "internal: image_deliver before image_check" ;;
	esac
}

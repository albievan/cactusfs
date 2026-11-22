#!/bin/sh
# The lifecycle of a standalone deployment made by deploy/deploy.sh, against
# a real engine on this host (PLAN 10.5): install; an object of 5 MiB put
# and read back over TLS with SigV4 (curl --aws-sigv4), the CA and the root
# credentials taken from STATE_DIR; kms.json in STATE_DIR equal to the one in
# the config volume, and no kms-local.json in the metadata volume; every file
# of the volumes <name>-secrets and <name>-config, and their roots, readable
# by the node's user 65532 as PLAN 10.3's helper filled them with "cp -a";
# a second deployment on a port the first holds refused, with nothing made;
# an upgrade to a new tag that keeps the object; --status with the version,
# the addresses and /readyz 200; an uninstall that keeps the volumes; a
# reinstall that reads the object; a purge refused without its name; and a
# purge that deletes the deployment's volumes and then its entry of
# STATE_DIR, while a sentinel volume and a sentinel entry stay.
#
# With LIFECYCLE_MODE=cluster ("make test-deploy DEPLOY_MODE=cluster", PLAN
# 10.8) the deployment is a compose cluster of four nodes
# (deploy/compose/compose.yaml): install; the round trip on node 1;
# kms.json in STATE_DIR equal to the one the nodes mount, compose's secret
# files 0444 in a 0700 directory; an upgrade to an image of a new ID, one
# node at a time, with the object read from whichever node answers all
# through it;
# --status with every node ready; an uninstall that removes every container
# and keeps the 13 volumes; a reinstall that reads the object; and the two
# purges, with the sentinels.
#
# Run by "make test-deploy" (ENGINE=docker|podman), which builds the image
# for this host first; not part of "make check". Its settings, from the
# environment:
#
#   ENGINE              docker (default) or podman
#   LIFECYCLE_MODE      standalone (default) or cluster
#   LIFECYCLE_SOURCE    image (default): deploy LIFECYCLE_IMAGE; or binaries:
#                       the script builds the image itself from the binaries
#                       of make image-binaries (IMAGE_SOURCE=binaries) - the
#                       helper of the secrets' volumes is then the runtime
#                       image, which has no /var/lib/cactusfs, so step 4
#                       proves the volumes' roots on that path too
#   LIFECYCLE_IMAGE     the image to deploy, on the engine (required with
#                       LIFECYCLE_SOURCE=image)
#   LIFECYCLE_RELEASE_DIR
#                       the binaries (default: dist/image of this tree)
#   LIFECYCLE_ALLOW_DIRTY=yes
#                       ALLOW_DIRTY=yes for the deployment: binaries of a
#                       tree with changes, to an engine on another machine
#                       (a Podman machine)
#   LIFECYCLE_TARGET    the target: ce for docker (dd on Docker Desktop),
#                       podman for podman
#   LIFECYCLE_CONTEXT   the Docker context (default: default)
#   LIFECYCLE_NAME      the deployment's name (default: cfs-lifecycle)
#   LIFECYCLE_S3_PORT, LIFECYCLE_ADMIN_PORT
#                       the ports on 127.0.0.1 (default: 19000 and 19001);
#                       a cluster uses LIFECYCLE_S3_PORT to + 31, on every
#                       address for S3
#   LIFECYCLE_KEEP=yes  leave everything in place when a step fails
#
# Everything it makes carries the deployment's name. It refuses to start
# when anything of its names is there already - a container of the name,
# of <name>-previous, <name>-other or <name>-commit, volumes or containers
# labelled with
# either name, the sentinel volume, the extra tag - and it removes what it
# made when it ends, unless LIFECYCLE_KEEP=yes and a step failed: the
# deployment by the script's own purge, and the sentinel volume and the
# extra tag of the image only when its work directory records that it made
# them and the engine still shows them as made (the sentinel's label
# io.cactusfs.lifecycle.run, the tag's image ID). It removes nothing else:
# no prune, no other volume or image.
set -eu
LC_ALL=C
export LC_ALL
# The engine is the one named here, never one the shell points at.
unset DOCKER_HOST DOCKER_CONTEXT CONTAINER_HOST

lc_here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
lc_repo=$(CDPATH='' cd -- "$lc_here/../.." && pwd -P)
lc_deploy=$lc_repo/deploy/deploy.sh

lc_engine=${ENGINE:-docker}
lc_image=${LIFECYCLE_IMAGE:-}
lc_source=${LIFECYCLE_SOURCE:-image}
lc_rel=${LIFECYCLE_RELEASE_DIR:-$lc_repo/dist/image}
lc_dirty=${LIFECYCLE_ALLOW_DIRTY:-no}
lc_context=${LIFECYCLE_CONTEXT:-default}
lc_name=${LIFECYCLE_NAME:-cfs-lifecycle}
lc_s3=${LIFECYCLE_S3_PORT:-19000}
lc_admin=${LIFECYCLE_ADMIN_PORT:-19001}
lc_keep=${LIFECYCLE_KEEP:-no}
lc_mode=${LIFECYCLE_MODE:-standalone}
case $lc_mode in
standalone) ;;
cluster) lc_admin=$((lc_s3 + 1)) ;;
*)
	echo "lifecycle: FAIL - LIFECYCLE_MODE=$lc_mode: standalone or cluster" >&2
	exit 1
	;;
esac
case $lc_engine in
docker) lc_target=${LIFECYCLE_TARGET:-ce} ;;
podman) lc_target=${LIFECYCLE_TARGET:-podman} ;;
*)
	echo "lifecycle: FAIL - ENGINE=$lc_engine: docker or podman" >&2
	exit 1
	;;
esac

# say <text>, fail <text>
say() {
	printf 'lifecycle: %s\n' "$*"
}
fail() {
	printf 'lifecycle: FAIL - %s\n' "$*" >&2
	exit 1
}

case $lc_source in
image) [ -n "$lc_image" ] || fail "LIFECYCLE_IMAGE is not set: the image to deploy (make test-deploy builds one)" ;;
binaries) [ -f "$lc_rel/BUILDINFO" ] || fail "no $lc_rel/BUILDINFO: run make image-binaries (make test-deploy LIFECYCLE_SOURCE=binaries does)" ;;
*) fail "LIFECYCLE_SOURCE=$lc_source: image or binaries" ;;
esac
for lc_tool in "$lc_engine" curl tar cmp awk; do
	command -v "$lc_tool" >/dev/null 2>&1 || fail "$lc_tool is not installed"
done
curl --help all 2>/dev/null | grep -q -- '--aws-sigv4' || fail "this curl has no --aws-sigv4 (curl 7.75 or later)"

# eng <word>...: the engine, as the script reaches it.
eng() {
	if [ "$lc_engine" = podman ]; then
		podman "$@"
	else
		docker --context "$lc_context" "$@"
	fi
}

# sha <file>: the SHA-256 of a file, in hex.
sha() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	else
		shasum -a 256 "$1" | awk '{ print $1 }'
	fi
}

# quoted <word>: the word quoted for a shell, for the text of the trap.
quoted() {
	printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

if [ "$lc_source" = binaries ]; then
	lc_version=$(sed -n 's/^version=//p' "$lc_rel/BUILDINFO")-$(sed -n 's/^build=//p' "$lc_rel/BUILDINFO")
	lc_image="the binaries of $lc_rel"
else
	eng image inspect "$lc_image" >/dev/null 2>&1 || fail "the image $lc_image is not on $lc_engine"
	lc_version=$(eng image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$lc_image")
fi
[ "$lc_version" != - ] && [ -n "$lc_version" ] || fail "$lc_image has no version: no label org.opencontainers.image.version, or no version= and build= in BUILDINFO"
lc_tag=cactusfs-lifecycle-upgrade:$lc_name

# What the run makes must not be there before it: it removes only what it
# made, and refuses to start on anything of its names. Each question is a
# listing, which answers nothing when there is nothing - so "not there" is
# told apart from an engine that cannot be asked, and a listing that fails
# fails the run (it is never taken for "not there").
# listed <what> <word>...: the engine's listing, one name a line, or fail.
listed() {
	_ls_what=$1
	shift
	eng "$@" || fail "could not list $_ls_what of $lc_engine (its message is above): nothing is started on an engine that cannot be asked"
}
lc_out=$(listed volumes volume ls --format '{{.Name}}') || exit 1
printf '%s\n' "$lc_out" | grep -qx "$lc_name-sentinel" &&
	fail "a volume $lc_name-sentinel is there already: this run would not have made it; remove it or set LIFECYCLE_NAME"
lc_out=$(listed images image ls --format '{{.Repository}}:{{.Tag}}') || exit 1
printf '%s\n' "$lc_out" | grep -qxF -e "$lc_tag" -e "localhost/$lc_tag" &&
	fail "an image $lc_tag is there already: this run would not have made it; remove it or set LIFECYCLE_NAME"
# The images the script builds from the binaries are tagged
# cactusfs:<version>-<build>-<12 hex of the ID>: those there before the run
# are not the run's to remove.
lc_before_images=$(printf '%s\n' "$lc_out" | sed 's|^localhost/||' | grep "^cactusfs:$lc_version-" || :)
lc_out=$(listed containers ps -a --format '{{.Names}}') || exit 1
for lc_n in "$lc_name" "$lc_name-previous" "$lc_name-other" "$lc_name-commit"; do
	printf '%s\n' "$lc_out" | grep -qx "$lc_n" && fail "a container $lc_n is there already: remove it or set LIFECYCLE_NAME"
done
for lc_n in "$lc_name" "$lc_name-other"; do
	lc_labelled=$(listed containers ps -a --filter "label=io.cactusfs.deploy.name=$lc_n" --format '{{.Names}}') || exit 1
	[ -z "$lc_labelled" ] || fail "containers labelled $lc_n are there already ($lc_labelled): remove them or set LIFECYCLE_NAME"
	lc_labelled=$(listed volumes volume ls --filter "label=io.cactusfs.deploy.name=$lc_n" --format '{{.Name}}') || exit 1
	[ -z "$lc_labelled" ] || fail "volumes labelled $lc_n are there already ($lc_labelled): purge them (deploy/deploy.sh --uninstall --purge) or set LIFECYCLE_NAME"
done

lc_work=$(mktemp -d "${TMPDIR:-/tmp}/cactusfs-lifecycle.XXXXXX")
# What the end of the run needs to remove only the image tags this run made:
# the tags of that form there before it, and the version they carry.
printf '%s\n' "$lc_before_images" >"$lc_work/images-before"
printf '%s\n' "$lc_version" >"$lc_work/version"
chmod 0700 "$lc_work"
lc_state=$lc_work/state
lc_done=$lc_work/done
# The run's own mark: the sentinel volume carries it as a label, and the
# work directory records what the run made (sentinel-made, tag-made).
lc_run=${lc_work##*/}

# lifecycle_end <work> <done> <engine> <context> <name> <tag> <keep> <deploy>
# <env file>: what the run made is removed - unless it failed and
# LIFECYCLE_KEEP=yes - by the script's own purge (of the deployment recorded
# in the run's own STATE_DIR), then the image the script built from the
# binaries (built-made), the sentinel volume and the extra tag,
# each only when the work directory records that this run made it and the
# engine still shows it as made (the volume's label, the tag's image ID),
# and last the work directory. Its arguments are written into the trap
# when it is set; its variables are its own (_le_*).
# built_record <ref>: an image the script built from the binaries, recorded
# ("<ref> <ID>" a line) so that the end of the run removes it - only when it
# was not there before this run.
built_record() {
	printf '%s\n' "$lc_before_images" | grep -qxF "${1#localhost/}" && return 0
	printf '%s %s\n' "$1" "$(eng image inspect --format '{{.Id}}' "$1")" >>"$lc_work/built-made"
}

lifecycle_end() {
	_le_work=$1
	_le_done=$2
	_le_engine=$3
	_le_context=$4
	_le_name=$5
	_le_tag=$6
	_le_keep=$7
	_le_deploy=$8
	_le_env=$9
	if [ ! -f "$_le_done" ] && [ "$_le_keep" = yes ]; then
		echo "lifecycle: LIFECYCLE_KEEP=yes: left in place - the deployment $_le_name and $_le_work" >&2
		return 0
	fi
	if [ "$_le_engine" = podman ]; then
		set -- podman
	else
		set -- docker --context "$_le_context"
	fi
	if [ -f "$_le_env" ]; then
		# The run's own STATE_DIR still records the deployment (a step
		# failed before step 9 purged it): it is purged. What the purge
		# says is kept, on standard error; then whatever is still labelled
		# with the name is listed, so nothing left goes unsaid.
		if [ -n "$(find "$_le_work/state" -path "*/$_le_name/target" 2>/dev/null)" ]; then
			echo "lifecycle: removing the deployment $_le_name (deploy/deploy.sh --uninstall --purge):" >&2
			"$_le_deploy" --env "$_le_env" --uninstall --purge "--purge-confirm=$_le_name" >&2 ||
				echo "lifecycle: the purge of $_le_name failed (its message is above)" >&2
		fi
		_le_left=$({
			"$@" ps -a --filter "label=io.cactusfs.deploy.name=$_le_name" --format 'container {{.Names}}'
			"$@" volume ls --filter "label=io.cactusfs.deploy.name=$_le_name" --format 'volume {{.Name}}'
		} 2>&1) || _le_left="$_le_left (a listing failed)"
		if [ -n "$_le_left" ]; then
			printf 'lifecycle: still labelled %s after the purge:\n%s\n' "$_le_name" "$_le_left" >&2
		fi
	fi
	# The container the cluster's upgrade image is committed from, when a
	# run stopped between its creation and its removal: only this run's (its
	# label names the run's directory).
	if [ "$("$@" container inspect --format '{{index .Config.Labels "io.cactusfs.lifecycle.run"}}' "$_le_name-commit" 2>/dev/null)" = "${_le_work##*/}" ]; then
		"$@" rm -f "$_le_name-commit" >/dev/null 2>&1 || :
	fi
	if [ -f "$_le_work/sentinel-made" ] &&
		[ "$("$@" volume inspect --format '{{index .Labels "io.cactusfs.lifecycle.run"}}' "$_le_name-sentinel" 2>/dev/null)" = "$(cat "$_le_work/sentinel-made")" ]; then
		"$@" volume rm "$_le_name-sentinel" >/dev/null 2>&1 || :
	fi
	if [ -f "$_le_work/built-made" ]; then
		while read -r _le_built _le_built_id; do
			if [ -n "$_le_built" ] && [ "$("$@" image inspect --format '{{.Id}}' "$_le_built" 2>/dev/null)" = "$_le_built_id" ]; then
				"$@" image rm "$_le_built" >/dev/null 2>&1 || :
			fi
		done <"$_le_work/built-made"
	fi
	if [ -f "$_le_work/images-before" ] && [ -f "$_le_work/version" ]; then
		# Every cactusfs:<version>-<12 hex of an ID> tag the script gave an
		# image during this run (each build from the binaries and each
		# given image is tagged so): removed by name, a tag only, unless it
		# was there before the run.
		_le_ver=$(cat "$_le_work/version")
		for _le_ref in $("$@" images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null); do
			_le_short=${_le_ref#localhost/}
			case $_le_short in
			"cactusfs:$_le_ver-"*) ;;
			*) continue ;;
			esac
			printf '%s\n' "${_le_short#"cactusfs:$_le_ver-"}" | grep -qxE '[0-9a-f]{12}' || continue
			grep -qxF "$_le_short" "$_le_work/images-before" && continue
			"$@" image rm "$_le_ref" >/dev/null 2>&1 || :
		done
	fi
	if [ -f "$_le_work/tag-made" ]; then
		# The tag the script gave that image, cactusfs:<version>-<build>-<12
		# hex of its ID> (PLAN 10.4): removed by name, only while it still
		# names this run's image - a tag, never the image under another name.
		_le_id=$(sed 's/^sha256://' "$_le_work/tag-made")
		for _le_ref in $("$@" images --no-trunc --format '{{.Repository}}:{{.Tag}}@{{.ID}}' 2>/dev/null); do
			case $_le_ref in
			cactusfs:*@sha256:"$_le_id" | localhost/cactusfs:*@sha256:"$_le_id" | cactusfs:*@"$_le_id" | localhost/cactusfs:*@"$_le_id")
				"$@" image rm "${_le_ref%@*}" >/dev/null 2>&1 || :
				;;
			esac
		done
		if [ "$("$@" image inspect --format '{{.Id}}' "$_le_tag" 2>/dev/null)" = "$(cat "$_le_work/tag-made")" ]; then
			"$@" image rm "$_le_tag" >/dev/null 2>&1 || :
		fi
	fi
	case $_le_work in
	*/cactusfs-lifecycle.*) rm -rf "$_le_work" ;;
	esac
	return 0
}
lc_env=$lc_work/site.env
trap "lifecycle_end $(quoted "$lc_work") $(quoted "$lc_done") $(quoted "$lc_engine") $(quoted "$lc_context") $(quoted "$lc_name") $(quoted "$lc_tag") $(quoted "$lc_keep") $(quoted "$lc_deploy") $(quoted "$lc_env")" EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# settings <image> [<name>] [<source>]: the env file of the deployment -
# from LIFECYCLE_SOURCE, or the source given.
settings() {
	{
		echo "KMS=local"
		echo "DEPLOY_TARGET=$lc_target"
		echo "DEPLOY_NAME=${2:-$lc_name}"
		echo "STATE_DIR=$lc_state"
		if [ "${3:-$lc_source}" = binaries ]; then
			echo "IMAGE_SOURCE=binaries"
			echo "RELEASE_DIR=$lc_rel"
			echo "ALLOW_DIRTY=$lc_dirty"
		else
			echo "IMAGE_SOURCE=image"
			echo "IMAGE=$1"
		fi
		echo "S3_PORT=$lc_s3"
		echo "ADMIN_PORT=$lc_admin"
		echo "WAIT_TIMEOUT=180"
		[ "$lc_engine" = podman ] || echo "ENGINE_CONTEXT=$lc_context"
		if [ "$lc_mode" = cluster ]; then
			echo "DEPLOY_MODE=cluster"
			echo "ALLOW_SHARED_LOCAL_KMS=yes"
		fi
	}
}

# deploy <argument>...: the script, with the env file.
deploy() {
	"$lc_deploy" --env "$lc_env" "$@"
}

# The volumes labelled with a deployment's name, one a line.
labelled() {
	eng volume ls --filter "label=io.cactusfs.deploy.name=$1" --format '{{.Name}}' | sort
}

# node_ctr <service>: the container of a node of the cluster, by the
# deployment's label and compose's service label.
node_ctr() {
	eng ps -a --filter "label=io.cactusfs.deploy.name=$lc_name" --filter "label=com.docker.compose.service=$1" --format '{{.Names}}'
}

# first_ctr: the container of the standalone node, or of node1.
first_ctr() {
	if [ "$lc_mode" = cluster ]; then
		node_ctr node1
	else
		printf '%s\n' "$lc_name"
	fi
}

say "deploying $lc_image (version $lc_version) with $lc_engine (target $lc_target) as $lc_name, S3 on 127.0.0.1:$lc_s3"

# 1. Install.
settings "$lc_image" >"$lc_env"
deploy --install
lc_entry=$(find "$lc_state" -mindepth 2 -maxdepth 2 -type d -name "$lc_name")
[ -n "$lc_entry" ] && [ -f "$lc_entry/target" ] || fail "no entry of $lc_name in $lc_state"
lc_ca=$lc_entry/config/s3-ca.pem
[ -f "$lc_ca" ] || fail "no CA in $lc_entry/config"
# The image the script built from the binaries: the node's, recorded so
# that the end of the run removes it when this run made it.
lc_built=
if [ "$lc_source" = binaries ]; then
	lc_first=$(first_ctr)
	[ -n "$lc_first" ] || fail "no container of the deployment's first node"
	lc_built=$(eng container inspect --format '{{.Config.Image}}' "$lc_first")
	case ${lc_built#localhost/} in
	"cactusfs:$lc_version-"*) ;;
	*) fail "the node runs $lc_built, not an image the script built from the binaries (cactusfs:$lc_version-<ID>)" ;;
	esac
	built_record "$lc_built"
	say "the script built $lc_built from $lc_rel"
fi

# 2. An object of 5 MiB, put and read back over TLS with SigV4. The root
# credentials go to curl in a config file of the work directory (0600),
# never on its command line.
{
	printf 'user = "'
	tr -d '\n' <"$lc_entry/secrets/root-access-key"
	printf ':'
	tr -d '\n' <"$lc_entry/secrets/root-secret-key"
	printf '"\n'
} >"$lc_work/curl.cfg"
chmod 0600 "$lc_work/curl.cfg"
lc_empty=$(printf '' | {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum; else shasum -a 256; fi
} | awk '{ print $1 }')

# s3_at <port> <method> <path> <payload hash> <curl argument>...
s3_at() {
	_s3_port=$1
	_s3_method=$2
	_s3_path=$3
	_s3_hash=$4
	shift 4
	curl -sS -f -g --noproxy '*' --max-time 60 --cacert "$lc_ca" -K "$lc_work/curl.cfg" \
		--aws-sigv4 "aws:amz:us-east-1:s3" -H "x-amz-content-sha256: $_s3_hash" -H 'Expect:' \
		-X "$_s3_method" "$@" "https://localhost:$_s3_port$_s3_path"
}

# s3 <method> <path> <payload hash> <curl argument>...: on node 1, or the
# standalone node.
s3() {
	s3_at "$lc_s3" "$@"
}

: >"$lc_work/empty"
head -c 5242880 /dev/urandom >"$lc_work/object"
lc_sum=$(sha "$lc_work/object")
s3 PUT /lifecycle "$lc_empty" -T "$lc_work/empty" -o /dev/null || fail "the bucket could not be made (curl's message is above)"
s3 PUT /lifecycle/object "$lc_sum" -T "$lc_work/object" -o /dev/null || fail "the object could not be put (curl's message is above)"
# read_back <when>: the object read back is the one put.
read_back() {
	rm -f "$lc_work/got"
	s3 GET /lifecycle/object "$lc_empty" -o "$lc_work/got" || fail "$1: the object could not be read (curl's message is above)"
	[ "$(sha "$lc_work/got")" = "$lc_sum" ] || fail "$1: the object read back is not the one put"
	say "$1: the object of 5 MiB read back, SHA-256 $lc_sum"
}
read_back "install"

if [ "$lc_mode" = cluster ]; then
	# 3c. kms.json in STATE_DIR is the one the nodes mount; compose's secret
	# files are 0444 in a 0700 directory, equal to the entry's.
	lc_node1=$(node_ctr node1)
	[ -n "$lc_node1" ] || fail "no container of node1"
	mkdir "$lc_work/kms"
	eng cp "$lc_node1:/var/lib/cactusfs/config/kms.json" - >"$lc_work/kms.tar"
	tar -x -f "$lc_work/kms.tar" -C "$lc_work/kms"
	cmp -s "$lc_entry/config/kms.json" "$lc_work/kms/kms.json" || fail "kms.json in $lc_entry/config and the one node1 mounts differ"
	rm -rf "$lc_work/kms" "$lc_work/kms.tar"
	[ -z "$(find "$lc_entry/compose/secrets" -type f ! -perm 0444)" ] || fail "a file of $lc_entry/compose/secrets is not 0444"
	[ -z "$(find "$lc_entry/compose/secrets" -prune ! -perm 0700)" ] || fail "$lc_entry/compose/secrets is not 0700"
	for lc_f in root-access-key root-secret-key kms-passphrase; do
		cmp -s "$lc_entry/secrets/$lc_f" "$lc_entry/compose/secrets/$lc_f" || fail "compose's secret file $lc_f is not the entry's"
	done
	say "kms.json in STATE_DIR is the one the nodes mount; compose's secret files are the entry's, 0444 in a 0700 directory"

	# The node's user, 65532, reads each of compose's secret files where
	# the node finds it, bind-mounted at /run/secrets/<name>: the entry of
	# each, as the engine reads it out with its owner and mode, is readable
	# by 65532 - as its owner or group, or by everyone.
	for lc_f in root-access-key root-secret-key kms-passphrase; do
		eng cp "$lc_node1:/run/secrets/$lc_f" - >"$lc_work/secret.tar"
		tar -t -v --numeric-owner -f "$lc_work/secret.tar" | awk '
			{
				perm = $1
				if ($2 ~ /\//) { split($2, id, "/"); uid = id[1]; gid = id[2] } else { uid = $3; gid = $4 }
				owner = uid == 65532 && substr(perm, 2, 1) == "r"
				group = gid == 65532 && substr(perm, 5, 1) == "r"
				other = substr(perm, 8, 1) == "r"
				if (!(owner || group || other)) { print "not readable by 65532: " $NF " (" perm ", " uid ":" gid ")"; bad = 1 }
				n++
			}
			END { if (n < 1) { print "nothing listed"; exit 1 }; exit bad }' || fail "the node's user cannot read compose's secret file $lc_f"
		rm -f "$lc_work/secret.tar"
	done
	say "the node's user 65532 reads every one of compose's secret files at /run/secrets"

	# A second cluster on the ports the first holds is refused before
	# anything is made.
	settings "$lc_image" "$lc_name-other" >"$lc_work/other.env"
	lc_status=0
	"$lc_deploy" --env "$lc_work/other.env" --install >"$lc_work/other.out" 2>&1 || lc_status=$?
	[ "$lc_status" = 2 ] && grep -q 'in use\|is published by' "$lc_work/other.out" ||
		fail "a second cluster on port $lc_s3 was not refused (exit $lc_status): $(cat "$lc_work/other.out")"
	labelled "$lc_name-other" | grep -q . && fail "the refused cluster made volumes"
	[ -z "$(find "$lc_state" -mindepth 2 -maxdepth 2 -name "$lc_name-other")" ] || fail "the refused cluster made an entry in STATE_DIR"
	say "a second cluster on the ports of the first is refused, and makes nothing"

	# The leader, as the admin API names it before the upgrade (the root
	# credentials as the bearer API key, on curl's standard input): the
	# upgrade must make it again last.
	lc_leader=$({
		printf 'Authorization: Bearer '
		tr -d '\n' <"$lc_entry/secrets/root-access-key"
		printf ':'
		tr -d '\n' <"$lc_entry/secrets/root-secret-key"
		printf '\n'
	} | curl -sS -f -g --noproxy '*' --max-time 10 --cacert "$lc_ca" -H @- "https://localhost:$lc_admin/api/v1/cluster" |
		tr -d '\n' | sed -n 's/.*"leader"[[:space:]]*:[[:space:]]*"\(node[123]\)".*/\1/p') ||
		fail "the admin API could not be asked for the leader (curl's message is above)"
	[ -n "$lc_leader" ] || fail "the admin API names no leader"
	say "the admin API names $lc_leader the leader"

	# 4c. An upgrade to an image of a new ID, one node at a time, while the
	# object is read from whichever node answers: no round goes by without
	# a node that serves it, and none serves other bytes. The script runs
	# an image that no registry holds by a tag that names its ID (PLAN
	# 10.4), and compose makes a node again only when its configuration
	# changes: another tag of the same image would change nothing. So the
	# upgrade is to the image deployed (given, or built from the binaries),
	# committed from a container of it that is never started - the same
	# content and labels, a new ID - tagged $lc_tag and given as
	# IMAGE_SOURCE=image.
	if [ "$lc_source" = binaries ]; then lc_from=$lc_built; else lc_from=$lc_image; fi
	lc_from_id=$(eng image inspect --format '{{.Id}}' "$lc_from") || fail "could not inspect $lc_from"
	eng container create --name "$lc_name-commit" --label "io.cactusfs.lifecycle.run=$lc_run" "$lc_from" >/dev/null ||
		fail "could not create a container of $lc_from to commit the upgrade's image from (the engine's message is above)"
	lc_status=0
	eng commit "$lc_name-commit" "$lc_tag" >/dev/null || lc_status=$?
	# Recorded at once, so that the end of the run removes the tag even
	# when the removal of the container below fails.
	if [ "$lc_status" = 0 ]; then
		eng image inspect --format '{{.Id}}' "$lc_tag" >"$lc_work/tag-made" || lc_status=$?
	fi
	eng rm "$lc_name-commit" >/dev/null || fail "could not remove $lc_name-commit (the engine's message is above)"
	[ "$lc_status" = 0 ] || fail "could not commit $lc_tag from $lc_from (exit $lc_status; the engine's message is above)"
	[ "$(cat "$lc_work/tag-made")" != "$lc_from_id" ] || fail "$lc_tag, committed from $lc_from, has its ID: the upgrade would change nothing"
	settings "$lc_tag" "$lc_name" image >"$lc_env"
	deploy --install >"$lc_work/upgrade.out" 2>&1 &
	lc_pid=$!
	lc_reads=0
	while kill -0 "$lc_pid" 2>/dev/null; do
		lc_served=
		for lc_n in 0 1 2 3; do
			rm -f "$lc_work/got"
			if s3_at $((lc_s3 + lc_n * 10)) GET /lifecycle/object "$lc_empty" -o "$lc_work/got" 2>/dev/null; then
				if [ "$(sha "$lc_work/got")" != "$lc_sum" ]; then
					kill "$lc_pid" 2>/dev/null || :
					fail "during the upgrade node$((lc_n + 1)) served other bytes than the object put"
				fi
				lc_served=1
				break
			fi
		done
		if [ -z "$lc_served" ]; then
			kill "$lc_pid" 2>/dev/null || :
			fail "during the upgrade no node served the object; the upgrade's output: $(cat "$lc_work/upgrade.out")"
		fi
		lc_reads=$((lc_reads + 1))
		sleep 1
	done
	lc_status=0
	wait "$lc_pid" || lc_status=$?
	cat "$lc_work/upgrade.out"
	[ "$lc_status" = 0 ] || fail "the upgrade exited $lc_status"
	[ "$lc_reads" -ge 2 ] || fail "the object was read $lc_reads times during the upgrade: too fast to prove anything"
	# Each node runs that image, by its ID (the script's tag names it).
	for lc_svc in node1 node2 node3 node4; do
		[ "$(eng container inspect --format '{{.Image}}' "$(node_ctr "$lc_svc")")" = "$(cat "$lc_work/tag-made")" ] ||
			fail "after the upgrade $lc_svc does not run $lc_tag ($(cat "$lc_work/tag-made"))"
	done
	grep -q 'upgrade: one node at a time: node4 ' "$lc_work/upgrade.out" || fail "the upgrade did not begin with node4"
	grep -q "upgrade: one node at a time: node4 .* $lc_leader\$" "$lc_work/upgrade.out" ||
		fail "the upgrade did not put the leader, $lc_leader, last: $(grep 'one node at a time' "$lc_work/upgrade.out")"
	lc_last=$(grep 'run: .* compose .* up -d --no-build --no-deps' "$lc_work/upgrade.out" | tail -n 1)
	case $lc_last in
	*" $lc_leader") ;;
	*) fail "the last node the upgrade made again is not the leader, $lc_leader: $lc_last" ;;
	esac
	read_back "upgrade (read $lc_reads times during it)"

	# 5c. --status: every node ready.
	deploy --status >"$lc_work/status.out"
	for lc_text in "version $lc_version" "S3 of node1: https://<any address of this machine>:$lc_s3" "admin API and console: https://127.0.0.1:$lc_admin" "/readyz: 200 on every node"; do
		grep -qF "$lc_text" "$lc_work/status.out" || fail "--status does not say '$lc_text': $(cat "$lc_work/status.out")"
	done
	say "--status says every node is ready"

	# 6c. Uninstall removes every container and keeps the 13 volumes; a
	# reinstall reads the object.
	lc_before=$(labelled "$lc_name")
	deploy --uninstall
	[ -z "$(eng ps -a --filter "label=io.cactusfs.deploy.name=$lc_name" --format '{{.Names}}')" ] ||
		fail "containers of $lc_name are left after --uninstall"
	[ "$(labelled "$lc_name")" = "$lc_before" ] || fail "--uninstall changed the volumes"
	[ "$(printf '%s\n' "$lc_before" | grep -c .)" = 13 ] || fail "the cluster has not its 13 volumes: $lc_before"
	deploy --install
	read_back "reinstall"
else
	# 3. kms.json in STATE_DIR is the one in the config volume, and the node
	# made no key file of its own.
	mkdir "$lc_work/kms"
	eng cp "$lc_name:/var/lib/cactusfs/config/kms.json" - >"$lc_work/kms.tar"
	tar -x -f "$lc_work/kms.tar" -C "$lc_work/kms"
	cmp -s "$lc_entry/config/kms.json" "$lc_work/kms/kms.json" || fail "kms.json in $lc_entry/config and in the volume $lc_name-config differ"
	rm -rf "$lc_work/kms" "$lc_work/kms.tar"
	eng cp "$lc_name:/var/lib/cactusfs/meta" - | tar -t -f - >"$lc_work/meta.list"
	grep -q . "$lc_work/meta.list" || fail "the metadata volume could not be listed"
	if grep -q 'kms-local\.json' "$lc_work/meta.list"; then
		fail "the metadata volume holds kms-local.json: the node made a key file of its own"
	fi
	say "kms.json in STATE_DIR is the config volume's; the metadata volume holds no kms-local.json"

	# 4. The node's user, 65532, reads every file of the secrets' volumes and
	# their roots, as PLAN 10.3's helper left them (cp -a): each entry of the
	# volume, as the engine reads it out with its owner and mode, is readable by
	# 65532 - its owner or its group with the read bit (and the search bit of a
	# directory), or everyone's. GNU tar lists the owner as uid/gid, bsdtar in
	# two fields. The node being ready proves it read those it uses.
	for lc_unit in secrets config; do
		eng cp "$lc_name:/var/lib/cactusfs/$lc_unit" - >"$lc_work/$lc_unit.tar"
		tar -t -v --numeric-owner -f "$lc_work/$lc_unit.tar" | awk -v unit="$lc_unit" '
		{
			perm = $1
			if ($2 ~ /\//) { split($2, id, "/"); uid = id[1]; gid = id[2] } else { uid = $3; gid = $4 }
			dir = substr(perm, 1, 1) == "d"
			owner = uid == 65532 && substr(perm, 2, 1) == "r" && (!dir || substr(perm, 4, 1) == "x")
			group = gid == 65532 && substr(perm, 5, 1) == "r" && (!dir || substr(perm, 7, 1) == "x")
			other = substr(perm, 8, 1) == "r" && (!dir || substr(perm, 10, 1) == "x")
			if (!(owner || group || other)) { print "not readable by 65532: " $NF " (" perm ", " uid ":" gid ")"; bad = 1 }
			n++
		}
		END {
			if (n < 2) { print "the volume " unit " lists " n " entries"; exit 1 }
			exit bad
		}' || fail "the node's user cannot read every file of the volume $lc_name-$lc_unit"
		rm -f "$lc_work/$lc_unit.tar"
	done
	say "every file of $lc_name-secrets and $lc_name-config, and their roots, is readable by 65532"

	# 5. A second deployment on the ports the first holds is refused before
	# anything is made.
	settings "$lc_image" "$lc_name-other" >"$lc_work/other.env"
	lc_status=0
	"$lc_deploy" --env "$lc_work/other.env" --install >"$lc_work/other.out" 2>&1 || lc_status=$?
	[ "$lc_status" = 2 ] && grep -q 'in use\|is published by' "$lc_work/other.out" ||
		fail "a second deployment on port $lc_s3 was not refused (exit $lc_status): $(cat "$lc_work/other.out")"
	labelled "$lc_name-other" | grep -q . && fail "the refused deployment made volumes"
	[ -z "$(find "$lc_state" -mindepth 2 -maxdepth 2 -name "$lc_name-other")" ] || fail "the refused deployment made an entry in STATE_DIR"
	say "a second deployment on port $lc_s3 is refused, and makes nothing"

	# 6. An upgrade to a new tag keeps the object, and leaves no previous node.
	# From the binaries, the upgrade builds them again: the same content, the
	# same tag, a new node.
	if [ "$lc_source" = binaries ]; then
		deploy --install
		# A build is not byte for byte the same twice (times in the layers), so
		# the upgrade may run another ID's tag: it must still be one the script
		# built from these binaries.
		lc_built2=$(eng container inspect --format '{{.Config.Image}}' "$lc_name")
		case ${lc_built2#localhost/} in
		"cactusfs:$lc_version-"*) built_record "$lc_built2" ;;
		*) fail "after the upgrade the node runs $lc_built2, not an image the script built from the binaries" ;;
		esac
	else
		eng tag "$lc_image" "$lc_tag"
		eng image inspect --format '{{.Id}}' "$lc_tag" >"$lc_work/tag-made"
		settings "$lc_tag" >"$lc_env"
		deploy --install
		# The script runs a given image that no registry holds by a tag that
		# names its ID (PLAN 10.4): the node must run that image, by its ID.
		[ "$(eng container inspect --format '{{.Image}}' "$lc_name")" = "$(cat "$lc_work/tag-made")" ] ||
			fail "after the upgrade the node does not run $lc_tag ($(cat "$lc_work/tag-made"))"
	fi
	eng ps -a --format '{{.Names}}' | grep -qx "$lc_name-previous" && fail "the upgrade left $lc_name-previous"
	read_back "upgrade"

	# 7. --status: the version, the addresses, /readyz 200.
	deploy --status >"$lc_work/status.out"
	for lc_text in "version $lc_version" "S3: https://127.0.0.1:$lc_s3" "admin API and console: https://127.0.0.1:$lc_admin" "/readyz: 200"; do
		grep -qF "$lc_text" "$lc_work/status.out" || fail "--status does not say '$lc_text': $(cat "$lc_work/status.out")"
	done
	say "--status says the version, the addresses and /readyz 200"

	# 8. Uninstall keeps the volumes; a reinstall reads the object.
	lc_before=$(labelled "$lc_name")
	deploy --uninstall
	eng ps -a --format '{{.Names}}' | grep -qx "$lc_name" && fail "the container $lc_name is still there after --uninstall"
	[ "$(labelled "$lc_name")" = "$lc_before" ] || fail "--uninstall changed the volumes"
	[ "$(printf '%s\n' "$lc_before" | grep -c .)" = 7 ] || fail "the deployment has not its 7 volumes: $lc_before"
	deploy --install
	read_back "reinstall"
fi

# 9. A purge without the name is refused; one with it deletes the
# deployment's volumes and its entry of STATE_DIR, and nothing else.
eng volume create --label "io.cactusfs.lifecycle.run=$lc_run" "$lc_name-sentinel" >/dev/null
echo "$lc_run" >"$lc_work/sentinel-made"
lc_sentinel=${lc_entry%/*}/$lc_name-sentinel
mkdir -p "$lc_sentinel"
echo kept >"$lc_sentinel/kept"
lc_status=0
deploy --uninstall --purge </dev/null >"$lc_work/purge.out" 2>&1 || lc_status=$?
[ "$lc_status" = 2 ] || fail "a purge without the name at a terminal was not refused (exit $lc_status)"
[ "$(labelled "$lc_name")" = "$lc_before" ] || fail "a refused purge changed the volumes"
deploy --uninstall --purge "--purge-confirm=$lc_name"
[ -z "$(labelled "$lc_name")" ] || fail "volumes of $lc_name are left after the purge: $(labelled "$lc_name")"
eng volume inspect "$lc_name-sentinel" >/dev/null 2>&1 || fail "the purge deleted the sentinel volume $lc_name-sentinel"
[ ! -e "$lc_entry" ] || fail "the purge left $lc_entry"
[ "$(cat "$lc_sentinel/kept")" = kept ] || fail "the purge touched the sentinel entry $lc_sentinel"
say "the purge deleted the volumes of $lc_name and $lc_entry; the sentinel volume and entry stay"

: >"$lc_done"
say "PASS ($lc_engine, target $lc_target, from $lc_source)"
